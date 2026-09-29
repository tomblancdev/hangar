package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The binary as an operator runs it: built, checked, a first token made on
// its host, served, asked for a box, its engine edited behind its back, and
// stopped with SIGTERM.
func TestTheBinary(t *testing.T) {
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
	engine := filepath.Join(dir, "zone.json")
	cfg := filepath.Join(dir, "hangar.yaml")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, `
data_dir: %s/data
tiers:
  - name: users
    groups: [users]
    zones: [z]
    limits: {toy.boxes: 1, toy.cores: 2, toy.memory_gb: 2, toy.kind: [container]}
zones:
  - {name: z, driver: fake, endpoint: %s}
plugins:
  - {name: toy, builtin: toy, zones: [z]}
reconcile: {every: 5s}
`, dir, engine), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command(bin, append(args, "--config", cfg)...)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		if err != nil {
			return out.String(), fmt.Errorf("%v: %s", err, errb.String())
		}
		return out.String(), nil
	}

	out, err := run("check")
	if err != nil || !strings.Contains(out, "sound:") || !strings.Contains(out, "box") {
		t.Fatalf("check: %v\n%s", err, out)
	}
	secret, err := run("token", "create", "--subject", "alice", "--groups", "users", "--name", "e2e", "--ttl", "1h")
	if err != nil || !strings.HasPrefix(secret, "hgr_") {
		t.Fatalf("token: %v %q", err, secret)
	}
	secret = strings.TrimSpace(secret)
	if byHand := exec.Command(bin, "plugin", "toy"); byHand.Run() == nil {
		t.Fatal("a plugin ran by hand, outside the core")
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	var logs bytes.Buffer
	srv := exec.Command(bin, "serve", "--config", cfg)
	srv.Env = append(os.Environ(), "HANGAR_LISTEN="+addr)
	srv.Stdout, srv.Stderr = &logs, &logs
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	t.Cleanup(func() { _ = srv.Process.Kill() })

	base := "http://" + addr
	call := func(method, path string, body any) (int, map[string]any) {
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		req, _ := http.NewRequest(method, base+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	for deadline := time.Now().Add(15 * time.Second); ; {
		if code, _ := call("GET", "/healthz", nil); code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never healthy:\n%s", logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	code, acc := call("POST", "/v1/resources", map[string]any{"type": "box", "zone": "z", "spec": map[string]any{"cores": 2}})
	if code != 202 {
		t.Fatalf("create: %d %v", code, acc)
	}
	id := acc["resource"].(map[string]any)["id"].(string)
	op := acc["operation"].(map[string]any)["id"].(string)
	if _, o := call("GET", "/v1/operations/"+op+"?wait=20", nil); o["state"] != "succeeded" {
		t.Fatalf("operation: %v", o)
	}
	if code, r := call("POST", "/v1/resources", map[string]any{"type": "box", "zone": "z"}); code != 403 ||
		r["detail"] != "1 of 1 toy.boxes used; this asks for 1 more" {
		t.Fatalf("the second box: %d %v", code, r)
	}

	// the engine loses the box: the next reconcile says so
	if err := os.WriteFile(engine, []byte(`{"seq": 1, "guests": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; {
		if _, r := call("GET", "/v1/resources/"+id, nil); r["state"] == "lost" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never found lost:\n%s", logs.String())
		}
		time.Sleep(250 * time.Millisecond)
	}

	_ = srv.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("SIGTERM: %v\n%s", err, logs.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("still running 15 s after SIGTERM")
	}
	if !strings.Contains(logs.String(), `"kind":"audit"`) {
		t.Fatal("no audit line")
	}
}
