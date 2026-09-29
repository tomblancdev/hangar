package core

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tomblancdev/hangar/internal/audit"
	"github.com/tomblancdev/hangar/internal/config"
	"github.com/tomblancdev/hangar/internal/identity"
	"github.com/tomblancdev/hangar/internal/limits"
	"github.com/tomblancdev/hangar/internal/registry"
	"github.com/tomblancdev/hangar/internal/room"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// ---- The room: a zone's pools, its reservations, holds, waking (§6) --------
//
// What a reservation waits on is read by surveys (a plugin asks its driver:
// does guest X run, is node Y down, is the zone awake). A guest's hook claims
// the room before the guest starts and releases it after it stops; a claim
// stands for the zone's grace even before its condition reads true, and past
// it only while the condition does. A hold the engine's node placed alone
// (the brain could not be reached) is adopted as a claim when a survey sees
// it. When a conditional reservation is in force, every resource of the zone
// that takes room is held — the plugin makes that mean what it means for its
// type — and when none is, the holds are lifted.

type zoneState struct {
	met      map[string]bool   // reservation → its condition read true at the last survey
	read     map[string]bool   // reservation → its condition was read at the last survey
	errs     map[string]string // reservation → why its condition could not be read (logged once)
	awake    bool
	known    bool // a survey was answered
	surveyed time.Time
}

type roomState struct {
	mu     sync.Mutex
	zones  map[string]*zoneState
	claims map[string]map[string]registry.Claim // zone → reservation → claim
	// deciding: one room decision at a time per zone (a claim or a release,
	// a pass's survey and holds)
	deciding sync.Map // zone → *sync.Mutex
}

// decide takes a zone's decision lock.
func (c *Core) decide(zone string) func() {
	m, _ := c.room.deciding.LoadOrStore(zone, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// zoneStateLocked returns a zone's state; c.room.mu is held.
func (c *Core) zoneStateLocked(zone string) *zoneState {
	if c.room.zones == nil {
		c.room.zones = map[string]*zoneState{}
	}
	s, ok := c.room.zones[zone]
	if !ok {
		s = &zoneState{met: map[string]bool{}, read: map[string]bool{}, errs: map[string]string{}}
		c.room.zones[zone] = s
	}
	return s
}

func (c *Core) setClaim(cl registry.Claim) {
	c.room.mu.Lock()
	defer c.room.mu.Unlock()
	if c.room.claims == nil {
		c.room.claims = map[string]map[string]registry.Claim{}
	}
	if c.room.claims[cl.Zone] == nil {
		c.room.claims[cl.Zone] = map[string]registry.Claim{}
	}
	c.room.claims[cl.Zone][cl.Reservation] = cl
}

func (c *Core) dropClaim(zone, reservation string) {
	c.room.mu.Lock()
	defer c.room.mu.Unlock()
	delete(c.room.claims[zone], reservation)
}

// loadClaims reads the claims in force from the registry (at start).
func (c *Core) loadClaims(ctx context.Context) error {
	cs, err := c.store.Claims(ctx)
	if err != nil {
		return err
	}
	for _, cl := range cs {
		c.setClaim(cl)
	}
	return nil
}

// inForce says which of a zone's reservations are in force now.
func (c *Core) inForce(z config.Zone, now time.Time) map[string]bool {
	out := map[string]bool{}
	if z.Room == nil {
		return out
	}
	c.room.mu.Lock()
	defer c.room.mu.Unlock()
	st := c.zoneStateLocked(z.Name)
	for _, rv := range z.Room.Reservations {
		switch {
		case !room.Conditional(rv), st.met[rv.Name]:
			out[rv.Name] = true
		default:
			// a claim stands for its grace; past it, while its condition
			// cannot be read (a hiccup is not a guest gone)
			if cl, ok := c.room.claims[z.Name][rv.Name]; ok && (now.Sub(cl.ClaimedAt) < z.Room.Grace || !st.read[rv.Name]) {
				out[rv.Name] = true
			}
		}
	}
	return out
}

// holdKey is the key every room-taking resource of the zone is held by now
// ("" = none).
func (c *Core) holdKey(z config.Zone, now time.Time) string {
	if z.Room == nil {
		return ""
	}
	if h := room.HeldBy(z.Room, c.inForce(z, now)); h != nil {
		return h.Key()
	}
	return ""
}

// admitRoom checks a request against its zone's pools, inside the admission
// transaction, and returns the hold the resource takes.
func (c *Core) admitRoom(tx *registry.Tx, zone, id string, before registry.Room, beforeHold string, after registry.Room) (string, *room.Refusal, error) {
	z, _ := c.cfg.Zone(zone)
	if z.Room == nil || (after.GuaranteedMB == 0 && after.SpotMB == 0) {
		return "", nil, nil
	}
	booked, spot, err := tx.ZoneUse(zone, id)
	if err != nil {
		return "", nil, err
	}
	now := time.Now()
	inForce := c.inForce(z, now)
	b := room.Take{GuaranteedMB: before.GuaranteedMB, SpotMB: before.SpotMB, Running: before.Running && beforeHold == ""}
	a := room.Take{GuaranteedMB: after.GuaranteedMB, SpotMB: after.SpotMB, Running: after.Running}
	if ref := room.Admit(zone, z.Room, inForce, room.Use{BookedMB: booked, SpotUsedMB: spot}, b, a); ref != nil {
		return "", ref, nil
	}
	if h := room.HeldBy(z.Room, inForce); h != nil {
		return h.Key(), nil, nil
	}
	return "", nil, nil
}

func roomOf(r *pluginpb.Room) registry.Room {
	return registry.Room{GuaranteedMB: r.GetGuaranteedMb(), SpotMB: r.GetSpotMb(), Running: r.GetRunning()}
}

func roomProblem(ref *room.Refusal) *Problem {
	p := problem(409, KindRoom, "%s", ref.Message)
	p.Room = ref
	return p
}

// ---- Surveys ----------------------------------------------------------------

// surveyZone asks the zone's plugins what its reservations wait on, whether
// it is awake and which resources carry a hold; settling (the zone's decision
// lock held), it then ends the claims past their grace and adopts the holds a
// node placed alone. false = no plugin answered (what the core knew stays).
func (c *Core) surveyZone(ctx context.Context, z config.Zone, settle bool) bool {
	var conds []*pluginpb.Condition
	var names []string
	if z.Room != nil {
		for _, rv := range z.Room.Reservations {
			switch {
			case rv.WhileRunning != "":
				conds = append(conds, &pluginpb.Condition{What: &pluginpb.Condition_GuestRunning{GuestRunning: rv.WhileRunning}})
			case rv.WhileDown != "":
				conds = append(conds, &pluginpb.Condition{What: &pluginpb.Condition_NodeDown{NodeDown: rv.WhileDown}})
			default:
				continue
			}
			names = append(names, rv.Name)
		}
	}
	answered, met, errs := make([]bool, len(conds)), make([]bool, len(conds)), make([]string, len(conds))
	holds := map[string]string{}
	awake, any := false, false
	for _, p := range c.host.Plugins() {
		if st, ok := p.Zones[z.Name]; !ok || st.Error != "" {
			continue
		}
		client, err := p.Client(ctx)
		if err != nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		resp, err := client.Survey(cctx, &pluginpb.SurveyRequest{Zone: z.Name, Conditions: conds})
		cancel()
		if status.Code(err) == codes.Unimplemented {
			continue
		}
		if err != nil {
			c.log.Warn("survey: the plugin could not answer", "zone", z.Name, "plugin", p.Name, "err", status.Convert(err).Message())
			continue
		}
		any, awake = true, awake || resp.GetAwake()
		for i, cs := range resp.GetConditions() {
			if i >= len(conds) || answered[i] {
				continue
			}
			if cs.GetError() != "" {
				errs[i] = cs.GetError()
				continue
			}
			answered[i], met[i], errs[i] = true, cs.GetMet(), ""
		}
		maps.Copy(holds, resp.GetHolds())
	}
	if !any {
		return false
	}
	now := time.Now()
	c.room.mu.Lock()
	s := c.zoneStateLocked(z.Name)
	s.known, s.awake, s.surveyed = true, awake, now
	for i, n := range names {
		s.read[n] = answered[i]
		if answered[i] {
			s.met[n] = met[i]
		}
		if errs[i] != s.errs[n] {
			if errs[i] != "" {
				c.log.Warn("survey: a reservation's condition cannot be read; what was known stays", "zone", z.Name, "reservation", n, "err", errs[i])
			}
			s.errs[n] = errs[i]
		}
	}
	c.room.mu.Unlock()
	if z.Room != nil && settle {
		c.expireClaims(ctx, z, now)
		c.adoptHolds(ctx, z, holds, now)
	}
	return true
}

// expireClaims ends the claims past the zone's grace whose condition was read
// false: the release that never came (a node that lost its power mid-run). A
// condition that could not be read ends nothing — a hiccup of the engine's
// API is not a guest gone.
func (c *Core) expireClaims(ctx context.Context, z config.Zone, now time.Time) {
	c.room.mu.Lock()
	var stale []registry.Claim
	st := c.zoneStateLocked(z.Name)
	for name, cl := range c.room.claims[z.Name] {
		if st.read[name] && !st.met[name] && now.Sub(cl.ClaimedAt) >= z.Room.Grace {
			stale = append(stale, cl)
		}
	}
	c.room.mu.Unlock()
	for _, cl := range stale {
		if err := c.store.Tx(ctx, func(tx *registry.Tx) error { return tx.DeleteClaim(cl.Zone, cl.Reservation) }); err != nil {
			c.log.Error("the end of a claim could not be written", "zone", cl.Zone, "reservation", cl.Reservation, "err", err)
			continue
		}
		c.dropClaim(cl.Zone, cl.Reservation)
		c.audit.Write(audit.Event{Action: "room", Actor: "hangar", Zone: z.Name, Result: "claim-expired",
			Detail: fmt.Sprintf("the claim on %s (by %s at %s) ended: its condition does not hold, %s after", cl.Reservation, cl.By,
				cl.ClaimedAt.Format(time.RFC3339), z.Room.Grace), Fields: map[string]string{"reservation": cl.Reservation}})
	}
}

// adoptHolds turns a hold the engine's node placed alone into a claim, when
// the core never placed it: the hook could not reach the brain, and did what
// the brain would have.
func (c *Core) adoptHolds(ctx context.Context, z config.Zone, holds map[string]string, now time.Time) {
	for _, id := range slices.Sorted(maps.Keys(holds)) {
		rv := room.ByKey(z.Room, holds[id])
		if rv == nil || c.inForce(z, now)[rv.Name] {
			continue
		}
		r, err := c.store.Resource(ctx, id)
		if err != nil || r.Zone != z.Name || r.Hold != "" {
			continue // unknown, or a hold the core itself placed
		}
		name := rv.Name
		if err := c.store.Tx(ctx, func(tx *registry.Tx) error { return tx.PutClaim(z.Name, name, "node") }); err != nil {
			c.log.Error("a node's hold could not be adopted", "zone", z.Name, "reservation", name, "err", err)
			continue
		}
		c.setClaim(registry.Claim{Zone: z.Name, Reservation: name, By: "node", ClaimedAt: now})
		c.audit.Write(audit.Event{Action: "room", Actor: "hangar", Zone: z.Name, Resource: id, Result: "claim-adopted",
			Detail: fmt.Sprintf("the engine's node held %s for %s on its own: the brain takes it as a claim", id, name),
			Fields: map[string]string{"reservation": name}})
	}
}

// ---- Holds ------------------------------------------------------------------

// holdZone writes on every room-taking resource of the zone the hold the zone
// calls for now, and returns those whose hold changed.
func (c *Core) holdZone(ctx context.Context, z config.Zone) ([]string, error) {
	if z.Room == nil {
		return nil, nil
	}
	key := c.holdKey(z, time.Now())
	var changed []string
	var after int64
	for {
		rs, next, err := c.store.Resources(ctx, registry.Filter{Zone: z.Name, After: after, Limit: 500})
		if err != nil {
			return changed, err
		}
		for _, r := range rs {
			if (r.Room.GuaranteedMB == 0 && r.Room.SpotMB == 0) || r.Hold == key {
				continue
			}
			k := key
			if err := c.store.Tx(ctx, func(tx *registry.Tx) error { return tx.Update(r.ID, registry.Change{Hold: &k}) }); err != nil {
				return changed, err
			}
			changed = append(changed, r.ID)
		}
		if next == 0 {
			break
		}
		after = next
	}
	if len(changed) > 0 {
		e := audit.Event{Action: "room", Actor: "hangar", Zone: z.Name, Result: "released",
			Detail: fmt.Sprintf("the borrowed room is back: %d resource(s) released", len(changed))}
		if rv := room.ByKey(z.Room, key); rv != nil {
			e.Result = "held"
			e.Detail = fmt.Sprintf("the borrowed room is held for %s (%s): %d resource(s) held", rv.Name, room.Condition(*rv), len(changed))
			e.Fields = map[string]string{"reservation": rv.Name}
		}
		c.audit.Write(e)
	}
	return changed, nil
}

// HoldResult is what bringing one resource to its hold did.
type HoldResult struct {
	Resource string `json:"resource"`
	// Result: the reconcile's verdict (in_sync, repaired, drifted, missing),
	// or "skipped" for a resource in the middle of something else.
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
}

// reconcileNow brings resources to their holds at once, side by side, each
// waiting for any operation on it to end first.
func (c *Core) reconcileNow(ctx context.Context, ids []string) []HoldResult {
	out := make([]HoldResult, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu := c.lock(id)
			mu.Lock()
			defer mu.Unlock()
			out[i] = HoldResult{Resource: id}
			r, err := c.store.Resource(ctx, id)
			switch {
			case err != nil:
				out[i].Result, out[i].Detail = "skipped", err.Error()
			case r.State != registry.Ready && r.State != registry.Lost:
				out[i].Result, out[i].Detail = "skipped", "it is "+r.State+"; the next pass brings it there"
			default:
				out[i].Result, out[i].Detail = c.reconcileLocked(ctx, r)
			}
		}()
	}
	wg.Wait()
	return out
}

// ---- Claims -----------------------------------------------------------------

// ClaimResult is what a claim or a release did.
type ClaimResult struct {
	Zone        string `json:"zone"`
	Reservation string `json:"reservation"`
	Condition   string `json:"condition"`
	// HeldBy: the reservation holding the zone's borrowed room after the
	// call ("" = none).
	HeldBy    string       `json:"held_by,omitempty"`
	Resources []HoldResult `json:"resources"`
}

// Claim puts in force the reservation that waits on a guest running — its
// hook calls it before the guest starts — and holds what the zone lends; a
// release (claim false) ends it after the guest stopped, and lifts the holds.
// The holds are applied before it answers, and finish even if the caller has
// stopped waiting.
func (c *Core) Claim(ctx context.Context, who *Caller, zone, guest string, claim bool) (*ClaimResult, error) {
	rec := audit.From(ctx)
	rec.Set(func(e *audit.Event) { e.Zone, e.Fields = zone, map[string]string{"guest": guest} })
	if !slices.Contains(who.Scopes, identity.ScopeRoom) && !who.Can(identity.ScopeWrite) {
		return nil, c.refused(ctx, problem(403, KindScope, "this token cannot claim room"))
	}
	if !who.Tier.Room && !who.Tier.Operator {
		return nil, c.refused(ctx, problem(403, KindScope, "tier %s claims no room: that is the tier of the guests' hooks (room: true)", who.Tier.Name))
	}
	z, ok := c.cfg.Zone(zone)
	if !ok || !limits.ZoneOpen(who.Tier, zone) {
		return nil, c.refused(ctx, problem(403, KindZone, "zone %q is not open to tier %s", zone, who.Tier.Name))
	}
	if z.Room == nil {
		return nil, c.refused(ctx, problem(404, KindNotFound, "zone %s keeps no room", zone))
	}
	rv := room.ForGuest(z.Room, guest)
	if rv == nil {
		return nil, c.refused(ctx, problem(404, KindNotFound, "zone %s keeps no room for guest %s", zone, guest))
	}
	rec.Set(func(e *audit.Event) { e.Fields["reservation"] = rv.Name })
	defer c.decide(zone)()
	err := c.store.Tx(ctx, func(tx *registry.Tx) error {
		if claim {
			return tx.PutClaim(zone, rv.Name, who.Subject)
		}
		return tx.DeleteClaim(zone, rv.Name)
	})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if claim {
		c.setClaim(registry.Claim{Zone: zone, Reservation: rv.Name, By: who.Subject, ClaimedAt: now})
	} else {
		c.dropClaim(zone, rv.Name)
		c.room.mu.Lock()
		c.zoneStateLocked(zone).met[rv.Name] = false // its guest stopped: the hook's word beats the last survey
		c.room.mu.Unlock()
	}
	// the holds finish even if the hook stopped waiting: it then does the
	// same on its own, and the two meet
	hctx, cancel := context.WithTimeout(c.life, 2*time.Minute)
	defer cancel()
	changed, err := c.holdZone(hctx, z)
	if err != nil {
		return nil, err
	}
	if BetweenHoldsAndEngine != nil {
		BetweenHoldsAndEngine()
	}
	res := &ClaimResult{Zone: zone, Reservation: rv.Name, Condition: room.Condition(*rv), Resources: c.reconcileNow(hctx, changed)}
	if h := room.HeldBy(z.Room, c.inForce(z, time.Now())); h != nil {
		res.HeldBy = h.Name
	}
	rec.Set(func(e *audit.Event) {
		e.Result = map[bool]string{true: "claimed", false: "released"}[claim]
		e.Detail = fmt.Sprintf("%d resource(s) brought to the zone's room", len(changed))
	})
	return res, nil
}

// ---- Waking -----------------------------------------------------------------

// awake reports whether a zone is known awake (false when it is not known).
func (c *Core) awake(zone string) (awake, known bool) {
	c.room.mu.Lock()
	defer c.room.mu.Unlock()
	s := c.zoneStateLocked(zone)
	return s.awake, s.known
}

// BetweenHoldsAndEngine, when set (tests only), runs in a claim or a release
// after the holds are written and before the engine is brought to them — the
// moment a pass's survey once read the core's own tag, not yet taken off, as
// a node acting alone (the zone's decision lock keeps it out).
var BetweenHoldsAndEngine func()

// WakePoll is how often a zone being woken is asked whether it is awake.
var WakePoll = 5 * time.Second

// wakeZone makes sure a zone that sleeps is awake before something starts in
// it: asked first, then woken by its webhook, then asked until it answers or
// its timeout ends.
func (c *Core) wakeZone(ctx context.Context, z config.Zone) error {
	if z.Wake == nil {
		return nil
	}
	if c.surveyZone(ctx, z, false) {
		if up, _ := c.awake(z.Name); up {
			return nil
		}
	}
	if err := c.callWake(ctx, z); err != nil {
		return err
	}
	c.audit.Write(audit.Event{Action: "room", Actor: "hangar", Zone: z.Name, Result: "wake", Detail: "the zone was asleep: its wake was called"})
	deadline := time.Now().Add(z.Wake.Timeout)
	for {
		if !c.surveyZone(ctx, z, false) {
			return nil // nothing can tell: the webhook's word is all there is
		}
		if up, _ := c.awake(z.Name); up {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("zone %s did not wake within %s of its wake call", z.Name, z.Wake.Timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(WakePoll):
		}
	}
}

func (c *Core) callWake(ctx context.Context, z config.Zone) error {
	w := z.Wake
	var body io.Reader
	if w.Body != "" {
		body = strings.NewReader(w.Body)
	}
	cctx, cancel := context.WithTimeout(ctx, w.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, w.Method, w.URL, body)
	if err != nil {
		return fmt.Errorf("zone %s's wake: %w", z.Name, err)
	}
	if w.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for h, s := range w.Headers {
		v, err := s.Read()
		if err != nil {
			return fmt.Errorf("zone %s's wake, header %s: %w", z.Name, h, err)
		}
		req.Header.Set(h, string(v))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("zone %s's wake did not answer: %w", z.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("zone %s's wake answered %s: %s", z.Name, resp.Status, bytes.TrimSpace(b))
	}
	return nil
}

// ---- What the zones' listing shows -------------------------------------------

// RoomView is a zone's room as the listing shows it, in MiB.
type RoomView struct {
	room.Pools
	BookedMB   int64 `json:"booked_mb"`
	SpotUsedMB int64 `json:"spot_used_mb"`
	// HeldBy: the reservation holding the borrowed room now.
	HeldBy       string            `json:"held_by,omitempty"`
	Reservations []ReservationView `json:"reservations"`
}

// ReservationView is one reservation and its state.
type ReservationView struct {
	Name      string          `json:"name"`
	MemoryMB  int64           `json:"memory_mb"`
	Condition string          `json:"condition"`
	InForce   bool            `json:"in_force"`
	Claim     *registry.Claim `json:"claim,omitempty"`
}

func (c *Core) roomView(ctx context.Context, z config.Zone) (*RoomView, error) {
	booked, spot, err := c.store.ZoneUse(ctx, z.Name)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	inForce := c.inForce(z, now)
	v := &RoomView{Pools: room.Of(z.Room), BookedMB: booked, SpotUsedMB: spot, Reservations: []ReservationView{}}
	if h := room.HeldBy(z.Room, inForce); h != nil {
		v.HeldBy = h.Name
	}
	c.room.mu.Lock()
	defer c.room.mu.Unlock()
	for _, rv := range z.Room.Reservations {
		r := ReservationView{Name: rv.Name, MemoryMB: int64(rv.MemoryGB) * 1024, Condition: room.Condition(rv), InForce: inForce[rv.Name]}
		if cl, ok := c.room.claims[z.Name][rv.Name]; ok {
			r.Claim = &cl
		}
		v.Reservations = append(v.Reservations, r)
	}
	return v, nil
}
