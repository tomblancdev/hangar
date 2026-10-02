package proxmox

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/internal/ids"
)

// notes reads a guest's description on the node, as Proxmox holds it.
func (b *bench) notes(t *testing.T, vmid string) string {
	t.Helper()
	return b.must(t, "pvesh get /nodes/pve-bench/lxc/"+vmid+"/config --output-format json | python3 -c 'import json,sys; print(json.load(sys.stdin).get(\"description\", \"\"), end=\"\")'")
}

// What a machine and its volumes are called is read in the guest's notes on
// Proxmox's own screen — written at birth, kept true by a rename, carried
// with a volume that moves, gone with one that is deleted — and nothing the
// driver finds a guest or a volume by moves. Two writers share those notes
// (the machines' driver and the volumes'): a write made on a config that
// changed since it was read is refused by Proxmox itself.
func TestBenchNamesInAGuestsNotes(t *testing.T) {
	b := onBench(t)
	archive := "local:vztmpl/" + b.must(t, "ls /var/lib/vz/template/cache/ | grep \"^debian-13-standard_.*_$(dpkg --print-architecture)\\.\" | sort -V | tail -1")
	m, v := b.openVolumes(t, archive)
	ctx := context.Background()
	owner := "bench-owner-" + ids.New("x")
	id, vol := ids.New("m"), ids.New("vol")
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := v.PlaceVolume(ctx, vol, driver.Place{Owner: owner}); err == nil || !errors.Is(err, driver.ErrNotFound) {
			if err := v.DeleteVolume(ctx, vol); err != nil {
				t.Errorf("cleaning up %s: %v", vol, err)
			}
		}
		if err := m.DeleteGuest(ctx, id); err != nil {
			t.Errorf("cleaning up %s: %v", id, err)
		}
		b.dropShelves(t, "pct", owner)
	})

	g, err := m.CreateGuest(ctx, driver.GuestSpec{ID: id, Kind: "container", Name: "bench-names", Label: "bench-names · machine of alice — the build box",
		Cores: 1, MemoryMB: 256, DiskGB: 2, Image: archive, Tags: map[string]string{"class": "spot"}, Stopped: true})
	if err != nil {
		t.Fatal(err)
	}
	if g.Label != "bench-names · machine of alice — the build box" || g.Name != "bench-names" {
		t.Fatalf("born as %+v", g)
	}
	want := marker(id) + "\nhangar name " + id + " bench-names · machine of alice — the build box"
	if got := b.notes(t, vmid(g)); strings.TrimRight(got, "\n") != want {
		t.Fatalf("the notes at birth:\n%s\nwant:\n%s", got, want)
	}
	t.Logf("the notes at birth:\n%s", want)

	// a volume plugged into it, named: its line, and under it what it is called
	got, err := v.CreateVolume(ctx, driver.VolumeSpec{ID: vol, Content: driver.ContentFilesystem, SizeGB: 1, At: driver.Place{Guest: id, Mount: "/home", Owner: owner}, Label: "home"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "home" || got.Guest != id {
		t.Fatalf("the volume: %+v", got)
	}
	notes := b.notes(t, vmid(g))
	lines := strings.Split(strings.TrimRight(notes, "\n"), "\n")
	if len(lines) != 4 || lines[0] != marker(id) || !strings.HasPrefix(lines[1], "hangar name "+id+" ") ||
		!strings.HasPrefix(lines[2], volumeWord+" "+vol+" mp0 ") || lines[3] != "hangar name "+vol+" home" {
		t.Fatalf("the notes with a volume:\n%s", notes)
	}
	t.Logf("with a volume:\n%s", notes)

	// renamed, each by its own driver: the other's lines are as they were, and both are still found
	if g, err = m.Relabel(ctx, id, "forge · machine of alice"); err != nil || g.Label != "forge · machine of alice" {
		t.Fatalf("the guest renamed: %+v %v", g, err)
	}
	if got, err = v.RelabelVolume(ctx, vol, "maison"); err != nil || got.Label != "maison" || got.Guest != id || got.Mount != "/home" {
		t.Fatalf("the volume renamed: %+v %v", got, err)
	}
	notes = b.notes(t, vmid(g))
	lines = strings.Split(strings.TrimRight(notes, "\n"), "\n")
	if len(lines) != 4 || lines[1] != "hangar name "+id+" forge · machine of alice" || lines[3] != "hangar name "+vol+" maison" {
		t.Fatalf("the notes after both renames:\n%s", notes)
	}
	if again, err := m.Guest(ctx, id); err != nil || again.Label != "forge · machine of alice" || again.Name != "bench-names" {
		t.Fatalf("read again: %+v %v", again, err)
	}
	// the same name again writes nothing: the config's digest does not move
	var before, after map[string]any
	r, _ := m.find(ctx, id)
	_ = m.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &before)
	if _, err := m.Relabel(ctx, id, "forge · machine of alice"); err != nil {
		t.Fatal(err)
	}
	_ = m.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &after)
	if str(before["digest"]) == "" || str(before["digest"]) != str(after["digest"]) {
		t.Fatalf("a name unchanged was written: digest %v → %v", before["digest"], after["digest"])
	}

	// a write made on a config that changed since it was read is refused: the
	// volumes' driver writes between the machines' read and its write
	stale := before
	if _, err := v.RelabelVolume(ctx, vol, "home"); err != nil {
		t.Fatal(err)
	}
	keep, vl := parseDesc(str(stale["description"]))
	keep, _ = withName(keep, id, "overwriting")
	err = m.writeDesc(ctx, r, stale, formatDesc(keep, vl))
	if err == nil {
		t.Fatalf("a write on a stale config was taken:\n%s", b.notes(t, vmid(g)))
	}
	t.Logf("a stale write is refused: %v", err)
	if notes = b.notes(t, vmid(g)); !strings.Contains(notes, "hangar name "+vol+" home") || strings.Contains(notes, "overwriting") {
		t.Fatalf("the notes after a refused write:\n%s", notes)
	}

	// parked: its name goes with it to its owner's shelf, and leaves the machine's notes
	if got, err = v.PlaceVolume(ctx, vol, driver.Place{Owner: owner}); err != nil || got.Guest != "" || got.Label != "home" {
		t.Fatalf("parked: %+v %v", got, err)
	}
	if notes = b.notes(t, vmid(g)); strings.Contains(notes, vol) || !strings.Contains(notes, "hangar name "+id+" forge") {
		t.Fatalf("the machine's notes after its volume left:\n%s", notes)
	}
	// unnamed: the line goes
	if g, err = m.Relabel(ctx, id, ""); err != nil || g.Label != "" {
		t.Fatalf("unnamed: %+v %v", g, err)
	}
	if notes = b.notes(t, vmid(g)); strings.TrimRight(notes, "\n") != marker(id) {
		t.Fatalf("the notes, unnamed:\n%s", notes)
	}
}
