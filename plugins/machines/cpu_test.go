package machines

import (
	"context"
	"strings"
	"testing"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// A VM's processor: the zone's own model unless it asks for its host's; VMs
// of its own go with the host's processor; each is a choice asked of the
// tier only by a machine that wants it; a weight is a share that only yields.
func TestAMachinesProcessor(t *testing.T) {
	p := configured(t, "strict", "")
	r, s := plan(t, p, `{"image":"debian-13"}`)
	if c := r.GetChoices(); len(r.GetRefusals()) > 0 || s.CPU != "" || s.Virtualization || s.CPUWeight != 0 || len(c) != 2 {
		t.Fatalf("a machine that asks nothing of its processor: %+v, choices %v, %s", s, c, reasons(r))
	}
	r, s = plan(t, p, `{"image":"debian-13","cpu":"host"}`)
	if c := r.GetChoices(); len(r.GetRefusals()) > 0 || s.CPU != "host" || c[CPUChoice] != "host" || len(c) != 3 {
		t.Fatalf("its host's processor, and nothing more asked of the tier: %+v, choices %v, %s", s, c, reasons(r))
	}
	r, s = plan(t, p, `{"image":"debian-13","cpu":"host","virtualization":true,"cpu_weight":25}`)
	if c := r.GetChoices(); len(r.GetRefusals()) > 0 || !s.Virtualization || s.CPUWeight != 25 ||
		c[CPUChoice] != "host" || c[VirtualizationChoice] != Nested {
		t.Fatalf("VMs of its own, at a quarter share: %+v, choices %v, %s", s, c, reasons(r))
	}
	// a full share is what a machine has unsaid: it is not written
	if r, s = plan(t, p, `{"image":"debian-13","cpu_weight":100}`); len(r.GetRefusals()) > 0 || s.CPUWeight != 0 {
		t.Fatalf("a full share: %+v %s", s, reasons(r))
	}
	// a container's weight is a container's too
	if r, s = plan(t, p, `{"image":"debian-13","kind":"container","cpu_weight":40}`); len(r.GetRefusals()) > 0 || s.CPUWeight != 40 {
		t.Fatalf("a container's share: %+v %s", s, reasons(r))
	}
	for spec, want := range map[string]string{
		`{"image":"debian-13","virtualization":true}`:                    "/virtualization a VM that runs VMs of its own sees its host's processor: say cpu: host with it",
		`{"image":"debian-13","kind":"container","cpu":"host"}`:          "/cpu a container sees its host's processor already",
		`{"image":"debian-13","kind":"container","virtualization":true}`: "/virtualization a container runs no VM of its own",
		`{"image":"debian-13","cpu":"EPYC"}`:                             `/cpu no processor "EPYC"`,
	} {
		if r, _ := plan(t, p, spec); !strings.Contains(reasons(r), want) {
			t.Errorf("%s: %q, want %q", spec, reasons(r), want)
		}
	}
	// a zone whose engine has none of it says so, lever by lever
	bare := configured(t, "strict", "kind.container,kind.vm,guest.tags,fence.pool")
	for spec, want := range map[string]string{
		`{"image":"debian-13","cpu":"host"}`:                       "zone z gives no VM its host's processor",
		`{"image":"debian-13","cpu":"host","virtualization":true}`: "zone z lets no VM run VMs of its own",
		`{"image":"debian-13","cpu_weight":25}`:                    "zone z weighs no CPU",
		`{"image":"debian-13","kind":"container","cpu_weight":25}`: "zone z weighs no CPU",
	} {
		if r, _ := plan(t, bare, spec); !strings.Contains(reasons(r), want) {
			t.Errorf("a bare zone, %s: %q, want %q", spec, reasons(r), want)
		}
	}
	if r, _ := plan(t, bare, `{"image":"debian-13","cpu_weight":100}`); reasons(r) != "" {
		t.Errorf("a full share where nothing is weighed is what every machine has: %s", reasons(r))
	}
}

// What a machine asks of its processor reaches its engine at its birth, and
// is read back from it.
func TestCreateHandsTheEngineItsProcessor(t *testing.T) {
	m := born(t, `{"image":"debian-13","cpu":"host","virtualization":true,"cpu_weight":25,"class":"guaranteed"}`)
	got, _ := m.e.Spec(m.r.GetId())
	if got.CPU != driver.CPUModelHost || !got.Virtualization || got.CPUWeight != 25 {
		t.Fatalf("the engine got %+v", got)
	}
	if o := m.observed(); o.CPU != "host" || !o.Virtualization || o.CPUWeight != 25 {
		t.Fatalf("read back as %+v", o)
	}
	// the control: one that asks nothing gets nothing, and a full share
	n := born(t, `{"image":"debian-13","class":"guaranteed"}`)
	got, _ = n.e.Spec(n.r.GetId())
	if got.CPU != "" || got.Virtualization || got.CPUWeight != 0 {
		t.Fatalf("a plain machine's engine got %+v", got)
	}
	if o := n.observed(); o.CPU != "" || o.Virtualization || o.CPUWeight != 0 {
		t.Fatalf("a plain machine reads back as %+v", o)
	}
}

// A machine's weight is set while it runs, and kept true on its engine: one
// changed behind the brain's back is put back at the next look.
func TestACPUWeightIsSetAndKept(t *testing.T) {
	m := born(t, small+`,"cpu_weight":25}`)
	on := func() int {
		t.Helper()
		g, err := m.e.Guest(context.Background(), m.r.GetId())
		if err != nil {
			t.Fatal(err)
		}
		return g.CPUWeight
	}
	if on() != 25 || !m.running() {
		t.Fatalf("born at %d", on())
	}
	if resp := m.pass(0); resp.GetDrift() != pluginpb.Drift_DRIFT_IN_SYNC {
		t.Fatalf("born in sync: %v %q", resp.GetDrift(), resp.GetDetail())
	}
	if why := m.act("set_cpu_weight", `{"cpu_weight":60}`); why != "" {
		t.Fatal(why)
	}
	if on() != 60 || m.spec().CPUWeight != 60 || m.observed().CPUWeight != 60 || !m.running() {
		t.Fatalf("set to 60: the engine %d, the spec %d, observed %d", on(), m.spec().CPUWeight, m.observed().CPUWeight)
	}
	if m.events[len(m.events)-1] != "machine.cpu_weight_set" {
		t.Fatalf("events %v", m.events)
	}
	// changed on the engine by someone else
	m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { g.CPUWeight = 100 })
	if resp := m.pass(0); resp.GetDrift() != pluginpb.Drift_DRIFT_REPAIRED || !strings.Contains(resp.GetDetail(), "cpu weight") || on() != 60 {
		t.Fatalf("put back: %v %q, the engine %d", resp.GetDrift(), resp.GetDetail(), on())
	}
	if resp := m.pass(0); resp.GetDrift() != pluginpb.Drift_DRIFT_IN_SYNC {
		t.Fatalf("a second look: %v %q", resp.GetDrift(), resp.GetDetail())
	}
	// back to a full share: nothing written in its spec
	if why := m.act("set_cpu_weight", `{"cpu_weight":100}`); why != "" {
		t.Fatal(why)
	}
	if on() != driver.FullWeight || m.spec().CPUWeight != 0 || m.observed().CPUWeight != 0 {
		t.Fatalf("a full share: the engine %d, the spec %d, observed %d", on(), m.spec().CPUWeight, m.observed().CPUWeight)
	}
	if resp := m.pass(0); resp.GetDrift() != pluginpb.Drift_DRIFT_IN_SYNC {
		t.Fatalf("a full share in sync: %v %q", resp.GetDrift(), resp.GetDetail())
	}
}

// A guest made before a weight was ever written reads as a full share: the
// next look repairs nothing on it.
func TestAGuestWithNoWeightWrittenIsInSync(t *testing.T) {
	m := born(t, small+`}`)
	m.e.Tamper(m.r.GetId(), func(g *driver.Guest) { g.CPUWeight = 0 })
	if resp := m.pass(0); resp.GetDrift() != pluginpb.Drift_DRIFT_IN_SYNC {
		t.Fatalf("%v %q", resp.GetDrift(), resp.GetDetail())
	}
}

// What apply asks to bring a machine to its file: its weight through its
// action, its processor and its virtualisation set at its birth.
func TestAProcessorIsSetAtBirthAndAWeightChanges(t *testing.T) {
	vm := `{"image":"debian-13","cores":2,"memory_gb":1,"class":"guaranteed"`
	m := born(t, vm+`,"cpu":"host","cpu_weight":25}`)
	for want, c := range map[string]struct{ steps, fixed string }{
		vm + `,"cpu":"host","cpu_weight":25}`:                       {"", ""},
		vm + `,"cpu":"host","cpu_weight":50}`:                       {`set_cpu_weight {"cpu_weight":50}`, ""},
		vm + `,"cpu":"host"}`:                                       {`set_cpu_weight {"cpu_weight":100}`, ""},
		vm + `,"cpu":"host","cpu_weight":100}`:                      {`set_cpu_weight {"cpu_weight":100}`, ""},
		vm + `,"cpu_weight":25}`:                                    {"", "/cpu it is host, and a machine's processor is set at its birth"},
		vm + `,"cpu":"host","virtualization":true,"cpu_weight":25}`: {"", "/virtualization it is false, and whether a machine runs VMs of its own is set at its birth"},
	} {
		resp, err := m.p.PlanChange(context.Background(), &pluginpb.PlanChangeRequest{Current: m.r, Spec: []byte(want)})
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		steps, fixed := "", ""
		for _, s := range resp.GetSteps() {
			steps += s.GetAction() + " " + string(s.GetParams())
		}
		for _, f := range resp.GetFixed() {
			fixed += f.GetField() + " " + f.GetReason()
		}
		if steps != c.steps || fixed != c.fixed {
			t.Errorf("%s:\n steps %q, want %q\n fixed %q, want %q", want, steps, c.steps, fixed, c.fixed)
		}
	}
	// one born without a processor of its own cannot be given one
	n := born(t, vm+`}`)
	resp, err := n.p.PlanChange(context.Background(), &pluginpb.PlanChangeRequest{Current: n.r, Spec: []byte(vm + `,"cpu":"host"}`)})
	if err != nil || len(resp.GetFixed()) != 1 || !strings.Contains(resp.GetFixed()[0].GetReason(), "it is the zone's own model") {
		t.Fatalf("%v %v", resp.GetFixed(), err)
	}
}

// A machine's sentence says its processor only when it has one of its own.
func TestAMachinesSentenceSaysItsProcessor(t *testing.T) {
	for _, hole := range []string{`"{cpu=host?host CPU}"`, `"{virtualization?runs VMs}"`, `"CPU weight {cpu_weight}"`} {
		if !strings.Contains(machineSchema, hole) {
			t.Errorf("the sentence lost %s", hole)
		}
	}
}
