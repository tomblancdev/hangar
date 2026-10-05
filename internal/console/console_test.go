package console_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/console"
	"github.com/tomblancdev/hangar/internal/stacktest"
	"github.com/tomblancdev/hangar/internal/testoidc"
	"github.com/tomblancdev/hangar/ui"
)

func TestMain(m *testing.M) {
	stacktest.ServePlugins()
	os.Exit(m.Run())
}

const tiers = `
tiers:
  - name: operators
    groups: [ops]
    operator: true
    zones: ["*"]
    limits: {"*": unlimited}
  - name: users
    groups: [users]
    zones: [z]
    limits:
      toy.boxes: 2
      toy.cores: 4
      toy.memory_gb: 8
      toy.kind: [container]
zones:
  - name: z
    driver: fake
    endpoint: %[1]s/zone-z.json
    options: {capabilities: "kind.container,kind.vm,guest.tags"}
plugins:
  - name: toy
    path: %[3]s
    args: [hangar-test-plugin, toy]
    zones: [z]
reconcile:
  every: 1h
`

// withProvider: people sign in at the identity provider.
const withProvider = `
data_dir: %[1]s
identity:
  oidc: {issuer: %[2]s, audience: hangar}
` + tiers

// tokensOnly: a brain with no provider — API tokens made on its host.
const tokensOnly = `
data_dir: %[1]s
` + tiers

// ---- The door, both ways ------------------------------------------------------

// door is a console in front of a brain: inside the brain's own process, or
// a process of its own that reaches the brain over the network.
type door struct {
	t     *testing.T
	url   string // the console's address
	stack *stacktest.Stack
	logs  func() string // the console's log
	now   *clock        // on its own: the console's clock
}

type clock struct {
	mu sync.Mutex
	d  time.Duration
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return time.Now().Add(c.d) }
func (c *clock) Pass(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.d += d
}

// bothWays runs a test on the console inside the brain, then on its own.
func bothWays(t *testing.T, cfg string, f func(t *testing.T, d *door)) {
	t.Run("inside the brain", func(t *testing.T) {
		s := stacktest.New(t, cfg, "toy")
		f(t, &door{t: t, url: s.URL, stack: s, logs: s.Logs.String})
	})
	t.Run("on its own", func(t *testing.T) { f(t, onItsOwn(t, cfg, "")) })
}

// onItsOwn starts a console of its own in front of a brain's address.
func onItsOwn(t *testing.T, cfg, publicURL string) *door {
	t.Helper()
	s := stacktest.New(t, cfg, "toy")
	logs, now := &stacktest.SyncBuf{}, &clock{}
	c, err := console.New(console.Options{
		Brain: &http.Client{Timeout: 90 * time.Second}, BrainURL: s.URL, URL: publicURL,
		Static: ui.Console(), Mark: func() []byte { return ui.Still("") }, Version: "test",
		Log: slog.New(slog.NewJSONHandler(logs, nil)), Now: now.Now, Idle: time.Hour,
		StreamCheck: 100 * time.Millisecond, // a test does not wait twenty seconds
	})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(c.Handler())
	t.Cleanup(hs.Close)
	return &door{t: t, url: hs.URL, stack: s, logs: logs.String, now: now}
}

// ---- A browser ----------------------------------------------------------------

// browser is what a person's browser does: it keeps cookies, follows
// redirects, and — as the console's own page — sends the session's token
// with what changes something.
type browser struct {
	t    *testing.T
	c    *http.Client
	base string
	csrf string
}

func (d *door) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: d.t, base: d.url, c: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

type answer struct {
	code int
	body map[string]any
	raw  string
	resp *http.Response
}

func (a answer) str(k string) string {
	if v, ok := a.body[k].(string); ok {
		return v
	}
	return ""
}

func (b *browser) do(method, path string, body any, hdr map[string]string) answer {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, b.base+path, rd)
	if err != nil {
		b.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	a := answer{code: resp.StatusCode, raw: string(raw), resp: resp}
	_ = json.Unmarshal(raw, &a.body)
	return a
}

func (b *browser) get(path string) answer { b.t.Helper(); return b.do("GET", path, nil, nil) }

// ask is the app's own call: with the session's token in its header.
func (b *browser) ask(method, path string, body any) answer {
	b.t.Helper()
	return b.do(method, path, body, map[string]string{"X-Hangar-Csrf": b.csrf})
}

// signIn is the person pressing « sign in »: off to the provider, and back.
// It returns where the browser ended.
func (b *browser) signIn(next string) *url.URL {
	b.t.Helper()
	p := "/console/signin"
	if next != "" {
		p += "?next=" + url.QueryEscape(next)
	}
	a := b.get(p)
	if s := b.get("/console/session"); s.body["signed_in"] == true {
		b.csrf = s.str("csrf")
	}
	return a.resp.Request.URL
}

func (b *browser) cookie(name string) *http.Cookie {
	u, _ := url.Parse(b.base)
	for _, ck := range b.c.Jar.Cookies(u) {
		if ck.Name == name {
			return ck
		}
	}
	return nil
}

var alice = &testoidc.Claims{Subject: "alice", Name: "alice", Groups: []string{"users"}}

// ---- Signing in at the provider -------------------------------------------------

func TestSignInAtTheProvider(t *testing.T) {
	bothWays(t, withProvider, func(t *testing.T, d *door) {
		b := d.browser()

		// nobody yet: the app is told how one signs in here, and nothing else answers
		s := b.get("/console/session")
		if s.code != 200 || s.body["signed_in"] != false || s.str("how") != "provider" || s.str("provider") == "" {
			t.Fatalf("nobody signed in: %d %s", s.code, s.raw)
		}
		if a := b.get("/console/api/v1/whoami"); a.code != 401 || a.str("kind") != "sign-in" {
			t.Fatalf("the API without a sign-in: %d %s", a.code, a.raw)
		}

		// the provider knows only the address registered for the client
		d.stack.Iss.Redirects = []string{d.url + "/console/callback"}
		d.stack.Iss.SignedIn(alice)
		at := b.signIn("#/machine")
		if at.Path != "/console/" || at.Fragment != "/machine" {
			t.Fatalf("after the sign-in the browser is at %s", at)
		}
		if b.csrf == "" {
			t.Fatal("no session after the sign-in")
		}
		me := b.get("/console/api/v1/whoami")
		if me.code != 200 || me.str("subject") != "alice" || me.str("tier") != "users" || me.str("via") != "oidc" {
			t.Fatalf("whoami through the console: %d %s", me.code, me.raw)
		}

		// what the browser holds: one cookie no script reads, sent to no other site
		ck := b.cookie("hangar_console")
		if ck == nil {
			t.Fatal("no session cookie")
		}
		if s := b.get("/console/session"); strings.Contains(s.raw, "eyJ") || strings.Contains(s.raw, ck.Value) {
			t.Fatalf("the session's answer carries a token or the cookie: %s", s.raw)
		}

		// the brain saw alice herself, through her own token — and neither
		// log holds a token, a code or the cookie
		logs := d.stack.Logs.String() + d.logs()
		if !strings.Contains(d.stack.Logs.String(), `"action":"GET /v1/whoami","actor":"alice"`) {
			t.Error("the brain's audit does not name alice for the console's call")
		}
		if !strings.Contains(d.logs(), `"event":"signin","result":"ok","subject":"alice"`) {
			t.Errorf("the console did not log the sign-in: %s", d.logs())
		}
		for what, secret := range map[string]string{"the cookie": ck.Value, "a token": "eyJ", "the session's token": b.csrf} {
			if strings.Contains(logs, secret) {
				t.Errorf("a log holds %s", what)
			}
		}

		// signed in already: the sign-in page sends the browser home
		if at := b.get("/console/signin").resp.Request.URL; at.Path != "/console/" || d.stack.Iss.CodeAsks() != 1 {
			t.Fatalf("a second sign-in went to the provider again (%d), or to %s", d.stack.Iss.CodeAsks(), at)
		}
	})
}

// The cookie as a browser is told to keep it.
func TestTheCookie(t *testing.T) {
	read := func(t *testing.T, d *door) string {
		t.Helper()
		tok := d.stack.Token(t, "alice", "users")
		a := d.browser().do("POST", "/console/signin/token", map[string]string{"token": tok}, nil)
		if a.code != 200 {
			t.Fatalf("%d %s", a.code, a.raw)
		}
		return a.resp.Header.Get("Set-Cookie")
	}
	plain := read(t, onItsOwn(t, tokensOnly, ""))
	for _, want := range []string{"hangar_console=", "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(plain, want) {
			t.Errorf("the cookie lacks %s: %s", want, plain)
		}
	}
	if strings.Contains(plain, "Secure") {
		t.Errorf("a cookie marked Secure on plain http would never come back: %s", plain)
	}
	// behind TLS: a name a browser takes only over TLS, for this host alone
	tls := read(t, onItsOwn(t, tokensOnly, "https://hangar.example.org"))
	for _, want := range []string{"__Host-hangar_console=", "Secure", "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(tls, want) {
			t.Errorf("behind TLS the cookie lacks %s: %s", want, tls)
		}
	}
	if _, err := console.PublicURL("https://hangar.example.org/console"); err == nil {
		t.Error("an address with a path was taken")
	}
}

// What the provider or the brain refuses is said, and signs nobody in.
func TestASignInRefused(t *testing.T) {
	bothWays(t, withProvider, func(t *testing.T, d *door) {
		// the provider says no
		b := d.browser()
		d.stack.Iss.SignedIn(nil)
		at := b.signIn("")
		if q, _ := url.ParseQuery(strings.TrimPrefix(at.Fragment, "/signin?")); at.Path != "/console/" || q.Get("error") != "refused" {
			t.Fatalf("refused at the provider: the browser is at %s", at)
		}
		// the provider says yes, the brain knows no tier for them: its words
		d.stack.Iss.SignedIn(&testoidc.Claims{Subject: "eve", Name: "eve", Groups: []string{"visitors"}})
		at = b.signIn("")
		q, _ := url.ParseQuery(strings.TrimPrefix(at.Fragment, "/signin?"))
		if q.Get("error") != "no-tier" || q.Get("detail") == "" {
			t.Fatalf("no tier: the browser is at %s", at)
		}
		if b.csrf != "" || b.cookie("hangar_console") != nil {
			t.Fatal("a refused sign-in left a session")
		}
		if a := b.get("/console/api/v1/whoami"); a.code != 401 {
			t.Fatalf("after a refused sign-in: %d", a.code)
		}
		// the provider's answer is for another sign-in than the one begun here
		d.stack.Iss.SignedIn(alice)
		d.stack.Iss.OtherNonce = true
		at = b.signIn("")
		if q, _ := url.ParseQuery(strings.TrimPrefix(at.Fragment, "/signin?")); q.Get("error") != "refused" || b.csrf != "" {
			t.Fatalf("a token that answers another sign-in: the browser is at %s", at)
		}
		d.stack.Iss.OtherNonce = false
		// an address the provider does not know: it shows its own page, and
		// never sends the browser back
		d.stack.Iss.Redirects = []string{"https://elsewhere.example.org/console/callback"}
		d.stack.Iss.SignedIn(alice)
		if a := b.get("/console/signin"); a.code != 400 || !strings.Contains(a.raw, "redirect_uri mismatch") {
			t.Fatalf("an unregistered address: %d %s", a.code, a.raw)
		}
	})
}

// A sign-in finishes only in the browser that began it, and only once.
func TestTheBrowserThatAsked(t *testing.T) {
	bothWays(t, withProvider, func(t *testing.T, d *door) {
		d.stack.Iss.SignedIn(alice)
		// mallory begins a sign-in and stops at the provider's door
		mallory := d.browser()
		mallory.c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		begun := mallory.get("/console/signin")
		toProvider := begun.resp.Header.Get("Location")
		if begun.code != 303 || !strings.Contains(toProvider, "code_challenge=") || !strings.Contains(toProvider, "code_challenge_method=S256") ||
			!strings.Contains(toProvider, "state=") || !strings.Contains(toProvider, "nonce=") {
			t.Fatalf("the way to the provider: %d %s", begun.code, toProvider)
		}
		// …and hands the link to a victim: their browser comes back from the
		// provider without the cookie of the one that asked
		victim := d.browser()
		resp, err := victim.c.Get(toProvider)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		q, _ := url.ParseQuery(strings.TrimPrefix(resp.Request.URL.Fragment, "/signin?"))
		if q.Get("error") != "expired" || !strings.Contains(q.Get("detail"), "another browser") {
			t.Fatalf("a sign-in begun elsewhere: %s", resp.Request.URL)
		}
		if a := victim.get("/console/session"); a.body["signed_in"] != false {
			t.Fatal("the victim's browser was signed in by someone else's link")
		}
		// the state was spent by that attempt: mallory cannot finish it either
		mallory.c.CheckRedirect = nil
		resp, err = mallory.c.Get(toProvider)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if q, _ := url.ParseQuery(strings.TrimPrefix(resp.Request.URL.Fragment, "/signin?")); q.Get("error") != "expired" {
			t.Fatalf("a state used twice: %s", resp.Request.URL)
		}
		// a callback nobody began
		if a := d.browser().get("/console/callback?code=x&state=y"); a.resp.Request.URL.Fragment == "" || a.code != 200 {
			t.Fatalf("a callback nobody began: %d %s", a.code, a.resp.Request.URL)
		}
		// the way back after a sign-in is a place in the app, nowhere else
		b := d.browser()
		if at := b.signIn("https://elsewhere.example.org/"); at.Path != "/console/" || at.Fragment != "" || at.Host != mustHost(d.url) {
			t.Fatalf("a way back out of the app was followed: %s", at)
		}
	})
}

func mustHost(raw string) string { u, _ := url.Parse(raw); return u.Host }

// ---- The API through the console ------------------------------------------------

func TestTheAPIThroughTheConsole(t *testing.T) {
	bothWays(t, withProvider, func(t *testing.T, d *door) {
		d.stack.Iss.SignedIn(alice)
		b := d.browser()
		b.signIn("")
		box := map[string]any{"type": "box", "zone": "z", "spec": map[string]any{"cores": 1}}

		// what changes something carries the session's own token…
		if a := b.do("POST", "/console/api/v1/resources", box, nil); a.code != 403 || a.str("kind") != "cross-origin" {
			t.Fatalf("a create without the console's token: %d %s", a.code, a.raw)
		}
		if a := b.do("POST", "/console/api/v1/resources", box, map[string]string{"X-Hangar-Csrf": "guessed"}); a.code != 403 {
			t.Fatalf("a create with a guessed token: %d", a.code)
		}
		// …and never comes from another site's page, token or not
		if a := b.do("POST", "/console/api/v1/resources", box, map[string]string{"X-Hangar-Csrf": b.csrf, "Sec-Fetch-Site": "cross-site"}); a.code != 403 || a.str("kind") != "cross-origin" {
			t.Fatalf("a create from another site: %d %s", a.code, a.raw)
		}
		if a := b.do("POST", "/console/api/v1/resources", box, map[string]string{"X-Hangar-Csrf": b.csrf, "Origin": "https://elsewhere.example.org"}); a.code != 403 {
			t.Fatalf("a create from another origin: %d", a.code)
		}
		if n := len(b.get("/console/api/v1/resources").body["resources"].([]any)); n != 0 {
			t.Fatalf("a refused create made %d resources", n)
		}

		// the app's own call: the brain answers as it answers any door
		made := b.ask("POST", "/console/api/v1/resources", box)
		if made.code != 202 {
			t.Fatalf("create: %d %s", made.code, made.raw)
		}
		res := made.body["resource"].(map[string]any)
		id := res["id"].(string)
		if res["owner"] != "alice" {
			t.Fatalf("made for %v", res["owner"])
		}
		op := made.body["operation"].(map[string]any)["id"].(string)
		if a := b.get("/console/api/v1/operations/" + op + "?wait=20"); a.str("state") != "succeeded" {
			t.Fatalf("the create: %s", a.raw)
		}
		// its refusals too, with the numbers
		over := b.ask("POST", "/console/api/v1/resources", map[string]any{"type": "box", "zone": "z", "spec": map[string]any{"cores": 8}})
		if over.code != 403 || over.str("kind") != "limit" || over.resp.Header.Get("Content-Type") != "application/problem+json" || !strings.Contains(over.raw, "toy.cores") {
			t.Fatalf("over the limit: %d %s", over.code, over.raw)
		}
		resized := b.ask("POST", "/console/api/v1/resources/"+id+"/actions/resize", map[string]any{"params": map[string]any{"cores": 2}})
		if resized.code != 202 {
			t.Fatalf("an action: %d %s", resized.code, resized.raw)
		}
		// a wait held open through the console, as the app holds it
		if a := b.get("/console/api/v1/operations/" + resized.body["operation"].(map[string]any)["id"].(string) + "?wait=20"); a.str("state") != "succeeded" {
			t.Fatalf("the resize: %s", a.raw)
		}
		// an API token of her own, made from the console: she signed in at the provider
		if a := b.ask("POST", "/console/api/v1/tokens", map[string]any{"name": "ci", "expires_in": 3600}); a.code != 201 || !strings.HasPrefix(a.str("secret"), "hgr_") {
			t.Fatalf("a token: %d %s", a.code, a.raw)
		}
		if a := b.ask("DELETE", "/console/api/v1/resources/"+id+"?client_token=x", nil); a.code != 202 {
			t.Fatalf("delete: %d %s", a.code, a.raw)
		}

		// the console passes on the API's calls and nothing else of the brain's
		for _, p := range []string{"/console/api/metrics", "/console/api/healthz", "/console/api/v1/../metrics", "/console/api/v1/../../metrics", "/console/api/"} {
			if a := b.get(p); a.code != 404 {
				t.Errorf("%s: %d %s", p, a.code, a.raw[:min(len(a.raw), 80)])
			}
		}
		if a := b.get("/console/nothing"); a.code != 404 {
			t.Errorf("a path the console does not serve: %d", a.code)
		}
	})
}

// ---- A sign-in's life -----------------------------------------------------------

func TestTheTokenRenewed(t *testing.T) {
	bothWays(t, withProvider, func(t *testing.T, d *door) {
		// tokens that end at once: every call finds one about to end
		d.stack.Iss.TokenTTL = 2 * time.Second
		d.stack.Iss.SignedIn(alice)
		b := d.browser()
		b.signIn("")
		for i := range 3 {
			if a := b.get("/console/api/v1/whoami"); a.code != 200 {
				t.Fatalf("call %d after the token ended: %d %s", i, a.code, a.raw)
			}
		}
		if n := d.stack.Iss.Refreshes(); n != 3 {
			t.Fatalf("renewed %d times for 3 calls", n)
		}
		// many calls at once: each refresh token is good once — one renewal
		// at a time, or all but the first would end the sign-in
		var wg sync.WaitGroup
		codes := make([]int, 12)
		for i := range codes {
			wg.Add(1)
			go func() { defer wg.Done(); codes[i] = b.get("/console/api/v1/whoami").code }()
		}
		wg.Wait()
		for i, c := range codes {
			if c != 200 {
				t.Fatalf("call %d of 12 at once: %d (%v)", i, c, codes)
			}
		}

		// the provider ends it (an account disabled): said, and forgotten
		d.stack.Iss.Forget()
		a := b.get("/console/api/v1/whoami")
		if a.code != 401 || a.str("kind") != "sign-in" || !strings.Contains(a.str("detail"), "ended") {
			t.Fatalf("after the provider forgot: %d %s", a.code, a.raw)
		}
		if s := b.get("/console/session"); s.body["signed_in"] != false || b.cookie("hangar_console") != nil {
			t.Fatalf("an ended sign-in is still kept: %s", s.raw)
		}
	})
}

func TestSigningOut(t *testing.T) {
	bothWays(t, withProvider, func(t *testing.T, d *door) {
		d.stack.Iss.SignedIn(alice)
		b := d.browser()
		b.signIn("")
		if a := b.do("POST", "/console/signout", nil, nil); a.code != 403 {
			t.Fatalf("a sign-out without the console's token: %d", a.code)
		}
		if a := b.get("/console/api/v1/whoami"); a.code != 200 {
			t.Fatalf("a refused sign-out signed alice out: %d", a.code)
		}
		if a := b.ask("POST", "/console/signout", nil); a.code != 204 {
			t.Fatalf("sign-out: %d %s", a.code, a.raw)
		}
		if n := d.stack.Iss.Revoked(); n != 1 {
			t.Fatalf("the provider was told of %d sign-outs", n)
		}
		if a := b.get("/console/api/v1/whoami"); a.code != 401 || b.cookie("hangar_console") != nil {
			t.Fatalf("after the sign-out: %d", a.code)
		}
		if !strings.Contains(d.logs(), `"event":"signout","subject":"alice"`) {
			t.Error("the console did not log the sign-out")
		}
		// the sign-in page does not send the browser back in by itself
		if s := b.get("/console/session"); s.body["signed_in"] != false || s.str("how") != "provider" {
			t.Fatalf("after the sign-out: %s", s.raw)
		}
	})
}

// A sign-in left unused ends by itself.
func TestASignInLeftUnused(t *testing.T) {
	d := onItsOwn(t, withProvider, "")
	d.stack.Iss.SignedIn(alice)
	b := d.browser()
	b.signIn("")
	d.now.Pass(50 * time.Minute)
	if a := b.get("/console/api/v1/whoami"); a.code != 200 {
		t.Fatalf("50 minutes on, of an hour: %d", a.code)
	}
	// used: the hour starts again
	d.now.Pass(50 * time.Minute)
	if a := b.get("/console/api/v1/whoami"); a.code != 200 {
		t.Fatalf("50 minutes after its last use: %d", a.code)
	}
	d.now.Pass(61 * time.Minute)
	if a := b.get("/console/api/v1/whoami"); a.code != 401 {
		t.Fatalf("an hour unused: %d", a.code)
	}
}

// ---- Without a provider: an API token ---------------------------------------------

func TestSignInWithAToken(t *testing.T) {
	bothWays(t, tokensOnly, func(t *testing.T, d *door) {
		b := d.browser()
		if s := b.get("/console/session"); s.body["signed_in"] != false || s.str("how") != "token" {
			t.Fatalf("a brain without a provider: %s", s.raw)
		}
		// the provider's way is not offered
		if at := b.get("/console/signin").resp.Request.URL; !strings.Contains(at.Fragment, "error=no-provider") {
			t.Fatalf("the provider's sign-in on a brain without one: %s", at)
		}
		tok := d.stack.Token(t, "alice", "users")
		if a := b.do("POST", "/console/signin/token", map[string]string{"token": "hgr_nope"}, nil); a.code != 401 || a.str("kind") != "sign-in" {
			t.Fatalf("a token the brain does not know: %d %s", a.code, a.raw)
		}
		if a := b.do("POST", "/console/signin/token", map[string]string{"token": "eyJhbGciOi"}, nil); a.code != 400 {
			t.Fatalf("something that is no API token: %d", a.code)
		}
		// a form posted from another site's page cannot sign this browser in
		req, _ := http.NewRequest("POST", d.url+"/console/signin/token", strings.NewReader("token="+tok))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := b.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 415 {
			t.Fatalf("a form's post: %d", resp.StatusCode)
		}
		if a := b.do("POST", "/console/signin/token", map[string]string{"token": tok}, map[string]string{"Sec-Fetch-Site": "cross-site"}); a.code != 403 {
			t.Fatalf("a sign-in from another site: %d", a.code)
		}

		a := b.do("POST", "/console/signin/token", map[string]string{"token": tok}, nil)
		if a.code != 200 || a.body["signed_in"] != true || a.str("csrf") == "" || strings.Contains(a.raw, tok) {
			t.Fatalf("the token's sign-in: %d %s", a.code, a.raw)
		}
		b.csrf = a.str("csrf")
		me := b.get("/console/api/v1/whoami")
		if me.str("subject") != "alice" || !strings.HasPrefix(me.str("via"), "token:") {
			t.Fatalf("whoami: %s", me.raw)
		}
		// a token cannot make tokens — through the console as anywhere
		if a := b.ask("POST", "/console/api/v1/tokens", map[string]any{"name": "x", "expires_in": 3600}); a.code != 403 {
			t.Fatalf("a token made a token: %d", a.code)
		}
		if strings.Contains(d.stack.Logs.String()+d.logs(), tok) {
			t.Error("a log holds the API token")
		}
		if a := b.ask("POST", "/console/signout", nil); a.code != 204 {
			t.Fatalf("sign-out: %d", a.code)
		}
		if a := b.get("/console/api/v1/whoami"); a.code != 401 {
			t.Fatalf("after the sign-out: %d", a.code)
		}
	})
	// where people sign in at a provider, a pasted token is not a way in
	bothWays(t, withProvider, func(t *testing.T, d *door) {
		tok := d.stack.Token(t, "alice", "users")
		if a := d.browser().do("POST", "/console/signin/token", map[string]string{"token": tok}, nil); a.code != 403 {
			t.Fatalf("a pasted token beside a provider: %d %s", a.code, a.raw)
		}
	})
}

// ---- The page -------------------------------------------------------------------

func TestThePage(t *testing.T) {
	bothWays(t, tokensOnly, func(t *testing.T, d *door) {
		b := d.browser()
		page := b.get("/console/")
		if page.code != 200 || !strings.Contains(page.raw, `src="static/js/main.js"`) {
			t.Fatalf("the page: %d", page.code)
		}
		h := page.resp.Header
		csp := h.Get("Content-Security-Policy")
		// a terminal asked nothing of it: no inline style, and the one socket
		// it may open is the console's own
		ws := "connect-src 'self' ws://" + mustHost(d.url) + ";"
		for _, want := range []string{"default-src 'none'", "script-src 'self';", "style-src 'self';", ws, "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("the page's policy lacks %s: %s", want, csp)
			}
		}
		if strings.Contains(csp, "unsafe") {
			t.Errorf("the page's policy allows inline code: %s", csp)
		}
		if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("the page's headers: %v", h)
		}
		// the page itself holds no inline script or style: its policy would stop them
		for _, bad := range []string{"<style", " style=", " onclick=", "<script>"} {
			if strings.Contains(page.raw, bad) {
				t.Errorf("the page holds %q", bad)
			}
		}
		// its files: tagged, answered 304 while unchanged
		js := b.get("/console/static/js/main.js")
		if js.code != 200 || !strings.HasPrefix(js.resp.Header.Get("Content-Type"), "text/javascript") || js.resp.Header.Get("ETag") == "" {
			t.Fatalf("the app's script: %d %v", js.code, js.resp.Header)
		}
		if a := b.do("GET", "/console/static/js/main.js", nil, map[string]string{"If-None-Match": js.resp.Header.Get("ETag")}); a.code != 304 {
			t.Fatalf("an unchanged file asked again: %d", a.code)
		}
		if a := b.get("/console/static/../index.html"); a.code == 200 && strings.Contains(a.raw, "hgr_") {
			t.Fatal("a path out of the app's files")
		}
		if a := b.get("/console/static/nope.js"); a.code != 404 {
			t.Fatalf("a file that is not there: %d", a.code)
		}
		mark := b.get("/console/mark.svg")
		if mark.code != 200 || !strings.Contains(mark.raw, "<svg") || !strings.Contains(mark.resp.Header.Get("Content-Security-Policy"), "style-src 'unsafe-inline'") {
			t.Fatalf("the mark: %d %s", mark.code, mark.resp.Header.Get("Content-Security-Policy"))
		}
		// every answer that is not a file is kept nowhere
		if cc := b.get("/console/session").resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("the session's answer may be cached: %s", cc)
		}
	})
}

// A token the brain stops taking ends the sign-in it was.
func TestARevokedToken(t *testing.T) {
	bothWays(t, tokensOnly, func(t *testing.T, d *door) {
		tok := d.stack.Token(t, "alice", "users")
		b := d.browser()
		if a := b.do("POST", "/console/signin/token", map[string]string{"token": tok}, nil); a.code != 200 {
			t.Fatalf("sign-in: %d %s", a.code, a.raw)
		}
		toks, err := d.stack.Store.Tokens(context.Background(), "alice")
		if err != nil || len(toks) != 1 {
			t.Fatalf("alice's tokens: %v %v", toks, err)
		}
		if err := d.stack.Store.RevokeToken(context.Background(), "alice", toks[0].ID); err != nil {
			t.Fatal(err)
		}
		a := b.get("/console/api/v1/whoami")
		if a.code != 401 || a.str("kind") != "sign-in" {
			t.Fatalf("after the token was revoked: %d %s", a.code, a.raw)
		}
		if s := b.get("/console/session"); s.body["signed_in"] != false {
			t.Fatalf("a sign-in the brain no longer takes is still kept: %s", s.raw)
		}
	})
}

// One person's renewal, held up at the provider, holds up nobody else.
func TestASlowRenewalIsItsOwn(t *testing.T) {
	bothWays(t, withProvider, func(t *testing.T, d *door) {
		// alice's token ends at once; bob's lives an hour
		d.stack.Iss.TokenTTL = 2 * time.Second
		d.stack.Iss.SignedIn(alice)
		a := d.browser()
		a.signIn("")
		d.stack.Iss.TokenTTL = time.Hour
		d.stack.Iss.SignedIn(&testoidc.Claims{Subject: "bob", Name: "bob", Groups: []string{"users"}})
		b := d.browser()
		b.signIn("")
		d.stack.Iss.SlowRefresh(3 * time.Second)

		// alice's page asks twice at once: her first call renews, her second
		// waits for it — and it is while she waits that nobody else may
		slow := make(chan int, 2)
		go func() { slow <- a.get("/console/api/v1/whoami").code }()
		time.Sleep(150 * time.Millisecond) // her renewal is at the provider now
		go func() { slow <- a.get("/console/api/v1/whoami").code }()
		time.Sleep(150 * time.Millisecond)
		start := time.Now()
		if got := b.get("/console/api/v1/whoami"); got.code != 200 || got.str("subject") != "bob" {
			t.Fatalf("bob: %d %s", got.code, got.raw)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("bob's call waited %s behind alice's renewal", took.Round(time.Millisecond))
		}
		for range 2 {
			if code := <-slow; code != 200 {
				t.Fatalf("alice's call, once renewed: %d", code)
			}
		}
	})
}

// Sign-ins begun and left there never close the door: the oldest gives way.
func TestSignInsBegunAndLeft(t *testing.T) {
	d := onItsOwn(t, withProvider, "")
	d.stack.Iss.SignedIn(alice)
	first := d.browser()
	first.c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	toProvider := first.get("/console/signin").resp.Header.Get("Location")
	flood := d.browser()
	flood.c.CheckRedirect = first.c.CheckRedirect
	for range 1000 {
		if a := flood.get("/console/signin"); a.code != 303 || !strings.Contains(a.resp.Header.Get("Location"), "code_challenge") {
			t.Fatalf("a sign-in begun: %d %s", a.code, a.resp.Header.Get("Location"))
		}
	}
	// someone arriving now signs in
	b := d.browser()
	if at := b.signIn(""); at.Path != "/console/" || b.csrf == "" {
		t.Fatalf("after a thousand sign-ins left there, a new one ends at %s", at)
	}
	// the oldest of them gave way: it finishes nowhere
	first.c.CheckRedirect = nil
	resp, err := first.c.Get(toProvider)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if q, _ := url.ParseQuery(strings.TrimPrefix(resp.Request.URL.Fragment, "/signin?")); q.Get("error") != "expired" {
		t.Fatalf("the sign-in that gave way: %s", resp.Request.URL)
	}
}
