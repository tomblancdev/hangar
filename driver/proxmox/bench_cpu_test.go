package proxmox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/internal/ids"
)

// guestKey makes a key pair for a test's guests: its public half for their
// specs, its private half on the bench (root's own, removed at the end) —
// the tests enter a guest from the bench, never the driver.
func (b *bench) guestKey(t *testing.T, name string) (pub, onBench string) {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", filepath.Join(dir, "k")).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	p, _ := os.ReadFile(filepath.Join(dir, "k.pub"))
	priv, _ := os.ReadFile(filepath.Join(dir, "k"))
	onBench = "/root/hangar-" + name + "-key"
	b.must(t, "umask 077 && cat > "+onBench, string(priv))
	t.Cleanup(func() { _, _ = b.ssh(t, "rm -f "+onBench) })
	return strings.TrimSpace(string(p)), onBench
}

// lease waits for the address the bench's own DHCP gave a VM, by the MAC of
// its config: a guest needs no agent to be found.
func (b *bench) lease(t *testing.T, vmid string) string {
	t.Helper()
	mac := strings.ToLower(b.must(t, "qm config "+vmid+` | sed -n 's/^net0: virtio=\([^,]*\),.*/\1/p'`))
	if mac == "" {
		t.Fatalf("VM %s has no MAC: %s", vmid, b.must(t, "qm config "+vmid))
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if ip := b.must(t, "cat /var/lib/misc/dnsmasq.*.leases 2>/dev/null | awk -v m="+mac+" 'tolower($2) == m {print $3}' | tail -1"); ip != "" {
			return ip
		}
		if time.Now().After(deadline) {
			t.Fatalf("no lease for VM %s (%s) after 5 minutes", vmid, mac)
		}
		time.Sleep(5 * time.Second)
	}
}

// inGuest runs a command inside a guest, as the image's own user, from the
// bench; it waits for the guest's ssh the first time.
func (b *bench) inGuest(t *testing.T, key, ip, cmd string) string {
	t.Helper()
	line := "ssh -i " + key + " -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes debian@" + ip + " "
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if _, err := b.ssh(t, line+"true"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ssh into %s after 5 minutes", ip)
		}
		time.Sleep(5 * time.Second)
	}
	return b.must(t, line+"'"+strings.ReplaceAll(cmd, "'", `'\''`)+"'")
}

// usedMB is what a guest's system disk holds on the bench's storage.
func (b *bench) usedMB(t *testing.T, vmid string) int {
	t.Helper()
	n, err := strconv.Atoi(b.must(t, "zfs get -Hp -o value used rpool/data/vm-"+vmid+"-disk-0"))
	if err != nil {
		t.Fatal(err)
	}
	return n >> 20
}

// A VM's processor, read INSIDE real guests. One that asks nothing sees the
// zone's own model — AES, and no virtualisation; cpu host sees the node's own
// processor and still no virtualisation; with virtualization it may run VMs
// (/dev/kvm). Its share of the cores is the cgroup's own weight, changed
// while it runs. And its system disk gives back what is deleted inside it.
// All of it through the plugin's own fenced token.
func TestBenchAVMsProcessor(t *testing.T) {
	b := onBench(t)
	d := b.open(t, b.token)
	ctx := context.Background()
	pub, key := b.guestKey(t, "bench-cpu")
	node := strings.TrimSpace(b.must(t, `grep -m1 "model name" /proc/cpuinfo | cut -d: -f2`))
	nodeVirt := b.must(t, `grep -m1 -o -w -E "vmx|svm" /proc/cpuinfo | head -1`)
	if nodeVirt == "" {
		t.Fatal("the bench itself has no virtualisation: its guests are VMs")
	}
	nodeAES := b.must(t, `grep -m1 -c -w aes /proc/cpuinfo || true`)

	type vm struct {
		name string
		spec driver.GuestSpec
		line string // its cpu line on Proxmox
		id   string
		vmid string
		ip   string
	}
	vms := []*vm{
		{name: "plain", spec: driver.GuestSpec{}, line: "x86-64-v2-AES,flags=-nested-virt"},
		{name: "host", spec: driver.GuestSpec{CPU: driver.CPUModelHost, CPUWeight: 25}, line: "host,flags=-nested-virt"},
		{name: "nested", spec: driver.GuestSpec{CPU: driver.CPUModelHost, Virtualization: true}, line: "host,flags=+nested-virt"},
	}
	for _, v := range vms {
		v.id = ids.New("m")
		id := v.id
		t.Cleanup(func() {
			if err := d.DeleteGuest(context.Background(), id); err != nil {
				t.Errorf("cleaning up %s: %v", id, err)
			}
		})
		s := v.spec
		s.ID, s.Kind, s.Name, s.Cores, s.MemoryMB, s.DiskGB, s.Image = v.id, "vm", "bench-cpu-"+v.name, 1, 1024, 4, "debian-13" // a VM whose memory is hot-plugged has 1024 MB at least
		s.SSHKeys, s.Tags = []string{pub}, map[string]string{"class": "guaranteed"}
		g, err := d.CreateGuest(ctx, s)
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if !g.Running || g.CPU != v.spec.CPU || g.Virtualization != v.spec.Virtualization || g.CPUWeight != driver.WeightOf(v.spec.CPUWeight) {
			t.Fatalf("%s created as %+v", v.name, g)
		}
		v.vmid = vmid(g)
		if got := b.config(t, "qm", v.vmid, "cpu"); got != v.line {
			t.Fatalf("%s: Proxmox holds cpu %q, want %q", v.name, got, v.line)
		}
		if got := b.config(t, "qm", v.vmid, "scsi0"); !strings.Contains(got, "discard=on") || !strings.Contains(got, "size=4G") {
			t.Fatalf("%s: its system disk is %q", v.name, got)
		}
	}
	for _, v := range vms {
		v.ip = b.lease(t, v.vmid)
		in := strings.Split(b.inGuest(t, key, v.ip,
			`grep -m1 "model name" /proc/cpuinfo | cut -d: -f2; grep -m1 -c -w -E "vmx|svm" /proc/cpuinfo; grep -m1 -c -w aes /proc/cpuinfo; test -e /dev/kvm && echo kvm || echo nokvm`), "\n")
		if len(in) != 4 {
			t.Fatalf("%s: inside it: %q", v.name, in)
		}
		model, virt, aes, kvm := strings.TrimSpace(in[0]), in[1], in[2], in[3]
		t.Logf("%s (VM %s, %s): %q, virtualisation %s, aes %s, %s", v.name, v.vmid, v.ip, model, virt, aes, kvm)
		switch v.name {
		case "plain":
			if model == node || virt != "0" || aes != nodeAES || kvm != "nokvm" {
				t.Errorf("a VM that asked nothing sees %q (the node: %q), virtualisation %s, aes %s (the node: %s), %s", model, node, virt, aes, nodeAES, kvm)
			}
		case "host":
			if model != node || virt != "0" || kvm != "nokvm" {
				t.Errorf("cpu host sees %q (the node: %q), virtualisation %s, %s — it asked for none", model, node, virt, kvm)
			}
		case "nested":
			if model != node || virt != "1" || kvm != "kvm" {
				t.Errorf("cpu host with virtualization sees %q (the node: %q), virtualisation %s, %s", model, node, virt, kvm)
			}
		}
	}

	// its share of the cores: the cgroup's own weight, changed while it runs
	plain, host := vms[0], vms[1]
	weight := func(vmid string) string {
		return b.must(t, "cat /sys/fs/cgroup/qemu.slice/"+vmid+".scope/cpu.weight")
	}
	if weight(host.vmid) != "25" || weight(plain.vmid) != "100" {
		t.Fatalf("the cgroups weigh %s (asked 25) and %s (asked nothing)", weight(host.vmid), weight(plain.vmid))
	}
	pid := b.must(t, "cat /var/run/qemu-server/"+host.vmid+".pid")
	g, err := d.SetCPUWeight(ctx, host.id, 60)
	if err != nil || g.CPUWeight != 60 || !g.Running {
		t.Fatalf("a share set on a running VM: %+v %v", g, err)
	}
	if weight(host.vmid) != "60" {
		t.Fatalf("the cgroup weighs %s after 60 was set", weight(host.vmid))
	}
	if out := b.must(t, "qm pending "+host.vmid+" | grep -c -E '^(new|del) ' || true"); out != "0" {
		t.Fatalf("the share waits for a restart: %s", b.must(t, "qm pending "+host.vmid))
	}
	if now := b.must(t, "cat /var/run/qemu-server/"+host.vmid+".pid"); now != pid {
		t.Fatalf("QEMU changed from %s to %s", pid, now)
	}
	if g, err = d.SetCPUWeight(ctx, host.id, 0); err != nil || g.CPUWeight != driver.FullWeight || weight(host.vmid) != "100" {
		t.Fatalf("back to a full share: %+v %v, the cgroup %s", g, err, weight(host.vmid))
	}

	// its system disk gives back what is deleted inside it
	before := b.usedMB(t, host.vmid)
	b.inGuest(t, key, host.ip, "dd if=/dev/urandom of=/var/tmp/blob bs=1M count=300 status=none && sync")
	time.Sleep(8 * time.Second) // ZFS counts at its next transaction
	full := b.usedMB(t, host.vmid)
	trimmed := b.inGuest(t, key, host.ip, "rm /var/tmp/blob && sync && sudo fstrim -v /")
	time.Sleep(8 * time.Second)
	after := b.usedMB(t, host.vmid)
	t.Logf("the disk on the storage: %d MB, %d MB with 300 MB written, %d MB once deleted (%s)", before, full, after, trimmed)
	if full-before < 250 || full-after < 250 {
		t.Fatalf("300 MB written then deleted: %d MB → %d MB → %d MB on the storage", before, full, after)
	}
}

// A container's share of the cores is its cgroup's weight too, from birth
// and changed while it runs.
func TestBenchAContainersWeight(t *testing.T) {
	b := onBench(t)
	d := b.open(t, b.token)
	ctx := context.Background()
	archive := "local:vztmpl/" + b.must(t, "ls /var/lib/vz/template/cache/ | grep \"^debian-13-standard_.*_$(dpkg --print-architecture)\\.\" | sort -V | tail -1")
	id := ids.New("m")
	t.Cleanup(func() {
		if err := d.DeleteGuest(context.Background(), id); err != nil {
			t.Errorf("cleaning up %s: %v", id, err)
		}
	})
	g, err := d.CreateGuest(ctx, driver.GuestSpec{ID: id, Kind: "container", Name: "bench-weight", Cores: 1, MemoryMB: 256, DiskGB: 2,
		Image: archive, Tags: map[string]string{"class": "spot"}, CPUWeight: 30})
	if err != nil || !g.Running || g.CPUWeight != 30 {
		t.Fatalf("%+v %v", g, err)
	}
	v := vmid(g)
	weight := func() string { return b.must(t, "cat /sys/fs/cgroup/lxc/"+v+"/cpu.weight") }
	if weight() != "30" {
		t.Fatalf("born at 30, its cgroup weighs %s", weight())
	}
	if g, err = d.SetCPUWeight(ctx, id, 80); err != nil || g.CPUWeight != 80 || !g.Running || weight() != "80" {
		t.Fatalf("set to 80: %+v %v, its cgroup %s", g, err, weight())
	}
	// what a container cannot be: a processor of its own, VMs inside
	for name, s := range map[string]driver.GuestSpec{
		"its host's processor": {CPU: driver.CPUModelHost},
		"VMs inside":           {Virtualization: true},
	} {
		s.ID, s.Kind, s.Cores, s.MemoryMB, s.Image = ids.New("m"), "container", 1, 256, archive
		_, err := d.CreateGuest(ctx, s)
		refused(t, err, "both are a VM's")
		if _, err := d.Guest(ctx, s.ID); err == nil {
			t.Fatalf("%s: a refused container was made", name)
		}
	}
}
