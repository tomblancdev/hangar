package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/tomblancdev/hangar/internal/audit"
	"github.com/tomblancdev/hangar/internal/identity"
	"github.com/tomblancdev/hangar/internal/ids"
	"github.com/tomblancdev/hangar/internal/plugins"
	"github.com/tomblancdev/hangar/internal/registry"
)

// ---- Names: what a person calls a resource, and what the brain calls a person
//
// A resource has an id and, when its owner gives one, a name: the id is the
// identity — never changed, never used again, what the audit and the engine
// carry — and the name is what a person reads and types. A name is one thing
// among its owner's live resources of one type, so it goes wherever an id
// goes; it is the core's own, on every type, never a spec's.

// nameShape: a host's label — a name is also what a machine is born as.
var nameShape = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// maxDescription is a description's length, in characters: a line.
const maxDescription = 256

// about says which of a resource's own two words a problem is about: a
// violation whose field is "name" or "description" — no pointer into a spec,
// which begins with "/".
func about(p *Problem, field string) *Problem {
	p.Violations = []plugins.Violation{{Field: field, Reason: p.Detail}}
	return p
}

func checkName(name string) *Problem {
	switch {
	case name == "":
		return nil
	case !nameShape.MatchString(name):
		return about(problem(422, KindBadRequest, "a name is a-z, 0-9 and -, 1 to 63 characters, neither end a -: not %q", name), "name")
	case ids.Valid(name):
		return about(problem(422, KindBadRequest, "%s has the shape of an id: a name must not be mistaken for one", name), "name")
	}
	return nil
}

func checkDescription(d string) *Problem {
	if utf8.RuneCountInString(d) > maxDescription {
		return about(problem(422, KindBadRequest, "a description is one line of at most %d characters", maxDescription), "description")
	}
	for _, r := range d {
		if unicode.IsControl(r) {
			return about(problem(422, KindBadRequest, "a description is one line: no line break, no control character"), "description")
		}
	}
	return nil
}

// errNameTaken: the owner already calls a live resource of the type so.
var errNameTaken = errors.New("name taken")

func nameTaken(typ, name, by string) *Problem {
	return about(problem(409, KindConflict, "you already have a %s named %s (%s): a name is one thing — rename one, or pick another", typ, name, by), "name")
}

// RenameInput is what a resource is to be called; a field left out stays.
type RenameInput struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// Rename writes what a resource is called and its description. Nothing of
// the resource itself moves: its id, and what a machine was born as on its
// engine (its host name), stay. Its plugin writes the new words where the
// engine shows them at the next look.
func (c *Core) Rename(ctx context.Context, who *Caller, id string, in RenameInput) (*registry.Resource, error) {
	if p := c.needWrite(ctx, who); p != nil {
		return nil, p
	}
	r, _, p := c.resourceFor(ctx, who, id, true)
	if p != nil {
		return nil, p
	}
	if r.State == registry.Deleted {
		return nil, c.refused(ctx, problem(404, KindNotFound, "%s was deleted at %s", r.ID, r.DeletedAt.Format("2006-01-02T15:04:05Z07:00")))
	}
	if in.Name == nil && in.Description == nil {
		return nil, c.refused(ctx, problem(422, KindBadRequest, "say its name, its description, or both"))
	}
	if in.Name != nil {
		if p := checkName(*in.Name); p != nil {
			return nil, c.refused(ctx, p)
		}
	}
	if in.Description != nil {
		if p := checkDescription(*in.Description); p != nil {
			return nil, c.refused(ctx, p)
		}
	}
	taken := ""
	err := c.store.Tx(ctx, func(tx *registry.Tx) error {
		if in.Name != nil && *in.Name != "" {
			by, err := tx.Named(r.Owner, r.Type, *in.Name)
			if err != nil {
				return err
			}
			if by != "" && by != r.ID {
				taken = by
				return errNameTaken
			}
		}
		return tx.Update(r.ID, registry.Change{Name: in.Name, Description: in.Description})
	})
	if errors.Is(err, errNameTaken) {
		return nil, c.refused(ctx, nameTaken(r.Type, *in.Name, taken))
	}
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}
	if in.Name != nil {
		fields["name"], fields["was"] = *in.Name, r.Name
	}
	if in.Description != nil {
		fields["description"] = *in.Description
	}
	audit.From(ctx).Set(func(e *audit.Event) { e.Fields = fields })
	now, err := c.store.Resource(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	// the engine's own screen follows: its plugin is asked to look now
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.reconcile(c.life, now)
	}()
	return now, nil
}

// ---- People --------------------------------------------------------------

// people is the name each subject last signed in under.
type people struct {
	mu     sync.RWMutex
	names  map[string]string
	loaded bool
}

func (c *Core) loadPeople(ctx context.Context) {
	c.people.mu.Lock()
	defer c.people.mu.Unlock()
	if c.people.loaded {
		return
	}
	names, err := c.store.Subjects(ctx)
	if err != nil {
		c.log.Warn("the subjects' names cannot be read", "err", err)
		return
	}
	c.people.names, c.people.loaded = names, true
}

// Seen remembers the name a person signed in under at the identity provider,
// so what they own is shown by it. A token says its own name, not its
// owner's: it teaches nothing.
func (c *Core) Seen(ctx context.Context, id *identity.Identity) {
	if id == nil || id.Via != "oidc" || id.Name == "" || id.Subject == "" {
		return
	}
	c.loadPeople(ctx)
	c.people.mu.RLock()
	known := c.people.names[id.Subject] == id.Name
	c.people.mu.RUnlock()
	if known {
		return
	}
	if err := c.store.SeeSubject(ctx, id.Subject, id.Name); err != nil {
		c.log.Warn("a subject's name cannot be written", "subject", id.Subject, "err", err)
		return
	}
	c.people.mu.Lock()
	if c.people.names == nil {
		c.people.names = map[string]string{}
	}
	c.people.names[id.Subject] = id.Name
	c.people.mu.Unlock()
}

// nameOf is the name a subject last signed in under; "" when never seen.
func (c *Core) nameOf(ctx context.Context, subject string) string {
	c.loadPeople(ctx)
	c.people.mu.RLock()
	defer c.people.mu.RUnlock()
	return c.people.names[subject]
}

// subjectOf reads an owner as an operator may write it: a subject, or the
// name of the one person who signed in under it.
func (c *Core) subjectOf(ctx context.Context, owner string) string {
	c.loadPeople(ctx)
	c.people.mu.RLock()
	defer c.people.mu.RUnlock()
	if _, known := c.people.names[owner]; known {
		return owner
	}
	var subs []string
	for sub, name := range c.people.names {
		if name == owner {
			subs = append(subs, sub)
		}
	}
	if len(subs) == 1 {
		return subs[0]
	}
	return owner
}

// ---- A resource as it is served -------------------------------------------

var busyStates = []string{registry.Creating, registry.Updating, registry.Deleting}

// Dress writes on resources what the registry does not store, for whoever
// reads them: the owner's name, the one word each wears and how it is lit,
// its type's sentence, and the names of the resources it names. who is the
// reader: what a resource names is called by name only for its owner and
// for an operator — someone it is shared with reads ids there.
func (c *Core) Dress(ctx context.Context, who *Caller, rs ...*registry.Resource) {
	var named []string
	refs := make([][]string, len(rs))
	for i, r := range rs {
		t := c.host.Type(r.Type)
		if t == nil || who != nil && !visible(who, r.Owner) {
			continue
		}
		for _, ref := range t.Refs {
			refs[i] = append(refs[i], refIDs(ref, r.Spec)...)
		}
		named = append(named, refs[i]...)
	}
	slices.Sort(named)
	names, err := c.store.NamesOf(ctx, slices.Compact(named))
	if err != nil {
		c.log.Warn("the names cannot be read", "err", err)
		names = map[string]string{}
	}
	for i, r := range rs {
		r.OwnerName = c.nameOf(ctx, r.Owner)
		r.Names = nil
		for _, id := range refs[i] {
			if n := names[id]; n != "" {
				if r.Names == nil {
					r.Names = map[string]string{}
				}
				r.Names[id] = n
			}
		}
		t := c.host.Type(r.Type)
		r.Summary = t.Summarize(r.Spec, r.Observed, r.Names)
		r.Status, r.Light = statusOf(t, r)
	}
}

// statusOf is the one word a resource wears: the core's state while
// something moves or went wrong, then its plugin's say on whether it may be
// named, then its type's own word; "ready" for a type that says none.
func statusOf(t *plugins.Type, r *registry.Resource) (string, string) {
	switch {
	case slices.Contains(busyStates, r.State):
		return r.State, "busy"
	case r.State == registry.Failed || r.State == registry.Lost:
		return r.State, "bad"
	case r.State == registry.Deleted:
		return r.State, "off"
	case r.Pending:
		if r.Unusable != "" {
			return r.Unusable, "busy"
		}
		return "pending", "busy"
	case r.Unusable == "failed":
		return r.Unusable, "bad"
	case r.Unusable != "":
		return r.Unusable, "off"
	}
	if word, on, ok := t.StatusOf(r.Spec, r.Observed); ok {
		if on {
			return word, "on"
		}
		return word, "off"
	}
	return "ready", "on"
}

// DressOps writes on operations the name of what each is about and of who
// asked.
func (c *Core) DressOps(ctx context.Context, ops ...*registry.Operation) {
	var about []string
	for _, op := range ops {
		about = append(about, op.ResourceID)
	}
	slices.Sort(about)
	names, err := c.store.NamesOf(ctx, slices.Compact(about))
	if err != nil {
		names = map[string]string{}
	}
	for _, op := range ops {
		op.ResourceName, op.OwnerName = names[op.ResourceID], c.nameOf(ctx, op.Owner)
	}
}

// ---- A name where an id goes ---------------------------------------------

// named finds what an owner calls by a name where a reference is asked: one
// of their OWN, never one shared with them. A name is its owner's word for
// their own thing; what someone else shares is named by its id (or by the
// schedule that made it) — anyone may call theirs anything, and a search by
// name would hand a request a look-alike (the « whoAMI » confusion, §4 "The
// latest"). It returns the id, or why there is none.
func (c *Core) named(ctx context.Context, owner string, ref plugins.Ref, name string) (string, string) {
	if !nameShape.MatchString(name) {
		return "", fmt.Sprintf("neither an id (%s-…) nor a name", prefixOf(c.host, ref.Type))
	}
	id, err := c.store.Named(ctx, owner, ref.Type, name)
	if err != nil {
		return "", "the registry cannot be read"
	}
	if id == "" {
		return "", fmt.Sprintf("you have no %s named %s (one shared with you is named by its id)", ref.Type, name)
	}
	return id, ""
}

func prefixOf(h *plugins.Host, typ string) string {
	if t := h.Type(typ); t != nil {
		return t.Prefix
	}
	return typ
}
