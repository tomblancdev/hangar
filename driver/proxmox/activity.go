package proxmox

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/tomblancdev/hangar/driver"
)

// The activity facet: whether a guest is idle, read from the history Proxmox
// VE keeps of every guest by itself — nothing is installed in the guest, and
// the token needs VM.Audit on it, which it has.
//
// Read on a 9.2 node: /nodes/<node>/<kind>/<vmid>/rrddata answers one sample
// a minute for the last hour (timeframe=hour, 60 samples) and for the last
// day (timeframe=day, 1439); a week is half-hours. A sample is its minute's
// average, stamped with the minute's END: cpu as a share of the guest's
// cores (× maxcpu = cores' worth), netout in bytes a second. It is there
// within seconds of its minute's end; a minute the guest did not run whole
// has none (null), so a guest just started has no reading at all.
//
// status/current's own "cpu" is not read: it is a rate since the same API
// worker last looked at that guest — three calls in a row answered 0 for a
// VM the node's own statistics saw at 0.97.
//
// The history is kept by VMID, not by guest: a guest made on the number of
// one deleted a minute before finds that guest's samples under its own name
// (read on the bench: a container 22 seconds old read quiet for five
// minutes, and was stopped for it). So nothing older than the guest's own
// start is read — its uptime says when that was.

const (
	// historyStale: a newest sample older than this is a history no longer
	// written (the node's statistics daemon down): it says nothing of now.
	historyStale = 5 * time.Minute
	// hourReach: the longest window the hour's history answers whole.
	hourReach = 50 * time.Minute
)

type sample struct {
	Time   int64    `json:"time"`
	CPU    *float64 `json:"cpu"`
	MaxCPU *float64 `json:"maxcpu"`
	NetOut *float64 `json:"netout"`
}

// QuietFor reads the guest's history as far back as window asks and says how
// long it has stayed quiet up to its newest sample.
func (d *Driver) QuietFor(ctx context.Context, id string, window time.Duration, q driver.Quiet) (time.Duration, error) {
	r, err := d.find(ctx, id)
	if err != nil {
		return 0, d.engine(err)
	}
	var st struct {
		Status string `json:"status"`
		Uptime int64  `json:"uptime"`
	}
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/status/current", nil, &st); err != nil {
		return 0, d.engine(err)
	}
	if st.Status != "running" {
		return 0, nil
	}
	now := time.Now()
	started := now.Add(-time.Duration(st.Uptime) * time.Second)
	timeframe := "hour"
	if window > hourReach {
		timeframe = "day"
	}
	var samples []sample
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/rrddata", url.Values{"timeframe": {timeframe}, "cf": {"AVERAGE"}}, &samples); err != nil {
		return 0, d.engine(err)
	}
	return quietFor(samples, now, started, window, q), nil
}

// quietFor walks a history back from its newest sample for as long as each
// one is quiet, follows the next without a hole, and is of this run of the
// guest — its minute began after started. A hole, a busy sample, the guest's
// start or the window ends the walk; a history with no fresh sample of this
// run says nothing.
func quietFor(samples []sample, now, started time.Time, window time.Duration, q driver.Quiet) time.Duration {
	step := int64(60)
	if len(samples) > 1 && samples[1].Time > samples[0].Time {
		step = samples[1].Time - samples[0].Time
	}
	var newest, oldest, next int64
	for i := len(samples) - 1; i >= 0; i-- {
		s := samples[i]
		if s.CPU == nil || s.MaxCPU == nil || s.NetOut == nil {
			continue // no reading: the minute still being written, or one the guest did not run whole
		}
		if s.Time-step < started.Unix() {
			break // before it started: another run's, or another guest's of the same number
		}
		if newest == 0 {
			if now.Sub(time.Unix(s.Time, 0)) > historyStale {
				return 0
			}
			newest = s.Time
		} else if next-s.Time > step {
			break // a hole: the minutes between have no reading
		}
		if *s.CPU**s.MaxCPU > q.CPU || *s.NetOut > q.SentBps {
			break
		}
		oldest, next = s.Time-step, s.Time
		if time.Duration(newest-oldest)*time.Second >= window {
			break
		}
	}
	if oldest == 0 {
		return 0
	}
	return min(time.Duration(newest-oldest)*time.Second, window)
}

var _ driver.Activity = (*Driver)(nil)
