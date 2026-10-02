package cli

import (
	"strings"
	"testing"

	"github.com/tomblancdev/hangar/internal/stacktest"
)

func rowsOf(out string) []string {
	var rows []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		rows = append(rows, strings.Join(strings.Fields(l), " "))
	}
	return rows
}

// A person reads their own lists without an id: each resource by what they
// call it, the word it wears, its type's own sentence; a name goes where an
// id goes; and everything held fits one screen, each machine with what hangs
// on it.
func TestAPersonReadsTheirLists(t *testing.T) {
	s := stacktest.New(t, labConfig, "machines", "volumes")
	alice := newCmd(t, s.URL, s.Token(t, "alice", "users"))
	file := writeFile(t, t.TempDir(), "dev.yaml", devBox(false, false, true, "container"))
	alice.ok("apply", file)
	got := live(t, s, "alice")
	box, home := got["box"], got["home"]
	if box.Name != "box" || home.Name != "home" || got["me"].Name != "me" {
		t.Fatalf("an entry's key is what its resource is called: %q %q %q", box.Name, home.Name, got["me"].Name)
	}

	out, _ := alice.ok("volume", "list")
	want := []string{
		"NAME STATE WHAT ZONE AGE ID",
		"cache attached 8 GB · on box at /srv/cache lab 0s " + got["cache"].ID,
		"home attached 4 GB · on box at /home · backed up lab 0s " + home.ID,
	}
	if rows := rowsOf(out); strings.Join(rows, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the volumes:\n%s", out)
	}
	if out, _ = alice.ok("machine", "list"); !strings.Contains(out, "box   running  container · 4 cores · 8 GB (2 guaranteed) · debian-13") || strings.Contains(out, "OWNER") {
		t.Fatalf("the machines:\n%s", out)
	}
	if out, _ = alice.ok("keypair", "list"); !strings.Contains(out, "ssh-ed25519 · alice@laptop") {
		t.Fatalf("the key pairs:\n%s", out)
	}

	// one resource, as a card: what it uses and what uses it, by name
	out, _ = alice.ok("machine", "get", "box")
	for _, w := range []string{"box  machine · running  " + box.ID, "container · 4 cores · 8 GB (2 guaranteed) · debian-13", "whose     you · set dev",
		"room      2 GB guaranteed · 6 GB borrowed while it runs", "uses      key pair me", "used by   volume cache · volume home", "key pairs      me", "as seen"} {
		if !strings.Contains(out, w) {
			t.Fatalf("the card lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, got["me"].ID) {
		t.Fatalf("the card names a key pair by its id:\n%s", out)
	}

	// made at the command line, named there; a field takes a name too
	_, errs := alice.ok("volume", "create", "--name", "scratch", "--description", "throwaway", "--size-gb", "1", "--content", "filesystem")
	if !strings.Contains(errs, "created scratch vol-") || !strings.Contains(errs, "(parked,") {
		t.Fatalf("created: %s", errs)
	}
	if _, errs, code := alice.run("volume", "create", "--name", "home", "--size-gb", "1"); code != 1 || !strings.Contains(errs, "you already have a volume named home ("+home.ID+")") {
		t.Fatalf("a second home: %d %s", code, errs)
	}
	if _, errs, code := alice.run("volume", "get", "nope"); code != 1 || !strings.Contains(errs, "no volume named nope") {
		t.Fatalf("a name nobody holds: %d %s", code, errs)
	}
	if _, errs, code := alice.run("volume", "get", box.ID); code != 2 || !strings.Contains(errs, "is not a volume's id") {
		t.Fatalf("another type's id: %d %s", code, errs)
	}

	// everything held, on one screen
	out, _ = alice.ok("list")
	want = []string{
		"set dev · zone lab",
		"box machine running container · 4 cores · 8 GB (2 guaranteed) · debian-13",
		"├ cache volume attached 8 GB · on box at /srv/cache",
		"├ home volume attached 4 GB · on box at /home · backed up",
		"└ me key pair ready ssh-ed25519 · alice@laptop",
		"",
		"zone lab",
		"scratch volume parked 1 GB",
	}
	if rows := rowsOf(out); strings.Join(rows, "\n") != strings.Join(want, "\n") {
		t.Fatalf("everything:\n%s", out)
	}
	if lines := strings.Split(out, "\n"); !strings.HasPrefix(lines[3], "├ home   volume    attached  ") || !strings.HasPrefix(lines[7], "scratch  volume    parked    ") {
		t.Fatalf("the columns line up down the screen:\n%s", out)
	}

	// plugged in by name, on a machine named by name
	alice.ok("machine", "stop", "box")
	alice.ok("volume", "attach", "scratch", "--machine", "box", "--mount", "/scratch")
	if out, _ = alice.ok("list"); !strings.Contains(out, "├ scratch") || !strings.Contains(out, "1 GB · on box at /scratch") || !strings.Contains(out, "box        machine   stopped ") {
		t.Fatalf("after the attach:\n%s", out)
	}

	// an operator reads whose each is; and finds a person's by name
	root := newCmd(t, s.URL, s.Token(t, "root", "ops"))
	if out, _ = root.ok("machine", "list"); !strings.Contains(rowsOf(out)[0], "ID OWNER") || !strings.HasSuffix(rowsOf(out)[1], box.ID+" alice") {
		t.Fatalf("an operator's list:\n%s", out)
	}
	if out, _ = root.ok("machine", "get", "box", "-o", "id"); strings.TrimSpace(out) != box.ID {
		t.Fatalf("an operator, by name: %q", out)
	}
	if out, _ = root.ok("list"); !strings.Contains(out, "You hold nothing yet") {
		t.Fatalf("an operator's own: %s", out)
	}
}
