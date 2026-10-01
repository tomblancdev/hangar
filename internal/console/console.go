// Package console is the web console's server side: a door on the brain's
// API, like the command line. It serves the app (static files drawn from
// what the brain serves, ui/console), signs people in at the operator's
// identity provider, keeps their sign-in — the browser holds only a cookie
// no script can read — and passes the app's calls on to the API with the
// person's own token.
//
// It knows the brain only through its API: inside `hangar serve` it calls
// the brain's handler, and on its own (`hangar console --brain URL`) it
// calls the brain over the network, holding none of the plugins' keys — the
// process to put on a public door, the brain staying behind it.
//
// Nothing of a sign-in is written to disk: a restart signs everyone out,
// and the provider signs them back in.
package console

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Prefix is where the console lives: everything it serves is under it, so a
// gateway routes one path to it and nothing else.
const Prefix = "/console"

// Options is what a console is made of.
type Options struct {
	// Brain reaches the brain's API: BrainURL + "/v1/…" through this client.
	Brain    *http.Client
	BrainURL string
	// URL is the console's public address (https://hangar.example.org): where
	// the provider sends people back, and what makes the cookie Secure. Empty:
	// read from each request (its Host, and X-Forwarded-Proto).
	URL string
	// Idle: a sign-in unused this long ends (default 12h).
	Idle time.Duration
	// HTTP reaches the identity provider (default: a client with a timeout).
	HTTP *http.Client
	// Static is the app: index.html and what it loads.
	Static fs.FS
	// Mark renders the mark (the operator's house word, when one is set) —
	// at rest: it is shown as an image.
	Mark    func() []byte
	Version string
	Log     *slog.Logger
	Now     func() time.Time
}

// Console is the door. Its Handler serves everything under Prefix.
type Console struct {
	o        Options
	base     *url.URL // the public address, when set
	sessions *sessions
	begun    *pendings // sign-ins on their way to the provider and back
	provider *discoveryCache
	files    map[string]file // by path under static/
	index    file
	mux      http.Handler
}

type file struct {
	body []byte
	typ  string
	etag string
}

// New builds a console. It fails on an address that is not one, or an app
// without its page.
func New(o Options) (*Console, error) {
	if o.Brain == nil || o.BrainURL == "" {
		return nil, errors.New("console: no brain to reach")
	}
	if o.Idle == 0 {
		o.Idle = 12 * time.Hour
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	c := &Console{o: o, sessions: newSessions(o.Now, o.Idle), begun: &pendings{m: map[string]*pending{}}, files: map[string]file{}}
	c.provider = &discoveryCache{hc: o.HTTP, now: o.Now}
	if o.URL != "" {
		u, err := PublicURL(o.URL)
		if err != nil {
			return nil, err
		}
		c.base = u
	}
	if err := c.load(); err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+Prefix+"/{$}", c.page)
	mux.HandleFunc("GET "+Prefix+"/static/", c.static)
	mux.HandleFunc("GET "+Prefix+"/mark.svg", c.mark)
	mux.HandleFunc("GET "+Prefix+"/session", c.session)
	mux.HandleFunc("GET "+Prefix+"/signin", c.signin)
	mux.HandleFunc("GET "+Prefix+"/callback", c.callback)
	mux.HandleFunc("POST "+Prefix+"/signin/token", c.signinToken)
	mux.HandleFunc("POST "+Prefix+"/signout", c.signout)
	mux.HandleFunc(Prefix+"/api/", c.api)
	mux.HandleFunc(Prefix+"/", func(w http.ResponseWriter, _ *http.Request) {
		problem(w, 404, "not-found", "nothing here: the console is at "+Prefix+"/")
	})

	// A browser never sends a request that changes something from another
	// site's page: refused on its Sec-Fetch-Site (or Origin) before anything
	// reads it. The token in a header (below) is the second lock.
	cop := http.NewCrossOriginProtection()
	if c.base != nil {
		_ = cop.AddTrustedOrigin(c.base.Scheme + "://" + c.base.Host)
	}
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		problem(w, 403, "cross-origin", "this request came from another site's page: refused")
	}))
	c.mux = c.headers(cop.Handler(mux))
	return c, nil
}

// PublicURL reads a console's public address: scheme and host, nothing else.
func PublicURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("console: %q is not an address people open the console at (https://host, no path)", raw)
	}
	return u, nil
}

// Handler is everything under Prefix.
func (c *Console) Handler() http.Handler { return c.mux }

// Sessions counts the sign-ins held.
func (c *Console) Sessions() int { return c.sessions.count() }

// load reads the app into memory: it is small, and an embedded file has no
// time to compare — each gets a tag from its bytes.
func (c *Console) load() error {
	if c.o.Static == nil {
		return errors.New("console: no app to serve")
	}
	err := fs.WalkDir(c.o.Static, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(c.o.Static, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		typ := mime.TypeByExtension(path.Ext(p))
		switch path.Ext(p) {
		case ".js":
			typ = "text/javascript; charset=utf-8"
		case ".css":
			typ = "text/css; charset=utf-8"
		case ".html":
			typ = "text/html; charset=utf-8"
		case ".svg":
			typ = "image/svg+xml; charset=utf-8"
		case ".woff2":
			typ = "font/woff2"
		}
		if typ == "" {
			typ = "application/octet-stream"
		}
		f := file{body: b, typ: typ, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		if p == "index.html" {
			c.index = f
			return nil
		}
		c.files[p] = f
		return nil
	})
	if err != nil {
		return fmt.Errorf("console: the app: %w", err)
	}
	if c.index.body == nil {
		return errors.New("console: the app has no index.html")
	}
	return nil
}

// headers are every answer's: the page may load only what the console
// itself serves, run no inline script, sit in no frame.
func (c *Console) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (c *Console) serve(w http.ResponseWriter, r *http.Request, f file) {
	w.Header().Set("Content-Type", f.typ)
	w.Header().Set("ETag", f.etag)
	// asked again each time, answered 304 while it has not changed: a new
	// release is on the page at the next load
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(f.body)
}

func (c *Console) page(w http.ResponseWriter, r *http.Request) { c.serve(w, r, c.index) }

func (c *Console) static(w http.ResponseWriter, r *http.Request) {
	f, ok := c.files[strings.TrimPrefix(r.URL.Path, Prefix+"/static/")]
	if !ok {
		problem(w, 404, "not-found", "no such file")
		return
	}
	c.serve(w, r, f)
}

func (c *Console) mark(w http.ResponseWriter, _ *http.Request) {
	if c.o.Mark == nil {
		problem(w, 404, "not-found", "no mark")
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	// the mark carries its own style and faces: its policy is its own
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; font-src data:")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(c.o.Mark())
}

// ---- Answers ----------------------------------------------------------------

// problem answers in the API's own shape (RFC 9457), so the app reads the
// console's refusals as it reads the brain's.
func problem(w http.ResponseWriter, status int, kind, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "urn:hangar:problem:" + kind, "status": status, "kind": kind, "detail": detail,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---- The address ------------------------------------------------------------

// address is the console's public address for this request: the one set, or
// the one the request came to.
func (c *Console) address(r *http.Request) *url.URL {
	if c.base != nil {
		return c.base
	}
	u := &url.URL{Scheme: "http", Host: r.Host}
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		u.Scheme = "https"
	}
	return u
}

func (c *Console) secure(r *http.Request) bool { return c.address(r).Scheme == "https" }

// ---- The brain --------------------------------------------------------------

// ask calls the brain's API with a bearer token (none: the public calls).
func (c *Console) ask(r *http.Request, method, p, bearer string, body []byte, contentType string) (*http.Response, error) {
	var rd io.Reader = http.NoBody
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, c.o.BrainURL+p, rd)
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "hangar-console/"+c.o.Version)
	return c.o.Brain.Do(req)
}

// brainProblem reads a refusal of the brain's.
type brainProblem struct {
	Status int    `json:"status"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// whoami asks the brain who a token is: the person, or the brain's refusal.
func (c *Console) whoami(r *http.Request, bearer string) (subject, name string, refusal *brainProblem, err error) {
	resp, err := c.ask(r, http.MethodGet, "/v1/whoami", bearer, nil, "")
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", nil, err
	}
	if resp.StatusCode != http.StatusOK {
		p := &brainProblem{Status: resp.StatusCode}
		if json.Unmarshal(b, p) != nil || p.Detail == "" {
			p.Detail = "the brain answered " + resp.Status
		}
		return "", "", p, nil
	}
	var me struct {
		Subject string `json:"subject"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(b, &me); err != nil {
		return "", "", nil, err
	}
	return me.Subject, me.Name, nil, nil
}

// ---- The API, passed on -----------------------------------------------------

// api passes the app's call on to the brain with the person's own token. The
// brain answers as it answers any door: the console adds nothing, decides
// nothing.
func (c *Console) api(w http.ResponseWriter, r *http.Request) {
	s := c.sessions.of(r)
	if s == nil {
		problem(w, 401, "sign-in", "sign in")
		return
	}
	if !safe(r.Method) && !s.csrfOK(r) {
		problem(w, 403, "cross-origin", "this request does not carry the console's own token: refused")
		return
	}
	p := path.Clean(strings.TrimPrefix(r.URL.Path, Prefix+"/api"))
	if !strings.HasPrefix(p, "/v1/") {
		problem(w, 404, "not-found", "the console passes on the API's own calls: /v1/…")
		return
	}
	if r.URL.RawQuery != "" {
		p += "?" + r.URL.RawQuery
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		problem(w, 400, "bad-request", "the request's body is too large, or could not be read")
		return
	}
	for attempt := 0; ; attempt++ {
		bearer, err := s.bearer(r.Context(), c, attempt > 0)
		if err != nil {
			c.ended(w, r, s, err)
			return
		}
		resp, err := c.ask(r, r.Method, p, bearer, body, r.Header.Get("Content-Type"))
		if err != nil {
			c.o.Log.Warn("console: the brain cannot be reached", "err", err.Error())
			problem(w, 502, "brain-unreachable", "the brain cannot be reached")
			return
		}
		// the brain refused the token itself (nothing was done): a provider's
		// may have ended between the check and the call — once more, fresh
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			if attempt == 0 && s.refreshable() {
				continue
			}
			c.ended(w, r, s, errors.New("the brain no longer takes this sign-in"))
			return
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
}

func safe(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// ended drops a sign-in that is over, and says so.
func (c *Console) ended(w http.ResponseWriter, r *http.Request, s *session, why error) {
	var t *transient
	if errors.As(why, &t) {
		c.o.Log.Warn("console: the identity provider cannot be reached", "err", why.Error())
		problem(w, 503, "provider-unreachable", "the identity provider cannot be reached: your sign-in could not be renewed")
		return
	}
	c.sessions.drop(s)
	c.clearCookie(w, r)
	c.o.Log.Info("console: a sign-in ended", "kind", "console", "event", "ended", "subject", s.subject, "why", why.Error())
	problem(w, 401, "sign-in", "your sign-in ended: sign in again")
}
