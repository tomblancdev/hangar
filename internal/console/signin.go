package console

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tomblancdev/hangar/internal/provider"
)

// ---- Signing in ---------------------------------------------------------------
//
// With a provider: the authorization code flow, with the brain's own client
// id — the public client the command line signs in with (no secret: the
// console proves it is the one that asked by PKCE, RFC 7636). The browser is
// sent to the provider's own page; the password, the second factor and the
// passkey never pass through here. It comes back with a code the console
// trades for tokens, server to server, and the console asks the brain who
// that is — a sign-in the brain refuses (no tier) is said at once.
//
// Without one: an API token, pasted.

const (
	signinCookie = "hangar_signin"
	maxPending   = 1000
	pendingFor   = 10 * time.Minute
)

// pending is a sign-in begun: what the callback needs to finish it.
type pending struct {
	verifier string
	nonce    string
	next     string
	redirect string
	issuer   string
	client   string
	binder   string // the hash of the browser's own cookie: the one that asked comes back
	at       time.Time
}

type pendings struct {
	mu sync.Mutex
	m  map[string]*pending
}

// signinInfo is what the brain says of where people sign in (GET /v1/signin).
type signinInfo struct {
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes"`
}

// how asks the brain where people sign in. Nil, nil: it has no provider.
func (c *Console) how(r *http.Request) (*signinInfo, error) {
	resp, err := c.ask(r, http.MethodGet, "/v1/signin", "", nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	var info signinInfo
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &info) != nil || info.Issuer == "" || info.ClientID == "" {
		return nil, &transient{errString("the brain did not say where people sign in: " + resp.Status)}
	}
	return &info, nil
}

type errString string

func (e errString) Error() string { return string(e) }

// session tells the app whether someone is signed in, and — when nobody is —
// how one signs in here. It hands the token every changing request must
// carry: readable by the console's own page only.
func (c *Console) session(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"version": c.o.Version}
	if s := c.sessions.of(r); s != nil {
		out["signed_in"], out["csrf"], out["via"] = true, s.csrf, "provider"
		if s.issuer == "" {
			out["via"] = "token"
		}
		writeJSON(w, out)
		return
	}
	out["signed_in"] = false
	info, err := c.how(r)
	switch {
	case err != nil:
		c.o.Log.Warn("console: the brain cannot be reached", "err", err.Error())
		problem(w, 502, "brain-unreachable", "the brain cannot be reached")
		return
	case info == nil:
		out["how"] = "token"
	default:
		out["how"] = "provider"
		if u, err := url.Parse(info.Issuer); err == nil {
			out["provider"] = u.Host
		}
	}
	writeJSON(w, out)
}

// local keeps a place to come back to inside the app: a fragment, nothing
// that could send the browser elsewhere.
func local(next string) string {
	if strings.HasPrefix(next, "#/") && len(next) <= 512 {
		return next
	}
	return ""
}

func (c *Console) home(next string) string { return Prefix + "/" + local(next) }

// back sends the browser to the app's sign-in page with why it failed. The
// words ride in the fragment: no server and no log ever sees them.
func (c *Console) back(w http.ResponseWriter, r *http.Request, code, detail string) {
	q := url.Values{"error": {code}}
	if detail != "" {
		q.Set("detail", detail)
	}
	http.Redirect(w, r, Prefix+"/#/signin?"+q.Encode(), http.StatusSeeOther)
}

// signin begins the provider's flow: the browser leaves for the provider's
// own page.
func (c *Console) signin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if s := c.sessions.of(r); s != nil {
		http.Redirect(w, r, c.home(next), http.StatusSeeOther)
		return
	}
	info, err := c.how(r)
	if err != nil {
		c.back(w, r, "brain", "the brain cannot be reached")
		return
	}
	if info == nil {
		c.back(w, r, "no-provider", "")
		return
	}
	d, err := c.provider.get(r.Context(), info.Issuer)
	if err != nil {
		c.o.Log.Warn("console: the identity provider cannot be reached", "err", err.Error())
		c.back(w, r, "provider", "the identity provider cannot be reached")
		return
	}
	if d.Authorization == "" {
		c.back(w, r, "provider", "the identity provider names no page to sign in at")
		return
	}
	p := &pending{
		verifier: random(48), nonce: random(24), next: local(next),
		redirect: c.address(r).String() + Prefix + "/callback",
		issuer:   info.Issuer, client: info.ClientID, at: c.o.Now(),
	}
	binder, state := random(24), random(24)
	p.binder = hash(binder)
	ps := c.begun
	ps.mu.Lock()
	for k, old := range ps.m {
		if c.o.Now().Sub(old.at) > pendingFor {
			delete(ps.m, k)
		}
	}
	// full: the oldest gives way — a flood of sign-ins begun and left there
	// must not close the door on everyone for ten minutes
	for len(ps.m) >= maxPending {
		var oldest string
		for k, old := range ps.m {
			if oldest == "" || old.at.Before(ps.m[oldest].at) {
				oldest = k
			}
		}
		delete(ps.m, oldest)
	}
	ps.m[state] = p
	ps.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name: signinCookie, Value: binder, Path: Prefix + "/", MaxAge: int(pendingFor.Seconds()),
		HttpOnly: true, Secure: c.secure(r),
		// Lax: it must come back with the browser as the provider returns it
		SameSite: http.SameSiteLaxMode,
	})
	sum := sha256.Sum256([]byte(p.verifier))
	q := url.Values{
		"response_type": {"code"}, "client_id": {info.ClientID}, "redirect_uri": {p.redirect},
		"scope": {strings.Join(info.Scopes, " ")}, "state": {state}, "nonce": {p.nonce},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(d.Authorization, "?") {
		sep = "&"
	}
	http.Redirect(w, r, d.Authorization+sep+q.Encode(), http.StatusSeeOther)
}

// callback finishes it: the code traded for tokens, the brain asked who
// that is, the sign-in kept.
func (c *Console) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ps := c.begun
	ps.mu.Lock()
	p := ps.m[q.Get("state")]
	delete(ps.m, q.Get("state")) // good once, whatever comes next
	ps.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: signinCookie, Value: "", Path: Prefix + "/", MaxAge: -1, HttpOnly: true, Secure: c.secure(r), SameSite: http.SameSiteLaxMode})

	ck, _ := r.Cookie(signinCookie)
	switch {
	case p == nil || c.o.Now().Sub(p.at) > pendingFor:
		c.back(w, r, "expired", "that sign-in was not begun here, or took too long: sign in again")
		return
	case ck == nil || subtle.ConstantTimeCompare([]byte(hash(ck.Value)), []byte(p.binder)) != 1:
		// begun in another browser: a link someone sent must never sign this one in
		c.back(w, r, "expired", "that sign-in was begun in another browser: sign in again")
		return
	case q.Get("error") != "":
		c.o.Log.Info("console: a sign-in refused at the provider", "kind", "console", "event", "signin", "result", "refused", "why", q.Get("error"))
		c.back(w, r, "refused", "the sign-in was refused at the identity provider")
		return
	case q.Get("code") == "":
		c.back(w, r, "refused", "the identity provider sent no code back")
		return
	}
	d, err := c.provider.get(r.Context(), p.issuer)
	if err != nil {
		c.back(w, r, "provider", "the identity provider cannot be reached")
		return
	}
	t, err := provider.PostForm(r.Context(), c.o.HTTP, d.Token, url.Values{
		"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "redirect_uri": {p.redirect},
		"client_id": {p.client}, "code_verifier": {p.verifier},
	})
	if err != nil {
		c.o.Log.Warn("console: the identity provider cannot be reached", "err", err.Error())
		c.back(w, r, "provider", "the identity provider cannot be reached")
		return
	}
	if err := t.Refused(); err != nil {
		c.o.Log.Info("console: a code refused by the provider", "kind", "console", "event", "signin", "result", "refused", "why", err.Error())
		c.back(w, r, "refused", "the identity provider refused the sign-in")
		return
	}
	// The ID token came straight from the provider, over the console's own
	// connection: what is checked here is that it answers THIS sign-in (its
	// nonce). Its signature, issuer, audience and expiry are the brain's to
	// check — it does, at the call below and at every call after.
	seen, ok := provider.Peek(t.IDToken)
	if !ok || subtle.ConstantTimeCompare([]byte(seen.Nonce), []byte(p.nonce)) != 1 {
		c.o.Log.Warn("console: a token that answers another sign-in", "kind", "console", "event", "signin", "result", "refused", "why", "nonce")
		c.back(w, r, "refused", "the identity provider's answer was not for this sign-in")
		return
	}
	s := &session{token: t.IDToken, expires: t.Expiry(c.o.Now()), refresh: t.RefreshToken, issuer: p.issuer, client: p.client}
	c.open(w, r, s, p.next)
}

// open asks the brain who a sign-in is, keeps it, and sends the browser to
// the app.
func (c *Console) open(w http.ResponseWriter, r *http.Request, s *session, next string) {
	subject, name, refusal, err := c.whoami(r, s.token)
	switch {
	case err != nil:
		c.o.Log.Warn("console: the brain cannot be reached", "err", err.Error())
		c.back(w, r, "brain", "the brain cannot be reached")
		return
	case refusal != nil:
		c.o.Log.Info("console: a sign-in the brain refused", "kind", "console", "event", "signin", "result", "refused", "why", refusal.Kind)
		c.back(w, r, refusal.Kind, refusal.Detail)
		return
	}
	s.subject, s.name = subject, name
	value, err := c.sessions.open(s)
	if err != nil {
		c.back(w, r, "busy", err.Error())
		return
	}
	c.setCookie(w, r, value)
	c.o.Log.Info("console: signed in", "kind", "console", "event", "signin", "result", "ok", "subject", subject, "name", name, "via", s.via())
	http.Redirect(w, r, c.home(next), http.StatusSeeOther)
}

func (s *session) via() string {
	if s.issuer == "" {
		return "token"
	}
	return "provider"
}

// signinToken signs in with an API token — the way in on a brain that has no
// provider (the one `hangar token create` made on its host).
func (c *Console) signinToken(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		problem(w, 415, "bad-request", "send the token as JSON")
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		problem(w, 400, "bad-request", "send the token as JSON: {\"token\": \"hgr_…\"}")
		return
	}
	in.Token = strings.TrimSpace(in.Token)
	if !strings.HasPrefix(in.Token, "hgr_") {
		problem(w, 400, "bad-request", "an API token starts hgr_")
		return
	}
	info, err := c.how(r)
	switch {
	case err != nil:
		problem(w, 502, "brain-unreachable", "the brain cannot be reached")
		return
	case info != nil:
		// with a provider, people sign in there: a token is a script's
		problem(w, 403, "sign-in", "this hangar signs people in at its identity provider")
		return
	}
	subject, name, refusal, err := c.whoami(r, in.Token)
	switch {
	case err != nil:
		problem(w, 502, "brain-unreachable", "the brain cannot be reached")
		return
	case refusal != nil:
		c.o.Log.Info("console: a token the brain refused", "kind", "console", "event", "signin", "result", "refused", "why", refusal.Kind)
		problem(w, refusal.Status, refusal.Kind, refusal.Detail)
		return
	}
	s := &session{token: in.Token, subject: subject, name: name}
	value, err := c.sessions.open(s)
	if err != nil {
		problem(w, 503, "busy", err.Error())
		return
	}
	c.setCookie(w, r, value)
	c.o.Log.Info("console: signed in", "kind", "console", "event", "signin", "result", "ok", "subject", subject, "name", name, "via", "token")
	writeJSON(w, map[string]any{"signed_in": true, "csrf": s.csrf, "via": "token", "version": c.o.Version})
}

// signout forgets the sign-in, and tells the provider its refresh token is
// no longer wanted (RFC 7009). The provider's own session is the person's to
// end on the provider's page.
func (c *Console) signout(w http.ResponseWriter, r *http.Request) {
	s := c.sessions.of(r)
	if s == nil {
		c.clearCookie(w, r)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.csrfOK(r) {
		problem(w, 403, "cross-origin", "this request does not carry the console's own token: refused")
		return
	}
	c.sessions.drop(s)
	c.clearCookie(w, r)
	s.mu.Lock()
	refresh, issuer, client := s.refresh, s.issuer, s.client
	s.mu.Unlock()
	if refresh != "" {
		if d, err := c.provider.get(r.Context(), issuer); err != nil || d.Revocation == "" {
			c.o.Log.Warn("console: the provider could not be told of a sign-out: its refresh token lives until it expires", "subject", s.subject)
		} else if err := provider.Revoke(r.Context(), c.o.HTTP, d.Revocation, client, refresh); err != nil {
			c.o.Log.Warn("console: the provider could not be told of a sign-out: its refresh token lives until it expires", "subject", s.subject, "err", err.Error())
		}
	}
	c.o.Log.Info("console: signed out", "kind", "console", "event", "signout", "subject", s.subject)
	w.WriteHeader(http.StatusNoContent)
}
