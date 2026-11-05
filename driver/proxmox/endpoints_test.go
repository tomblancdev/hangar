package proxmox

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tomblancdev/hangar/driver"
)

// epZone is a zone whose gateways carry endpoints: an archive named after
// recipe 2.
var epZone = with(netZone, "net_archive", "local:vztmpl/hangar-gateway-2.tar.zst")

// What makes a zone carry endpoints: Proxmox VE 9.1 or later, and its
// gateways' archive named after a recipe that reads them — and it says why
// not, in words, where it does not.
func TestAZoneCarriesEndpointsWhereItsGatewaysReadThem(t *testing.T) {
	for archive, want := range map[string]int{
		"local:vztmpl/hangar-gateway-2.tar.zst": 2, "local:vztmpl/hangar-gateway-12.tar.zst": 12,
		"local:vztmpl/hangar-gateway-2-draft.tar.zst": 2, "local:vztmpl/hangar-gateway-1.tar.zst": 1,
		"local:vztmpl/gateway.tar.zst": 0, "hangar-gateway-3.tar.gz": 3, "local:vztmpl/my-hangar-gateway-4.tar.zst": 0,
	} {
		if got := recipeOf(archive); got != want {
			t.Errorf("%s: recipe %d, want %d", archive, got, want)
		}
	}
	for v, want := range map[string]bool{"9.1": true, "9.2.25": true, "10.0.1": true, "9.0.11": false, "8.4": false, "nine": false} {
		if atLeast(v, 9, 1) != want {
			t.Errorf("%s: %v", v, !want)
		}
	}
	if d := open(t, newAPI(), epZone); !has(d.Capabilities(), driver.NetEndpoints) || d.EndpointsReport() != "" {
		t.Fatalf("recipe 2 on 9.2: %v %q", d.Capabilities(), d.EndpointsReport())
	}
	if d := open(t, newAPI(), netZone); has(d.Capabilities(), driver.NetEndpoints) || !strings.Contains(d.EndpointsReport(), "hangar-gateway-1.tar.zst is not named after a recipe that reads endpoints") {
		t.Fatalf("recipe 1: %v %q", d.Capabilities(), d.EndpointsReport())
	}
	old := newAPI()
	old.h["GET /version"] = func(w http.ResponseWriter, _ *http.Request) { data(w, map[string]string{"version": "9.0.11"}) }
	if d := open(t, old, epZone); has(d.Capabilities(), driver.NetEndpoints) || !strings.Contains(d.EndpointsReport(), "its Proxmox VE is 9.0.11") {
		t.Fatalf("9.0: %v %q", d.Capabilities(), d.EndpointsReport())
	}
	if d := open(t, newAPI(), walledZone); has(d.Capabilities(), driver.NetEndpoints) || d.EndpointsReport() != "" {
		t.Fatal("a zone that cuts no networks says nothing of endpoints")
	}
}

// envNode keeps a gateway's env as Proxmox does: written to a stopped
// container at once; to a running one, PENDING until its next start.
type envNode struct {
	*node
	emu     sync.Mutex
	running map[int]string // what it runs with ("" none)
	pending map[int]*string
	puts    []string // "env 11200" / "delete 11200"
}

func newEnvNode(a *api) *envNode {
	e := &envNode{node: newCluster(a), running: map[int]string{}, pending: map[int]*string{}}
	create := a.h["POST /nodes/node-a/lxc"]
	a.h["POST /nodes/node-a/lxc"] = func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		vmid, _ := strconv.Atoi(r.PostForm.Get("vmid"))
		e.emu.Lock()
		e.running[vmid], e.pending[vmid] = r.PostForm.Get("env"), nil
		e.emu.Unlock()
		create(w, r)
	}
	inner := a.any
	a.any = func(w http.ResponseWriter, r *http.Request, p string) bool {
		rest, ok := strings.CutPrefix(p, "/nodes/node-a/lxc/")
		if !ok {
			return inner(w, r, p)
		}
		id, what, _ := strings.Cut(rest, "/")
		vmid, _ := strconv.Atoi(id)
		e.mu.Lock()
		on := e.on[vmid]
		e.mu.Unlock()
		switch {
		case what == "pending" && r.Method == http.MethodGet:
			e.emu.Lock()
			defer e.emu.Unlock()
			rows := []map[string]any{}
			if cur, p := e.running[vmid], e.pending[vmid]; cur != "" || p != nil {
				row := map[string]any{"key": "env"}
				if cur != "" {
					row["value"] = cur
				}
				switch {
				case p != nil && *p == "":
					row["delete"] = 1
				case p != nil:
					row["pending"] = *p
				}
				rows = append(rows, row)
			}
			data(w, rows)
			return true
		case what == "config" && r.Method == http.MethodPut:
			_ = r.ParseForm()
			env, del := r.PostForm.Get("env"), r.PostForm.Get("delete") == "env"
			if env == "" && !del {
				return inner(w, r, p)
			}
			e.emu.Lock()
			e.puts = append(e.puts, map[bool]string{true: "delete ", false: "env "}[del]+id)
			if on {
				e.pending[vmid] = &env
			} else {
				e.running[vmid], e.pending[vmid] = env, nil
			}
			e.emu.Unlock()
			data(w, nil)
			return true
		case what == "status/reboot" || what == "status/start":
			e.emu.Lock()
			if p := e.pending[vmid]; p != nil {
				e.running[vmid], e.pending[vmid] = *p, nil
			}
			e.emu.Unlock()
			if what == "status/reboot" {
				e.mu.Lock()
				e.acts = append(e.acts, "reboot "+id)
				e.mu.Unlock()
				data(w, a.task("OK", ""))
				return true
			}
		}
		return inner(w, r, p)
	}
	return e
}

func (e *envNode) wrote() []string {
	e.emu.Lock()
	defer e.emu.Unlock()
	out := e.puts
	e.puts = nil
	return out
}

func (e *envNode) env(vmid int) (running string, pending *string) {
	e.emu.Lock()
	defer e.emu.Unlock()
	return e.running[vmid], e.pending[vmid]
}

const logs, syslog = "tcp:192.0.2.35:3101", "udp:192.0.2.35:514"

// A gateway is born with its endpoints in its env; one written to it while it
// runs waits for its next start, so it is started again — and a gateway
// whose env is as it should be is left alone, written nothing, started
// nothing.
func TestAGatewayCarriesItsEndpoints(t *testing.T) {
	a := newAPI()
	n := newEnvNode(a)
	d := open(t, a, epZone)
	ctx := context.Background()
	spec := driver.NetworkSpec{ID: "net-a", Label: "lab", Endpoints: &driver.Endpoints{To: []string{logs}, Private: []string{"192.0.2.0/24"}}}
	got, err := d.CreateNetwork(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	born := "HANGAR_ENDPOINTS=" + logs + "\x00HANGAR_PRIVATE=192.0.2.0/24\x00HANGAR_RESOLVERS=198.51.100.1"
	if form := n.forms[11200]["env"]; form != born {
		t.Fatalf("born with env %q, want %q", form, born)
	}
	if !slices.Equal(got.Endpoints, []string{logs}) || got.Lane != "198.51.100.200" {
		t.Fatalf("%+v", got)
	}
	n.did()
	// it runs: a second endpoint is written, and it is started again
	n.mu.Lock()
	n.on[11200] = true
	n.mu.Unlock()
	spec.Endpoints.To = []string{logs, syslog}
	got, fixed, err := d.TendNetwork(ctx, spec, got.EngineRef)
	if err != nil || !slices.Equal(fixed, []string{"endpoints"}) {
		t.Fatalf("%v %v", fixed, err)
	}
	if acts := n.did(); !slices.Equal(acts, []string{"reboot 11200"}) {
		t.Fatalf("acts %v", acts)
	}
	if !slices.Equal(got.Endpoints, []string{logs, syslog}) {
		t.Fatalf("it runs with %v", got.Endpoints)
	}
	if run, pend := n.env(11200); pend != nil || !strings.Contains(run, logs+" "+syslog) {
		t.Fatalf("%q %v", run, pend)
	}
	n.wrote()
	// as it should be: nothing
	if _, fixed, err := d.TendNetwork(ctx, spec, got.EngineRef); err != nil || len(fixed) != 0 || len(n.wrote()) != 0 || len(n.did()) != 0 {
		t.Fatalf("%v %v", fixed, err)
	}
	// written, and its restart never came (the brain stopped between the
	// two): started again, nothing written
	pend := strings.Replace(born, logs, logs+" "+syslog, 1)
	n.emu.Lock()
	n.running[11200], n.pending[11200] = born, &pend
	n.emu.Unlock()
	if _, fixed, err := d.TendNetwork(ctx, spec, got.EngineRef); err != nil || len(fixed) != 1 || !strings.HasPrefix(fixed[0], "endpoints (started again") {
		t.Fatalf("%v %v", fixed, err)
	}
	if w, acts := n.wrote(), n.did(); len(w) != 0 || !slices.Equal(acts, []string{"reboot 11200"}) {
		t.Fatalf("wrote %v, did %v", w, acts)
	}
	// a hand wrote another line: written again, started again
	n.emu.Lock()
	n.running[11200] = "HANGAR_ENDPOINTS=tcp:192.0.2.35:22\x00HANGAR_PRIVATE=192.0.2.0/24" // no-environment: ok — the documentation's own range
	n.emu.Unlock()
	if _, fixed, err := d.TendNetwork(ctx, spec, got.EngineRef); err != nil || !slices.Equal(fixed, []string{"endpoints"}) {
		t.Fatalf("%v %v", fixed, err)
	}
	if w, acts := n.wrote(), n.did(); !slices.Equal(w, []string{"env 11200"}) || !slices.Equal(acts, []string{"reboot 11200"}) {
		t.Fatalf("wrote %v, did %v", w, acts)
	}
	// stopped, its endpoints taken away (the zone publishes nothing now): the
	// field removed, at once, nothing started
	n.mu.Lock()
	n.on[11200] = false
	n.mu.Unlock()
	spec.Endpoints = nil
	got, fixed, err = d.TendNetwork(ctx, spec, got.EngineRef)
	if err != nil || !slices.Equal(fixed, []string{"endpoints"}) || len(got.Endpoints) != 0 {
		t.Fatalf("%v %v %+v", fixed, err, got)
	}
	if w, acts := n.wrote(), n.did(); !slices.Equal(w, []string{"delete 11200"}) || len(acts) != 0 {
		t.Fatalf("wrote %v, did %v", w, acts)
	}
}

// A zone whose gateways cannot read endpoints is never written one: asked,
// it refuses — and one that publishes nothing reads no env at all.
func TestAZoneWithoutEndpointsWritesNone(t *testing.T) {
	a := newAPI()
	n := newEnvNode(a)
	d := open(t, a, netZone)
	ctx := context.Background()
	_, err := d.CreateNetwork(ctx, driver.NetworkSpec{ID: "net-a", Endpoints: &driver.Endpoints{To: []string{logs}}})
	if !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), "carry no endpoints") {
		t.Fatalf("%v", err)
	}
	got, err := d.CreateNetwork(ctx, driver.NetworkSpec{ID: "net-b"})
	if err != nil || got.Endpoints != nil {
		t.Fatal(err)
	}
	if _, fixed, err := d.TendNetwork(ctx, driver.NetworkSpec{ID: "net-b"}, got.EngineRef); err != nil || len(fixed) != 0 {
		t.Fatalf("%v %v", fixed, err)
	}
	for _, c := range a.calls {
		if strings.HasSuffix(c, "/pending") {
			t.Fatalf("read an env where none can be: %s", c)
		}
	}
	for vmid, form := range n.forms {
		if form["env"] != "" {
			t.Fatalf("%d born with an env: %v", vmid, form)
		}
	}
}
