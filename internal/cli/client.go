package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---- Where the sign-ins are kept ------------------------------------------

// store is the file the command line keeps its sign-ins in: 0600, in a 0700
// directory — a refresh token is a credential (as the AWS CLI's sso cache
// and kubelogin keep theirs).
type store struct {
	Current string            `json:"current,omitempty"`
	Brains  map[string]*entry `json:"brains"`
}

// entry is one brain's sign-in: an API token, or a provider's tokens.
type entry struct {
	APIToken     string    `json:"api_token,omitempty"`
	Issuer       string    `json:"issuer,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Token        string    `json:"token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
}

func (env *Env) storePath() string { return filepath.Join(env.Home, "credentials.json") }

func loadStore(env *Env) (*store, error) {
	s := &store{Brains: map[string]*entry{}}
	b, err := os.ReadFile(env.storePath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("%s: %v", env.storePath(), err)
	}
	if s.Brains == nil {
		s.Brains = map[string]*entry{}
	}
	return s, nil
}

func (s *store) save(env *Env) error {
	if err := os.MkdirAll(env.Home, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(env.Home, ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), env.storePath())
}

// brainURL reads an address: http(s), no trailing slash.
func brainURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", usagef("%q is not a brain's address (https://…)", raw)
	}
	return u.String(), nil
}

// ---- The API ----------------------------------------------------------------

// client is a signed-in conversation with one brain.
type client struct {
	env   *Env
	base  string
	token func() (string, error)
	who   *whoamiView // the caller, once asked (me)
}

// connect finds the brain and how to sign in to it: $HANGAR_URL, else the
// last brain signed in to; $HANGAR_TOKEN, else what was kept for it.
func connect(env *Env) (*client, error) {
	s, err := loadStore(env)
	if err != nil {
		return nil, err
	}
	base := env.Getenv("HANGAR_URL")
	if base == "" {
		base = s.Current
	}
	if base == "" {
		return nil, errors.New("no brain: hangar login URL (or set HANGAR_URL)")
	}
	if base, err = brainURL(base); err != nil {
		return nil, err
	}
	c := &client{env: env, base: base}
	if t := env.Getenv("HANGAR_TOKEN"); t != "" {
		c.token = func() (string, error) { return t, nil }
		return c, nil
	}
	e := s.Brains[base]
	if e == nil {
		return nil, fmt.Errorf("not signed in to %s: hangar login %s", base, base)
	}
	c.token = func() (string, error) { return bearer(env, base, e) }
	return c, nil
}

// bearer is the token to send: an API token as it is; a provider's while it
// lives, refreshed when it is about to end.
func bearer(env *Env, base string, e *entry) (string, error) {
	if e.APIToken != "" {
		return e.APIToken, nil
	}
	if e.Token != "" && env.Now().Add(30*time.Second).Before(e.ExpiresAt) {
		return e.Token, nil
	}
	if err := refresh(env, e); err != nil {
		return "", fmt.Errorf("your sign-in to %s ended (%v): hangar login %s", base, err, base)
	}
	s, err := loadStore(env)
	if err != nil {
		return "", err
	}
	s.Brains[base] = e
	if err := s.save(env); err != nil {
		return "", err
	}
	return e.Token, nil
}

// Problem is a refusal as the API returns it (RFC 9457).
type Problem struct {
	Status     int    `json:"status"`
	Kind       string `json:"kind"`
	Detail     string `json:"detail"`
	Violations []struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	} `json:"violations"`
	Refusals []struct {
		Message string `json:"message"`
	} `json:"refusals"`
	Room *struct {
		Message string `json:"message"`
	} `json:"room"`
	Operation string `json:"operation"`
}

func (p *Problem) Error() string {
	if p.Detail == "" {
		return fmt.Sprintf("the brain answered %d", p.Status)
	}
	return p.Detail
}

// lines are what a refusal says beyond its first line.
func (p *Problem) lines() []string {
	var out []string
	if len(p.Violations) > 1 {
		for _, v := range p.Violations {
			out = append(out, v.Field+": "+v.Reason)
		}
	}
	if len(p.Refusals) > 1 {
		for _, r := range p.Refusals {
			out = append(out, r.Message)
		}
	}
	if p.Room != nil && p.Room.Message != "" && p.Room.Message != p.Detail {
		out = append(out, p.Room.Message)
	}
	if p.Operation != "" {
		out = append(out, "its operation: hangar wait "+p.Operation)
	}
	return out
}

// do asks the brain; a refusal comes back as a *Problem.
func (c *client) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	tok, err := c.token()
	if err != nil {
		return err
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.env.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("the brain at %s cannot be reached: %w", c.base, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		p := &Problem{Status: resp.StatusCode}
		if json.Unmarshal(b, p) != nil || p.Detail == "" {
			p.Detail = fmt.Sprintf("the brain answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		return p
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func clientToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "cli-" + hex.EncodeToString(b)
}

// ---- What the API returns ---------------------------------------------------

type resource struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Owner       string            `json:"owner"`
	OwnerName   string            `json:"owner_name,omitempty"`
	Zone        string            `json:"zone"`
	State       string            `json:"state"`
	Status      string            `json:"status,omitempty"`
	Summary     string            `json:"summary,omitempty"`
	Names       map[string]string `json:"names,omitempty"`
	Spec        json.RawMessage   `json:"spec"`
	Observed    json.RawMessage   `json:"observed"`
	Tags        map[string]string `json:"tags"`
	SharedWith  []string          `json:"shared_with"`
	Unusable    string            `json:"unusable"`
	Pending     bool              `json:"pending"`
	Drift       string            `json:"drift"`
	Hold        string            `json:"hold,omitempty"`
	Room        struct {
		GuaranteedMB int64 `json:"guaranteed_mb"`
		SpotMB       int64 `json:"spot_mb"`
	} `json:"room"`
	CreatedAt time.Time `json:"created_at"`
}

type operation struct {
	ID           string          `json:"id"`
	ResourceID   string          `json:"resource_id"`
	ResourceName string          `json:"resource_name,omitempty"`
	Kind         string          `json:"kind"`
	Action       string          `json:"action"`
	State        string          `json:"state"`
	Error        string          `json:"error"`
	Result       json.RawMessage `json:"result"`
	CreatedAt    time.Time       `json:"created_at"`
	FinishedAt   *time.Time      `json:"finished_at"`
}

type accepted struct {
	Operation operation `json:"operation"`
	Resource  resource  `json:"resource"`
}

// wait asks for an operation until it ends; a failed one is an error in its
// own words.
func (c *client) wait(id string, timeout time.Duration) (*operation, error) {
	end := c.env.Now().Add(timeout)
	for {
		var op operation
		if err := c.do("GET", "/v1/operations/"+id+"?wait=60", nil, &op); err != nil {
			return nil, err
		}
		if op.State != "running" {
			if op.State == "failed" {
				return &op, fmt.Errorf("%s failed: %s", describeOp(&op), op.Error)
			}
			return &op, nil
		}
		if c.env.Now().After(end) {
			return &op, fmt.Errorf("%s still runs after %s: hangar wait %s", describeOp(&op), timeout, id)
		}
		c.env.Sleep(c.env.Poll)
	}
}

func describeOp(op *operation) string {
	what := op.Kind
	if op.Action != "" {
		what = op.Action
	}
	return fmt.Sprintf("the %s of %s (%s)", what, op.ResourceID, op.ID)
}

// settle waits for a resource being made, changed or deleted to be one of
// the others, and returns it.
func (c *client) settle(id string, timeout time.Duration, say func(string)) (*resource, error) {
	for {
		var r resource
		if err := c.do("GET", "/v1/resources/"+id, nil, &r); err != nil {
			return nil, err
		}
		if r.State != "creating" && r.State != "updating" && r.State != "deleting" {
			return &r, nil
		}
		var ops struct {
			Operations []operation `json:"operations"`
		}
		if err := c.do("GET", "/v1/operations?resource="+id+"&limit=1", nil, &ops); err != nil {
			return nil, err
		}
		if len(ops.Operations) == 0 || ops.Operations[0].State != "running" {
			c.env.Sleep(c.env.Poll)
			continue
		}
		say(fmt.Sprintf("%s is %s (%s): waiting", id, r.State, ops.Operations[0].ID))
		if _, err := c.wait(ops.Operations[0].ID, timeout); err != nil {
			// a failure is the resource's to show: read it again
			say(err.Error())
		}
	}
}
