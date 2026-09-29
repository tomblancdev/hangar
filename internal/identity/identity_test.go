package identity

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/config"
	"github.com/tomblancdev/hangar/internal/registry"
	"github.com/tomblancdev/hangar/internal/testoidc"
)

func store(t *testing.T) *registry.Store {
	t.Helper()
	s, err := registry.Open(filepath.Join(t.TempDir(), "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func ask(a *Authenticator, bearer string) (*Identity, error) {
	r := httptest.NewRequest("GET", "/v1/whoami", nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return a.Authenticate(r)
}

func TestOIDC(t *testing.T) {
	iss := testoidc.New(t)
	a := New(config.Identity{OIDC: &config.OIDC{Issuer: iss.URL, Audience: "hangar", GroupsClaim: "groups", NameClaim: "preferred_username"}}, store(t), nil)

	id, err := ask(a, iss.Token(t, testoidc.Claims{Subject: "u-1", Audience: "hangar", Groups: []string{"users"}, Name: "alice"}))
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "u-1" || id.Name != "alice" || len(id.Groups) != 1 || id.Groups[0] != "users" || id.Via != "oidc" {
		t.Fatalf("%+v", id)
	}
	if !id.Can(ScopeWrite) {
		t.Fatal("a person signed in may do what their tier allows")
	}

	refused := map[string]string{
		"another client's token": iss.Token(t, testoidc.Claims{Subject: "u-1", Audience: "someone-else"}),
		"an expired token":       iss.Token(t, testoidc.Claims{Subject: "u-1", Audience: "hangar", Expiry: time.Now().Add(-time.Minute)}),
		"a forged token":         iss.Forged(t, testoidc.Claims{Subject: "u-1", Audience: "hangar"}),
		"another issuer's token": iss.Token(t, testoidc.Claims{Subject: "u-1", Audience: "hangar", Issuer: "https://other.example.com"}),
		"not a token at all":     "eyJhbGciOi.nothing.here",
	}
	for name, tok := range refused {
		var ie *Error
		if _, err := ask(a, tok); !errors.As(err, &ie) {
			t.Errorf("%s: got %v, want a refusal", name, err)
		}
	}
}

func TestAnUnreachableProviderIsNotARefusal(t *testing.T) {
	// a 401 would tell the person their token is bad; it is the brain that
	// cannot check it
	a := New(config.Identity{OIDC: &config.OIDC{Issuer: "http://127.0.0.1:1", Audience: "hangar", GroupsClaim: "groups"}}, store(t), nil)
	_, err := ask(a, "eyJhbGciOi.x.y")
	var ie *Error
	if err == nil || errors.As(err, &ie) {
		t.Fatalf("got %v", err)
	}
}

func TestAPITokens(t *testing.T) {
	s := store(t)
	a := New(config.Identity{}, s, nil)
	ctx := context.Background()

	secret, tok, err := Mint(ctx, s, "u-1", "ci", []string{"users"}, []string{ScopeRead}, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) || strings.Contains(tok.Hash, secret) {
		t.Fatal("the secret is hgr_… and only its hash is kept")
	}
	id, err := ask(a, secret)
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "u-1" || id.Via != "token" || id.Can(ScopeWrite) || !id.Can(ScopeRead) || id.ViaLabel() != "token:"+tok.ID {
		t.Fatalf("%+v", id)
	}

	if _, err := ask(a, TokenPrefix+"unknown"); err == nil {
		t.Fatal("an unknown token was accepted")
	}
	if _, err := ask(a, ""); !errors.Is(err, ErrNoCredentials) {
		t.Fatal("no header is no credentials")
	}
	if _, err := ask(a, iss(t)); err == nil || !strings.Contains(err.Error(), "no identity provider") {
		t.Fatalf("a JWT with no provider configured: %v", err)
	}

	a.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := ask(a, secret); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired: %v", err)
	}
	a.now = time.Now
	_ = s.RevokeToken(ctx, "u-1", tok.ID)
	if _, err := ask(a, secret); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked: %v", err)
	}
}

func iss(t *testing.T) string {
	return testoidc.New(t).Token(t, testoidc.Claims{Subject: "x", Audience: "hangar"})
}

func TestMintBounds(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	var re *RequestError
	if _, _, err := Mint(ctx, s, "u", "n", nil, nil, 48*time.Hour, 24*time.Hour); !errors.As(err, &re) {
		t.Fatal("longer than max_ttl")
	}
	if _, _, err := Mint(ctx, s, "u", "n", nil, []string{"admin"}, time.Hour, 24*time.Hour); !errors.As(err, &re) {
		t.Fatal("an unknown scope")
	}
	_, tok, err := Mint(ctx, s, "u", "n", nil, []string{ScopeWrite}, time.Hour, 24*time.Hour)
	if err != nil || len(tok.Scopes) != 2 {
		t.Fatalf("write implies read: %v %v", tok.Scopes, err)
	}
}
