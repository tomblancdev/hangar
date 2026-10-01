package limits

import (
	"strings"
	"testing"
	"time"

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

// A meter is admitted by what the month has consumed, not by what is held:
// spent at its limit, refused with the numbers and the day it is back; a
// tier that does not name it allows none of it; unlimited is never spent.
func TestAMeterIsTheMonths(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	meters := map[string]Dimension{"toy.hours": {Name: "toy.hours", Kind: Meter, Unit: "hours", Plugin: "toy"}}
	// 23:30 UTC on the 31st is already November in Paris
	m := MonthOf(time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC), paris)
	if m.Key != "2026-11" || m.Name != "November 2026" || !m.Next.Equal(time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("the month in Paris: %+v", m)
	}
	if utc := MonthOf(time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC), time.UTC); utc.Key != "2026-10" || !utc.Next.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the month in UTC: %+v", utc)
	}
	// December's next is January of the next year
	if dec := MonthOf(time.Date(2026, 12, 15, 12, 0, 0, 0, time.UTC), time.UTC); dec.Key != "2026-12" || !dec.Next.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("December: %+v", dec)
	}
	ten := tier(map[string]config.Limit{"toy.hours": n(10)})
	for used, spent := range map[float64]bool{0: false, 9.99: false, 10: true, 10.133: true} {
		got := Spent(ten, map[string]float64{"toy.hours": used}, []string{"toy.hours"})
		if (len(got) == 1) != spent {
			t.Errorf("%v of 10: spent %v", used, got)
		}
	}
	rs := AdmitMeters(ten, meters, map[string]float64{"toy.hours": 10.133}, []string{"toy.hours"}, m)
	if len(rs) != 1 || rs[0].Reason != "meter" || *rs[0].Limit != 10 || rs[0].Period != "2026-11" || !rs[0].Resets.Equal(m.Next) ||
		rs[0].Message != "10.1 of 10 hours (toy.hours) used in November 2026: it is back on 1 December" {
		t.Fatalf("%+v", rs)
	}
	if rs := AdmitMeters(ten, meters, map[string]float64{"toy.hours": 9.99}, []string{"toy.hours"}, m); len(rs) != 0 {
		t.Fatalf("hours left, and refused: %+v", rs)
	}
	if rs := AdmitMeters(ten, meters, map[string]float64{"toy.hours": 50}, nil, m); len(rs) != 0 {
		t.Fatalf("a request that draws on nothing was refused: %+v", rs)
	}
	if rs := AdmitMeters(tier(nil), meters, nil, []string{"toy.hours"}, m); len(rs) != 1 || rs[0].Message != "tier users allows no toy.hours" {
		t.Fatalf("a meter the tier does not name: %+v", rs)
	}
	for _, open := range []map[string]config.Limit{{"toy.hours": {Unlimited: true}}, {"toy.*": {Unlimited: true}}, {"*": {Unlimited: true}}} {
		if rs := AdmitMeters(tier(open), meters, map[string]float64{"toy.hours": 1e6}, []string{"toy.hours"}, m); len(rs) != 0 {
			t.Fatalf("unlimited, and spent: %+v", rs)
		}
	}
	for v, want := range map[float64]string{0: "0", 4: "4", 4.25: "4.2", 9.99: "9.9", 10.133: "10.1", 1023.96: "1023.9"} {
		if got := Amount(v); got != want {
			t.Errorf("%v reads %q, want %q", v, got, want)
		}
	}
	// a meter's limit is a number or unlimited, never a list
	err = Check([]config.Tier{*tier(map[string]config.Limit{"toy.hours": {IsChoice: true, Allowed: []string{"x"}}})}, meters, []string{"toy"})
	if err == nil || !strings.Contains(err.Error(), "is a meter") {
		t.Fatalf("a list for a meter: %v", err)
	}
	if err := Check([]config.Tier{*tier(map[string]config.Limit{"toy.hours": n(40)})}, meters, []string{"toy"}); err != nil {
		t.Fatal(err)
	}
}
