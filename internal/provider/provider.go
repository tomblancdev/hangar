// Package provider is a door's side of the operator's identity provider: its
// discovery document, what its token endpoint answers, and a token's claims
// read without checking them — the brain checks. The command line (the
// device flow) and the console (the authorization code flow) both sign
// people in with it, as the one public client the brain names.
package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Discovery is what a door reads of the provider's discovery document.
type Discovery struct {
	Issuer        string `json:"issuer"`
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	Device        string `json:"device_authorization_endpoint"`
	Revocation    string `json:"revocation_endpoint"`
}

// Discover reads the provider's discovery document, and holds it to the
// issuer it was asked for (as OIDC's discovery says).
func Discover(ctx context.Context, hc *http.Client, issuer string) (*Discovery, error) {
	u := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("the identity provider %s: %w", issuer, err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the identity provider %s: %w", issuer, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("the identity provider %s: %w", issuer, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the identity provider %s: %s answered %s", issuer, u, resp.Status)
	}
	var d Discovery
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("the identity provider %s: %w", issuer, err)
	}
	if d.Issuer != issuer {
		return nil, fmt.Errorf("the identity provider at %s calls itself %s", issuer, d.Issuer)
	}
	if d.Token == "" {
		return nil, fmt.Errorf("the identity provider %s names no token endpoint", issuer)
	}
	return &d, nil
}

// Tokens is a token endpoint's answer: the tokens, or the error it names.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
	Interval     int    `json:"interval"`
}

// RefusedError is the provider saying no, in its own words — as opposed to
// a provider that could not be reached.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "the identity provider said " + e.Reason }

// Refused is the provider's refusal, or nil.
func (t *Tokens) Refused() error {
	if t.Error == "" {
		return nil
	}
	return &RefusedError{Reason: strings.TrimSpace(t.Error + " " + t.Description)}
}

// Bearer is the token the brain reads: the ID token (it is issued to the
// brain's client id), the access token when none came.
func (t *Tokens) Bearer() string {
	if t.IDToken != "" {
		return t.IDToken
	}
	return t.AccessToken
}

// Expiry is when that token ends: read from the token itself, else from the
// answer's expires_in.
func (t *Tokens) Expiry(now time.Time) time.Time {
	if c, ok := Peek(t.Bearer()); ok && !c.Expiry.IsZero() {
		return c.Expiry
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second)
}

// PostForm asks the provider's token (or device) endpoint. An answer that
// names an error comes back in Tokens.Error, not as an error.
func PostForm(ctx context.Context, hc *http.Client, endpoint string, form url.Values) (*Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var t Tokens
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s answered %s", endpoint, resp.Status)
	}
	if t.Error == "" && resp.StatusCode != http.StatusOK {
		t.Error = resp.Status
	}
	return &t, nil
}

// Peeked is what a door reads of a token it does not check.
type Peeked struct {
	Expiry time.Time
	Nonce  string
}

// Peek reads a JWT's claims without checking its signature — the brain
// checks, at every call. False for what is not a JWT.
func Peek(tok string) (Peeked, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Peeked{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Peeked{}, false
	}
	var c struct {
		Exp   int64  `json:"exp"`
		Nonce string `json:"nonce"`
	}
	if json.Unmarshal(b, &c) != nil {
		return Peeked{}, false
	}
	p := Peeked{Nonce: c.Nonce}
	if c.Exp != 0 {
		p.Expiry = time.Unix(c.Exp, 0)
	}
	return p, true
}

// Refresh trades a refresh token for new tokens. A *RefusedError is the
// provider saying no; any other error, a provider that could not be asked.
func Refresh(ctx context.Context, hc *http.Client, tokenEndpoint, clientID, refreshToken string) (*Tokens, error) {
	t, err := PostForm(ctx, hc, tokenEndpoint, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {clientID},
	})
	if err != nil {
		return nil, err
	}
	if err := t.Refused(); err != nil {
		return nil, err
	}
	if t.Bearer() == "" {
		return nil, &RefusedError{Reason: "nothing: it handed no token"}
	}
	return t, nil
}

// Revoke tells the provider a refresh token is no longer wanted (RFC 7009).
func Revoke(ctx context.Context, hc *http.Client, revocationEndpoint, clientID, refreshToken string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, revocationEndpoint, strings.NewReader(url.Values{
		"token": {refreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {clientID},
	}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the provider answered %s to the revocation", resp.Status)
	}
	return nil
}
