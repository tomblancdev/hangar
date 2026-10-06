package console_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tomblancdev/hangar/internal/testoidc"
)

// socket is a WebSocket a test's browser opened at the console.
type socket struct {
	t    *testing.T
	conn *websocket.Conn
	got  chan [2]string // a message: its kind ("text", "binary"), its body
	end  chan websocket.CloseError
}

// open is the page opening a stream: the browser's cookie rides along, and
// the request says which page asks (origin; "" = the console's own).
func (b *browser) open(path, origin string) (*socket, *http.Response) {
	b.t.Helper()
	if origin == "" {
		origin = b.base
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, b.base+path, &websocket.DialOptions{HTTPClient: b.c, HTTPHeader: http.Header{"Origin": {origin}}})
	if err != nil {
		if resp == nil {
			b.t.Fatal(err)
		}
		return nil, resp
	}
	s := &socket{t: b.t, conn: conn, got: make(chan [2]string, 64), end: make(chan websocket.CloseError, 1)}
	go func() {
		for {
			typ, msg, err := conn.Read(context.Background())
			if err != nil {
				var ce websocket.CloseError
				errors.As(err, &ce)
				s.end <- ce
				return
			}
			kind := "binary"
			if typ == websocket.MessageText {
				kind = "text"
			}
			s.got <- [2]string{kind, string(msg)}
		}
	}()
	b.t.Cleanup(func() { _ = conn.CloseNow() })
	return s, resp
}

func (s *socket) send(data string) {
	s.t.Helper()
	if err := s.conn.Write(context.Background(), websocket.MessageBinary, []byte(data)); err != nil {
		s.t.Fatal(err)
	}
}

func (s *socket) next() [2]string {
	s.t.Helper()
	select {
	case m := <-s.got:
		return m
	case <-time.After(10 * time.Second):
		s.t.Fatal("nothing came")
		return [2]string{}
	}
}

// opened takes the console's own first word on a stream the brain opened:
// the page's socket is taken before the brain is asked, and « open » — and a
// machine's silence — is counted from this word, which comes before anything
// the resource says.
func (s *socket) opened() {
	s.t.Helper()
	if m := s.next(); m != [2]string{"text", `{"opened":true}`} {
		s.t.Fatalf("a stream the brain opened says so first, in the console's word: %q", m)
	}
}

func (s *socket) ended() websocket.CloseError {
	s.t.Helper()
	select {
	case ce := <-s.end:
		return ce
	case <-time.After(10 * time.Second):
		s.t.Fatal("the socket was never closed")
		return websocket.CloseError{}
	}
}

func box(t *testing.T, b *browser) string {
	t.Helper()
	a := b.ask("POST", "/console/api/v1/resources", map[string]any{"type": "box", "zone": "z", "spec": map[string]any{"kind": "container", "cores": 1, "memory_gb": 1}})
	if a.code != 202 {
		t.Fatalf("a box: %d %s", a.code, a.raw)
	}
	op := a.body["operation"].(map[string]any)["id"].(string)
	if done := b.get("/console/api/v1/operations/" + op + "?wait=20"); done.str("state") != "succeeded" {
		t.Fatalf("the box was not made: %s", done.raw)
	}
	return a.body["resource"].(map[string]any)["id"].(string)
}

var (
	bob  = &testoidc.Claims{Subject: "bob", Name: "bob", Groups: []string{"users"}}
	root = &testoidc.Claims{Subject: "root", Name: "root", Groups: []string{"ops"}}
)

// A stream through the console: the page opens a WebSocket on its cookie,
// the console opens the brain's with the person's own token, and what
// passes, passes as it came — inside the brain as on its own. The brain's
// refusals arrive IN the socket, where a page can read them; its endings
// arrive as the brain said them.
func TestAStreamThroughTheConsole(t *testing.T) {
	bothWays(t, withProvider, func(t *testing.T, d *door) {
		d.stack.Iss.SignedIn(alice)
		b := d.browser()
		b.signIn("")
		id := box(t, b)
		path := "/console/api/v1/resources/" + id + "/streams/echo"

		s, resp := b.open(path, "")
		if s == nil {
			t.Fatalf("its owner's stream: %s", resp.Status)
		}
		s.opened()
		s.send("hello")
		if m := s.next(); m != [2]string{"binary", "hello"} {
			t.Fatalf("what was sent comes back: %q", m)
		}
		// the audit names the person, as for every call passed on
		if logs := d.stack.Logs.String(); !strings.Contains(logs, `"actor":"alice"`) || !strings.Contains(logs, `"result":"opened"`) {
			t.Fatal("the brain's audit does not hold alice's opening")
		}

		// opened again — another tab: the first is told, in the brain's words
		again, _ := b.open(path, "")
		if ce := s.ended(); ce.Code != 4001 || !strings.Contains(ce.Reason, "opened elsewhere") {
			t.Fatalf("the older of two: %d %q", ce.Code, ce.Reason)
		}
		again.opened()
		// ended from the box's side: the reason, as the plugin gave it
		again.send("bye")
		if ce := again.ended(); ce.Code != 4000 || ce.Reason != "the box said bye" {
			t.Fatalf("an end from the resource's side: %d %q", ce.Code, ce.Reason)
		}

		// someone else: the socket opens, says the brain's refusal, and closes
		refused := func(who *testoidc.Claims, status float64, kind string) {
			t.Helper()
			d.stack.Iss.SignedIn(who)
			other := d.browser()
			other.signIn("")
			s, resp := other.open(path, "")
			if s == nil {
				t.Fatalf("%s: the socket itself was refused: %s", who.Subject, resp.Status)
			}
			m := s.next()
			var said struct {
				Refused map[string]any `json:"refused"`
			}
			if m[0] != "text" || json.Unmarshal([]byte(m[1]), &said) != nil || said.Refused["status"] != status || said.Refused["kind"] != kind {
				t.Fatalf("%s is told the brain's refusal in the socket: %q", who.Subject, m)
			}
			if ce := s.ended(); ce.Code != 4003 {
				t.Fatalf("%s: closed with %d", who.Subject, ce.Code)
			}
		}
		refused(bob, 404, "not-found")
		refused(root, 403, "owner")

		// asked as an ordinary call, it is passed on as one
		if a := b.get(path); a.code != 426 {
			t.Fatalf("a stream asked without a WebSocket: %d %s", a.code, a.raw)
		}
	})
}

// The console takes a socket only from its own pages. A cookie rides a
// WebSocket's opening like any request: another site's page asking is
// refused before anything is opened — and nobody without a sign-in gets one.
func TestAStreamOnlyFromTheConsolesOwnPage(t *testing.T) {
	bothWays(t, withProvider, func(t *testing.T, d *door) {
		d.stack.Iss.SignedIn(alice)
		b := d.browser()
		b.signIn("")
		path := "/console/api/v1/resources/" + box(t, b) + "/streams/echo"

		if s, resp := b.open(path, "https://elsewhere.example"); s != nil || resp.StatusCode != 403 {
			t.Fatalf("another site's page, with alice's cookie: opened=%v %v", s != nil, resp.Status)
		}
		if strings.Contains(d.stack.Logs.String(), `"result":"opened"`) {
			t.Fatal("the brain opened a stream for another site's page")
		}
		// the control: the same request from the console's own page
		if s, resp := b.open(path, ""); s == nil {
			t.Fatalf("the console's own page: %s", resp.Status)
		}
		if s, resp := d.browser().open(path, ""); s != nil || resp.StatusCode != 401 {
			t.Fatalf("nobody signed in: opened=%v %v", s != nil, resp.Status)
		}
	})
}

// What is open on a sign-in ends with it: signed out, the stream is closed
// and the page told why.
func TestAStreamEndsWithItsSignIn(t *testing.T) {
	bothWays(t, withProvider, func(t *testing.T, d *door) {
		d.stack.Iss.SignedIn(alice)
		b := d.browser()
		b.signIn("")
		s, _ := b.open("/console/api/v1/resources/"+box(t, b)+"/streams/echo", "")
		s.opened()
		s.send("a")
		s.next()
		if a := b.ask("POST", "/console/signout", nil); a.code != 204 {
			t.Fatalf("sign-out: %d", a.code)
		}
		if ce := s.ended(); ce.Code != 4002 || ce.Reason != "your sign-in ended" {
			t.Fatalf("a stream after its sign-out: %d %q", ce.Code, ce.Reason)
		}
		// and the brain let go of it too
		deadline := time.Now().Add(5 * time.Second)
		for d.stack.Core.Streams() != 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if n := d.stack.Core.Streams(); n != 0 {
			t.Fatalf("%d streams still open at the brain after the sign-out", n)
		}
	})
}

// While a stream is open the sign-in under it is looked at: the provider's
// token renewed before it ends, the new one handed to the brain — the
// stream outlives the token it was opened on — and a sign-in the provider no
// longer renews ends, its stream with it. Typing keeps nothing alive that
// the provider ended.
func TestAStreamIsHeldOnASignInThatIsLookedAt(t *testing.T) {
	d := onItsOwn(t, withProvider, "")
	d.stack.Iss.TokenTTL = 2 * time.Second // tokens that end at once: every look renews
	d.stack.Iss.SignedIn(alice)
	b := d.browser()
	b.signIn("")
	s, _ := b.open("/console/api/v1/resources/"+box(t, b)+"/streams/echo", "")
	s.opened()
	before := d.stack.Iss.Refreshes()
	// past the life of the token it was opened on, typing all the while
	for until := time.Now().Add(3 * time.Second); time.Now().Before(until); time.Sleep(200 * time.Millisecond) {
		s.send("still")
		if m := s.next(); m != [2]string{"binary", "still"} {
			t.Fatalf("a stream past its first token's life: %q", m)
		}
	}
	if n := d.stack.Iss.Refreshes() - before; n < 2 {
		t.Fatalf("the sign-in under an open stream was renewed %d times in 3 s", n)
	}
	if strings.Contains(d.stack.Logs.String(), "eyJ") || strings.Contains(d.logs(), "eyJ") {
		t.Fatal("a token handed on for a stream is in a log")
	}
	// the provider ends it (an account disabled)
	d.stack.Iss.Forget()
	if ce := s.ended(); ce.Code != 4002 || ce.Reason != "your sign-in ended" {
		t.Fatalf("a stream whose sign-in the provider ended: %d %q", ce.Code, ce.Reason)
	}
	if a := b.get("/console/api/v1/whoami"); a.code != 401 {
		t.Fatalf("the sign-in after the provider ended it: %d", a.code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for d.stack.Core.Streams() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := d.stack.Core.Streams(); n != 0 {
		t.Fatalf("%d streams still open at the brain", n)
	}
}
