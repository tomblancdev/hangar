package proxmox

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/internal/ids"
)

// A guest's activity on a real Proxmox VE, read with the fenced token from
// the node's own history: a container and a VM left alone read quiet once
// they have run their first whole minutes; twenty seconds of one busy core
// in the container end its quiet, and so does a packet a second sent from
// it; and a guest says since when it runs. The node keeps that history by
// the guest's number, and keeps it when the guest is deleted: a guest made on
// a dead one's number must not read the dead one's quiet as its own.
//
//	sh tools/bench/bench.sh up && eval "$(sh tools/bench/bench.sh env)" && go test ./driver/proxmox/ -run BenchAGuestsActivity -v
func TestBenchAGuestsActivity(t *testing.T) {
	b := onBench(t)
	d := b.open(t, b.token)
	ctx := context.Background()
	if !has(d.Capabilities(), driver.GuestActivity) {
		t.Fatalf("no guest.activity: %v", d.Capabilities())
	}
	archive := "local:vztmpl/" + b.must(t, "ls /var/lib/vz/template/cache/ | grep \"^debian-13-standard_.*_$(dpkg --print-architecture)\\.\" | sort -V | tail -1")
	ct, vm, heir := ids.New("m"), ids.New("m"), ids.New("m")
	t.Cleanup(func() {
		for _, id := range []string{ct, vm, heir} {
			if err := d.DeleteGuest(context.Background(), id); err != nil {
				t.Errorf("cleaning up %s: %v", id, err)
			}
		}
	})
	born := time.Now()
	c, err := d.CreateGuest(ctx, driver.GuestSpec{ID: ct, Kind: "container", Name: "quiet-ct", Cores: 2, MemoryMB: 512, DiskGB: 2, Image: archive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateGuest(ctx, driver.GuestSpec{ID: vm, Kind: "vm", Name: "quiet-vm", Cores: 1, MemoryMB: 1024, Image: "debian-13"}); err != nil {
		t.Fatal(err)
	}
	// since when it runs: its start, within the seconds its create took
	if c.StartedAt == nil || c.StartedAt.Before(born.Add(-5*time.Second)) || c.StartedAt.After(time.Now()) {
		t.Fatalf("the container runs since %v; it was made between %s and %s", c.StartedAt, born.Format(time.TimeOnly), time.Now().Format(time.TimeOnly))
	}
	q := driver.Quiet{CPU: 0.05, SentBps: 20}
	const window = 3 * time.Minute
	quiet := func(id string) time.Duration {
		t.Helper()
		got, err := d.QuietFor(ctx, id, window, q)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// just made, the history has no whole minute of them: nothing says idle
	if got := quiet(ct); got != 0 {
		t.Fatalf("a container %s old read quiet for %s", time.Since(born).Round(time.Second), got)
	}
	// until reads true within a time, looking every ten seconds
	until := func(what string, within time.Duration, ok func() bool) time.Duration {
		t.Helper()
		start := time.Now()
		for !ok() {
			if time.Since(start) > within {
				t.Fatalf("%s: not within %s", what, within)
			}
			time.Sleep(10 * time.Second)
		}
		return time.Since(start).Round(time.Second)
	}
	// left alone, both read quiet for the whole window — the VM once its
	// first boot is over
	took := until("both quiet", 9*time.Minute, func() bool { return quiet(ct) == window && quiet(vm) == window })
	t.Logf("a container and a VM left alone read quiet for %s, %s after they were made", window, time.Since(born).Round(time.Second))
	_ = took
	if s, _ := d.Guest(ctx, vm); s.StartedAt == nil || s.At.Sub(*s.StartedAt) < 2*time.Minute {
		t.Fatalf("the VM runs since %v", s.StartedAt)
	}

	// twenty seconds of one busy core: its minute reads above a twentieth
	// of a core, and the quiet begins again after it
	v := vmid(c)
	b.must(t, "pct exec "+v+" -- timeout 20 sh -c 'while :; do :; done' || true")
	took = until("the burst is seen", 3*time.Minute, func() bool { return quiet(ct) < window })
	t.Logf("20 s of one busy core ended the container's quiet, seen %s after the burst (it reads %s)", took, quiet(ct))
	if got := quiet(vm); got != window {
		t.Fatalf("the VM beside it read %s", got)
	}
	until("quiet again", 6*time.Minute, func() bool { return quiet(ct) == window })

	// a packet a second sent from it: under any CPU threshold, above 20 B/s
	gw := b.must(t, "pct exec "+v+" -- sh -c \"ip route | awk '/default/ {print \\$3}'\"")
	b.must(t, "pct exec "+v+" -- sh -c 'nohup ping -q -i 1 "+gw+" >/dev/null 2>&1 & echo $! >/tmp/ping.pid'")
	took = until("the packets are seen", 3*time.Minute, func() bool { return quiet(ct) < window })
	t.Logf("a packet a second ended the container's quiet, seen %s after the first (it reads %s)", took, quiet(ct))
	time.Sleep(70 * time.Second)
	if got := quiet(ct); got != 0 {
		t.Fatalf("sending a packet a second for minutes, it read quiet for %s", got)
	}
	// the control: the same minutes with what it sends not counted read quiet
	if got, err := d.QuietFor(ctx, ct, time.Minute, driver.Quiet{CPU: 0.05, SentBps: 1e9}); err != nil || got != time.Minute {
		t.Fatalf("its CPU alone read busy: %s %v", got, err)
	}
	b.must(t, "pct exec "+v+" -- sh -c 'kill $(cat /tmp/ping.pid)'")

	// a stopped guest is not quiet, and runs since nothing
	if s, err := d.SetPower(ctx, ct, false); err != nil || s.StartedAt != nil {
		t.Fatalf("%+v %v", s, err)
	}
	if got := quiet(ct); got != 0 {
		t.Fatalf("a stopped container read quiet for %s", got)
	}

	// the VM, quiet for minutes, is deleted; a container is made at once and
	// takes its number — and with it the VM's history, which the node keeps
	dead, err := d.Guest(ctx, vm)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteGuest(ctx, vm); err != nil {
		t.Fatal(err)
	}
	h, err := d.CreateGuest(ctx, driver.GuestSpec{ID: heir, Kind: "container", Name: "heir-ct", Cores: 1, MemoryMB: 512, DiskGB: 2, Image: archive})
	if err != nil {
		t.Fatal(err)
	}
	if vmid(h) != vmid(dead) {
		t.Fatalf("the new container took %s, not the deleted VM's %s: the bench is not empty", vmid(h), vmid(dead))
	}
	// the control: that number's history, read as it is, says quiet for the
	// whole window — the dead VM's minutes
	var raw []sample
	if err := d.c.call(ctx, http.MethodGet, "/nodes/pve-bench/lxc/"+vmid(h)+"/rrddata", url.Values{"timeframe": {"hour"}, "cf": {"AVERAGE"}}, &raw); err != nil {
		t.Fatal(err)
	}
	past := quietFor(raw, time.Now(), time.Now().Add(-24*time.Hour), window, q)
	if past != window {
		t.Fatalf("the control: number %s's history reads quiet for %s, not the dead VM's %s", vmid(h), past, window)
	}
	if got := quiet(heir); got != 0 {
		t.Fatalf("a container %s old, on a number whose history reads quiet for %s, read quiet for %s", time.Since(*h.StartedAt).Round(time.Second), past, got)
	}
	t.Logf("a container made on the deleted VM's number %s: that number's history reads quiet for %s, the container reads %s", vmid(h), past, quiet(heir))
}
