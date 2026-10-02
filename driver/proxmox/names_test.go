package proxmox

import (
	"strings"
	"testing"
)

// A guest's notes say what it is called, and what each volume on it is
// called, for whoever opens it on Proxmox's own screen — each on a line of
// its own that nothing is found by, so that the lines the driver does find
// things by read exactly as before.
func TestNameLinesInAGuestsNotes(t *testing.T) {
	const m, v1, v2 = "m-00000000000000001", "vol-00000000000000001", "vol-00000000000000002"
	desc := marker(m) + "\n" +
		volumeWord + " " + v1 + " mp0 tank:subvol-110-disk-1\n" +
		volumeWord + " " + v2 + " mp1 tank:subvol-110-disk-2"
	keep, lines := parseDesc(desc)
	if len(keep) != 1 || len(lines) != 2 {
		t.Fatalf("%v %v", keep, lines)
	}

	// the guest's own, under the line it is found by; each volume's, under its line
	keep, changed := withName(keep, m, "dev · machine of alice — the\nbuild  box")
	if !changed {
		t.Fatal("a first name changes nothing")
	}
	keep, _ = withName(keep, v2, "cache")
	keep, _ = withName(keep, v1, "home")
	got := formatDesc(keep, lines)
	want := strings.Join([]string{
		"made by hangar: " + m,
		"hangar name " + m + " dev · machine of alice — the build box",
		"hangar volume " + v1 + " mp0 tank:subvol-110-disk-1",
		"hangar name " + v1 + " home",
		"hangar volume " + v2 + " mp1 tank:subvol-110-disk-2",
		"hangar name " + v2 + " cache",
	}, "\n")
	if got != want {
		t.Fatalf("the notes:\n%s\nwant:\n%s", got, want)
	}

	// read back: the same lines, the same names — and what the driver finds
	// things by is untouched (the first line, each volume's line)
	keep2, lines2 := parseDesc(got)
	if strings.SplitN(got, "\n", 2)[0] != marker(m) || len(lines2) != 2 || lines2[0] != lines[0] || lines2[1] != lines[1] {
		t.Fatalf("what things are found by moved: %v", lines2)
	}
	if nameIn(keep2, m) != "dev · machine of alice — the build box" || nameIn(keep2, v1) != "home" || nameIn(keep2, v2) != "cache" {
		t.Fatalf("names read back: %v", keep2)
	}
	if formatDesc(keep2, lines2) != got {
		t.Fatal("written twice, the notes move")
	}

	// renamed: one line, not two; the same name again changes nothing
	keep2, changed = withName(keep2, v1, "maison")
	if !changed || nameIn(keep2, v1) != "maison" || strings.Count(formatDesc(keep2, lines2), "hangar name "+v1) != 1 {
		t.Fatalf("renamed: %v", keep2)
	}
	if _, changed = withName(keep2, v1, "maison"); changed {
		t.Fatal("the same name is a change")
	}
	// unnamed: its line goes
	keep2, changed = withName(keep2, m, "")
	if !changed || strings.Contains(formatDesc(keep2, lines2), "hangar name "+m) {
		t.Fatalf("unnamed: %v", keep2)
	}
	// a volume that left takes its name with it: its line names nothing here
	if out := formatDesc(keep2, lines2[:1]); !strings.Contains(out, "hangar name "+v1+" maison") || !strings.Contains(out, "hangar name "+v2+" cache") {
		t.Fatalf("a line whose volume is elsewhere is kept until it is forgotten:\n%s", out)
	}
	gone, _ := withName(keep2, v2, "")
	if out := formatDesc(gone, lines2[:1]); strings.Contains(out, v2) {
		t.Fatalf("forgotten, it still shows:\n%s", out)
	}

	// what a person wrote there by hand stays where it is
	keep3, lines3 := parseDesc(marker(m) + "\nnever stop this one — the operator\n" + volumeWord + " " + v1 + " mp0 tank:x")
	keep3, _ = withName(keep3, m, "dev")
	if out := formatDesc(keep3, lines3); out != marker(m)+"\nhangar name "+m+" dev\nnever stop this one — the operator\n"+volumeWord+" "+v1+" mp0 tank:x" {
		t.Fatalf("a hand-written line moved:\n%s", out)
	}
	// a shelf's first line is still its own
	shelf, _ := parseDesc(shelfMarker + "abc\n" + nameWord + " " + v1 + " home")
	if shelf[0] != shelfMarker+"abc" {
		t.Fatalf("a shelf's first line: %v", shelf)
	}
}
