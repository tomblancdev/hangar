package images

import (
	"slices"
	"strings"
	"testing"
)

// A recipe the brain could not bake as written is refused with the
// operator's config — not at the first bake.
func TestARecipeIsCheckedWhenConfigured(t *testing.T) {
	good := Recipe{Base: map[string]string{"vm": "store:import/debian.qcow2"}, DiskGB: 4, UserData: "#cloud-config\npackages: [qemu-guest-agent]\n"}
	if err := checkRecipe(good); err != nil {
		t.Fatalf("a good recipe: %v", err)
	}
	for _, tc := range []struct {
		change func(*Recipe)
		words  string
	}{
		{func(r *Recipe) { r.Base = nil }, "base"},
		{func(r *Recipe) { r.Base = map[string]string{"lxc": "x"} }, "vm or container"},
		{func(r *Recipe) { r.DiskGB = 0 }, "disk_gb"},
		{func(r *Recipe) { r.MemoryMB = 128 }, "memory_mb"},
		{func(r *Recipe) { r.Timeout = "10s" }, "1m to 6h"},
		{func(r *Recipe) { r.UserData = "packages: [x]\n" }, "#cloud-config document, or a #! script"},
		{func(r *Recipe) { r.UserData += "power_state:\n  mode: poweroff\n" }, "no power_state"},
	} {
		r := good
		r.Base = map[string]string{"vm": "store:import/debian.qcow2"}
		tc.change(&r)
		if err := checkRecipe(r); err == nil || !strings.Contains(err.Error(), tc.words) {
			t.Errorf("want %q: %v", tc.words, err)
		}
	}
	script := good
	script.UserData = "#!/bin/sh\necho baked\n"
	if err := checkRecipe(script); err != nil {
		t.Fatalf("a script recipe: %v", err)
	}
}

// Whom an image is shared with, written one way: sorted, without repeats,
// everyone alone saying it all — and the choice the tier's limit reads.
func TestSharesAreNormalized(t *testing.T) {
	for _, tc := range []struct {
		in, want []string
		vis      string
	}{
		{nil, []string{}, Private},
		{[]string{"b", "a", "b"}, []string{"a", "b"}, Shared},
		{[]string{"a", "*"}, []string{"*"}, Public},
	} {
		got, r := normalize(tc.in)
		if r != nil || !slices.Equal(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) || visibility(got) != tc.vis {
			t.Errorf("%v → %v %v (%s)", tc.in, got, r, visibility(got))
		}
	}
	if _, r := normalize([]string{" padded"}); r == nil {
		t.Error("a group name with a leading space")
	}
}

// Whether a machine may be born from an image, as the brain is told it: a
// bake not over is pending (or waiting), a failed one failed — until it is
// baked again —, a retired one retired, anything else usable.
func TestUsability(t *testing.T) {
	baked := Spec{Recipe: "debian", Attempt: 1}
	for _, tc := range []struct {
		s       Spec
		o       Observed
		word    string
		pending bool
	}{
		{baked, Observed{State: "pending", Attempt: 1}, "pending", true},
		{baked, Observed{State: "waiting", Attempt: 1}, "waiting", true},
		{baked, Observed{}, "pending", true},
		{baked, Observed{State: "available", Attempt: 1}, "", false},
		{baked, Observed{State: "failed", Attempt: 1}, "failed", false},
		{Spec{Recipe: "debian", Attempt: 2}, Observed{State: "failed", Attempt: 1}, "pending", true}, // rebaked
		{Spec{Recipe: "debian", Attempt: 1, Retired: true}, Observed{State: "available", Attempt: 1}, "retired", false},
		{Spec{Machine: "m-1"}, Observed{State: "available"}, "", false},
	} {
		u := usability(tc.s, tc.o)
		if u.GetUnusable() != tc.word || u.GetPending() != tc.pending {
			t.Errorf("%+v %+v: %q pending=%v, want %q pending=%v", tc.s, tc.o, u.GetUnusable(), u.GetPending(), tc.word, tc.pending)
		}
	}
}
