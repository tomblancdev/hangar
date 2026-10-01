package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// logBuffer is a process's output, read while the process still writes it.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *logBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// The console as its own process, as an operator puts it on a public door:
// a brain that serves no console, and `hangar console` in front of it —
// started with no config file, no registry, no plugin's key, knowing the
// brain's address and nothing else. A browser signs in through it and asks
// for a box; the brain is reached for the API alone.
func TestTheBinaryConsoleOnItsOwn(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	dir, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	bin := filepath.Join(dir, "hangar")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cfg := filepath.Join(dir, "hangar.yaml")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, `
data_dir: %s/data
console: {enabled: false}
tiers:
  - name: users
    groups: [users]
    zones: [z]
    limits: {toy.boxes: 1, toy.cores: 2, toy.memory_gb: 2, toy.kind: [container]}
zones:
  - {name: z, driver: fake, endpoint: %s/zone.json}
plugins:
  - {name: toy, builtin: toy, zones: [z]}
reconcile: {every: 5s}
`, dir, dir), 0o600); err != nil {
		t.Fatal(err)
	}
	mint := exec.Command(bin, "token", "create", "--subject", "alice", "--groups", "users", "--name", "e2e", "--ttl", "1h", "--config", cfg)
	out, err := mint.Output()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	secret := strings.TrimSpace(string(out))

	free := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}
	start := func(logs *logBuffer, env []string, args ...string) *exec.Cmd {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = logs, logs
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd
	}
	healthy := func(base string, logs *logBuffer) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			if resp, err := http.Get(base + "/healthz"); err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never healthy:\n%s", base, logs.String())
			}
		}
	}

	brainAddr, consoleAddr := free(), free()
	var brainLogs, consoleLogs logBuffer
	start(&brainLogs, append(os.Environ(), "HANGAR_LISTEN="+brainAddr), "serve", "--config", cfg)
	healthy("http://"+brainAddr, &brainLogs)

	// the brain itself serves no console
	if resp, err := http.Get("http://" + brainAddr + "/console/"); err != nil || resp.StatusCode != 404 {
		t.Fatalf("a brain with its console off still serves one: %v", resp)
	}
	resp, err := http.Get("http://" + brainAddr + "/")
	if err != nil {
		t.Fatal(err)
	}
	front, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(front), "/mark.svg") {
		t.Fatalf("its front page: %s", front)
	}

	// the console: an environment of its own, emptied of everything but PATH —
	// it is handed the brain's address, and nothing of the brain's
	con := start(&consoleLogs, []string{"PATH=" + os.Getenv("PATH")}, "console", "--brain", "http://"+brainAddr, "--listen", consoleAddr, "--house", "Example House")
	base := "http://" + consoleAddr
	healthy(base, &consoleLogs)

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	ask := func(method, path string, body any, csrf string) (int, map[string]any, string) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, base+path, rd)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if csrf != "" {
			req.Header.Set("X-Hangar-Csrf", csrf)
		}
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		out := map[string]any{}
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out, string(raw)
	}

	if code, _, page := ask("GET", "/", nil, ""); code != 200 || !strings.Contains(page, `id="app"`) {
		t.Fatalf("its address does not lead to the app: %d", code)
	}
	if _, _, mark := ask("GET", "/console/mark.svg", nil, ""); !strings.Contains(mark, "Example House") {
		t.Fatal("the mark does not carry the house word it was given")
	}
	if code, s, _ := ask("GET", "/console/session", nil, ""); code != 200 || s["how"] != "token" {
		t.Fatalf("session: %d %v", code, s)
	}
	code, s, _ := ask("POST", "/console/signin/token", map[string]string{"token": secret}, "")
	if code != 200 {
		t.Fatalf("sign-in: %d %v", code, s)
	}
	csrf := s["csrf"].(string)
	if code, me, _ := ask("GET", "/console/api/v1/whoami", nil, ""); code != 200 || me["subject"] != "alice" {
		t.Fatalf("whoami: %d %v", code, me)
	}
	code, acc, raw := ask("POST", "/console/api/v1/resources", map[string]any{"type": "box", "zone": "z", "spec": map[string]any{"cores": 1}}, csrf)
	if code != 202 {
		t.Fatalf("create: %d %s", code, raw)
	}
	op := acc["operation"].(map[string]any)["id"].(string)
	if _, o, raw := ask("GET", "/console/api/v1/operations/"+op+"?wait=20", nil, ""); o["state"] != "succeeded" {
		t.Fatalf("the create: %s", raw)
	}
	if code, r, _ := ask("POST", "/console/api/v1/resources", map[string]any{"type": "box", "zone": "z"}, csrf); code != 403 ||
		r["detail"] != "1 of 1 toy.boxes used; this asks for 1 more" {
		t.Fatalf("the second box: %d %v", code, r)
	}
	if code, h, _ := ask("GET", "/healthz", nil, ""); code != 200 || h["sessions"] != float64(1) {
		t.Fatalf("its health: %d %v", code, h)
	}
	// the brain's audit names alice for what the console asked; the token is in neither log
	if !strings.Contains(brainLogs.String(), `"action":"POST /v1/resources","actor":"alice"`) {
		t.Error("the brain's audit does not name alice")
	}
	if strings.Contains(brainLogs.String()+consoleLogs.String(), secret) {
		t.Error("a log holds the API token")
	}

	// without its brain it says so, and stops cleanly when told
	if err := con.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := con.Wait(); err != nil {
		t.Fatalf("the console did not stop cleanly: %v\n%s", err, consoleLogs.String())
	}
	if err := exec.Command(bin, "console").Run(); err == nil {
		t.Fatal("a console started with no brain to reach")
	}
}
