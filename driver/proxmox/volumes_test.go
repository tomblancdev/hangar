package proxmox

import (
	"slices"
	"testing"

	"github.com/tomblancdev/hangar/driver"
)

func holderOf(vmid int, typ, tags string, cfg map[string]any) holder {
	h := holder{r: resource{VMID: vmid, Node: "n", Type: typ, Tags: tags, Pool: "p"}, cfg: cfg}
	h.keep, h.lines = parseDesc(str(cfg["description"]))
	return h
}

// A description keeps every line that is not ours as it was, ours parsed out
// and written back the same.
func TestTheDescriptionsLines(t *testing.T) {
	desc := "made by hangar: m-1\nsomeone's note\n\nhangar volume vol-1 scsi1 local-zfs:vm-7-disk-1\nhangar volume vol-2 scsi2 -"
	keep, lines := parseDesc(desc)
	if !slices.Equal(keep, []string{"made by hangar: m-1", "someone's note", ""}) {
		t.Fatalf("kept %q", keep)
	}
	if !slices.Equal(lines, []volLine{{"vol-1", "scsi1", "local-zfs:vm-7-disk-1"}, {"vol-2", "scsi2", "-"}}) {
		t.Fatalf("lines %v", lines)
	}
	if got := formatDesc(keep, lines); got != desc {
		t.Fatalf("written back as %q", got)
	}
}

// Where a volume is, whatever a cut left: its volid found at any key of the
// guest that recorded it (an unplug makes it unusedN), else the key a line
// still waiting for its volid names — unless another line claims that disk.
func TestAVolumeIsFoundWhereverACutLeftIt(t *testing.T) {
	a := holderOf(7, "qemu", "hangar-id.m-a", map[string]any{
		"scsi0":       "local-zfs:vm-7-disk-0,size=8G",
		"unused0":     "local-zfs:vm-7-disk-1",
		"description": "made by hangar: m-a\nhangar volume vol-1 scsi1 local-zfs:vm-7-disk-1",
	})
	shelf := holderOf(9, "qemu", "", map[string]any{
		"scsi1":       "local-zfs:vm-9-disk-0,backup=0,serial=vol2,size=1G",
		"scsi2":       "local-zfs:vm-9-disk-1,size=1G",
		"description": "made by hangar: shelf abc\nhangar volume vol-2 scsi1 -\nhangar volume vol-3 scsi2 -\nhangar volume vol-4 scsi2 local-zfs:vm-9-disk-1",
	})
	// a move cut after the engine moved it, before the source's line went
	b := holderOf(8, "qemu", "hangar-id.m-b", map[string]any{
		"scsi3":       "local-zfs:vm-8-disk-2,size=1G",
		"description": "hangar volume vol-5 scsi3 -",
	})
	stale := holderOf(6, "qemu", "hangar-id.m-c", map[string]any{
		"description": "hangar volume vol-5 scsi1 local-zfs:vm-6-disk-1",
	})
	hs := []holder{stale, a, shelf, b}
	for _, c := range []struct{ id, key string }{
		{"vol-1", "unused0"}, // unplugged live: found by its volid
		{"vol-2", "scsi1"},   // its line waits for its volid
		{"vol-4", "scsi2"},   // its volid; vol-3's line names the same key, but the disk is claimed
		{"vol-5", "scsi3"},   // on its target; the source's line names a disk that left
	} {
		sp, ok := locate(hs, c.id)
		if !ok || sp.key != c.key {
			t.Errorf("%s: found at %q (%v), want %s", c.id, sp.key, ok, c.key)
		}
	}
	if _, ok := locate(hs, "vol-3"); ok {
		t.Error("vol-3 was found on a disk another volume's line claims")
	}
	if got := a.holds(); !slices.Equal(got, []string{"vol-1"}) {
		t.Errorf("m-a holds %v (an unplugged volume still keeps its machine)", got)
	}
	if shelf.machine() != "" || a.machine() != "m-a" {
		t.Errorf("a shelf reads as a machine, or a machine does not")
	}
}

func TestFreeKeysAndOptions(t *testing.T) {
	h := holderOf(7, "qemu", "hangar-id.m-a", map[string]any{"scsi0": "x", "scsi1": "y", "description": "hangar volume vol-9 scsi2 -"})
	if k, _ := freeKey(h); k != "scsi3" {
		t.Errorf("a VM's free key: %s (scsi0 is its own disk, scsi1 taken, scsi2 promised)", k)
	}
	c := holderOf(8, "lxc", "hangar-id.m-b", map[string]any{"rootfs": "r", "mp0": "m"})
	if k, _ := freeKey(c); k != "mp1" {
		t.Errorf("a container's free key: %s", k)
	}
	line := "local-zfs:vm-7-disk-1,backup=1,size=2G"
	if got := withOpts(line, map[string]string{"backup": "0", "serial": "vol0123"}); got != "local-zfs:vm-7-disk-1,size=2G,backup=0,serial=vol0123" {
		t.Errorf("options written as %q", got)
	}
	if v, _ := optOf("local-zfs:subvol-8-disk-1,mp=/data,backup=1", "mp"); v != "/data" {
		t.Errorf("mp read as %q", v)
	}
	if got := driver.SerialOf("vol-0123456789abcdef0"); got != "vol0123456789abcdef0" {
		t.Errorf("serial %q", got)
	}
}
