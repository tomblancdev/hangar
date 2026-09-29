// Package core is the brain: one flow for every request (ARCHITECTURE.md,
// "The ask") — signed in, in a tier, the zone open to it, the request valid,
// the plugin's plan within the tier's limits — then an operation that the
// plugin carries out, and a loop that keeps the registry honest against the
// engines.
//
// The core never knows what a resource is. It knows ids, owners, zones,
// states, what each resource holds per dimension, and which plugin answers
// for its type.
package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tomblancdev/hangar/internal/audit"
	"github.com/tomblancdev/hangar/internal/config"
	"github.com/tomblancdev/hangar/internal/identity"
	"github.com/tomblancdev/hangar/internal/ids"
	"github.com/tomblancdev/hangar/internal/limits"
	"github.com/tomblancdev/hangar/internal/metrics"
	"github.com/tomblancdev/hangar/internal/plugins"
	"github.com/tomblancdev/hangar/internal/registry"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// Core is the brain.
type Core struct {
	cfg     *config.Config
	store   *registry.Store
	host    *plugins.Host
	audit   *audit.Log
	log     *slog.Logger
	metrics *metrics.Set

	// Retry is how long to wait before each new try of a call the engine
	// could not answer (UNAVAILABLE); its length is the number of retries.
	Retry []time.Duration
	// CallTimeout bounds one call to a plugin during an operation.
	CallTimeout time.Duration

	admit sync.Mutex // one admission at a time: the arithmetic stays true
	life  context.Context
	wg    sync.WaitGroup
	slots chan struct{}

	locks sync.Map // resource id -> *sync.Mutex: an operation and a reconcile never overlap

	waitMu  sync.Mutex
	waiters map[string]*waiter
}

type waiter struct {
	ch chan struct{}
	n  int
}

// New returns a core. Operations run under life: when it ends they stop
// where they are, stay running in the registry, and resume at the next start.
func New(life context.Context, cfg *config.Config, store *registry.Store, host *plugins.Host, a *audit.Log, log *slog.Logger, m *metrics.Set) *Core {
	m.Counter("hangar_requests_refused_total", "Requests refused, by reason.", "reason")
	m.Counter("hangar_operations_total", "Operations finished, by kind and result.", "kind", "result")
	m.Counter("hangar_reconcile_total", "Reconcile verdicts, by type and verdict.", "type", "verdict")
	return &Core{
		cfg: cfg, store: store, host: host, audit: a, log: log, metrics: m,
		Retry:       []time.Duration{time.Second, 4 * time.Second, 15 * time.Second},
		CallTimeout: 15 * time.Minute,
		life:        life,
		slots:       make(chan struct{}, 8),
		waiters:     map[string]*waiter{},
	}
}

// ---- Problems: every refusal says why, with the numbers ---------------------

// Problem is a refused request, as the API returns it (RFC 9457).
type Problem struct {
	Status     int                 `json:"status"`
	Kind       string              `json:"kind"`
	Detail     string              `json:"detail"`
	Violations []plugins.Violation `json:"violations,omitempty"`
	Refusals   []limits.Refusal    `json:"refusals,omitempty"`
	Operation  string              `json:"operation,omitempty"`
}

func (p *Problem) Error() string { return p.Detail }

// errOf returns a problem as an error — nil when there is none. A nil
// *Problem passed straight into an error is a non-nil interface holding a nil
// pointer: the handler would read it as a failure and dereference nothing.
func errOf(p *Problem) error {
	if p == nil {
		return nil
	}
	return p
}

func problem(status int, kind, format string, a ...any) *Problem {
	return &Problem{Status: status, Kind: kind, Detail: fmt.Sprintf(format, a...)}
}

// The kinds a problem can be; the API turns each into a type URI.
const (
	KindSignIn      = "sign-in"
	KindNoTier      = "no-tier"
	KindScope       = "scope"
	KindNotFound    = "not-found"
	KindBadRequest  = "bad-request"
	KindZone        = "zone"
	KindUnavailable = "unavailable-here"
	KindSchema      = "schema"
	KindPlugin      = "plugin-refused"
	KindLimit       = "limit"
	KindBusy        = "busy"
	KindConflict    = "conflict"
	KindEngine      = "engine"
	KindDown        = "plugin-down"
	KindInternal    = "internal"
)

// fromPlugin turns a plugin's gRPC status into a problem.
func fromPlugin(plugin string, err error) *Problem {
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.InvalidArgument:
		return problem(422, KindPlugin, "%s", st.Message())
	case codes.FailedPrecondition:
		return problem(409, KindEngine, "%s", st.Message())
	case codes.Unavailable:
		return problem(503, KindDown, "plugin %s: %s", plugin, st.Message())
	}
	return problem(502, KindDown, "plugin %s failed: %s", plugin, st.Message())
}

func (c *Core) refused(ctx context.Context, p *Problem) *Problem {
	reason := p.Kind
	if len(p.Refusals) > 0 {
		reason = p.Refusals[0].Reason
	}
	c.metrics.Inc("hangar_requests_refused_total", reason)
	audit.From(ctx).Set(func(e *audit.Event) { e.Result, e.Reason, e.Detail = "refused", reason, p.Detail })
	return p
}

// ---- The caller ---------------------------------------------------------

// Caller is a signed-in person with their tier.
type Caller struct {
	*identity.Identity
	Tier *config.Tier
}

// Caller finds the tier of an identity: the first, in the file's order, one
// of its groups reaches.
func (c *Core) Caller(ctx context.Context, id *identity.Identity) (*Caller, *Problem) {
	t, ok := limits.For(c.cfg.Tiers, id.Groups)
	if !ok {
		return nil, c.refused(ctx, problem(403, KindNoTier, "signed in as %s, but none of your groups is in a tier here", id.Subject))
	}
	audit.From(ctx).Set(func(e *audit.Event) { e.Tier = t.Name })
	return &Caller{Identity: id, Tier: t}, nil
}

func (c *Core) needWrite(ctx context.Context, who *Caller) *Problem {
	if !who.Can(identity.ScopeWrite) {
		return c.refused(ctx, problem(403, KindScope, "this token is read-only"))
	}
	return nil
}

// visible: a person sees their own; an operator sees everyone's.
func visible(who *Caller, owner string) bool { return who.Tier.Operator || owner == who.Subject }

// ---- Create -------------------------------------------------------------

// CreateInput is a create request.
type CreateInput struct {
	Zone        string            `json:"zone"`
	Spec        json.RawMessage   `json:"spec"`
	Tags        map[string]string `json:"tags"`
	ClientToken string            `json:"client_token"`
}

// Create asks for a new resource. replayed=true: the client token was used
// before for this very request, and its operation is returned as it stands.
func (c *Core) Create(ctx context.Context, who *Caller, typeName string, in CreateInput) (op *registry.Operation, r *registry.Resource, replayed bool, err error) {
	if p := c.needWrite(ctx, who); p != nil {
		return nil, nil, false, p
	}
	rec := audit.From(ctx)
	rec.Set(func(e *audit.Event) { e.Type, e.Zone = typeName, in.Zone })
	t := c.host.Type(typeName)
	if t == nil {
		return nil, nil, false, c.refused(ctx, problem(404, KindNotFound, "no type %q here", typeName))
	}
	if p := c.zoneFor(ctx, who, t, in.Zone); p != nil {
		return nil, nil, false, p
	}
	if p := checkTags(in.Tags); p != nil {
		return nil, nil, false, c.refused(ctx, p)
	}
	if len(in.Spec) == 0 {
		in.Spec = json.RawMessage("{}")
	}
	hash := requestHash("create", typeName, in.Zone, in.Spec, in.Tags)
	if in.ClientToken != "" {
		if op, r, p := c.replay(ctx, who, in.ClientToken, hash); p != nil || op != nil {
			return op, r, op != nil, errOf(p)
		}
	}
	if v := t.Validate(in.Spec); len(v) > 0 {
		p := problem(422, KindSchema, "the spec does not fit type %s: %s", t.Name, v[0].Reason)
		p.Violations = v
		return nil, nil, false, c.refused(ctx, p)
	}

	plan, p := c.plan(ctx, t, &pluginpb.PlanRequest{Type: t.Name, Zone: in.Zone, Spec: in.Spec})
	if p != nil {
		return nil, nil, false, c.refused(ctx, p)
	}

	r = &registry.Resource{
		ID: ids.New(t.Prefix), Type: t.Name, Plugin: t.Plugin, Owner: who.Subject, Zone: in.Zone,
		State: registry.Creating, Spec: plan.GetSpec(), Choices: plan.GetChoices(), Usage: plan.GetUsage(), Tags: in.Tags,
	}
	op = &registry.Operation{
		ID: ids.New(ids.Operation), Owner: who.Subject, ResourceID: r.ID, Kind: registry.OpCreate,
		ClientToken: in.ClientToken, RequestHash: hash,
	}
	var refusals []limits.Refusal
	c.admit.Lock()
	err = c.store.Tx(ctx, func(tx *registry.Tx) error {
		if in.ClientToken != "" {
			if prev, err := tx.OperationByToken(who.Subject, in.ClientToken); err != nil || prev != nil {
				if err == nil {
					err = errReplayRace
				}
				return err
			}
		}
		used, err := tx.Usage(who.Subject)
		if err != nil {
			return err
		}
		if refusals = limits.Admit(who.Tier, c.host.Dimensions(), used, nil, r.Usage, nil, r.Choices); len(refusals) > 0 {
			return errRefused
		}
		if err := tx.InsertResource(r); err != nil {
			return err
		}
		return tx.InsertOperation(op)
	})
	c.admit.Unlock()
	switch {
	case errors.Is(err, errRefused):
		return nil, nil, false, c.refused(ctx, limitProblem(refusals))
	case errors.Is(err, errReplayRace):
		// the same token arrived twice at once; the other one won
		op, r, p := c.replay(ctx, who, in.ClientToken, hash)
		if p != nil {
			return nil, nil, false, p
		}
		return op, r, true, nil
	case err != nil:
		return nil, nil, false, err
	}
	rec.Set(func(e *audit.Event) { e.Resource, e.Operation, e.Result = r.ID, op.ID, "accepted" })
	c.start(op)
	return op, r, false, nil
}

var (
	errRefused    = errors.New("refused")
	errReplayRace = errors.New("client token raced")
)

func limitProblem(rs []limits.Refusal) *Problem {
	p := problem(403, KindLimit, "%s", rs[0].Message)
	p.Refusals = rs
	return p
}

// zoneFor checks the zone exists, is open to the tier and can host the type.
func (c *Core) zoneFor(ctx context.Context, who *Caller, t *plugins.Type, zone string) *Problem {
	if zone == "" {
		return c.refused(ctx, problem(400, KindBadRequest, "name a zone"))
	}
	if _, ok := c.cfg.Zone(zone); !ok || !limits.ZoneOpen(who.Tier, zone) {
		open := []string{}
		for _, z := range c.cfg.Zones {
			if limits.ZoneOpen(who.Tier, z.Name) {
				open = append(open, z.Name)
			}
		}
		return c.refused(ctx, problem(403, KindZone, "zone %q is not open to tier %s (open: %s)", zone, who.Tier.Name, strings.Join(open, ", ")))
	}
	if ok, why := c.host.Available(t, zone); !ok {
		return c.refused(ctx, problem(422, KindUnavailable, "%s", why))
	}
	return nil
}

// replay returns the operation a client token was already used for, or a
// conflict when it was used for a different request. Nothing = (nil, nil, nil).
func (c *Core) replay(ctx context.Context, who *Caller, token, hash string) (*registry.Operation, *registry.Resource, *Problem) {
	var prev *registry.Operation
	err := c.store.Tx(ctx, func(tx *registry.Tx) error {
		var err error
		prev, err = tx.OperationByToken(who.Subject, token)
		return err
	})
	if err != nil {
		return nil, nil, problem(500, KindInternal, "%v", err)
	}
	if prev == nil {
		return nil, nil, nil
	}
	if prev.RequestHash != hash {
		return nil, nil, c.refused(ctx, problem(409, KindConflict, "client token %q was used for a different request (operation %s)", token, prev.ID))
	}
	r, err := c.store.Resource(ctx, prev.ResourceID)
	if err != nil {
		return nil, nil, problem(500, KindInternal, "%v", err)
	}
	audit.From(ctx).Set(func(e *audit.Event) { e.Resource, e.Operation, e.Result = r.ID, prev.ID, "replayed" })
	return prev, r, nil
}

// plan asks the plugin what a request would hold, and checks the answer.
func (c *Core) plan(ctx context.Context, t *plugins.Type, req *pluginpb.PlanRequest) (*pluginpb.PlanResponse, *Problem) {
	client, err := c.host.Plugin(t.Plugin).Client(ctx)
	if err != nil {
		return nil, problem(503, KindDown, "%v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	plan, err := client.Plan(cctx, req)
	if err != nil {
		return nil, fromPlugin(t.Plugin, err)
	}
	if rs := plan.GetRefusals(); len(rs) > 0 {
		p := problem(422, KindPlugin, "%s", rs[0].GetReason())
		for _, r := range rs {
			p.Violations = append(p.Violations, plugins.Violation{Field: r.GetField(), Reason: r.GetReason()})
		}
		return nil, p
	}
	dims := c.host.Dimensions()
	for d := range plan.GetUsage() {
		if dm, ok := dims[d]; !ok || dm.Plugin != t.Plugin || dm.Kind != limits.Quantity {
			return nil, problem(502, KindDown, "plugin %s planned %q, a quantity it never declared", t.Plugin, d)
		}
	}
	for d := range plan.GetChoices() {
		if dm, ok := dims[d]; !ok || dm.Plugin != t.Plugin || dm.Kind != limits.Choice {
			return nil, problem(502, KindDown, "plugin %s planned %q, a choice it never declared", t.Plugin, d)
		}
	}
	if !json.Valid(plan.GetSpec()) {
		return nil, problem(502, KindDown, "plugin %s planned a spec that is not JSON", t.Plugin)
	}
	return plan, nil
}

func checkTags(tags map[string]string) *Problem {
	if len(tags) > 50 {
		return problem(422, KindBadRequest, "at most 50 tags")
	}
	for k, v := range tags {
		switch {
		case k == "" || len(k) > 128:
			return problem(422, KindBadRequest, "a tag's key is 1 to 128 characters")
		case len(v) > 256:
			return problem(422, KindBadRequest, "tag %s: a value is at most 256 characters", k)
		case strings.HasPrefix(strings.ToLower(k), "hangar"):
			return problem(422, KindBadRequest, "tag %s: keys starting with hangar are the core's", k)
		}
	}
	return nil
}

// requestHash identifies a request, so a client token cannot be reused for a
// different one. The JSON is re-encoded to be compared by content.
func requestHash(parts ...any) string {
	h := sha256.New()
	for _, p := range parts {
		if raw, ok := p.(json.RawMessage); ok {
			var v any
			if json.Unmarshal(raw, &v) == nil {
				p = v
			}
		}
		b, _ := json.Marshal(p)
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---- Delete and actions -------------------------------------------------

// resourceFor finds a resource the caller may see and act on.
func (c *Core) resourceFor(ctx context.Context, who *Caller, id string) (*registry.Resource, *plugins.Type, *Problem) {
	audit.From(ctx).Set(func(e *audit.Event) { e.Resource = id })
	r, err := c.store.Resource(ctx, id)
	if errors.Is(err, registry.ErrNotFound) || (err == nil && !visible(who, r.Owner)) {
		return nil, nil, c.refused(ctx, problem(404, KindNotFound, "no resource %s", id))
	}
	if err != nil {
		return nil, nil, problem(500, KindInternal, "%v", err)
	}
	audit.From(ctx).Set(func(e *audit.Event) { e.Type, e.Zone = r.Type, r.Zone })
	t := c.host.Type(r.Type)
	if t == nil {
		return nil, nil, c.refused(ctx, problem(503, KindDown, "no enabled plugin answers for type %s", r.Type))
	}
	return r, t, nil
}

func (c *Core) busy(ctx context.Context, r *registry.Resource) *Problem {
	if !slices.Contains(registry.Busy, r.State) {
		return nil
	}
	p := problem(409, KindBusy, "%s is %s: wait for its operation", r.ID, r.State)
	if ops, err := c.store.Operations(ctx, "", r.ID, 1); err == nil && len(ops) > 0 {
		p.Operation = ops[0].ID
	}
	return c.refused(ctx, p)
}

// Delete asks for a resource to go.
func (c *Core) Delete(ctx context.Context, who *Caller, id, clientToken string) (*registry.Operation, *registry.Resource, bool, error) {
	if p := c.needWrite(ctx, who); p != nil {
		return nil, nil, false, p
	}
	r, _, p := c.resourceFor(ctx, who, id)
	if p != nil {
		return nil, nil, false, p
	}
	if r.State == registry.Deleted {
		return nil, nil, false, c.refused(ctx, problem(404, KindNotFound, "%s was deleted at %s", r.ID, r.DeletedAt.Format(time.RFC3339)))
	}
	hash := requestHash("delete", id)
	if clientToken != "" {
		if op, r, p := c.replay(ctx, who, clientToken, hash); p != nil || op != nil {
			return op, r, op != nil, errOf(p)
		}
	}
	if p := c.busy(ctx, r); p != nil {
		return nil, nil, false, p
	}
	op := &registry.Operation{
		ID: ids.New(ids.Operation), Owner: who.Subject, ResourceID: r.ID, Kind: registry.OpDelete,
		Params: json.RawMessage(`{"from_state":"` + r.State + `"}`), ClientToken: clientToken, RequestHash: hash,
	}
	err := c.store.Tx(ctx, func(tx *registry.Tx) error {
		if err := tx.Update(r.ID, registry.Change{State: registry.Deleting, IfState: r.State}); err != nil {
			return err
		}
		return tx.InsertOperation(op)
	})
	if errors.Is(err, registry.ErrMoved) {
		return nil, nil, false, c.refused(ctx, problem(409, KindBusy, "%s changed while you asked: try again", r.ID))
	}
	if err != nil {
		return nil, nil, false, err
	}
	r.State = registry.Deleting
	audit.From(ctx).Set(func(e *audit.Event) { e.Operation, e.Result = op.ID, "accepted" })
	c.start(op)
	return op, r, false, nil
}

// ActInput is an action request.
type ActInput struct {
	Params      json.RawMessage `json:"params"`
	ClientToken string          `json:"client_token"`
}

// Act asks for one of a type's actions.
func (c *Core) Act(ctx context.Context, who *Caller, id, action string, in ActInput) (*registry.Operation, *registry.Resource, bool, error) {
	if p := c.needWrite(ctx, who); p != nil {
		return nil, nil, false, p
	}
	r, t, p := c.resourceFor(ctx, who, id)
	if p != nil {
		return nil, nil, false, p
	}
	audit.From(ctx).Set(func(e *audit.Event) { e.Fields = map[string]string{"action": action} })
	a := t.Action(action)
	if a == nil {
		names := []string{}
		for _, x := range t.Actions {
			names = append(names, x.Name)
		}
		return nil, nil, false, c.refused(ctx, problem(404, KindNotFound, "type %s has no action %q (it has: %s)", t.Name, action, strings.Join(names, ", ")))
	}
	if len(in.Params) == 0 {
		in.Params = json.RawMessage("{}")
	}
	hash := requestHash("action", id, action, in.Params)
	if in.ClientToken != "" {
		if op, r, p := c.replay(ctx, who, in.ClientToken, hash); p != nil || op != nil {
			return op, r, op != nil, errOf(p)
		}
	}
	switch r.State {
	case registry.Ready:
	case registry.Lost:
		return nil, nil, false, c.refused(ctx, problem(409, KindEngine, "%s is lost: its engine no longer has it — delete it", r.ID))
	default:
		if p := c.busy(ctx, r); p != nil {
			return nil, nil, false, p
		}
		return nil, nil, false, c.refused(ctx, problem(409, KindEngine, "%s is %s", r.ID, r.State))
	}
	if ok, why := c.host.ActionAvailable(t, a, r.Zone); !ok {
		return nil, nil, false, c.refused(ctx, problem(422, KindUnavailable, "%s", why))
	}
	if v := a.Validate(in.Params); len(v) > 0 {
		p := problem(422, KindSchema, "the params do not fit %s: %s", a.Name, v[0].Reason)
		p.Violations = v
		return nil, nil, false, c.refused(ctx, p)
	}
	change := registry.Change{State: registry.Updating, IfState: registry.Ready}
	var plan *pluginpb.PlanResponse
	if a.ChangesUsage {
		plan, p = c.plan(ctx, t, &pluginpb.PlanRequest{Type: t.Name, Zone: r.Zone, Action: a.Name, Params: in.Params, Current: toProto(r)})
		if p != nil {
			return nil, nil, false, c.refused(ctx, p)
		}
		change.Spec, change.Usage, change.Choices = plan.GetSpec(), plan.GetUsage(), plan.GetChoices()
		if change.Usage == nil {
			change.Usage = map[string]int64{}
		}
		if change.Choices == nil {
			change.Choices = map[string]string{}
		}
	}
	op := &registry.Operation{
		ID: ids.New(ids.Operation), Owner: who.Subject, ResourceID: r.ID, Kind: registry.OpAction, Action: a.Name,
		Params: in.Params, ClientToken: in.ClientToken, RequestHash: hash,
	}
	var refusals []limits.Refusal
	c.admit.Lock()
	err := c.store.Tx(ctx, func(tx *registry.Tx) error {
		if plan != nil {
			// the owner's usage, counted in the transaction — the resource's
			// own share is in it, which is why only the growth is admitted
			used, err := tx.Usage(r.Owner)
			if err != nil {
				return err
			}
			if refusals = limits.Admit(who.Tier, c.host.Dimensions(), used, r.Usage, change.Usage, r.Choices, change.Choices); len(refusals) > 0 {
				return errRefused
			}
		}
		if err := tx.Update(r.ID, change); err != nil {
			return err
		}
		return tx.InsertOperation(op)
	})
	c.admit.Unlock()
	switch {
	case errors.Is(err, errRefused):
		return nil, nil, false, c.refused(ctx, limitProblem(refusals))
	case errors.Is(err, registry.ErrMoved):
		return nil, nil, false, c.refused(ctx, problem(409, KindBusy, "%s changed while you asked: try again", r.ID))
	case err != nil:
		return nil, nil, false, err
	}
	audit.From(ctx).Set(func(e *audit.Event) { e.Operation, e.Result = op.ID, "accepted" })
	// what the resource was, to give back if the action fails
	before := *r
	c.start(op, &before)
	r.State = registry.Updating
	return op, r, false, nil
}

// ---- Reads --------------------------------------------------------------

// Get returns a resource the caller may see.
func (c *Core) Get(ctx context.Context, who *Caller, id string) (*registry.Resource, error) {
	r, _, p := c.resourceFor(ctx, who, id)
	if p != nil {
		return nil, p
	}
	return r, nil
}

// List lists resources: a person's own; an operator's choice of owner, or all.
func (c *Core) List(ctx context.Context, who *Caller, f registry.Filter) ([]*registry.Resource, int64, error) {
	if !who.Tier.Operator {
		f.Owner = who.Subject
	}
	return c.store.Resources(ctx, f)
}

// Operation returns an operation the caller may see, waiting up to wait for
// it to finish.
func (c *Core) Operation(ctx context.Context, who *Caller, id string, wait time.Duration) (*registry.Operation, error) {
	var w *waiter
	if wait > 0 {
		w = c.waitFor(id)
		defer c.unwait(id, w)
	}
	op, err := c.store.Operation(ctx, id)
	if errors.Is(err, registry.ErrNotFound) || (err == nil && !visible(who, op.Owner)) {
		return nil, c.refused(ctx, problem(404, KindNotFound, "no operation %s", id))
	}
	if err != nil || op.State != registry.OpRunning || w == nil {
		return op, err
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-w.ch:
	case <-t.C:
	case <-ctx.Done():
	}
	return c.store.Operation(ctx, id)
}

// Operations lists the caller's operations (an operator's: everyone's).
func (c *Core) Operations(ctx context.Context, who *Caller, resourceID string, limit int) ([]*registry.Operation, error) {
	owner := who.Subject
	if who.Tier.Operator {
		owner = ""
	}
	return c.store.Operations(ctx, owner, resourceID, limit)
}

func (c *Core) waitFor(id string) *waiter {
	c.waitMu.Lock()
	defer c.waitMu.Unlock()
	w, ok := c.waiters[id]
	if !ok {
		w = &waiter{ch: make(chan struct{})}
		c.waiters[id] = w
	}
	w.n++
	return w
}

func (c *Core) unwait(id string, w *waiter) {
	c.waitMu.Lock()
	defer c.waitMu.Unlock()
	w.n--
	if w.n == 0 && c.waiters[id] == w {
		delete(c.waiters, id)
	}
}

func (c *Core) wake(id string) {
	c.waitMu.Lock()
	defer c.waitMu.Unlock()
	if w, ok := c.waiters[id]; ok {
		close(w.ch)
		delete(c.waiters, id)
	}
}

// ---- The catalogue ------------------------------------------------------

// TypeView is a type as the doors draw it.
type TypeView struct {
	Name        string          `json:"name"`
	IDPrefix    string          `json:"id_prefix"`
	Plugin      string          `json:"plugin"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Zones       []string        `json:"zones"`
	Actions     []ActionView    `json:"actions"`
}

// ActionView is an action as the doors draw it.
type ActionView struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	ParamsSchema json.RawMessage `json:"params_schema,omitempty"`
	ChangesUsage bool            `json:"changes_usage"`
	Zones        []string        `json:"zones"`
}

// Types is the catalogue: every type, with the zones open to the caller where
// it and each of its actions can be asked for.
func (c *Core) Types(who *Caller) []TypeView {
	out := []TypeView{}
	for _, t := range c.host.Types() {
		v := TypeView{Name: t.Name, IDPrefix: t.Prefix, Plugin: t.Plugin, Title: t.Title, Description: t.Description,
			Schema: t.Schema, Zones: []string{}, Actions: []ActionView{}}
		for _, z := range c.cfg.Zones {
			if ok, _ := c.host.Available(t, z.Name); ok && limits.ZoneOpen(who.Tier, z.Name) {
				v.Zones = append(v.Zones, z.Name)
			}
		}
		for _, a := range t.Actions {
			av := ActionView{Name: a.Name, Description: a.Description, ParamsSchema: a.ParamsSchema, ChangesUsage: a.ChangesUsage, Zones: []string{}}
			for _, z := range v.Zones {
				if ok, _ := c.host.ActionAvailable(t, a, z); ok {
					av.Zones = append(av.Zones, z)
				}
			}
			v.Actions = append(v.Actions, av)
		}
		out = append(out, v)
	}
	return out
}

// ZoneView is a zone as the caller sees it.
type ZoneView struct {
	Name    string                       `json:"name"`
	Driver  string                       `json:"driver"`
	Plugins map[string]plugins.ZoneState `json:"plugins"`
}

// Zones lists the zones open to the caller.
func (c *Core) Zones(who *Caller) []ZoneView {
	out := []ZoneView{}
	for _, z := range c.cfg.Zones {
		if !limits.ZoneOpen(who.Tier, z.Name) {
			continue
		}
		v := ZoneView{Name: z.Name, Driver: z.Driver, Plugins: map[string]plugins.ZoneState{}}
		for _, p := range c.host.Plugins() {
			if st, ok := p.Zones[z.Name]; ok {
				v.Plugins[p.Name] = st
			}
		}
		out = append(out, v)
	}
	return out
}

// LimitView is one dimension of the caller's tier: its limit and their use.
type LimitView struct {
	limits.Dimension
	// Limit is a number, "unlimited", or the list of values allowed.
	Limit any   `json:"limit"`
	Used  int64 `json:"used"`
}

// Limits lists every dimension with the caller's limit and usage.
func (c *Core) Limits(ctx context.Context, who *Caller) (string, []LimitView, error) {
	used, err := c.store.Usage(ctx, who.Subject)
	if err != nil {
		return "", nil, err
	}
	dims := c.host.Dimensions()
	out := []LimitView{}
	for _, name := range slices.Sorted(maps.Keys(dims)) {
		d := dims[name]
		v := LimitView{Dimension: d, Used: used[name]}
		l, named := limits.Of(who.Tier, name)
		switch {
		case named && l.Unlimited:
			v.Limit = "unlimited"
		case d.Kind == limits.Choice:
			v.Limit = append([]string{}, l.Allowed...)
		default:
			v.Limit = l.Max
		}
		out = append(out, v)
	}
	return who.Tier.Name, out, nil
}

// ---- Operations: carried out in the background --------------------------

// Run resumes the operations the brain died during, then reconciles every
// Reconcile.Every until ctx ends, then waits for operations in flight.
func (c *Core) Run(ctx context.Context) error {
	running, err := c.store.Running(ctx)
	if err != nil {
		return err
	}
	for _, op := range running {
		c.log.Info("resuming an operation the brain stopped during", "operation", op.ID, "kind", op.Kind, "resource", op.ResourceID)
		c.start(op)
	}
	tick := time.NewTicker(c.cfg.Reconcile.Every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			c.wg.Wait()
			return nil
		case <-tick.C:
			c.ReconcileOnce(ctx)
		}
	}
}

// Wait waits for every operation in flight (tests, shutdown).
func (c *Core) Wait() { c.wg.Wait() }

// start runs an operation in the background. before is the resource as it
// was admitted from (an action gives its usage back when it fails).
func (c *Core) start(op *registry.Operation, before ...*registry.Resource) {
	var prev *registry.Resource
	if len(before) > 0 {
		prev = before[0]
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.slots <- struct{}{}
		defer func() { <-c.slots }()
		c.execute(op, prev)
	}()
}

func (c *Core) lock(id string) *sync.Mutex {
	m, _ := c.locks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (c *Core) execute(op *registry.Operation, prev *registry.Resource) {
	ctx := c.life
	mu := c.lock(op.ResourceID)
	mu.Lock()
	defer mu.Unlock()

	r, err := c.store.Resource(ctx, op.ResourceID)
	if err != nil {
		c.finish(op, nil, nil, fmt.Errorf("the registry cannot read %s: %w", op.ResourceID, err))
		return
	}
	t := c.host.Type(r.Type)
	if t == nil {
		c.finish(op, r, nil, fmt.Errorf("no enabled plugin answers for type %s", r.Type))
		return
	}
	p := c.host.Plugin(t.Plugin)
	var result outcome
	for attempt := 0; ; attempt++ {
		_ = c.store.Attempt(ctx, op.ID)
		result, err = c.call(ctx, p, op, r)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return // shutting down: the operation stays running and resumes next start
		}
		st, _ := status.FromError(err)
		if st.Code() != codes.Unavailable || attempt >= len(c.Retry) {
			break
		}
		c.log.Warn("the engine did not answer; trying again", "operation", op.ID, "attempt", attempt+1, "err", st.Message())
		select {
		case <-time.After(c.Retry[attempt]):
		case <-ctx.Done():
			return
		}
	}
	if err != nil && op.Kind == registry.OpCreate {
		// a create that failed may have left something behind: the plugin's
		// delete is idempotent, so ask it to clear whatever there is
		if client, cerr := p.Client(ctx); cerr == nil {
			cctx, cancel := context.WithTimeout(ctx, c.CallTimeout)
			if _, derr := client.Delete(cctx, &pluginpb.DeleteRequest{Resource: toProto(r)}); derr != nil {
				c.log.Warn("clearing after a failed create failed too", "resource", r.ID, "err", derr)
			}
			cancel()
		}
	}
	c.finish(op, r, &result, err, prev)
}

type outcome struct {
	spec, observed, result json.RawMessage
	events                 []*pluginpb.Event
}

func (c *Core) call(ctx context.Context, p *plugins.Plugin, op *registry.Operation, r *registry.Resource) (outcome, error) {
	client, err := p.Client(ctx)
	if err != nil {
		return outcome{}, status.Error(codes.Unavailable, err.Error())
	}
	cctx, cancel := context.WithTimeout(ctx, c.CallTimeout)
	defer cancel()
	switch op.Kind {
	case registry.OpCreate:
		resp, err := client.Create(cctx, &pluginpb.CreateRequest{Resource: toProto(r)})
		return outcome{observed: resp.GetObserved(), events: resp.GetEvents()}, err
	case registry.OpDelete:
		resp, err := client.Delete(cctx, &pluginpb.DeleteRequest{Resource: toProto(r)})
		return outcome{events: resp.GetEvents()}, err
	default:
		resp, err := client.Act(cctx, &pluginpb.ActRequest{Resource: toProto(r), Action: op.Action, Params: op.Params})
		return outcome{spec: resp.GetSpec(), observed: resp.GetObserved(), result: resp.GetResult(), events: resp.GetEvents()}, err
	}
}

// finish writes an operation's end on it and on its resource.
func (c *Core) finish(op *registry.Operation, r *registry.Resource, out *outcome, err error, prev ...*registry.Resource) {
	ctx := context.Background() // the end is written even while shutting down
	defer c.wake(op.ID)
	state, msg := registry.OpSucceeded, ""
	if err != nil {
		state = registry.OpFailed
		msg = err.Error()
		if st, ok := status.FromError(err); ok {
			msg = st.Message()
		}
	}
	var change registry.Change
	if r != nil {
		switch {
		case op.Kind == registry.OpCreate && err == nil:
			change = registry.Change{State: registry.Ready, Observed: out.observed}
		case op.Kind == registry.OpCreate:
			change = registry.Change{State: registry.Failed}
		case op.Kind == registry.OpDelete && err == nil:
			change = registry.Change{State: registry.Deleted, Usage: map[string]int64{}}
		case op.Kind == registry.OpDelete:
			var from struct {
				FromState string `json:"from_state"`
			}
			_ = json.Unmarshal(op.Params, &from)
			if from.FromState == "" || from.FromState == registry.Deleting {
				from.FromState = registry.Ready
			}
			change = registry.Change{State: from.FromState}
		case err == nil:
			change = registry.Change{State: registry.Ready, Spec: out.spec, Observed: out.observed}
		default:
			change = registry.Change{State: registry.Ready}
			if len(prev) > 0 && prev[0] != nil {
				// give back what the admission reserved
				change.Spec, change.Usage, change.Choices = prev[0].Spec, prev[0].Usage, prev[0].Choices
				if change.Usage == nil {
					change.Usage = map[string]int64{}
				}
			}
		}
	}
	werr := c.store.Tx(ctx, func(tx *registry.Tx) error {
		if r != nil {
			if err := tx.Update(r.ID, change); err != nil {
				return err
			}
		}
		var result json.RawMessage
		if out != nil {
			result = out.result
		}
		return tx.FinishOperation(op.ID, state, msg, result)
	})
	if werr != nil {
		c.log.Error("the end of an operation could not be written", "operation", op.ID, "err", werr)
	}
	c.metrics.Inc("hangar_operations_total", op.Kind, state)
	e := audit.Event{Action: "operation", Actor: op.Owner, Resource: op.ResourceID, Operation: op.ID, Result: state, Detail: msg,
		Fields: map[string]string{"kind": op.Kind}}
	if op.Action != "" {
		e.Fields["action"] = op.Action
	}
	if r != nil {
		e.Type, e.Zone = r.Type, r.Zone
	}
	c.audit.Write(e)
	if out != nil && r != nil {
		c.events(r, out.events)
	}
}

// events writes a plugin's events to the audit.
func (c *Core) events(r *registry.Resource, evs []*pluginpb.Event) {
	for _, ev := range evs {
		c.audit.Write(audit.Event{Action: "event", Actor: "plugin:" + r.Plugin, Resource: r.ID, Type: r.Type, Zone: r.Zone,
			Result: ev.GetName(), Detail: ev.GetMessage(), Fields: ev.GetFields()})
	}
}

func toProto(r *registry.Resource) *pluginpb.Resource {
	return &pluginpb.Resource{Id: r.ID, Type: r.Type, Zone: r.Zone, Owner: r.Owner, Spec: r.Spec, Observed: r.Observed, Tags: r.Tags}
}

// ---- Reconcile ----------------------------------------------------------

// ReconcileOnce compares every settled resource with its engine: repaired or
// reported, and a resource its engine lost is marked lost (and found again
// when it comes back).
func (c *Core) ReconcileOnce(ctx context.Context) {
	var after int64
	for {
		rs, next, err := c.store.Resources(ctx, registry.Filter{States: []string{registry.Ready, registry.Lost}, After: after, Limit: 500})
		if err != nil {
			c.log.Error("reconcile: the registry cannot be listed", "err", err)
			return
		}
		for _, r := range rs {
			if ctx.Err() != nil {
				return
			}
			c.reconcile(ctx, r)
		}
		if next == 0 {
			return
		}
		after = next
	}
}

func (c *Core) reconcile(ctx context.Context, r *registry.Resource) {
	mu := c.lock(r.ID)
	if !mu.TryLock() {
		return // an operation holds it; the next round will look
	}
	defer mu.Unlock()
	t := c.host.Type(r.Type)
	if t == nil {
		return
	}
	client, err := c.host.Plugin(t.Plugin).Client(ctx)
	if err != nil {
		c.log.Warn("reconcile: plugin down", "plugin", t.Plugin, "err", err)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	resp, err := client.Reconcile(cctx, &pluginpb.ReconcileRequest{Resource: toProto(r)})
	if err != nil {
		c.log.Warn("reconcile: the plugin could not answer", "resource", r.ID, "err", err)
		return
	}
	verdict := strings.TrimPrefix(strings.ToLower(resp.GetDrift().String()), "drift_")
	c.metrics.Inc("hangar_reconcile_total", r.Type, verdict)
	change := registry.Change{IfState: r.State, Observed: resp.GetObserved()}
	e := audit.Event{Action: "reconcile", Actor: "hangar", Resource: r.ID, Type: r.Type, Zone: r.Zone, Result: verdict, Detail: resp.GetDetail()}
	clear := ""
	loud := true
	switch resp.GetDrift() {
	case pluginpb.Drift_DRIFT_IN_SYNC:
		if r.State == registry.Lost {
			change.State, e.Result = registry.Ready, "found"
		} else {
			loud = r.Drift != "" // back in sync after a drift is worth a line
		}
		change.Drift = &clear
	case pluginpb.Drift_DRIFT_REPAIRED:
		if r.State == registry.Lost {
			change.State = registry.Ready
		}
		change.Drift = &clear
	case pluginpb.Drift_DRIFT_DRIFTED:
		d := resp.GetDetail()
		change.Drift = &d
		loud = r.Drift != d
	case pluginpb.Drift_DRIFT_MISSING:
		if r.State == registry.Lost {
			return
		}
		d := resp.GetDetail()
		change.State, change.Drift = registry.Lost, &d
	default:
		return
	}
	err = c.store.Tx(ctx, func(tx *registry.Tx) error { return tx.Update(r.ID, change) })
	if errors.Is(err, registry.ErrMoved) {
		return
	}
	if err != nil {
		c.log.Error("reconcile: the verdict could not be written", "resource", r.ID, "err", err)
		return
	}
	if loud {
		c.audit.Write(e)
	}
	c.events(r, resp.GetEvents())
}
