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
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The hours on a real Proxmox VE — the throwaway tools/bench/bench.sh makes —
// through the binary, as an operator runs it. Two people, two tiers:
//
// bob's three containers all say idle_after: 5m. One is left alone: the
// brain stops it once the node's own history has seen it quiet five minutes,
// and it stays stopped. One sends a packet a second: it runs on, and is
// stopped in its turn five minutes after the packets end. One is kept awake:
// it runs on, as quiet as the first, until let_sleep.
//
// alice's tier allows one vCPU-hour a month. Her two containers of four
// cores each burn it in seven and a half minutes: both are stopped, a start
// and a new machine are refused with the numbers.
//
//	sh tools/bench/bench.sh up && eval "$(sh tools/bench/bench.sh env)" && go test ./cmd/hangar -run BenchTheHours -v -timeout 40m
func TestBenchTheHours(t *testing.T) {
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
	must := func(cmd string) string {
		t.Helper()
		out, err := benchSSH(cmd)
		if err != nil {
			t.Fatalf("on the bench: %s: %v\n%s", cmd, err, out)
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
	cfg := filepath.Join(dir, "hangar.yaml")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, `
data_dir: %s/data
tiers:
  - name: metered
    groups: [metered]
    zones: [bench]
    limits:
      machines.count: 3
      machines.vcpu_hours: 1     # a month: two machines of 4 cores burn it in 7.5 minutes
      machines.vcpu: 9
      machines.memory_gb: 3
      machines.disk_gb: 8
      machines.kind: [container]
      machines.class: [guaranteed]
  - name: idlers
    groups: [idlers]
    zones: [bench]
    limits:
      machines.count: 3
      machines.vcpu_hours: 1000
      machines.vcpu: 3
      machines.memory_gb: 3
      machines.disk_gb: 8
      machines.kind: [container]
      machines.class: [guaranteed]
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
      memory_gb: 9
      reservations: [{name: priority, memory_gb: 3, while_running: "100"}]
plugins:
  - name: machines
    builtin: machines
    zones: [bench]
    credentials:
      bench: {file: %s}
    settings:
      images:
        debian-13: {container: "local:vztmpl/%s"}
reconcile: {every: 10s}
`, dir, url, os.Getenv("HANGAR_BENCH_CA_FILE"), os.Getenv("HANGAR_BENCH_TOKEN_FILE"), archive), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		cmd := exec.Command(bin, append(args, "--config", cfg)...)
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("hangar %s: %v: %s", strings.Join(args, " "), err, errb.String())
		}
		return strings.TrimSpace(out.String())
	}
	if out := run("check"); !regexp.MustCompile(`machines\s+bench\s+ok`).MatchString(out) {
		t.Fatalf("check:\n%s", out)
	}
	alice := run("token", "create", "--subject", "alice", "--groups", "metered", "--name", "bench", "--ttl", "1h")
	bob := run("token", "create", "--subject", "bob", "--groups", "idlers", "--name", "bench", "--ttl", "1h")

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	var logs syncLog
	srv := exec.Command(bin, "serve", "--config", cfg)
	srv.Env = append(os.Environ(), "HANGAR_LISTEN="+addr)
	srv.Stdout, srv.Stderr = &logs, &logs
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	t.Cleanup(func() { _ = srv.Process.Kill() })

	call := func(who, method, path string, body any) (int, map[string]any) {
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		req, _ := http.NewRequest(method, "http://"+addr+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+who)
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
		if code, _ := call(alice, "GET", "/healthz", nil); code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never healthy:\n%s", logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	await := func(who, what string, code int, acc map[string]any) {
		t.Helper()
		if code != 202 {
			t.Fatalf("%s: %d %v", what, code, acc)
		}
		op := acc["operation"].(map[string]any)["id"].(string)
		for {
			_, o := call(who, "GET", "/v1/operations/"+op+"?wait=60", nil)
			switch o["state"] {
			case "succeeded":
				return
			case "failed":
				t.Fatalf("%s failed: %v\n%s", what, o, logs.String())
			}
		}
	}
	var made []string
	t.Cleanup(func() {
		for _, m := range made {
			who, id, _ := strings.Cut(m, " ")
			if code, acc := call(who, "DELETE", "/v1/resources/"+id, nil); code == 202 {
				op := acc["operation"].(map[string]any)["id"].(string)
				call(who, "GET", "/v1/operations/"+op+"?wait=60", nil)
			}
		}
	})
	create := func(who, name string, spec map[string]any) (id, vmid string) {
		t.Helper()
		full := map[string]any{"kind": "container", "class": "guaranteed", "image": "debian-13", "cores": 1, "memory_gb": 1, "disk_gb": 2}
		for k, v := range spec {
			full[k] = v
		}
		code, acc := call(who, "POST", "/v1/resources", map[string]any{"type": "machine", "zone": "bench", "name": name, "spec": full})
		await(who, name, code, acc)
		id = acc["resource"].(map[string]any)["id"].(string)
		made = append(made, who+" "+id)
		_, r := call(who, "GET", "/v1/resources/"+id, nil)
		ref, _ := r["observed"].(map[string]any)["engine_ref"].(string)
		return id, ref[strings.LastIndex(ref, "/")+1:]
	}
	act := func(who, id, action string, params map[string]any) (int, map[string]any) {
		var body any
		if params != nil {
			body = map[string]any{"params": params}
		}
		return call(who, "POST", "/v1/resources/"+id+"/actions/"+action, body)
	}
	field := func(who, id, part, k string) any {
		_, r := call(who, "GET", "/v1/resources/"+id, nil)
		m, _ := r[part].(map[string]any)
		return m[k]
	}
	runs := func(vmid string) bool { st, _ := benchSSH("pct status " + vmid); return st == "status: running" }
	start := time.Now()
	since := func() string { return time.Since(start).Round(time.Second).String() }
	until := func(what string, within time.Duration, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(within); !ok(); {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not within %s (at %s)\n%s", what, within, since(), logs.Tail(40))
			}
			time.Sleep(5 * time.Second)
		}
	}
	used := func(who string) (float64, map[string]any) {
		_, r := call(who, "GET", "/v1/limits", nil)
		for _, l := range r["limits"].([]any) {
			if m := l.(map[string]any); m["name"] == "machines.vcpu_hours" {
				return m["used"].(float64), m
			}
		}
		return -1, nil
	}

	// alice's two machines burn her month; bob's three idle, or not
	big1, bv1 := create(alice, "burn-1", map[string]any{"cores": 4})
	big2, bv2 := create(alice, "burn-2", map[string]any{"cores": 4})
	burning := time.Now()
	idle, iv := create(bob, "left-alone", map[string]any{"idle_after": "5m"})
	idleBorn := time.Now()
	talks, tv := create(bob, "talks", map[string]any{"idle_after": "5m"})
	kept, kv := create(bob, "kept-awake", map[string]any{"idle_after": "5m"})
	gw := must("pct exec " + tv + ` -- sh -c "ip route | awk '/default/ {print \$3}'"`)
	must("pct exec " + tv + " -- sh -c 'nohup ping -q -i 1 " + gw + " >/dev/null 2>&1 & echo $! >/tmp/ping.pid'")
	code, acc := act(bob, kept, "keep_awake", map[string]any{"for": "1h"})
	await(bob, "keep_awake", code, acc)
	t.Logf("at %s: alice's two 4-core machines run; bob's three say idle_after 5m (one left alone, one sending a packet a second, one kept awake for 1h)", since())

	// the one left alone is stopped at its idle_after — not before
	until("the machine left alone is stopped", 9*time.Minute, func() bool { return !runs(iv) })
	stoppedAfter := time.Since(idleBorn)
	if stoppedAfter < 5*time.Minute {
		t.Fatalf("stopped %s after it was made, before its idle_after", stoppedAfter.Round(time.Second))
	}
	until("the registry knows", 30*time.Second, func() bool { return field(bob, idle, "spec", "running") == false })
	t.Logf("at %s: the machine left alone was stopped %s after it was made (idle_after 5m)", since(), stoppedAfter.Round(time.Second))
	if !strings.Contains(logs.String(), `"result":"machine.idle"`) {
		t.Fatalf("no machine.idle in the audit:\n%s", logs.Tail(40))
	}
	// the controls beside it, as old and as idle of CPU: sending, kept awake
	if !runs(tv) || !runs(kv) {
		t.Fatalf("the machine that talks runs: %v; the one kept awake runs: %v", runs(tv), runs(kv))
	}
	t.Logf("the machine sending a packet a second reads quiet for %v; the one kept awake, %v — both run", field(bob, talks, "observed", "quiet_for"), field(bob, kept, "observed", "quiet_for"))
	if q := field(bob, talks, "observed", "quiet_for"); q != "0m" && q != "1m" {
		t.Fatalf("sending a packet a second, it reads quiet for %v", q)
	}

	// alice's month runs out: both stopped, with the numbers
	until("alice's machines are stopped", 8*time.Minute, func() bool { return !runs(bv1) && !runs(bv2) })
	n, line := used(alice)
	t.Logf("at %s: alice's machines were stopped %s after they began to run; her month reads %.3f of %v %v (back on %v)",
		since(), time.Since(burning).Round(time.Second), n, line["limit"], line["unit"], line["resets"])
	if burned := time.Since(burning); burned < 7*time.Minute || n < 1 || n > 1.2 {
		t.Fatalf("stopped after %s at %.3f vCPU-hours: 8 cores burn 1 vCPU-hour in 7m30s", burned.Round(time.Second), n)
	}
	if !strings.Contains(logs.String(), `"result":"machine.spent"`) {
		t.Fatalf("no machine.spent in the audit:\n%s", logs.Tail(40))
	}
	until("the registry knows", 30*time.Second, func() bool {
		return field(alice, big1, "spec", "running") == false && field(alice, big2, "spec", "running") == false
	})
	for what, try := range map[string]func() (int, map[string]any){
		"a start": func() (int, map[string]any) { return act(alice, big1, "start", nil) },
		"a new machine": func() (int, map[string]any) {
			return call(alice, "POST", "/v1/resources", map[string]any{"type": "machine", "zone": "bench", "spec": map[string]any{"kind": "container", "class": "guaranteed", "image": "debian-13", "cores": 1, "memory_gb": 1, "disk_gb": 2}})
		},
	} {
		code, r := try()
		detail, _ := r["detail"].(string)
		if code != 403 || !strings.Contains(detail, "of 1 vCPU-hours (machines.vcpu_hours) used in") || !strings.Contains(detail, "it is back on 1 ") {
			t.Fatalf("%s on a spent month: %d %v", what, code, r)
		}
		t.Logf("%s refused: %s", what, detail)
	}
	// bob's month is his own: his machines run on, and count
	if n, _ := used(bob); n <= 0 || n > 1 {
		t.Fatalf("bob's month reads %v", n)
	}

	// the packets end: five quiet minutes later it is stopped in its turn
	must("pct exec " + tv + " -- sh -c 'kill $(cat /tmp/ping.pid)'")
	silent := time.Now()
	// let sleep: as quiet as the first all along, it is stopped at the next pass
	code, acc = act(bob, kept, "let_sleep", nil)
	await(bob, "let_sleep", code, acc)
	until("the machine let sleep is stopped", 90*time.Second, func() bool { return !runs(kv) })
	t.Logf("at %s: let sleep, the machine kept awake was stopped %s later", since(), time.Since(silent).Round(time.Second))
	until("the machine that talked is stopped", 9*time.Minute, func() bool { return !runs(tv) })
	if quiet := time.Since(silent); quiet < 5*time.Minute {
		t.Fatalf("stopped %s after its packets ended, before its idle_after", quiet.Round(time.Second))
	}
	t.Logf("at %s: the machine that talked was stopped %s after its last packet", since(), time.Since(silent).Round(time.Second))
	// stopped for idleness, it starts again when its owner says
	code, acc = act(bob, idle, "start", nil)
	await(bob, "start", code, acc)
	if !runs(iv) {
		t.Fatal("its owner's start did not start it")
	}

	// let go of everything while the brain still answers: nothing is left on
	// the bench (the cleanup above is for a run that failed before here)
	for _, m := range made {
		who, id, _ := strings.Cut(m, " ")
		code, acc := call(who, "DELETE", "/v1/resources/"+id, nil)
		await(who, "delete "+id, code, acc)
	}
	made = nil
	for _, v := range []string{bv1, bv2, iv, tv, kv} {
		if out, err := benchSSH("pct status " + v); err == nil {
			t.Fatalf("the engine still has %s: %s", v, out)
		}
	}
	if n, _ := used(alice); n < 1 {
		t.Fatalf("her machines deleted, her month reads %v: a delete gives nothing back", n)
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

// syncLog is the brain's output: written by its process, read by the test.
type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *syncLog) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// Tail is its last n lines.
func (l *syncLog) Tail(n int) string {
	lines := strings.Split(strings.TrimSpace(l.String()), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}
