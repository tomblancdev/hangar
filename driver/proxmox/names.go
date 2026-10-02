package proxmox

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/tomblancdev/hangar/driver"
)

// ---- What a person reads in a guest's notes ---------------------------------
//
// A guest's description says what it is to whoever opens it on Proxmox's own
// screen: under the line the driver finds it by, one line per thing the
// brain made there —
//
//	made by hangar: m-0123456789abcdef0
//	hangar name m-0123456789abcdef0 dev · machine of alice
//	hangar volume vol-0123456789abcdef0 mp0 tank:subvol-110-disk-1
//	hangar name vol-0123456789abcdef0 home
//
// A name line is for people: nothing is ever found by it. It is its own line
// (never a word more on a volume's line) so that an older driver reads past
// it, and a guest's two writers — the machines' driver and the volumes' —
// each write theirs holding the config's digest: a write that would undo the
// other's is refused by Proxmox, and tried again at the next look.

// nameWord begins a description line that says what an id is called.
const nameWord = "hangar name"

// nameLine reads one line: the id it names and its words.
func nameLine(l string) (id, text string, ok bool) {
	rest, ok := strings.CutPrefix(l, nameWord+" ")
	if !ok {
		return "", "", false
	}
	id, text, _ = strings.Cut(rest, " ")
	return id, strings.TrimSpace(text), id != ""
}

// nameIn is what a description's lines call an id; "" = nothing.
func nameIn(keep []string, id string) string {
	for _, l := range keep {
		if i, text, ok := nameLine(l); ok && i == id {
			return text
		}
	}
	return ""
}

// oneLine keeps a name's words on one line.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// withName writes what an id is called among a description's lines — after
// the first line, which says what the guest is — or takes it off (text "").
// It reports whether anything changed.
func withName(keep []string, id, text string) ([]string, bool) {
	text = oneLine(text)
	if nameIn(keep, id) == text {
		return keep, false
	}
	out := make([]string, 0, len(keep)+1)
	for _, l := range keep {
		if i, _, ok := nameLine(l); ok && i == id {
			continue
		}
		out = append(out, l)
	}
	if text == "" {
		return out, true
	}
	at := min(1, len(out))
	return slices.Insert(out, at, nameWord+" "+id+" "+text), true
}

// writeDesc writes a guest's description holding the digest its config was
// read with: Proxmox refuses the write when anything changed since.
func (d *Driver) writeDesc(ctx context.Context, r resource, cfg map[string]any, desc string) error {
	set := url.Values{}
	if dg := str(cfg["digest"]); dg != "" {
		set.Set("digest", dg)
	}
	if desc == "" {
		return d.setConfig(ctx, r, set, "description")
	}
	set.Set("description", desc)
	return d.setConfig(ctx, r, set)
}

// relabel writes a guest's own name line when it differs.
func (d *Driver) relabel(ctx context.Context, r resource, id, label string) error {
	var cfg map[string]any
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
		return err
	}
	keep, lines := parseDesc(str(cfg["description"]))
	keep, changed := withName(keep, id, label)
	if !changed {
		return nil
	}
	return d.writeDesc(ctx, r, cfg, formatDesc(keep, lines))
}

// Relabel writes the line that says what a guest is called.
func (d *Driver) Relabel(ctx context.Context, id, label string) (driver.Guest, error) {
	r, err := d.find(ctx, id)
	if err != nil {
		return driver.Guest{}, d.engine(err)
	}
	if err := d.relabel(ctx, r, id, label); err != nil {
		return driver.Guest{}, d.engine(err)
	}
	return d.read(ctx, r, id)
}

// RelabelVolume writes the line that says what a volume is called, on the
// guest it is on now.
func (d *Driver) RelabelVolume(ctx context.Context, id, label string) (driver.Volume, error) {
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
	keep, changed := withName(sp.h.keep, id, label)
	if changed {
		if err := d.writeDesc(ctx, sp.h.r, sp.h.cfg, formatDesc(keep, sp.h.lines)); err != nil {
			return driver.Volume{}, d.engine(err)
		}
		sp.h.keep = keep
	}
	return d.volumeOf(ctx, sp, id), nil
}
