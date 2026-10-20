package core

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/tomblancdev/hangar/internal/audit"
	"github.com/tomblancdev/hangar/internal/ids"
	"github.com/tomblancdev/hangar/internal/plugins"
	"github.com/tomblancdev/hangar/internal/registry"
)

// endParams mark a delete the brain made of a resource that came to its end
// by a rule of its own: its operation is its owner's, its actor the brain.
const endParams = `{"from_state":"` + registry.Ready + `","ended":true}`

// ended says whether an operation is such a delete.
func ended(op *registry.Operation) bool {
	var p struct {
		Ended bool `json:"ended"`
	}
	return op.Kind == registry.OpDelete && json.Unmarshal(op.Params, &p) == nil && p.Ended
}

// end deletes a resource its plugin said came to its end — a machine whose
// own power-off means its delete, or whose until came — in the brain's name,
// as its owner's delete would: an operation of its owner's, refused while
// something is attached to it or stands on it, and then the resource stays as
// its plugin left it (stopped), the audit saying why — once: its plugin
// hands its end over again at every look, until the delete can be done. Its
// lock is held.
func (c *Core) end(ctx context.Context, id string, t *plugins.Type, why string) {
	r, err := c.store.Resource(ctx, id) // as the verdict just left it
	if err != nil || r.State != registry.Ready {
		return
	}
	op := &registry.Operation{
		ID: ids.New(ids.Operation), Owner: r.Owner, ResourceID: r.ID, Kind: registry.OpDelete, Params: json.RawMessage(endParams),
	}
	var refused *Problem
	err = c.store.Tx(ctx, func(tx *registry.Tx) error {
		var err error
		if refused, err = c.attachments(tx, r, t); err != nil || refused != nil {
			if err == nil {
				err = errAttached
			}
			return err
		}
		if refused, err = c.members(tx, r); err != nil || refused != nil {
			if err == nil {
				err = errAttached
			}
			return err
		}
		if err := tx.Update(r.ID, registry.Change{State: registry.Deleting, IfState: registry.Ready}); err != nil {
			return err
		}
		return tx.InsertOperation(op)
	})
	e := audit.Event{Action: "end", Actor: "hangar", Resource: r.ID, Type: r.Type, Zone: r.Zone, Detail: why}
	switch {
	case errors.Is(err, errAttached):
		e.Result, e.Detail = "refused", why+" — not deleted, it stays so: "+refused.Detail
		if said, _ := c.endSaid.Swap(r.ID, e.Detail); said == e.Detail {
			return
		}
	case errors.Is(err, registry.ErrMoved):
		return // an operation came first; the next look sees what it left
	case err != nil:
		c.log.Error("an end's delete could not be written", "resource", r.ID, "err", err)
		return
	default:
		e.Operation, e.Result = op.ID, "accepted"
		c.endSaid.Delete(r.ID)
	}
	c.audit.Write(e)
	if e.Result == "accepted" {
		c.start(op)
	}
}
