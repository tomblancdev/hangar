package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// A volume through the binary's API on a real Proxmox VE, both plugins
// running, each with its own token: born on one container with a file
// written in it, refused leaving it while it runs (and the container refused
// its delete), moved to a second once the first stops, parked on its shelf,
// plugged back into the first — the file read at every step. Without the
// bench's variables it skips:
//
//	sh tools/bench/bench.sh up && eval "$(sh tools/bench/bench.sh env)" && go test ./cmd/hangar -run BenchAVolume -v
func TestBenchAVolumeThroughTheAPI(t *testing.T) {
	url, volTok := os.Getenv("HANGAR_BENCH_URL"), os.Getenv("HANGAR_BENCH_VOLUMES_TOKEN_FILE")
	if url == "" || volTok == "" {
		t.Skip("no bench (or one set up before the volumes token): sh tools/bench/bench.sh up, then eval its env")
	}
	benchSSH := func(cmd string) (string, error) {
		out, err := exec.Command("ssh", "-i", os.Getenv("HANGAR_BENCH_SSH_KEY"), "-p", os.Getenv("HANGAR_BENCH_SSH_PORT"),
			"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
			"root@127.0.0.1", cmd).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(cmd string) string {
		t.Helper()
		out, err := benchSSH(cmd)
		if err != nil {
			t.Fatalf("on the bench: %s: %v %s", cmd, err, out)
		}
		return out
	}
	archive := must(`ls /var/lib/vz/template/cache/ | grep "^debian-13-standard_.*_$(dpkg --print-architecture)\." | sort -V | tail -1`)

	dir, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	bin := filepath.Join(dir, "hangar")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	subject := fmt.Sprintf("bench-%d", time.Now().UnixNano()) // its own shelf, removed at the end
	cfg := filepath.Join(dir, "hangar.yaml")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, `
data_dir: %[1]s/data
tiers:
  - name: users
    groups: [users]
    zones: [bench]
    limits:
      machines.count: 2
      machines.vcpu_hours: 1000
      machines.vcpu: 4
      machines.memory_gb: 2
      machines.disk_gb: 8
      machines.kind: [container]
      machines.class: [spot]
      volumes.count: 1
      volumes.size_gb: 2
      volumes.backup_gb: 1
zones:
  - name: bench
    driver: proxmox
    endpoint: %[2]s
    options:
      node: pve-bench
      pool: hangar
      images_pool: hangar-images
      storage: local-zfs
      seed_storage: hangar-seeds
      bridge: hbnet
      vmids: 11060-11079
      ca_file: %[3]s
      shelf_archive: local:vztmpl/%[6]s
    room:                     # the bench's priority guest, VM 100: the machines' token reads its power
      memory_gb: 6
      reservations: [{name: priority, memory_gb: 3, while_running: "100"}]
plugins:
  - name: machines
    builtin: machines
    zones: [bench]
    credentials:
      bench: {file: %[4]s}
    settings:
      images:
        debian-13: {vm: debian-13, container: "local:vztmpl/%[6]s"}
  - name: volumes
    builtin: volumes
    zones: [bench]
    credentials:
      bench: {file: %[5]s}
reconcile: {every: 10s}
`, dir, url, os.Getenv("HANGAR_BENCH_CA_FILE"), os.Getenv("HANGAR_BENCH_TOKEN_FILE"), volTok, archive), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		var out, errb bytes.Buffer
		cmd := exec.Command(bin, append(args, "--config", cfg)...)
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return out.String(), fmt.Errorf("%v: %s", err, errb.String())
		}
		return out.String(), nil
	}
	if out, err := run("check"); err != nil || !strings.Contains(out, "volume") {
		t.Fatalf("check: %v\n%s", err, out)
	} else {
		t.Logf("hangar check:\n%s", out)
	}
	secret, err := run("token", "create", "--subject", subject, "--groups", "users", "--name", "bench", "--ttl", "1h")
	if err != nil {
		t.Fatalf("token: %v %s", err, secret)
	}
	secret = strings.TrimSpace(secret)

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

	call := func(method, path string, body any) (int, map[string]any) {
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		req, _ := http.NewRequest(method, "http://"+addr+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, map[string]any{"error": err.Error()}
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	for deadline := time.Now().Add(20 * time.Second); ; {
		if code, _ := call("GET", "/healthz", nil); code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never healthy\n%s", logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	// an operation's end, awaited: its state and its error
	end := func(what string, code int, acc map[string]any) map[string]any {
		t.Helper()
		if code != 202 {
			t.Fatalf("%s: %d %v", what, code, acc)
		}
		op := acc["operation"].(map[string]any)["id"].(string)
		for {
			_, o := call("GET", "/v1/operations/"+op+"?wait=60", nil)
			if o["state"] != "running" {
				return o
			}
		}
	}
	ok := func(what string, code int, acc map[string]any) map[string]any {
		t.Helper()
		if o := end(what, code, acc); o["state"] != "succeeded" {
			t.Fatalf("%s failed: %v\n%s", what, o, logs.String())
		}
		return acc
	}
	get := func(id string) map[string]any { _, r := call("GET", "/v1/resources/"+id, nil); return r }
	obs := func(id, k string) any { return get(id)["observed"].(map[string]any)[k] }
	act := func(id, action string, params map[string]any) (int, map[string]any) {
		return call("POST", "/v1/resources/"+id+"/actions/"+action, map[string]any{"params": params})
	}

	okAct := func(what, id, action string, params map[string]any) {
		t.Helper()
		code, acc := act(id, action, params)
		ok(what, code, acc)
	}
	machine := func(name string) (string, string) {
		code, acc := call("POST", "/v1/resources", map[string]any{"type": "machine", "zone": "bench", "spec": map[string]any{
			"name": name, "kind": "container", "image": "debian-13", "type": "t3.micro", "disk_gb": 2}})
		ok(name, code, acc)
		id := acc["resource"].(map[string]any)["id"].(string)
		ref := obs(id, "engine_ref").(string)
		return id, ref[strings.LastIndex(ref, "/")+1:]
	}
	a, va := machine("vol-a")
	b, vb := machine("vol-b")
	var vol string
	t.Cleanup(func() {
		for _, m := range []string{a, b} {
			code, acc := act(m, "stop", nil)
			if code == 202 {
				end("stop", code, acc)
			}
		}
		if vol != "" {
			if code, acc := act(vol, "detach", nil); code == 202 {
				end("detach", code, acc)
			}
			if code, acc := call("DELETE", "/v1/resources/"+vol, nil); code == 202 {
				end("delete the volume", code, acc)
			}
		}
		for _, m := range []string{a, b} {
			if code, acc := call("DELETE", "/v1/resources/"+m, nil); code == 202 {
				end("delete "+m, code, acc)
			}
		}
		sum := sha256.Sum256([]byte(subject))                                                                                                  // the shelf's name: its owner's, hashed
		_, _ = benchSSH("for id in $(pct list | awk 'NR>1 {print $1}'); do pct config $id | grep -q '^description: made by hangar%3A shelf " + // the CLI prints it url-encoded

			hex.EncodeToString(sum[:6]) + "' && pct destroy $id --purge >/dev/null; done; true")
	})

	// born on A, running: hot-mounted, a file written in it
	code, acc := call("POST", "/v1/resources", map[string]any{"type": "volume", "zone": "bench",
		"spec": map[string]any{"size_gb": 1, "machine": a, "mount": "/data"}})
	ok("the volume", code, acc)
	vol = acc["resource"].(map[string]any)["id"].(string)
	t.Logf("%s on %s (%s): %v", vol, a, va, get(vol)["observed"])
	must("pct exec " + va + " -- sh -c 'echo le-hangar > /data/proof'")

	// neither goes while one is in the other; a running container keeps it
	if code, r := call("DELETE", "/v1/resources/"+a, nil); code != 409 || !strings.Contains(fmt.Sprint(r["detail"]), vol) {
		t.Fatalf("the machine's delete: %d %v", code, r)
	} else {
		t.Logf("refused: %v", r["detail"])
	}
	code, acc = act(vol, "move", map[string]any{"machine": b, "mount": "/srv"})
	if o := end("the move from a running container", code, acc); o["state"] != "failed" || !strings.Contains(fmt.Sprint(o["error"]), "running container") {
		t.Fatalf("the move from a running container: %v", o)
	} else {
		t.Logf("refused: %v", o["error"])
	}

	// A stopped: moved to B, running — the file there
	okAct("stop A", a, "stop", nil)
	okAct("move to B", vol, "move", map[string]any{"machine": b, "mount": "/srv"})
	if got := must("pct exec " + vb + " -- cat /srv/proof"); got != "le-hangar" {
		t.Fatalf("in B: %q", got)
	}
	okAct("stop B", b, "stop", nil)
	okAct("detach", vol, "detach", nil)
	if m := obs(vol, "machine"); m != nil {
		t.Fatalf("parked, it reads on %v", m)
	}
	ref := obs(vol, "engine_ref").(string)
	if got := must("cat $(pvesm path " + ref + ")/proof"); got != "le-hangar" {
		t.Fatalf("parked at %s: %q", ref, got)
	}
	t.Logf("parked as %s", ref)

	// back into A, started: the file where it was
	okAct("attach to A", vol, "attach", map[string]any{"machine": a, "mount": "/data"})
	okAct("start A", a, "start", nil)
	if got := must("pct exec " + va + " -- cat /data/proof"); got != "le-hangar" {
		t.Fatalf("back in A: %q", got)
	}
	if got := obs(vol, "in_guest"); got != "/data" {
		t.Fatalf("back in A at %v", got)
	}
	// the backup budget, a size
	if code, r := act(vol, "resize", map[string]any{"size_gb": 2}); code != 202 {
		t.Fatalf("resize: %d %v", code, r)
	} else {
		ok("resize", code, r)
	}
	if code, r := act(vol, "set_backup", map[string]any{"backup": true}); code != 403 || !strings.Contains(fmt.Sprint(r["detail"]), "volumes.backup_gb") {
		t.Fatalf("a 2 GB backup on a 1 GB budget: %d %v", code, r)
	} else {
		t.Logf("refused: %v", r["detail"])
	}

	_ = srv.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatal("still running 20 s after SIGTERM")
	}
	for _, f := range []string{os.Getenv("HANGAR_BENCH_TOKEN_FILE"), volTok} {
		if strings.Contains(logs.String(), strings.TrimSpace(readFile(f))) {
			t.Fatalf("an engine token is in the brain's logs (%s)", f)
		}
	}
	// the brain again, for the clean-up's calls
	srv = exec.Command(bin, "serve", "--config", cfg)
	srv.Env = append(os.Environ(), "HANGAR_LISTEN="+addr)
	srv.Stdout, srv.Stderr = &logs, &logs
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Wait() }()
	for deadline := time.Now().Add(20 * time.Second); ; {
		if code, _ := call("GET", "/healthz", nil); code == 200 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
}
