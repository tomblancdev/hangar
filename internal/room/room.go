// Package room is the arithmetic of a zone's capacity (ARCHITECTURE.md §6):
// two pools out of the zone's memory and its reservations, which reservation
// holds the borrowed room now, and whether a request fits — answered with the
// numbers, never a bare no.
//
// Like package limits, everything here is a pure function of the zone's
// config, what is in force, and what the registry counted; the core calls it
// inside the transaction that writes the resource.
package room

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tomblancdev/hangar/internal/config"
)

// Pools are a zone's two pools, in MiB.
type Pools struct {
	MemoryMB int64 `json:"memory_mb"`
	// GuaranteedMB: the memory less every reservation, as if all were in
	// force — what can be promised.
	GuaranteedMB int64 `json:"guaranteed_mb"`
	// SpotMB: the room the conditional reservations keep while none is in
	// force — what can be lent.
	SpotMB int64 `json:"spot_mb"`
}

// Of returns a zone's pools.
func Of(r *config.Room) Pools {
	p := Pools{MemoryMB: int64(r.MemoryGB) * 1024, GuaranteedMB: int64(r.MemoryGB) * 1024}
	for _, rv := range r.Reservations {
		mb := int64(rv.MemoryGB) * 1024
		p.GuaranteedMB -= mb
		if rv.Key() != "" {
			p.SpotMB += mb
		}
	}
	return p
}

// Conditional reports whether a reservation waits on a condition (the others
// are always in force, and hold nothing back from anyone).
func Conditional(rv config.Reservation) bool { return rv.Key() != "" }

// HeldBy is the conditional reservation holding the zone's borrowed room now:
// the first in the file's order that is in force, or nil. One in force takes
// the whole spot pool back — every spot resource of the zone stops, every
// floor shrinks — which is exact for a zone with one conditional reservation
// and errs on the side of the one that needs the room otherwise.
func HeldBy(r *config.Room, inForce map[string]bool) *config.Reservation {
	for i, rv := range r.Reservations {
		if Conditional(rv) && inForce[rv.Name] {
			return &r.Reservations[i]
		}
	}
	return nil
}

// ByKey finds the reservation a hold's key names.
func ByKey(r *config.Room, key string) *config.Reservation {
	for i, rv := range r.Reservations {
		if rv.Key() != "" && rv.Key() == key {
			return &r.Reservations[i]
		}
	}
	return nil
}

// ForGuest finds the reservation that waits on a guest running.
func ForGuest(r *config.Room, guest string) *config.Reservation {
	for i, rv := range r.Reservations {
		if rv.WhileRunning == guest {
			return &r.Reservations[i]
		}
	}
	return nil
}

// Take is what one resource takes from its zone. Running: it uses its spot
// room — false for one meant to run whose borrowed room is held back.
type Take struct {
	GuaranteedMB int64
	SpotMB       int64
	Running      bool
}

func (t Take) spot() int64 {
	if t.Running {
		return t.SpotMB
	}
	return 0
}

// Use is what the zone's other resources take: MiB booked in the guaranteed
// pool, and MiB of the spot pool in use.
type Use struct {
	BookedMB   int64
	SpotUsedMB int64
}

// Refusal says why a request does not fit the zone, with the numbers.
type Refusal struct {
	// Pool: "guaranteed" or "spot".
	Pool string `json:"pool"`
	// HeldFor: the reservation holding the spot pool, when that is why.
	HeldFor  string `json:"held_for,omitempty"`
	PoolMB   int64  `json:"pool_mb"`
	UsedMB   int64  `json:"used_mb"`
	AskedMB  int64  `json:"asked_mb"`
	Message  string `json:"message"`
	ZoneName string `json:"zone"`
}

// Admit checks one request against a zone: only growth is checked — a
// shrink, a stop, always fits — and only in the pool that grows. use is what
// the zone's OTHER resources take; before is empty for a create. While a
// reservation holds the borrowed room, a resource runs on what it booked
// only: one with a floor starts on it (its top-up waits for the room), one
// with nothing booked does not start.
func Admit(zone string, r *config.Room, inForce map[string]bool, use Use, before, after Take) *Refusal {
	p := Of(r)
	if d := after.GuaranteedMB - before.GuaranteedMB; d > 0 && use.BookedMB+after.GuaranteedMB > p.GuaranteedMB {
		return &Refusal{Pool: "guaranteed", ZoneName: zone, PoolMB: p.GuaranteedMB, UsedMB: use.BookedMB, AskedMB: d,
			Message: fmt.Sprintf("zone %s's guaranteed pool holds %s; %s booked; this asks for %s more — ask for spot, or less",
				zone, GB(p.GuaranteedMB), GB(use.BookedMB), GB(d))}
	}
	d := after.spot() - before.spot()
	if d <= 0 {
		return nil
	}
	if p.SpotMB == 0 {
		return &Refusal{Pool: "spot", ZoneName: zone, AskedMB: d,
			Message: fmt.Sprintf("zone %s lends no room (none of its reservations waits on a condition): ask for guaranteed", zone)}
	}
	if h := HeldBy(r, inForce); h != nil {
		if after.GuaranteedMB > 0 {
			return nil
		}
		return &Refusal{Pool: "spot", ZoneName: zone, HeldFor: h.Name, PoolMB: p.SpotMB, UsedMB: use.SpotUsedMB, AskedMB: d,
			Message: fmt.Sprintf("zone %s's borrowed room is held for %s (%s): ask again when it ends, or for guaranteed room",
				zone, h.Name, Condition(*h))}
	}
	if use.SpotUsedMB+after.spot() > p.SpotMB {
		return &Refusal{Pool: "spot", ZoneName: zone, PoolMB: p.SpotMB, UsedMB: use.SpotUsedMB, AskedMB: d,
			Message: fmt.Sprintf("zone %s's spot pool holds %s; %s in use; this asks for %s more — ask for less, or when one stops",
				zone, GB(p.SpotMB), GB(use.SpotUsedMB), GB(d))}
	}
	return nil
}

// Condition says what a reservation waits on, in words.
func Condition(rv config.Reservation) string {
	switch {
	case rv.WhileRunning != "":
		return "while guest " + rv.WhileRunning + " runs"
	case rv.WhileDown != "":
		return "while node " + rv.WhileDown + " is down"
	}
	return "always"
}

// GB writes MiB as gigabytes, with a decimal only when it needs one.
func GB(mb int64) string {
	if mb%1024 == 0 {
		return strconv.FormatInt(mb/1024, 10) + " GB"
	}
	return strings.TrimSuffix(strconv.FormatFloat(float64(mb)/1024, 'f', 1, 64), ".0") + " GB"
}
