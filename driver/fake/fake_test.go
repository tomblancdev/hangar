package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
)

func open(t *testing.T, endpoint string, opts map[string]string) *Engine {
	t.Helper()
	d, err := driver.Open(context.Background(), Name, driver.Params{Zone: "z", Endpoint: endpoint, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	return d.(*Engine)
}

func TestCreateIsIdempotentOnTheID(t *testing.T) {
	e := open(t, "", nil)
	ctx := context.Background()
	spec := driver.GuestSpec{ID: "box-1", Kind: "container", Cores: 2, MemoryMB: 1024}
	a, err := e.CreateGuest(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := e.CreateGuest(ctx, spec)
	all, _ := e.Guests(ctx)
	if a.EngineRef != b.EngineRef || len(all) != 1 {
		t.Fatalf("a retried create made a second guest: %v", all)
	}
}

func TestTheFileIsTheEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zone.json")
	ctx := context.Background()
	e := open(t, path, nil)
	if _, err := e.CreateGuest(ctx, driver.GuestSpec{ID: "box-1", Kind: "container", Cores: 1, MemoryMB: 1024}); err != nil {
		t.Fatal(err)
	}
	// someone edits the engine behind the brain's back
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), `"cores": 1`, `"cores": 7`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := e.Guest(ctx, "box-1")
	if err != nil || g.Cores != 7 {
		t.Fatalf("the edit was not seen: %+v %v", g, err)
	}
	// a second engine opened on the file sees the same guests
	if _, err := open(t, path, nil).Guest(ctx, "box-1"); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	if _, err := e.Guest(ctx, "box-1"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatal("a removed file is an empty engine")
	}
}

func TestCapabilitiesAreEnforced(t *testing.T) {
	ctx := context.Background()
	e := open(t, "", map[string]string{"capabilities": "kind.container,guest.tags"})
	if _, err := e.CreateGuest(ctx, driver.GuestSpec{ID: "v", Kind: "vm"}); !errors.Is(err, driver.ErrRefused) {
		t.Fatal("a zone without kind.vm ran a VM")
	}
	if _, err := e.CreateGuest(ctx, driver.GuestSpec{ID: "c", Kind: "container", Cores: 1, MemoryMB: 2048}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ResizeGuest(ctx, "c", 1, 1024); !errors.Is(err, driver.ErrRefused) {
		t.Fatal("a running guest's memory went down without resize.live.memory_down")
	}
	if _, err := e.SetPower(ctx, "c", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ResizeGuest(ctx, "c", 1, 1024); err != nil {
		t.Fatalf("a stopped guest can shrink: %v", err)
	}
	if _, err := driver.Open(ctx, Name, driver.Params{Options: map[string]string{"capabilities": "kind.teleport"}}); err == nil {
		t.Fatal("a driver advertised a capability nobody documents")
	}
}

func TestTheZoneCanExpectACredential(t *testing.T) {
	sum := sha256.Sum256([]byte("right"))
	opts := map[string]string{"expect_credential_sha256": hex.EncodeToString(sum[:])}
	if _, err := driver.Open(context.Background(), Name, driver.Params{Options: opts, Credential: []byte("wrong")}); err == nil {
		t.Fatal("the wrong credential opened the zone")
	}
	if _, err := driver.Open(context.Background(), Name, driver.Params{Options: opts, Credential: []byte("right")}); err != nil {
		t.Fatal(err)
	}
}

// The engine has a clock of its own, which the file moves: a guest says
// since when it runs, every reading is stamped with that clock, and its
// history of activity is "busy until" — quiet since it started otherwise.
func TestTheClockAndTheActivity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zone.json")
	ctx := context.Background()
	e := open(t, path, nil)
	t0 := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	e.SetNow(t0)
	g, err := e.CreateGuest(ctx, driver.GuestSpec{ID: "m-1", Kind: "vm", Cores: 2, MemoryMB: 1024})
	if err != nil || g.StartedAt == nil || !g.StartedAt.Equal(t0) || !g.At.Equal(t0) {
		t.Fatalf("born running at the engine's time: %+v %v", g, err)
	}
	q := driver.Quiet{CPU: 0.05, SentBps: 20}
	quiet := func(window time.Duration) time.Duration {
		t.Helper()
		d, err := e.QuietFor(ctx, "m-1", window, q)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	e.SetNow(t0.Add(40 * time.Minute))
	if g, _ = e.Guest(ctx, "m-1"); !g.At.Equal(t0.Add(40*time.Minute)) || !g.StartedAt.Equal(t0) {
		t.Fatalf("a reading is stamped with the engine's clock: %+v", g)
	}
	if d := quiet(time.Hour); d != 40*time.Minute {
		t.Fatalf("quiet since it started: %s", d)
	}
	if d := quiet(30 * time.Minute); d != 30*time.Minute {
		t.Fatalf("no further back than asked: %s", d)
	}
	e.SetBusy("m-1", t0.Add(35*time.Minute))
	if d := quiet(time.Hour); d != 5*time.Minute {
		t.Fatalf("quiet since it was last busy: %s", d)
	}
	e.FailActivity(true)
	if _, err := e.QuietFor(ctx, "m-1", time.Hour, q); err == nil {
		t.Fatal("an unreadable history answered")
	}
	e.FailActivity(false)
	// a stopped guest is not quiet: it does not run; started again, its
	// quiet begins anew
	if g, _ = e.SetPower(ctx, "m-1", false); g.StartedAt != nil {
		t.Fatalf("a stopped guest runs since: %+v", g)
	}
	if d := quiet(time.Hour); d != 0 {
		t.Fatalf("a stopped guest read quiet for %s", d)
	}
	e.SetNow(t0.Add(2 * time.Hour))
	if g, _ = e.SetPower(ctx, "m-1", true); g.StartedAt == nil || !g.StartedAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("started again at the engine's time: %+v", g)
	}
	if d := quiet(time.Hour); d != 0 {
		t.Fatalf("just started, quiet for %s", d)
	}
	if !e.Now().Equal(t0.Add(2 * time.Hour)) {
		t.Fatalf("the engine's clock reads %s", e.Now())
	}
}

// A guest's processor and its share of the cores are kept as they were asked,
// and each needs the zone's flag for it.
func TestAGuestsProcessor(t *testing.T) {
	ctx := context.Background()
	e := open(t, "", nil)
	g, err := e.CreateGuest(ctx, driver.GuestSpec{ID: "m-1", Kind: "vm", Cores: 2, MemoryMB: 1024, CPU: driver.CPUModelHost, Virtualization: true, CPUWeight: 25})
	if err != nil || g.CPU != driver.CPUModelHost || !g.Virtualization || g.CPUWeight != 25 {
		t.Fatalf("%+v %v", g, err)
	}
	if g, err = e.CreateGuest(ctx, driver.GuestSpec{ID: "m-2", Kind: "container", Cores: 1, MemoryMB: 512}); err != nil || g.CPU != "" || g.Virtualization || g.CPUWeight != driver.FullWeight {
		t.Fatalf("one that asks nothing: %+v %v", g, err)
	}
	if g, err = e.SetCPUWeight(ctx, "m-2", 40); err != nil || g.CPUWeight != 40 {
		t.Fatalf("a share set: %+v %v", g, err)
	}
	if g, err = e.SetCPUWeight(ctx, "m-2", 0); err != nil || g.CPUWeight != driver.FullWeight {
		t.Fatalf("none named is a full share: %+v %v", g, err)
	}
	for name, s := range map[string]driver.GuestSpec{
		"another model":           {ID: "x-1", Kind: "vm", CPU: "EPYC"},
		"a container's processor": {ID: "x-2", Kind: "container", CPU: driver.CPUModelHost},
		"VMs inside a container":  {ID: "x-3", Kind: "container", Virtualization: true},
		"more than a full share":  {ID: "x-4", Kind: "vm", CPUWeight: 101},
		"less than nothing":       {ID: "x-5", Kind: "vm", CPUWeight: -1},
	} {
		if _, err := e.CreateGuest(ctx, s); !errors.Is(err, driver.ErrRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.SetCPUWeight(ctx, "m-2", 101); !errors.Is(err, driver.ErrRefused) {
		t.Errorf("a share above a full one: %v", err)
	}
	// a zone without the flags
	bare := open(t, "", map[string]string{"capabilities": "kind.vm,kind.container,guest.tags,fence.pool"})
	for name, s := range map[string]driver.GuestSpec{
		"its host's processor": {ID: "y-1", Kind: "vm", CPU: driver.CPUModelHost},
		"VMs inside":           {ID: "y-2", Kind: "vm", Virtualization: true},
		"a share":              {ID: "y-3", Kind: "vm", CPUWeight: 25},
	} {
		if _, err := bare.CreateGuest(ctx, s); !errors.Is(err, driver.ErrRefused) {
			t.Errorf("a bare zone, %s: %v", name, err)
		}
	}
	if g, err := bare.CreateGuest(ctx, driver.GuestSpec{ID: "y-4", Kind: "vm", Cores: 1, MemoryMB: 512, CPUWeight: driver.FullWeight}); err != nil || g.CPUWeight != driver.FullWeight {
		t.Fatalf("a full share where nothing is weighed: %+v %v", g, err)
	}
	if _, err := bare.SetCPUWeight(ctx, "y-4", 25); !errors.Is(err, driver.ErrRefused) {
		t.Errorf("a share set where nothing is weighed: %v", err)
	}
}
