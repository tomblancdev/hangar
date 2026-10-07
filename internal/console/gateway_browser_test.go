package console_test

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/browsertest"
	"github.com/tomblancdev/hangar/internal/stacktest"
	"github.com/tomblancdev/hangar/internal/testoidc"
)

// gateway plays a front that locks the console behind a verdict of its own,
// as a proxy's forward_auth does. A request that carries no verdict it holds
// — a page, a call, a socket's opening alike — is answered a redirect to its
// sign-in, on another origin, and a fresh cookie: an empty session, for a
// sign-in a call never performs. A page follows it: the sign-in knows the
// person and sends them straight back, through the gateway's own callback,
// to the address they asked for. What it lets through reaches the console
// as it was asked — the same host, sockets too. A verdict lasts until
// lapse().
type gateway struct {
	URL string // where people open the console

	front, far *httptest.Server

	mu       sync.Mutex
	let      map[string]bool   // the verdicts it holds, by cookie
	began    map[string]string // by cookie: where a sign-in under way goes back to
	deaf     string            // a path under which it lets nothing through, verdict or not
	last     string            // what ends its time: this call is let through, and none after it
	hold     string            // the next call of this path is let through and kept, until release()
	parked   chan struct{}     // closed by release(): a call is kept
	socks    map[net.Conn]bool // the sockets it carries
	inflight int               // calls it is answering, sockets aside
	pages    int               // the console's page, let through
	signins  int               // sign-ins that came back with a verdict
	turned   []string          // what it turned back since its time was last up, in order
}

const gatewayCookie = "gateway_session"

func newGateway(t *testing.T, console string) *gateway {
	t.Helper()
	to, err := url.Parse(console)
	if err != nil {
		t.Fatal(err)
	}
	g := &gateway{let: map[string]bool{}, began: map[string]string{}, socks: map[net.Conn]bool{}}
	// its sign-in: it knows the person, and asks nothing
	g.far = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Query().Get("back"), http.StatusFound)
	}))
	through := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(to)
		pr.Out.Host = pr.In.Host
	}}
	g.front = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gateway/callback" {
			g.callback(w, r)
			return
		}
		socket := r.Header.Get("Upgrade") != ""
		c, _ := r.Cookie(gatewayCookie)
		var kept chan struct{}
		g.mu.Lock()
		ok := c != nil && g.let[c.Value] && !(g.deaf != "" && strings.HasPrefix(r.URL.Path, g.deaf))
		if ok {
			if r.URL.Path == "/console/" {
				g.pages++
			}
			if g.last != "" && strings.HasSuffix(r.URL.Path, g.last) {
				g.let, g.turned, g.last = map[string]bool{}, nil, ""
			}
			if g.hold != "" && strings.HasSuffix(r.URL.Path, g.hold) {
				g.hold, g.parked = "", make(chan struct{})
				kept = g.parked
			}
		}
		if !socket {
			g.inflight++
		}
		g.mu.Unlock()
		if !socket {
			defer func() { g.mu.Lock(); g.inflight--; g.mu.Unlock() }()
		}
		if !ok {
			g.turnBack(w, r)
			return
		}
		if kept != nil {
			select {
			case <-kept:
			case <-r.Context().Done():
				return
			}
		}
		through.ServeHTTP(w, r)
	}))
	g.front.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateHijacked {
			g.mu.Lock()
			g.socks[c] = true
			g.mu.Unlock()
		}
	}
	g.front.Start()
	g.URL = g.front.URL
	t.Cleanup(func() {
		g.release()
		g.cut()
		g.front.CloseClientConnections()
		g.front.Close()
		g.far.Close()
	})
	return g
}

func (g *gateway) turnBack(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	g.mu.Lock()
	g.began[id] = r.URL.RequestURI()
	g.turned = append(g.turned, r.URL.Path)
	g.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: gatewayCookie, Value: id, Path: "/", MaxAge: 3601, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	back := g.URL + "/gateway/callback?state=" + id
	http.Redirect(w, r, g.far.URL+"/authorize?back="+url.QueryEscape(back), http.StatusFound)
}

func (g *gateway) callback(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie(gatewayCookie)
	state := r.URL.Query().Get("state")
	g.mu.Lock()
	to, known := g.began[state]
	if c == nil || c.Value != state || !known {
		g.mu.Unlock()
		// a sign-in whose session a later refusal replaced: begun again
		http.Redirect(w, r, "/console/", http.StatusFound)
		return
	}
	delete(g.began, state)
	g.let[state] = true
	g.signins++
	g.mu.Unlock()
	http.Redirect(w, r, to, http.StatusFound)
}

// lapse: its time is up — every verdict is forgotten.
func (g *gateway) lapse() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.let = map[string]bool{}
	g.turned = nil
}

// lapseAfter: its time is up behind the next call of this path — that one
// is let through, and nothing after it.
func (g *gateway) lapseAfter(path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.last = path
}

// holdNext: the next call of this path is let through, and kept on its way
// to the console until release() — a call asked before the gateway's time is
// up, and answered after.
func (g *gateway) holdNext(path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hold = path
}

func (g *gateway) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.parked != nil {
		close(g.parked)
		g.parked = nil
	}
}

// wait waits for the gateway to be somewhere: a call kept, or none on its way.
func (g *gateway) wait(t *testing.T, what string, there func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		g.mu.Lock()
		ok := there()
		g.mu.Unlock()
		if ok {
			return
		}
	}
	t.Fatalf("the gateway: %s, never", what)
}

func (g *gateway) keeps(t *testing.T) {
	t.Helper()
	g.wait(t, "a call kept on its way", func() bool { return g.parked != nil })
}

// idle: nothing it was asked is still on its way — what a page asked before
// it was left has been answered.
func (g *gateway) idle(t *testing.T) {
	t.Helper()
	for range 2 {
		g.wait(t, "no call on its way", func() bool { return g.inflight == 0 })
		time.Sleep(30 * time.Millisecond)
	}
}

// turnedBack lists what it turned back since its time was last up.
func (g *gateway) turnedBack() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.turned...)
}

// cut ends every socket it carries, as a proxy's reload does.
func (g *gateway) cut() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for c := range g.socks {
		_ = c.Close()
	}
	g.socks = map[net.Conn]bool{}
}

// deafTo: lets nothing through under a path, whatever the verdict — a front
// that going through again does not cure.
func (g *gateway) deafTo(path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deaf = path
}

func (g *gateway) seen() (pages, signins int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pages, g.signins
}

// A tab she is not looking at, and one she comes back to: a page that is not
// looked at asks nothing.
const (
	lookAway = `Object.defineProperty(document, 'visibilityState', {configurable: true, get: () => 'hidden'}); document.dispatchEvent(new Event('visibilitychange'))`
	lookBack = `delete document.visibilityState; document.dispatchEvent(new Event('visibilitychange'))`
)

// Her page's clock, which the test moves on: the minute a page must have
// worked for is not waited sixty real seconds.
const aMinuteLater = `(() => {
  if (!window.__later) {
    const real = performance.now.bind(performance);
    let on = 0;
    performance.now = () => real() + on;
    window.__later = (ms) => { on += ms; };
  }
  window.__later(61000);
})()`

// The page's own notice that the gateway turned it back; what it says to
// someone whose page holds something; its way through in another tab.
const (
	pageTimedOut = `document.querySelector('#gate .flyer')`
	noNotice     = `!document.querySelector('#gate .flyer')`
	noticeLink   = `document.querySelector('#gate .flyer a')`
	termLink     = `document.querySelector('.term-note .flyer a')`
)

// The console behind a gateway, from a real browser. A gateway lets a person
// through for a time; past it, it turns the page's calls back to its
// sign-in, which a call cannot follow. The page heals itself: it is loaded
// anew — through the gateway, nothing asked — and comes back where it was.
// Unless it holds what that would lose: with a terminal, or a form someone
// has used, it says so and waits, and carries on by itself once another tab
// went through. A terminal that is open is left alone; one the gateway
// turned back says so, where it said « the connection was lost » for ever.
// And a page that has not worked for a minute does not load itself anew:
// one turned back as soon as it is loaded is not cured by loading it again.
func TestThePageBehindAGateway(t *testing.T) {
	var ok bool
	br := browsertest.Start(t)
	s := stacktest.New(t, lab, "machines", "volumes", "images")
	g := newGateway(t, s.URL)
	s.Iss.SignedIn(&testoidc.Claims{Subject: "alice", Name: "alice", Groups: []string{"users"}})
	p := br.Page(1280, 900, false)

	do := func(js string) {
		t.Helper()
		if err := p.Eval(js, nil); err != nil {
			t.Fatal(err)
		}
	}
	is := func(what, js string) {
		t.Helper()
		if err := p.Eval(js, &ok); err != nil || !ok {
			t.Fatalf("%s (%v)\nthe page reads:\n%s", what, err, p.Text())
		}
	}
	// later: a minute in which every answer was the console's
	later := func() { t.Helper(); do(aMinuteLater) }
	// here marks the page that is loaded: gone, it was loaded anew
	here := func() { t.Helper(); do(`window.__here = true`) }
	const anew = `window.__here !== true`
	stays := func(what string) { t.Helper(); is(what+": the page was loaded anew", `window.__here === true`) }
	// away: she looks elsewhere — the page asks nothing more, and what it had
	// asked is answered
	away := func() { t.Helper(); do(lookAway); g.idle(t) }
	back := func() { t.Helper(); do(lookBack) }
	// notice: the page says the gateway turned it back — and, to someone
	// whose page holds something, how to keep it
	notice := func(held bool) {
		t.Helper()
		p.Wait("the page's notice", pageTimedOut)
		p.Sees("Reload the page to go through")
		if err := p.Eval(`!!`+noticeLink, &ok); err != nil || ok != held {
			t.Fatalf("the notice offers another tab: %v, the page holding something: %v (%v)", ok, held, err)
		}
	}
	// loaded: the page was loaded anew — once, and once through the gateway
	var pages, signins int
	count := func() { pages, signins = g.seen() }
	loaded := func(what, ready string) {
		t.Helper()
		p.Wait("the page loaded anew "+what, anew+` && `+ready)
		if now, through := g.seen(); now != pages+1 || through != signins+1 {
			t.Fatalf("%s, the page was loaded %d times and went through the gateway %d times", what, now-pages, through-signins)
		}
	}
	const machine = `document.querySelector('.under .stamp')`
	// look: a look of the page's own, asked for now
	look := func() { t.Helper(); p.Press("rename"); p.Press("Cancel") }
	// keys: she clicks her terminal's screen
	keys := func() {
		t.Helper()
		do(`document.querySelector('.term .xterm-helper-textarea').focus()`)
		p.Wait("the keyboard the terminal's", termKeys)
	}
	// elsewhere: she goes through the gateway in another tab — the one the
	// notice offers — and comes back to this one
	elsewhere := func(link string) {
		t.Helper()
		var href string
		if err := p.Eval(link+`.href`, &href); err != nil || href != g.URL+"/console/" {
			t.Fatalf("the other tab the notice offers: %q (%v)", href, err)
		}
		is("the notice's way through opens beside this page, and hands it nothing", link+`.target === '_blank' && `+link+`.rel === 'noopener'`)
		away()
		_, before := g.seen()
		tab := p.Tab(1280, 900)
		tab.Goto(href)
		tab.Sees("YOUR LIMITS")
		tab.Quiet()
		tab.Close()
		if _, after := g.seen(); after != before+1 {
			t.Fatalf("the other tab went through the gateway %d times", after-before)
		}
		back()
	}

	// ---- through the gateway, then signed in: the place asked for is kept
	p.Goto(g.URL + "/console/#/t/machine/new")
	p.Press("SIGN IN")
	p.Sees("ASK FOR A MACHINE")
	p.Fill("name", "box")
	p.Fill("type", "t3.micro")
	p.Fill("image", "debian-13")
	p.Submit()
	p.Wait("the new machine's page", "location.hash.startsWith('#/r/')")
	id := strings.TrimPrefix(p.Hash(), "#/r/")
	p.Reads(stamp, "RUNNING")
	if _, n := g.seen(); n != 1 {
		t.Fatalf("she went through the gateway %d times to come in", n)
	}
	p.Quiet()

	// ---- a page turned back as soon as it is loaded does not load itself
	// again: a front that lets the page through and none of its calls
	g.deafTo("/console/api/")
	count()
	do(`location.reload()`)
	p.Sees("asks who you are again")
	time.Sleep(2 * time.Second)
	if now, _ := g.seen(); now != pages+1 {
		t.Fatalf("a page whose calls are turned back at once was loaded %d times", now-pages)
	}
	g.deafTo("")
	p.Press("Try again")
	p.Reads(stamp, "RUNNING")

	// ---- nor does one that has worked for six seconds: it says so, and its
	// key loads it anew
	time.Sleep(6 * time.Second)
	here()
	away()
	g.lapse()
	count()
	back()
	notice(false)
	stays("six seconds after it was loaded")
	if now, _ := g.seen(); now != pages {
		t.Fatalf("a page that worked for six seconds was loaded %d times", now-pages)
	}
	p.Shot("46-timed-out")
	p.Press("RELOAD")
	loaded("by her key", machine)
	p.Reads(stamp, "RUNNING")

	// ---- after a minute's work, the gateway's time is up while she looks
	// elsewhere: back on the tab, the page is loaded anew — through the
	// gateway, nothing asked — and she is where she was
	later()
	here()
	away()
	g.lapse()
	count()
	back()
	loaded("when she came back to it", machine)
	p.Reads(stamp, "RUNNING")
	if h := p.Hash(); h != "#/r/"+id {
		t.Fatalf("loaded anew, the page is at %s", h)
	}
	// what asked was the tab she came back to, at once — not a look, later
	if first := g.turnedBack(); len(first) == 0 || first[0] != "/console/session" {
		t.Fatalf("back on the tab, what the gateway turned back first: %v", first)
	}
	p.Quiet()

	// ---- with her terminal open — asked for by the address — the page is not
	// loaded anew: it says so, once, and the terminal is left alone
	p.Open("#/r/" + id + "?open=terminal")
	termState(p, "open")
	screenSays(p, "a shell, signed in", "user@box:~$")
	p.Type("echo before-its-time", true)
	screenSays(p, "typed before the gateway's time", "\nbefore-its-time\nuser@box:~$")
	// a look of hers is on its way — let through, not answered yet — when the
	// gateway's time is up
	later()
	here()
	g.holdNext("/api/v1/resources/" + id)
	look()
	g.keeps(t)
	g.lapse()
	back()
	notice(true)
	p.Sees("TIMED OUT")
	p.Sees("What is open here goes with it")
	stays("with a terminal open")
	do(pageTimedOut + `.__first = true`)
	p.Shot("47-timed-out-with-a-terminal")
	// answered now, that look says nothing of the gateway now: the notice
	// stays. And a look turned back meanwhile adds nothing — it is said once,
	// by the notice someone is reading, not again as each look's failure
	g.release()
	look()
	g.wait(t, "the page's own look turned back", func() bool {
		for _, path := range g.turned {
			if strings.HasSuffix(path, "/api/v1/resources/"+id) {
				return true
			}
		}
		return false
	})
	time.Sleep(300 * time.Millisecond)
	is("the notice she was reading was taken away, or drawn again", pageTimedOut+` && `+pageTimedOut+`.__first === true`)
	is("a look turned back by the gateway is said again, as its own failure", `!document.querySelector('.banner')`)
	stays("its looks turned back")
	keys()
	p.Type("echo after-its-time", true)
	screenSays(p, "her terminal, past the gateway's time", "\nafter-its-time\nuser@box:~$")
	// another tab went through: the page carries on, her terminal never touched
	elsewhere(noticeLink)
	p.Wait("the notice gone: the page carries on", noNotice)
	stays("once another tab went through")
	keys()
	p.Type("echo and-on", true)
	screenSays(p, "her terminal, the page carrying on", "\nand-on\nuser@box:~$")
	if n := openings(s); n != 1 {
		t.Fatalf("the terminal was opened %d times", n)
	}

	// ---- an action asked as the gateway's time ends: taken by the brain, how
	// it ends not read — which is no refusal, and is not said as one
	later()
	g.lapseAfter("/actions/reboot")
	p.Press("reboot")
	p.Sees("reboot: asked")
	notice(true)
	p.Lacks("nothing was changed")
	stays("an action's end not read")
	p.Wait("the machine's new boot on her screen", termRead+`.split("(automatic login)").length === 3 && `+termRead+`.endsWith("user@box:~$")`)
	// put away by its key, it is no longer what the address asks for, and
	// nothing is in the way: at its next look the page is loaded anew by
	// itself — and opens no terminal nobody asked for
	count()
	p.Press("Close")
	p.Wait("the terminal put away", `!document.querySelector('.term')`)
	if h := p.Hash(); h != "#/r/"+id {
		t.Fatalf("its terminal put away, the page's address is %s (it complained of: %v)", h, p.Errors())
	}
	stays("its terminal put away, nothing asked yet")
	look()
	loaded("once the terminal was put away", machine)
	p.Reads(stamp, "RUNNING")
	time.Sleep(500 * time.Millisecond)
	is("loaded anew, the page shows a terminal nobody asked for", `!document.querySelector('.term')`)
	if n := openings(s); n != 1 {
		t.Fatalf("loaded anew, the page opened the terminal again: %d openings", n)
	}
	p.Quiet()

	// ---- a form: a key pressed in it is enough for the page to keep it
	p.Open("#/")
	p.Press("+ Key pair")
	p.Sees("ASK FOR A KEY PAIR")
	p.Wait("the form's first field", `__t.field("name")`)
	do(`__t.field("name").focus()`)
	p.Ctrl('a')
	later()
	here()
	away()
	g.lapse()
	back()
	notice(true)
	stays("with a key pressed in a form")
	elsewhere(noticeLink)
	p.Wait("the notice gone", noNotice)
	// …and what she types in it is kept: asked past the gateway's time,
	// nothing is sent, nothing is lost, and it is asked again once another
	// tab went through
	p.Fill("name", "laptop")
	p.Fill("public key", aliceKey)
	later()
	g.lapse()
	p.Submit()
	notice(true)
	p.Sees("nothing was changed")
	stays("with a form typed in")
	p.Shot("48-timed-out-with-a-form")
	elsewhere(noticeLink)
	p.Wait("the notice gone: the page carries on", noNotice)
	stays("once another tab went through")
	is("the form lost what she typed", `__t.field("name").value === "laptop" && __t.field("public key").value.startsWith("ssh-ed25519 ")`)
	p.Submit()
	p.Wait("the key pair's page", "location.hash.startsWith('#/r/')")
	p.Sees("laptop")
	p.Quiet()
	// a form nobody touched keeps nothing — what was typed on another page
	// counts no more: the page is loaded anew
	p.Open("#/t/machine/new")
	p.Sees("ASK FOR A MACHINE")
	later()
	here()
	away()
	g.lapse()
	count()
	back()
	loaded("its form untouched", `__t.field("name")`)
	if h := p.Hash(); h != "#/t/machine/new" {
		t.Fatalf("loaded anew, the form's page is at %s", h)
	}

	// ---- her terminal in the whole window: nothing there asks the console
	// anything, so nothing is said while it is open — and when its socket is
	// cut, what stands in front says which ending it is
	p.Open("#/r/" + id + "/terminal")
	termState(p, "open")
	p.Wait("the keyboard the terminal's", termKeys)
	p.Type("echo on-the-screen", true)
	screenSays(p, "a mark on the screen", "\non-the-screen\nuser@box:~$")
	// cut, the gateway letting through: the connection was lost, and it opens again
	g.cut()
	termState(p, "cut")
	p.Sees("The connection was lost.")
	time.Sleep(500 * time.Millisecond)
	termState(p, "cut")
	p.Press("OPEN AGAIN")
	termState(p, "open")
	screenSays(p, "the screen kept across the cut", "\non-the-screen\nuser@box:~$")
	keys()
	p.Type("echo opened-again", true)
	screenSays(p, "typed on it again", "\nopened-again\nuser@box:~$")
	// cut past the gateway's time: timed out — the page is not loaded anew,
	// the screen is kept, and opening it again asks first: no socket is tried
	// that the gateway would turn back once more
	later()
	here()
	g.lapse()
	g.cut()
	termState(p, "timed out")
	p.Sees("a terminal cannot answer")
	p.Lacks("The connection was lost.")
	p.Shot("49-terminal-timed-out")
	was := openings(s)
	p.Press("OPEN AGAIN")
	g.wait(t, "her key's own question turned back", func() bool { return len(g.turned) >= 2 })
	time.Sleep(300 * time.Millisecond)
	termState(p, "timed out")
	for _, path := range g.turnedBack() {
		if path != "/console/session" {
			t.Fatalf("timed out, the terminal asked the gateway for more than a word: %v", g.turnedBack())
		}
	}
	stays("with a screen kept")
	elsewhere(termLink)
	p.Press("OPEN AGAIN")
	termState(p, "open")
	stays("its terminal opened again")
	screenSays(p, "the screen kept past the gateway's time", "\nopened-again\nuser@box:~$")
	keys()
	p.Type("echo and-again", true)
	screenSays(p, "typed on it once more", "\nand-again\nuser@box:~$")
	if n := openings(s) - was; n != 1 {
		t.Fatalf("turned back, then let through: the brain saw %d openings", n)
	}
	// …or by its own key: the page loaded anew, its terminal opened by its address
	g.lapse()
	g.cut()
	termState(p, "timed out")
	count()
	p.Press("RELOAD")
	loaded("by the terminal's key", termOpen)
	if h := p.Hash(); h != "#/r/"+id+"/terminal" {
		t.Fatalf("loaded anew, the terminal's page is at %s", h)
	}

	// ---- a terminal asked for past the gateway's time, on a page that has
	// asked nothing since: its opening is turned back, it holds nothing yet —
	// the page is loaded anew
	p.Open("#/")
	p.Sees("YOUR LIMITS")
	p.Open("#/r/" + id)
	p.Reads(stamp, "RUNNING")
	is("the machine's page shows a terminal that was let go of", `!document.querySelector('.term')`)
	later()
	here()
	away()
	g.lapse()
	count()
	p.Press("terminal")
	loaded("by a terminal's opening", machine)
	p.Reads(stamp, "RUNNING")
	// the socket first, then the question it led to — and nothing before them
	if first := g.turnedBack(); len(first) < 2 || !strings.HasSuffix(first[0], "/streams/terminal") || first[1] != "/console/session" {
		t.Fatalf("what the gateway turned back, a terminal asked for past its time: %v", first)
	}

	// ---- …and on a page that has shown no terminal yet: what draws one is
	// asked for first, and turned back as its socket would be — the page is
	// loaded anew all the same
	later()
	here()
	away()
	g.lapse()
	count()
	p.Press("terminal")
	loaded("by a terminal that could not be loaded", machine)
	p.Reads(stamp, "RUNNING")
	if first := g.turnedBack(); len(first) < 2 || !strings.HasSuffix(first[0], "/static/vendor/xterm.js") || first[1] != "/console/session" {
		t.Fatalf("what the gateway turned back, a first terminal asked for past its time: %v", first)
	}

	// ---- an address that names no resource asks the console nothing: what
	// a server answers to a path that is not one is a redirect too, and it is
	// not the gateway's
	here()
	p.Open("#/r/m-a%2F%2Fb/terminal")
	p.Sees("That is not an id.")
	stays("at an address that names nothing")

	// what the page complained of, as a browser reports it: the socket the
	// gateway turned back, and the gateway's sign-in refused as a script's
	// address by the page's own policy — each once, and nothing else
	var socket, script int
	for _, e := range p.Errors() {
		switch {
		case strings.Contains(e, "WebSocket") && strings.Contains(e, "Unexpected response code: 302"):
			socket++
		case strings.Contains(e, g.far.URL+"/authorize") && strings.Contains(e, "script-src 'self'"):
			script++
		default:
			t.Errorf("the page complained: %s", e)
		}
	}
	if socket != 1 || script != 1 {
		t.Errorf("the page complained of %d sockets turned back and %d scripts refused; one of each was", socket, script)
	}
}
