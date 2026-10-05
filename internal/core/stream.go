package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/tomblancdev/hangar/internal/audit"
	"github.com/tomblancdev/hangar/internal/identity"
	"github.com/tomblancdev/hangar/internal/registry"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// A stream is something of a resource held open, bytes both ways: a
// machine's terminal. The core opens it and relays it; its plugin carries it
// to the engine. Three rules are the core's, whatever the plugin:
//
//   - Its owner alone. A stream reaches INSIDE a resource: an operator sees a
//     machine, stops it, deletes it, and does not enter it; someone a
//     resource is shared with names it and nothing more. Asked with the
//     owner's own credential, one that may write.
//   - One at a time per resource and stream. A second opening ends the
//     first, which is told — a tab left open on another screen never holds a
//     machine's terminal against its owner.
//   - Audited at both ends: who opened which resource's stream, when, and —
//     when it closes — for how long, how many bytes each way, and why it
//     ended. Never a byte of what passed.

// Stream is one of a resource's streams, open.
type Stream struct {
	c    *Core
	key  string
	rpc  grpc.BidiStreamingClient[pluginpb.OpenRequest, pluginpb.OpenResponse]
	stop context.CancelFunc
	ev   audit.Event // the opening's line: the closing's is drawn from it
	at   time.Time

	smu sync.Mutex // one send at a time

	mu     sync.Mutex
	in     int64 // bytes its owner sent
	out    int64 // bytes the resource said
	ended  bool
	why    string // why it ended, for whoever reads next
	closed chan struct{}
}

// StreamEnd is how a stream's Recv ends: why, in words its owner reads.
type StreamEnd struct {
	Reason string
	// Taken: it ended because its owner opened it again elsewhere.
	Taken bool
}

func (e *StreamEnd) Error() string { return e.Reason }

type streams struct {
	mu   sync.Mutex
	open map[string]*Stream
}

// streamOpenTimeout bounds a stream's opening at its engine.
const streamOpenTimeout = 30 * time.Second

// streamTaken is what the older of two openings is told.
const streamTaken = "opened elsewhere: it is open in one place at a time"

// OpenStream opens one of a resource's streams for its owner. size is the
// owner's window, in characters (zero: unknown).
func (c *Core) OpenStream(ctx context.Context, who *Caller, id, name string, cols, rows int) (*Stream, error) {
	audit.From(ctx).Set(func(e *audit.Event) { e.Fields = map[string]string{"stream": name} })
	if !who.Can(identity.ScopeWrite) {
		return nil, c.refused(ctx, problem(403, KindScope, "this token is read-only: a stream is not opened with it"))
	}
	r, t, p := c.resourceFor(ctx, who, id, false)
	if p != nil {
		return nil, p
	}
	st := t.Stream(name)
	if st == nil {
		names := []string{}
		for _, x := range t.Streams {
			names = append(names, x.Name)
		}
		has := "it has none"
		if len(names) > 0 {
			has = "it has: " + strings.Join(names, ", ")
		}
		return nil, c.refused(ctx, problem(404, KindNotFound, "type %s has no stream %q (%s)", t.Name, name, has))
	}
	// its owner alone: not an operator, not someone it is shared with
	if r.Owner != who.Subject {
		return nil, c.refused(ctx, problem(403, KindOwner,
			"%s is %s's: its %s is its owner's alone — nobody else enters it, an operator included", r.ID, r.Owner, name))
	}
	switch r.State {
	case registry.Ready:
	case registry.Lost:
		return nil, c.refused(ctx, problem(409, KindEngine, "%s is lost: its engine no longer has it — delete it", r.ID))
	default:
		if p := c.busy(ctx, r); p != nil {
			return nil, p
		}
		return nil, c.refused(ctx, problem(409, KindEngine, "%s is %s", r.ID, r.State))
	}
	if ok, why := c.host.StreamAvailable(t, st, r.Zone); !ok {
		return nil, c.refused(ctx, problem(422, KindUnavailable, "%s", why))
	}
	client, err := c.host.Plugin(t.Plugin).Client(ctx)
	if err != nil {
		return nil, c.refused(ctx, problem(503, KindDown, "%v", err))
	}
	// one at a time: whoever holds it now is told, and let go of, BEFORE the
	// engine is asked for another — a machine's port has one other end
	key := r.ID + "/" + name
	c.streams.mu.Lock()
	held := c.streams.open[key]
	c.streams.mu.Unlock()
	if held != nil {
		held.end(streamTaken, true)
	}
	// the stream outlives the request that opened it, and ends with the brain
	sctx, stop := context.WithCancel(c.life)
	rpc, err := client.Open(sctx)
	if err == nil {
		open := &pluginpb.StreamOpen{Resource: c.proto(ctx, r), Stream: name}
		if cols > 0 && rows > 0 {
			open.Size = &pluginpb.StreamSize{Cols: uint32(cols), Rows: uint32(rows)}
		}
		err = rpc.Send(&pluginpb.OpenRequest{What: &pluginpb.OpenRequest_Open{Open: open}})
	}
	var first *pluginpb.OpenResponse
	if err == nil {
		// the plugin opens it at its engine, or refuses: bounded like any call
		got := make(chan struct{})
		timer := time.AfterFunc(streamOpenTimeout, stop)
		go func() { first, err = rpc.Recv(); close(got) }()
		select {
		case <-got:
		case <-ctx.Done():
			// whoever asked left before it was open: nothing is held for nobody
			stop()
			<-got
		}
		timer.Stop()
	}
	switch {
	case ctx.Err() != nil:
		stop()
		return nil, c.refused(ctx, problem(400, KindBadRequest, "the request was given up before the %s was open", name))
	case err == nil && sctx.Err() != nil:
		err = errors.New("it took too long to open")
	case err == nil && first.GetOpened() == nil:
		err = errors.New("the plugin answered something else than « open »")
	}
	if err != nil {
		stop()
		return nil, c.refused(ctx, fromPlugin(t.Plugin, err))
	}
	s := &Stream{c: c, key: key, rpc: rpc, stop: stop, at: time.Now(), closed: make(chan struct{})}
	audit.From(ctx).Set(func(e *audit.Event) { e.Result = "opened" })
	s.ev = audit.From(ctx).Event()

	c.streams.mu.Lock()
	if c.streams.open == nil {
		c.streams.open = map[string]*Stream{}
	}
	prev := c.streams.open[s.key]
	c.streams.open[s.key] = s
	c.streams.mu.Unlock()
	if prev != nil {
		prev.end(streamTaken, true)
	}
	c.metrics.Inc("hangar_streams_opened_total", t.Name, name)
	return s, nil
}

// Send passes on what its owner sent.
func (s *Stream) Send(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	if err := s.rpc.Send(&pluginpb.OpenRequest{What: &pluginpb.OpenRequest_Data{Data: data}}); err != nil {
		return s.gone()
	}
	s.mu.Lock()
	s.in += int64(len(data))
	s.mu.Unlock()
	return nil
}

// Resize passes on its owner's window.
func (s *Stream) Resize(cols, rows int) error {
	if cols < 1 || rows < 1 || cols > 1000 || rows > 1000 {
		return nil
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	if err := s.rpc.Send(&pluginpb.OpenRequest{What: &pluginpb.OpenRequest_Size{Size: &pluginpb.StreamSize{Cols: uint32(cols), Rows: uint32(rows)}}}); err != nil {
		return s.gone()
	}
	return nil
}

// gone is why a send failed: the stream's own end, when it has one.
func (s *Stream) gone() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.why != "" {
		return &StreamEnd{Reason: s.why}
	}
	return &StreamEnd{Reason: "it is closed"}
}

// Recv is what the resource says next. It ends with a *StreamEnd: why the
// stream is over.
func (s *Stream) Recv() ([]byte, error) {
	for {
		m, err := s.rpc.Recv()
		if err != nil {
			// ended from this side (taken, closed, the brain stopping) — or
			// the plugin's process went away
			s.mu.Lock()
			why, taken := s.why, s.why == streamTaken
			s.mu.Unlock()
			if why == "" {
				why = "its plugin let go of it"
				if s.c.life.Err() != nil {
					why = "the brain is restarting"
				}
				s.end(why, false)
			}
			return nil, &StreamEnd{Reason: why, Taken: taken}
		}
		switch {
		case m.GetClosed() != nil:
			why := m.GetClosed().GetReason()
			if why == "" {
				why = "it was closed"
			}
			s.end(why, false)
			return nil, &StreamEnd{Reason: why}
		case len(m.GetData()) > 0:
			s.mu.Lock()
			s.out += int64(len(m.GetData()))
			s.mu.Unlock()
			return m.GetData(), nil
		}
	}
}

// Close lets go of it from its owner's side, and says why.
func (s *Stream) Close(why string) { s.end(why, false) }

// end ends it once, for the first reason given, and writes the closing.
func (s *Stream) end(why string, taken bool) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended, s.why = true, why
	in, out := s.in, s.out
	close(s.closed)
	s.mu.Unlock()
	s.stop()

	s.c.streams.mu.Lock()
	if s.c.streams.open[s.key] == s {
		delete(s.c.streams.open, s.key)
	}
	s.c.streams.mu.Unlock()

	e := s.ev
	e.Status, e.Result, e.Reason, e.Detail = 0, "closed", "", why
	e.Fields = map[string]string{
		"stream":  s.ev.Fields["stream"],
		"seconds": strconv.FormatFloat(time.Since(s.at).Seconds(), 'f', 1, 64),
		// how much passed, never what
		"bytes_in":  strconv.FormatInt(in, 10),
		"bytes_out": strconv.FormatInt(out, 10),
	}
	if taken {
		e.Reason = "taken"
	}
	s.c.audit.Write(e)
}

// Done is closed once the stream has ended.
func (s *Stream) Done() <-chan struct{} { return s.closed }

// EndStreams ends every stream held open, for one reason: the brain's own
// stop — each closing written before the process leaves.
func (c *Core) EndStreams(why string) {
	c.streams.mu.Lock()
	all := make([]*Stream, 0, len(c.streams.open))
	for _, s := range c.streams.open {
		all = append(all, s)
	}
	c.streams.mu.Unlock()
	for _, s := range all {
		s.end(why, false)
	}
}

// Streams counts what is held open.
func (c *Core) Streams() int {
	c.streams.mu.Lock()
	defer c.streams.mu.Unlock()
	return len(c.streams.open)
}

// String names it in a log line.
func (s *Stream) String() string { return fmt.Sprintf("stream %s", s.key) }
