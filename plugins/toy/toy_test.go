package toy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/driver/fake"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

func configured(t *testing.T, caps string) *Plugin {
	t.Helper()
	p := New()
	resp, err := p.Configure(context.Background(), &pluginpb.ConfigureRequest{Zones: []*pluginpb.ZoneConfig{
		{Name: "z", Driver: fake.Name, Options: map[string]string{"capabilities": caps}},
	}})
	if err != nil || resp.GetZones()[0].GetError() != "" {
		t.Fatalf("%v %v", resp, err)
	}
	return p
}

func TestPlanAppliesDefaultsAndNamesWhatItHolds(t *testing.T) {
	p := configured(t, "kind.container,guest.tags")
	plan, err := p.Plan(context.Background(), &pluginpb.PlanRequest{Type: "box", Zone: "z", Spec: []byte(`{"cores":3}`)})
	if err != nil {
		t.Fatal(err)
	}
	var s Spec
	_ = json.Unmarshal(plan.GetSpec(), &s)
	if s != (Spec{Kind: "container", Cores: 3, MemoryGB: 1, Running: true}) {
		t.Fatalf("%+v", s)
	}
	if plan.GetUsage()["toy.cores"] != 3 || plan.GetUsage()["toy.boxes"] != 1 || plan.GetChoices()["toy.kind"] != "container" {
		t.Fatalf("%v %v", plan.GetUsage(), plan.GetChoices())
	}
	plan, _ = p.Plan(context.Background(), &pluginpb.PlanRequest{Type: "box", Zone: "z", Spec: []byte(`{"kind":"vm"}`)})
	if len(plan.GetRefusals()) != 1 || plan.GetRefusals()[0].GetField() != "/kind" {
		t.Fatalf("a VM in a zone of containers: %v", plan.GetRefusals())
	}
}

func TestShrinkingARunningBoxNeedsTheCapability(t *testing.T) {
	p := configured(t, "kind.container,guest.tags")
	cur := &pluginpb.Resource{Spec: []byte(`{"kind":"container","cores":2,"memory_gb":4,"running":true}`)}
	plan, _ := p.Plan(context.Background(), &pluginpb.PlanRequest{Type: "box", Zone: "z", Action: "resize", Params: []byte(`{"memory_gb":2}`), Current: cur})
	if len(plan.GetRefusals()) != 1 || !strings.Contains(plan.GetRefusals()[0].GetReason(), "stop it first") {
		t.Fatalf("%v", plan.GetRefusals())
	}
	p = configured(t, "kind.container,guest.tags,resize.live.memory_down")
	plan, _ = p.Plan(context.Background(), &pluginpb.PlanRequest{Type: "box", Zone: "z", Action: "resize", Params: []byte(`{"memory_gb":2}`), Current: cur})
	if len(plan.GetRefusals()) != 0 || plan.GetUsage()["toy.memory_gb"] != 2 {
		t.Fatalf("%v", plan)
	}
}

func TestReconcileRepairsWhatItCanAndReportsTheRest(t *testing.T) {
	p := configured(t, "kind.container,guest.tags")
	ctx := context.Background()
	r := &pluginpb.Resource{Id: "box-1", Type: "box", Zone: "z", Owner: "alice",
		Spec: []byte(`{"kind":"container","cores":2,"memory_gb":1,"running":true}`)}
	if _, err := p.Create(ctx, &pluginpb.CreateRequest{Resource: r}); err != nil {
		t.Fatal(err)
	}
	engine := p.Driver("z").(*fake.Engine)

	got, _ := p.Reconcile(ctx, &pluginpb.ReconcileRequest{Resource: r})
	if got.GetDrift() != pluginpb.Drift_DRIFT_IN_SYNC {
		t.Fatalf("the control: a fresh box is in sync, got %v", got.GetDrift())
	}
	engine.Tamper("box-1", func(g *driver.Guest) { g.Cores, g.Running = 8, false })
	got, _ = p.Reconcile(ctx, &pluginpb.ReconcileRequest{Resource: r})
	if got.GetDrift() != pluginpb.Drift_DRIFT_REPAIRED {
		t.Fatalf("%v %s", got.GetDrift(), got.GetDetail())
	}
	if g, _ := engine.Guest(ctx, "box-1"); g.Cores != 2 || !g.Running {
		t.Fatalf("not put back: %+v", g)
	}
	engine.Forget("box-1")
	if got, _ := p.Reconcile(ctx, &pluginpb.ReconcileRequest{Resource: r}); got.GetDrift() != pluginpb.Drift_DRIFT_MISSING {
		t.Fatalf("%v", got.GetDrift())
	}
	// a delete of what is gone succeeds (the contract's rule 1)
	if _, err := p.Delete(ctx, &pluginpb.DeleteRequest{Resource: r}); err != nil {
		t.Fatal(err)
	}
	// an action on it is refused as the engine's state, not retried
	_, err := p.Act(ctx, &pluginpb.ActRequest{Resource: r, Action: "stop"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("%v", err)
	}
}
