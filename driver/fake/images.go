package fake

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/tomblancdev/hangar/driver"
)

// ---- The images facet -------------------------------------------------------

// A fake bake takes two calls: the first starts its builder (pending), the
// next finishes it — available, or failed when its user data carries the
// words FailBake. Held, a builder is let go and the bake waits.
//
// Zone option image_kinds: the kinds it makes images for (comma-separated;
// default: the kinds the zone runs).

// FailBake in a recipe's user data makes a fake bake fail, as a first boot
// whose cloud-init reported errors would.
const FailBake = "hangar-fake: fail this bake"

// fakeImage is an image, or its bake, in the file.
type fakeImage struct {
	driver.Image
	// Builder: its bake's builder is at work.
	Builder bool `json:"builder,omitempty"`
	// From: the guest it was saved from.
	From string `json:"from,omitempty"`
}

func (e *Engine) ImageKinds() []string {
	if e.imageKinds != nil {
		return slices.Clone(e.imageKinds)
	}
	var out []string
	if e.has(driver.KindVM) {
		out = append(out, "vm")
	}
	if e.has(driver.KindContainer) {
		out = append(out, "container")
	}
	return out
}

func (e *Engine) Bake(_ context.Context, s driver.BakeSpec, held bool) (driver.Image, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Image{}, err
	}
	if !slices.Contains(e.ImageKinds(), s.Kind) {
		return driver.Image{}, fmt.Errorf("%w: this zone makes no %s images", driver.ErrRefused, s.Kind)
	}
	if e.state.Images == nil {
		e.state.Images = map[string]*fakeImage{}
	}
	im, ok := e.state.Images[s.ID]
	if ok && im.State == driver.ImageAvailable {
		return im.Image, nil
	}
	if !ok || im.Attempt != s.Attempt {
		// a bake not begun, or one of another attempt: this attempt's own
		e.state.Seq++
		im = &fakeImage{Image: driver.Image{ID: s.ID, EngineRef: fmt.Sprintf("fake-%d", e.state.Seq), Kind: s.Kind, Node: "fake",
			State: driver.ImageWaiting, Attempt: s.Attempt, SizeGB: s.DiskGB}}
		e.state.Images[s.ID] = im
	}
	switch {
	case held:
		im.Builder, im.State = false, driver.ImageWaiting
	case !im.Builder:
		im.Builder, im.State = true, driver.ImagePending
	case strings.Contains(string(s.UserData), FailBake):
		delete(e.state.Images, s.ID) // the builder is gone; its words stay with the plugin
		failed := im.Image
		failed.State, failed.Detail = driver.ImageFailed, "the builder's first boot reported errors: "+FailBake
		return failed, e.save()
	default:
		im.Builder, im.State, im.Ref = false, driver.ImageAvailable, "fake-img-"+s.ID
	}
	return im.Image, e.save()
}

func (e *Engine) SaveImage(_ context.Context, id, guest string) (driver.Image, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Image{}, err
	}
	if im, ok := e.state.Images[id]; ok {
		return im.Image, nil
	}
	g, ok := e.state.Guests[guest]
	switch {
	case !ok:
		return driver.Image{}, fmt.Errorf("%w: no guest %s", driver.ErrNotFound, guest)
	case !slices.Contains(e.ImageKinds(), g.Kind):
		return driver.Image{}, fmt.Errorf("%w: this zone makes no %s images", driver.ErrRefused, g.Kind)
	case g.Running:
		return driver.Image{}, fmt.Errorf("%w: %s runs: an image is saved from a stopped guest", driver.ErrRefused, guest)
	}
	if held := e.volumesOn(guest); len(held) > 0 {
		return driver.Image{}, fmt.Errorf("%w: %s holds %s: an image is its system disk alone — detach them first", driver.ErrRefused,
			guest, strings.Join(held, ", "))
	}
	if e.state.Images == nil {
		e.state.Images = map[string]*fakeImage{}
	}
	e.state.Seq++
	im := &fakeImage{From: guest, Image: driver.Image{ID: id, EngineRef: fmt.Sprintf("fake-%d", e.state.Seq), Kind: g.Kind, Node: "fake",
		State: driver.ImageAvailable, Ref: "fake-img-" + id, SizeGB: g.DiskGB}}
	e.state.Images[id] = im
	return im.Image, e.save()
}

func (e *Engine) Image(_ context.Context, id string) (driver.Image, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Image{}, err
	}
	im, ok := e.state.Images[id]
	if !ok {
		return driver.Image{}, driver.ErrNotFound
	}
	return im.Image, nil
}

// DeleteImage refuses an image a guest was born from: the fake's guests
// share their image's disk, as Proxmox VE's linked clones do.
func (e *Engine) DeleteImage(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return err
	}
	im, ok := e.state.Images[id]
	if !ok {
		return nil
	}
	if im.Ref != "" {
		var born []string
		for gid, s := range e.state.Specs {
			if _, live := e.state.Guests[gid]; live && s.Image == im.Ref {
				born = append(born, gid)
			}
		}
		sort.Strings(born)
		if len(born) > 0 {
			return fmt.Errorf("%w: %s still share its disk: delete them first — retire it meanwhile", driver.ErrRefused, strings.Join(born, ", "))
		}
	}
	delete(e.state.Images, id)
	return e.save()
}

var _ driver.Images = (*Engine)(nil)
