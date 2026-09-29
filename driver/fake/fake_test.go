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
