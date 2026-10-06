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

// The slip a terminal lays over its screen while its machine says nothing.
const termQuiet = `document.querySelector('.term[data-quiet] .slip')`

// No slip is laid over the screen.
const noSlip = `!document.querySelector('.term[data-quiet]') && !document.querySelector('.slip')`

// The terminal is unfolded under its machine's keys, and open.
const underKeys = `document.querySelector('.term-panel .term[data-state="open"]')`

// The keyboard is the terminal's: what is typed goes to the machine.
const termKeys = `document.activeElement && document.activeElement.closest('.term')`

// openings counts the terminals the brain's audit saw opened.
func openings(s *stacktest.Stack) int {
	return strings.Count(s.Logs.String(), `"result":"opened"`)
}

// A machine's terminal, from a real browser, under the page's own policy —
// which lets no script write a style, and is not loosened for it. A person
// with no key makes a machine, opens its terminal and is in: what they type
// comes back, the machine learns their window's size. The whole window is
// the same terminal, carried there and back — its screen, the line being
// typed, its socket: nothing is opened again. Opened again on their phone it
// moves there, and the desk is told; the shell left there says nothing by
// itself, so the page says it, with a key that asks the machine to draw; the
// phone has the keys its keyboard lacks. An operator finds no terminal on
// that machine's page, and is refused one by its address. The machine
// stopped, its terminal says so.
func TestATerminalInThePage(t *testing.T) {
	var ok bool
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
	// …and its new boot signs her in again, on the same screen
	p.Wait("the machine's new boot on the same screen", termRead+`.split("(automatic login)").length === 3 && `+termRead+`.endsWith("user@box:~$")`)
	// a terminal whose machine spoke is never called quiet: five seconds,
	// past the four the page waits for a first word
	time.Sleep(5 * time.Second)
	if err := p.Eval(noSlip, &ok); err != nil || !ok {
		t.Fatalf("a terminal whose machine greeted says it is quiet (%v)", err)
	}
	// the size her shell was signed in at: the grid under the keys
	gridOf := func(p *browsertest.Page) (cols, rows int) {
		t.Helper()
		var grid string
		if err := p.Eval(`document.querySelector('.term-grid').textContent`, &grid); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Sscanf(grid, "%d × %d", &cols, &rows); err != nil || cols < 40 || rows < 10 {
			t.Fatalf("the grid the bar shows: %q (%v)", grid, err)
		}
		return cols, rows
	}
	// lastSays: the screen ENDS with this — the last answer, not an earlier one
	lastSays := func(p *browsertest.Page, what, words string) {
		t.Helper()
		p.Wait(what, termRead+".endsWith("+fmt.Sprintf("%q", words)+")")
	}
	cols0, rows0 := gridOf(p)
	p.Type("stty size", true)
	lastSays(p, "the size the machine took at her sign-in", fmt.Sprintf("stty size\n%d %d\nuser@box:~$", rows0, cols0))
	p.Lacks("resized: exit signs you in")

	// ---- the whole window, when wanted: the same screen, alone — and back.
	// A mark on the screen and a line begun and not entered: both go with it
	p.Type("echo carried-over", true)
	screenSays(p, "a mark on the screen", "carried-over\nuser@box:~$")
	p.Type("echo half", false)
	screenSays(p, "a line begun", "user@box:~$ echo half")
	p.Press("Full screen")
	p.Wait("the terminal's own address", "location.hash.endsWith('/terminal')")
	termState(p, "open")
	p.Lacks("AS ASKED")
	// the same terminal: what its screen held is on it, and the keyboard is
	// its own again — the line is ended there
	screenSays(p, "the same screen, in the whole window", "carried-over\nuser@box:~$ echo half")
	p.Wait("the keyboard its own again, in the whole window", termKeys)
	p.Type("-typed", true)
	screenSays(p, "the line begun under the keys, ended in the whole window", "echo half-typed\nhalf-typed\nuser@box:~$")
	if n, held := openings(s), s.Core.Streams(); n != 1 || held != 1 {
		t.Fatalf("the whole window is the same terminal: the brain saw %d openings and holds %d", n, held)
	}
	p.Type("whoami", true)
	screenSays(p, "what was typed, and its answer", "whoami\nuser\n")
	// a window that changed tells the machine's shell nothing — its port
	// carries no size: the shell keeps its sign-in's, the bar says so, and
	// exit signs her in again at the size the window has now
	cols, rows := gridOf(p)
	if cols <= cols0 || rows <= rows0 {
		t.Fatalf("the whole window's grid, %d × %d, is no larger than the one under the keys, %d × %d", cols, rows, cols0, rows0)
	}
	p.Sees("resized: exit signs you in at this size")
	p.Type("stty size", true)
	lastSays(p, "her shell's size, still its sign-in's", fmt.Sprintf("stty size\n%d %d\nuser@box:~$", rows0, cols0))
	p.Type("exit", true)
	p.Wait("signed in again, in the whole window", termRead+`.split("(automatic login)").length === 4 && `+termRead+`.endsWith("user@box:~$")`)
	p.Type("stty size", true)
	lastSays(p, "the size taken at the new sign-in", fmt.Sprintf("stty size\n%d %d\nuser@box:~$", rows, cols))
	p.Wait("the bar no longer says « resized »: the machine asked the size again", `!document.querySelector('.term-grid .term-hint')`)
	p.Sees("‹ box")
	// her window dragged narrow, then wide again, a line begun on it: the
	// line is whole. A machine's port tells no shell its window changed, so
	// none would draw a line again that the screen had cut
	const long = "echo a-line-far-longer-than-a-narrow-window-is-wide-0123456789-0123456789-0123456789"
	gridCols := `parseInt(document.querySelector('.term-grid').textContent)`
	p.Type(long, false)
	screenSays(p, "a long line begun", "user@box:~$ "+long)
	p.Size(420, 700)
	p.Wait("the grid narrowed with her window", gridCols+" < 60")
	p.Size(1280, 900)
	p.Wait("the grid as wide as it was", fmt.Sprintf("%s === %d", gridCols, cols))
	screenSays(p, "the line whole after the window narrowed and widened", "user@box:~$ "+long)
	// a window with no room for a terminal at all — a moment on its way to
	// another size — is not fitted to: the grid stays, and so does the screen
	p.Size(1, 1)
	p.Wait("her window, one pixel", "innerWidth === 1 && innerHeight === 1")
	time.Sleep(300 * time.Millisecond)
	if err := p.Eval(fmt.Sprintf("%s === %d", gridCols, cols), &ok); err != nil || !ok {
		t.Fatalf("a window of one pixel refitted the terminal's grid (%v)", err)
	}
	p.Size(1280, 900)
	p.Wait("her window back", fmt.Sprintf("innerWidth === 1280 && %s === %d", gridCols, cols))
	screenSays(p, "the screen as it was", "user@box:~$ "+long)
	p.Type("", true)
	screenSays(p, "the long line, entered", "\n"+strings.TrimPrefix(long, "echo ")+"\nuser@box:~$")
	p.Shot("41a-terminal-full-screen")
	p.Press("Leave full screen")
	p.Wait("its page again", `location.hash.startsWith("#/r/`+id+`") && !location.hash.includes("/terminal")`)
	termState(p, "open")
	p.Sees("AS ASKED")
	// and back under its machine's keys: the same one still
	p.Wait("the terminal back under the keys", underKeys)
	screenSays(p, "the same screen, back in its page", "0123456789\nuser@box:~$")
	p.Wait("the keyboard its own again, under the keys", termKeys)
	p.Type("echo back-under-the-keys", true)
	screenSays(p, "typed there again", "\nback-under-the-keys\nuser@box:~$")
	// …and by any way back, as by that key: the browser's own, then the name
	// in the terminal's bar, which leads to its machine's page as it is
	// called — its terminal comes with it each time, unfolded as it was
	for _, back := range []struct{ how, do string }{
		{"the browser's way back", `history.back()`},
		{"the name in its bar", `document.querySelector('.term-bar a.term-back').click()`},
	} {
		p.Press("Full screen")
		p.Wait("the terminal's own address, again", "location.hash.endsWith('/terminal')")
		screenSays(p, "the same screen, in the whole window again", "\nback-under-the-keys\nuser@box:~$")
		if err := p.Eval(back.do, nil); err != nil {
			t.Fatal(err)
		}
		p.Wait("its page, by "+back.how, `location.hash.split("?")[0] === "#/r/`+id+`"`)
		p.Wait("the terminal under the keys, come back by "+back.how, underKeys)
		screenSays(p, "the same screen, come back by "+back.how, "\nback-under-the-keys\nuser@box:~$")
		p.Wait("the keyboard its own again, after "+back.how, termKeys)
	}
	var where string
	if _ = p.Eval("location.hash", &where); where != "#/r/"+id {
		t.Fatalf("the name in the bar leads to the machine's page as it is called: %s", where)
	}
	if n, held := openings(s), s.Core.Streams(); n != 1 || held != 1 {
		t.Fatalf("after the whole window and back, twice: the brain saw %d openings and holds %d terminals", n, held)
	}
	if strings.Contains(s.Logs.String(), `"result":"closed"`) {
		t.Fatal("a terminal carried between its two pages was closed on the way")
	}
	p.Shot("41b-terminal-back-in-the-page")
	// a line begun at the desk and not entered, left there
	p.Type("echo left-ha", false)
	lastSays(p, "a line begun at the desk", "user@box:~$ echo left-ha")
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
	// opened again: the shell she left is there and says nothing by itself —
	// an empty screen, which the page explains, with the key that asks the
	// machine to draw. Nothing is typed for her meanwhile
	ph.Wait("the phone says its machine is quiet", termQuiet)
	var held string
	if err := ph.Eval(termRead, &held); err != nil || held != "" {
		t.Fatalf("a terminal opened again holds %q before anything is asked (%v)", held, err)
	}
	ph.Sees("It is where it was left")
	ph.Shot("43a-phone-terminal-quiet")
	// Redraw: the line she had begun is drawn again — drawn, not entered: the
	// screen holds her prompt and that line, and nothing it would have said
	ph.Press("Redraw")
	ph.Wait("her shell, drawn at her asking: the line she left, not run", termRead+` === "user@box:~$ echo left-ha"`)
	ph.Wait("the slip gone at the machine's first word", noSlip)
	// what is typed there goes to the machine, and comes back
	ph.Type("lf", true)
	lastSays(ph, "the line left at the desk, ended on the phone", "user@box:~$ echo left-half\nleft-half\nuser@box:~$")
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
	// the desk still shows what it last saw, and the machine adds nothing:
	// said there too
	p.Wait("the desk says its machine is quiet", termQuiet)
	lastSays(p, "the desk's screen, as it was when it was taken", "back-under-the-keys\nuser@box:~$ echo left-ha")
	p.Shot("43b-terminal-quiet-at-the-desk")
	// …until her first key — one the machine answers nothing to: the slip
	// leaves at her key, the screen is as it was
	var before string
	_ = p.Eval(termRead, &before)
	p.Ctrl('a')
	p.Wait("the slip gone at her first key", noSlip)
	time.Sleep(200 * time.Millisecond)
	var after string
	if err := p.Eval(termRead, &after); err != nil || after != before {
		t.Fatalf("Ctrl and A drew on the screen: the slip left for the machine's word, not her key\n%q\n%q", before, after)
	}
	p.Type("echo at-the-desk-again", true)
	screenSays(p, "typed at the desk again", "\nat-the-desk-again\nuser@box:~$")

	// ---- a program that asked to be told of the window's focus, and is gone:
	// the desk's screen keeps what it asked. (The screen answers that asking
	// at once — « focused » — and, nothing being left to read it, the answer
	// lands on her shell's line: the very thing the page must not do itself.)
	p.Type(`echo -e \e[?1004h`, true)
	screenSays(p, "the mode asked of her screen", "1004h\n\nuser@box:~$")
	p.Ctrl('c')
	lastSays(p, "her line, cleared of the screen's own report", "^C\nuser@box:~$")
	// her terminal goes to her phone and comes back: taking the keyboard
	// back at the desk types no report into her shell
	ph.Press("OPEN IT HERE")
	termState(ph, "open")
	termState(p, "taken")
	// (her press on the notice takes the keyboard from the screen)
	if err := p.Eval(`document.activeElement && document.activeElement.blur()`, nil); err != nil {
		t.Fatal(err)
	}
	p.Press("OPEN IT HERE")
	termState(p, "open")
	termState(ph, "taken")
	p.Wait("the keyboard its own again, taken back", termKeys)
	// a report typed for her would have been echoed: the machine would have
	// spoken, and the page would not say it is quiet
	p.Wait("quiet at the desk: nothing was typed for her", termQuiet)
	p.Type("echo and-typed-by-her", true)
	screenSays(p, "typed by her, and only by her", "user@box:~$ echo and-typed-by-her\nand-typed-by-her\nuser@box:~$")

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
	// …and let go of when its page is left for anywhere but its own other
	// page — from the whole window, and from under the keys: at once
	p.Press("terminal")
	termState(p, "open")
	p.Press("Full screen")
	p.Wait("the terminal's own address", "location.hash.endsWith('/terminal')")
	termState(p, "open")
	if n := s.Core.Streams(); n != 1 {
		t.Fatalf("%d terminals open at the brain, carried to the whole window", n)
	}
	p.Open("#/")
	p.Sees("YOUR LIMITS")
	deadline = time.Now().Add(2 * time.Second)
	for s.Core.Streams() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := s.Core.Streams(); n != 0 {
		t.Fatalf("%d terminals still open at the brain after the whole window was left", n)
	}
	p.Open("#/r/" + id)
	p.Press("terminal")
	termState(p, "open")
	p.Open("#/")
	p.Sees("YOUR LIMITS")
	deadline = time.Now().Add(2 * time.Second)
	for s.Core.Streams() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := s.Core.Streams(); n != 0 {
		t.Fatalf("%d terminals still open at the brain after its page was left", n)
	}
	// a page left before the brain answered it opens nothing: the address
	// that asks for its terminal, and another one at once — what that page
	// would open, nobody could see, or close
	// (the machine's page is built — it has asked the brain — and is left in
	// the same breath, before the answer can have come)
	was := openings(s)
	if err := p.Eval(`(async () => {
	  location.hash = "#/r/`+id+`?open=terminal";
	  await new Promise((done) => addEventListener("hashchange", done, {once: true}));
	  location.hash = "#/tokens";
	  return "left";
	})()`, nil); err != nil {
		t.Fatal(err)
	}
	p.Wait("the page gone to", `location.hash === "#/tokens" && !document.querySelector('.term')`)
	time.Sleep(time.Second)
	if n, held := openings(s)-was, s.Core.Streams(); n != 0 || held != 0 {
		t.Fatalf("a page left before it was answered opened %d terminals, and %d are held at the brain", n, held)
	}
	// the audit names who opened what, and holds nothing typed
	logs := s.Logs.String()
	if !strings.Contains(logs, `"result":"opened"`) || !strings.Contains(logs, `"result":"closed"`) || !strings.Contains(logs, `"reason":"taken"`) {
		t.Error("the audit lacks an opening, a closing or a taking")
	}
	if strings.Contains(logs, "from-the-phone") || strings.Contains(logs, "sleep 100") || strings.Contains(logs, "left-ha") || strings.Contains(logs, "and-typed-by-her") {
		t.Error("something typed in a terminal is in the brain's log")
	}
	p.Quiet()
	op.Quiet()
}
