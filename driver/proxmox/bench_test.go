package proxmox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/internal/ids"
)

// The tests below run against a real Proxmox VE: the throwaway one
// tools/bench/bench.sh makes. Without its variables they skip:
//
//	sh tools/bench/bench.sh up && eval "$(sh tools/bench/bench.sh env)" && go test ./driver/proxmox/ -run Bench -v

type bench struct {
	url, ca, token, wide, key, port string
	// host: where root on the bench answers ssh — this machine, through
	// bench.sh's forwarded port, unless HANGAR_BENCH_SSH_HOST names a bench
	// that answers by itself (a machine that is one)
	host string
}

func onBench(t *testing.T) *bench {
	t.Helper()
	b := &bench{
		url: os.Getenv("HANGAR_BENCH_URL"), ca: os.Getenv("HANGAR_BENCH_CA_FILE"),
		token: os.Getenv("HANGAR_BENCH_TOKEN_FILE"), wide: os.Getenv("HANGAR_BENCH_WIDE_TOKEN_FILE"),
		key: os.Getenv("HANGAR_BENCH_SSH_KEY"), port: os.Getenv("HANGAR_BENCH_SSH_PORT"), host: "127.0.0.1",
	}
	if h := os.Getenv("HANGAR_BENCH_SSH_HOST"); h != "" {
		b.host = h
	}
	if b.url == "" {
		t.Skip("no bench: sh tools/bench/bench.sh up, then eval its env")
	}
	return b
}

// ssh runs a command as root on the bench (the tests' own reads, never the
// driver's way in).
func (b *bench) ssh(t *testing.T, cmd string, stdin ...string) (string, error) {
	t.Helper()
	c := exec.Command("ssh", "-i", b.key, "-p", b.port, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "root@"+b.host, cmd)
	if len(stdin) > 0 {
		c.Stdin = strings.NewReader(stdin[0])
	}
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (b *bench) must(t *testing.T, cmd string, stdin ...string) string {
	t.Helper()
	out, err := b.ssh(t, cmd, stdin...)
	if err != nil {
		t.Fatalf("on the bench: %s: %v\n%s", cmd, err, out)
	}
	return out
}

func (b *bench) open(t *testing.T, tokenFile string) *Driver {
	t.Helper()
	return b.openWith(t, tokenFile, "11000-11019", []string{"100"}, nil) // 100: the bench's priority guest
}

// openWith opens the bench as one plugin's token would, on a range of VMIDs,
// with what else the zone says.
func (b *bench) openWith(t *testing.T, tokenFile, vmids string, watch []string, more map[string]string) *Driver {
	t.Helper()
	tok, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	o := map[string]string{
		"node": "pve-bench", "pool": "hangar", "images_pool": "hangar-images", "storage": "local-zfs",
		"seed_storage": "hangar-seeds", "bridge": "hbnet", "vmids": vmids, "ca_file": b.ca,
	}
	for k, v := range more {
		o[k] = v
	}
	d, err := Open(context.Background(), driver.Params{Zone: "bench", Endpoint: b.url, Credential: tok, Watch: watch, Options: o})
	if err != nil {
		t.Fatal(err)
	}
	return d.(*Driver)
}

// vmid reads the guest's VMID from its engine ref (node/type/vmid).
func vmid(g driver.Guest) string { return g.EngineRef[strings.LastIndex(g.EngineRef, "/")+1:] }

func TestBenchTheFence(t *testing.T) {
	b := onBench(t)
	d := b.open(t, b.token)
	if !has(d.Capabilities(), driver.FencePool) {
		t.Fatalf("the plugin's own token is not fenced: %s", d.FenceReport())
	}
	// the control: root's token, which reaches everything
	w := b.open(t, b.wide)
	if has(w.Capabilities(), driver.FencePool) || w.FenceReport() == "" {
		t.Fatalf("root's token reads as fenced: %v", w.Capabilities())
	}
	t.Logf("root's token: %s", w.FenceReport())
}

func TestBenchAContainersLife(t *testing.T) {
	b := onBench(t)
	d := b.open(t, b.token)
	ctx := context.Background()
	archive := "local:vztmpl/" + b.must(t, "ls /var/lib/vz/template/cache/ | grep \"^debian-13-standard_.*_$(dpkg --print-architecture)\\.\" | sort -V | tail -1")
	id := ids.New("m")
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGXj2Dq6dbWAg2wXCN9pDWc3cS/cJEHWIr0sRd4b3M5V bench-ct"
	spec := driver.GuestSpec{
		ID: id, Kind: "container", Name: "bench-ct", Cores: 1, MemoryMB: 512, DiskGB: 4,
		Image: archive, SSHKeys: []string{key}, Tags: map[string]string{"class": "spot"},
	}
	t.Cleanup(func() {
		if err := d.DeleteGuest(context.Background(), id); err != nil {
			t.Errorf("cleaning up %s: %v", id, err)
		}
	})

	g, err := d.CreateGuest(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Running || g.Name != "bench-ct" || g.Cores != 1 || g.MemoryMB != 512 || g.DiskGB != 4 || g.Tags["class"] != "spot" || g.ID != id {
		t.Fatalf("created as %+v", g)
	}
	again, err := d.CreateGuest(ctx, spec)
	if err != nil || again.EngineRef != g.EngineRef {
		t.Fatalf("a second create made %s (%v), not the first %s", again.EngineRef, err, g.EngineRef)
	}
	v := vmid(g)
	if got := b.must(t, "pct exec "+v+" -- cat /root/.ssh/authorized_keys"); !strings.Contains(got, "bench-ct") {
		t.Fatalf("the key did not arrive: %q", got)
	}
	initPID := b.must(t, "lxc-info -n "+v+" -p -H")

	// live: more cores and memory, then less memory above what it holds
	if g, err = d.ResizeGuest(ctx, id, 2, 768); err != nil || g.Cores != 2 || g.MemoryMB != 768 {
		t.Fatalf("grow: %+v %v", g, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for b.must(t, "pct exec "+v+" -- nproc") != "2" {
		if time.Now().After(deadline) {
			t.Fatal("two cores did not reach the container within 30 s")
		}
		time.Sleep(2 * time.Second)
	}
	if g, err = d.ResizeGuest(ctx, id, 2, 384); err != nil || g.MemoryMB != 384 {
		t.Fatalf("shrink above use: %+v %v", g, err)
	}
	if got := b.must(t, "pct exec "+v+" -- sh -c \"free -m | awk '/Mem:/ {print \\$2}'\""); got != "384" {
		t.Errorf("free inside says %s MB, not 384", got)
	}
	// below what it holds: refused, and the container untouched
	_, err = d.ResizeGuest(ctx, id, 2, 16)
	if !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), "holds") {
		t.Fatalf("a shrink below use was not refused: %v", err)
	}
	t.Logf("refused: %v", err)
	if g, _ = d.Guest(ctx, id); g.MemoryMB != 384 || !g.Running {
		t.Fatalf("after the refusal: %+v", g)
	}
	if now := b.must(t, "lxc-info -n "+v+" -p -H"); now != initPID {
		t.Fatalf("init changed from %s to %s: the live changes restarted it", initPID, now)
	}
	if len(g.Addresses) == 0 {
		t.Log("no address read yet (DHCP may still be answering)")
	} else {
		t.Logf("addresses: %v", g.Addresses)
	}

	if g, err = d.SetPower(ctx, id, false); err != nil || g.Running {
		t.Fatalf("stop: %+v %v", g, err)
	}
	if _, err = d.Reboot(ctx, id); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("a stopped container rebooted: %v", err)
	}
	if g, err = d.SetPower(ctx, id, true); err != nil || !g.Running {
		t.Fatalf("start: %+v %v", g, err)
	}
	if err := d.DeleteGuest(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Guest(ctx, id); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("still there after delete: %v", err)
	}
	if out, err := b.ssh(t, "pct status "+v); err == nil {
		t.Fatalf("the node still has %s: %s", v, out)
	}
	if err := d.DeleteGuest(ctx, id); err != nil {
		t.Fatalf("a second delete: %v", err)
	}
}

// A VM cloned from the template, its first boot fed by the seed disc: the
// user data installs the guest agent (so the driver reads its address) and
// writes a proof; the key lets the bench's root in as the image's user.
func TestBenchAVMsLife(t *testing.T) {
	b := onBench(t)
	d := b.open(t, b.token)
	ctx := context.Background()
	dir := t.TempDir()
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "bench-vm", "-f", filepath.Join(dir, "k")).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	pub, _ := os.ReadFile(filepath.Join(dir, "k.pub"))
	priv, _ := os.ReadFile(filepath.Join(dir, "k"))
	id := ids.New("m")
	t.Cleanup(func() {
		if err := d.DeleteGuest(context.Background(), id); err != nil {
			t.Errorf("cleaning up %s: %v", id, err)
		}
	})
	userData := "#cloud-config\npackage_update: true\npackages: [qemu-guest-agent]\nruncmd:\n" +
		"  - [systemctl, start, qemu-guest-agent]\n  - [sh, -c, 'echo hangar-proof > /var/lib/hangar-proof']\n"
	g, err := d.CreateGuest(ctx, driver.GuestSpec{
		ID: id, Kind: "vm", Name: "bench-vm", Cores: 1, MemoryMB: 1024, DiskGB: 5, Image: "debian-13",
		SSHKeys: []string{strings.TrimSpace(string(pub))}, UserData: []byte(userData), Tags: map[string]string{"class": "guaranteed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !g.Running || g.Cores != 1 || g.MemoryMB != 1024 || g.DiskGB != 5 || g.Tags["class"] != "guaranteed" {
		t.Fatalf("created as %+v", g)
	}
	v := vmid(g)
	qemuPID := b.must(t, "cat /var/run/qemu-server/"+v+".pid")

	// the agent answers once the user data has run
	deadline := time.Now().Add(10 * time.Minute)
	for len(g.Addresses) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no address from the guest agent after 10 minutes: the user data did not run?")
		}
		time.Sleep(10 * time.Second)
		if g, err = d.Guest(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	ip := g.Addresses[0]
	t.Logf("the guest agent reports %v", g.Addresses)
	b.must(t, "umask 077 && cat > /root/bench-vm-key", string(priv))
	in := b.must(t, "ssh -i /root/bench-vm-key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR debian@"+ip+
		" 'hostname; cat /var/lib/hangar-proof; lsblk -bdno SIZE /dev/sda'")
	lines := strings.Fields(in)
	if len(lines) != 3 || lines[0] != "bench-vm" || lines[1] != "hangar-proof" || lines[2] != fmt.Sprint(5<<30) {
		t.Fatalf("inside the VM: %q (want the name, the proof, a 5 GiB disk)", in)
	}

	// live: memory grows (a DIMM hot-plugged); cores and a shrink wait for a stop
	if g, err = d.ResizeGuest(ctx, id, 1, 1536); err != nil || g.MemoryMB != 1536 {
		t.Fatalf("grow live: %+v %v", g, err)
	}
	if _, err = d.ResizeGuest(ctx, id, 2, 1536); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("a running VM's cores changed: %v", err)
	}
	if _, err = d.ResizeGuest(ctx, id, 1, 1024); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("a running VM's memory shrank: %v", err)
	}
	if now := b.must(t, "cat /var/run/qemu-server/"+v+".pid"); now != qemuPID {
		t.Fatalf("QEMU changed from %s to %s", qemuPID, now)
	}
	if g, err = d.Reboot(ctx, id); err != nil || !g.Running {
		t.Fatalf("reboot: %+v %v", g, err)
	}
	if g, err = d.SetPower(ctx, id, false); err != nil || g.Running {
		t.Fatalf("stop: %+v %v", g, err)
	}
	if g, err = d.ResizeGuest(ctx, id, 2, 1024); err != nil || g.Cores != 2 || g.MemoryMB != 1024 {
		t.Fatalf("resize stopped: %+v %v", g, err)
	}
	if err := d.DeleteGuest(ctx, id); err != nil {
		t.Fatal(err)
	}
	if out := b.must(t, "pvesm list hangar-seeds"); strings.Contains(out, id) {
		t.Fatalf("the seed disc stayed: %s", out)
	}
	if out, err := b.ssh(t, "qm status "+v); err == nil {
		t.Fatalf("the node still has %s: %s", v, out)
	}
}

// A pre-start hook that refuses: Proxmox answers the start with 200 and a
// task that fails; the driver must say so, in the hook's words.
func TestBenchAHookRefusesAStart(t *testing.T) {
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
	g, err := d.CreateGuest(ctx, driver.GuestSpec{ID: id, Kind: "container", Cores: 1, MemoryMB: 256, DiskGB: 2, Image: archive, Stopped: true})
	if err != nil || g.Running {
		t.Fatalf("%+v %v", g, err)
	}
	hook := "#!/bin/sh\nif [ \"$2\" = pre-start ]; then echo 'bench hook: no room for this guest' >&2; exit 1; fi\nexit 0\n"
	b.must(t, "mkdir -p /var/lib/vz/snippets && cat > /var/lib/vz/snippets/bench-refuse.sh && chmod +x /var/lib/vz/snippets/bench-refuse.sh", hook)
	b.must(t, "pct set "+vmid(g)+" --hookscript local:snippets/bench-refuse.sh") // root's alone to attach
	_, err = d.SetPower(ctx, id, true)
	if !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), "bench hook: no room for this guest") {
		t.Fatalf("the hook's refusal did not come back in its words: %v", err)
	}
	t.Logf("refused: %v", err)
	if g, _ = d.Guest(ctx, id); g.Running {
		t.Fatal("it runs anyway")
	}
}

// What a zone's reservations wait on, read with the machines' own token: the
// power of the priority guest it may watch (VM.Audit on VM 100, nothing
// more), the node's state (no grant needed) — and a guest it does not watch
// refused.
func TestBenchTheWatcher(t *testing.T) {
	b := onBench(t)
	d := b.open(t, b.token)
	ctx := context.Background()
	b.must(t, "qm stop 100 >/dev/null 2>&1; true")
	if on, err := d.GuestRunning(ctx, "100"); err != nil || on {
		t.Fatalf("VM 100 stopped reads %v, %v", on, err)
	}
	b.must(t, "qm set 100 --delete hookscript >/dev/null 2>&1; qm start 100")
	t.Cleanup(func() { b.must(t, "qm stop 100") })
	if on, err := d.GuestRunning(ctx, "100"); err != nil || !on {
		t.Fatalf("VM 100 running reads %v, %v", on, err)
	}
	// a lone node is never "offline" (no membership says so); when pvestatd
	// lags it reads "unknown", which cannot tell — never down
	if down, err := d.NodeDown(ctx, "pve-bench"); down || (err != nil && !strings.Contains(err.Error(), "cannot tell")) {
		t.Fatalf("the node reads down: %v, %v", down, err)
	} else if err != nil {
		t.Logf("the node: %v", err)
	}
	if up, err := d.Awake(ctx); err != nil || !up {
		t.Fatalf("the zone reads asleep: %v, %v", up, err)
	}
	if _, err := d.GuestRunning(ctx, "9000"); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("a guest the zone does not watch: %v", err)
	}
	// the control: the same token cannot stop what it watches
	if err := d.c.run(ctx, "POST", "/nodes/pve-bench/qemu/100/status/stop", nil); err == nil || !strings.Contains(err.Error(), "VM.PowerMgmt") {
		t.Fatalf("the token stopped the watched guest: %v", err)
	}
}
