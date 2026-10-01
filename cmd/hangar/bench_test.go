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

// The binary against a real Proxmox VE — the throwaway tools/bench/bench.sh
// makes — as an operator runs it: checked, a first token, served, and asked
// through its API for a key pair and a machine that is resized live,
// changed behind its back (reconcile puts it back), stopped and deleted.
// Without the bench's variables it skips:
//
//	sh tools/bench/bench.sh up && eval "$(sh tools/bench/bench.sh env)" && go test ./cmd/hangar -run Bench -v
func TestBenchThroughTheAPI(t *testing.T) {
	url := os.Getenv("HANGAR_BENCH_URL")
	if url == "" {
		t.Skip("no bench: sh tools/bench/bench.sh up, then eval its env")
	}
	benchSSH := func(cmd string) (string, error) {
		out, err := exec.Command("ssh", "-i", os.Getenv("HANGAR_BENCH_SSH_KEY"), "-p", os.Getenv("HANGAR_BENCH_SSH_PORT"),
			"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
			"root@127.0.0.1", cmd).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	archive, err := benchSSH(`ls /var/lib/vz/template/cache/ | grep "^debian-13-standard_.*_$(dpkg --print-architecture)\." | sort -V | tail -1`)
	if err != nil || archive == "" {
		t.Fatalf("no container template on the bench: %v %s", err, archive)
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
tiers:
  - name: users
    groups: [users]
    zones: [bench]
    limits:
      machines.count: 2
      machines.vcpu_hours: 1000
      machines.vcpu: 4
      machines.memory_gb: 4
      machines.disk_gb: 16
      machines.key_pairs: 1
      machines.kind: [vm, container]
      machines.class: [spot, guaranteed]
zones:
  - name: bench
    driver: proxmox
    endpoint: %s
    options:
      node: pve-bench
      pool: hangar
      images_pool: hangar-images
      storage: local-zfs
      seed_storage: hangar-seeds
      bridge: hbnet
      vmids: 11000-11019
      ca_file: %s
    room:                     # the bench's priority guest, VM 100: the token reads its power
      memory_gb: 6
      reservations: [{name: priority, memory_gb: 3, while_running: "100"}]
plugins:
  - name: machines
    builtin: machines
    zones: [bench]
    credentials:
      bench: {file: %s}
    settings:
      images:
        debian-13: {vm: debian-13, container: "local:vztmpl/%s"}
reconcile: {every: 5s}
`, dir, url, os.Getenv("HANGAR_BENCH_CA_FILE"), os.Getenv("HANGAR_BENCH_TOKEN_FILE"), archive), 0o600); err != nil {
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
	if out, err := run("check"); err != nil || !strings.Contains(out, "machine") {
		t.Fatalf("check: %v\n%s", err, out)
	} else {
		t.Logf("hangar check:\n%s", out)
	}
	secret, err := run("token", "create", "--subject", "alice", "--groups", "users", "--name", "bench", "--ttl", "1h")
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
		code, health := call("GET", "/healthz", nil)
		if code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never healthy: %d %v\n%s", code, health, logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	// an operation's end, awaited
	await := func(what string, code int, acc map[string]any) map[string]any {
		t.Helper()
		if code != 202 {
			t.Fatalf("%s: %d %v", what, code, acc)
		}
		op := acc["operation"].(map[string]any)["id"].(string)
		for {
			_, o := call("GET", "/v1/operations/"+op+"?wait=60", nil)
			switch o["state"] {
			case "succeeded":
				return o
			case "failed":
				t.Fatalf("%s failed: %v\n%s", what, o, logs.String())
			}
		}
	}
	get := func(id string) map[string]any { _, r := call("GET", "/v1/resources/"+id, nil); return r }
	obs := func(r map[string]any, k string) any { return r["observed"].(map[string]any)[k] }

	key, _ := os.ReadFile(filepath.Join(os.Getenv("HANGAR_BENCH_SSH_KEY") + ".pub"))
	code, acc := call("POST", "/v1/resources", map[string]any{"type": "keypair", "zone": "bench", "spec": map[string]any{"public_key": string(key)}})
	await("the key pair", code, acc)
	kp := acc["resource"].(map[string]any)["id"].(string)

	code, acc = call("POST", "/v1/resources", map[string]any{"type": "machine", "zone": "bench", "spec": map[string]any{
		"name": "api-ct", "kind": "container", "image": "debian-13", "type": "t3.micro", "disk_gb": 4, "key_pairs": []string{kp}}})
	await("the machine", code, acc)
	id := acc["resource"].(map[string]any)["id"].(string)
	t.Cleanup(func() { call("DELETE", "/v1/resources/"+id, nil) })
	m := get(id)
	ref, _ := obs(m, "engine_ref").(string)
	if m["state"] != "ready" || obs(m, "running") != true || obs(m, "memory_mb") != float64(1024) || !strings.HasPrefix(ref, "pve-bench/lxc/") {
		t.Fatalf("the machine: %v", m)
	}
	vmid := ref[strings.LastIndex(ref, "/")+1:]
	t.Logf("%s is %s", id, ref)
	if keys, _ := benchSSH("pct exec " + vmid + " -- cat /root/.ssh/authorized_keys"); !strings.Contains(keys, strings.Fields(string(key))[1]) {
		t.Fatalf("the key pair did not reach the machine: %q", keys)
	}
	initPID, _ := benchSSH("lxc-info -n " + vmid + " -p -H")

	// resized live: a container's memory both ways, init untouched
	for _, gb := range []int{2, 1} {
		code, acc = call("POST", "/v1/resources/"+id+"/actions/resize", map[string]any{"params": map[string]any{"memory_gb": gb}})
		await(fmt.Sprintf("resize to %d GB", gb), code, acc)
		if got := obs(get(id), "memory_mb"); got != float64(gb*1024) {
			t.Fatalf("after resize to %d GB: memory_mb %v", gb, got)
		}
	}
	if now, _ := benchSSH("lxc-info -n " + vmid + " -p -H"); now != initPID {
		t.Fatalf("init went from %s to %s", initPID, now)
	}
	// beyond the tier: refused with the numbers, before the engine hears of it
	if code, r := call("POST", "/v1/resources/"+id+"/actions/resize", map[string]any{"params": map[string]any{"memory_gb": 5}}); code != 403 {
		t.Fatalf("resize beyond the tier: %d %v", code, r)
	} else {
		t.Logf("refused: %v", r["detail"])
	}

	// changed on the engine behind the brain's back: reconcile puts it back
	if out, err := benchSSH("pct set " + vmid + " --memory 1536"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for deadline := time.Now().Add(30 * time.Second); ; {
		if mem, _ := benchSSH("pct config " + vmid + " | awk '/^memory:/ {print $2}'"); mem == "1024" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reconcile did not put the memory back:\n%s", logs.String())
		}
		time.Sleep(time.Second)
	}
	if !strings.Contains(logs.String(), `"result":"repaired"`) {
		t.Errorf("no repaired line in the audit")
	}

	code, acc = call("POST", "/v1/resources/"+id+"/actions/stop", nil)
	await("stop", code, acc)
	if got := obs(get(id), "running"); got != false {
		t.Fatalf("after stop: running %v", got)
	}
	if st, _ := benchSSH("pct status " + vmid); st != "status: stopped" {
		t.Fatalf("the engine says %q", st)
	}
	code, acc = call("DELETE", "/v1/resources/"+id, nil)
	await("delete", code, acc)
	if out, err := benchSSH("pct status " + vmid); err == nil {
		t.Fatalf("the engine still has %s: %s", vmid, out)
	}

	_ = srv.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatal("still running 20 s after SIGTERM")
	}
	if strings.Contains(logs.String(), strings.TrimSpace(readFile(os.Getenv("HANGAR_BENCH_TOKEN_FILE")))) {
		t.Fatal("the engine's token is in the brain's logs")
	}
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }
