package machines

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/driver/fake"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// Where its zone keeps a wall a machine is born behind it, with the address
// its zone gave it — both on its page, running or not.
func TestAMachineIsBornBehindItsWall(t *testing.T) {
	m := born(t, small+`}`)
	g, _ := m.e.Guest(context.Background(), m.r.GetId())
	if g.Wall != driver.WallExact || g.Address == "" {
		t.Fatalf("born as %+v", g)
	}
	if o := m.observed(); o.Wall != driver.WallExact || o.Address != g.Address {
		t.Fatalf("its page: wall %q, address %q (the engine gave %s)", o.Wall, o.Address, g.Address)
	}
	if resp := m.pass(0); resp.GetDrift() != pluginpb.Drift_DRIFT_IN_SYNC {
		t.Fatalf("born in sync: %v %q", resp.GetDrift(), resp.GetDetail())
	}
	if why := m.act("stop", ""); why != "" {
		t.Fatal(why)
	}
	if o := m.observed(); o.Wall != driver.WallExact || o.Address != g.Address {
		t.Fatalf("stopped, its page: wall %q, address %q", o.Wall, o.Address)
	}

	// the control: a zone that keeps none gives neither, and repairs nothing
	bare := configured(t, "fake", "kind.container,kind.vm,guest.tags,fence.pool")
	pr, _ := plan(t, bare, small+`}`)
	r := &pluginpb.Resource{Id: "m-0123456789abcdef1", Type: "machine", Zone: "z", Owner: "alice", Spec: pr.GetSpec()}
	cr, err := bare.Create(context.Background(), &pluginpb.CreateRequest{Resource: r})
	if err != nil {
		t.Fatal(err)
	}
	r.Observed = cr.GetObserved()
	if o := observedOf(r); o.Wall != "" || o.Address != "" {
		t.Fatalf("a zone with no wall: wall %q, address %q", o.Wall, o.Address)
	}
	if resp, err := bare.Reconcile(context.Background(), &pluginpb.ReconcileRequest{Resource: r}); err != nil || resp.GetDrift() != pluginpb.Drift_DRIFT_IN_SYNC {
		t.Fatalf("a zone with no wall, looked at: %v %v", resp, err)
	}
}

// A wall taken off behind the brain's back is put back at the next look, and
// said: repaired, in the engine's own words for what it wrote.
func TestAWallTakenOffIsPutBack(t *testing.T) {
	m := born(t, small+`}`)
	open := func() { m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { g.Wall = "" }) }
	on := func() string {
		g, _ := m.e.Guest(context.Background(), m.r.GetId())
		return g.Wall
	}
	open()
	resp := m.pass(0)
	if resp.GetDrift() != pluginpb.Drift_DRIFT_REPAIRED || !strings.Contains(resp.GetDetail(), "wall (card)") || on() != driver.WallExact {
		t.Fatalf("put back: %v %q, the engine %q", resp.GetDrift(), resp.GetDetail(), on())
	}
	if m.observed().Wall != driver.WallExact || m.events[len(m.events)-1] != "machine.repaired" {
		t.Fatalf("its page says %q, events %v", m.observed().Wall, m.events)
	}
	if resp := m.pass(0); resp.GetDrift() != pluginpb.Drift_DRIFT_IN_SYNC {
		t.Fatalf("a second look: %v %q", resp.GetDrift(), resp.GetDetail())
	}

	// a stopped machine's wall is put back before it starts: never a first
	// packet without it
	if why := m.act("stop", ""); why != "" {
		t.Fatal(why)
	}
	open()
	if why := m.act("start", ""); why != "" {
		t.Fatal(why)
	}
	if on() != driver.WallExact || !m.running() || m.observed().Wall != driver.WallExact {
		t.Fatalf("started behind %q, its page %q", on(), m.observed().Wall)
	}

	// a wall that cannot be written: the machine does not start, and says why
	if why := m.act("stop", ""); why != "" {
		t.Fatal(why)
	}
	open()
	m.p.mu.Lock()
	m.p.zones["z"] = &refusingWall{Engine: m.e}
	m.p.mu.Unlock()
	if why := m.act("start", ""); !strings.Contains(why, "its wall") || !strings.Contains(why, "no security group") {
		t.Fatalf("a start with no wall: %q", why)
	}
	if m.running() {
		t.Fatal("it runs without its wall")
	}
	if resp := m.pass(0); resp.GetDrift() != pluginpb.Drift_DRIFT_DRIFTED || !strings.Contains(resp.GetDetail(), "its wall") {
		t.Fatalf("a wall that cannot be put back, looked at: %v %q", resp.GetDrift(), resp.GetDetail())
	}
}

// refusingWall is an engine whose wall cannot be written.
type refusingWall struct{ *fake.Engine }

func (refusingWall) Wall(context.Context, string) ([]string, error) {
	return nil, errors.Join(driver.ErrRefused, errors.New("no security group 'floor'"))
}
