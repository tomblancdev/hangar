package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// A stream — a machine's terminal — passed on like every other call: the
// page opens a WebSocket here, on its cookie, and the console opens the
// brain's with the person's own token and carries the messages between the
// two, each as it came. It decides nothing: who may open a stream is the
// brain's to say.
//
// Two things are the console's own, because a browser reaches it on a cookie.
// The socket is taken only from the console's own pages (its Origin is the
// console's address) — a cookie rides a WebSocket's opening as it rides any
// request, and nothing else stops another site's page from asking. And a
// page cannot read why an opening was refused: so the socket is taken
// first, the brain asked next, and a refusal is said inside it — a text
// message {"refused": the brain's problem} — before it is closed.
//
// A stream ends with its sign-in: signed out, or left unused too long,
// whatever was open on it closes. And while one is open the sign-in is
// looked at (Options.StreamCheck): the provider's token renewed when it is
// about to end and the new one passed on to the brain — which asks again,
// every half minute, what the opening asked —; a sign-in the provider no
// longer renews ends, and its streams with it. Typing is using the sign-in;
// it is not a way to keep one the provider has ended.

// The close codes the console adds to the brain's own (4000 ended, 4001
// taken elsewhere).
const (
	streamSignedOut websocket.StatusCode = 4002 // the sign-in ended
	streamRefused   websocket.StatusCode = 4003 // refused at its opening: the message before says why
)

// streamPing is how often the page's end is asked whether it is still there.
const streamPing = 30 * time.Second

func (c *Console) stream(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		c.api(w, r) // asked as an ordinary call: the brain answers what it is
		return
	}
	s := c.sessions.of(r)
	if s == nil {
		problem(w, 401, "sign-in", "sign in")
		return
	}
	opts := &websocket.AcceptOptions{}
	if c.base != nil {
		opts.OriginPatterns = []string{c.base.Host}
	}
	// refused here unless the page asking is the console's own
	page, err := websocket.Accept(w, r, opts)
	if err != nil {
		c.o.Log.Warn("console: a stream was not opened", "err", err.Error())
		return
	}
	defer page.CloseNow()
	// ends with this call — and not with the sign-in: a sign-in that ends is
	// SAID to the page (below), which a cancelled read could not do
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refuse := func(p *brainProblem) {
		wctx, done := context.WithTimeout(ctx, 10*time.Second)
		defer done()
		b, _ := json.Marshal(map[string]any{"refused": p})
		_ = page.Write(wctx, websocket.MessageText, b)
		_ = page.Close(streamRefused, cut(p.Detail))
	}
	brain, bearer, refusal := c.dialStream(ctx, r, s)
	if refusal != nil {
		refuse(refusal)
		return
	}
	defer brain.CloseNow()
	page.SetReadLimit(256 << 10)
	brain.SetReadLimit(1 << 20)

	// the brain → the page; its end is the page's end, in the same words
	go func() {
		defer cancel()
		for {
			typ, msg, err := brain.Read(ctx)
			if err != nil {
				var ce websocket.CloseError
				switch {
				case s.life.Err() != nil || ctx.Err() != nil:
					// the sign-in ended, or the page left: said elsewhere
				case errors.As(err, &ce):
					_ = page.Close(ce.Code, ce.Reason)
				default:
					_ = page.Close(websocket.StatusBadGateway, "the brain let go of it")
				}
				return
			}
			wctx, done := context.WithTimeout(ctx, 30*time.Second)
			err = page.Write(wctx, typ, msg)
			done()
			if err != nil {
				return
			}
		}
	}()
	// a phone that lost its network says nothing: ask; and a sign-in that
	// ended takes its streams with it
	go func() {
		t, check := time.NewTicker(streamPing), time.NewTicker(c.o.StreamCheck)
		defer t.Stop()
		defer check.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.life.Done():
				_ = page.Close(streamSignedOut, "your sign-in ended")
				_ = brain.Close(websocket.StatusGoingAway, "")
				return
			case <-check.C:
				// the token the brain holds this stream on, renewed before it
				// ends; a provider that no longer renews it ends the sign-in
				cctx, done := context.WithTimeout(ctx, 20*time.Second)
				next, err := s.bearer(cctx, c, false)
				done()
				var tr *transient
				switch {
				case err != nil && !errors.As(err, &tr) && ctx.Err() == nil:
					c.o.Log.Info("console: a sign-in ended", "kind", "console", "event", "ended", "subject", s.subject, "why", err.Error())
					c.sessions.drop(s) // said to the page at the next turn, above
				case err == nil && next != bearer:
					bearer = next
					b, _ := json.Marshal(map[string]string{"token": next})
					wctx, done := context.WithTimeout(ctx, 20*time.Second)
					_ = brain.Write(wctx, websocket.MessageText, b)
					done()
				}
			case <-t.C:
				if !c.sessions.alive(s) {
					continue // its life just ended: said above, at the next turn
				}
				pctx, done := context.WithTimeout(ctx, 15*time.Second)
				err := page.Ping(pctx)
				done()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	// the page → the brain
	var used time.Time
	for {
		typ, msg, err := page.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if errors.As(err, &ce) && (ce.Code == websocket.StatusNormalClosure || ce.Code == websocket.StatusGoingAway) {
				_ = brain.Close(ce.Code, "")
			}
			return
		}
		// typing is using the sign-in
		if now := c.o.Now(); now.Sub(used) > time.Minute {
			used = now
			c.sessions.touch(s)
		}
		wctx, done := context.WithTimeout(ctx, 30*time.Second)
		err = brain.Write(wctx, typ, msg)
		done()
		if err != nil {
			return
		}
	}
}

// dialStream opens the brain's end with the person's token — once more,
// fresh, when the brain refused the token itself.
func (c *Console) dialStream(ctx context.Context, r *http.Request, s *session) (*websocket.Conn, string, *brainProblem) {
	p := strings.TrimPrefix(r.URL.Path, Prefix+"/api")
	if r.URL.RawQuery != "" {
		p += "?" + r.URL.RawQuery
	}
	for attempt := 0; ; attempt++ {
		bearer, err := s.bearer(ctx, c, attempt > 0)
		if err != nil {
			var t *transient
			if errors.As(err, &t) {
				return nil, "", &brainProblem{Status: 503, Kind: "provider-unreachable", Detail: "the identity provider cannot be reached: your sign-in could not be renewed"}
			}
			c.sessions.drop(s)
			return nil, "", &brainProblem{Status: 401, Kind: "sign-in", Detail: "your sign-in ended: sign in again"}
		}
		conn, resp, err := websocket.Dial(ctx, c.o.BrainURL+p, &websocket.DialOptions{
			HTTPClient: c.o.Brain,
			HTTPHeader: http.Header{"Authorization": {"Bearer " + bearer}, "User-Agent": {"hangar-console/" + c.o.Version}},
		})
		if err == nil {
			return conn, bearer, nil
		}
		if resp == nil {
			c.o.Log.Warn("console: the brain cannot be reached", "err", err.Error())
			return nil, "", &brainProblem{Status: 502, Kind: "brain-unreachable", Detail: "the brain cannot be reached"}
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && s.refreshable() {
			continue
		}
		bp := &brainProblem{Status: resp.StatusCode}
		if resp.Body == nil || json.NewDecoder(resp.Body).Decode(bp) != nil || bp.Detail == "" {
			bp.Detail = "the brain answered " + resp.Status
		}
		if resp.StatusCode == http.StatusUnauthorized {
			c.sessions.drop(s)
			bp.Kind, bp.Detail = "sign-in", "your sign-in ended: sign in again"
		}
		return nil, "", bp
	}
}

// cut fits words in a close frame: 123 bytes at most, between characters.
func cut(s string) string {
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
