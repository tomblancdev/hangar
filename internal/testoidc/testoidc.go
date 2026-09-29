// Package testoidc is an OIDC issuer for tests: discovery, a key set, and
// tokens signed on demand — the identity provider an operator already runs,
// reduced to what the brain reads of it.
package testoidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Issuer is a running test issuer.
type Issuer struct {
	URL string
	key *rsa.PrivateKey
}

// New starts an issuer for the test's lifetime.
func New(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss := &Issuer{key: key}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	iss.URL = srv.URL
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": iss.URL, "jwks_uri": iss.URL + "/jwks",
			"authorization_endpoint": iss.URL + "/authorize", "token_endpoint": iss.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
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
