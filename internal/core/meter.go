package core

import (
	"context"
	"maps"
	"math"
	"slices"

	"github.com/tomblancdev/hangar/internal/config"
	"github.com/tomblancdev/hangar/internal/limits"
	"github.com/tomblancdev/hangar/internal/registry"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// ---- Meters: what is consumed as time passes (§3, §6 power) -----------------
//
// A meter is a dimension a resource uses up while it does something — the
// hours a machine runs — summed per owner over the calendar month, in the
// operator's time zone. Its plugin says how much each resource consumed
// since it last said (with every action, delete and reconcile), and which
// meters a request would leave the resource drawing on; the core adds, and
// refuses a request that would draw on a month already spent. Nobody asks at
// the moment a month runs out: the core tells the plugin at the next
// reconcile (Resource.spent), under the tier the resource was last admitted
// in, and the plugin brings it to what that means.

// month is the calendar month the meters count in now.
func (c *Core) month() limits.Month { return limits.MonthOf(c.Now(), c.cfg.Location) }

// metersOf lists a plugin's meter dimensions, sorted.
func (c *Core) metersOf(plugin string) []string {
	var out []string
	for name, d := range c.host.Dimensions() {
		if d.Plugin == plugin && d.Kind == limits.Meter {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// admitMeters refuses, inside the admission's transaction, a request that
// would leave a resource of owner's drawing on a meter whose month is spent.
func (c *Core) admitMeters(tx *registry.Tx, t *config.Tier, owner string, meters []string) ([]limits.Refusal, error) {
	if len(meters) == 0 {
		return nil, nil
	}
	m := c.month()
	consumed, err := tx.Metered(owner, m.Key)
	if err != nil {
		return nil, err
	}
	return limits.AdmitMeters(t, c.host.Dimensions(), consumed, meters, m), nil
}

// meter adds what a plugin says a resource consumed to its owner's month, in
// the transaction that writes the observed state it came with. An amount the
// plugin never declared a meter for, or that is no amount, is left out and
// said.
func (c *Core) meter(tx *registry.Tx, r *registry.Resource, consumed *pluginpb.Consumed) error {
	amounts := consumed.GetAmounts()
	if len(amounts) == 0 {
		return nil
	}
	dims := c.host.Dimensions()
	key := c.month().Key
	for _, name := range slices.Sorted(maps.Keys(amounts)) {
		v := amounts[name]
		if d, ok := dims[name]; !ok || d.Plugin != r.Plugin || d.Kind != limits.Meter || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			c.log.Warn("a plugin reported a consumption that is not one", "plugin", r.Plugin, "resource", r.ID, "dimension", name, "amount", v)
			continue
		}
		if v == 0 {
			continue
		}
		if err := tx.AddMeter(r.Owner, name, key, v); err != nil {
			return err
		}
		c.metrics.Add("hangar_metered_total", v, name)
	}
	return nil
}

// spent lists the meters of a resource's plugin whose month its owner has
// used up, under the tier its last request was admitted in. None when that
// tier is not known (admitted before tiers were recorded, or a tier that left
// the file): nothing is taken away on a guess.
func (c *Core) spent(ctx context.Context, r *registry.Resource) []string {
	if r.Tier == "" {
		return nil
	}
	meters := c.metersOf(r.Plugin)
	if len(meters) == 0 {
		return nil
	}
	t, ok := c.cfg.Tier(r.Tier)
	if !ok {
		return nil
	}
	consumed, err := c.store.Metered(ctx, r.Owner, c.month().Key)
	if err != nil {
		c.log.Error("the meters cannot be read", "resource", r.ID, "err", err)
		return nil
	}
	return limits.Spent(t, consumed, meters)
}

// proto is a resource as its plugin is handed it when it is to act: with the
// meters its owner has spent.
func (c *Core) proto(ctx context.Context, r *registry.Resource) *pluginpb.Resource {
	p := toProto(r)
	p.Spent = c.spent(ctx, r)
	return p
}
