package proxmox

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/internal/ids"
)

// netBench is the bench as a zone that cuts networks: walled, its guests'
// ids from first, four networks of /27 on the bridge setup.sh made, their
// gateways from .200 of the bench's own guest network — where its DHCP gives
// nothing — born from the archive the bench built by the product's recipe.
func netBench(b *bench, t *testing.T, first int) map[string]string {
	t.Helper()
	archive, err := b.ssh(t, "ls /var/lib/vz/template/cache/ | grep '^hangar-gateway-.*\\.tar\\.zst$' | sort -V | tail -1")
	if err != nil || archive == "" || os.Getenv("HANGAR_BENCH_NETWORKS_TOKEN_FILE") == "" || os.Getenv("HANGAR_BENCH_MACHINES_NETS_TOKEN_FILE") == "" {
		t.Skip("a bench set up before the networks: sh tools/bench/bench.sh up")
	}
	o := walledBench(first)
	for k, v := range map[string]string{
		"net_bridge": "hgnets", "net_tag": "100", "net_block": "203.0.113.0/24", "net_size": "27",
		"net_vmids": "11200-11203", "net_pool": "hangar-nets", "net_address": "198.51.100.200",
		"net_archive": "local:vztmpl/" + archive,
	} {
		o[k] = v
	}
	return o
}

// Networks of the cloud's own, on a real Proxmox VE, through the two
// plugins' own fenced tokens. Two networks cannot see each other, and one
// sees itself; a machine goes out to the web through its gateway — the lane
// sees the gateway's address only — and nothing comes in; its owner's key
// jumps through the gateway into its network and nowhere else, gets no shell
// there, and no other key passes; a VM is told its address in its network on
// its first-boot disc; a gateway runs while a machine of its network does, is
// made again when its keys change, and is put back by a look. Without the
// bench's variables it skips.
func TestBenchTwoNetworksAndAJump(t *testing.T) {
	b := onBench(t)
	zone := netBench(b, t, 11080)
	nd := b.openWith(t, os.Getenv("HANGAR_BENCH_NETWORKS_TOKEN_FILE"), "11080-11099", nil, zone)
	md := b.openWith(t, os.Getenv("HANGAR_BENCH_MACHINES_NETS_TOKEN_FILE"), "11080-11099", []string{"100"}, zone) // 100: the bench's priority guest
	for name, d := range map[string]*Driver{"networks": nd, "machines": md} {
		if !has(d.Capabilities(), driver.NetPrivate) || !has(d.Capabilities(), driver.FencePool) {
			t.Fatalf("the %s plugin's token, in a zone that cuts networks: %v — %s", name, d.Capabilities(), d.FenceReport())
		}
	}
	// the same token, in a zone that cuts none: a right on the gateways' pool
	// is then a reach beyond its fence
	if plain := b.openWith(t, os.Getenv("HANGAR_BENCH_MACHINES_NETS_TOKEN_FILE"), "11080-11099", []string{"100"}, nil); has(plain.Capabilities(), driver.FencePool) || has(plain.Capabilities(), driver.NetPrivate) {
		t.Fatalf("in a zone with no networks that token reads %v", plain.Capabilities())
	}
	ctx := context.Background()
	archive := "local:vztmpl/" + b.must(t, "ls /var/lib/vz/template/cache/ | grep \"^debian-13-standard_.*_$(dpkg --print-architecture)\\.\" | sort -V | tail -1")
	ownerPub, ownerKey := b.guestKey(t, "net-owner")
	otherPub, otherKey := b.guestKey(t, "net-other")

	in := func(v, cmd string) (string, error) {
		return b.ssh(t, "pct exec "+v+" -- sh -c '"+strings.ReplaceAll(cmd, "'", `'\''`)+"'")
	}
	pings := func(from, to string) bool { _, err := in(from, "ping -c2 -W1 "+to); return err == nil }
	knocks := func(from, to string) bool { // its ssh port
		_, err := in(from, "timeout 3 bash -c '</dev/tcp/"+to+"/22'")
		return err == nil
	}
	status := func(v string) string { return b.must(t, "pct status "+v) }
	network := func(id string, keys ...string) (driver.Network, string) {
		t.Helper()
		t.Cleanup(func() {
			if err := nd.DeleteNetwork(context.Background(), id); err != nil {
				t.Errorf("cleaning up %s: %v", id, err)
			}
		})
		began := time.Now()
		n, err := nd.CreateNetwork(ctx, driver.NetworkSpec{ID: id, Label: "bench network", JumpKeys: keys})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("a network was made in %s (its gateway, its wall, Proxmox's pass waited for): %s, %s", time.Since(began).Round(time.Second), n.Range, n.Jump)
		return n, n.EngineRef[strings.LastIndex(n.EngineRef, "/")+1:]
	}
	machine := func(name, on string) (driver.Guest, string) {
		t.Helper()
		id := ids.New("m")
		t.Cleanup(func() {
			if err := md.DeleteGuest(context.Background(), id); err != nil {
				t.Errorf("cleaning up %s: %v", id, err)
			}
		})
		g, err := md.CreateGuest(ctx, driver.GuestSpec{ID: id, Kind: "container", Name: name, Cores: 1, MemoryMB: 256, DiskGB: 2,
			Image: archive, SSHKeys: []string{ownerPub}, Network: on, Tags: map[string]string{"class": "spot"}})
		if err != nil {
			t.Fatal(err)
		}
		return g, vmid(g)
	}

	// ---- two networks: each its gateway, born stopped behind its wall
	netA, netB := ids.New("net"), ids.New("net")
	a, gwA := network(netA, ownerPub)
	bn, gwB := network(netB)
	ia, _ := strconv.Atoi(gwA)
	ib, _ := strconv.Atoi(gwB)
	ia, ib = ia-11200, ib-11200
	laneA, laneB := nd.nets.lane(ia).String(), nd.nets.lane(ib).String()
	if a.Range != nd.nets.rangeOf(ia).String() || a.Gateway != nd.nets.gateway(ia).String() || a.Jump != "jump@"+laneA ||
		a.Running || a.Wall != driver.WallExact || a.Keys != 1 || bn.Range == a.Range || bn.Keys != 0 {
		t.Fatalf("made as %+v and %+v", a, bn)
	}
	if status(gwA) != "status: stopped" {
		t.Fatalf("a gateway born running: %s", status(gwA))
	}
	conf := b.must(t, "pct config "+gwA)
	for _, want := range []string{"bridge=hbnet,firewall=1", "ip=" + laneA + "/24", "bridge=hgnets,firewall=1", "tag=" + strconv.Itoa(100+ia),
		"ip=" + a.Gateway + "/27", "unprivileged: 1", "memory: 128", "hangar-id." + netA} {
		if !strings.Contains(conf, want) {
			t.Errorf("its gateway's config lacks %q:\n%s", want, conf)
		}
	}
	file := b.must(t, "cat /etc/pve/firewall/"+gwA+".fw")
	for _, want := range []string{"enable: 1", "policy_in: DROP", "ipfilter: 0", "[IPSET ipfilter-net0]", laneA, "GROUP hangar-floor",
		"IN ACCEPT -i net1 -source " + a.Range, "OUT DROP -p udp -sport 67"} {
		if !strings.Contains(file, want) {
			t.Errorf("its gateway's firewall file lacks %q:\n%s", want, file)
		}
	}
	if pool := b.must(t, "pvesh get /pools/hangar-nets --output-format json"); !strings.Contains(pool, `"vmid":`+gwA) {
		t.Fatalf("its gateway is not in the gateways' pool: %s", pool)
	}

	// ---- machines: two on one network, one on the other
	began := time.Now()
	m1, v1 := machine("net-a-one", netA)
	t.Logf("a machine on a network was born in %s (its gateway started first)", time.Since(began).Round(time.Second))
	n1, _ := strconv.Atoi(v1)
	if want := nd.nets.place(ia, n1-11080, nil).addr.Addr().String(); m1.Address != want || m1.Wall != driver.WallExact || !m1.Running {
		t.Fatalf("born as %+v — want %s, behind its wall", m1, want)
	}
	if status(gwA) != "status: running" || status(gwB) != "status: stopped" {
		t.Fatalf("its network's gateway %s, the other %s", status(gwA), status(gwB))
	}
	inside, _ := in(v1, "ip -4 -o addr show eth0; ip route show default; cat /etc/resolv.conf")
	for _, want := range []string{"inet " + m1.Address + "/27", "default via " + a.Gateway, "nameserver 198.51.100.1"} {
		if !strings.Contains(inside, want) {
			t.Errorf("inside it, no %q:\n%s", want, inside)
		}
	}
	mfile := b.must(t, "cat /etc/pve/firewall/"+v1+".fw")
	if !strings.Contains(mfile, "IN ACCEPT -source "+a.Range) || strings.Contains(mfile, "GROUP") || !strings.Contains(mfile, m1.Address) {
		t.Fatalf("its firewall file — its own network in, no group of the lane's:\n%s", mfile)
	}
	m2, v2 := machine("net-a-two", netA)
	m3, v3 := machine("net-b-one", netB)
	if status(gwB) != "status: running" {
		t.Fatalf("the other network's gateway, its first machine born: %s", status(gwB))
	}

	// ---- out through its gateway, to the web; the lane sees the gateway only
	eventually(t, "a machine reaches its gateway", 30*time.Second, func() bool { return pings(v1, a.Gateway) })
	web, err := in(v1, `bash -c "exec 3<>/dev/tcp/deb.debian.org/80 && printf 'HEAD / HTTP/1.0\r\nHost: deb.debian.org\r\n\r\n' >&3 && head -1 <&3"`)
	if err != nil || !strings.Contains(web, " 200 ") {
		t.Fatalf("out to the web through its gateway: %q %v", web, err)
	}
	seen := b.must(t, "(timeout 8 tcpdump -ni hbnet -c 2 'icmp[icmptype] = icmp-echo and dst host 198.51.100.1' 2>/dev/null &) ; sleep 1; pct exec "+v1+" -- ping -c2 -W1 198.51.100.1 >/dev/null; sleep 2; true")
	if !strings.Contains(seen, "IP "+laneA+" > 198.51.100.1") || strings.Contains(seen, m1.Address) {
		t.Fatalf("what the lane saw of a machine's packets:\n%s", seen)
	}

	// ---- two networks apart; one network sees itself
	eventually(t, "two machines of one network reach each other", 30*time.Second, func() bool { return pings(v1, m2.Address) && knocks(v1, m2.Address) })
	if pings(v1, m3.Address) || knocks(v1, m3.Address) || pings(v3, m1.Address) || knocks(v3, m1.Address) {
		t.Fatal("a machine reached a machine of the other network")
	}
	// the other gateway, on the lane: the wall there lets in what the
	// operator's group does — the node's ssh — and a machine is not the node
	if knocks(v1, laneB) {
		t.Fatal("a machine knocked on the other network's gateway")
	}

	// ---- nothing in: the lane's own node, even given a route to the network
	b.must(t, "ip route replace "+a.Range+" via "+laneA)
	t.Cleanup(func() { _, _ = b.ssh(t, "ip route del "+a.Range) })
	if _, err := b.ssh(t, "ping -c2 -W1 "+m1.Address); err == nil {
		t.Fatal("the lane's node pinged a machine through its gateway")
	}
	if _, err := b.ssh(t, "timeout 3 bash -c '</dev/tcp/"+m1.Address+"/22'"); err == nil {
		t.Fatal("the lane's node knocked on a machine through its gateway")
	}
	// they did reach the gateway's card: its wall says what it refused
	eventually(t, "the gateway's wall logged what it refused", 30*time.Second, func() bool {
		out, _ := b.ssh(t, "grep -c '^"+gwA+" .*DST="+m1.Address+"' /var/log/pve-firewall.log")
		return out != "" && out != "0"
	})
	b.must(t, "ip route del "+a.Range)

	// ---- a VM on a network: told its address on its first-boot disc, as on
	// the lane — and it boots while the rest is read
	vmID := ids.New("m")
	t.Cleanup(func() {
		if err := md.DeleteGuest(context.Background(), vmID); err != nil {
			t.Errorf("cleaning up %s: %v", vmID, err)
		}
	})
	vmBegan := time.Now()
	vm, err := md.CreateGuest(ctx, driver.GuestSpec{ID: vmID, Kind: "vm", Name: "net-a-vm", Cores: 1, MemoryMB: 1024, Image: "debian-13",
		SSHKeys: []string{ownerPub}, UserData: []byte("#cloud-config\n"), Network: netA, Tags: map[string]string{"class": "spot"}})
	if err != nil {
		t.Fatal(err)
	}
	vv := vmid(vm)
	nv, _ := strconv.Atoi(vv)
	if want := nd.nets.place(ia, nv-11080, nil).addr.Addr().String(); vm.Address != want || vm.Wall != driver.WallExact {
		t.Fatalf("a VM born as %+v — want %s, behind its wall", vm, want)
	}
	// (Proxmox writes a card's options back in its own order)
	card := b.must(t, "qm config "+vv+" | grep ^net0")
	if conf := b.must(t, "qm config "+vv); !strings.Contains(conf, "ipconfig0: ip="+vm.Address+"/27,gw="+a.Gateway) ||
		cardOpt(card, "bridge") != "hgnets" || cardOpt(card, "tag") != strconv.Itoa(100+ia) || cardOpt(card, "firewall") != "1" {
		t.Fatalf("its config:\n%s", conf)
	}

	// ---- the jump: the owner's key, through the gateway, into its network
	ssh := "ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=6 "
	jump := func(key, gateway, to, cmd string) (string, error) {
		return b.ssh(t, ssh+"-i "+key+" -o ProxyCommand=\""+ssh+"-i "+key+" -W %h:%p jump@"+gateway+"\" root@"+to+" "+cmd)
	}
	through := func(key, gateway, to string) error { // a connection opened through the gateway
		_, err := b.ssh(t, ssh+"-i "+key+" -W "+to+" jump@"+gateway+" </dev/null")
		return err
	}
	eventually(t, "the owner's key jumps into its network", time.Minute, func() bool {
		out, err := jump(ownerKey, laneA, m1.Address, "hostname")
		return err == nil && out == "net-a-one"
	})
	if out, err := jump(ownerKey, laneA, m2.Address, "hostname"); err != nil || out != "net-a-two" {
		t.Fatalf("to the second machine of its network: %q %v", out, err)
	}
	// the machine sees the gateway, never where the jump came from
	if out, _ := jump(ownerKey, laneA, m1.Address, "'echo $SSH_CONNECTION'"); !strings.HasPrefix(out, a.Gateway+" ") {
		t.Fatalf("a machine sees the jump come from %q, want its gateway %s", out, a.Gateway)
	}
	if out, err := jump(otherKey, laneA, m1.Address, "hostname"); err == nil || !strings.Contains(out, "Permission denied") {
		t.Fatalf("a key the network does not name jumped: %q %v", out, err)
	}
	// never a shell on the gateway, nor a command, nor a file, nor root
	for what, cmd := range map[string]string{
		"a shell":   ssh + "-i " + ownerKey + " jump@" + laneA,
		"a command": ssh + "-i " + ownerKey + " jump@" + laneA + " id",
		"a file":    "sftp -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -i " + ownerKey + " jump@" + laneA + ":/etc/passwd /tmp/net-bench-x",
		"root":      ssh + "-i " + ownerKey + " root@" + laneA + " id",
	} {
		if out, err := b.ssh(t, cmd); err == nil {
			t.Fatalf("the owner's key got %s on the gateway: %q", what, out)
		}
	}
	// and it opens its own network, nothing else
	if err := through(ownerKey, laneA, m1.Address+":22"); err != nil {
		t.Fatalf("the control — a connection opened through the gateway into its network: %v", err)
	}
	for what, to := range map[string]string{
		"the lane's own node":            "198.51.100.1:22",
		"the other network's gateway":    laneB + ":22",
		"a machine of the other network": m3.Address + ":22",
		"the gateway itself":             "127.0.0.1:22",
		"the gateway's network address":  a.Gateway + ":22",
		"the web":                        "151.101.2.132:80", // no-environment: ok
	} {
		if err := through(ownerKey, laneA, to); err == nil {
			t.Fatalf("the jump opened %s (%s)", what, to)
		}
	}
	if refused := b.must(t, "pct exec "+gwA+" -- nft list chain inet gateway output | sed -n 's/.*counter packets \\([0-9]*\\).*/\\1/p'"); refused == "" || refused == "0" {
		t.Fatalf("the gateway's own count of what it refused a jump: %q", refused)
	}
	// the VM, by the same jump, as its image's own user: at the address its
	// disc told it, out through its gateway
	var inVM string
	eventually(t, "the owner's key jumps into the VM", 5*time.Minute, func() bool {
		out, err := b.ssh(t, ssh+"-i "+ownerKey+" -o ProxyCommand=\""+ssh+"-i "+ownerKey+" -W %h:%p jump@"+laneA+"\" debian@"+vm.Address+
			" 'ip -4 -o addr show; ip route show default; resolvectl dns; curl -sI -m 10 http://deb.debian.org | head -1'")
		inVM = out
		return err == nil
	})
	t.Logf("a VM on a network answered its owner's jump %s after its create began", time.Since(vmBegan).Round(time.Second))
	for _, want := range []string{"inet " + vm.Address + "/27", "default via " + a.Gateway, "198.51.100.1", "200 OK"} {
		if !strings.Contains(inVM, want) {
			t.Errorf("inside the VM, no %q:\n%s", want, inVM)
		}
	}
	// a network that names no key lets nobody jump
	if out, err := jump(ownerKey, laneB, m3.Address, "hostname"); err == nil || !strings.Contains(out, "Permission denied") {
		t.Fatalf("a jump into a network that names no key: %q %v", out, err)
	}

	// ---- two keys, kept apart: each token holds what it needs and no more
	gw := md.gateway(ia)
	if err := md.c.call(ctx, http.MethodPut, gw.path()+"/config", url.Values{"memory": {"256"}}, nil); err == nil {
		t.Fatal("the machines' token changed a gateway's config: it holds its power, nothing of its config")
	}
	if _, err := nd.SetPower(ctx, m1.ID, false); err == nil {
		t.Fatal("the networks' token stopped a machine")
	}

	// ---- its keys changed: the gateway is made again with them, and back at work
	began = time.Now()
	after, fixed, err := nd.TendNetwork(ctx, driver.NetworkSpec{ID: netA, Label: "bench network", JumpKeys: []string{ownerPub, otherPub}}, a.EngineRef)
	if err != nil || len(fixed) == 0 || fixed[0] != "gateway (made again: its keys changed)" {
		t.Fatalf("its keys changed: %v %v", fixed, err)
	}
	t.Logf("a gateway was made again in %s — its machines' way out cut that long", time.Since(began).Round(time.Second))
	if after.EngineRef != a.EngineRef || after.Range != a.Range || after.Keys != 2 || !after.Running || after.Wall != driver.WallExact {
		t.Fatalf("after: %+v", after)
	}
	// (through the gateway: the machine itself lets in the key it was born with)
	if err := through(otherKey, laneB, m3.Address+":22"); err == nil {
		t.Fatal("the control: that key passes a gateway that was not given it")
	}
	eventually(t, "the second key passes the gateway made again", time.Minute, func() bool {
		return through(otherKey, laneA, m1.Address+":22") == nil
	})
	if out, err := jump(ownerKey, laneA, m1.Address, "hostname"); err != nil || out != "net-a-one" {
		t.Fatalf("the first key, through the gateway made again: %q %v", out, err)
	}
	if again, fixed, err := nd.TendNetwork(ctx, driver.NetworkSpec{ID: netA, Label: "bench network", JumpKeys: []string{otherPub, ownerPub}}, a.EngineRef); err != nil || len(fixed) != 0 || !again.Running {
		t.Fatalf("a second look: %v %v", fixed, err)
	}
	eventually(t, "a machine is out again through the gateway made again", 30*time.Second, func() bool { return pings(v1, "198.51.100.1") })

	// ---- it runs while a machine of its network does
	// (the VM first: deleted while the others run, it takes no gateway with it)
	if err := md.DeleteGuest(ctx, vmID); err != nil {
		t.Fatal(err)
	}
	if status(gwA) != "status: running" {
		t.Fatal("a machine deleted while others of its network run, and the gateway went with it")
	}
	if _, err := md.SetPower(ctx, m1.ID, false); err != nil {
		t.Fatal(err)
	}
	if status(gwA) != "status: running" {
		t.Fatal("one of two machines stopped, and the gateway with it")
	}
	if _, err := md.SetPower(ctx, m2.ID, false); err != nil {
		t.Fatal(err)
	}
	if status(gwA) != "status: stopped" || status(gwB) != "status: running" {
		t.Fatalf("its last machine stopped: its gateway %s, the other network's %s", status(gwA), status(gwB))
	}
	if n, err := nd.Network(ctx, netA); err != nil || n.Running {
		t.Fatalf("the network reads %+v %v", n, err)
	}
	began = time.Now()
	if _, err := md.SetPower(ctx, m1.ID, true); err != nil {
		t.Fatal(err)
	}
	t.Logf("a machine started with its network's gateway before it, in %s", time.Since(began).Round(100*time.Millisecond))
	if status(gwA) != "status: running" {
		t.Fatal("a machine started, and its gateway did not")
	}
	eventually(t, "started again, a machine is out through its gateway", 30*time.Second, func() bool { return pings(v1, "198.51.100.1") })
	// a hand stops the gateway under a running machine: the look starts it
	b.must(t, "pct stop "+gwA)
	if did, err := md.WayOut(ctx, m1.ID); err != nil || did != "started" || status(gwA) != "status: running" {
		t.Fatalf("a gateway stopped under a running machine: %q %v, %s", did, err, status(gwA))
	}
	// and one left running with nothing behind it is put to rest
	b.must(t, "pct stop "+v1)
	if did, err := md.WayOut(ctx, m1.ID); err != nil || did != "stopped" || status(gwA) != "status: stopped" {
		t.Fatalf("a gateway left running with no machine: %q %v, %s", did, err, status(gwA))
	}

	// ---- a network a machine stands on is not deleted
	if err := nd.DeleteNetwork(ctx, netA); !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), m1.ID) || !strings.Contains(err.Error(), m2.ID) {
		t.Fatalf("deleted under its machines: %v", err)
	}
	for _, id := range []string{m1.ID, m2.ID} {
		if err := md.DeleteGuest(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := nd.DeleteNetwork(ctx, netA); err != nil {
		t.Fatal(err)
	}
	if out, err := b.ssh(t, "test ! -e /etc/pve/firewall/"+gwA+".fw && ! pct status "+gwA+" 2>/dev/null"); err != nil {
		t.Fatalf("its gateway, or its firewall file, stayed: %s", out)
	}
	_ = v2 // the others go with the test's own cleanup
}
