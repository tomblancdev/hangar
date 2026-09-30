package cron

import (
	"strings"
	"testing"
	"time"
)

func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNext(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		line, from, want string
		loc              *time.Location
	}{
		{"* * * * *", "2026-09-30 10:00", "2026-09-30 10:01", time.UTC},
		{"*/15 * * * *", "2026-09-30 10:07", "2026-09-30 10:15", time.UTC},
		{"5/15 * * * *", "2026-09-30 10:21", "2026-09-30 10:35", time.UTC},
		{"0 3 * * 0", "2026-09-30 10:00", "2026-10-04 03:00", paris},   // a Wednesday: the Sunday after
		{"0 3 * * sun", "2026-10-04 03:00", "2026-10-11 03:00", paris}, // strictly after
		{"0 3 * * 7", "2026-10-04 02:59", "2026-10-04 03:00", paris},
		{"30 1 1,15 * *", "2026-09-30 10:00", "2026-10-01 01:30", time.UTC},
		{"0 0 * feb-mar mon", "2026-09-30 10:00", "2027-02-01 00:00", time.UTC},
		{"0 0 29 2 *", "2026-01-01 00:00", "2028-02-29 00:00", time.UTC},
		// both days restricted: either one (the 13th, or any Friday)
		{"0 12 13 * fri", "2026-10-01 00:00", "2026-10-02 12:00", time.UTC},
		{"0 12 13 * fri", "2026-10-10 00:00", "2026-10-13 12:00", time.UTC},
		// */2 in the days counts as unrestricted: both must match
		{"0 0 */2 * mon", "2026-10-01 00:00", "2026-10-05 00:00", time.UTC},
		// the spring day in Paris: 02:30 does not exist, it comes at 03:30
		{"30 2 * * *", "2026-03-29 01:00", "2026-03-29 03:30", paris},
	} {
		l, err := Parse(c.line)
		if err != nil {
			t.Fatalf("%s: %v", c.line, err)
		}
		got := l.Next(at(t, c.loc, c.from), c.loc)
		want := at(t, c.loc, c.want)
		if !got.Equal(want) {
			t.Errorf("%q after %s: %s, want %s", c.line, c.from, got.In(c.loc), want.In(c.loc))
		}
	}
}

// The autumn day in Paris: 02:00–03:00 comes twice; a line at 02:30 comes
// once, never twice.
func TestTheHourTwiceComesOnce(t *testing.T) {
	paris, _ := time.LoadLocation("Europe/Paris")
	l, _ := Parse("30 2 * * *")
	first := l.Next(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), paris) // 02:00 CEST
	second := l.Next(first, paris)
	if u := first.UTC(); u.Day() != 25 || u.Minute() != 30 || u.Hour() > 1 {
		t.Fatalf("02:30 on the 25th is 00:30 or 01:30 UTC: %s", u)
	}
	if second.Sub(first) < 23*time.Hour {
		t.Fatalf("02:30 came again an hour later: %s then %s", first.UTC(), second.UTC())
	}
}

func TestLast(t *testing.T) {
	l, _ := Parse("0 3 * * 0")
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// four Sundays passed: the latest of them, once
	last, ok := l.Last(since, time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC), time.UTC)
	if !ok || !last.Equal(time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("the last Sunday before the 29th: %s %v", last, ok)
	}
	if _, ok := l.Last(since, time.Date(2026, 9, 6, 2, 59, 0, 0, time.UTC), time.UTC); ok {
		t.Fatal("no Sunday 03:00 between the 1st and the 6th at 02:59")
	}
}

func TestRefused(t *testing.T) {
	for line, words := range map[string]string{
		"* * * *":        "five fields",
		"60 * * * *":     "minute",
		"* 24 * * *":     "hour",
		"* * 0 * *":      "day of the month",
		"* * * 13 *":     "month",
		"* * * * 8":      "day of the week",
		"*/0 * * * *":    "a step",
		"5-1 * * * *":    "a range goes up",
		"0 0 30 2 *":     "never comes",
		"0 0 * * funday": "day of the week",
	} {
		if _, err := Parse(line); err == nil || !strings.Contains(err.Error(), words) {
			t.Errorf("%q: want %q, got %v", line, words, err)
		}
	}
}
