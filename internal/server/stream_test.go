package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tomblancdev/hangar/internal/identity"
	"github.com/tomblancdev/hangar/internal/testoidc"
)

const terminalConfig = `
data_dir: %[1]s
identity:
  oidc: {issuer: %[2]s, audience: hangar}
tiers:
  - name: operators
    groups: [ops]
    operator: true
    zones: ["*"]
    limits: {"*": unlimited}
  - name: users
    groups: [users]
    zones: [m, quiet]
    limits:
      machines.count: 8
      machines.vcpu_hours: 1000
      machines.vcpu: 32
      machines.memory_gb: 64
      machines.disk_gb: 256
      machines.key_pairs: 3
      machines.kind: [vm, container]
      machines.class: [spot]
zones:
  - {name: m, driver: fake, endpoint: "%[1]s/zone-m.json"}
  - name: quiet
    driver: fake
    options: {capabilities: "kind.vm,kind.container,guest.tags,fence.pool"}
plugins:
  - name: machines
    path: %[3]s
    args: [hangar-test-plugin, machines]
    zones: [m, quiet]
    settings:
      images:
        debian-13: {vm: debian-13, container: "store:vztmpl/debian-13.tar.zst"}
reconcile:
  every: 1h
`

// term is a terminal held open by a test: what the machine said so far, and
// how it ended.
type term struct {
	t    *testing.T
	conn *websocket.Conn
	said chan string
	end  chan websocket.CloseError
	seen string
}

// open asks for a resource's stream as a client would. A refusal is the
// reply (its code, its problem); an opening, the terminal.
func (s *stack) open(bearer, id, stream, query string) (*term, reply) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, s.url+"/v1/resources/"+id+"/streams/"+stream+query,
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + bearer}}})
	if err != nil {
		if resp == nil {
			s.t.Fatal(err)
		}
		out := reply{code: resp.StatusCode, hdr: resp.Header, body: map[string]any{}}
		b, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(b, &out.body)
		return nil, out
	}
	tm := &term{t: s.t, conn: conn, said: make(chan string, 256), end: make(chan websocket.CloseError, 1)}
	go func() {
		for {
			_, msg, err := conn.Read(context.Background())
			if err != nil {
				var ce websocket.CloseError
				errors.As(err, &ce)
				tm.end <- ce
				return
			}
			tm.said <- string(msg)
		}
	}()
	s.t.Cleanup(func() { _ = conn.CloseNow() })
	return tm, reply{code: 101}
}

// until reads what the machine says until it has said want.
func (tm *term) until(want string) string {
	tm.t.Helper()
	deadline := time.After(10 * time.Second)
	for !strings.Contains(tm.seen, want) {
		select {
		case s := <-tm.said:
			tm.seen += s
		case <-deadline:
			tm.t.Fatalf("the terminal never said %q; it said %q", want, tm.seen)
		}
	}
	out := tm.seen
	tm.seen = ""
	return out
}

func (tm *term) typ(keys string) {
	tm.t.Helper()
	if err := tm.conn.Write(context.Background(), websocket.MessageBinary, []byte(keys)); err != nil {
		tm.t.Fatal(err)
	}
}

// ended waits for the brain's close frame.
func (tm *term) ended() websocket.CloseError {
	tm.t.Helper()
	select {
	case ce := <-tm.end:
		return ce
	case <-time.After(10 * time.Second):
		tm.t.Fatal("the terminal was never closed")
		return websocket.CloseError{}
	}
}

// auditOf returns the audit lines of a stream's route that hold every one of
// the words.
func (s *stack) auditOf(words ...string) []string {
	var out []string
lines:
	for _, l := range strings.Split(s.logs.String(), "\n") {
		if !strings.Contains(l, `"kind":"audit"`) || !strings.Contains(l, "/streams/{stream}") {
			continue
		}
		for _, w := range words {
			if !strings.Contains(l, w) {
				continue lines
			}
		}
		out = append(out, l)
	}
	return out
}

// A machine's terminal: its own screen and keyboard, carried to its owner —
// and to nobody else. One born with no key pair is born open: its owner
// lands in a shell, nothing asked. An operator, who may stop and delete it,
// is refused; so is anyone else, and a token that only reads. The audit
// holds the opening and the closing, and not a byte of what passed.
func TestAMachinesTerminalIsItsOwnersAlone(t *testing.T) {
	s := newStackWith(t, terminalConfig, "machines")
	alice, bob, op := s.token("alice", "users"), s.token("bob", "users"), s.token("root", "ops")

	// the catalogue says a machine has a terminal, and where
	var streams string
	for _, ty := range s.do("GET", "/v1/types", alice, nil).body["types"].([]any) {
		if ty.(map[string]any)["name"] == "machine" {
			b, _ := json.Marshal(ty.(map[string]any)["streams"])
			streams = string(b)
		}
	}
	if !strings.Contains(streams, `"name":"terminal"`) || !strings.Contains(streams, `"zones":["m"]`) {
		t.Fatalf("the catalogue's streams of a machine: %s", streams)
	}

	r := s.create(alice, map[string]any{"type": "machine", "zone": "m", "name": "box", "spec": map[string]any{"image": "debian-13"}})
	id := r.str("resource", "id")
	if got := s.do("GET", "/v1/resources/"+id, alice, nil); got.str("spec", "terminal") != "open" {
		t.Fatalf("a VM that names no key pair is born with its terminal open: %v", got.body["spec"])
	}

	// nobody but its owner: someone else does not even see it, an operator
	// sees it and is refused its terminal, a read-only token is refused
	if _, r := s.open(bob, id, "terminal", ""); r.code != 404 {
		t.Fatalf("someone else's machine: %d %v", r.code, r.body)
	}
	if _, r := s.open(op, id, "terminal", ""); r.code != 403 || r.str("kind") != "owner" || !strings.Contains(r.str("detail"), "is alice's") {
		t.Fatalf("an operator at a machine's terminal: %d %v", r.code, r.body)
	}
	if _, r := s.open(s.readOnly("alice", "users"), id, "terminal", ""); r.code != 403 || r.str("kind") != "scope" {
		t.Fatalf("a read-only token at a terminal: %d %v", r.code, r.body)
	}
	if _, r := s.open(alice, id, "screen", ""); r.code != 404 || !strings.Contains(r.str("detail"), "it has: terminal") {
		t.Fatalf("a stream a machine does not have: %d %v", r.code, r.body)
	}
	// asked as an ordinary call, it says what it is
	if r := s.do("GET", "/v1/resources/"+id+"/streams/terminal", alice, nil); r.code != 426 || r.str("kind") != "websocket" {
		t.Fatalf("a stream asked without a WebSocket: %d %v", r.code, r.body)
	}
	// the operator can still stop it: their reach ends at its door, not before
	if r := s.do("POST", "/v1/resources/"+id+"/actions/reboot", op, map[string]any{}); r.code != 202 {
		t.Fatalf("an operator rebooting it: %d %v", r.code, r.body)
	} else {
		s.done(op, r.str("operation", "id"))
	}

	// its owner: in, as the machine's user, nothing asked
	tm, r := s.open(alice, id, "terminal", "?cols=132&rows=41")
	if tm == nil {
		t.Fatalf("its owner at its terminal: %d %v", r.code, r.body)
	}
	if got := tm.until("$ "); !strings.Contains(got, "(automatic login)") {
		t.Fatalf("a terminal born open greets with a shell: %q", got)
	}
	tm.typ("whoami\r")
	if got := tm.until("$ "); !strings.Contains(got, "\r\nuser\r\n") {
		t.Fatalf("whoami: %q", got)
	}
	// the window it opened with, and a window that changed
	tm.typ("stty size\r")
	if got := tm.until("$ "); !strings.Contains(got, "41 132") {
		t.Fatalf("the window as it opened: %q", got)
	}
	if err := tm.conn.Write(context.Background(), websocket.MessageText, []byte(`{"size":[100,30]}`)); err != nil {
		t.Fatal(err)
	}
	tm.typ("stty size\r")
	if got := tm.until("$ "); !strings.Contains(got, "30 100") {
		t.Fatalf("a window that changed: %q", got)
	}
	tm.typ("echo a-secret-word\r")
	tm.until("$ ")
	_ = tm.conn.Close(websocket.StatusNormalClosure, "")

	// the audit: the opening, then the closing — how long, how much, why;
	// and nothing of what passed
	deadline := time.Now().Add(5 * time.Second)
	for len(s.auditOf(`"result":"closed"`, id)) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	opened, closed := s.auditOf(`"result":"opened"`, id, `"actor":"alice"`, `"status":101`, `"stream":"terminal"`), s.auditOf(`"result":"closed"`, id, `"actor":"alice"`)
	if len(opened) != 1 || len(closed) != 1 {
		t.Fatalf("the audit holds one opening and one closing: %d and %d\n%s", len(opened), len(closed), strings.Join(s.auditOf(), "\n"))
	}
	if !strings.Contains(closed[0], `"detail":"closed by its owner"`) || !regexp.MustCompile(`"bytes_in":"[1-9]\d*"`).MatchString(closed[0]) ||
		!regexp.MustCompile(`"bytes_out":"[1-9]\d*"`).MatchString(closed[0]) || !strings.Contains(closed[0], `"seconds":"`) {
		t.Fatalf("the closing says how long, how much and why: %s", closed[0])
	}
	if strings.Contains(s.logs.String(), "a-secret-word") || strings.Contains(s.logs.String(), "stty size") {
		t.Fatal("something that was typed is in the brain's log")
	}
	if refused := s.auditOf(`"result":"refused"`, `"actor":"root"`, `"reason":"owner"`); len(refused) != 1 {
		t.Fatalf("the operator's refusal is one audit line: %d", len(refused))
	}
}

// A machine has one screen: opened a second time — another tab, a phone —
// the newer takes it and the older is told. A machine that stops ends its
// terminal, and says so; a stopped one opens none.
func TestATerminalIsOpenInOnePlaceAndEndsWithItsMachine(t *testing.T) {
	s := newStackWith(t, terminalConfig, "machines")
	alice := s.token("alice", "users")
	id := s.create(alice, map[string]any{"type": "machine", "zone": "m", "spec": map[string]any{"image": "debian-13"}}).str("resource", "id")

	first, _ := s.open(alice, id, "terminal", "")
	first.until("$ ")
	second, r := s.open(alice, id, "terminal", "")
	if second == nil {
		t.Fatalf("a second opening: %d %v", r.code, r.body)
	}
	if ce := first.ended(); ce.Code != StreamTaken || !strings.Contains(ce.Reason, "opened elsewhere") {
		t.Fatalf("the older of two openings is told it was taken: %d %q", ce.Code, ce.Reason)
	}
	second.until("$ ")
	second.typ("hostname\r")
	second.until("$ ")
	if n := s.core.Streams(); n != 1 {
		t.Fatalf("%d streams held open, of one machine's one terminal", n)
	}
	if taken := s.auditOf(`"result":"closed"`, `"reason":"taken"`); len(taken) != 1 {
		t.Fatalf("the first one's closing says it was taken: %d\n%s", len(taken), strings.Join(s.auditOf(), "\n"))
	}

	// an opening that could never be switched takes nothing from who holds it
	bad, _ := http.NewRequest("GET", s.url+"/v1/resources/"+id+"/streams/terminal", nil)
	bad.Header.Set("Authorization", "Bearer "+alice)
	bad.Header.Set("Connection", "Upgrade")
	bad.Header.Set("Upgrade", "websocket")
	if resp, err := http.DefaultClient.Do(bad); err != nil || resp.StatusCode != 400 {
		t.Fatalf("an opening with no key of its own: %v %v", resp, err)
	}
	second.typ("hostname\r")
	second.until("$ ")

	// rebooted under its terminal: the same socket, and the machine's new
	// boot on it — its owner watches it come back
	if r := s.do("POST", "/v1/resources/"+id+"/actions/reboot", alice, map[string]any{}); r.code != 202 {
		t.Fatalf("reboot: %d %v", r.code, r.body)
	} else {
		s.done(alice, r.str("operation", "id"))
	}
	if got := second.until("$ "); !strings.Contains(got, "(automatic login)") {
		t.Fatalf("a machine rebooted under its terminal greets again on it: %q", got)
	}
	second.typ("whoami\r")
	second.until("user\r\n")
	if n := s.core.Streams(); n != 1 {
		t.Fatalf("%d streams after a reboot, of one terminal", n)
	}

	// stopped under an open terminal
	stop := s.do("POST", "/v1/resources/"+id+"/actions/stop", alice, map[string]any{})
	if stop.code != 202 {
		t.Fatalf("stop: %d %v", stop.code, stop.body)
	}
	if ce := second.ended(); ce.Code != StreamEnded || ce.Reason != "the machine was stopped" {
		t.Fatalf("a machine stopped under its terminal: %d %q", ce.Code, ce.Reason)
	}
	s.done(alice, stop.str("operation", "id"))
	if _, r := s.open(alice, id, "terminal", ""); r.code != 409 || !strings.Contains(r.str("detail"), "is stopped: start it") {
		t.Fatalf("a stopped machine's terminal: %d %v", r.code, r.body)
	}
	if n := s.core.Streams(); n != 0 {
		t.Fatalf("%d streams left open after the machine stopped", n)
	}
}

// How a terminal greets is set at a machine's birth: open for a VM that
// names no key pair, a login asked of one that names a key — unless it says
// open. A container has none, and neither has a zone that opens no console:
// there the form does not offer it, and a machine is made as it always was.
func TestHowATerminalGreetsIsSetAtBirth(t *testing.T) {
	s := newStackWith(t, terminalConfig, "machines")
	alice := s.token("alice", "users")
	kp := s.create(alice, map[string]any{"type": "keypair", "zone": "m", "spec": map[string]any{"public_key": aliceKey}}).str("resource", "id")
	machine := func(zone string, spec map[string]any) reply {
		spec["image"] = "debian-13"
		return s.do("POST", "/v1/resources", alice, map[string]any{"type": "machine", "zone": zone, "spec": spec})
	}
	born := func(spec map[string]any) (string, string) {
		t.Helper()
		r := machine("m", spec)
		if r.code != 202 {
			t.Fatalf("%v: %d %v", spec, r.code, r.body)
		}
		s.done(alice, r.str("operation", "id"))
		id := r.str("resource", "id")
		return id, s.do("GET", "/v1/resources/"+id, alice, nil).str("spec", "terminal")
	}

	keyed, how := born(map[string]any{"key_pairs": []string{kp}})
	if how != "login" {
		t.Fatalf("a VM that names a key pair asks a login: %q", how)
	}
	tm, _ := s.open(alice, keyed, "terminal", "")
	if got := tm.until("login: "); strings.Contains(got, "automatic") {
		t.Fatalf("a terminal that asks: %q", got)
	}
	tm.typ("root\r")
	tm.until("Password: ")
	tm.typ("guess\r")
	tm.until("Login incorrect")

	if _, how := born(map[string]any{"key_pairs": []string{kp}, "terminal": "open"}); how != "open" {
		t.Fatalf("a keyed VM that asks to be open: %q", how)
	}
	if _, how := born(map[string]any{"terminal": "login"}); how != "login" {
		t.Fatalf("a keyless VM that asks a login: %q", how)
	}

	// a container: none, and one asked for is refused in words
	if r := machine("m", map[string]any{"kind": "container", "terminal": "open"}); r.code != 422 || r.str("violations", "0", "field") != "/terminal" ||
		!strings.Contains(r.str("detail"), "a container in zone m has no terminal") {
		t.Fatalf("a container asked a terminal: %d %v", r.code, r.body)
	}
	ct, how := born(map[string]any{"kind": "container"})
	if how != "<nil>" {
		t.Fatalf("a container says nothing of a terminal: %q", how)
	}
	if _, r := s.open(alice, ct, "terminal", ""); r.code != 422 || !strings.Contains(r.str("detail"), "a container in zone m has no terminal") {
		t.Fatalf("a container's terminal: %d %v", r.code, r.body)
	}

	// a zone that opens no console
	if r := machine("quiet", map[string]any{"terminal": "open"}); r.code != 422 || !strings.Contains(r.str("detail"), "a VM in zone quiet has no terminal") {
		t.Fatalf("a terminal asked where none is opened: %d %v", r.code, r.body)
	}
	r := machine("quiet", map[string]any{})
	if r.code != 202 {
		t.Fatalf("a machine where no terminal is opened: %d %v", r.code, r.body)
	}
	s.done(alice, r.str("operation", "id"))
	if _, r := s.open(alice, r.str("resource", "id"), "terminal", ""); r.code != 422 || r.str("kind") != "unavailable-here" {
		t.Fatalf("a terminal where none is opened: %d %v", r.code, r.body)
	}

	// set at its birth: a plan that names another is told so; one that names
	// none leaves it where it is
	plan := func(spec map[string]any) reply {
		spec["image"] = "debian-13"
		return s.do("POST", "/v1/resources/"+keyed+"/plan", alice, map[string]any{"spec": spec})
	}
	if p := plan(map[string]any{"key_pairs": []string{kp}, "terminal": "open"}); p.code != 200 || !strings.Contains(p.str("fixed", "0", "reason"), "it is login, and how a machine's terminal greets is set at its birth") {
		t.Fatalf("a plan that names another greeting: %d %v", p.code, p.body)
	}
	if p := plan(map[string]any{"key_pairs": []string{kp}}); p.code != 200 || p.body["fixed"] != nil {
		t.Fatalf("a plan that names none: %d %v", p.code, p.body)
	}
}

// A stream is held on a credential, and ends with it: what its opening asked
// is asked again while it is open. A token revoked ends what it opened, with
// the reason; a client may hand the next token before the last one ends —
// the same person's, or the stream is nobody's to hold. And the brain's own
// stop ends every stream, its closing written.
func TestAStreamEndsWithItsCredential(t *testing.T) {
	s := newStackWith(t, terminalConfig, "machines")
	ctx := context.Background()
	mint := func(who string) (secret, id string) {
		t.Helper()
		secret, tok, err := identity.Mint(ctx, s.store, who, "test", []string{"users"}, nil, time.Hour, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return secret, tok.ID
	}
	first, firstID := mint("alice")
	machine := s.create(first, map[string]any{"type": "machine", "zone": "m", "spec": map[string]any{"image": "debian-13"}}).str("resource", "id")

	// revoked under an open terminal
	tm, _ := s.open(first, machine, "terminal", "")
	tm.until("$ ")
	// the control: asked again several times over, a good token holds it
	time.Sleep(700 * time.Millisecond)
	tm.typ("whoami\r")
	tm.until("user\r\n")
	if err := s.store.RevokeToken(ctx, "alice", firstID); err != nil {
		t.Fatal(err)
	}
	if ce := tm.ended(); ce.Code != StreamEnded || !strings.Contains(ce.Reason, "the credential it was opened with ended") || !strings.Contains(ce.Reason, "was revoked") {
		t.Fatalf("a terminal whose token was revoked: %d %q", ce.Code, ce.Reason)
	}
	if closed := s.auditOf(`"result":"closed"`, "was revoked"); len(closed) != 1 {
		t.Fatalf("the closing says the token was revoked: %d", len(closed))
	}

	// the next token, handed before the last one ends: the stream is then
	// held on it
	second, secondID := mint("alice")
	third, thirdID := mint("alice")
	tm, _ = s.open(second, machine, "terminal", "")
	tm.until("$ ")
	renew, _ := json.Marshal(map[string]string{"token": third})
	if err := tm.conn.Write(ctx, websocket.MessageText, renew); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := s.store.RevokeToken(ctx, "alice", secondID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	tm.typ("whoami\r")
	tm.until("user\r\n")
	// …and someone else's token is not a renewal: the stream is nobody's
	bob, _ := mint("bob")
	theirs, _ := json.Marshal(map[string]string{"token": bob})
	if err := tm.conn.Write(ctx, websocket.MessageText, theirs); err != nil {
		t.Fatal(err)
	}
	if ce := tm.ended(); ce.Code != StreamEnded || ce.Reason != "that credential is not its opener's" {
		t.Fatalf("a terminal handed someone else's token: %d %q", ce.Code, ce.Reason)
	}
	if strings.Contains(s.logs.String(), third) || strings.Contains(s.logs.String(), bob) {
		t.Fatal("a token handed to a stream is in the brain's log")
	}
	_ = thirdID

	// a person taken out of every group: the next token the provider gives
	// them says so, and the stream held on it is closed
	tm, _ = s.open(third, machine, "terminal", "")
	tm.until("$ ")
	out, _ := json.Marshal(map[string]string{"token": s.iss.Token(t, testoidc.Claims{Subject: "alice", Name: "alice", Audience: "hangar"})})
	if err := tm.conn.Write(ctx, websocket.MessageText, out); err != nil {
		t.Fatal(err)
	}
	if ce := tm.ended(); ce.Code != StreamEnded || ce.Reason != "its opener is in no tier here any more" {
		t.Fatalf("a terminal whose person left every group: %d %q", ce.Code, ce.Reason)
	}

	// the brain's own stop: every stream ended, with the reason, its closing written
	tm, _ = s.open(third, machine, "terminal", "")
	tm.until("$ ")
	s.core.EndStreams("the brain is restarting")
	if ce := tm.ended(); ce.Code != StreamEnded || ce.Reason != "the brain is restarting" {
		t.Fatalf("a terminal at the brain's stop: %d %q", ce.Code, ce.Reason)
	}
	if closed := s.auditOf(`"result":"closed"`, "the brain is restarting"); len(closed) != 1 {
		t.Fatalf("the closing at the brain's stop: %d", len(closed))
	}
}
