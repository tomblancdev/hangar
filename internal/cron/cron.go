// Package cron reads the five fields of a cron line — minute, hour, day of
// the month, month, day of the week — and says when it next comes, in a
// time zone. What it reads is what Kubernetes' CronJob and most crons read:
// numbers, "*", lists (1,15), ranges (1-5), steps (*/15, 0-30/10), and the
// three-letter names of months and days (jan, sun). As in Vixie cron, when
// both days are restricted a time matches either of them.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Line is a parsed cron line.
type Line struct {
	minute, hour, dom, month, dow uint64 // bit n set = n allowed
	domAny, dowAny                bool
	text                          string
}

func (l Line) String() string { return l.text }

type field struct {
	name     string
	min, max int
	names    []string // names[i] spells min+i
}

var fields = [5]field{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of the month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}},
	// 0 and 7 are both Sunday
	{name: "day of the week", min: 0, max: 7, names: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}},
}

// Parse reads a line of five fields.
func Parse(text string) (Line, error) {
	parts := strings.Fields(text)
	if len(parts) != 5 {
		return Line{}, fmt.Errorf("cron %q: five fields — minute hour day-of-month month day-of-week", text)
	}
	var sets [5]uint64
	for i, p := range parts {
		set, err := fields[i].parse(p)
		if err != nil {
			return Line{}, fmt.Errorf("cron %q: %s: %w", text, fields[i].name, err)
		}
		sets[i] = set
	}
	if sets[4]&(1<<7) != 0 {
		sets[4] = sets[4]&^(1<<7) | 1 // 7 is Sunday too
	}
	l := Line{minute: sets[0], hour: sets[1], dom: sets[2], month: sets[3], dow: sets[4],
		// a field starting with * counts as unrestricted there (Vixie's rule)
		domAny: strings.HasPrefix(parts[2], "*"), dowAny: strings.HasPrefix(parts[4], "*"), text: strings.Join(parts, " ")}
	// a line that can never come (the 30th of February) is refused now,
	// not found out a year later
	if _, ok := l.next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), 8*366*24*time.Hour); !ok {
		return Line{}, fmt.Errorf("cron %q: never comes", text)
	}
	return l, nil
}

func (f field) parse(p string) (uint64, error) {
	var set uint64
	for _, item := range strings.Split(p, ",") {
		rng, stepText, stepped := strings.Cut(item, "/")
		step := 1
		if stepped {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("%q: a step is a number above 0", item)
			}
			step = n
		}
		lo, hi := f.min, f.max
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = f.value(a); err != nil {
				return 0, err
			}
			if hi, err = f.value(b); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("%q: a range goes up", item)
			}
		default:
			v, err := f.value(rng)
			if err != nil {
				return 0, err
			}
			lo, hi = v, v
			if stepped {
				hi = f.max // 5/15 = from 5, every 15
			}
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << v
		}
	}
	return set, nil
}

func (f field) value(s string) (int, error) {
	for i, n := range f.names {
		if strings.EqualFold(s, n) {
			return f.min + i, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < f.min || v > f.max {
		return 0, fmt.Errorf("%q: %d to %d", s, f.min, f.max)
	}
	return v, nil
}

func has(set uint64, v int) bool { return set&(1<<v) != 0 }

func (l Line) day(t time.Time) bool {
	dom, dow := has(l.dom, t.Day()), has(l.dow, int(t.Weekday()))
	switch {
	case l.domAny && l.dowAny:
		return true
	case l.domAny:
		return dow
	case l.dowAny:
		return dom
	}
	return dom || dow
}

// Next is the first time the line comes strictly after t, read in loc. A
// time in the hour a change of clocks skips comes an hour later (02:30 on
// the spring day comes at 03:30); a time in the hour that comes twice comes
// once.
func (l Line) Next(t time.Time, loc *time.Location) time.Time {
	n, _ := l.next(t.In(loc), 8*366*24*time.Hour)
	return n
}

// Last is the latest time the line came at or before t, after since — and
// false when it did not come in between.
func (l Line) Last(since, t time.Time, loc *time.Location) (time.Time, bool) {
	var last time.Time
	for n := l.Next(since, loc); !n.IsZero() && !n.After(t); n = l.Next(n, loc) {
		last = n
	}
	return last, !last.IsZero()
}

func (l Line) next(t time.Time, within time.Duration) (time.Time, bool) {
	loc := t.Location()
	// the search runs on the wall clock — a calendar with no changes of
	// clocks — and each match is read back in loc: an hour that comes twice
	// is searched once, an hour that is skipped is still searched
	y, mo, d := t.Date()
	w := time.Date(y, mo, d, t.Hour(), t.Minute(), 0, 0, time.UTC).Add(time.Minute)
	end := w.Add(within)
	for w.Before(end) {
		y, mo, d := w.Date()
		switch {
		case !has(l.month, int(mo)):
			w = time.Date(y, mo+1, 1, 0, 0, 0, 0, time.UTC)
		case !l.day(w):
			w = time.Date(y, mo, d+1, 0, 0, 0, 0, time.UTC)
		case !has(l.hour, w.Hour()):
			w = w.Truncate(time.Hour).Add(time.Hour)
		case !has(l.minute, w.Minute()):
			w = w.Add(time.Minute)
		default:
			if n := time.Date(y, mo, d, w.Hour(), w.Minute(), 0, 0, loc); n.After(t) {
				return n, true
			}
			w = w.Add(time.Minute)
		}
	}
	return time.Time{}, false
}
