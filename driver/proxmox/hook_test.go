package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNode is a node's guests as maps, recording what the hook did.
type fakeNode struct {
	mu     sync.Mutex
	guests map[string]*NodeGuest
	did    []string
}

func (n *fakeNode) Guest(vmid string) (NodeGuest, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	g, ok := n.guests[vmid]
	if !ok {
		return NodeGuest{}, fmt.Errorf("no guest %s", vmid)
	}
	return *g, nil
}

func (n *fakeNode) Guests() ([]NodeGuest, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []NodeGuest
	for _, g := range n.guests {
		out = append(out, *g)
	}
	return out, nil
}

func (n *fakeNode) Shutdown(g NodeGuest, _ time.Duration) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.guests[g.VMID].Running = false
	n.did = append(n.did, "shutdown "+g.VMID)
	return nil
}

func (n *fakeNode) Set(g NodeGuest, changes map[string]string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	cfg := n.guests[g.VMID].Config
	for k, v := range changes {
		if v == "" {
			delete(cfg, k)
		} else {
			cfg[k] = v
		}
		n.did = append(n.did, "set "+g.VMID+" "+k+"="+v)
	}
	return nil
}

func guest(vmid, kind string, running bool, cfg ...string) *NodeGuest {
	g := &NodeGuest{VMID: vmid, Kind: kind, Running: running, Config: map[string]string{}}
	for i := 0; i+1 < len(cfg); i += 2 {
		g.Config[cfg[i]] = cfg[i+1]
	}
	return g
}

// A node with a priority guest (4100) and four machines: spot, a floor,
// guaranteed with a cap, guaranteed without.
func newNode() *fakeNode {
	return &fakeNode{guests: map[string]*NodeGuest{
		"4100":  guest("4100", "qemu", false, "memory", "32768"),
		"11000": guest("11000", "qemu", true, "memory", "2048", "tags", "admitted.2048;class.spot;hangar-id.m-00000000000000001"),
		"11001": guest("11001", "lxc", true, "memory", "3072", "tags", "admitted.3072;beside.1;class.guaranteed+spot;floor.1024;hangar-id.m-00000000000000002"),
		"11002": guest("11002", "lxc", true, "memory", "1024", "tags", "admitted.1024;beside.1;class.guaranteed;hangar-id.m-00000000000000003"),
		"11003": guest("11003", "lxc", true, "memory", "1024", "tags", "admitted.1024;class.guaranteed;hangar-id.m-00000000000000004"),
	}}
}

type brain struct {
	*httptest.Server
	mu    sync.Mutex
	calls []string
	code  int
}

func newBrain(t *testing.T) *brain {
	b := &brain{code: 200}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		b.mu.Lock()
		b.calls = append(b.calls, r.Header.Get("Authorization")+" "+r.URL.Path+" "+in["guest"])
		code := b.code
		b.mu.Unlock()
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"reservation": "priority", "detail": "said the brain",
			"resources": []map[string]string{{"resource": "m-00000000000000001", "result": "repaired", "detail": "held for 4100: [power]"}}})
	}))
	t.Cleanup(b.Close)
	return b
}

func newHook(t *testing.T, node Node, brainURL string) (*Hook, *[]string) {
	t.Helper()
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	if err := os.WriteFile(tok, []byte("hgr_room\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	var mu sync.Mutex
	return &Hook{
		Cfg: HookConfig{Brain: brainURL, TokenFile: tok, Zone: "lab", Timeout: Duration(2 * time.Second),
			ShutdownTimeout: Duration(time.Second), Grace: Duration(10 * time.Minute), RunDir: filepath.Join(dir, "run")},
		Node: node,
		Log: func(f string, a ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, fmt.Sprintf(f, a...))
		},
	}, &logs
}

func TestThePriorityGuestPhonesTheBrainFirst(t *testing.T) {
	node, b := newNode(), newBrain(t)
	h, logs := newHook(t, node, b.URL)
	if err := h.Run(context.Background(), "4100", "pre-start"); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 1 || b.calls[0] != "Bearer hgr_room /v1/zones/lab/claim 4100" {
		t.Fatalf("the brain heard %v", b.calls)
	}
	if len(node.did) != 0 {
		t.Fatalf("the brain answered, and the node acted too: %v", node.did)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "m-00000000000000001 repaired (held for 4100: [power])") {
		t.Fatalf("what the brain did is not in the log: %v", *logs)
	}
	if _, err := os.Stat(filepath.Join(h.Cfg.RunDir, "4100")); err != nil {
		t.Fatal("no marker while the priority guest starts")
	}
	if err := h.Run(context.Background(), "4100", "post-stop"); err != nil || b.calls[1] != "Bearer hgr_room /v1/zones/lab/release 4100" {
		t.Fatalf("%v %v", err, b.calls)
	}
	if _, err := os.Stat(filepath.Join(h.Cfg.RunDir, "4100")); !os.IsNotExist(err) {
		t.Fatal("the marker outlived its guest")
	}
	// the brain keeps no room for this guest: the operator's word, nothing held
	b.code = 404
	_ = h.Run(context.Background(), "4100", "pre-start")
	if len(node.did) != 0 {
		t.Fatalf("a guest the brain keeps no room for held the node: %v", node.did)
	}
}

// The brain out of reach: the node does what the brain would, from the tags
// — the tag first, spot stopped, the floor shrunk as far as its use allows,
// caps — and gives it back after, never refusing the priority guest.
func TestTheNodeActsAloneWhenTheBrainCannotBeReached(t *testing.T) {
	for name, url := range map[string]string{"unreachable": "http://127.0.0.1:1", "refusing the token": ""} {
		t.Run(name, func(t *testing.T) {
			node := newNode()
			node.guests["11001"].MemoryUsedMB = 1500
			if url == "" {
				b := newBrain(t)
				b.code = 401
				url = b.URL
			}
			h, logs := newHook(t, node, url)
			if err := h.Run(context.Background(), "4100", "pre-start"); err != nil {
				t.Fatalf("a priority guest's start refused: %v", err)
			}
			g := node.guests
			if g["11000"].Running || !slices.Contains(g["11000"].Tags(), "held.4100") {
				t.Errorf("spot: %+v", g["11000"])
			}
			if g["11001"].Config["memory"] != "1564" || g["11001"].Config["cpulimit"] != "1" || !slices.Contains(g["11001"].Tags(), "held.4100") {
				t.Errorf("the floor: %+v", g["11001"].Config)
			}
			if g["11002"].Config["cpulimit"] != "1" || g["11002"].Config["memory"] != "1024" {
				t.Errorf("guaranteed with a cap: %+v", g["11002"].Config)
			}
			if _, capped := g["11003"].Config["cpulimit"]; capped || slices.Contains(g["11003"].Tags(), "held.4100") {
				t.Errorf("guaranteed, no cap, was touched: %+v", g["11003"].Config)
			}
			// the tag leads: written before the stop
			if i, j := slices.Index(node.did, "set 11000 tags=admitted.2048;class.spot;hangar-id.m-00000000000000001;held.4100"), slices.Index(node.did, "shutdown 11000"); i < 0 || j < i {
				t.Errorf("the order: %v", node.did)
			}
			if !strings.Contains(strings.Join(*logs, "\n"), "holds 1500 MB, above its floor of 1024 MB: 540 MB stay lent") {
				t.Errorf("the shortfall is not said: %v", *logs)
			}

			node.did = nil
			if err := h.Run(context.Background(), "4100", "post-stop"); err != nil {
				t.Fatal(err)
			}
			if g["11001"].Config["memory"] != "3072" || g["11001"].Config["cpulimit"] != "" || slices.Contains(g["11001"].Tags(), "held.4100") {
				t.Errorf("the floor given back: %+v", g["11001"].Config)
			}
			if g["11002"].Config["cpulimit"] != "" || g["11000"].Running || slices.Contains(g["11000"].Tags(), "held.4100") {
				t.Errorf("given back: %+v %+v", g["11002"].Config, g["11000"])
			}
		})
	}
}

// Held by two, given back by one: what the other holds stays held.
func TestAHoldOfAnotherStays(t *testing.T) {
	node := newNode()
	node.guests["11001"].Config["tags"] += ";held.4100;held.4101"
	node.guests["11001"].Config["memory"], node.guests["11001"].Config["cpulimit"] = "1024", "1"
	h, _ := newHook(t, node, "http://127.0.0.1:1")
	_ = h.Run(context.Background(), "4100", "post-stop")
	c := node.guests["11001"].Config
	if c["memory"] != "1024" || c["cpulimit"] != "1" || !strings.HasSuffix(c["tags"], "held.4101") || strings.Contains(c["tags"], "held.4100") {
		t.Fatalf("%+v", c)
	}
}

// The node's own admission: whoever starts a machine.
func TestTheNodeAdmitsWhoeverStarts(t *testing.T) {
	node := newNode()
	for _, g := range node.guests {
		g.Running = false
	}
	h, _ := newHook(t, node, "http://127.0.0.1:1")
	start := func(vmid string) error { return h.Run(context.Background(), vmid, "pre-start") }
	for _, vmid := range []string{"11000", "11001", "11002"} {
		if err := start(vmid); err != nil {
			t.Fatalf("%s refused with nothing holding the room: %v", vmid, err)
		}
	}
	// set by hand above what the brain admitted
	node.guests["11002"].Config["memory"] = "4096"
	if err := start("11002"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "set to 4096 MB, and was admitted at 1024 MB") {
		t.Fatalf("an oversized start: %v", err)
	}
	node.guests["11002"].Config["memory"] = "1024"

	// the priority guest starting: its marker is fresh
	h.mark("4100")
	if err := start("11000"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "is spot, and guest 4100 of this node has its room") {
		t.Fatalf("a spot start beside it: %v", err)
	}
	if err := start("11001"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "starts on its floor, 1024 MB") {
		t.Fatalf("a floor's start above its floor: %v", err)
	}
	node.guests["11001"].Config["memory"] = "1024"
	if err := start("11001"); err != nil {
		t.Fatalf("a floor on its floor: %v", err)
	}
	if err := start("11002"); err != nil {
		t.Fatalf("guaranteed: %v", err)
	}
	// an old marker counts only while its guest runs
	old := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	_ = os.WriteFile(filepath.Join(h.Cfg.RunDir, "4100"), []byte(old), 0o644)
	node.guests["4100"].Running = true
	if err := start("11000"); !errors.Is(err, ErrRefused) {
		t.Fatalf("spot beside a running priority guest: %v", err)
	}
	node.guests["4100"].Running = false
	if err := start("11000"); err != nil {
		t.Fatalf("a stale marker refused: %v", err)
	}
}

// The node as Proxmox VE lays it out: configs under /etc/pve, power from
// the cgroups, changes through qm and pct.
func TestTheLocalNode(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("/etc/pve/nodes/pve-a/lxc/11001.conf", "#made by hangar%3A m-1\narch: amd64\nmemory: 3072\ntags: class.spot;hangar-id.m-1\n\n[snap]\nmemory: 512\n")
	write("/etc/pve/nodes/pve-a/qemu-server/4100.conf", "memory: 32768\nhookscript: local:snippets/hangar-hook\n")
	write("/sys/fs/cgroup/lxc/11001/memory.current", "734003200\n")
	var ran []string
	n := &LocalNode{Name: "pve-a", Root: root, Run: func(name string, a ...string) ([]byte, error) {
		ran = append(ran, strings.TrimPrefix(name, root)+" "+strings.Join(a, " "))
		return nil, nil
	}}
	g, err := n.Guest("11001")
	if err != nil || g.Kind != "lxc" || !g.Running || g.MemoryUsedMB != 700 || g.Config["memory"] != "3072" || g.Tag("class") != "spot" {
		t.Fatalf("%+v %v", g, err)
	}
	p, _ := n.Guest("4100")
	if p.Kind != "qemu" || p.Running || p.Product() {
		t.Fatalf("%+v", p)
	}
	if gs, _ := n.Guests(); len(gs) != 2 {
		t.Fatalf("%v", gs)
	}
	_ = n.Set(g, map[string]string{"memory": "1024", "cpulimit": "1", "tags": "a;b"})
	_ = n.Set(g, map[string]string{"cpulimit": ""})
	_ = n.Shutdown(p, 30*time.Second)
	want := []string{
		"/usr/sbin/pct set 11001 --cpulimit 1 --memory 1024 --tags a;b",
		"/usr/sbin/pct set 11001 --delete cpulimit",
		"/usr/sbin/qm shutdown 4100 --timeout 30 --forceStop 1",
	}
	if !slices.Equal(ran, want) {
		t.Fatalf("ran %q", ran)
	}
}
