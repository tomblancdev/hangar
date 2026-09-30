// Package testoidc is an OIDC issuer for tests: discovery, a key set, and
// tokens signed on demand — the identity provider an operator already runs,
// reduced to what the brain reads of it. It also signs a command line in by
// the device flow (RFC 8628) for a public client, refreshes, and revokes
// (RFC 7009): the person's approval is the test's own call.
package testoidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	mu        sync.Mutex
	devices   map[string]*device // by device code
	refresh   map[string]device  // a live refresh token → who it signs in
	refreshes int
	revoked   int
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
	iss := &Issuer{key: key, t: t, devices: map[string]*device{}, refresh: map[string]device{}}
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
		})
	})
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
	raw, err := jwt.Signed(signer).Claims(std).Claims(extra).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
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
	rt := random(16)
	i.refresh[rt] = d
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": random(16), "token_type": "Bearer", "expires_in": int(ttl.Seconds()),
		"id_token": sign(i.t, i.key, i.URL, c), "refresh_token": rt, "scope": d.scope,
	})
}

func (i *Issuer) token(w http.ResponseWriter, r *http.Request) {
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
