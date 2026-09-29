package room

import (
	"strings"
	"testing"

	"github.com/tomblancdev/hangar/internal/config"
)

// A big zone that sleeps, shared with a priority guest: 62 GB, 15 of them
// always the host's own, 32 kept for a guest while it runs.
var big = &config.Room{MemoryGB: 62, Reservations: []config.Reservation{
	{Name: "house", MemoryGB: 15},
	{Name: "priority", MemoryGB: 32, WhileRunning: "4100"},
}}

const gb = 1024

func TestThePools(t *testing.T) {
	if p := Of(big); p.GuaranteedMB != 15*gb || p.SpotMB != 32*gb || p.MemoryMB != 62*gb {
		t.Fatalf("%+v", p)
	}
	if HeldBy(big, map[string]bool{"house": true}) != nil {
		t.Fatal("a reservation with no condition held the borrowed room")
	}
	if h := HeldBy(big, map[string]bool{"priority": true}); h == nil || h.Name != "priority" {
		t.Fatalf("%v", h)
	}
	if ByKey(big, "4100").Name != "priority" || ForGuest(big, "4100").Name != "priority" || ByKey(big, "house") != nil {
		t.Fatal("finding a reservation by its key")
	}
}

func TestAdmission(t *testing.T) {
	none := map[string]bool{}
	priority := map[string]bool{"priority": true}
	for _, c := range []struct {
		name          string
		inForce       map[string]bool
		use           Use
		before, after Take
		want          string // "" = fits; else a part of the refusal
	}{
		{"a guaranteed floor that fits", none, Use{BookedMB: 3 * gb}, Take{}, Take{GuaranteedMB: 12 * gb, SpotMB: 28 * gb, Running: true}, ""},
		{"booked beyond the pool", none, Use{BookedMB: 12 * gb}, Take{}, Take{GuaranteedMB: 4 * gb},
			"guaranteed pool holds 15 GB; 12 GB booked; this asks for 4 GB more — ask for spot, or less"},
		{"a stopped guaranteed machine still books", none, Use{BookedMB: 12 * gb}, Take{}, Take{GuaranteedMB: 4 * gb, Running: false}, "guaranteed pool"},
		{"spot that fits", none, Use{SpotUsedMB: 28 * gb}, Take{}, Take{SpotMB: 4 * gb, Running: true}, ""},
		{"spot beyond the pool", none, Use{SpotUsedMB: 30 * gb}, Take{}, Take{SpotMB: 4 * gb, Running: true},
			"spot pool holds 32 GB; 30 GB in use; this asks for 4 GB more"},
		{"spot while the room is held", priority, Use{}, Take{}, Take{SpotMB: 1 * gb, Running: true},
			"borrowed room is held for priority (while guest 4100 runs)"},
		{"a spot machine created stopped borrows nothing", priority, Use{SpotUsedMB: 32 * gb}, Take{}, Take{SpotMB: 4 * gb}, ""},
		{"a floor starts on it while the room is held", priority, Use{}, Take{GuaranteedMB: 12 * gb, SpotMB: 28 * gb},
			Take{GuaranteedMB: 12 * gb, SpotMB: 28 * gb, Running: true}, ""},
		{"a guaranteed machine starts while the room is held", priority, Use{BookedMB: 15 * gb}, Take{GuaranteedMB: 3 * gb},
			Take{GuaranteedMB: 3 * gb, Running: true}, ""},
		{"a stop always fits", priority, Use{SpotUsedMB: 40 * gb}, Take{SpotMB: 4 * gb, Running: true}, Take{SpotMB: 4 * gb}, ""},
		{"a shrink fits over the pool", none, Use{BookedMB: 20 * gb}, Take{GuaranteedMB: 8 * gb}, Take{GuaranteedMB: 6 * gb}, ""},
		{"a grown spot machine counts only its growth", none, Use{SpotUsedMB: 28 * gb}, Take{SpotMB: 2 * gb, Running: true},
			Take{SpotMB: 5 * gb, Running: true}, "this asks for 3 GB more"},
	} {
		got := Admit("big", big, c.inForce, c.use, c.before, c.after)
		switch {
		case c.want == "" && got != nil:
			t.Errorf("%s: refused: %s", c.name, got.Message)
		case c.want != "" && (got == nil || !strings.Contains(got.Message, c.want)):
			t.Errorf("%s: want %q, got %+v", c.name, c.want, got)
		}
	}
}

func TestAZoneThatLendsNothing(t *testing.T) {
	r := &config.Room{MemoryGB: 8, Reservations: []config.Reservation{{Name: "host", MemoryGB: 2}}}
	got := Admit("small", r, nil, Use{}, Take{}, Take{SpotMB: gb, Running: true})
	if got == nil || !strings.Contains(got.Message, "lends no room") {
		t.Fatalf("%+v", got)
	}
	if got := Admit("small", r, nil, Use{BookedMB: 5 * gb}, Take{}, Take{GuaranteedMB: gb}); got != nil {
		t.Fatalf("6 of 6 GB refused: %s", got.Message)
	}
}

func TestGB(t *testing.T) {
	for mb, want := range map[int64]string{1024: "1 GB", 1536: "1.5 GB", 15974: "15.6 GB", 0: "0 GB"} {
		if got := GB(mb); got != want {
			t.Errorf("%d MB: %q, want %q", mb, got, want)
		}
	}
}
