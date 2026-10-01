// Package limits is the arithmetic of the ask: which tier a person is in,
// what that tier allows per dimension, and whether a request fits — answered
// with the numbers, never a bare no.
//
// Everything here is a pure function of the tier, the dimensions the plugins
// declared and the usage the registry counted; the core calls it inside the
// transaction that writes the resource, so what it decides stays true.
package limits

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tomblancdev/hangar/internal/config"
)

// Dimension kinds, as the plugin protocol names them.
const (
	Quantity = "quantity"
	Choice   = "choice"
	// Meter: an amount consumed as time passes, summed per owner over the
	// calendar month; a tier's limit is the month's.
	Meter = "meter"
)

// Dimension is one thing a plugin counts or lets a tier choose.
type Dimension struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Unit        string `json:"unit,omitempty"`
	Description string `json:"description,omitempty"`
	Plugin      string `json:"plugin"`
}

// For returns the first tier, in the file's order, that one of groups
// reaches.
func For(tiers []config.Tier, groups []string) (*config.Tier, bool) {
	for i := range tiers {
		for _, g := range tiers[i].Groups {
			if slices.Contains(groups, g) {
				return &tiers[i], true
			}
		}
	}
	return nil, false
}

// Of returns the tier's limit on a dimension: its own name first, then
// "<plugin>.*", then "*". ok=false: the tier does not name it, and allows
// nothing.
func Of(t *config.Tier, dim string) (config.Limit, bool) {
	if l, ok := t.Limits[dim]; ok {
		return l, true
	}
	if plugin, _, found := strings.Cut(dim, "."); found {
		if l, ok := t.Limits[plugin+".*"]; ok {
			return l, true
		}
	}
	l, ok := t.Limits["*"]
	return l, ok
}

// ZoneOpen reports whether the tier may ask for things in a zone.
func ZoneOpen(t *config.Tier, zone string) bool {
	return slices.Contains(t.Zones, "*") || slices.Contains(t.Zones, zone)
}

// Refusal says why a request does not fit, with the numbers.
type Refusal struct {
	Reason    string   `json:"reason"` // "limit", "choice" or "meter"
	Dimension string   `json:"dimension"`
	Limit     *int64   `json:"limit,omitempty"`
	Used      int64    `json:"used,omitempty"`
	Asked     int64    `json:"asked,omitempty"`
	Value     string   `json:"value,omitempty"`
	Allowed   []string `json:"allowed,omitempty"`
	// A meter's: what the month has consumed, which month, and when the
	// next one begins.
	Consumed float64    `json:"consumed,omitempty"`
	Period   string     `json:"period,omitempty"`
	Resets   *time.Time `json:"resets,omitempty"`
	Message  string     `json:"message"`
}

// Month is the calendar month the meters count in.
type Month struct {
	Key  string    // "2026-10": what the registry sums under
	Name string    // "October 2026"
	Next time.Time // when the next one begins
}

// MonthOf is the month now falls in, in the operator's time zone.
func MonthOf(now time.Time, loc *time.Location) Month {
	now = now.In(loc)
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	return Month{Key: now.Format("2006-01"), Name: now.Format("January 2006"), Next: first.AddDate(0, 1, 0)}
}

// Amount writes a consumed amount as people read one: 12, 12.5.
func Amount(v float64) string { return strconv.FormatFloat(float64(int64(v*10))/10, 'f', -1, 64) }

// Spent lists which of the meters named a tier's month has used up: what
// was consumed has reached the limit. A meter the tier does not name allows
// nothing, as any dimension.
func Spent(t *config.Tier, consumed map[string]float64, meters []string) []string {
	var out []string
	for _, name := range meters {
		if l, named := Of(t, name); named && l.Unlimited {
			continue
		} else if consumed[name] >= float64(l.Max) {
			out = append(out, name)
		}
	}
	return out
}

// AdmitMeters refuses a request that would leave a resource drawing on a
// meter whose month is spent — with the numbers, and when the next begins.
func AdmitMeters(t *config.Tier, dims map[string]Dimension, consumed map[string]float64, meters []string, m Month) []Refusal {
	var out []Refusal
	for _, name := range Spent(t, consumed, slices.Sorted(slices.Values(meters))) {
		l, _ := Of(t, name)
		limit, next := l.Max, m.Next
		unit := name
		if u := dims[name].Unit; u != "" {
			unit = u + " (" + name + ")"
		}
		msg := fmt.Sprintf("%s of %d %s used in %s: it is back on %s", Amount(consumed[name]), limit, unit, m.Name, next.Format("2 January"))
		if limit == 0 {
			msg = fmt.Sprintf("tier %s allows no %s", t.Name, name)
		}
		out = append(out, Refusal{Reason: "meter", Dimension: name, Limit: &limit, Consumed: consumed[name], Period: m.Key, Resets: &next, Message: msg})
	}
	return out
}

// Admit checks one request against a tier. used is what the owner holds now
// (the resource's own share included); before and after are what the
// resource holds before and after the request (before is empty for a
// create). Only growth is checked — shrinking always fits, even over a limit
// lowered since — and only a choice that changes is checked, so a start is
// never refused because the tier's list moved.
func Admit(t *config.Tier, dims map[string]Dimension, used, before, after map[string]int64,
	choicesBefore, choicesAfter map[string]string) []Refusal {
	var out []Refusal
	for _, name := range slices.Sorted(maps.Keys(after)) {
		d := dims[name]
		delta := after[name] - before[name]
		if delta <= 0 {
			continue
		}
		l, named := Of(t, name)
		if named && l.Unlimited {
			continue
		}
		limit := l.Max
		if used[name]+delta <= limit {
			continue
		}
		unit := name
		if d.Unit != "" {
			unit = d.Unit + " (" + name + ")"
		}
		msg := fmt.Sprintf("%d of %d %s used; this asks for %d more", used[name], limit, unit, delta)
		if limit == 0 {
			msg = fmt.Sprintf("tier %s allows no %s", t.Name, name)
		}
		out = append(out, Refusal{Reason: "limit", Dimension: name, Limit: &limit, Used: used[name], Asked: delta, Message: msg})
	}
	for _, name := range slices.Sorted(maps.Keys(choicesAfter)) {
		v := choicesAfter[name]
		if prev, had := choicesBefore[name]; had && prev == v {
			continue
		}
		l, named := Of(t, name)
		if named && l.Unlimited {
			continue
		}
		if slices.Contains(l.Allowed, v) {
			continue
		}
		msg := fmt.Sprintf("%s %q is not open to tier %s (open: %s)", name, v, t.Name, strings.Join(l.Allowed, ", "))
		if len(l.Allowed) == 0 {
			msg = fmt.Sprintf("tier %s allows no %s", t.Name, name)
		}
		out = append(out, Refusal{Reason: "choice", Dimension: name, Value: v, Allowed: l.Allowed, Message: msg})
	}
	return out
}

// Check refuses a tier that names a dimension no plugin declared, or spells a
// limit the wrong way for its kind: a typo there would silently allow
// nothing, and the core would rather not start.
func Check(tiers []config.Tier, dims map[string]Dimension, plugins []string) error {
	var errs []error
	for _, t := range tiers {
		for _, key := range slices.Sorted(maps.Keys(t.Limits)) {
			l := t.Limits[key]
			if key == "*" {
				if l.IsChoice {
					errs = append(errs, fmt.Errorf("tier %s: \"*\" must be a number or unlimited", t.Name))
				}
				continue
			}
			if p, ok := strings.CutSuffix(key, ".*"); ok {
				if !slices.Contains(plugins, p) {
					errs = append(errs, fmt.Errorf("tier %s: %q names no enabled plugin", t.Name, key))
				}
				if l.IsChoice {
					errs = append(errs, fmt.Errorf("tier %s: %q must be a number or unlimited", t.Name, key))
				}
				continue
			}
			d, ok := dims[key]
			switch {
			case !ok:
				errs = append(errs, fmt.Errorf("tier %s: %q is no dimension an enabled plugin declared", t.Name, key))
			case d.Kind == Quantity && l.IsChoice:
				errs = append(errs, fmt.Errorf("tier %s: %s is a quantity: a number or unlimited, not a list", t.Name, key))
			case d.Kind == Meter && l.IsChoice:
				errs = append(errs, fmt.Errorf("tier %s: %s is a meter: how much a month, or unlimited, not a list", t.Name, key))
			case d.Kind == Choice && !l.IsChoice && !l.Unlimited:
				errs = append(errs, fmt.Errorf("tier %s: %s is a choice: the list of values allowed, or unlimited", t.Name, key))
			}
		}
	}
	return errors.Join(errs...)
}
