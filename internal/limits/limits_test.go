package limits

import (
	"strings"
	"testing"

	"github.com/tomblancdev/hangar/internal/config"
)

var dims = map[string]Dimension{
	"toy.boxes":     {Name: "toy.boxes", Kind: Quantity, Plugin: "toy"},
	"toy.cores":     {Name: "toy.cores", Kind: Quantity, Unit: "vCPU", Plugin: "toy"},
	"toy.memory_gb": {Name: "toy.memory_gb", Kind: Quantity, Unit: "GB", Plugin: "toy"},
	"toy.kind":      {Name: "toy.kind", Kind: Choice, Plugin: "toy"},
}

func tier(limits map[string]config.Limit) *config.Tier {
	return &config.Tier{Name: "users", Groups: []string{"g"}, Zones: []string{"z"}, Limits: limits}
}

func n(v int64) config.Limit { return config.Limit{Max: v} }

func TestTheFirstTierInTheFileWins(t *testing.T) {
	tiers := []config.Tier{
		{Name: "operators", Groups: []string{"ops"}},
		{Name: "users", Groups: []string{"users", "ops"}},
	}
	got, ok := For(tiers, []string{"users", "ops"})
	if !ok || got.Name != "operators" {
		t.Fatalf("a person in both is in the first tier listed; got %v", got)
	}
	if _, ok := For(tiers, []string{"strangers"}); ok {
		t.Fatal("a person in no listed group is in no tier")
	}
}

func TestTheMostPreciseLimitWins(t *testing.T) {
	tr := tier(map[string]config.Limit{"toy.cores": n(4), "toy.*": n(10), "*": {Unlimited: true}})
	for dim, want := range map[string]int64{"toy.cores": 4, "toy.boxes": 10} {
		if l, _ := Of(tr, dim); l.Max != want {
			t.Errorf("%s: got %d, want %d", dim, l.Max, want)
		}
	}
	if l, _ := Of(tr, "other.thing"); !l.Unlimited {
		t.Error(`"*" covers what nothing more precise names`)
	}
	if _, named := Of(tier(nil), "toy.cores"); named {
		t.Error("a tier naming nothing names nothing")
	}
}

func TestAdmitSaysWhyWithTheNumbers(t *testing.T) {
	tr := tier(map[string]config.Limit{"toy.boxes": n(2), "toy.cores": n(4), "toy.kind": {IsChoice: true, Allowed: []string{"container"}}})
	used := map[string]int64{"toy.boxes": 2, "toy.cores": 3}
	after := map[string]int64{"toy.boxes": 1, "toy.cores": 1}
	rs := Admit(tr, dims, used, nil, after, nil, map[string]string{"toy.kind": "container"})
	if len(rs) != 1 || rs[0].Dimension != "toy.boxes" {
		t.Fatalf("only the boxes are over: %+v", rs)
	}
	if rs[0].Message != "2 of 2 toy.boxes used; this asks for 1 more" {
		t.Errorf("message: %q", rs[0].Message)
	}
	if *rs[0].Limit != 2 || rs[0].Used != 2 || rs[0].Asked != 1 {
		t.Errorf("numbers: %+v", rs[0])
	}

	rs = Admit(tr, dims, map[string]int64{"toy.cores": 3}, nil, map[string]int64{"toy.cores": 2}, nil, nil)
	if len(rs) != 1 || !strings.Contains(rs[0].Message, "3 of 4 vCPU (toy.cores) used; this asks for 2 more") {
		t.Fatalf("a unit is spelled with its dimension: %+v", rs)
	}
}

func TestADimensionNotNamedAllowsNothing(t *testing.T) {
	rs := Admit(tier(map[string]config.Limit{"toy.boxes": n(5)}), dims, nil, nil,
		map[string]int64{"toy.boxes": 1, "toy.memory_gb": 1}, nil, nil)
	if len(rs) != 1 || rs[0].Dimension != "toy.memory_gb" || rs[0].Message != "tier users allows no toy.memory_gb" {
		t.Fatalf("%+v", rs)
	}
}

func TestShrinkingAlwaysFits(t *testing.T) {
	// the limit was lowered after the box was made: growing is refused,
	// shrinking toward the limit never is
	tr := tier(map[string]config.Limit{"toy.cores": n(2)})
	used := map[string]int64{"toy.cores": 8}
	before := map[string]int64{"toy.cores": 8}
	if rs := Admit(tr, dims, used, before, map[string]int64{"toy.cores": 6}, nil, nil); len(rs) != 0 {
		t.Fatalf("shrink refused: %+v", rs)
	}
	if rs := Admit(tr, dims, used, before, map[string]int64{"toy.cores": 9}, nil, nil); len(rs) != 1 {
		t.Fatal("growth over a lowered limit must be refused")
	}
	// the resource's own share is in used: resizing 2 -> 3 of a limit of 4,
	// with 2 used, is 3 of 4
	tr = tier(map[string]config.Limit{"toy.cores": n(4)})
	if rs := Admit(tr, dims, map[string]int64{"toy.cores": 2}, map[string]int64{"toy.cores": 2},
		map[string]int64{"toy.cores": 3}, nil, nil); len(rs) != 0 {
		t.Fatalf("a resize inside the limit refused: %+v", rs)
	}
}

func TestChoices(t *testing.T) {
	tr := tier(map[string]config.Limit{"toy.kind": {IsChoice: true, Allowed: []string{"container"}}})
	rs := Admit(tr, dims, nil, nil, nil, nil, map[string]string{"toy.kind": "vm"})
	if len(rs) != 1 || rs[0].Reason != "choice" || rs[0].Message != `toy.kind "vm" is not open to tier users (open: container)` {
		t.Fatalf("%+v", rs)
	}
	// unchanged since the list moved: never refused (a start is not a new choice)
	if rs := Admit(tr, dims, nil, nil, nil, map[string]string{"toy.kind": "vm"}, map[string]string{"toy.kind": "vm"}); len(rs) != 0 {
		t.Fatalf("an unchanged choice was refused: %+v", rs)
	}
	if rs := Admit(tier(map[string]config.Limit{"*": {Unlimited: true}}), dims, nil, nil, nil, nil, map[string]string{"toy.kind": "vm"}); len(rs) != 0 {
		t.Fatal(`"*": unlimited opens every choice`)
	}
}

func TestCheckRefusesATypo(t *testing.T) {
	bad := []struct {
		limits map[string]config.Limit
		want   string
	}{
		{map[string]config.Limit{"toy.boxs": n(1)}, `"toy.boxs" is no dimension`},
		{map[string]config.Limit{"toy.boxes": {IsChoice: true, Allowed: []string{"x"}}}, "is a quantity"},
		{map[string]config.Limit{"toy.kind": n(3)}, "is a choice"},
		{map[string]config.Limit{"other.*": n(3)}, "names no enabled plugin"},
	}
	for _, b := range bad {
		err := Check([]config.Tier{*tier(b.limits)}, dims, []string{"toy"})
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%v: got %v, want %q", b.limits, err, b.want)
		}
	}
	ok := map[string]config.Limit{"toy.boxes": n(1), "toy.kind": {Unlimited: true}, "toy.*": n(2), "*": {Unlimited: true}}
	if err := Check([]config.Tier{*tier(ok)}, dims, []string{"toy"}); err != nil {
		t.Fatal(err)
	}
}
