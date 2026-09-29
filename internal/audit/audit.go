// Package audit writes one event per thing that happened: every API call
// (who, what, which resource, the result, why it was refused), every end of
// an operation, every drift reconcile found. Structured JSON on stdout,
// beside the brain's other logs; the operator ships them wherever their logs
// go and finds them by "kind":"audit".
package audit

import (
	"context"
	"log/slog"
	"sync"
)

// Event is one line of the audit.
type Event struct {
	Action    string // "POST /v1/resources/{type}", "operation", "reconcile", …
	Actor     string // the subject, or "hangar" for the brain's own doing
	Name      string // the actor's display name, when the identity carries one
	Tier      string
	Via       string // "oidc", "token:tok-…"
	Resource  string
	Type      string
	Zone      string
	Operation string
	Status    int    // the HTTP status, for a call
	Result    string // "ok", "refused", "succeeded", "failed", "drift", …
	Reason    string // for a refusal: "auth", "tier", "zone", "limit", "choice", "schema", "plugin", …
	Detail    string
	Fields    map[string]string
}

// Log writes events.
type Log struct {
	l *slog.Logger
}

// New returns an audit log writing through l.
func New(l *slog.Logger) *Log { return &Log{l: l} }

// Write writes one event.
func (a *Log) Write(e Event) {
	attrs := []any{slog.String("kind", "audit"), slog.String("action", e.Action)}
	add := func(k, v string) {
		if v != "" {
			attrs = append(attrs, slog.String(k, v))
		}
	}
	add("actor", e.Actor)
	add("name", e.Name)
	add("tier", e.Tier)
	add("via", e.Via)
	add("resource", e.Resource)
	add("type", e.Type)
	add("zone", e.Zone)
	add("operation", e.Operation)
	if e.Status != 0 {
		attrs = append(attrs, slog.Int("status", e.Status))
	}
	add("result", e.Result)
	add("reason", e.Reason)
	add("detail", e.Detail)
	if len(e.Fields) > 0 {
		f := make([]any, 0, len(e.Fields))
		for k, v := range e.Fields {
			f = append(f, slog.String(k, v))
		}
		attrs = append(attrs, slog.Group("fields", f...))
	}
	a.l.Info("audit", attrs...)
}

// The event of the request in flight: handlers add to it, the middleware
// writes it once the response is sent — one line per call, whatever path the
// handler took.

type ctxKey struct{}

// Record is the event a request is building.
type Record struct {
	mu sync.Mutex
	e  Event
}

// WithRecord returns a context carrying a fresh record.
func WithRecord(ctx context.Context) (context.Context, *Record) {
	r := &Record{}
	return context.WithValue(ctx, ctxKey{}, r), r
}

// From returns the request's record; a detached one when there is none, so
// callers never check.
func From(ctx context.Context) *Record {
	if r, ok := ctx.Value(ctxKey{}).(*Record); ok {
		return r
	}
	return &Record{}
}

// Set edits the record.
func (r *Record) Set(f func(*Event)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(&r.e)
}

// Event returns a copy of the record.
func (r *Record) Event() Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.e
}
