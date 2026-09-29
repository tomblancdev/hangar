// Package server is the brain's HTTP face: the house contract (/healthz,
// /metrics, /openapi.json, the mark) and the API under /v1 that every door —
// the command line, the console, a Terraform provider, curl — is a client of.
//
// Every /v1 call writes exactly one audit line when it ends, whatever path it
// took; every refusal is an RFC 9457 problem document.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tomblancdev/hangar/api"
	"github.com/tomblancdev/hangar/internal/audit"
	"github.com/tomblancdev/hangar/internal/config"
	"github.com/tomblancdev/hangar/internal/core"
	"github.com/tomblancdev/hangar/internal/identity"
	"github.com/tomblancdev/hangar/internal/metrics"
	"github.com/tomblancdev/hangar/internal/plugins"
	"github.com/tomblancdev/hangar/internal/registry"
	"github.com/tomblancdev/hangar/ui"
)

// Server holds what every handler needs.
type Server struct {
	cfg     *config.Config
	core    *core.Core
	auth    *identity.Authenticator
	store   *registry.Store
	host    *plugins.Host
	audit   *audit.Log
	metrics *metrics.Set
	log     *slog.Logger
	version string
	spec    []byte
}

// New builds the server. It fails only if the embedded contract cannot be
// read — a build bug.
func New(cfg *config.Config, c *core.Core, auth *identity.Authenticator, store *registry.Store, host *plugins.Host,
	a *audit.Log, m *metrics.Set, log *slog.Logger, version string) (*Server, error) {
	spec, err := SpecJSON(version)
	if err != nil {
		return nil, err
	}
	m.Counter("hangar_http_requests_total", "API calls, by route and status.", "route", "code")
	return &Server{cfg: cfg, core: c, auth: auth, store: store, host: host, audit: a, metrics: m, log: log, version: version, spec: spec}, nil
}

// SpecJSON renders the embedded contract as JSON, its version filled in.
func SpecJSON(version string) ([]byte, error) {
	var doc any
	if err := yaml.Unmarshal([]byte(strings.ReplaceAll(string(api.OpenAPI), "{{VERSION}}", version)), &doc); err != nil {
		return nil, fmt.Errorf("the embedded OpenAPI document: %w", err)
	}
	return json.Marshal(doc)
}

// Routes lists every route the handler serves, as "METHOD /path" with the
// spec's {placeholders} — the test that holds the code and the contract
// together reads it.
func Routes() []string {
	var out []string
	for _, r := range routes {
		out = append(out, r.pattern)
	}
	return out
}

type route struct {
	pattern string
	h       func(*Server) http.HandlerFunc
	api     func(*Server) apiFunc // a /v1 route: signed in, audited
}

type apiFunc func(w http.ResponseWriter, r *http.Request, who *core.Caller) error

var routes = []route{
	{pattern: "GET /{$}", h: func(s *Server) http.HandlerFunc { return s.front }},
	{pattern: "GET /healthz", h: func(s *Server) http.HandlerFunc { return s.healthz }},
	{pattern: "GET /metrics", h: func(s *Server) http.HandlerFunc { return s.metricsPage }},
	{pattern: "GET /openapi.json", h: func(s *Server) http.HandlerFunc { return s.openapi }},
	{pattern: "GET /mark.svg", h: func(s *Server) http.HandlerFunc { return s.mark }},

	{pattern: "GET /v1/whoami", api: func(s *Server) apiFunc { return s.whoami }},
	{pattern: "GET /v1/types", api: func(s *Server) apiFunc { return s.types }},
	{pattern: "GET /v1/zones", api: func(s *Server) apiFunc { return s.zones }},
	{pattern: "GET /v1/limits", api: func(s *Server) apiFunc { return s.limits }},
	{pattern: "GET /v1/resources", api: func(s *Server) apiFunc { return s.listResources }},
	{pattern: "POST /v1/resources", api: func(s *Server) apiFunc { return s.createResource }},
	{pattern: "GET /v1/resources/{id}", api: func(s *Server) apiFunc { return s.getResource }},
	{pattern: "DELETE /v1/resources/{id}", api: func(s *Server) apiFunc { return s.deleteResource }},
	{pattern: "POST /v1/resources/{id}/actions/{action}", api: func(s *Server) apiFunc { return s.act }},
	{pattern: "GET /v1/operations", api: func(s *Server) apiFunc { return s.listOperations }},
	{pattern: "GET /v1/operations/{id}", api: func(s *Server) apiFunc { return s.getOperation }},
	{pattern: "GET /v1/tokens", api: func(s *Server) apiFunc { return s.listTokens }},
	{pattern: "POST /v1/tokens", api: func(s *Server) apiFunc { return s.createToken }},
	{pattern: "DELETE /v1/tokens/{id}", api: func(s *Server) apiFunc { return s.revokeToken }},
}

// Handler is the whole HTTP surface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range routes {
		if rt.api != nil {
			mux.Handle(rt.pattern, s.v1(rt.pattern, rt.api(s)))
		} else {
			mux.HandleFunc(rt.pattern, rt.h(s))
		}
	}
	return mux
}

// ---- The /v1 middleware -----------------------------------------------------

type recorder struct {
	http.ResponseWriter
	code int
}

func (r *recorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) v1(pattern string, h apiFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, rec := audit.WithRecord(r.Context())
		r = r.WithContext(ctx)
		rw := &recorder{ResponseWriter: w, code: 200}
		rec.Set(func(e *audit.Event) { e.Action = pattern })
		defer func() {
			e := rec.Event()
			e.Status = rw.code
			if e.Result == "" {
				e.Result = "ok"
				if rw.code >= 400 {
					e.Result = "error"
				}
			}
			s.audit.Write(e)
			s.metrics.Inc("hangar_http_requests_total", pattern, strconv.Itoa(rw.code))
		}()
		r.Body = http.MaxBytesReader(rw, r.Body, 1<<20)

		id, err := s.auth.Authenticate(r)
		if err != nil {
			var ie *identity.Error
			if errors.As(err, &ie) {
				rec.Set(func(e *audit.Event) { e.Result, e.Reason, e.Detail = "refused", "auth", ie.Message })
				s.metrics.Inc("hangar_requests_refused_total", "auth")
				rw.Header().Set("WWW-Authenticate", `Bearer realm="hangar"`)
				writeProblem(rw, &core.Problem{Status: 401, Kind: core.KindSignIn, Detail: ie.Message})
				return
			}
			rec.Set(func(e *audit.Event) { e.Result, e.Reason, e.Detail = "error", "identity-provider", err.Error() })
			writeProblem(rw, &core.Problem{Status: 503, Kind: core.KindDown, Detail: err.Error()})
			return
		}
		rec.Set(func(e *audit.Event) { e.Actor, e.Name, e.Via = id.Subject, id.Name, id.ViaLabel() })
		who, p := s.core.Caller(ctx, id)
		if p != nil {
			writeProblem(rw, p)
			return
		}
		if r.Method == http.MethodGet && !who.Can(identity.ScopeRead) {
			writeProblem(rw, &core.Problem{Status: 403, Kind: core.KindScope, Detail: "this token cannot read"})
			return
		}
		if err := h(rw, r, who); err != nil {
			var p *core.Problem
			if !errors.As(err, &p) {
				s.log.Error("request failed", "route", pattern, "err", err)
				rec.Set(func(e *audit.Event) { e.Result, e.Detail = "error", err.Error() })
				p = &core.Problem{Status: 500, Kind: core.KindInternal, Detail: "the brain failed to answer; the operator's logs say why"}
			}
			writeProblem(rw, p)
		}
	})
}

var titles = map[string]string{
	core.KindSignIn:      "Sign in",
	core.KindNoTier:      "Not in any tier",
	core.KindScope:       "Not with this token",
	core.KindNotFound:    "Not found",
	core.KindBadRequest:  "Bad request",
	core.KindZone:        "Zone not open to you",
	core.KindUnavailable: "Not offered there",
	core.KindSchema:      "Does not fit the schema",
	core.KindPlugin:      "Refused by the plugin",
	core.KindLimit:       "Over your limits",
	core.KindBusy:        "Busy",
	core.KindConflict:    "Conflict",
	core.KindEngine:      "Refused by the engine",
	core.KindDown:        "A plugin is down",
	core.KindInternal:    "Internal error",
}

func writeProblem(w http.ResponseWriter, p *core.Problem) {
	body := map[string]any{
		"type": "urn:hangar:problem:" + p.Kind, "title": titles[p.Kind], "status": p.Status,
		"detail": p.Detail, "kind": p.Kind,
	}
	if len(p.Violations) > 0 {
		body["violations"] = p.Violations
	}
	if len(p.Refusals) > 0 {
		body["refusals"] = p.Refusals
	}
	if p.Operation != "" {
		body["operation"] = p.Operation
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, code int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	return json.NewEncoder(w).Encode(v)
}

// decode reads a JSON body strictly: an unknown field is a typo, refused.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: "the request has no body"}
		}
		return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: "the body is not the JSON this call takes: " + err.Error()}
	}
	return nil
}

// ---- The house contract -----------------------------------------------------

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	var why []string
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		why = append(why, "the registry does not answer: "+err.Error())
	}
	for _, p := range s.host.Plugins() {
		if !p.Up() {
			why = append(why, "plugin "+p.Name+" is not running")
		}
	}
	body := map[string]any{"status": "ok", "version": s.version}
	code := 200
	if len(why) > 0 {
		body["status"], body["why"], code = "degraded", why, 503
	}
	_ = writeJSON(w, code, body)
}

func (s *Server) metricsPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP hangar_build_info The running version.\n# TYPE hangar_build_info gauge\n")
	metrics.Gauge(w, "hangar_build_info", []string{"version"}, []string{s.version}, 1)
	fmt.Fprintf(w, "# HELP hangar_plugin_up Whether each plugin's process runs.\n# TYPE hangar_plugin_up gauge\n")
	for _, p := range s.host.Plugins() {
		up := 0.0
		if p.Up() {
			up = 1
		}
		metrics.Gauge(w, "hangar_plugin_up", []string{"plugin"}, []string{p.Name}, up)
	}
	if counts, err := s.store.Counts(r.Context()); err == nil {
		fmt.Fprintf(w, "# HELP hangar_resources Resources in the registry, by type and state.\n# TYPE hangar_resources gauge\n")
		keys := make([][2]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b [2]string) int { return strings.Compare(a[0]+a[1], b[0]+b[1]) })
		for _, k := range keys {
			metrics.Gauge(w, "hangar_resources", []string{"type", "state"}, k[:], float64(counts[k]))
		}
	}
	s.metrics.Write(w)
}

func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(s.spec)
}

func (s *Server) mark(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(ui.Lockup(s.cfg.House))
}

var front = template.Must(template.New("front").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0b0d10;color:#c9d1d9;font:16px/1.5 system-ui,sans-serif}
main{max-width:40rem;padding:2rem 1rem;text-align:center}img{width:100%;max-width:32rem}a{color:#7ee0a1}code{font-size:.9em}</style></head>
<body><main><img src="/mark.svg" alt="{{.Title}}">
<p>This is a small cloud's brain. Its API is under <code>/v1</code>; the contract is <a href="/openapi.json">/openapi.json</a>.</p>
<p>Version <code>{{.Version}}</code>.</p></main></body></html>`))

func (s *Server) front(w http.ResponseWriter, _ *http.Request) {
	title := "Le Hangar"
	if s.cfg.House != "" {
		title = s.cfg.House + " — le hangar"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = front.Execute(w, map[string]string{"Title": title, "Version": s.version})
}

// ---- The catalogue ------------------------------------------------------

func (s *Server) whoami(w http.ResponseWriter, _ *http.Request, who *core.Caller) error {
	groups := who.Groups
	if groups == nil {
		groups = []string{}
	}
	scopes := who.Scopes
	if scopes == nil {
		scopes = []string{identity.ScopeRead, identity.ScopeWrite}
	}
	return writeJSON(w, 200, map[string]any{
		"subject": who.Subject, "name": who.Name, "groups": groups, "tier": who.Tier.Name,
		"operator": who.Tier.Operator, "via": who.ViaLabel(), "scopes": scopes,
	})
}

func (s *Server) types(w http.ResponseWriter, _ *http.Request, who *core.Caller) error {
	return writeJSON(w, 200, map[string]any{"types": s.core.Types(who)})
}

func (s *Server) zones(w http.ResponseWriter, _ *http.Request, who *core.Caller) error {
	return writeJSON(w, 200, map[string]any{"zones": s.core.Zones(who)})
}

func (s *Server) limits(w http.ResponseWriter, r *http.Request, who *core.Caller) error {
	tier, ls, err := s.core.Limits(r.Context(), who)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"tier": tier, "limits": ls})
}

// ---- Resources ----------------------------------------------------------

func (s *Server) listResources(w http.ResponseWriter, r *http.Request, who *core.Caller) error {
	q := r.URL.Query()
	f := registry.Filter{Type: q.Get("type"), Zone: q.Get("zone"), Owner: q.Get("owner"), States: q["state"], Tags: map[string]string{}}
	for _, st := range f.States {
		if !slices.Contains([]string{registry.Creating, registry.Ready, registry.Updating, registry.Deleting, registry.Deleted, registry.Failed, registry.Lost}, st) {
			return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: fmt.Sprintf("no state %q", st)}
		}
	}
	for _, t := range q["tag"] {
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: "a tag filter reads key=value"}
		}
		f.Tags[k] = v
	}
	var err error
	if a := q.Get("after"); a != "" {
		if f.After, err = strconv.ParseInt(a, 10, 64); err != nil || f.After < 0 {
			return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: "after is the next of the previous page"}
		}
	}
	if l := q.Get("limit"); l != "" {
		if f.Limit, err = strconv.Atoi(l); err != nil || f.Limit < 1 || f.Limit > 1000 {
			return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: "limit is 1 to 1000"}
		}
	}
	rs, next, err := s.core.List(r.Context(), who, f)
	if err != nil {
		return err
	}
	if rs == nil {
		rs = []*registry.Resource{}
	}
	body := map[string]any{"resources": rs}
	if next > 0 {
		body["next"] = next
	}
	return writeJSON(w, 200, body)
}

type createBody struct {
	Type string `json:"type"`
	core.CreateInput
}

func (s *Server) createResource(w http.ResponseWriter, r *http.Request, who *core.Caller) error {
	var in createBody
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Type == "" {
		return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: "name a type (GET /v1/types lists them)"}
	}
	if len(in.ClientToken) > 64 {
		return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: "a client token is at most 64 characters"}
	}
	op, res, replayed, err := s.core.Create(r.Context(), who, in.Type, in.CreateInput)
	return accepted(w, op, res, replayed, err)
}

func accepted(w http.ResponseWriter, op *registry.Operation, res *registry.Resource, replayed bool, err error) error {
	if err != nil {
		return err
	}
	code := 202
	if replayed {
		code = 200
	}
	return writeJSON(w, code, map[string]any{"operation": op, "resource": res})
}

func (s *Server) getResource(w http.ResponseWriter, r *http.Request, who *core.Caller) error {
	res, err := s.core.Get(r.Context(), who, r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, res)
}

func (s *Server) deleteResource(w http.ResponseWriter, r *http.Request, who *core.Caller) error {
	token := r.URL.Query().Get("client_token")
	if len(token) > 64 {
		return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: "a client token is at most 64 characters"}
	}
	op, res, replayed, err := s.core.Delete(r.Context(), who, r.PathValue("id"), token)
	return accepted(w, op, res, replayed, err)
}

func (s *Server) act(w http.ResponseWriter, r *http.Request, who *core.Caller) error {
	var in core.ActInput
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			return err
		}
	}
	if len(in.ClientToken) > 64 {
		return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: "a client token is at most 64 characters"}
	}
	op, res, replayed, err := s.core.Act(r.Context(), who, r.PathValue("id"), r.PathValue("action"), in)
	return accepted(w, op, res, replayed, err)
}

// ---- Operations ---------------------------------------------------------

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request, who *core.Caller) error {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ops, err := s.core.Operations(r.Context(), who, r.URL.Query().Get("resource"), limit)
	if err != nil {
		return err
	}
	if ops == nil {
		ops = []*registry.Operation{}
	}
	return writeJSON(w, 200, map[string]any{"operations": ops})
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request, who *core.Caller) error {
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 60 {
			return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: "wait is 0 to 60 seconds"}
		}
		wait = time.Duration(n) * time.Second
	}
	op, err := s.core.Operation(r.Context(), who, r.PathValue("id"), wait)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, op)
}

// ---- Tokens -------------------------------------------------------------

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request, who *core.Caller) error {
	toks, err := s.store.Tokens(r.Context(), who.Subject)
	if err != nil {
		return err
	}
	if toks == nil {
		toks = []*registry.Token{}
	}
	return writeJSON(w, 200, map[string]any{"tokens": toks})
}

type tokenBody struct {
	Name      string   `json:"name"`
	ExpiresIn int64    `json:"expires_in"`
	Scopes    []string `json:"scopes"`
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, who *core.Caller) error {
	if who.Via != "oidc" {
		return &core.Problem{Status: 403, Kind: core.KindScope, Detail: "a token cannot make tokens: sign in through the identity provider"}
	}
	var in tokenBody
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Name == "" || len(in.Name) > 64 {
		return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: "name the token (1 to 64 characters)"}
	}
	if in.ExpiresIn < 60 {
		return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: "expires_in is at least 60 seconds"}
	}
	secret, tok, err := identity.Mint(r.Context(), s.store, who.Subject, in.Name, who.Groups, in.Scopes,
		time.Duration(in.ExpiresIn)*time.Second, s.cfg.Identity.Tokens.MaxTTL)
	var re *identity.RequestError
	if errors.As(err, &re) {
		return &core.Problem{Status: 400, Kind: core.KindBadRequest, Detail: re.Message}
	}
	if err != nil {
		return err
	}
	audit.From(r.Context()).Set(func(e *audit.Event) { e.Resource, e.Result = tok.ID, "made" })
	return writeJSON(w, 201, map[string]any{"token": tok, "secret": secret})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, who *core.Caller) error {
	owner := who.Subject
	if who.Tier.Operator {
		owner = ""
	}
	id := r.PathValue("id")
	audit.From(r.Context()).Set(func(e *audit.Event) { e.Resource = id })
	if err := s.store.RevokeToken(r.Context(), owner, id); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return &core.Problem{Status: 404, Kind: core.KindNotFound, Detail: "no live token " + id}
		}
		return err
	}
	w.WriteHeader(204)
	return nil
}
