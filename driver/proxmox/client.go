package proxmox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// client speaks Proxmox VE's HTTP API (/api2/json) with one API token.
//
// Two things about that API shape everything here, both read on a live
// cluster before this was written:
//
//   - A refusal's words are the answer, not its code. The same refusal can
//     come back as a 403 on one kind of guest and a 500 on the other; the
//     reason is in the status line and the body's "message".
//   - A long call answers 200 with a task id at once, and fails LATER, inside
//     the task. A start a pre-start hook refused is a 200 and a task whose
//     exit status is the refusal — so every such call waits for its task and
//     reads how it ended.
type client struct {
	base string // https://host:8006/api2/json
	auth string // PVEAPIToken=user@realm!name=secret
	http *http.Client
	poll time.Duration
}

// apiError is a call Proxmox refused.
type apiError struct {
	Status  int
	Message string
	Params  map[string]string // per-parameter reasons, on a 400
}

func (e *apiError) Error() string {
	msg := e.Message
	if len(e.Params) > 0 {
		var parts []string
		for k, v := range e.Params {
			parts = append(parts, k+": "+strings.TrimSpace(v))
		}
		msg += " (" + strings.Join(parts, "; ") + ")"
	}
	return fmt.Sprintf("proxmox %d: %s", e.Status, msg)
}

// taskError is a task that ended in anything but OK.
type taskError struct {
	Exit string   // the task's exit status, Proxmox's own words
	Log  []string // the end of its log (a hook's stderr is there)
}

func (e *taskError) Error() string {
	if len(e.Log) == 0 {
		return e.Exit
	}
	return strings.TrimSpace(e.Exit) + " — " + strings.Join(e.Log, " | ")
}

// tlsConfig builds the TLS settings for the options: a CA bundle to verify
// the chain (and the name, or tls_server_name), or a pinned SHA-256
// fingerprint of the server's certificate, or the system's roots.
func tlsConfig(opt map[string]string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: opt["tls_server_name"]}
	caFile, fp := opt["ca_file"], opt["fingerprint"]
	switch {
	case caFile != "" && fp != "":
		return nil, errors.New("ca_file or fingerprint, not both")
	case caFile != "":
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s holds no certificate", caFile)
		}
		cfg.RootCAs = pool
	case fp != "":
		want, err := hex.DecodeString(strings.ReplaceAll(strings.ToLower(fp), ":", ""))
		if err != nil || len(want) != sha256.Size {
			return nil, errors.New("fingerprint: a SHA-256 in hex (colons allowed)")
		}
		// The pin replaces the chain and the name: the certificate is the
		// one expected, or the connection ends.
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("the server showed no certificate")
			}
			got := sha256.Sum256(raw[0])
			if !bytes.Equal(got[:], want) {
				return fmt.Errorf("the server's certificate is not the pinned one (it is %X)", got)
			}
			return nil
		}
	}
	return cfg, nil
}

func newClient(endpoint string, token []byte, opt map[string]string) (*client, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("endpoint %q: the API's URL, https://host:8006", endpoint)
	}
	tok := strings.TrimSpace(string(token))
	if i := strings.Index(tok, "="); i < 0 || !strings.Contains(tok[:i], "!") || i == len(tok)-1 {
		return nil, errors.New("the credential is an API token: user@realm!name=secret")
	}
	tc, err := tlsConfig(opt)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	tr.Proxy = nil
	return &client{
		base: u.String() + "/api2/json",
		auth: "PVEAPIToken=" + tok,
		http: &http.Client{Transport: tr, Timeout: 60 * time.Second},
		poll: time.Second,
	}, nil
}

// call makes one request. GET and DELETE carry params in the query; POST and
// PUT as a form. out receives the response's "data".
func (c *client) call(ctx context.Context, method, path string, params url.Values, out any) error {
	u := c.base + path
	var body io.Reader
	if method == http.MethodGet || method == http.MethodDelete {
		if len(params) > 0 {
			u += "?" + params.Encode()
		}
	} else if params != nil {
		body = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return c.send(req, out)
}

// upload sends one file as multipart form data (the storage upload call).
func (c *client) upload(ctx context.Context, path string, fields map[string]string, filename string, data []byte, out any) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	fw, err := w.CreateFormFile("filename", filename)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return c.send(req, out)
}

func (c *client) send(req *http.Request, out any) error {
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return &unreachable{err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return &unreachable{err}
	}
	var env struct {
		Data    json.RawMessage   `json:"data"`
		Message string            `json:"message"`
		Errors  map[string]string `json:"errors"`
	}
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			// Proxmox puts its reason in the status line itself
			msg = strings.TrimSpace(strings.TrimPrefix(resp.Status, fmt.Sprint(resp.StatusCode)))
		}
		return &apiError{Status: resp.StatusCode, Message: msg, Params: env.Errors}
	}
	if out == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("proxmox %s %s: an answer this driver does not read: %w", req.Method, req.URL.Path, err)
	}
	return nil
}

// unreachable: the API did not answer at all.
type unreachable struct{ err error }

func (e *unreachable) Error() string { return "the Proxmox API did not answer: " + e.err.Error() }
func (e *unreachable) Unwrap() error { return e.err }

// run makes a call that starts a task, and waits for the task to end.
func (c *client) run(ctx context.Context, method, path string, params url.Values) error {
	var upid string
	if err := c.call(ctx, method, path, params, &upid); err != nil {
		return err
	}
	return c.wait(ctx, upid)
}

// wait polls a task until it has stopped; an exit status other than OK (or
// OK with warnings) is a taskError carrying the end of its log.
func (c *client) wait(ctx context.Context, upid string) error {
	if !strings.HasPrefix(upid, "UPID:") {
		return nil // a call that finished in place
	}
	node := strings.Split(upid, ":")[1]
	base := "/nodes/" + url.PathEscape(node) + "/tasks/" + url.PathEscape(upid)
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := c.call(ctx, http.MethodGet, base+"/status", nil, &st); err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus == "OK" || strings.HasPrefix(st.ExitStatus, "WARNINGS") {
				return nil
			}
			var lines []struct {
				N int    `json:"n"`
				T string `json:"t"`
			}
			_ = c.call(ctx, http.MethodGet, base+"/log", url.Values{"start": {"0"}, "limit": {"500"}}, &lines)
			var tail []string
			for _, l := range lines {
				if t := strings.TrimSpace(l.T); t != "" && t != "TASK ERROR: "+st.ExitStatus && !strings.HasPrefix(t, "TASK ") {
					tail = append(tail, t)
				}
			}
			if len(tail) > 5 {
				tail = tail[len(tail)-5:]
			}
			return &taskError{Exit: st.ExitStatus, Log: tail}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.poll):
		}
	}
}
