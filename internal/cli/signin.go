package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tomblancdev/hangar/internal/provider"
)

// ---- Signing in: the device flow (RFC 8628) --------------------------------
//
// The brain names its identity provider, its client id and the scopes to ask
// for (GET /v1/signin). The command line asks the provider for a code, shows
// where to approve it — the provider's own page, in any browser, on any
// device — and waits. It keeps the refresh token and sends the ID token (the
// brain reads tokens issued to its client id), refreshed when it ends.

type signinInfo struct {
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes"`
}

func getJSON(env *Env, u string, out any) (int, error) {
	resp, err := env.HTTP.Get(u)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode != 200 {
		var p Problem
		if json.Unmarshal(b, &p) == nil && p.Detail != "" {
			return resp.StatusCode, &p
		}
		return resp.StatusCode, fmt.Errorf("%s answered %s", u, resp.Status)
	}
	return 200, json.Unmarshal(b, out)
}

// discover reads the provider's discovery document.
func discover(env *Env, issuer string) (*provider.Discovery, error) {
	return provider.Discover(context.Background(), env.HTTP, issuer)
}

func postForm(env *Env, endpoint string, form url.Values) (*provider.Tokens, error) {
	return provider.PostForm(context.Background(), env.HTTP, endpoint, form)
}

// keep writes a provider's answer into a sign-in: the ID token is what the
// brain reads (the access token when no ID token came), its expiry read
// from the token itself.
func (e *entry) keep(env *Env, t *provider.Tokens) error {
	tok := t.Bearer()
	if tok == "" {
		return errors.New("the provider handed no token")
	}
	e.Token = tok
	if t.RefreshToken != "" {
		e.RefreshToken = t.RefreshToken
	}
	e.ExpiresAt = t.Expiry(env.Now())
	return nil
}

func deviceSignIn(env *Env, base string, info *signinInfo) (*entry, error) {
	d, err := discover(env, info.Issuer)
	if err != nil {
		return nil, err
	}
	if d.Device == "" {
		return nil, fmt.Errorf("the identity provider %s offers no device sign-in (RFC 8628): turn it on for client %s, or sign in with an API token (hangar login --with-token)", info.Issuer, info.ClientID)
	}
	resp, err := env.HTTP.PostForm(d.Device, url.Values{"client_id": {info.ClientID}, "scope": {strings.Join(info.Scopes, " ")}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var code struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		URI        string `json:"verification_uri"`
		URIWhole   string `json:"verification_uri_complete"`
		ExpiresIn  int    `json:"expires_in"`
		Interval   int    `json:"interval"`
		Error      string `json:"error"`
		Desc       string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&code); err != nil || resp.StatusCode != 200 || code.DeviceCode == "" {
		why := resp.Status
		if code.Error != "" {
			why = strings.TrimSpace(code.Error + " " + code.Desc)
		}
		return nil, fmt.Errorf("the identity provider refused a device code for client %s: %s", info.ClientID, why)
	}
	where := code.URI
	if code.URIWhole != "" {
		where = code.URIWhole
	}
	fmt.Fprintf(env.Stderr, "To sign in to %s, open\n\n    %s\n\nand check the code there reads  %s\n\nWaiting…\n", base, where, code.UserCode)
	interval := 5 * time.Second // RFC 8628's default
	if code.Interval > 0 {
		interval = time.Duration(code.Interval) * time.Second
	}
	if code.ExpiresIn == 0 {
		code.ExpiresIn = 600
	}
	end := env.Now().Add(time.Duration(code.ExpiresIn) * time.Second)
	for env.Now().Before(end) {
		env.Sleep(interval)
		t, err := postForm(env, d.Token, url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {code.DeviceCode}, "client_id": {info.ClientID},
		})
		if err != nil {
			return nil, err
		}
		switch t.Error {
		case "":
			e := &entry{Issuer: info.Issuer, ClientID: info.ClientID}
			if err := e.keep(env, t); err != nil {
				return nil, err
			}
			return e, nil
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return nil, errors.New("the sign-in was refused at the identity provider")
		case "expired_token":
			return nil, errors.New("the code expired before it was approved: hangar login again")
		default:
			return nil, fmt.Errorf("the identity provider refused the sign-in: %s", strings.TrimSpace(t.Error+" "+t.Description))
		}
	}
	return nil, errors.New("the code expired before it was approved: hangar login again")
}

// refresh trades a sign-in's refresh token for a new token.
func refresh(env *Env, e *entry) error {
	if e.RefreshToken == "" {
		return errors.New("no refresh token was kept")
	}
	d, err := discover(env, e.Issuer)
	if err != nil {
		return err
	}
	t, err := provider.Refresh(context.Background(), env.HTTP, d.Token, e.ClientID, e.RefreshToken)
	if err != nil {
		return err
	}
	return e.keep(env, t)
}

// ---- The commands -------------------------------------------------------------

func login(env *Env, args []string) error {
	var withToken bool
	opts := []*opt{boolOpt(&withToken, "read an API token (hgr_…) from stdin instead of signing in at the provider", "with-token")}
	pos, err := parse(args, opts)
	if errors.Is(err, errHelp) {
		fmt.Fprintln(env.Stdout, "hangar login [URL] [--with-token]\n\nSign in to a brain: the device flow at its identity provider — a code to approve\nin any browser — or an API token read from stdin. URL defaults to $HANGAR_URL,\nthen the brain signed in to last.")
		flagHelp(env.Stdout, opts)
		return nil
	}
	if err != nil {
		return err
	}
	s, err := loadStore(env)
	if err != nil {
		return err
	}
	raw := env.Getenv("HANGAR_URL")
	if raw == "" {
		raw = s.Current
	}
	if len(pos) > 0 {
		raw = pos[0]
	}
	if raw == "" || len(pos) > 1 {
		return usagef("login URL — the brain's address")
	}
	base, err := brainURL(raw)
	if err != nil {
		return err
	}
	if u, _ := url.Parse(base); u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		fmt.Fprintf(env.Stderr, "warning: %s is plain http: a token sent to it crosses the network in clear — use the https address its gateway serves\n", base)
	}
	var e *entry
	if withToken {
		line, _ := bufio.NewReader(env.Stdin).ReadString('\n')
		tok := strings.TrimSpace(line)
		if !strings.HasPrefix(tok, "hgr_") {
			return errors.New("an API token starts hgr_: read none on stdin")
		}
		e = &entry{APIToken: tok}
	} else {
		var info signinInfo
		code, err := getJSON(env, base+"/v1/signin", &info)
		switch {
		case code == http.StatusNotFound:
			return fmt.Errorf("%s has no identity provider: sign in with an API token — hangar login %s --with-token < file", base, base)
		case err != nil:
			return fmt.Errorf("the brain at %s: %w", base, err)
		}
		if e, err = deviceSignIn(env, base, &info); err != nil {
			return err
		}
	}
	c := &client{env: env, base: base, token: func() (string, error) { return bearer(env, base, e) }}
	var me whoamiView
	if err := c.do("GET", "/v1/whoami", nil, &me); err != nil {
		return fmt.Errorf("signed in at the provider, but the brain refused the token: %w", err)
	}
	s.Brains[base] = e
	s.Current = base
	if err := s.save(env); err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "Signed in to %s as %s — tier %s.\n", base, me.who(), me.Tier)
	return nil
}

func logout(env *Env, args []string) error {
	if _, err := parse(args, nil); err != nil {
		if errors.Is(err, errHelp) {
			fmt.Fprintln(env.Stdout, "hangar logout\n\nRevoke the sign-in to the current brain at its provider, and forget it.")
			return nil
		}
		return err
	}
	s, err := loadStore(env)
	if err != nil {
		return err
	}
	base := env.Getenv("HANGAR_URL")
	if base == "" {
		base = s.Current
	}
	if base, err = brainURL(base); err != nil {
		return errors.New("signed in nowhere")
	}
	e := s.Brains[base]
	if e == nil {
		return fmt.Errorf("not signed in to %s", base)
	}
	if e.RefreshToken != "" {
		if d, err := discover(env, e.Issuer); err == nil && d.Revocation != "" {
			if err := provider.Revoke(context.Background(), env.HTTP, d.Revocation, e.ClientID, e.RefreshToken); err != nil {
				fmt.Fprintf(env.Stderr, "warning: the provider could not be told (%v): the refresh token lives until it expires\n", err)
			}
		} else {
			fmt.Fprintln(env.Stderr, "warning: the provider names no revocation endpoint: the refresh token lives until it expires")
		}
	}
	delete(s.Brains, base)
	if s.Current == base {
		s.Current = ""
	}
	if err := s.save(env); err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "Signed out of %s.\n", base)
	return nil
}
