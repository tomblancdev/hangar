package console

import (
	"bytes"
	"io"
	"net/http"
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
