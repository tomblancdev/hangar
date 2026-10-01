package proxmox

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
)

func f(v float64) *float64 { return &v }

// history builds samples a minute apart ending at end: each entry is the
// minute's cpu (a share of 2 cores) and bytes sent a second; a negative cpu
// is a minute with no reading.
func history(end int64, minutes ...[2]float64) []sample {
	var out []sample
	for i, m := range minutes {
		s := sample{Time: end - int64(len(minutes)-1-i)*60}
		if m[0] >= 0 {
			s.CPU, s.MaxCPU, s.NetOut = f(m[0]), f(2), f(m[1])
		}
		out = append(out, s)
	}
	return out
}

// A guest's quiet is walked back from its newest sample, a minute at a time:
// a busy minute, a hole or the window ends the walk; no fresh sample says
// nothing.
func TestQuietIsReadFromTheHistory(t *testing.T) {
	now := time.Unix(1790830000, 0)
	end := now.Unix() - 20
	q := driver.Quiet{CPU: 0.05, SentBps: 20}
	idle, busyCPU, busyNet, none := [2]float64{0.004, 3}, [2]float64{0.166, 1}, [2]float64{0.001, 48}, [2]float64{-1, 0}
	long := now.Add(-24 * time.Hour) // it runs since yesterday
	for name, c := range map[string]struct {
		samples []sample
		now     time.Time
		window  time.Duration
		want    time.Duration
	}{
		"quiet all along, as far as asked":    {history(end, idle, idle, idle, idle, idle, idle, idle), now, 5 * time.Minute, 5 * time.Minute},
		"quiet for less than asked":           {history(end, idle, idle, idle), now, 30 * time.Minute, 3 * time.Minute},
		"a busy minute ends the walk":         {history(end, idle, idle, busyCPU, idle, idle), now, 30 * time.Minute, 2 * time.Minute},
		"what it sends counts as its CPU":     {history(end, idle, busyNet, idle, idle, idle), now, 30 * time.Minute, 3 * time.Minute},
		"busy now":                            {history(end, idle, idle, idle, busyCPU), now, 30 * time.Minute, 0},
		"the minute being written is passed":  {history(end, idle, idle, idle, none), now, 30 * time.Minute, 3 * time.Minute},
		"a hole ends the walk":                {history(end, idle, idle, none, idle, idle), now, 30 * time.Minute, 2 * time.Minute},
		"a guest just started has no reading": {history(end, none, none, none), now, 30 * time.Minute, 0},
		"no history at all":                   {nil, now, 30 * time.Minute, 0},
		"a history no longer written":         {history(end, idle, idle, idle, idle), now.Add(10 * time.Minute), 30 * time.Minute, 0},
		// cpu is a share of the guest's cores: 0.03 of 2 cores is 0.06 cores' worth
		"cores' worth, not a share": {history(end, idle, [2]float64{0.03, 0}, idle), now, 30 * time.Minute, time.Minute},
	} {
		if got := quietFor(c.samples, c.now, long, c.window, q); got != c.want {
			t.Errorf("%s: quiet for %s, want %s", name, got, c.want)
		}
	}
	// The history is kept by the guest's number: what is older than this
	// guest's own start is another run's — or another guest's, deleted a
	// minute ago, whose number this one took.
	quiet := history(end, idle, idle, idle, idle, idle, idle, idle, idle)
	for name, c := range map[string]struct {
		up   time.Duration
		want time.Duration
	}{
		"made 30 s ago on a number with a quiet past": {30 * time.Second, 0},
		"its first whole minute":                      {100 * time.Second, time.Minute},
		"three and a half minutes old":                {210 * time.Second, 3 * time.Minute},
		"older than the window":                       {time.Hour, 5 * time.Minute},
	} {
		if got := quietFor(quiet, now, now.Add(-c.up), 5*time.Minute, q); got != c.want {
			t.Errorf("%s: quiet for %s, want %s", name, got, c.want)
		}
	}
}

// Through the API: the hour's history for a short window, the day's for a
// long one; a stopped guest is not asked; and a guest's start is its uptime.
func TestQuietForAsksTheGuestsOwnHistory(t *testing.T) {
	a := newAPI()
	a.res = []resource{{VMID: 11001, Node: "node-a", Type: "qemu", Name: "m", Pool: "hangar", Tags: "hangar-id.m-0123456789abcdef0"}}
	status := "running"
	a.h["GET /nodes/node-a/qemu/11001/status/current"] = func(w http.ResponseWriter, _ *http.Request) {
		data(w, map[string]any{"status": status, "uptime": 600, "mem": 1 << 28})
	}
	a.h["GET /nodes/node-a/qemu/11001/config"] = func(w http.ResponseWriter, _ *http.Request) {
		data(w, map[string]any{"tags": "hangar-id.m-0123456789abcdef0", "cores": 2, "memory": "1024"})
	}
	var asked []string
	a.h["GET /nodes/node-a/qemu/11001/rrddata"] = func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Query().Get("timeframe")+"/"+r.URL.Query().Get("cf"))
		data(w, history(time.Now().Unix()-30, [2]float64{0.004, 0}, [2]float64{0.004, 0}, [2]float64{0.004, 0}, [2]float64{0.004, 0}))
	}
	d := open(t, a, nil)
	ctx := context.Background()
	q := driver.Quiet{CPU: 0.05, SentBps: 20}
	got, err := d.QuietFor(ctx, "m-0123456789abcdef0", 30*time.Minute, q)
	if err != nil || got != 4*time.Minute {
		t.Fatalf("quiet for %s %v", got, err)
	}
	if _, err := d.QuietFor(ctx, "m-0123456789abcdef0", 2*time.Hour, q); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 2 || asked[0] != "hour/AVERAGE" || asked[1] != "day/AVERAGE" {
		t.Fatalf("the history asked for: %v", asked)
	}
	g, err := d.Guest(ctx, "m-0123456789abcdef0")
	if err != nil || g.StartedAt == nil || g.At.IsZero() {
		t.Fatalf("%+v %v", g, err)
	}
	if up := g.At.Sub(*g.StartedAt); up != 600*time.Second {
		t.Fatalf("it runs since its uptime: %s", up)
	}
	status = "stopped"
	if got, err := d.QuietFor(ctx, "m-0123456789abcdef0", 30*time.Minute, q); err != nil || got != 0 || len(asked) != 2 {
		t.Fatalf("a stopped guest: %s %v, history asked %d times", got, err, len(asked))
	}
	if g, _ = d.Guest(ctx, "m-0123456789abcdef0"); g.StartedAt != nil {
		t.Fatalf("a stopped guest runs since %v", g.StartedAt)
	}
}
