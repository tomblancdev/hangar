package console

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/tomblancdev/hangar/internal/provider"
)

// ---- Sign-ins, kept in memory -------------------------------------------------
//
// A session is a person's sign-in as the console holds it: the token the
// brain reads, and — from a provider — the refresh token that renews it. The
// browser holds a random value in a cookie no script can read; the console
// keeps that value's hash only, so nothing here names a cookie that would
// work. Nothing is written to disk.

const (
	cookieName       = "hangar_console"
	cookieNameSecure = "__Host-hangar_console" // a browser takes it only over TLS, for this host alone
	csrfHeader       = "X-Hangar-Csrf"

	maxSessions = 10000
)

type session struct {
	key     string // the hash of the cookie's value
	csrf    string
	subject string
	name    string
	created time.Time
	seen    time.Time // its last use — the sessions' lock keeps it, not its own

	// mu keeps the token and its renewal: held while the provider is asked,
	// so nothing that every request passes through may wait on it
	mu      sync.Mutex
	token   string    // what the brain reads
	expires time.Time // a provider's token; zero for an API token
	refresh string
	issuer  string
	client  string
}

type sessions struct {
	now  func() time.Time
	idle time.Duration

	mu sync.Mutex
	m  map[string]*session
}

func newSessions(now func() time.Time, idle time.Duration) *sessions {
	return &sessions{now: now, idle: idle, m: map[string]*session{}}
}

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system has no randomness: nothing here may run
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// errFull: more sign-ins than a console holds.
var errFull = errors.New("the console holds too many sign-ins: try again later")

// open keeps a new sign-in and returns the cookie's value.
func (ss *sessions) open(s *session) (string, error) {
	value := random(32)
	now := ss.now()
	s.key, s.csrf, s.created, s.seen = hash(value), random(24), now, now
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if len(ss.m) >= maxSessions {
		ss.sweepLocked(now)
	}
	if len(ss.m) >= maxSessions {
		return "", errFull
	}
	ss.m[s.key] = s
	return value, nil
}

func (ss *sessions) sweepLocked(now time.Time) {
	for k, s := range ss.m {
		if now.Sub(s.seen) > ss.idle {
			delete(ss.m, k)
		}
	}
}

// of finds the request's sign-in, and marks it used. Nil: none, or one left
// unused too long.
func (ss *sessions) of(r *http.Request) *session {
	var value string
	for _, name := range []string{cookieNameSecure, cookieName} {
		if ck, err := r.Cookie(name); err == nil && ck.Value != "" {
			value = ck.Value
			break
		}
	}
	if value == "" {
		return nil
	}
	now := ss.now()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s := ss.m[hash(value)]
	if s == nil {
		return nil
	}
	if now.Sub(s.seen) > ss.idle {
		delete(ss.m, s.key)
		return nil
	}
	s.seen = now
	return s
}

func (ss *sessions) drop(s *session) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.m, s.key)
}

func (ss *sessions) count() int {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return len(ss.m)
}

// csrfOK: a request that changes something carries the session's own token
// in a header — which another site's page can neither read nor send.
func (s *session) csrfOK(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeader)), []byte(s.csrf)) == 1
}

func (s *session) refreshable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refresh != ""
}

// transient is a failure that says nothing of the sign-in: the provider
// could not be reached.
type transient struct{ err error }

func (t *transient) Error() string { return t.err.Error() }
func (t *transient) Unwrap() error { return t.err }

// bearer is the token to send the brain: an API token as it is; a
// provider's while it lives, renewed when it is about to end (or when the
// brain just refused it: force). One renewal at a time per sign-in — a
// refresh token is good once.
func (s *session) bearer(ctx context.Context, c *Console, force bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.issuer == "" { // an API token
		return s.token, nil
	}
	if !force && c.o.Now().Add(30*time.Second).Before(s.expires) {
		return s.token, nil
	}
	if s.refresh == "" {
		return "", errors.New("the provider's token ended, and it gave nothing to renew it with")
	}
	d, err := c.provider.get(ctx, s.issuer)
	if err != nil {
		return "", &transient{err}
	}
	t, err := provider.Refresh(ctx, c.o.HTTP, d.Token, s.client, s.refresh)
	if err != nil {
		var refused *provider.RefusedError
		if errors.As(err, &refused) {
			return "", err
		}
		return "", &transient{err}
	}
	s.token, s.expires = t.Bearer(), t.Expiry(c.o.Now())
	if t.RefreshToken != "" {
		s.refresh = t.RefreshToken
	}
	return s.token, nil
}

// ---- The cookie ---------------------------------------------------------------

func (c *Console) setCookie(w http.ResponseWriter, r *http.Request, value string) {
	ck := &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode}
	if c.secure(r) {
		ck.Name, ck.Secure = cookieNameSecure, true
	}
	http.SetCookie(w, ck)
}

func (c *Console) clearCookie(w http.ResponseWriter, r *http.Request) {
	ck := &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode}
	if c.secure(r) {
		ck.Name, ck.Secure = cookieNameSecure, true
	}
	http.SetCookie(w, ck)
}

// ---- The provider's endpoints, kept a while -----------------------------------

type discoveryCache struct {
	hc  *http.Client
	now func() time.Time

	mu     sync.Mutex
	issuer string
	d      *provider.Discovery
	at     time.Time
}

func (dc *discoveryCache) get(ctx context.Context, issuer string) (*provider.Discovery, error) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if dc.d != nil && dc.issuer == issuer && dc.now().Sub(dc.at) < 10*time.Minute {
		return dc.d, nil
	}
	d, err := provider.Discover(ctx, dc.hc, issuer)
	if err != nil {
		return nil, err
	}
	dc.issuer, dc.d, dc.at = issuer, d, dc.now()
	return d, nil
}
