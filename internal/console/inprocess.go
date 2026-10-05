package console

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
)

// InProcess reaches a brain that is this very process: its handler, called as
// a request over the network would call it. The console inside `hangar
// serve` asks the API through it — the same calls, the same checks, the same
// audit lines as from anywhere else.
func InProcess(h http.Handler) *http.Client {
	return &http.Client{Transport: inProcess{h}}
}

// InProcessURL is the address such a client's requests are written to.
const InProcessURL = "http://brain"

type inProcess struct{ h http.Handler }

func (t inProcess) RoundTrip(req *http.Request) (*http.Response, error) {
	// a transport leaves the request it was given as it is
	req = req.Clone(req.Context())
	if req.Body == nil {
		req.Body = http.NoBody
	}
	// a handler reads these two where a client's request leaves them empty
	req.RequestURI = req.URL.RequestURI()
	if req.RemoteAddr == "" {
		req.RemoteAddr = "console"
	}
	if req.Header.Get("Upgrade") != "" {
		return t.upgrade(req)
	}
	rec := &answer{header: http.Header{}, code: http.StatusOK}
	t.h.ServeHTTP(rec, req)
	return &http.Response{
		StatusCode: rec.code, Status: http.StatusText(rec.code),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: rec.header, Body: io.NopCloser(&rec.body), ContentLength: int64(rec.body.Len()), Request: req,
	}, nil
}

// answer is what a handler wrote.
type answer struct {
	header http.Header
	code   int
	wrote  bool
	body   bytes.Buffer
}

func (a *answer) Header() http.Header { return a.header }

func (a *answer) WriteHeader(code int) {
	if !a.wrote {
		a.code, a.wrote = code, true
	}
}

func (a *answer) Write(p []byte) (int, error) {
	a.wrote = true
	return a.body.Write(p)
}

// upgrade is a call that asks to switch protocols (a stream's WebSocket):
// over the network the handler would take the connection over; here it is
// handed one end of a pipe, and the caller the other, as the answer's body.
// A handler that answers without switching is an ordinary answer.
func (t inProcess) upgrade(req *http.Request) (*http.Response, error) {
	near, far := net.Pipe()
	rec := &switching{answer: answer{header: http.Header{}, code: http.StatusOK}, conn: far, taken: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		t.h.ServeHTTP(rec, req)
	}()
	select {
	case <-rec.taken:
		return &http.Response{
			StatusCode: http.StatusSwitchingProtocols, Status: "101 Switching Protocols",
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: rec.header, Body: near, ContentLength: -1, Request: req,
		}, nil
	case <-done:
		near.Close()
		far.Close()
		return &http.Response{
			StatusCode: rec.code, Status: http.StatusText(rec.code),
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: rec.header, Body: io.NopCloser(&rec.body), ContentLength: int64(rec.body.Len()), Request: req,
		}, nil
	}
}

// switching is an answer whose handler may take the connection over.
type switching struct {
	answer
	conn  net.Conn
	once  sync.Once
	taken chan struct{}
}

func (s *switching) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	took := false
	s.once.Do(func() { took = true; close(s.taken) })
	if !took {
		return nil, nil, errors.New("the connection was taken over already")
	}
	return s.conn, bufio.NewReadWriter(bufio.NewReader(s.conn), bufio.NewWriter(s.conn)), nil
}
