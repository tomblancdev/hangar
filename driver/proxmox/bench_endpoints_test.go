package proxmox

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/internal/ids"
)

// Endpoints on a real Proxmox VE, through the networks plugin's own fenced
// token, with a gateway born from recipe 2: a network of a zone that
// publishes services reaches its endpoint on the operator's side and nothing
// else of it — another port of the same address and the node's ssh refused,
// and counted —, the zone's resolver and the web answering; a change written
// to its running gateway, which is started again (its way out cut for
// seconds), the new endpoint reached then; a line its gateway cannot read
// leaves its network with no way out, never with an open one, and the next
// look writes it again; the field removed, everything the lane lets out
// reached, as before; and its owner's key still jumps into it. Without the
// bench's variables it skips.
func TestBenchAnEndpoint(t *testing.T) {
	b := onBench(t)
	zone := netBench(b, t, 11080)
	nd := b.openWith(t, os.Getenv("HANGAR_BENCH_NETWORKS_TOKEN_FILE"), "11080-11099", nil, zone)
	md := b.openWith(t, os.Getenv("HANGAR_BENCH_MACHINES_NETS_TOKEN_FILE"), "11080-11099", []string{"100"}, zone)
	if !has(nd.Capabilities(), driver.NetEndpoints) {
		t.Fatalf("the bench's zone carries no endpoints: %s", nd.EndpointsReport())
	}
	ctx := context.Background()
	archive := "local:vztmpl/" + b.must(t, "ls /var/lib/vz/template/cache/ | grep \"^debian-13-standard_.*_$(dpkg --print-architecture)\\.\" | sort -V | tail -1")
	ownerPub, ownerKey := b.guestKey(t, "ep-owner")
	in := func(v, cmd string) (string, error) {
		return b.ssh(t, "pct exec "+v+" -- bash -c '"+strings.ReplaceAll(cmd, "'", `'\''`)+"'")
	}
	reaches := func(v, host string, port int) bool {
		_, err := in(v, "timeout 3 bash -c 'exec 3<>/dev/tcp/"+host+"/"+strconv.Itoa(port)+"'")
		return err == nil
	}
	web := func(v string) bool {
		out, err := in(v, `exec 3<>/dev/tcp/deb.debian.org/80 && printf 'HEAD / HTTP/1.0\r\nHost: deb.debian.org\r\n\r\n' >&3 && timeout 5 head -1 <&3`)
		return err == nil && strings.Contains(out, " 200 ")
	}
	resolves := func(v string) bool { _, err := in(v, "timeout 5 getent hosts deb.debian.org"); return err == nil }

	// the operator's side: two doors at the lane's node, one published
	b.must(t, "for p in 3101 3102; do (cd /tmp && setsid python3 -m http.server $p --bind 198.51.100.1 </dev/null >/dev/null 2>&1 &); done; sleep 1; true")
	t.Cleanup(func() { _, _ = b.ssh(t, "pkill -f 'http.server 310[12]' ; true") })
	side := []string{"198.51.100.0/24"} // the bench's lane: its node, its resolver, its doors
	one := &driver.Endpoints{To: []string{"tcp:198.51.100.1:3101"}, Private: side}

	id := ids.New("net")
	t.Cleanup(func() { _ = nd.DeleteNetwork(context.Background(), id) })
	began := time.Now()
	n, err := nd.CreateNetwork(ctx, driver.NetworkSpec{ID: id, Label: "bench endpoints", JumpKeys: []string{ownerPub}, Endpoints: one})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("a network carrying an endpoint was made in %s", time.Since(began).Round(100*time.Millisecond))
	gw := n.EngineRef[strings.LastIndex(n.EngineRef, "/")+1:]
	if !slices.Equal(n.Endpoints, one.To) || n.Lane == "" {
		t.Fatalf("made as %+v", n)
	}
	if conf := b.must(t, "grep -a '^lxc.environment.runtime' /etc/pve/lxc/"+gw+".conf"); !strings.Contains(conf, "HANGAR_ENDPOINTS=tcp:198.51.100.1:3101") ||
		!strings.Contains(conf, "HANGAR_PRIVATE=198.51.100.0/24") || !strings.Contains(conf, "HANGAR_RESOLVERS=198.51.100.1") {
		t.Fatalf("what Proxmox keeps:\n%s", conf)
	}

	mid := ids.New("m")
	t.Cleanup(func() { _ = md.DeleteGuest(context.Background(), mid) })
	m, err := md.CreateGuest(ctx, driver.GuestSpec{ID: mid, Kind: "container", Name: "ep-one", Cores: 1, MemoryMB: 256, DiskGB: 2,
		Image: archive, SSHKeys: []string{ownerPub}, Network: id, Tags: map[string]string{"class": "spot"}})
	if err != nil {
		t.Fatal(err)
	}
	v := vmid(m)
	eventually(t, "its gateway's unit has read its endpoints", time.Minute, func() bool {
		out, _ := b.ssh(t, "pct exec "+gw+" -- systemctl is-active hangar-endpoints")
		return out == "active"
	})
	if env := b.must(t, "pct exec "+gw+" -- sh -c \"tr '\\\\0' '\\\\n' </proc/1/environ | grep ^HANGAR_\""); !strings.Contains(env, "HANGAR_ENDPOINTS=tcp:198.51.100.1:3101") {
		t.Fatalf("its first process's environment:\n%s", env)
	}
	eventually(t, "a machine reaches its endpoint", 30*time.Second, func() bool { return reaches(v, "198.51.100.1", 3101) })
	if reaches(v, "198.51.100.1", 3102) || reaches(v, "198.51.100.1", 22) {
		t.Fatal("another door of the operator's side was reached with no endpoint")
	}
	if !resolves(v) || !web(v) {
		t.Fatalf("the resolver %v, the web %v", resolves(v), web(v))
	}
	counters := b.must(t, "pct exec "+gw+" -- nft list chain inet gateway forward")
	if !strings.Contains(counters, `comment "refused: the private side, no endpoint"`) || strings.Contains(refusedLine(counters), "packets 0 ") {
		t.Fatalf("its count of what it refused:\n%s", counters)
	}

	// a change: a second endpoint, written to it while it runs — started
	// again, a connection to the first tried every 0.2 s across it
	loop := time.Now()
	b.must(t, "setsid pct exec "+v+" -- bash -c 'end=$((SECONDS+45)); while [ $SECONDS -lt $end ]; do if timeout 0.5 bash -c \"exec 3<>/dev/tcp/198.51.100.1/3101\" 2>/dev/null; then echo \"$(date +%s.%N) ok\"; else echo \"$(date +%s.%N) cut\"; fi; sleep 0.2; done' </dev/null >/root/ep-cut.log 2>&1 & sleep 2; true")
	two := &driver.Endpoints{To: []string{"tcp:198.51.100.1:3101", "tcp:198.51.100.1:3102"}, Private: side}
	began = time.Now()
	got, fixed, err := nd.TendNetwork(ctx, driver.NetworkSpec{ID: id, Label: "bench endpoints", JumpKeys: []string{ownerPub}, Endpoints: two}, n.EngineRef)
	if err != nil || !slices.Equal(fixed, []string{"endpoints"}) || !slices.Equal(got.Endpoints, two.To) {
		t.Fatalf("a change: %v %v %+v", fixed, err, got)
	}
	t.Logf("a change written and its gateway started again in %s", time.Since(began).Round(100*time.Millisecond))
	eventually(t, "the second endpoint reached", time.Minute, func() bool { return reaches(v, "198.51.100.1", 3102) })
	if reaches(v, "198.51.100.1", 22) {
		t.Fatal("the node's ssh reached after the change")
	}
	time.Sleep(time.Until(loop.Add(48 * time.Second))) // the loop's own end
	cut := b.must(t, "awk '$2==\"cut\" && !a {a=$1} $2==\"ok\" && a && !c {c=$1} END {if (a) printf \"%.1f\", c-a; else print \"none\"}' /root/ep-cut.log")
	t.Logf("its network's way out cut %s s by the change", cut)
	if cut == "none" {
		t.Fatal("a running gateway's change cut nothing: was it started again?")
	}
	// as it should be: a look writes nothing
	if _, fixed, err := nd.TendNetwork(ctx, driver.NetworkSpec{ID: id, Label: "bench endpoints", JumpKeys: []string{ownerPub}, Endpoints: two}, n.EngineRef); err != nil || len(fixed) != 0 {
		t.Fatalf("a look at a gateway that stands: %v %v", fixed, err)
	}

	// its owner's key still jumps in, and nowhere else
	ssh := "ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=6 "
	eventually(t, "the owner's key jumps into its network", time.Minute, func() bool {
		_, err := b.ssh(t, ssh+"-i "+ownerKey+" -W "+m.Address+":22 jump@"+n.Lane+" </dev/null")
		return err == nil
	})
	if _, err := b.ssh(t, ssh+"-i "+ownerKey+" -W 198.51.100.1:3101 jump@"+n.Lane+" </dev/null"); err == nil {
		t.Fatal("the jump opened the operator's side")
	}

	// a line it cannot read, written by the networks' own token and the
	// gateway started again: forwarding stays off — closed, never open
	gwr := resource{VMID: atoi(gw), Node: nd.node, Type: "lxc", Pool: nd.nets.pool}
	bad := "HANGAR_ENDPOINTS=tcp:999.1.1.1:80\x00HANGAR_PRIVATE=198.51.100.0/24" // no-environment: ok — an address of nobody's, the line the unit must refuse
	if err := nd.c.call(ctx, http.MethodPut, gwr.path()+"/config", url.Values{"env": {bad}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := nd.c.run(ctx, http.MethodPost, gwr.path()+"/status/reboot", url.Values{"timeout": {"30"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "its unit has refused the line", time.Minute, func() bool {
		out, _ := b.ssh(t, "pct exec "+gw+" -- systemctl is-failed hangar-endpoints")
		return out == "failed"
	})
	if fwd := b.must(t, "pct exec "+gw+" -- sysctl -n net.ipv4.ip_forward"); fwd != "0" || web(v) || reaches(v, "198.51.100.1", 3101) {
		t.Fatalf("a line it cannot read: forwarding %s, the web %v", fwd, web(v))
	}
	said := b.must(t, "pct exec "+gw+" -- journalctl -u hangar-endpoints -o cat --no-pager -b | grep hangar-endpoints: | tail -1")
	t.Logf("its unit said: %s", said)
	// the next look writes it again
	if _, fixed, err := nd.TendNetwork(ctx, driver.NetworkSpec{ID: id, Label: "bench endpoints", JumpKeys: []string{ownerPub}, Endpoints: two}, n.EngineRef); err != nil || !slices.Equal(fixed, []string{"endpoints"}) {
		t.Fatalf("a look after a hand's line: %v %v", fixed, err)
	}
	eventually(t, "its way out back", time.Minute, func() bool { return reaches(v, "198.51.100.1", 3101) && web(v) })

	// the field removed — a zone that publishes nothing: as recipe 1
	got, fixed, err = nd.TendNetwork(ctx, driver.NetworkSpec{ID: id, Label: "bench endpoints", JumpKeys: []string{ownerPub}}, n.EngineRef)
	if err != nil || !slices.Equal(fixed, []string{"endpoints"}) || len(got.Endpoints) != 0 {
		t.Fatalf("the field removed: %v %v %+v", fixed, err, got)
	}
	if conf, _ := b.ssh(t, "grep -a '^lxc.environment.runtime' /etc/pve/lxc/"+gw+".conf"); conf != "" {
		t.Fatalf("its config still carries:\n%s", conf)
	}
	eventually(t, "everything the lane lets out reached", time.Minute, func() bool {
		return reaches(v, "198.51.100.1", 3102) && reaches(v, "198.51.100.1", 22) && web(v)
	})
}

// refusedLine is the private side's refusal, with its counter.
func refusedLine(chain string) string {
	for _, l := range strings.Split(chain, "\n") {
		if strings.Contains(l, "refused: the private side") {
			return l
		}
	}
	return ""
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
