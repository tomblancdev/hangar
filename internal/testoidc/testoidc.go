// Package testoidc is an OIDC issuer for tests: discovery, a key set, and
// tokens signed on demand — the identity provider an operator already runs,
// reduced to what the brain reads of it. It also signs a command line in by
// the device flow (RFC 8628) for a public client, a console by the
// authorization code flow with PKCE (RFC 7636), refreshes, and revokes
// (RFC 7009): the person's approval is the test's own call.
package testoidc

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Issuer is a running test issuer.
type Issuer struct {
	URL string
	key *rsa.PrivateKey
	t   testing.TB

	// TokenTTL: how long the tokens it hands a signed-in command line live
	// (default an hour) — short, to prove the refresh.
	TokenTTL time.Duration

	// Redirects: the redirect addresses registered for the client, exactly;
	// nil accepts any (a provider never redirects to one it does not know).
	Redirects []string
	// OtherNonce: the tokens a code brings answer another sign-in than the
	// one that asked — what a door must refuse.
	OtherNonce bool
	// Slow: how long a refresh takes to be answered (a provider under load).
	Slow time.Duration

	mu        sync.Mutex
	devices   map[string]*device // by device code
	codes     map[string]*grant  // by authorization code
	person    *Claims            // who is signed in at the provider's own page
	refresh   map[string]device  // a live refresh token → who it signs in
	refreshes int
	revoked   int
	codeAsks  int
}

// grant is an authorization code waiting to be traded.
type grant struct {
	device
	redirect  string
	challenge string
	expires   time.Time
}

type device struct {
	userCode string
	clientID string
	scope    string
	claims   *Claims // set once approved
	denied   bool
	expires  time.Time
}

// New starts an issuer for the test's lifetime.
func New(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss := &Issuer{key: key, t: t, devices: map[string]*device{}, codes: map[string]*grant{}, refresh: map[string]device{}}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	iss.URL = srv.URL
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": iss.URL, "jwks_uri": iss.URL + "/jwks",
			"authorization_endpoint": iss.URL + "/authorize", "token_endpoint": iss.URL + "/token",
			"device_authorization_endpoint": iss.URL + "/device", "revocation_endpoint": iss.URL + "/revoke",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"code_challenge_methods_supported":      []string{"S256"},
		})
	})
	mux.HandleFunc("GET /authorize", iss.authorize)
	mux.HandleFunc("POST /device", iss.deviceAuthorization)
	mux.HandleFunc("POST /token", iss.token)
	mux.HandleFunc("POST /revoke", iss.revoke)
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	return iss
}

// Claims are what a token says.
type Claims struct {
	Subject  string
	Audience string
	Groups   []string
	Name     string
	Expiry   time.Time // zero: an hour from now
	Issuer   string    // empty: this issuer
	Nonce    string    // the sign-in request's, when it sent one
}

// Token signs a token with the issuer's key.
func (i *Issuer) Token(t testing.TB, c Claims) string {
	t.Helper()
	return sign(t, i.key, i.URL, c)
}

// Forged signs a token with a key the issuer never published.
func (i *Issuer) Forged(t testing.TB, c Claims) string {
	t.Helper()
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return sign(t, other, i.URL, c)
}

func sign(t testing.TB, key *rsa.PrivateKey, url string, c Claims) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Expiry.IsZero() {
		c.Expiry = time.Now().Add(time.Hour)
	}
	if c.Issuer == "" {
		c.Issuer = url
	}
	std := jwt.Claims{
		Issuer: c.Issuer, Subject: c.Subject, Audience: jwt.Audience{c.Audience},
		Expiry: jwt.NewNumericDate(c.Expiry), IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
	}
	extra := map[string]any{"groups": c.Groups, "preferred_username": c.Name}
	if c.Nonce != "" {
		extra["nonce"] = c.Nonce
	}
	raw, err := jwt.Signed(signer).Claims(std).Claims(extra).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ---- The authorization code flow (a browser's) ------------------------------

// SignedIn says who is signed in at the provider's own page: the next
// browser sent to /authorize comes back with a code for them, as with a
// provider's session already open. Nil: nobody — the browser comes back
// refused.
func (i *Issuer) SignedIn(c *Claims) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.person = c
}

// CodeAsks counts the browsers sent to /authorize.
func (i *Issuer) CodeAsks() int { i.mu.Lock(); defer i.mu.Unlock(); return i.codeAsks }

func (i *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codeAsks++
	back, err := url.Parse(q.Get("redirect_uri"))
	// what a provider cannot send back, it shows on its own page
	switch {
	case q.Get("client_id") == "":
		http.Error(w, "invalid_client", 400)
		return
	case err != nil || back.Host == "" || (i.Redirects != nil && !slices.Contains(i.Redirects, q.Get("redirect_uri"))):
		http.Error(w, "redirect_uri mismatch: "+q.Get("redirect_uri"), 400)
		return
	}
	answer := url.Values{"state": {q.Get("state")}}
	switch {
	case q.Get("response_type") != "code":
		answer.Set("error", "unsupported_response_type")
	case q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256":
		answer.Set("error", "invalid_request")
		answer.Set("error_description", "a public client proves itself by PKCE (S256)")
	case i.person == nil:
		answer.Set("error", "access_denied")
	default:
		c := *i.person
		c.Audience, c.Nonce = q.Get("client_id"), q.Get("nonce")
		if i.OtherNonce {
			c.Nonce = random(8)
		}
		code := random(16)
		i.codes[code] = &grant{
			device:   device{clientID: q.Get("client_id"), scope: q.Get("scope"), claims: &c},
			redirect: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), expires: time.Now().Add(time.Minute),
		}
		answer.Set("code", code)
	}
	back.RawQuery = answer.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

// tradeCode is the token endpoint's authorization_code grant: a code is good
// once, for the client and the address it was asked with, and only to the
// one holding the verifier its challenge was made from.
func (i *Issuer) tradeCode(w http.ResponseWriter, r *http.Request) {
	g, ok := i.codes[r.PostFormValue("code")]
	delete(i.codes, r.PostFormValue("code"))
	sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
	switch {
	case !ok || time.Now().After(g.expires) || g.clientID != r.PostFormValue("client_id") || g.redirect != r.PostFormValue("redirect_uri"):
		oauthError(w, "invalid_grant")
	case base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge:
		oauthError(w, "invalid_grant")
	default:
		i.issue(w, g.device)
	}
}

// ---- The device flow --------------------------------------------------------

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func oauthError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(400)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func (i *Issuer) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("client_id") == "" {
		oauthError(w, "invalid_client")
		return
	}
	code, user := random(16), strings.ToUpper(random(2)+"-"+random(2))
	i.mu.Lock()
	i.devices[code] = &device{userCode: user, clientID: r.PostFormValue("client_id"), scope: r.PostFormValue("scope"), expires: time.Now().Add(10 * time.Minute)}
	i.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"device_code": code, "user_code": user, "verification_uri": i.URL + "/activate",
		"verification_uri_complete": i.URL + "/activate?code=" + user, "expires_in": 600, "interval": 1,
	})
}

// Waiting returns the user codes of the sign-ins waiting for a person, and
// the scopes each asked for.
func (i *Issuer) Waiting() map[string]string {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := map[string]string{}
	for _, d := range i.devices {
		if d.claims == nil && !d.denied {
			out[d.userCode] = d.scope
		}
	}
	return out
}

// Approve is the person approving the code on the provider's page, signed in
// as the claims say (the audience is the client id the code was asked for).
func (i *Issuer) Approve(userCode string, c Claims) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, d := range i.devices {
		if d.userCode == userCode {
			c.Audience = d.clientID
			d.claims = &c
			return true
		}
	}
	return false
}

// Deny is the person refusing the code.
func (i *Issuer) Deny(userCode string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, d := range i.devices {
		if d.userCode == userCode {
			d.denied = true
		}
	}
}

// Refreshes counts the refresh grants served; Revoked, the refresh tokens
// revoked.
func (i *Issuer) Refreshes() int { i.mu.Lock(); defer i.mu.Unlock(); return i.refreshes }
func (i *Issuer) Revoked() int   { i.mu.Lock(); defer i.mu.Unlock(); return i.revoked }

func (i *Issuer) issue(w http.ResponseWriter, d device) {
	ttl := i.TokenTTL
	if ttl == 0 {
		ttl = time.Hour
	}
	c := *d.claims
	c.Expiry = time.Now().Add(ttl)
	out := map[string]any{
		"access_token": random(16), "token_type": "Bearer", "expires_in": int(ttl.Seconds()),
		"id_token": sign(i.t, i.key, i.URL, c), "scope": d.scope,
	}
	// a refresh token comes only to who asked to stay signed in
	if slices.Contains(strings.Fields(d.scope), "offline_access") {
		rt := random(16)
		// the tokens a refresh brings carry no nonce: it was the sign-in's
		again := *d.claims
		again.Nonce = ""
		d.claims = &again
		i.refresh[rt] = d
		out["refresh_token"] = rt
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// Forget ends every sign-in at the provider: no refresh token is good any
// more (an account disabled, a session revoked there).
func (i *Issuer) Forget() {
	i.mu.Lock()
	defer i.mu.Unlock()
	clear(i.refresh)
}

func (i *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("grant_type") == "refresh_token" {
		i.mu.Lock()
		slow := i.Slow
		i.mu.Unlock()
		time.Sleep(slow)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	switch r.PostFormValue("grant_type") {
	case "urn:ietf:params:oauth:grant-type:device_code":
		d, ok := i.devices[r.PostFormValue("device_code")]
		switch {
		case !ok || d.clientID != r.PostFormValue("client_id"):
			oauthError(w, "invalid_grant")
		case time.Now().After(d.expires):
			oauthError(w, "expired_token")
		case d.denied:
			oauthError(w, "access_denied")
		case d.claims == nil:
			oauthError(w, "authorization_pending")
		default:
			delete(i.devices, r.PostFormValue("device_code"))
			i.issue(w, *d)
		}
	case "authorization_code":
		i.tradeCode(w, r)
	case "refresh_token":
		d, ok := i.refresh[r.PostFormValue("refresh_token")]
		if !ok || d.clientID != r.PostFormValue("client_id") {
			oauthError(w, "invalid_grant")
			return
		}
		// each refresh token is good once: the next comes with the answer
		delete(i.refresh, r.PostFormValue("refresh_token"))
		i.refreshes++
		i.issue(w, d)
	default:
		oauthError(w, "unsupported_grant_type")
	}
}

func (i *Issuer) revoke(w http.ResponseWriter, r *http.Request) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.refresh[r.PostFormValue("token")]; ok {
		delete(i.refresh, r.PostFormValue("token"))
		i.revoked++
	}
	w.WriteHeader(200)
}

// SlowRefresh makes every refresh take this long from now on.
func (i *Issuer) SlowRefresh(d time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Slow = d
}
