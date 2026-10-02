package fake

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/tomblancdev/hangar/driver"
)

// ---- The volumes facet ------------------------------------------------------

// kindFor is the kind of guest a content goes on.
func kindFor(content string) string {
	if content == driver.ContentFilesystem {
		return "container"
	}
	return "vm"
}

// volumesOn lists the volumes a guest holds, sorted. Called with e.mu held.
func (e *Engine) volumesOn(guest string) []string {
	var out []string
	for id, v := range e.state.Volumes {
		if v.Guest == guest {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (e *Engine) CanPark(string) error { return nil }

// place checks a volume may go to at (a guest of its kind, a filesystem
// volume's path) and sets where it is. Called with e.mu held.
func (e *Engine) place(v *driver.Volume, at driver.Place) error {
	if at.Guest == "" {
		v.Guest, v.Device = "", ""
		if v.Content == driver.ContentFilesystem && at.Mount != "" {
			v.Mount = at.Mount
		}
		v.InGuest = ""
		return nil
	}
	g, ok := e.state.Guests[at.Guest]
	if !ok {
		return fmt.Errorf("%w: no guest %s", driver.ErrNotFound, at.Guest)
	}
	if g.Kind != kindFor(v.Content) {
		return fmt.Errorf("%w: a %s volume goes on a %s, and %s is a %s", driver.ErrRefused, v.Content, kindFor(v.Content), at.Guest, g.Kind)
	}
	if v.Content == driver.ContentFilesystem {
		if at.Mount == "" {
			return fmt.Errorf("%w: a filesystem volume on a container needs its path", driver.ErrRefused)
		}
		v.Mount, v.InGuest = at.Mount, at.Mount
	} else {
		v.InGuest = "/dev/disk/by-id/fake-" + driver.SerialOf(v.ID)
	}
	used := map[string]bool{}
	for _, o := range e.state.Volumes {
		if o.Guest == at.Guest && o.ID != v.ID {
			used[o.Device] = true
		}
	}
	v.Guest, v.Device = at.Guest, ""
	for i := 1; v.Device == "" || used[v.Device]; i++ {
		v.Device = "disk" + strconv.Itoa(i)
	}
	return nil
}

func (e *Engine) CreateVolume(_ context.Context, s driver.VolumeSpec) (driver.Volume, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Volume{}, err
	}
	if v, ok := e.state.Volumes[s.ID]; ok {
		return *v, nil
	}
	if s.Content != driver.ContentBlock && s.Content != driver.ContentFilesystem {
		return driver.Volume{}, fmt.Errorf("%w: no content %q", driver.ErrRefused, s.Content)
	}
	e.state.Seq++
	v := &driver.Volume{ID: s.ID, EngineRef: fmt.Sprintf("fake-vol-%d", e.state.Seq), Content: s.Content, SizeGB: s.SizeGB,
		Backup: s.Backup, Node: "fake", Label: s.Label}
	if err := e.place(v, s.At); err != nil {
		return driver.Volume{}, err
	}
	if e.state.Volumes == nil {
		e.state.Volumes = map[string]*driver.Volume{}
	}
	e.state.Volumes[s.ID] = v
	return *v, e.save()
}

func (e *Engine) Volume(_ context.Context, id string) (driver.Volume, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Volume{}, err
	}
	v, ok := e.state.Volumes[id]
	if !ok {
		return driver.Volume{}, driver.ErrNotFound
	}
	return *v, nil
}

func (e *Engine) PlaceVolume(_ context.Context, id string, at driver.Place) (driver.Volume, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Volume{}, err
	}
	v, ok := e.state.Volumes[id]
	if !ok {
		return driver.Volume{}, driver.ErrNotFound
	}
	if v.Guest == at.Guest && (at.Guest == "" || v.Mount == at.Mount || v.Content == driver.ContentBlock) {
		return *v, nil
	}
	if v.Guest != "" && at.Guest != "" && !e.has(driver.VolumeMoveBetweenGuests) {
		return driver.Volume{}, fmt.Errorf("%w: this zone moves no volume between guests", driver.ErrRefused)
	}
	if from, ok := e.state.Guests[v.Guest]; ok && from.Running && from.Kind == "container" {
		return driver.Volume{}, fmt.Errorf("%w: %s is a running container: it lets go of a volume only when stopped", driver.ErrRefused, v.Guest)
	}
	moved := *v
	if err := e.place(&moved, at); err != nil {
		return driver.Volume{}, err
	}
	*v = moved
	return *v, e.save()
}

func (e *Engine) ResizeVolume(_ context.Context, id string, sizeGB int) (driver.Volume, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Volume{}, err
	}
	v, ok := e.state.Volumes[id]
	if !ok {
		return driver.Volume{}, driver.ErrNotFound
	}
	if sizeGB < v.SizeGB {
		return driver.Volume{}, fmt.Errorf("%w: it is %d GB; a volume never shrinks", driver.ErrRefused, v.SizeGB)
	}
	v.SizeGB = sizeGB
	return *v, e.save()
}

func (e *Engine) RelabelVolume(_ context.Context, id, label string) (driver.Volume, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Volume{}, err
	}
	v, ok := e.state.Volumes[id]
	if !ok {
		return driver.Volume{}, driver.ErrNotFound
	}
	v.Label = label
	return *v, e.save()
}

func (e *Engine) SetVolumeBackup(_ context.Context, id string, backup bool) (driver.Volume, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return driver.Volume{}, err
	}
	v, ok := e.state.Volumes[id]
	if !ok {
		return driver.Volume{}, driver.ErrNotFound
	}
	v.Backup = backup
	return *v, e.save()
}

func (e *Engine) DeleteVolume(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return err
	}
	v, ok := e.state.Volumes[id]
	if !ok {
		return nil
	}
	if v.Guest != "" {
		return fmt.Errorf("%w: it is on %s: detach it first", driver.ErrRefused, v.Guest)
	}
	delete(e.state.Volumes, id)
	return e.save()
}

// ForgetVolume drops a volume as if someone destroyed it on the engine.
func (e *Engine) ForgetVolume(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.load()
	delete(e.state.Volumes, id)
	_ = e.save()
}

// TamperVolume changes a volume as if someone edited it on the engine.
func (e *Engine) TamperVolume(id string, f func(*driver.Volume)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.load()
	if v, ok := e.state.Volumes[id]; ok {
		f(v)
		_ = e.save()
	}
}
