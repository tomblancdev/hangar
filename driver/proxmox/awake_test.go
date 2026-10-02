package proxmox

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
)

// gone imitates a call the API hands to a node that is not there, as a live
// cluster answered it: nothing for as long as the node it was asked through
// keeps trying (30 s there, hang here), then 595. It counts the calls it got.
func gone(hang time.Duration, asked *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(hang):
		}
		w.WriteHeader(595)
		_, _ = w.Write([]byte(`{"data":null,"message":"Connection timed out"}`))
	}
}

// within runs f and fails the test when it took longer than most.
func within(t *testing.T, most time.Duration, what string, f func()) {
	t.Helper()
	start := time.Now()
	f()
	if took := time.Since(start); took > most {
		t.Fatalf("%s took %s (at most %s)", what, took.Round(time.Millisecond), most)
	}
}

// A zone asleep is known at once: the cluster's own list says its node is
// offline, and the node — which would answer nothing for 30 s — is not asked.
func TestASleepingZoneIsReadFromTheClustersListAtOnce(t *testing.T) {
	a := newAPI()
	a.nodes = []nodeEntry{{Node: "node-a", Status: "offline"}, {Node: "node-b", Status: "online"}}
	var asked atomic.Int32
	a.h["GET /nodes/node-a/version"] = gone(3*time.Second, &asked)
	d := open(t, a, nil)
	within(t, time.Second, "asking a zone whose node is offline", func() {
		if up, err := d.Awake(context.Background()); up || err != nil {
			t.Fatalf("a zone whose node is offline reads awake=%v, %v", up, err)
		}
	})
	if n := asked.Load(); n != 0 {
		t.Fatalf("the offline node was asked %d time(s)", n)
	}
}

// A node the list does not call offline is asked — and given its own time to
// answer, whatever time the caller has: "online" is also a node that stopped
// seconds ago, "unknown" one the cluster has no word on.
func TestANodeThatDoesNotAnswerIsGivenItsOwnTime(t *testing.T) {
	for _, state := range []string{"online", "unknown"} {
		a := newAPI()
		a.nodes = []nodeEntry{{Node: "node-a", Status: state}}
		var asked atomic.Int32
		a.h["GET /nodes/node-a/version"] = gone(3*time.Second, &asked)
		d := open(t, a, nil)
		d.answerIn = 150 * time.Millisecond
		// a caller with no deadline of its own: the bound is the driver's
		within(t, 1500*time.Millisecond, "asking a node that reads "+state+" and does not answer", func() {
			if up, err := d.Awake(context.Background()); up || err != nil {
				t.Fatalf("a node that does not answer reads awake=%v, %v", up, err)
			}
		})
		if n := asked.Load(); n != 1 {
			t.Fatalf("a node that reads %s was asked %d time(s), want 1", state, n)
		}
	}
}

// The list never says awake by itself: the node's own answer does. And a list
// that cannot be read is no zone awake.
func TestAwakeIsTheNodesOwnAnswer(t *testing.T) {
	version := func(w http.ResponseWriter, _ *http.Request) { data(w, map[string]string{"version": "9.2.1"}) }
	refused := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(595)
		_, _ = w.Write([]byte(`{"data":null,"message":"Connection refused"}`))
	}
	for name, c := range map[string]struct {
		nodes []nodeEntry
		node  http.HandlerFunc
		want  bool
	}{
		"online and answering":                  {[]nodeEntry{{Node: "node-a", Status: "online"}}, version, true},
		"unknown and answering (a busy node)":   {[]nodeEntry{{Node: "node-a", Status: "unknown"}}, version, true},
		"not in the list and answering":         {[]nodeEntry{{Node: "node-b", Status: "online"}}, version, true},
		"online and refusing (it is booting)":   {[]nodeEntry{{Node: "node-a", Status: "online"}}, refused, false},
		"another node offline, its own answers": {[]nodeEntry{{Node: "node-a", Status: "online"}, {Node: "node-b", Status: "offline"}}, version, true},
		"offline, whatever the node would say":  {[]nodeEntry{{Node: "node-a", Status: "offline"}}, version, false},
		"its own offline, another node answers": {[]nodeEntry{{Node: "node-a", Status: "offline"}, {Node: "node-b", Status: "online"}}, version, false},
	} {
		a := newAPI()
		a.nodes = c.nodes
		a.h["GET /nodes/node-a/version"] = c.node
		d := open(t, a, nil)
		if up, err := d.Awake(context.Background()); up != c.want || err != nil {
			t.Errorf("%s: awake=%v, %v; want %v", name, up, err, c.want)
		}
	}
	// the list itself refused: nothing says the zone is awake
	a := newAPI()
	a.h["GET /nodes/node-a/version"] = version
	d := open(t, a, nil)
	a.h["GET /cluster/resources"] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }
	if up, err := d.Awake(context.Background()); up || err != nil {
		t.Errorf("a list that cannot be read: awake=%v, %v", up, err)
	}
}

// What a reservation waits on, on a node that sleeps: a guest of an offline
// node does not run, said at once and without asking it; a node that is not
// called offline and does not answer is an error — what was known stays —
// within the driver's own time.
func TestAWatchedGuestOnASleepingNode(t *testing.T) {
	ctx := context.Background()
	a := newAPI()
	a.res = []resource{{VMID: 4100, Node: "node-a", Type: "qemu", Name: "priority"}}
	a.nodes = []nodeEntry{{Node: "node-a", Status: "offline"}}
	// what the node says of the guest's power; "" = nothing, as a node gone
	var asked atomic.Int32
	var says atomic.Value
	says.Store("")
	silent := gone(3*time.Second, &asked)
	a.h["GET /nodes/node-a/qemu/4100/status/current"] = func(w http.ResponseWriter, r *http.Request) {
		if st := says.Load().(string); st != "" {
			data(w, map[string]any{"status": st})
			return
		}
		silent(w, r)
	}
	d := open(t, a, nil)
	d.watch = []string{"4100"}
	d.answerIn = 150 * time.Millisecond
	within(t, time.Second, "reading a watched guest of an offline node", func() {
		if on, err := d.GuestRunning(ctx, "4100"); on || err != nil {
			t.Fatalf("a guest of an offline node reads running=%v, %v", on, err)
		}
	})
	if n := asked.Load(); n != 0 {
		t.Fatalf("the offline node was asked %d time(s)", n)
	}

	// the seconds before the cluster notices: it cannot be read, and says so
	a.nodes = []nodeEntry{{Node: "node-a", Status: "online"}}
	within(t, 1500*time.Millisecond, "reading a watched guest of a node that does not answer", func() {
		on, err := d.GuestRunning(ctx, "4100")
		if on || err == nil || errors.Is(err, driver.ErrRefused) || errors.Is(err, driver.ErrNotFound) {
			t.Fatalf("a guest whose node does not answer reads running=%v, %v", on, err)
		}
		if !strings.Contains(err.Error(), "node node-a did not say within 150ms whether guest 4100 runs") {
			t.Fatalf("the error does not say what was waited for: %v", err)
		}
	})

	// the control: the node answers, and its word is the guest's power
	for status, want := range map[string]bool{"running": true, "stopped": false} {
		says.Store(status)
		if on, err := d.GuestRunning(ctx, "4100"); on != want || err != nil {
			t.Fatalf("a %s guest reads running=%v, %v", status, on, err)
		}
	}
	// a caller that gives up first is the caller's end, not the node's silence
	says.Store("")
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := d.GuestRunning(short, "4100"); err == nil || strings.Contains(err.Error(), "did not say within") {
		t.Fatalf("a caller's own deadline read as the node's: %v", err)
	}
}
