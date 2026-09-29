package proxmox

// The volumes facet.
//
// Proxmox VE keeps no disk without a guest: a disk is a line of a guest's
// config, named after that guest (vm-<vmid>-disk-N, subvol-<vmid>-disk-N),
// and renamed when it moves to another (target-vmid, in qemu-server's and
// pve-container's source). So here:
//
//   - A volume is always a line of some guest's config: a machine's, or a
//     SHELF's — a guest of the pool the driver makes for one owner and one
//     kind, never starts, and never tags hangar-id (it is no machine, and no
//     hook or plugin takes it for one): a VM with no disk of its own for
//     block volumes, a container from the zone's shelf_archive for
//     filesystem ones. A stopped VM's disks and a stopped container's mount
//     points are backed up by vzdump by their own flag, so a parked volume
//     keeps its backup.
//   - Which line is which volume is written in that guest's description, one
//     line per volume: "hangar volume <id> <key> <volid>", the volid "-"
//     while a move that will name it is on its way. A move writes the line
//     on its target BEFORE it moves and takes the source's off AFTER, so a
//     volume is found wherever a cut left it (locate).
//   - A block volume shows its guest the serial driver.SerialOf(id), AWS's
//     own form: /dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_vol0123….
//   - A disk moves with its options only from a STOPPED guest (read on a
//     throwaway, then in the source: a running VM lets go of a disk only by
//     unplugging it, which makes it "unusedN" and drops its options; a
//     running container lets go of none, and an unused volume of a container
//     reaches another only as unused). So a volume leaving a running VM rests
//     on its shelf, where its options are written back, and goes on from
//     there; one leaving a running container is refused, in words.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/tomblancdev/hangar/driver"
)

// shelfMarker begins a shelf's description, followed by its owner's key.
const shelfMarker = "made by hangar: shelf "

// volumeWord begins each of a guest's description lines that says which of
// its disks is which volume.
const volumeWord = "hangar volume"

// diskKey: a config key that holds a volume.
var diskKey = regexp.MustCompile(`^(scsi|virtio|sata|ide|mp|unused)\d+$`)

// shelfKey is the name of an owner's shelves: a hash, so that no subject's
// spelling reaches the engine.
func shelfKey(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:6])
}

func typeFor(content string) string {
	if content == driver.ContentFilesystem {
		return "lxc"
	}
	return "qemu"
}

func contentOf(typ string) string {
	if typ == "lxc" {
		return driver.ContentFilesystem
	}
	return driver.ContentBlock
}

// ---- A guest's description --------------------------------------------------

// volLine is one "hangar volume <id> <key> <volid>" line.
type volLine struct{ ID, Key, VolID string }

// parseDesc splits a description into the lines that say which disk is which
// volume and every other line, kept as it was.
func parseDesc(desc string) (keep []string, lines []volLine) {
	for _, l := range strings.Split(strings.TrimRight(desc, "\n"), "\n") {
		f := strings.Fields(l)
		if len(f) == 5 && f[0]+" "+f[1] == volumeWord {
			lines = append(lines, volLine{ID: f[2], Key: f[3], VolID: f[4]})
			continue
		}
		if l != "" || len(keep) > 0 {
			keep = append(keep, l)
		}
	}
	return keep, lines
}

func formatDesc(keep []string, lines []volLine) string {
	out := slices.Clone(keep)
	for _, l := range lines {
		out = append(out, fmt.Sprintf("%s %s %s %s", volumeWord, l.ID, l.Key, l.VolID))
	}
	return strings.Join(out, "\n")
}

// volidOf is a disk line's volume: its first field
// ("local-zfs:vm-1-disk-0,size=8G", or an unused disk's bare volid).
func volidOf(line string) string {
	v, _, _ := strings.Cut(line, ",")
	if strings.Contains(v, "=") {
		return "" // "file=…" is not how Proxmox writes it back; nothing of ours
	}
	return v
}

// optOf reads one option of a disk line.
func optOf(line, name string) (string, bool) {
	for _, part := range strings.Split(line, ",")[1:] {
		if k, v, ok := strings.Cut(part, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// withOpts writes a disk line back with some options set (and others kept).
func withOpts(line string, set map[string]string) string {
	parts := strings.Split(line, ",")
	out := []string{parts[0]}
	for _, part := range parts[1:] {
		k, _, _ := strings.Cut(part, "=")
		if _, over := set[k]; !over {
			out = append(out, part)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(set)) {
		out = append(out, k+"="+set[k])
	}
	return strings.Join(out, ",")
}

func flag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ---- The guests that hold volumes -------------------------------------------

// holder is one guest of the pool as the volumes facet reads it.
type holder struct {
	r     resource
	cfg   map[string]any
	keep  []string
	lines []volLine
}

func (h holder) machine() string { return guestID(h.r.Tags) }

func (h holder) isShelf(owner string) bool {
	return h.machine() == "" && len(h.keep) > 0 && h.keep[0] == shelfMarker+shelfKey(owner)
}

// at is where a line's volume is on this guest: the disk that carries its
// volid (it may have moved from key to key, a detach makes it unused), or —
// while the volid is not yet known — the disk at its key that no other line
// claims.
func (h holder) at(l volLine) (key string, byVolid bool) {
	if l.VolID != "-" {
		for k, v := range h.cfg {
			if diskKey.MatchString(k) && volidOf(str(v)) == l.VolID {
				return k, true
			}
		}
		return "", false
	}
	v := str(h.cfg[l.Key])
	if v == "" || strings.Contains(v, "media=cdrom") || volidOf(v) == "" {
		return "", false
	}
	for _, o := range h.lines {
		if o.ID != l.ID && o.VolID == volidOf(v) {
			return "", false
		}
	}
	return l.Key, false
}

// holds lists the volumes a guest holds now.
func (h holder) holds() []string {
	var out []string
	for _, l := range h.lines {
		if k, _ := h.at(l); k != "" && !slices.Contains(out, l.ID) {
			out = append(out, l.ID)
		}
	}
	slices.Sort(out)
	return out
}

func (d *Driver) read1(ctx context.Context, r resource) (holder, error) {
	var cfg map[string]any
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
		return holder{}, err
	}
	h := holder{r: r, cfg: cfg}
	h.keep, h.lines = parseDesc(str(cfg["description"]))
	return h, nil
}

// survey reads every guest of the pool (machines and shelves) once.
func (d *Driver) survey(ctx context.Context) ([]holder, error) {
	rs, err := d.resources(ctx)
	if err != nil {
		return nil, err
	}
	var out []holder
	for _, r := range rs {
		if r.Pool != d.pool || r.Template != 0 {
			continue
		}
		h, err := d.read1(ctx, r)
		if err != nil {
			var ae *apiError
			if errors.As(err, &ae) && strings.Contains(ae.Message, "does not exist") {
				continue // gone between the list and the read
			}
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// spot is where a volume is: its guest and its key there.
type spot struct {
	h   holder
	key string
}

// locate finds a volume among the guests read: a disk carrying the volid its
// line recorded first, the disk a line still waiting for its volid names
// after.
func locate(hs []holder, id string) (spot, bool) {
	var byKey *spot
	for _, h := range hs {
		for _, l := range h.lines {
			if l.ID != id {
				continue
			}
			if k, byVolid := h.at(l); k != "" {
				if byVolid {
					return spot{h, k}, true
				}
				if byKey == nil {
					byKey = &spot{h, k}
				}
			}
		}
	}
	if byKey != nil {
		return *byKey, true
	}
	return spot{}, false
}

func machineOf(hs []holder, id string) (holder, error) {
	for _, h := range hs {
		if h.machine() == id {
			return h, nil
		}
	}
	return holder{}, fmt.Errorf("%w: no guest %s here", driver.ErrNotFound, id)
}

// setConfig writes some of a guest's config keys and deletes others.
func (d *Driver) setConfig(ctx context.Context, r resource, set url.Values, del ...string) error {
	p := url.Values{}
	for k, v := range set {
		p[k] = v
	}
	if len(del) > 0 {
		p.Set("delete", strings.Join(del, ","))
	}
	if r.Type == "lxc" {
		return d.c.call(ctx, http.MethodPut, r.path()+"/config", p, nil)
	}
	return d.c.run(ctx, http.MethodPost, r.path()+"/config", p)
}

// record writes a guest's description with one volume's line as given: any
// other line of that volume, and any line of another naming the same key,
// taken off (a key is chosen free, so such a line is one a cut left).
func (d *Driver) record(ctx context.Context, h *holder, l volLine, also url.Values) error {
	lines := []volLine{}
	for _, o := range h.lines {
		if o.ID != l.ID && o.Key != l.Key {
			lines = append(lines, o)
		}
	}
	h.lines = append(lines, l)
	set := url.Values{"description": {formatDesc(h.keep, h.lines)}}
	for k, v := range also {
		set[k] = v
	}
	return d.setConfig(ctx, h.r, set)
}

// forget takes a volume's lines off a guest's description.
func (d *Driver) forget(ctx context.Context, h *holder, id string) error {
	lines := []volLine{}
	for _, o := range h.lines {
		if o.ID != id {
			lines = append(lines, o)
		}
	}
	if len(lines) == len(h.lines) {
		return nil
	}
	h.lines = lines
	desc := formatDesc(h.keep, h.lines)
	if desc == "" {
		return d.setConfig(ctx, h.r, nil, "description")
	}
	return d.setConfig(ctx, h.r, url.Values{"description": {desc}})
}

// tidy takes a volume's stale lines off every guest but the one it is on.
func (d *Driver) tidy(ctx context.Context, hs []holder, id string, at spot) error {
	for i := range hs {
		if hs[i].r.VMID == at.h.r.VMID {
			continue
		}
		if err := d.forget(ctx, &hs[i], id); err != nil {
			return err
		}
	}
	return nil
}

// freeKey is the first key of a guest that no disk and no line takes: scsi1
// onward on a VM (scsi0 is a machine's own disk), mp0 onward on a container.
func freeKey(h holder) (string, error) {
	bus, from, n := "scsi", 1, 31
	if h.r.Type == "lxc" {
		bus, from, n = "mp", 0, 256
	}
	for i := from; i < n; i++ {
		k := bus + strconv.Itoa(i)
		if _, used := h.cfg[k]; used {
			continue
		}
		if slices.ContainsFunc(h.lines, func(l volLine) bool { return l.Key == k }) {
			continue
		}
		return k, nil
	}
	return "", fmt.Errorf("%w: guest %d has no free %s key left", driver.ErrRefused, h.r.VMID, bus)
}

// freeUnused is the first unusedN key of a guest nothing takes.
func freeUnused(h holder) (string, error) {
	for i := range 256 {
		k := "unused" + strconv.Itoa(i)
		if _, used := h.cfg[k]; !used && !slices.ContainsFunc(h.lines, func(l volLine) bool { return l.Key == k }) {
			return k, nil
		}
	}
	return "", fmt.Errorf("%w: guest %d has no free unused key left", driver.ErrRefused, h.r.VMID)
}

// ---- Shelves ----------------------------------------------------------------

func (d *Driver) CanPark(content string) error {
	if content == driver.ContentFilesystem && d.shelfArchive == "" {
		return fmt.Errorf("%w: zone %s keeps no container volume off a container: its option shelf_archive names no archive to make a shelf from",
			driver.ErrRefused, d.zone)
	}
	return nil
}

// shelf finds the owner's shelf of one kind among the guests read, or makes
// it: a VM with no disk, or a container from the zone's shelf archive — never
// started, never tagged hangar-id.
func (d *Driver) shelf(ctx context.Context, hs []holder, owner, typ string) (holder, error) {
	find := func(hs []holder) (holder, bool) {
		for _, h := range hs {
			if h.r.Type == typ && h.isShelf(owner) {
				return h, true
			}
		}
		return holder{}, false
	}
	if h, ok := find(hs); ok {
		return h, nil
	}
	if owner == "" {
		return holder{}, fmt.Errorf("%w: a volume parks on its owner's shelf; no owner given", driver.ErrRefused)
	}
	if err := d.CanPark(contentOf(typ)); err != nil {
		return holder{}, err
	}
	name := "hangar-shelf-" + shelfKey(owner)
	desc := shelfMarker + shelfKey(owner)
	d.mu.Lock()
	err := d.withFreeVMID(ctx, func(vmid int) error {
		if typ == "lxc" {
			return d.c.run(ctx, http.MethodPost, "/nodes/"+url.PathEscape(d.node)+"/lxc", url.Values{
				// no hostname: pve-container counts it as network (VM.Config.Network),
				// and a shelf has none — its description says what it is
				"vmid": {strconv.Itoa(vmid)}, "ostemplate": {d.shelfArchive}, "pool": {d.pool},
				"description": {desc}, "rootfs": {d.storage + ":1"}, "unprivileged": {"1"},
			})
		}
		return d.c.run(ctx, http.MethodPost, "/nodes/"+url.PathEscape(d.node)+"/qemu", url.Values{
			"vmid": {strconv.Itoa(vmid)}, "name": {name}, "pool": {d.pool}, "description": {desc},
		})
	})
	d.mu.Unlock()
	if err != nil {
		return holder{}, err
	}
	again, err := d.survey(ctx)
	if err != nil {
		return holder{}, err
	}
	if h, ok := find(again); ok {
		return h, nil
	}
	return holder{}, fmt.Errorf("%w: the shelf %s was made and cannot be found", driver.ErrRefused, name)
}

// withFreeVMID makes a guest at the lowest free id of the range, again at the
// next when another maker took it first (the machines and the volumes
// plugins each run their own driver: two processes pick from one range).
func (d *Driver) withFreeVMID(ctx context.Context, make func(vmid int) error) error {
	var err error
	for range 5 {
		var vmid int
		if vmid, err = d.freeVMID(ctx); err != nil {
			return err
		}
		if err = make(vmid); err == nil || !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}
	return err
}

// ---- Reading a volume -------------------------------------------------------

func (d *Driver) volumeOf(ctx context.Context, sp spot, id string) driver.Volume {
	line := str(sp.h.cfg[sp.key])
	v := driver.Volume{ID: id, EngineRef: volidOf(line), Content: contentOf(sp.h.r.Type), Device: sp.key, Node: sp.h.r.Node,
		SizeGB: sizeGB(line)}
	if v.SizeGB == 0 {
		v.SizeGB = d.storedGB(ctx, sp.h.r.Node, v.EngineRef)
	}
	unused := strings.HasPrefix(sp.key, "unused")
	b, set := optOf(line, "backup")
	if sp.h.r.Type == "lxc" {
		v.Backup = b == "1" // a mount point is left out of backups unless it says so
		v.Mount, _ = optOf(line, "mp")
	} else {
		v.Backup = !unused && (!set || b == "1") // a VM's disk is backed up unless it says not
	}
	if m := sp.h.machine(); m != "" {
		// still on its machine when a cut left it unplugged there: a place,
		// asked again, finishes the move (and the machine is not deleted
		// with it meanwhile)
		v.Guest = m
		if !unused {
			v.InGuest = v.Mount
			if v.Content == driver.ContentBlock {
				v.InGuest = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_" + driver.SerialOf(id)
			}
		}
	}
	return v
}

// storedGB reads a volume's size from its storage, for a line that does not
// say it (a disk moved from "unused" carries no size= until it is written).
func (d *Driver) storedGB(ctx context.Context, node, volid string) int {
	store, _, ok := strings.Cut(volid, ":")
	if !ok {
		return 0
	}
	var info struct {
		Size int64 `json:"size"`
	}
	path := "/nodes/" + url.PathEscape(node) + "/storage/" + url.PathEscape(store) + "/content/" + url.PathEscape(volid)
	if d.c.call(ctx, http.MethodGet, path, nil, &info) != nil {
		return 0
	}
	return int((info.Size + (1<<30 - 1)) >> 30)
}

// ---- The facet --------------------------------------------------------------

func (d *Driver) Volume(ctx context.Context, id string) (driver.Volume, error) {
	hs, err := d.survey(ctx)
	if err != nil {
		return driver.Volume{}, d.engine(err)
	}
	sp, ok := locate(hs, id)
	if !ok {
		return driver.Volume{}, driver.ErrNotFound
	}
	return d.volumeOf(ctx, sp, id), nil
}

func (d *Driver) CreateVolume(ctx context.Context, s driver.VolumeSpec) (driver.Volume, error) {
	d.vmu.Lock()
	defer d.vmu.Unlock()
	if s.Content != driver.ContentBlock && s.Content != driver.ContentFilesystem {
		return driver.Volume{}, fmt.Errorf("%w: no content %q", driver.ErrRefused, s.Content)
	}
	if s.SizeGB < 1 {
		return driver.Volume{}, fmt.Errorf("%w: a volume of %d GB", driver.ErrRefused, s.SizeGB)
	}
	hs, err := d.survey(ctx)
	if err != nil {
		return driver.Volume{}, d.engine(err)
	}
	if sp, ok := locate(hs, s.ID); ok { // a retry: the first call made it
		return d.volumeOf(ctx, sp, s.ID), nil
	}
	typ := typeFor(s.Content)
	var h holder
	if s.At.Guest != "" {
		if h, err = machineOf(hs, s.At.Guest); err != nil {
			return driver.Volume{}, err
		}
		if err := sameKind(h, typ, s.At.Guest); err != nil {
			return driver.Volume{}, err
		}
	} else if h, err = d.shelf(ctx, hs, s.At.Owner, typ); err != nil {
		return driver.Volume{}, d.engine(err)
	}
	key, err := freeKey(h)
	if err != nil {
		return driver.Volume{}, err
	}
	line := fmt.Sprintf("%s:%d,backup=%s", d.storage, s.SizeGB, flag(s.Backup))
	if typ == "lxc" {
		mount := s.At.Mount
		if mount == "" {
			mount = "/mnt/" + s.ID // a parked one's path is nobody's
		}
		line += ",mp=" + mount
	} else {
		line += ",serial=" + driver.SerialOf(s.ID)
	}
	// the disk and the line that says whose it is, in one write
	if err := d.record(ctx, &h, volLine{ID: s.ID, Key: key, VolID: "-"}, url.Values{key: {line}}); err != nil {
		return driver.Volume{}, d.engine(err)
	}
	return d.settle(ctx, s.ID)
}

// settle reads a volume where it is after a change, writes its volid on its
// line (so a later move within its guest finds it) and takes the lines a cut
// left elsewhere.
func (d *Driver) settle(ctx context.Context, id string) (driver.Volume, error) {
	hs, err := d.survey(ctx)
	if err != nil {
		return driver.Volume{}, d.engine(err)
	}
	sp, ok := locate(hs, id)
	if !ok {
		return driver.Volume{}, fmt.Errorf("%w: volume %s was written and cannot be found", driver.ErrNotFound, id)
	}
	if volid := volidOf(str(sp.h.cfg[sp.key])); volid != "" {
		if !slices.Contains(sp.h.lines, volLine{ID: id, Key: sp.key, VolID: volid}) {
			if err := d.record(ctx, &sp.h, volLine{ID: id, Key: sp.key, VolID: volid}, nil); err != nil {
				return driver.Volume{}, d.engine(err)
			}
		}
	}
	if err := d.tidy(ctx, hs, id, sp); err != nil {
		return driver.Volume{}, d.engine(err)
	}
	return d.volumeOf(ctx, sp, id), nil
}

func sameKind(h holder, typ, guest string) error {
	if h.r.Type == typ {
		return nil
	}
	word := map[string]string{"qemu": "VM", "lxc": "container"}
	return fmt.Errorf("%w: a %s volume goes on a %s, and %s is a %s", driver.ErrRefused,
		contentOf(typ), word[typ], guest, word[h.r.Type])
}

func (d *Driver) PlaceVolume(ctx context.Context, id string, at driver.Place) (driver.Volume, error) {
	d.vmu.Lock()
	defer d.vmu.Unlock()
	hs, err := d.survey(ctx)
	if err != nil {
		return driver.Volume{}, d.engine(err)
	}
	sp, ok := locate(hs, id)
	if !ok {
		return driver.Volume{}, driver.ErrNotFound
	}
	typ := sp.h.r.Type
	var target holder
	if at.Guest != "" {
		if target, err = machineOf(hs, at.Guest); err != nil {
			return driver.Volume{}, err
		}
		if err := sameKind(target, typ, at.Guest); err != nil {
			return driver.Volume{}, err
		}
	} else if sp.h.machine() == "" && !strings.HasPrefix(sp.key, "unused") {
		return d.settle(ctx, id) // parked already, on the shelf it was made on
	} else if target, err = d.shelf(ctx, hs, at.Owner, typ); err != nil {
		return driver.Volume{}, d.engine(err)
	}

	if target.r.VMID == sp.h.r.VMID && !strings.HasPrefix(sp.key, "unused") {
		if typ == "lxc" && at.Mount != "" {
			if m, _ := optOf(str(sp.h.cfg[sp.key]), "mp"); m != at.Mount {
				if err := d.remount(ctx, sp, at.Mount); err != nil {
					return driver.Volume{}, err
				}
			}
		}
		return d.settle(ctx, id)
	}

	opts := str(sp.h.cfg[sp.key]) // its options, before a move could drop them
	on, err := d.running(ctx, sp.h.r)
	if err != nil {
		return driver.Volume{}, d.engine(err)
	}
	if on && typ == "lxc" {
		return driver.Volume{}, fmt.Errorf("%w: %s is a running container: it lets go of a volume only once stopped", driver.ErrRefused, sp.h.machine())
	}
	if on && !strings.HasPrefix(sp.key, "unused") {
		// a running VM lets go of a disk by unplugging it: it becomes unusedN
		// on the same VM, found again by its volid
		volid := volidOf(opts)
		if err := d.record(ctx, &sp.h, volLine{ID: id, Key: sp.key, VolID: volid}, nil); err != nil {
			return driver.Volume{}, d.engine(err)
		}
		if err := d.setConfig(ctx, sp.h.r, nil, sp.key); err != nil {
			return driver.Volume{}, d.engine(err)
		}
		if sp, err = d.refind(ctx, sp.h.r, id); err != nil {
			return driver.Volume{}, err
		}
	}
	if strings.HasPrefix(sp.key, "unused") {
		// what left a running VM rests on its shelf, its options written back
		// there, before it goes on — parked, the shelf is where it goes (and
		// may have been made just now, after the guests were read)
		shelf := target
		if at.Guest != "" {
			if shelf, err = d.shelf(ctx, hs, at.Owner, typ); err != nil {
				return driver.Volume{}, d.engine(err)
			}
		}
		if shelf.r.VMID != sp.h.r.VMID {
			if sp, err = d.move(ctx, sp, shelf, id); err != nil {
				return driver.Volume{}, err
			}
		}
		if strings.HasPrefix(sp.key, "unused") { // a container's lands unused
			if sp, err = d.plugBack(ctx, sp, id, opts); err != nil {
				return driver.Volume{}, err
			}
		}
		if err := d.restore(ctx, sp, id, opts); err != nil {
			return driver.Volume{}, err
		}
		if sp, err = d.refind(ctx, sp.h.r, id); err != nil {
			return driver.Volume{}, err
		}
		if target.r.VMID == sp.h.r.VMID {
			return d.settle(ctx, id)
		}
		if target, err = d.read1(ctx, target.r); err != nil {
			return driver.Volume{}, d.engine(err)
		}
	}
	if typ == "lxc" && at.Mount != "" {
		// a container's path travels on its line: set it where it is stopped
		if m, _ := optOf(str(sp.h.cfg[sp.key]), "mp"); m != at.Mount {
			if err := d.remount(ctx, sp, at.Mount); err != nil {
				return driver.Volume{}, err
			}
			if sp, err = d.refind(ctx, sp.h.r, id); err != nil {
				return driver.Volume{}, err
			}
		}
	}
	if _, err := d.move(ctx, sp, target, id); err != nil {
		return driver.Volume{}, err
	}
	return d.settle(ctx, id)
}

// refind reads one guest again and finds the volume on it.
func (d *Driver) refind(ctx context.Context, r resource, id string) (spot, error) {
	h, err := d.read1(ctx, r)
	if err != nil {
		return spot{}, d.engine(err)
	}
	sp, ok := locate([]holder{h}, id)
	if !ok {
		return spot{}, fmt.Errorf("%w: volume %s left guest %d unseen", driver.ErrNotFound, id, r.VMID)
	}
	return sp, nil
}

// move hands a volume from its guest to another (the engine renames it
// after the target): the target's line first, the source's taken off after.
func (d *Driver) move(ctx context.Context, sp spot, target holder, id string) (spot, error) {
	key, err := freeKey(target)
	if strings.HasPrefix(sp.key, "unused") && target.r.Type == "lxc" {
		key, err = freeUnused(target) // a container's unused volume lands only as unused
	}
	if err != nil {
		return spot{}, err
	}
	if err := d.record(ctx, &target, volLine{ID: id, Key: key, VolID: "-"}, nil); err != nil {
		return spot{}, d.engine(err)
	}
	p := url.Values{"target-vmid": {strconv.Itoa(target.r.VMID)}}
	path := sp.h.r.path()
	if sp.h.r.Type == "lxc" {
		path += "/move_volume"
		p.Set("volume", sp.key)
		p.Set("target-volume", key)
	} else {
		path += "/move_disk"
		p.Set("disk", sp.key)
		p.Set("target-disk", key)
	}
	if err := d.c.run(ctx, http.MethodPost, path, p); err != nil {
		return spot{}, d.engine(err)
	}
	if err := d.forget(ctx, &sp.h, id); err != nil {
		return spot{}, d.engine(err)
	}
	return d.refind(ctx, target.r, id)
}

// plugBack plugs an unused disk of a (stopped) guest back into a free key of
// the same guest: a container's volume moved while unused, or one a cut left
// unused on its shelf. A mount point needs its path: the one it had, or a
// path of nobody's.
func (d *Driver) plugBack(ctx context.Context, sp spot, id, was string) (spot, error) {
	key, err := freeKey(sp.h)
	if err != nil {
		return spot{}, err
	}
	volid := volidOf(str(sp.h.cfg[sp.key]))
	line := volid
	if sp.h.r.Type == "lxc" {
		mount, ok := optOf(was, "mp")
		if !ok {
			mount = "/mnt/" + id
		}
		line += ",mp=" + mount
	}
	if err := d.record(ctx, &sp.h, volLine{ID: id, Key: key, VolID: volid}, url.Values{key: {line}}); err != nil {
		return spot{}, d.engine(err)
	}
	return d.refind(ctx, sp.h.r, id)
}

// restore writes back on a stopped guest the options a volume's line had
// before a move dropped them: its size, its backup flag, its serial.
func (d *Driver) restore(ctx context.Context, sp spot, id, was string) error {
	line := str(sp.h.cfg[sp.key])
	set := map[string]string{}
	if sizeGB(line) == 0 {
		gb := sizeGB(was)
		if gb == 0 {
			gb = d.storedGB(ctx, sp.h.r.Node, volidOf(line))
		}
		if gb > 0 {
			set["size"] = strconv.Itoa(gb) + "G"
		}
	}
	if b, ok := optOf(was, "backup"); ok {
		set["backup"] = b
	}
	if sp.h.r.Type == "qemu" {
		set["serial"] = driver.SerialOf(id)
	}
	next := withOpts(line, set)
	if next == line {
		return nil
	}
	return d.engine(d.setConfig(ctx, sp.h.r, url.Values{sp.key: {next}}))
}

// remount changes a filesystem volume's path on its (stopped) container.
func (d *Driver) remount(ctx context.Context, sp spot, mount string) error {
	if on, err := d.running(ctx, sp.h.r); err != nil {
		return d.engine(err)
	} else if on {
		return fmt.Errorf("%w: %s is a running container: a volume's path changes only while it is stopped", driver.ErrRefused, sp.h.machine())
	}
	return d.engine(d.setConfig(ctx, sp.h.r, url.Values{sp.key: {withOpts(str(sp.h.cfg[sp.key]), map[string]string{"mp": mount})}}))
}

// halfWay refuses a change to a volume a cut left unplugged: placing it again
// finishes the move.
func halfWay(sp spot, id string) error {
	if strings.HasPrefix(sp.key, "unused") {
		return fmt.Errorf("%w: %s was cut half-way through a move: place it again to finish it", driver.ErrRefused, id)
	}
	return nil
}

func (d *Driver) ResizeVolume(ctx context.Context, id string, sizeGB int) (driver.Volume, error) {
	d.vmu.Lock()
	defer d.vmu.Unlock()
	hs, err := d.survey(ctx)
	if err != nil {
		return driver.Volume{}, d.engine(err)
	}
	sp, ok := locate(hs, id)
	if !ok {
		return driver.Volume{}, driver.ErrNotFound
	}
	if err := halfWay(sp, id); err != nil {
		return driver.Volume{}, err
	}
	now := d.volumeOf(ctx, sp, id)
	switch {
	case sizeGB < now.SizeGB:
		return driver.Volume{}, fmt.Errorf("%w: it is %d GB; a volume never shrinks", driver.ErrRefused, now.SizeGB)
	case sizeGB == now.SizeGB:
		return now, nil
	}
	if err := d.c.run(ctx, http.MethodPut, sp.h.r.path()+"/resize", url.Values{"disk": {sp.key}, "size": {strconv.Itoa(sizeGB) + "G"}}); err != nil {
		return driver.Volume{}, d.engine(err)
	}
	return d.settle(ctx, id)
}

func (d *Driver) SetVolumeBackup(ctx context.Context, id string, backup bool) (driver.Volume, error) {
	d.vmu.Lock()
	defer d.vmu.Unlock()
	hs, err := d.survey(ctx)
	if err != nil {
		return driver.Volume{}, d.engine(err)
	}
	sp, ok := locate(hs, id)
	if !ok {
		return driver.Volume{}, driver.ErrNotFound
	}
	if err := halfWay(sp, id); err != nil {
		return driver.Volume{}, err
	}
	if d.volumeOf(ctx, sp, id).Backup == backup {
		return d.settle(ctx, id)
	}
	on, err := d.running(ctx, sp.h.r)
	if err != nil {
		return driver.Volume{}, d.engine(err)
	}
	if on && sp.h.r.Type == "lxc" {
		// read on a throwaway: a mount point's options wait for the next stop
		return driver.Volume{}, fmt.Errorf("%w: %s is a running container: a volume's backup flag changes only while it is stopped", driver.ErrRefused, sp.h.machine())
	}
	line := withOpts(str(sp.h.cfg[sp.key]), map[string]string{"backup": flag(backup)})
	if err := d.setConfig(ctx, sp.h.r, url.Values{sp.key: {line}}); err != nil {
		return driver.Volume{}, d.engine(err)
	}
	if on {
		if err := d.notPending(ctx, sp.h.r, sp.key); err != nil {
			return driver.Volume{}, err
		}
	}
	return d.settle(ctx, id)
}

// notPending: a change Proxmox could not make on a running guest waits for
// its next start; it is taken back, and said.
func (d *Driver) notPending(ctx context.Context, r resource, key string) error {
	var pending []struct {
		Key     string `json:"key"`
		Pending any    `json:"pending"`
	}
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/pending", nil, &pending); err != nil {
		return d.engine(err)
	}
	for _, pc := range pending {
		if pc.Key == key && pc.Pending != nil {
			_ = d.setConfig(ctx, r, url.Values{"revert": {key}})
			return fmt.Errorf("%w: Proxmox could not change %s while it runs; nothing was changed", driver.ErrRefused, key)
		}
	}
	return nil
}

func (d *Driver) DeleteVolume(ctx context.Context, id string) error {
	d.vmu.Lock()
	defer d.vmu.Unlock()
	hs, err := d.survey(ctx)
	if err != nil {
		return d.engine(err)
	}
	sp, ok := locate(hs, id)
	if !ok {
		return nil
	}
	if m := sp.h.machine(); m != "" {
		return fmt.Errorf("%w: %s is on %s: detach it first", driver.ErrRefused, id, m)
	}
	if !strings.HasPrefix(sp.key, "unused") {
		// a stopped guest's disk, deleted, becomes unusedN; an unused one,
		// deleted, is destroyed
		if err := d.setConfig(ctx, sp.h.r, nil, sp.key); err != nil {
			return d.engine(err)
		}
		if sp, err = d.refind(ctx, sp.h.r, id); err != nil {
			return err
		}
	}
	if err := d.setConfig(ctx, sp.h.r, nil, sp.key); err != nil {
		return d.engine(err)
	}
	h, err := d.read1(ctx, sp.h.r)
	if err != nil {
		return d.engine(err)
	}
	if err := d.forget(ctx, &h, id); err != nil {
		return d.engine(err)
	}
	return d.engine(d.tidy(ctx, hs, id, spot{h: h}))
}

// heldBy lists the volumes a guest holds (a machine is not deleted with one).
func (d *Driver) heldBy(ctx context.Context, r resource) ([]string, error) {
	h, err := d.read1(ctx, r)
	if err != nil {
		return nil, err
	}
	return h.holds(), nil
}
