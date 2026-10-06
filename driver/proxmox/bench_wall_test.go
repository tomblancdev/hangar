package proxmox

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/internal/ids"
)

// walledBench is the bench as a zone that gives its guests their address and
// keeps a wall: the ids from first get the addresses from .20 of the bench's
// own guest network (its DHCP gives .100 to .199), and wear the group
// setup.sh made — the node, on ssh.
func walledBench(first int) map[string]string {
	return map[string]string{
		"subnet": "198.51.100.0/24", "first_address": "198.51.100." + strconv.Itoa(20+first-11000), "gateway": "198.51.100.1",
		"firewall": "on", "firewall_groups": "hangar-floor", "firewall_log": "info",
	}
}

// eventually asks until the answer is the one wanted, or says what it still
// was: what a hand changes on a wall is on the wire at Proxmox's next pass.
func eventually(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: still not so after %s", what, within)
		}
		time.Sleep(2 * time.Second)
	}
}

// toldInside waits for a container to hold what it was told at its birth.
// Its own init writes it there a moment after its start — a second or so on
// a bench that has run other tests — so a read asked once, right then,
// sometimes finds it not yet there: it is asked until it is, and the log
// says how many reads that took.
func toldInside(t testing.TB, read func() string, told ...string) {
	t.Helper()
	began := time.Now()
	for asked := 1; ; asked++ {
		at := time.Since(began).Round(100 * time.Millisecond)
		last := read()
		missing := ""
		for _, want := range told {
			if !strings.Contains(last, want) {
				missing = want
				break
			}
		}
		if missing == "" {
			t.Logf("what it was told was inside it at read %d, asked %s after its birth returned", asked, at)
			return
		}
		if time.Since(began) > toldWithin {
			t.Fatalf("inside it, still no %q %s after its birth (%d reads):\n%s", missing, toldWithin, asked, last)
			return
		}
		t.Logf("read %d, asked %s after its birth returned: no %q inside yet", asked, at, missing)
		time.Sleep(toldEvery)
	}
}

// toldWithin, toldEvery: how long toldInside asks, and how often.
var toldWithin, toldEvery = time.Minute, time.Second

// refusing is a test that is told it failed, and goes on: what toldInside
// says of a container that never holds what it was told.
type refusing struct {
	testing.TB
	said []string
}

func (r *refusing) Helper()                   {}
func (r *refusing) Logf(string, ...any)       {}
func (r *refusing) Fatalf(f string, a ...any) { r.said = append(r.said, fmt.Sprintf(f, a...)) }

// The wait itself, without a bench: a container that holds what it was told
// only at the third read is asked three times, and one that never does is
// refused in words that name what is missing — the read asked once, as the
// bench's tests asked it, was red on the first of these.
func TestToldInsideAsksUntilItIsThere(t *testing.T) {
	within, every := toldWithin, toldEvery
	toldWithin, toldEvery = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { toldWithin, toldEvery = within, every })

	asked := 0
	late := func() string {
		asked++
		if asked < 3 {
			return "inet 198.51.100.7/24" // its address, and not yet its route
		}
		return "inet 198.51.100.7/24\ndefault via 198.51.100.1"
	}
	r := &refusing{TB: t}
	toldInside(r, late, "inet 198.51.100.7/24", "default via 198.51.100.1")
	if asked != 3 || len(r.said) != 0 {
		t.Fatalf("a container told late: asked %d times, refused %q", asked, r.said)
	}

	never := &refusing{TB: t}
	toldInside(never, func() string { return "inet 198.51.100.7/24" }, "inet 198.51.100.7/24", "default via 198.51.100.1")
	if len(never.said) != 1 || !strings.Contains(never.said[0], `no "default via 198.51.100.1"`) {
		t.Fatalf("a container never told its route: %q", never.said)
	}
}

// A machine born behind its wall, on a real Proxmox VE, through the plugin's
// own fenced token. Its address is the one its number gives, written inside
// it by Proxmox; a neighbour that pinged that address from before its birth
// never got an answer; it reaches out and is answered; the operator's group
// is the one door in; it cannot send as another address, as the gateway, or
// from another MAC. A wall opened by hand lets the neighbour in — the control
// — and the next look names what it puts back. A guest born before, with a
// lease, is walled as it runs and pinned to the zone's range. Without the
// bench's variables it skips.
func TestBenchAMachineBornBehindItsWall(t *testing.T) {
	b := onBench(t)
	if _, err := b.ssh(t, "grep -q '^\\[group hangar-floor\\]' /etc/pve/firewall/cluster.fw"); err != nil {
		t.Skip("a bench set up before the wall: sh tools/bench/bench.sh up")
	}
	d := b.openWith(t, b.token, "11000-11019", nil, walledBench(11000))
	plain := b.open(t, b.token)
	if !has(d.Capabilities(), driver.NetFirewall) || has(plain.Capabilities(), driver.NetFirewall) {
		t.Fatal("net.firewall is the flag of the zone that turned its wall on")
	}
	ctx := context.Background()
	archive := "local:vztmpl/" + b.must(t, "ls /var/lib/vz/template/cache/ | grep \"^debian-13-standard_.*_$(dpkg --print-architecture)\\.\" | sort -V | tail -1")
	born := func(d *Driver, name string) (driver.Guest, string) {
		t.Helper()
		id := ids.New("m")
		t.Cleanup(func() {
			if err := d.DeleteGuest(context.Background(), id); err != nil {
				t.Errorf("cleaning up %s: %v", id, err)
			}
		})
		g, err := d.CreateGuest(ctx, driver.GuestSpec{ID: id, Kind: "container", Name: name, Cores: 1, MemoryMB: 256, DiskGB: 2,
			Image: archive, Tags: map[string]string{"class": "spot"}})
		if err != nil {
			t.Fatal(err)
		}
		return g, vmid(g)
	}
	in := func(v, cmd string) (string, error) {
		return b.ssh(t, "pct exec "+v+" -- sh -c '"+strings.ReplaceAll(cmd, "'", `'\''`)+"'")
	}
	pings := func(from, to string) bool { _, err := in(from, "ping -c2 -W1 "+to); return err == nil }
	knocks := func(from, to string) bool { // its ssh port
		_, err := in(from, "timeout 3 bash -c '</dev/tcp/"+to+"/22'")
		return err == nil
	}

	// the control guest, born first: no wall, a lease — as every guest was
	open, ov := born(plain, "wall-open")
	if open.Wall != "" || open.Address != "" {
		t.Fatalf("a zone with no wall made %+v", open)
	}
	eventually(t, "the control guest has its lease", time.Minute, func() bool { return pings(ov, "198.51.100.1") })

	// the address the next guest will be given is known before it is born:
	// its number's. The control pings it from now on, five times a second.
	next, err := d.freeVMID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	addr := d.lane.address(next).String()
	if _, err := in(ov, "ping -i 0.2 -c 240 -W 1 "+addr+" >/tmp/first.log 2>&1 &"); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	a, av := born(d, "wall-a")
	t.Logf("a walled container was born in %s (its wall written, then Proxmox's pass waited for)", time.Since(began).Round(time.Second))
	if av != strconv.Itoa(next) || a.Address != addr || a.Wall != driver.WallExact || !a.Running {
		t.Fatalf("born as %+v — want guest %d at %s, behind its wall", a, next, addr)
	}
	toldInside(t, func() string {
		out, _ := in(av, "ip -4 -o addr show eth0; ip route show default; cat /etc/resolv.conf")
		return out
	}, "inet "+addr+"/24", "default via 198.51.100.1", "nameserver 198.51.100.1")
	file := b.must(t, "cat /etc/pve/firewall/"+av+".fw")
	for _, want := range []string{"enable: 1", "policy_in: DROP", "dhcp: 0", "[IPSET ipfilter-net0]", addr, "GROUP hangar-floor", "OUT DROP -p udp -sport 67"} {
		if !strings.Contains(file, want) {
			t.Errorf("its firewall file lacks %q:\n%s", want, file)
		}
	}

	// it reaches out, and is answered; nobody comes in but through the
	// operator's group
	if !pings(av, "198.51.100.1") {
		t.Fatal("it does not reach its gateway")
	}
	eventually(t, "it is answered by a neighbour it pings", 20*time.Second, func() bool { return pings(av, strings.Fields(b.must(t, "pct exec "+ov+" -- hostname -I"))[0]) })
	if pings(ov, addr) || knocks(ov, addr) {
		t.Fatal("its neighbour came in")
	}
	if _, err := b.ssh(t, "timeout 3 bash -c '</dev/tcp/"+addr+"/22'"); err != nil {
		t.Fatalf("the operator's group is not worn: the node's ssh is refused: %v", err)
	}
	// from before its birth to now, not one answer: never a first packet
	// without its wall. The pinger is waited for (unanswered by an address
	// nobody holds yet it slows down, then runs on at its pace); and its
	// packets did reach the card — the neighbour learnt its MAC.
	eventually(t, "the pinger begun before its birth has ended", 3*time.Minute, func() bool {
		_, err := in(ov, "pgrep -x ping")
		return err != nil
	})
	first, _ := in(ov, "cat /tmp/first.log")
	if n := strings.Count(first, "bytes from"); n != 0 || !strings.Contains(first, "240 packets transmitted, 0 received") {
		t.Fatalf("its neighbour, pinging since before its birth, was answered %d times:\n%s", n, first)
	}
	mac := b.must(t, "pct config "+av+" | sed -n 's/.*hwaddr=\\([^,]*\\).*/\\1/p'")
	if neigh, _ := in(ov, "ip neigh show "+addr); !strings.Contains(strings.ToUpper(neigh), mac) {
		t.Fatalf("the pinger's packets never reached its card (the neighbour knows no MAC for it: %q): the probe proves nothing", neigh)
	}

	// it sends only as itself. The control guest's own counter of what its
	// wall dropped is the witness — and the same lie told by a guest with no
	// wall passes.
	dropped := func(v string) int {
		n, _ := strconv.Atoi(b.must(t, "iptables -L veth"+v+"i0-OUT -v -n -x | awk '/match-set/ {print $1; exit}'"))
		return n
	}
	before := dropped(av)
	if out, err := in(av, "ip addr add 198.51.100.77/24 dev eth0; ping -c2 -W1 -I 198.51.100.77 198.51.100.1; r=$?; ip addr del 198.51.100.77/24 dev eth0; exit $r"); err == nil {
		t.Fatalf("it sent as another address of its lane:\n%s", out)
	}
	if out, err := in(ov, "ip addr add 198.51.100.78/24 dev eth0; ping -c2 -W1 -I 198.51.100.78 198.51.100.1; r=$?; ip addr del 198.51.100.78/24 dev eth0; exit $r"); err != nil {
		t.Fatalf("the control: a guest with no wall could not send as another address either — the probe proves nothing:\n%s", out)
	}
	// as the gateway itself, to a neighbour it has just talked to
	peer := strings.Fields(b.must(t, "pct exec "+ov+" -- hostname -I"))[0]
	_, _ = in(av, "ping -c1 -W1 "+peer+" >/dev/null; ip addr add 198.51.100.1/32 dev eth0; ping -c3 -W1 -I 198.51.100.1 "+peer+"; ip addr del 198.51.100.1/32 dev eth0")
	if after := dropped(av); after < before+5 {
		t.Fatalf("as another address and as the gateway, its wall dropped %d packets (five were sent)", after-before)
	}
	if arp := b.must(t, "ebtables -L veth"+av+"i0-OUT-ARP"); !strings.Contains(arp, "--arp-ip-src "+addr+" -j RETURN") || !strings.Contains(arp, "-j DROP") {
		t.Fatalf("it may answer ARP for more than its own address:\n%s", arp)
	}
	if out, err := in(av, "ip link set eth0 address 00:00:5E:00:53:99; ping -c2 -W1 198.51.100.1; r=$?; ip link set eth0 address "+mac+"; exit $r"); err == nil {
		t.Fatalf("it sent from another MAC:\n%s", out)
	}
	eventually(t, "back on its own MAC it reaches out again", 20*time.Second, func() bool { return pings(av, "198.51.100.1") })

	// a hand opens the wall behind the brain's back: the neighbour comes in
	// (the control: the wall was what stopped it), the guest reads as open,
	// and the next look names what it puts back
	fw := "/nodes/pve-bench/lxc/" + av + "/firewall"
	b.must(t, "pvesh set "+fw+"/options --enable 0 && pvesh create "+fw+"/rules --type in --action ACCEPT --enable 1 && pvesh create "+fw+"/ipset/ipfilter-net0 --cidr 198.51.100.0/24")
	eventually(t, "its wall opened by hand, its neighbour comes in", 40*time.Second, func() bool { return pings(ov, addr) })
	if g, err := d.Guest(ctx, a.ID); err != nil || g.Wall != "" {
		t.Fatalf("opened by hand, it reads as wall %q (%v)", g.Wall, err)
	}
	initPID := b.must(t, "lxc-info -n "+av+" -p -H")
	fixed, err := d.Wall(ctx, a.ID)
	if err != nil || !slices.Equal(fixed, []string{"options", "address", "rules"}) {
		t.Fatalf("put back %v, %v", fixed, err)
	}
	eventually(t, "its wall put back, its neighbour is refused again", 40*time.Second, func() bool { return !pings(ov, addr) })
	if again, err := d.Wall(ctx, a.ID); err != nil || len(again) != 0 {
		t.Fatalf("a second look wrote again: %v %v", again, err)
	}
	// the card's own flag, on a running guest: put back live
	b.must(t, "pct set "+av+" -net0 \"$(pct config "+av+" | sed -n 's/^net0: //p' | sed 's/,firewall=1//')\"")
	eventually(t, "its card unplugged from its wall, its neighbour comes in", 40*time.Second, func() bool { return pings(ov, addr) })
	if fixed, err = d.Wall(ctx, a.ID); err != nil || !slices.Equal(fixed, []string{"card"}) {
		t.Fatalf("put back %v, %v", fixed, err)
	}
	eventually(t, "its card behind its wall again, its neighbour is refused", 40*time.Second, func() bool { return !pings(ov, addr) })
	if now := b.must(t, "lxc-info -n "+av+" -p -H"); now != initPID {
		t.Fatalf("init changed from %s to %s: putting its wall back restarted it", initPID, now)
	}
	if g, err := d.Guest(ctx, a.ID); err != nil || g.Wall != driver.WallExact || g.Address != addr {
		t.Fatalf("after: %+v %v", g, err)
	}

	// a guest born before its zone gave addresses, walled as it runs: its
	// lease stays, DHCP still passes, and it is pinned to the zone's range —
	// it may take a neighbour's address, never one from outside its lane
	lease := strings.Fields(b.must(t, "pct exec "+ov+" -- hostname -I"))[0]
	if fixed, err = d.Wall(ctx, open.ID); err != nil || !slices.Equal(fixed, []string{"options", "address", "rules", "card"}) {
		t.Fatalf("a guest born before: put back %v, %v", fixed, err)
	}
	if g, err := d.Guest(ctx, open.ID); err != nil || g.Wall != driver.WallRange || g.Address != "" {
		t.Fatalf("a guest born before reads as %+v %v", g, err)
	}
	eventually(t, "the guest born before is closed to its neighbour", 40*time.Second, func() bool { return !pings(av, lease) })
	if _, err := d.SetPower(ctx, open.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetPower(ctx, open.ID, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "restarted behind its wall, it is given its lease again", time.Minute, func() bool { return pings(ov, "198.51.100.1") })
	if out, err := in(ov, "ip addr add 198.51.100.79/24 dev eth0; ping -c2 -W1 -I 198.51.100.79 198.51.100.1; r=$?; ip addr del 198.51.100.79/24 dev eth0; exit $r"); err != nil {
		t.Errorf("pinned to its zone's range, it could not send as another address of it (a wall tighter than it says):\n%s", out)
	}
	before = dropped(ov)
	_, _ = in(ov, "ip addr add 203.0.113.9/32 dev eth0; ping -c2 -W1 -I 203.0.113.9 198.51.100.1; ip addr del 203.0.113.9/32 dev eth0")
	if after := dropped(ov); after < before+2 {
		t.Errorf("pinned to its zone's range, as an address from outside it its wall dropped %d packets (two were sent)", after-before)
	}

	// what its wall refused is on the node, with its number
	if log := b.must(t, "grep -c '^"+av+" .*policy DROP' /var/log/pve-firewall.log || true"); log == "0" {
		t.Error("nothing its wall refused was logged on the node")
	}
	// deleted, its file goes with it
	if err := d.DeleteGuest(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if out, err := b.ssh(t, "test ! -e /etc/pve/firewall/"+av+".fw"); err != nil {
		t.Fatalf("its firewall file stayed: %s", out)
	}
}

// A VM is told its address on its first-boot disc, and is born closed even
// when its template carries a door: a clone begins with its template's
// firewall file. And a bake's builder, made by the images plugin's own
// token, is walled as a machine is.
func TestBenchAVMBornBehindItsWall(t *testing.T) {
	b := onBench(t)
	if _, err := b.ssh(t, "grep -q '^\\[group hangar-floor\\]' /etc/pve/firewall/cluster.fw"); err != nil {
		t.Skip("a bench set up before the wall: sh tools/bench/bench.sh up")
	}
	d := b.openWith(t, b.token, "11040-11049", nil, walledBench(11040))
	ctx := context.Background()
	pub, key := b.guestKey(t, "bench-wall")
	// the template as one baked in a walled zone leaves it, and worse: its
	// builder's address, and a door somebody opened
	b.must(t, "cat > /etc/pve/firewall/9000.fw", "[OPTIONS]\nenable: 1\npolicy_in: ACCEPT\n\n[IPSET ipfilter-net0]\n198.51.100.250\n\n[RULES]\nIN ACCEPT -p icmp\n")
	t.Cleanup(func() { _, _ = b.ssh(t, "rm -f /etc/pve/firewall/9000.fw") })

	id := ids.New("m")
	t.Cleanup(func() {
		if err := d.DeleteGuest(context.Background(), id); err != nil {
			t.Errorf("cleaning up %s: %v", id, err)
		}
	})
	spec := driver.GuestSpec{ID: id, Kind: "vm", Name: "wall-vm", Cores: 1, MemoryMB: 1024, Image: "debian-13",
		SSHKeys: []string{pub}, UserData: []byte("#cloud-config\n"), Tags: map[string]string{"class": "spot"}}
	began := time.Now()
	g, err := d.CreateGuest(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	v := vmid(g)
	n, _ := strconv.Atoi(v)
	addr := d.lane.address(n).String()
	if g.Address != addr || g.Wall != driver.WallExact || !g.Running {
		t.Fatalf("born as %+v — want %s, behind its wall", g, addr)
	}
	conf := b.must(t, "qm config "+v)
	if !strings.Contains(conf, "ipconfig0: ip="+addr+"/24,gw=198.51.100.1") || !strings.Contains(conf, ",firewall=1") {
		t.Fatalf("its config:\n%s", conf)
	}
	card := b.must(t, "qm config "+v+" | grep ^net0")
	// a second create finds it, and its card keeps the MAC its disc names
	if again, err := d.CreateGuest(ctx, spec); err != nil || again.EngineRef != g.EngineRef || b.must(t, "qm config "+v+" | grep ^net0") != card {
		t.Fatalf("a second create: %s (%v), its card %q then %q", again.EngineRef, err, card, b.must(t, "qm config "+v+" | grep ^net0"))
	}
	// the node comes in on ssh through the operator's group, at the address
	// the disc told it: no lease, no agent
	inside := b.inGuest(t, key, addr, "ip -4 -o addr show; ip route show default; resolvectl dns; cloud-init status; curl -sI -m 10 http://deb.debian.org | head -1")
	t.Logf("a walled VM answered ssh at the address it was told %s after its create began", time.Since(began).Round(time.Second))
	for _, want := range []string{"inet " + addr + "/24", "default via 198.51.100.1", "198.51.100.1", "200 OK"} {
		if !strings.Contains(inside, want) {
			t.Errorf("inside it, no %q:\n%s", want, inside)
		}
	}
	// the template's own door and address did not come with the clone
	file := b.must(t, "cat /etc/pve/firewall/"+v+".fw")
	if strings.Contains(file, "icmp") || strings.Contains(file, "198.51.100.250") || !strings.Contains(file, "policy_in: DROP") || !strings.Contains(file, addr) {
		t.Fatalf("its firewall file kept something of its template's:\n%s", file)
	}
	if _, err := b.ssh(t, "ping -c2 -W1 "+addr); err == nil {
		t.Fatal("the node's ping was answered: the template's door is open on its clone")
	}

	// a builder, by the images plugin's token: given its address, walled
	itok := os.Getenv("HANGAR_BENCH_IMAGES_TOKEN_FILE")
	if itok == "" {
		t.Skip("a bench set up before the images token: the builder's wall is not read")
	}
	im := b.openWith(t, itok, "11060-11069", nil, walledBench(11060))
	img := ids.New("img")
	t.Cleanup(func() {
		if err := im.DeleteImage(context.Background(), img); err != nil {
			t.Errorf("cleaning up %s: %v", img, err)
		}
	})
	bake, err := im.Bake(ctx, driver.BakeSpec{ID: img, Attempt: 1, Kind: "vm", Base: "local:import/debian-13-genericcloud-amd64.qcow2",
		DiskGB: 4, Cores: 1, MemoryMB: 1024, Timeout: 10 * time.Minute, UserData: []byte("#cloud-config\n"),
		Tags: map[string]string{"class": "spot", "admitted": "1024"}}, false)
	if err != nil || bake.State != driver.ImagePending {
		t.Fatalf("the bake: %+v %v", bake, err)
	}
	bv := bake.EngineRef[strings.LastIndex(bake.EngineRef, "/")+1:]
	bn, _ := strconv.Atoi(bv)
	baddr := im.lane.address(bn).String()
	bconf := b.must(t, "qm config "+bv)
	bfile := b.must(t, "cat /etc/pve/firewall/"+bv+".fw")
	if !strings.Contains(bconf, "ipconfig0: ip="+baddr+"/24") || !strings.Contains(bconf, ",firewall=1") ||
		!strings.Contains(bfile, "enable: 1") || !strings.Contains(bfile, baddr) || !strings.Contains(bfile, "GROUP hangar-floor") {
		t.Fatalf("the builder %s: its config\n%s\nits firewall file\n%s", bv, bconf, bfile)
	}
	t.Logf("a builder is guest %s at %s, behind its wall", bv, baddr)
}
