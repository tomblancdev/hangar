package proxmox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/internal/ids"
)

// openRange opens the bench as one plugin's token would, on a range of
// VMIDs of its own.
func (b *bench) openRange(t *testing.T, tokenFile, vmids string) *Driver {
	t.Helper()
	return b.openWith(t, tokenFile, vmids, nil, nil)
}

// bakeUntil calls Bake until the bake is over (or the deadline), as the
// plugin's reconcile would.
func bakeUntil(t *testing.T, d *Driver, s driver.BakeSpec, within time.Duration) driver.Image {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		im, err := d.Bake(context.Background(), s, false)
		if err != nil {
			t.Fatalf("bake %s: %v", s.ID, err)
		}
		if im.State == driver.ImageAvailable || im.State == driver.ImageFailed {
			return im
		}
		if time.Now().After(deadline) {
			t.Fatalf("bake %s still %s after %s", s.ID, im.State, within)
		}
		time.Sleep(10 * time.Second)
	}
}

// An image's life on a real Proxmox VE, each plugin on its own token: baked
// from Debian's cloud image with the guest agent, a file of the recipe's and
// a package the first boot of its machines no longer installs; two machines
// born from it — their address read at once, the owner's key letting them
// in, the recipe's file there, each with its own machine id and host keys;
// its delete refused while they share its disk; one saved as an image of its
// own and a machine born from that; a recipe that fails, layered on the
// baked image, failed with its own words — and a bake let go mid-way for a
// priority guest, then started over. Without the bench's variables it skips:
//
//	sh tools/bench/bench.sh up && eval "$(sh tools/bench/bench.sh env)" && go test ./driver/proxmox/ -run BenchAnImage -v -timeout 60m
func TestBenchAnImagesLife(t *testing.T) {
	b := onBench(t)
	itok := os.Getenv("HANGAR_BENCH_IMAGES_TOKEN_FILE")
	if itok == "" {
		t.Skip("a bench set up before the images token: sh tools/bench/bench.sh up")
	}
	ctx := context.Background()
	im := b.openRange(t, itok, "11080-11099")
	if !has(im.Capabilities(), driver.FencePool) {
		t.Fatalf("the images token is not fenced: %s", im.FenceReport())
	}
	gm := b.openRange(t, b.token, "11080-11099")
	dir := t.TempDir()
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "bench-img", "-f", filepath.Join(dir, "k")).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	pub, _ := os.ReadFile(filepath.Join(dir, "k.pub"))
	priv, _ := os.ReadFile(filepath.Join(dir, "k"))
	b.must(t, "umask 077 && cat > /root/bench-img-key", string(priv))
	inside := func(ip, cmd string) string {
		t.Helper()
		return b.must(t, "ssh -i /root/bench-img-key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=10 debian@"+ip+" '"+cmd+"'")
	}
	img, saved := ids.New("img"), ids.New("img")
	var machines []string
	t.Cleanup(func() {
		for _, m := range machines {
			if err := gm.DeleteGuest(context.Background(), m); err != nil {
				t.Errorf("cleaning up %s: %v", m, err)
			}
		}
		for _, i := range []string{saved, img} {
			if err := im.DeleteImage(context.Background(), i); err != nil {
				t.Errorf("cleaning up %s: %v", i, err)
			}
		}
	})

	// the bake: from the cloud image on the import storage
	spec := driver.BakeSpec{
		ID: img, Attempt: 1, Kind: "vm", Base: "local:import/debian-13-genericcloud-amd64.qcow2", DiskGB: 4, Cores: 2, MemoryMB: 1024,
		Timeout: 25 * time.Minute, Tags: map[string]string{"class": "spot", "admitted": "1024"},
		UserData: []byte("#cloud-config\npackage_update: true\npackages: [qemu-guest-agent, jq]\n" +
			"write_files:\n  - {path: /etc/hangar-baked, content: \"baked by its recipe\\n\"}\n"),
	}
	started := time.Now()
	first, err := im.Bake(ctx, spec, false)
	if err != nil || first.State != driver.ImagePending {
		t.Fatalf("a bake begins pending: %+v %v", first, err)
	}
	builder := vmid(driver.Guest{EngineRef: first.EngineRef})
	if cfg := b.must(t, "qm config "+builder); !strings.Contains(cfg, "class.spot") || !strings.Contains(cfg, "hangar-id."+img) {
		t.Fatalf("the builder carries its id and borrows its room (class spot):\n%s", cfg)
	}
	again, err := im.Bake(ctx, spec, false)
	if err != nil || again.EngineRef != first.EngineRef {
		t.Fatalf("a second call finds the same builder: %+v %v", again, err)
	}
	done := bakeUntil(t, im, spec, 25*time.Minute)
	if done.State != driver.ImageAvailable || done.Ref != img || done.SizeGB != 4 {
		t.Fatalf("baked: %+v", done)
	}
	t.Logf("baked in %s: %+v", time.Since(started).Round(time.Second), done)
	cfg := b.must(t, "qm config "+builder)
	if !strings.Contains(cfg, "template: 1") || strings.Contains(cfg, "ide2") || strings.Contains(cfg, "tags:") || !strings.Contains(cfg, "hangar bake 1 done") {
		t.Fatalf("the image is a template, its seed disc off, no tag its clones would copy:\n%s", cfg)
	}
	if out := b.must(t, "pvesm list hangar-seeds"); strings.Contains(out, img) {
		t.Fatalf("the builder's seed disc stayed: %s", out)
	}

	// two machines born from it: usable at birth, each its own
	type born struct{ id, ip string }
	var two []born
	for i := range 2 {
		id := ids.New("m")
		machines = append(machines, id)
		t0 := time.Now()
		g, err := gm.CreateGuest(ctx, driver.GuestSpec{ID: id, Kind: "vm", Name: "born-" + string(rune('a'+i)), Cores: 1, MemoryMB: 1024, DiskGB: 5,
			Image: img, SSHKeys: []string{strings.TrimSpace(string(pub))}, Tags: map[string]string{"class": "guaranteed"}})
		if err != nil {
			t.Fatal(err)
		}
		for len(g.Addresses) == 0 {
			if time.Since(t0) > 5*time.Minute {
				t.Fatalf("%s reports no address 5 minutes after its birth: its image's guest agent?", id)
			}
			time.Sleep(5 * time.Second)
			if g, err = gm.Guest(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("%s answered with %v %s after its create began", id, g.Addresses, time.Since(t0).Round(time.Second))
		two = append(two, born{id, g.Addresses[0]})
	}
	for i, m := range two {
		out := inside(m.ip, "hostname; cat /etc/hangar-baked; command -v jq; cloud-init status; lsblk -bdno SIZE /dev/sda")
		want := "born-" + string(rune('a'+i)) + "\nbaked by its recipe\n/usr/bin/jq\nstatus: done\n5368709120"
		if out != want {
			t.Fatalf("inside %s: %q, want %q (its name, the recipe's file and package, its own first boot done, a 5 GiB disk)", m.id, out, want)
		}
	}
	mid := func(ip string) string { return inside(ip, "cat /etc/machine-id") }
	if mid(two[0].ip) == mid(two[1].ip) {
		t.Fatal("two machines born from one image share their machine id")
	}
	key := func(ip string) string { return inside(ip, "sudo cat /etc/ssh/ssh_host_ed25519_key.pub") }
	if key(two[0].ip) == key(two[1].ip) {
		t.Fatal("two machines born from one image share their host key")
	}

	// the image is not deleted while they share its disk
	err = im.DeleteImage(ctx, img)
	if !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), two[0].id) || !strings.Contains(err.Error(), two[1].id) {
		t.Fatalf("a delete under linked clones is refused, naming them: %v", err)
	}
	if out := b.must(t, "qm config "+builder); !strings.Contains(out, "template: 1") {
		t.Fatalf("the refused delete took something: %s", out)
	}

	// a save: refused while it runs; then the machine's own disk, a machine born from it
	inside(two[0].ip, "echo mine | sudo tee /etc/hangar-mine >/dev/null; sync")
	if _, err := im.SaveImage(ctx, saved, two[0].id); !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), "runs") {
		t.Fatalf("a running machine is not saved: %v", err)
	}
	if _, err := gm.SetPower(ctx, two[0].id, false); err != nil {
		t.Fatal(err)
	}
	s, err := im.SaveImage(ctx, saved, two[0].id)
	if err != nil || s.State != driver.ImageAvailable || s.Ref != saved || s.SizeGB != 5 {
		t.Fatalf("saved: %+v %v", s, err)
	}
	if again, err := im.SaveImage(ctx, saved, two[0].id); err != nil || again.EngineRef != s.EngineRef {
		t.Fatalf("a second save finds the first: %+v %v", again, err)
	}
	third := ids.New("m")
	machines = append(machines, third)
	g, err := gm.CreateGuest(ctx, driver.GuestSpec{ID: third, Kind: "vm", Name: "from-saved", Cores: 1, MemoryMB: 1024, DiskGB: 5,
		Image: saved, SSHKeys: []string{strings.TrimSpace(string(pub))}, Tags: map[string]string{"class": "guaranteed"}})
	if err != nil {
		t.Fatal(err)
	}
	for t0 := time.Now(); len(g.Addresses) == 0; {
		if time.Since(t0) > 5*time.Minute {
			t.Fatal("the machine born from the saved image reports no address")
		}
		time.Sleep(5 * time.Second)
		if g, err = gm.Guest(ctx, third); err != nil {
			t.Fatal(err)
		}
	}
	if out := inside(g.Addresses[0], "hostname; cat /etc/hangar-mine"); out != "from-saved\nmine" {
		t.Fatalf("born from the saved image: %q", out)
	}

	// a recipe that fails, from the baked image (its guest agent there); let
	// go mid-way for a priority guest, started over, then failed in its words
	bad := ids.New("img")
	t.Cleanup(func() {
		if err := im.DeleteImage(context.Background(), bad); err != nil {
			t.Errorf("cleaning up %s: %v", bad, err)
		}
	})
	fail := driver.BakeSpec{ID: bad, Attempt: 1, Kind: "vm", Base: img, DiskGB: 4, Cores: 1, MemoryMB: 1024, Timeout: 15 * time.Minute,
		Tags:     map[string]string{"class": "spot", "admitted": "1024"},
		UserData: []byte("#cloud-config\nruncmd:\n  - [sh, -c, 'echo the recipe broke on purpose >&2; exit 3']\n")}
	p, err := im.Bake(ctx, fail, false)
	if err != nil || p.State != driver.ImagePending {
		t.Fatalf("%+v %v", p, err)
	}
	w, err := im.Bake(ctx, fail, true)
	if err != nil || w.State != driver.ImageWaiting {
		t.Fatalf("held, the builder is let go: %+v %v", w, err)
	}
	if out, err := b.ssh(t, "qm status "+vmid(driver.Guest{EngineRef: p.EngineRef})); err == nil {
		t.Fatalf("the let-go builder is still there: %s", out)
	}
	if w, err = im.Bake(ctx, fail, true); err != nil || w.State != driver.ImageWaiting {
		t.Fatalf("still held, it waits with nothing made: %+v %v", w, err)
	}
	f := bakeUntil(t, im, fail, 15*time.Minute)
	if f.State != driver.ImageFailed || !strings.Contains(f.Detail, "reported errors") || !strings.Contains(f.Detail, "the recipe broke on purpose") {
		t.Fatalf("a failed recipe comes back in its own words: %+v", f)
	}
	t.Logf("failed as it should: %s", f.Detail)
	if _, _, err := im.findImage(ctx, bad); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("a failed bake's builder is gone: %v", err)
	}
}
