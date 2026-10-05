package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/tomblancdev/hangar/internal/audit"
	"github.com/tomblancdev/hangar/internal/core"
	"github.com/tomblancdev/hangar/internal/identity"
)

// A stream — a machine's terminal — is the one call of the API that is not
// a request and its answer: GET /v1/resources/{id}/streams/{stream} is a
// WebSocket (RFC 6455), asked with the same bearer token as everything else.
//
// Who may open it, and that one is open at a time, is the core's (stream.go
// there). A refusal is an ordinary answer, before anything is switched: 403
// with its reason for anyone but the owner, 409 for a machine that is
// stopped. Once open:
//
//	client → brain   a binary message: what is typed, as it comes
//	                 a text message: {"size":[cols,rows]} — the window changed
//	brain → client   a binary message: what the machine says, as it comes
//	the end          a close frame from the brain — StreamEnded with why in
//	                 words (« the machine was stopped »), StreamTaken when
//	                 its owner opened it again elsewhere
//
// ?cols=&rows= on the address say the window as it opens. Two audit lines:
// the opening (101), and the closing, written by the core when either end
// lets go — how long, how many bytes each way, why. Nothing of what passed.
//
// A stream is held on a credential, and ends with it. What the opening
// asked is asked again every half minute — the same token, still good,
// still its opener's, still in a tier — and a stream whose answer is no is
// closed, with the reason: a revoked API token, a person taken out of every
// group, a provider's token that ended. A client whose token is short-lived
// sends the next one before that — a text message {"token":"…"} —, which
// must be the same person's.

const streamPattern = "GET /v1/resources/{id}/streams/{stream}"

// The close codes a stream ends with, in the range RFC 6455 leaves to
// applications.
const (
	// StreamEnded: over, from the resource's side; the reason says why.
	StreamEnded websocket.StatusCode = 4000
	// StreamTaken: its owner opened it again somewhere else.
	StreamTaken websocket.StatusCode = 4001
)

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	ctx, rec := audit.WithRecord(r.Context())
	r = r.WithContext(ctx)
	rw := &recorder{ResponseWriter: w, code: 200}
	rec.Set(func(e *audit.Event) { e.Action = streamPattern })
	said := false
	line := func() {
		if said {
			return
		}
		said = true
		e := rec.Event()
		e.Status = rw.code
		if e.Result == "" {
			e.Result = "ok"
			if rw.code >= 400 {
				e.Result = "error"
			}
		}
		s.audit.Write(e)
		s.metrics.Inc("hangar_http_requests_total", streamPattern, strconv.Itoa(rw.code))
	}
	defer line()

	who := s.caller(rw, r, rec)
	if who == nil {
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		rec.Set(func(e *audit.Event) { e.Result, e.Reason = "refused", core.KindUpgrade })
		rw.Header().Set("Upgrade", "websocket")
		writeProblem(rw, &core.Problem{Status: http.StatusUpgradeRequired, Kind: core.KindUpgrade,
			Detail: "a stream is a WebSocket: ask for it with Upgrade: websocket"})
		return
	}
	// an opening takes the stream from whoever holds it: one that could
	// never be switched is refused before it takes anything
	if r.Header.Get("Sec-WebSocket-Key") == "" || r.Header.Get("Sec-WebSocket-Version") != "13" || !r.ProtoAtLeast(1, 1) {
		rec.Set(func(e *audit.Event) { e.Result, e.Reason = "refused", core.KindBadRequest })
		writeProblem(rw, &core.Problem{Status: http.StatusBadRequest, Kind: core.KindBadRequest,
			Detail: "not a WebSocket's opening: it carries no Sec-WebSocket-Key, or not version 13"})
		return
	}
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	st, err := s.core.OpenStream(ctx, who, r.PathValue("id"), r.PathValue("stream"), cols, rows)
	if err != nil {
		var p *core.Problem
		if !errors.As(err, &p) {
			s.log.Error("request failed", "route", streamPattern, "err", err)
			rec.Set(func(e *audit.Event) { e.Result, e.Detail = "error", err.Error() })
			p = &core.Problem{Status: 500, Kind: core.KindInternal, Detail: "the brain failed to answer; the operator's logs say why"}
		}
		writeProblem(rw, p)
		return
	}
	// the opening, written now: it is open, whatever becomes of the socket —
	// and the closing is the core's, when it ends
	rw.code = http.StatusSwitchingProtocols
	line()
	// Whoever asks carries a bearer token, which no page of another site can
	// make a browser send on a WebSocket: there is no origin to check here.
	// The console — the door a browser does reach, on a cookie — checks its
	// own.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		st.Close("the connection could not be switched to a WebSocket")
		return
	}
	s.relays.Add(1)
	defer s.relays.Done()
	s.relay(conn, st, who, r.Header.Get("Authorization"))
}

// streamPing is how often the other end is asked whether it is still there:
// a phone that lost its network says nothing.
const streamPing = 30 * time.Second

// streamRecheck is how often what a stream was opened on is asked again.
const streamRecheck = 30 * time.Second

// stillMay asks again what a stream's opening asked: the credential still
// good, still its opener's, still one that writes, its person still in a
// tier. ok false: why, in words its owner reads. A provider that cannot be
// reached says nothing of a credential, and ends nothing.
func (s *Server) stillMay(ctx context.Context, credential string, who *core.Caller) (ok bool, why string) {
	req := (&http.Request{Header: http.Header{"Authorization": {credential}}}).WithContext(ctx)
	id, err := s.auth.Authenticate(req)
	if err != nil {
		var ie *identity.Error
		if errors.As(err, &ie) {
			return false, "the credential it was opened with ended: " + ie.Message
		}
		return true, ""
	}
	switch {
	case id.Subject != who.Subject:
		return false, "that credential is not its opener's"
	case !id.Can(identity.ScopeWrite):
		return false, "the credential it was opened with no longer writes"
	}
	if _, p := s.core.Caller(ctx, id); p != nil {
		return false, "its opener is in no tier here any more"
	}
	return true, ""
}

// relay passes a stream between the core and a socket until either lets go.
func (s *Server) relay(conn *websocket.Conn, st *core.Stream, who *core.Caller, credential string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// keys, a paste in pieces: nothing large comes this way
	conn.SetReadLimit(256 << 10)
	var mu sync.Mutex // the credential, which a client may renew

	// what the resource says → the client; and its end, in a close frame
	go func() {
		for {
			data, err := st.Recv()
			if err != nil {
				code, why := StreamEnded, err.Error()
				var end *core.StreamEnd
				if errors.As(err, &end) && end.Taken {
					code = StreamTaken
				}
				_ = conn.Close(code, closeReason(why))
				return
			}
			wctx, done := context.WithTimeout(ctx, 30*time.Second)
			err = conn.Write(wctx, websocket.MessageBinary, data)
			done()
			if err != nil {
				st.Close("the connection was lost")
				_ = conn.CloseNow()
				return
			}
		}
	}()
	go func() {
		ping, check := time.NewTicker(streamPing), time.NewTicker(s.recheck)
		defer ping.Stop()
		defer check.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-st.Done():
				return
			case <-check.C:
				mu.Lock()
				cred := credential
				mu.Unlock()
				cctx, done := context.WithTimeout(ctx, 20*time.Second)
				ok, why := s.stillMay(cctx, cred, who)
				done()
				if !ok {
					st.Close(why)
					return
				}
			case <-ping.C:
				pctx, done := context.WithTimeout(ctx, 15*time.Second)
				err := conn.Ping(pctx)
				done()
				if err != nil {
					_ = conn.CloseNow()
					return
				}
			}
		}
	}()
	// what is typed → the resource
	for {
		typ, msg, err := conn.Read(ctx)
		if err != nil {
			why := "the connection was lost"
			if c := websocket.CloseStatus(err); c == websocket.StatusNormalClosure || c == websocket.StatusGoingAway {
				why = "closed by its owner"
			}
			st.Close(why)
			return
		}
		switch typ {
		case websocket.MessageBinary:
			// a send that fails is a stream that ended: its close frame is on its way
			_ = st.Send(msg)
		case websocket.MessageText:
			var c struct {
				Size  []int  `json:"size"`
				Token string `json:"token"`
			}
			if json.Unmarshal(msg, &c) != nil {
				continue
			}
			if len(c.Size) == 2 {
				_ = st.Resize(c.Size[0], c.Size[1])
			}
			if c.Token != "" {
				// the next credential, before the last one ends: the same
				// person's, or the stream is no longer anyone's to hold
				next := "Bearer " + c.Token
				cctx, done := context.WithTimeout(ctx, 20*time.Second)
				ok, why := s.stillMay(cctx, next, who)
				done()
				if !ok {
					st.Close(why)
					continue
				}
				mu.Lock()
				credential = next
				mu.Unlock()
			}
		}
	}
}

// Drain waits — a few seconds at most — for the streams' relays to have
// said their last words: called as the brain stops, after the core ended
// them.
func (s *Server) Drain(most time.Duration) {
	done := make(chan struct{})
	go func() { s.relays.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(most):
	}
}

// closeReason fits a reason in a close frame: 123 bytes at most, cut between
// characters.
func closeReason(s string) string {
	const most = 123
	if len(s) <= most {
		return s
	}
	s = s[:most-3]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}
