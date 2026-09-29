package proxmox

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/internal/ids"
)

// openVolumes opens the zone as the volumes plugin does: its own, narrower
// token, and the zone's shelf archive. The machines' driver opens it on the
// same range of ids — two processes in real life, picking from one range.
func (b *bench) openVolumes(t *testing.T, archive string) (*Driver, *Driver) {
	t.Helper()
	file := os.Getenv("HANGAR_BENCH_VOLUMES_TOKEN_FILE")
	if file == "" {
		t.Skip("no volumes token: sh tools/bench/bench.sh up again (its setup makes one)")
	}
	open := func(tokenFile string) *Driver {
		tok, err := os.ReadFile(tokenFile)
		if err != nil {
			t.Fatal(err)
		}
		d, err := Open(context.Background(), driver.Params{
			Zone: "bench", Endpoint: b.url, Credential: tok,
			Options: map[string]string{
				"node": "pve-bench", "pool": "hangar", "images_pool": "hangar-images", "storage": "local-zfs",
				"seed_storage": "hangar-seeds", "bridge": "hbnet", "vmids": "11040-11059", "ca_file": b.ca,
				"shelf_archive": archive,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return d.(*Driver)
	}
	return open(b.token), open(file)
}

// dropShelves destroys a test owner's shelves (a real owner's stay, reused).
func (b *bench) dropShelves(t *testing.T, cmd, owner string) {
	t.Helper()
	b.must(t, "for id in $("+cmd+" list | awk 'NR>1 {print $1}'); do "+cmd+" config $id | grep -q '^description: made by hangar%3A shelf "+ // the CLI prints it url-encoded

		shelfKey(owner)+"' && "+cmd+" destroy $id --purge >/dev/null; done; true")
}

// marker writes a line at the start of a volume, from the node — the
// engine's own view of the bytes, whatever guest holds them.
func (b *bench) marker(t *testing.T, volid, text string) {
	t.Helper()
	b.must(t, "printf '%s' '"+text+"' | dd of=$(pvesm path "+volid+") bs=512 conv=notrunc status=none")
}

func (b *bench) readMarker(t *testing.T, volid string, n int) string {
	t.Helper()
	return b.must(t, "dd if=$(pvesm path "+volid+") bs=1 count="+strconv.Itoa(n)+" status=none")
}

// serial reads, through QEMU's own monitor, the serial of a device a running
// VM carries now — what its guest sees, hot-plugged or not.
func (b *bench) serial(t *testing.T, vmid, dev string) string {
	t.Helper()
	return b.must(t, "perl -MPVE::QemuServer::Monitor -e 'print PVE::QemuServer::Monitor::mon_cmd($ARGV[0], \"qom-get\", path => \"/machine/peripheral/$ARGV[1]\", property => \"serial\")' "+vmid+" "+dev)
}

// config reads one key of a guest's config on the node.
func (b *bench) config(t *testing.T, cmd, vmid, key string) string {
	t.Helper()
	out, _ := b.ssh(t, cmd+" config "+vmid+" | sed -n 's/^"+key+": //p'")
	return out
}

func refused(t *testing.T, err error, words string) {
	t.Helper()
	if !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), words) {
		t.Fatalf("not refused with %q: %v", words, err)
	}
	t.Logf("refused: %v", err)
}

// A container's volume: made parked (its shelf made with it), plugged into a
// running container (hot-mounted), refused leaving it while it runs, moved to
// another once stopped, grown, its backup flag set, parked, deleted — the
// same bytes all along, read inside the containers.
func TestBenchAContainersVolume(t *testing.T) {
	b := onBench(t)
	archive := "local:vztmpl/" + b.must(t, "ls /var/lib/vz/template/cache/ | grep \"^debian-13-standard_.*_$(dpkg --print-architecture)\\.\" | sort -V | tail -1")
	m, v := b.openVolumes(t, archive)
	if v.FenceReport() != "" || !has(v.Capabilities(), driver.VolumeMoveBetweenGuests) {
		t.Fatalf("the volumes token: fence %q, capabilities %v", v.FenceReport(), v.Capabilities())
	}
	ctx := context.Background()
	owner := "bench-owner-" + ids.New("x")
	a, bb := ids.New("m"), ids.New("m")
	vol := ids.New("vol")
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := v.PlaceVolume(ctx, vol, driver.Place{Owner: owner}); err != nil && !errors.Is(err, driver.ErrNotFound) {
			_, _ = m.SetPower(ctx, a, false)
			_, _ = m.SetPower(ctx, bb, false)
			_, _ = v.PlaceVolume(ctx, vol, driver.Place{Owner: owner})
		}
		if err := v.DeleteVolume(ctx, vol); err != nil {
			t.Errorf("cleaning up %s: %v", vol, err)
		}
		for _, id := range []string{a, bb} {
			if err := m.DeleteGuest(ctx, id); err != nil {
				t.Errorf("cleaning up %s: %v", id, err)
			}
		}
		b.dropShelves(t, "pct", owner)
	})
	for _, id := range []string{a, bb} {
		if _, err := m.CreateGuest(ctx, driver.GuestSpec{ID: id, Kind: "container", Cores: 1, MemoryMB: 256, DiskGB: 2, Image: archive}); err != nil {
			t.Fatal(err)
		}
	}
	ga, _ := m.Guest(ctx, a)
	gb, _ := m.Guest(ctx, bb)
	va, vb := vmid(ga), vmid(gb)

	spec := driver.VolumeSpec{ID: vol, Content: driver.ContentFilesystem, SizeGB: 1, At: driver.Place{Owner: owner}}
	got, err := v.CreateVolume(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if got.Guest != "" || got.SizeGB != 1 || got.Backup || got.Content != driver.ContentFilesystem {
		t.Fatalf("made as %+v", got)
	}
	again, err := v.CreateVolume(ctx, spec)
	if err != nil || again.EngineRef != got.EngineRef {
		t.Fatalf("a second create made %s (%v), not %s", again.EngineRef, err, got.EngineRef)
	}
	t.Logf("parked: %+v", got)
	// the shelf is no machine: the machines' view does not list it
	gs, _ := m.Guests(ctx)
	for _, g := range gs {
		if g.ID != a && g.ID != bb && strings.HasPrefix(g.Name, "hangar-shelf") {
			t.Fatalf("the machines' view lists a shelf: %+v", g)
		}
	}

	initA := b.must(t, "lxc-info -n "+va+" -p -H")
	if got, err = v.PlaceVolume(ctx, vol, driver.Place{Guest: a, Mount: "/data", Owner: owner}); err != nil {
		t.Fatal(err)
	}
	if got.Guest != a || got.Mount != "/data" || got.InGuest != "/data" {
		t.Fatalf("on %s as %+v", a, got)
	}
	b.must(t, "pct exec "+va+" -- sh -c 'echo le-hangar > /data/proof'")
	if now := b.must(t, "lxc-info -n "+va+" -p -H"); now != initA {
		t.Fatalf("plugging it restarted the container (init %s → %s)", initA, now)
	}
	_, err = v.PlaceVolume(ctx, vol, driver.Place{Guest: bb, Mount: "/srv", Owner: owner})
	refused(t, err, "running container")
	if err := m.DeleteGuest(ctx, a); err == nil || !strings.Contains(err.Error(), vol) {
		t.Fatalf("a container holding a volume was deleted, or said nothing: %v", err)
	}

	if _, err := m.SetPower(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	if got, err = v.PlaceVolume(ctx, vol, driver.Place{Guest: bb, Mount: "/srv", Owner: owner}); err != nil {
		t.Fatal(err)
	}
	if got.Guest != bb || got.Mount != "/srv" {
		t.Fatalf("on %s as %+v", bb, got)
	}
	if in := b.must(t, "pct exec "+vb+" -- cat /srv/proof"); in != "le-hangar" {
		t.Fatalf("inside %s: %q", vb, in)
	}
	if got, err = v.ResizeVolume(ctx, vol, 2); err != nil || got.SizeGB != 2 {
		t.Fatalf("grown: %+v %v", got, err)
	}
	if size := b.must(t, "pct exec "+vb+" -- df -BG --output=size /srv | tail -1"); strings.TrimSpace(size) != "2G" {
		t.Fatalf("inside, /srv is %s", size)
	}
	_, err = v.ResizeVolume(ctx, vol, 1)
	refused(t, err, "never shrinks")
	_, err = v.SetVolumeBackup(ctx, vol, true)
	refused(t, err, "running container")

	if _, err := m.SetPower(ctx, bb, false); err != nil {
		t.Fatal(err)
	}
	if got, err = v.PlaceVolume(ctx, vol, driver.Place{Owner: owner}); err != nil || got.Guest != "" {
		t.Fatalf("parked: %+v %v", got, err)
	}
	if got, err = v.SetVolumeBackup(ctx, vol, true); err != nil || !got.Backup {
		t.Fatalf("backup: %+v %v", got, err)
	}
	shelf := got.EngineRef[strings.Index(got.EngineRef, "subvol-")+7:]
	shelf = shelf[:strings.Index(shelf, "-")]
	if line := b.config(t, "pct", shelf, got.Device); !strings.Contains(line, "backup=1") || !strings.Contains(line, "size=2G") {
		t.Fatalf("on the shelf: %s", line)
	}
	// back into A, started: the bytes are there, at the path asked
	if got, err = v.PlaceVolume(ctx, vol, driver.Place{Guest: a, Mount: "/data", Owner: owner}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetPower(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	if in := b.must(t, "pct exec "+va+" -- cat /data/proof"); in != "le-hangar" {
		t.Fatalf("inside %s after the round trip: %q", va, in)
	}
	_, err = v.PlaceVolume(ctx, vol, driver.Place{Owner: owner})
	refused(t, err, "running container")
	err = v.DeleteVolume(ctx, vol)
	refused(t, err, "detach it first")
	if _, err := m.SetPower(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	if _, err = v.PlaceVolume(ctx, vol, driver.Place{Owner: owner}); err != nil {
		t.Fatal(err)
	}
	volid := b.must(t, "pvesm list local-zfs | awk '{print $1}' | grep 'subvol-"+shelf+"-' || true")
	if err := v.DeleteVolume(ctx, vol); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Volume(ctx, vol); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("still there: %v", err)
	}
	if out := b.must(t, "pvesm list local-zfs | awk '{print $1}'"); volid != "" && strings.Contains(out, volid) {
		t.Fatalf("its dataset stayed: %s", volid)
	}
	if err := v.DeleteVolume(ctx, vol); err != nil {
		t.Fatalf("a second delete: %v", err)
	}
}

// A VM's volume: made on a running VM (hot-plugged, its serial the id's),
// moved to another running VM — unplugged live, rested on its shelf where its
// options are written back, plugged again — its bytes read by the node at
// each name the engine gives it, its serial read in QEMU's own monitor.
func TestBenchAVMsVolume(t *testing.T) {
	b := onBench(t)
	archive := "local:vztmpl/" + b.must(t, "ls /var/lib/vz/template/cache/ | grep \"^debian-13-standard_.*_$(dpkg --print-architecture)\\.\" | sort -V | tail -1")
	m, v := b.openVolumes(t, archive)
	ctx := context.Background()
	owner := "bench-owner-" + ids.New("x")
	a, bb := ids.New("m"), ids.New("m")
	vol := ids.New("vol")
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := v.PlaceVolume(ctx, vol, driver.Place{Owner: owner}); err != nil && !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("parking %s to clean up: %v", vol, err)
		}
		if err := v.DeleteVolume(ctx, vol); err != nil {
			t.Errorf("cleaning up %s: %v", vol, err)
		}
		for _, id := range []string{a, bb} {
			if err := m.DeleteGuest(ctx, id); err != nil {
				t.Errorf("cleaning up %s: %v", id, err)
			}
		}
		b.dropShelves(t, "qm", owner)
	})
	for _, id := range []string{a, bb} {
		if _, err := m.CreateGuest(ctx, driver.GuestSpec{ID: id, Kind: "vm", Cores: 1, MemoryMB: 1024, Image: "debian-13"}); err != nil {
			t.Fatal(err)
		}
	}
	ga, _ := m.Guest(ctx, a)
	gb, _ := m.Guest(ctx, bb)
	va, vb := vmid(ga), vmid(gb)
	// a PCI hot-unplug needs the guest's kernel to answer: let them boot
	time.Sleep(45 * time.Second)

	got, err := v.CreateVolume(ctx, driver.VolumeSpec{ID: vol, Content: driver.ContentBlock, SizeGB: 1, Backup: false,
		At: driver.Place{Guest: a, Owner: owner}})
	if err != nil {
		t.Fatal(err)
	}
	serial := driver.SerialOf(vol)
	if got.Guest != a || got.Backup || !strings.HasSuffix(got.InGuest, serial) || len(serial) != 20 {
		t.Fatalf("made on %s as %+v", a, got)
	}
	if s := b.serial(t, va, got.Device); s != serial {
		t.Fatalf("the hot-plugged disk shows the serial %q, not %q", s, serial)
	}
	b.marker(t, got.EngineRef, "le-hangar")
	pidA := b.must(t, "cat /var/run/qemu-server/"+va+".pid")

	_, err = v.CreateVolume(ctx, driver.VolumeSpec{ID: ids.New("vol"), Content: driver.ContentFilesystem, SizeGB: 1, At: driver.Place{Guest: a, Mount: "/x"}})
	refused(t, err, "goes on a container")
	if err := m.DeleteGuest(ctx, a); err == nil || !strings.Contains(err.Error(), vol) {
		t.Fatalf("a VM holding a volume was deleted, or said nothing: %v", err)
	}

	// parked from a running VM, its owner's first shelf made on the way: one
	// shelf, the disk's options written back there
	if got, err = v.PlaceVolume(ctx, vol, driver.Place{Owner: owner}); err != nil || got.Guest != "" {
		t.Fatalf("parked from a running VM: %+v %v", got, err)
	}
	if n := b.must(t, "for id in $(qm list | awk 'NR>1 {print $1}'); do qm config $id | grep -c '^description: made by hangar%3A shelf "+shelfKey(owner)+"'; done | grep -c 1"); n != "1" {
		t.Fatalf("the owner has %s VM shelves, not one", n)
	}
	if b.config(t, "qm", va, "unused0") != "" {
		t.Fatalf("A kept an unused disk: %s", b.must(t, "qm config "+va))
	}
	// from the shelf into a running VM, hot-plugged with its options
	if got, err = v.PlaceVolume(ctx, vol, driver.Place{Guest: bb, Owner: owner}); err != nil || got.Guest != bb {
		t.Fatalf("on %s: %+v %v", bb, got, err)
	}
	if s := b.serial(t, vb, got.Device); s != serial {
		t.Fatalf("on %s the disk shows the serial %q, not %q", vb, s, serial)
	}

	// running to running: unplugged live, through the shelf, plugged again
	if got, err = v.PlaceVolume(ctx, vol, driver.Place{Guest: a, Owner: owner}); err != nil {
		t.Fatal(err)
	}
	if got.Guest != a || got.Backup {
		t.Fatalf("on %s as %+v", a, got)
	}
	if s := b.serial(t, va, got.Device); s != serial {
		t.Fatalf("on %s the disk shows the serial %q, not %q: its options did not travel", va, s, serial)
	}
	if line := b.config(t, "qm", va, got.Device); !strings.Contains(line, "backup=0") || !strings.Contains(line, "size=1G") {
		t.Fatalf("on %s: %s", va, line)
	}
	if mk := b.readMarker(t, got.EngineRef, 9); mk != "le-hangar" {
		t.Fatalf("its bytes at %s: %q", got.EngineRef, mk)
	}
	if now := b.must(t, "cat /var/run/qemu-server/"+va+".pid"); now != pidA {
		t.Fatalf("A's QEMU changed from %s to %s", pidA, now)
	}
	if b.config(t, "qm", vb, "unused0") != "" {
		t.Fatalf("B kept an unused disk: %s", b.must(t, "qm config "+vb))
	}
	if got, err = v.ResizeVolume(ctx, vol, 3); err != nil || got.SizeGB != 3 {
		t.Fatalf("grown live: %+v %v", got, err)
	}
	if got, err = v.SetVolumeBackup(ctx, vol, true); err != nil || !got.Backup {
		t.Fatalf("backup on a running VM: %+v %v", got, err)
	}
	if got, err = v.PlaceVolume(ctx, vol, driver.Place{Owner: owner}); err != nil || got.Guest != "" || !got.Backup || got.SizeGB != 3 {
		t.Fatalf("parked: %+v %v", got, err)
	}
	if mk := b.readMarker(t, got.EngineRef, 9); mk != "le-hangar" {
		t.Fatalf("its bytes, parked at %s: %q", got.EngineRef, mk)
	}
	if err := v.DeleteVolume(ctx, vol); err != nil {
		t.Fatal(err)
	}
	if out := b.must(t, "pvesm list local-zfs | awk '{print $1}'"); strings.Contains(out, got.EngineRef) {
		t.Fatalf("its zvol stayed: %s", got.EngineRef)
	}
}
