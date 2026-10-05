package console_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/browsertest"
	"github.com/tomblancdev/hangar/internal/stacktest"
	"github.com/tomblancdev/hangar/internal/testoidc"
)

// The terminal's own element, and what its screen holds as text (the screen
// may be drawn on a canvas, where nothing reads it).
const (
	termOpen = `document.querySelector('.term[data-state="open"]')`
	termRead = `(document.querySelector('.term') && document.querySelector('.term').read ? document.querySelector('.term').read() : '')`
)

func screenSays(p *browsertest.Page, what, words string) {
	p.Wait(what, termRead+".includes("+fmt.Sprintf("%q", words)+")")
}

func termState(p *browsertest.Page, state string) {
	p.Wait("the terminal "+state, `document.querySelector('.term[data-state="`+state+`"]')`)
}

// A machine's terminal, from a real browser, under the page's own policy —
// which lets no script write a style, and is not loosened for it. A person
// with no key makes a machine, opens its terminal and is in: what they type
// comes back, the machine learns their window's size. Opened again on their
// phone it moves there, and the desk is told; the phone has the keys its
// keyboard lacks. An operator finds no terminal on that machine's page, and
// is refused one by its address. The machine stopped, its terminal says so.
func TestATerminalInThePage(t *testing.T) {
	br := browsertest.Start(t)
	s := stacktest.New(t, lab, "machines", "volumes", "images")
	alice := &testoidc.Claims{Subject: "alice", Name: "alice", Groups: []string{"users"}}

	// ---- alice, at her desk: a machine that names no key
	s.Iss.SignedIn(alice)
	p := br.Page(1280, 900, false)
	p.Goto(s.URL + "/console/#/t/machine/new")
	p.Press("SIGN IN")
	p.Sees("ASK FOR A MACHINE")
	p.Fill("name", "box")
	p.Fill("type", "t3.micro")
	p.Fill("image", "debian-13")
	p.Submit()
	p.Wait("the new machine's page", "location.hash.startsWith('#/r/')")
	id := strings.TrimPrefix(p.Hash(), "#/r/")
	p.Reads(stamp, "RUNNING")
	// born with its terminal open, and its page says so
	p.Wait("how its terminal greets, as asked", `[...document.querySelectorAll('.kv-row')].some((r) => r.firstChild.textContent === 'terminal' && r.lastChild.textContent === 'open')`)
	p.Shot("40-machine-with-a-terminal")

	// ---- its terminal, in its page: in, nothing asked
	p.Press("terminal")
	termState(p, "open")
	if h := p.Hash(); h != "#/r/"+id {
		t.Fatalf("the terminal opens in the machine's page; the page went to %s", h)
	}
	screenSays(p, "a shell, signed in", "user@box:~$")
	// on a canvas where this browser has one, else in the page's own nodes:
	// either way under the page's policy, read below
	var drawn string
	_ = p.Eval(`document.querySelector('.term').dataset.drawn`, &drawn)
	t.Logf("the screen is drawn on: %s", drawn)
	p.Sees("AS ASKED") // the machine's own page, around it
	p.Shot("41-terminal-in-the-page")
	// an action asked meanwhile does not take it away: a reboot, watched
	p.Press("reboot")
	p.Sees("reboot: done")
	termState(p, "open")

	// ---- the whole window, when wanted: the same screen, alone — and back
	p.Press("Full screen")
	p.Wait("the terminal's own address", "location.hash.endsWith('/terminal')")
	termState(p, "open")
	screenSays(p, "a shell, in the whole window", "user@box:~$")
	p.Lacks("AS ASKED")
	p.Type("whoami", true)
	screenSays(p, "what was typed, and its answer", "whoami\nuser\n")
	// the machine was told the window it is opened in: the grid the bar shows
	var grid string
	if err := p.Eval(`document.querySelector('.term-grid').textContent`, &grid); err != nil {
		t.Fatal(err)
	}
	var cols, rows int
	if _, err := fmt.Sscanf(grid, "%d × %d", &cols, &rows); err != nil || cols < 80 || rows < 20 {
		t.Fatalf("the grid the bar shows: %q (%v)", grid, err)
	}
	p.Type("stty size", true)
	screenSays(p, "the size the machine was told", fmt.Sprintf("stty size\n%d %d\n", rows, cols))
	p.Sees("‹ box")
	p.Shot("41a-terminal-full-screen")
	p.Press("Leave full screen")
	p.Wait("its page again", `location.hash.startsWith("#/r/`+id+`") && !location.hash.includes("/terminal")`)
	termState(p, "open")
	p.Sees("AS ASKED")
	if n := s.Core.Streams(); n != 1 {
		t.Fatalf("%d terminals open at the brain after the whole window and back", n)
	}
	// nothing the page's policy had to block: no style written inline, no
	// socket but the console's own
	p.Quiet()

	// ---- the same person's phone: the terminal moves there
	s.Iss.SignedIn(alice)
	ph := br.Page(390, 780, true)
	ph.Goto(s.URL + "/console/#/r/" + id + "/terminal")
	ph.Press("SIGN IN")
	termState(ph, "open")
	termState(p, "taken")
	p.Sees("Opened elsewhere")
	p.Shot("42-terminal-taken")
	// what is typed there goes to the machine, and comes back
	ph.Type("echo from-the-phone", true)
	screenSays(ph, "the same shell, on the phone", "from-the-phone\nuser@box:~$")
	// the keys a phone's keyboard has none of
	ph.Wait("the phone's row of keys", `[...document.querySelectorAll('.term-keys button')].filter(__t.shown).length === 7`)
	ph.Type("sleep 100", false)
	ph.Press("Ctrl")
	ph.Type("c", false)
	screenSays(ph, "Ctrl and C", "sleep 100^C\n")
	var wide int
	if err := ph.Eval("document.documentElement.scrollWidth - window.innerWidth", &wide); err != nil || wide > 0 {
		t.Fatalf("the terminal is %d px wider than the phone (%v)", wide, err)
	}
	ph.Shot("43-phone-terminal")
	ph.Quiet()

	// ---- and back to the desk, by the notice's own button
	p.Press("OPEN IT HERE")
	termState(p, "open")
	termState(ph, "taken")
	if n := s.Core.Streams(); n != 1 {
		t.Fatalf("%d terminals open at the brain, of one machine", n)
	}

	// ---- an operator: no terminal on alice's machine, by its page or its address
	s.Iss.SignedIn(&testoidc.Claims{Subject: "olive", Name: "olive", Groups: []string{"ops"}})
	op := br.Page(1280, 900, false)
	op.Goto(s.URL + "/console/#/r/" + id)
	op.Press("SIGN IN")
	op.Sees("owner alice")
	op.Wait("the operator's keys on it", `__t.press('stop')`)
	var offered bool
	if err := op.Eval(`!!document.querySelector('.actions [data-stream]')`, &offered); err != nil || offered {
		t.Fatalf("an operator is offered the terminal of someone's machine (%v)", err)
	}
	op.Open("#/r/" + id + "/terminal")
	termState(op, "refused")
	op.Sees("is alice's: its terminal is its owner's alone")
	op.Shot("44-terminal-refused")
	// alice's own was not touched by the attempt
	termState(p, "open")

	// ---- the machine stopped (by the operator, who may): its terminal says so
	op.Open("#/r/" + id)
	op.Press("stop")
	termState(p, "ended")
	p.Sees("The machine was stopped.")
	p.Shot("45-terminal-ended")
	op.Sees("stop: done")
	p.Press("OPEN AGAIN")
	termState(p, "refused")
	p.Sees("is stopped: start it, then open its terminal")

	// ---- leaving the page lets go of it
	op.Press("start")
	op.Sees("start: done")
	p.Press("TRY AGAIN")
	termState(p, "open")
	// put away by its own key…
	p.Press("Close")
	p.Wait("the terminal put away", `!document.querySelector('.term')`)
	deadline := time.Now().Add(5 * time.Second)
	for s.Core.Streams() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := s.Core.Streams(); n != 0 {
		t.Fatalf("%d terminals still open at the brain after it was closed", n)
	}
	// …and let go of when its page is left
	p.Press("terminal")
	termState(p, "open")
	p.Open("#/")
	p.Sees("YOUR LIMITS")
	deadline = time.Now().Add(5 * time.Second)
	for s.Core.Streams() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := s.Core.Streams(); n != 0 {
		t.Fatalf("%d terminals still open at the brain after its page was left", n)
	}
	// the audit names who opened what, and holds nothing typed
	logs := s.Logs.String()
	if !strings.Contains(logs, `"result":"opened"`) || !strings.Contains(logs, `"result":"closed"`) || !strings.Contains(logs, `"reason":"taken"`) {
		t.Error("the audit lacks an opening, a closing or a taking")
	}
	if strings.Contains(logs, "from-the-phone") || strings.Contains(logs, "sleep 100") {
		t.Error("something typed in a terminal is in the brain's log")
	}
	p.Quiet()
	op.Quiet()
}
