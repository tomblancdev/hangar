package registry

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func put(t *testing.T, s *Store, id, owner, state string, usage map[string]int64, tags map[string]string) {
	t.Helper()
	err := s.Tx(context.Background(), func(tx *Tx) error {
		return tx.InsertResource(&Resource{ID: id, Type: "box", Plugin: "toy", Owner: owner, Zone: "z", State: state,
			Spec: json.RawMessage(`{}`), Usage: usage, Tags: tags})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigrationsRunOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hangar.db")
	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if v, _ := s.Version(context.Background()); v != 5 {
			t.Fatalf("schema version %d", v)
		}
		_ = s.Close()
	}
}

// A resource's usability is the plugin's say, written and read back; a
// schedule's clock survives the brain.
func TestUsabilityAndSchedules(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	put(t, s, "img-00000000000000001", "ops", Ready, nil, nil)
	r, _ := s.Resource(ctx, "img-00000000000000001")
	if r.Unusable != "" || r.Pending {
		t.Fatalf("usable unless a plugin says otherwise: %+v", r)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		return tx.Update(r.ID, Change{Usability: &Usability{Unusable: "pending", Pending: true}})
	}); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.Resource(ctx, r.ID); r.Unusable != "pending" || !r.Pending {
		t.Fatalf("pending: %+v", r)
	}
	if _, err := s.Schedule(ctx, "weekly"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a schedule never seen: %v", err)
	}
	since := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	run := since.Add(time.Hour)
	if err := s.PutSchedule(ctx, &ScheduleState{Name: "weekly", Since: since}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSchedule(ctx, &ScheduleState{Name: "weekly", Since: run, LastRun: &run, LastResult: "asked", LastResource: r.ID}); err != nil {
		t.Fatal(err)
	}
	st, err := s.Schedule(ctx, "weekly")
	if err != nil || !st.Since.Equal(run) || st.LastRun == nil || !st.LastRun.Equal(run) || st.LastResult != "asked" || st.LastResource != r.ID {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestUsageCountsOnlyWhatIsHeld(t *testing.T) {
	s := open(t)
	u := map[string]int64{"toy.boxes": 1, "toy.cores": 2}
	for i, st := range []string{Creating, Ready, Updating, Deleting, Lost, Deleted, Failed} {
		put(t, s, "box-0000000000000000"+string(rune('a'+i)), "alice", st, u, nil)
	}
	put(t, s, "box-00000000000000009", "bob", Ready, u, nil)
	got, err := s.Usage(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got["toy.boxes"] != 5 || got["toy.cores"] != 10 {
		t.Fatalf("five live boxes of alice's, not the deleted, failed or bob's: %v", got)
	}
}

func TestListingFiltersAndPages(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for i := range 5 {
		tags := map[string]string{"team": "a"}
		if i%2 == 1 {
			tags["team"] = "b"
		}
		put(t, s, "box-0000000000000000"+string(rune('a'+i)), "alice", Ready, nil, tags)
	}
	rs, next, err := s.Resources(ctx, Filter{Owner: "alice", Limit: 2})
	if err != nil || len(rs) != 2 || next == 0 {
		t.Fatalf("a first page of 2 and a cursor: %d %d %v", len(rs), next, err)
	}
	var all []string
	for after := int64(0); ; {
		rs, next, err := s.Resources(ctx, Filter{Owner: "alice", Limit: 2, After: after})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			all = append(all, r.ID)
		}
		if next == 0 {
			break
		}
		after = next
	}
	if len(all) != 5 || all[0] != "box-0000000000000000a" {
		t.Fatalf("pages in the order made: %v", all)
	}
	rs, _, _ = s.Resources(ctx, Filter{Tags: map[string]string{"team": "b"}})
	if len(rs) != 2 {
		t.Fatalf("tag filter: %d", len(rs))
	}
	rs, _, _ = s.Resources(ctx, Filter{States: []string{Deleted}})
	if len(rs) != 0 {
		t.Fatal("no deleted resource was made")
	}
}

// A listing of one's own and of what others shared: with one of one's
// groups, or with everyone — never what was shared with a group one is not
// in, nor what someone stopped sharing.
func TestSharesWidenAListingToWhatOthersOpened(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	put(t, s, "box-0000000000000000a", "alice", Ready, nil, nil)
	put(t, s, "box-0000000000000000b", "bob", Ready, nil, nil)
	put(t, s, "box-0000000000000000c", "bob", Ready, nil, nil)
	put(t, s, "box-0000000000000000d", "bob", Ready, nil, nil)
	put(t, s, "box-0000000000000000e", "bob", Ready, nil, nil)
	share := func(id string, groups ...string) {
		t.Helper()
		if err := s.Tx(ctx, func(tx *Tx) error { return tx.SetShares(id, groups) }); err != nil {
			t.Fatal(err)
		}
	}
	share("box-0000000000000000b", "*")
	share("box-0000000000000000c", "family")
	share("box-0000000000000000d", "friends")
	share("box-0000000000000000e", "family")
	share("box-0000000000000000e") // no longer
	ids := func(f Filter) []string {
		t.Helper()
		rs, _, err := s.Resources(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rs {
			out = append(out, r.ID+":"+strings.Join(r.SharedWith, ","))
		}
		return out
	}
	got := ids(Filter{Owner: "alice", SharedTo: []string{"family"}})
	want := []string{"box-0000000000000000a:", "box-0000000000000000b:*", "box-0000000000000000c:family"}
	if !slices.Equal(got, want) {
		t.Fatalf("alice in family sees %v, want %v", got, want)
	}
	if got := ids(Filter{Owner: "alice", SharedTo: []string{}}); len(got) != 2 {
		t.Fatalf("alice in no group sees her own and what is everyone's: %v", got)
	}
	if got := ids(Filter{Owner: "alice"}); len(got) != 1 {
		t.Fatalf("without SharedTo, her own alone: %v", got)
	}
	r, err := s.Resource(ctx, "box-0000000000000000c")
	if err != nil || !slices.Equal(r.SharedWith, []string{"family"}) {
		t.Fatalf("one resource read with its shares: %v %v", r.SharedWith, err)
	}
}

func TestAConditionalUpdateLosesToWhoeverMovedFirst(t *testing.T) {
	s := open(t)
	put(t, s, "box-0000000000000000a", "alice", Ready, nil, nil)
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.Update("box-0000000000000000a", Change{State: Updating}) }); err != nil {
		t.Fatal(err)
	}
	err := s.Tx(ctx, func(tx *Tx) error {
		return tx.Update("box-0000000000000000a", Change{State: Lost, IfState: Ready})
	})
	if !errors.Is(err, ErrMoved) {
		t.Fatalf("got %v", err)
	}
	r, _ := s.Resource(ctx, "box-0000000000000000a")
	if r.State != Updating {
		t.Fatalf("state %s", r.State)
	}
}

func TestAClientTokenIsOnePerOwner(t *testing.T) {
	s := open(t)
	put(t, s, "box-0000000000000000a", "alice", Ready, nil, nil)
	ctx := context.Background()
	ins := func(id, owner string) error {
		return s.Tx(ctx, func(tx *Tx) error {
			return tx.InsertOperation(&Operation{ID: id, Owner: owner, ResourceID: "box-0000000000000000a", Kind: OpCreate, ClientToken: "t1"})
		})
	}
	if err := ins("op-0000000000000000a", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := ins("op-0000000000000000b", "alice"); err == nil {
		t.Fatal("the same owner reused a client token")
	}
	if err := ins("op-0000000000000000c", "bob"); err != nil {
		t.Fatalf("another owner's token is another token: %v", err)
	}
	var got *Operation
	_ = s.Tx(ctx, func(tx *Tx) (err error) { got, err = tx.OperationByToken("alice", "t1"); return })
	if got == nil || got.ID != "op-0000000000000000a" {
		t.Fatalf("%+v", got)
	}
	running, _ := s.Running(ctx)
	if len(running) != 2 {
		t.Fatalf("running: %d", len(running))
	}
}

func TestTokens(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tok := &Token{ID: "tok-0000000000000000a", Hash: "h", Owner: "alice", Name: "ci", Groups: []string{"g"},
		Scopes: []string{"read"}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.InsertToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, err := s.TokenByHash(ctx, "h")
	if err != nil || got.Owner != "alice" || got.Groups[0] != "g" || got.Scopes[0] != "read" {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.RevokeToken(ctx, "bob", tok.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("bob revoked alice's token")
	}
	if err := s.RevokeToken(ctx, "alice", tok.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken(ctx, "alice", tok.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("a revoked token is revoked once")
	}
}

// A zone's pools count what its live resources take: every floor booked,
// running or not; borrowed room only while meant to run and not held.
func TestZoneUse(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	add := func(id, state string, room Room, hold string) {
		t.Helper()
		if err := s.Tx(ctx, func(tx *Tx) error {
			return tx.InsertResource(&Resource{ID: id, Type: "machine", Plugin: "machines", Owner: "alice", Zone: "z",
				State: state, Spec: json.RawMessage(`{}`), Room: room, Hold: hold})
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("m-1", Ready, Room{GuaranteedMB: 1024, SpotMB: 3072, Running: true}, "")   // a floor + a top-up in use
	add("m-2", Ready, Room{SpotMB: 2048, Running: true}, "4100")                   // held: borrows nothing now
	add("m-3", Ready, Room{SpotMB: 2048}, "")                                      // stopped
	add("m-4", Deleted, Room{GuaranteedMB: 8192, SpotMB: 8192, Running: true}, "") // gone
	add("m-5", Creating, Room{GuaranteedMB: 512}, "")
	var booked, spot int64
	err := s.Tx(ctx, func(tx *Tx) (err error) { booked, spot, err = tx.ZoneUse("z", "m-5"); return })
	if err != nil || booked != 1024 || spot != 3072 {
		t.Fatalf("booked %d, spot %d, %v", booked, spot, err)
	}
	r, _ := s.Resource(ctx, "m-2")
	if r.Hold != "4100" || r.HoldSince == nil || !r.Room.Running || r.Room.SpotMB != 2048 {
		t.Fatalf("%+v", r)
	}
	since := *r.HoldSince
	same, lifted := "4100", ""
	time.Sleep(2 * time.Millisecond)
	_ = s.Tx(ctx, func(tx *Tx) error { return tx.Update("m-2", Change{Hold: &same}) })
	if r, _ = s.Resource(ctx, "m-2"); !r.HoldSince.Equal(since) {
		t.Fatalf("the same hold began again: %v then %v", since, r.HoldSince)
	}
	_ = s.Tx(ctx, func(tx *Tx) error { return tx.Update("m-2", Change{Hold: &lifted}) })
	if r, _ = s.Resource(ctx, "m-2"); r.Hold != "" || r.HoldSince != nil {
		t.Fatalf("a lifted hold: %+v", r)
	}

	if err := s.Tx(ctx, func(tx *Tx) error { return tx.PutClaim("z", "priority", "hook") }); err != nil {
		t.Fatal(err)
	}
	cs, err := s.Claims(ctx)
	if err != nil || len(cs) != 1 || cs[0].Reservation != "priority" || cs[0].By != "hook" {
		t.Fatalf("%+v %v", cs, err)
	}
	_ = s.Tx(ctx, func(tx *Tx) error { return tx.DeleteClaim("z", "priority") })
	if cs, _ = s.Claims(ctx); len(cs) != 0 {
		t.Fatalf("a released claim stays: %+v", cs)
	}
}

// A meter only grows, month by month and owner by owner — a deleted
// resource takes nothing back; and a resource keeps the tier it was admitted
// under.
func TestMetersAndTheAdmittedTier(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	add := func(owner, period string, n float64) {
		t.Helper()
		if err := s.Tx(ctx, func(tx *Tx) error { return tx.AddMeter(owner, "machines.vcpu_hours", period, n) }); err != nil {
			t.Fatal(err)
		}
	}
	add("alice", "2026-10", 1.5)
	add("alice", "2026-10", 0.25)
	add("alice", "2026-11", 4)
	add("bob", "2026-10", 9)
	for period, want := range map[string]float64{"2026-10": 1.75, "2026-11": 4, "2026-12": 0} {
		got, err := s.Metered(ctx, "alice", period)
		if err != nil || got["machines.vcpu_hours"] != want {
			t.Fatalf("%s: %v %v, want %v", period, got, err, want)
		}
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.AddMeter("alice", "machines.vcpu_hours", "2026-10", -5) }); err == nil {
		t.Fatal("a meter went below nothing")
	}

	put(t, s, "m-00000000000000001", "alice", Ready, nil, nil)
	r, _ := s.Resource(ctx, "m-00000000000000001")
	if r.Tier != "" {
		t.Fatalf("no tier was written: %q", r.Tier)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.Update(r.ID, Change{Tier: "users"}) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.Update(r.ID, Change{Drift: new(string)}) }); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.Resource(ctx, r.ID); r.Tier != "users" {
		t.Fatalf("the tier stays until another request is admitted: %q", r.Tier)
	}
}
