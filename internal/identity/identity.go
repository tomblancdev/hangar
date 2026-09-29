// Package identity answers the ask's first question: who is this?
//
// Two kinds of bearer token are accepted. A token signed by the operator's
// OIDC provider (an ID token, or an access token in JWT form, issued to the
// configured client id): the person's subject and groups come from its
// claims. Or an API token made for automation (hgr_…): only its hash is kept,
// it expires, it carries its owner's groups as they were when it was made,
// and it may be read-only.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/tomblancdev/hangar/internal/config"
	"github.com/tomblancdev/hangar/internal/ids"
	"github.com/tomblancdev/hangar/internal/registry"
)

// Scopes an API token may carry. An OIDC session has both.
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
)

// TokenPrefix starts every API token's secret, so a leaked one is
// recognisable by a secret scanner and never mistaken for a JWT.
const TokenPrefix = "hgr_"

// Identity is the caller.
type Identity struct {
	Subject string
	Name    string
	Groups  []string
	// Via is "oidc" or "token".
	Via     string
	TokenID string
	// Scopes: nil for an OIDC session (everything).
	Scopes []string
}

// Can reports whether the caller holds a scope.
func (i *Identity) Can(scope string) bool {
	return i.Scopes == nil || slices.Contains(i.Scopes, scope)
}

// ViaLabel is how the audit names the credential used.
func (i *Identity) ViaLabel() string {
	if i.Via == "token" {
		return "token:" + i.TokenID
	}
	return i.Via
}

// Error is a refused credential; always a 401.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

// RequestError is a token request the caller must change (a 400).
type RequestError struct{ Message string }

func (e *RequestError) Error() string { return e.Message }

// ErrNoCredentials: the request carries no bearer token.
var ErrNoCredentials = &Error{Message: "sign in: this API takes a bearer token (Authorization: Bearer …)"}

// Authenticator checks bearer tokens.
type Authenticator struct {
	cfg   config.Identity
	store *registry.Store
	now   func() time.Time
	oidc  *lazyVerifier
}

// New returns an authenticator. The identity provider is reached on the first
// OIDC token, not at start-up, so a slow provider never keeps the brain down.
func New(cfg config.Identity, store *registry.Store, client *http.Client) *Authenticator {
	a := &Authenticator{cfg: cfg, store: store, now: time.Now}
	if cfg.OIDC != nil {
		if client == nil {
			client = &http.Client{Timeout: 10 * time.Second}
		}
		a.oidc = &lazyVerifier{cfg: cfg.OIDC, client: client}
	}
	return a
}

// Authenticate reads the request's bearer token.
func (a *Authenticator) Authenticate(r *http.Request) (*Identity, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return nil, ErrNoCredentials
	}
	raw, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, &Error{Message: "the Authorization header must read: Bearer <token>"}
	}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, TokenPrefix) {
		return a.apiToken(r.Context(), raw)
	}
	if a.oidc == nil {
		return nil, &Error{Message: "this hangar has no identity provider: use an API token"}
	}
	return a.oidc.identify(r.Context(), raw)
}

func (a *Authenticator) apiToken(ctx context.Context, secret string) (*Identity, error) {
	tok, err := a.store.TokenByHash(ctx, Hash(secret))
	if errors.Is(err, registry.ErrNotFound) {
		return nil, &Error{Message: "this API token is not known here"}
	}
	if err != nil {
		return nil, err
	}
	now := a.now()
	switch {
	case tok.RevokedAt != nil:
		return nil, &Error{Message: fmt.Sprintf("API token %s was revoked", tok.ID)}
	case !now.Before(tok.ExpiresAt):
		return nil, &Error{Message: fmt.Sprintf("API token %s expired at %s", tok.ID, tok.ExpiresAt.Format(time.RFC3339))}
	}
	// a write per request would be a lot for a timestamp: once a minute is enough
	if tok.LastUsed == nil || now.Sub(*tok.LastUsed) > time.Minute {
		_ = a.store.TouchToken(ctx, tok.ID)
	}
	return &Identity{Subject: tok.Owner, Name: tok.Name, Groups: tok.Groups, Via: "token", TokenID: tok.ID, Scopes: tok.Scopes}, nil
}

// Hash is what the registry keeps of a token's secret.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Mint makes an API token and returns its secret — the only time the secret
// exists outside the caller's hands.
func Mint(ctx context.Context, store *registry.Store, owner, name string, groups, scopes []string, ttl, maxTTL time.Duration) (string, *registry.Token, error) {
	if ttl <= 0 || ttl > maxTTL {
		return "", nil, &RequestError{Message: fmt.Sprintf("a token lives more than 0 and at most %s", maxTTL)}
	}
	if len(scopes) == 0 {
		scopes = []string{ScopeRead, ScopeWrite}
	}
	for _, s := range scopes {
		if s != ScopeRead && s != ScopeWrite {
			return "", nil, &RequestError{Message: fmt.Sprintf("no scope %q: read or write", s)}
		}
	}
	if slices.Contains(scopes, ScopeWrite) && !slices.Contains(scopes, ScopeRead) {
		scopes = append(scopes, ScopeRead)
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	secret := TokenPrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
	tok := &registry.Token{
		ID: ids.New(ids.Token), Hash: Hash(secret), Owner: owner, Name: name,
		Groups: groups, Scopes: scopes, ExpiresAt: store.Now().Add(ttl),
	}
	if err := store.InsertToken(ctx, tok); err != nil {
		return "", nil, err
	}
	return secret, tok, nil
}

// lazyVerifier discovers the provider on first use and retries a failed
// discovery at most every ten seconds.
type lazyVerifier struct {
	cfg    *config.OIDC
	client *http.Client

	mu      sync.Mutex
	v       *oidc.IDTokenVerifier
	lastTry time.Time
	lastErr error
}

func (l *lazyVerifier) verifier() (*oidc.IDTokenVerifier, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.v != nil {
		return l.v, nil
	}
	if time.Since(l.lastTry) < 10*time.Second && l.lastErr != nil {
		return nil, l.lastErr
	}
	l.lastTry = time.Now()
	// The provider keeps this context for every later fetch of its keys, so it
	// must be one that is never cancelled; the client's timeout bounds each.
	ctx := oidc.ClientContext(context.Background(), l.client)
	p, err := oidc.NewProvider(ctx, l.cfg.Issuer)
	if err != nil {
		l.lastErr = fmt.Errorf("the identity provider cannot be reached: %w", err)
		return nil, l.lastErr
	}
	l.v, l.lastErr = p.Verifier(&oidc.Config{ClientID: l.cfg.Audience}), nil
	return l.v, nil
}

func (l *lazyVerifier) identify(ctx context.Context, raw string) (*Identity, error) {
	v, err := l.verifier()
	if err != nil {
		return nil, err
	}
	tok, err := v.Verify(ctx, raw)
	if err != nil {
		return nil, &Error{Message: "the token was refused: " + err.Error()}
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, &Error{Message: "the token's claims cannot be read"}
	}
	id := &Identity{Subject: tok.Subject, Via: "oidc", Groups: stringsClaim(claims[l.cfg.GroupsClaim])}
	if n, ok := claims[l.cfg.NameClaim].(string); ok {
		id.Name = n
	}
	if id.Subject == "" {
		return nil, &Error{Message: "the token names no subject"}
	}
	return id, nil
}

// stringsClaim reads a claim that is a list of strings, or one string.
func stringsClaim(v any) []string {
	switch c := v.(type) {
	case string:
		return []string{c}
	case []any:
		out := make([]string, 0, len(c))
		for _, x := range c {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
