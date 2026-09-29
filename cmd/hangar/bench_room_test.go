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

// The room on a real Proxmox VE — the throwaway tools/bench/bench.sh makes —
// as an operator runs it: the hook (cmd/hangar-hook) put on the VM template
// and on a priority guest (VM 100, outside the pools), the brain served with
// a zone that keeps 3 GB for that guest while it runs. Then, the guest
// started as root on the node (the way a wake daemon's `qm start` does):
//
//  1. the hook phones the brain, which stops the spot machines and shrinks
//     the floor (capped, its init untouched) before the guest starts;
//
//  2. a spot machine started by hand beside it is refused by the hook;
//
//  3. the guest stopped: the floor regrows, the spot VM starts again, the
//     spot container that said resume: false stays stopped;
//
//  4. the brain down: the node holds alone from the tags, an oversized start
//     is refused by the hook; the brain back reads the guest running and
//     keeps the holds; down again for the stop, the node gives back alone
//     and the brain, back, restarts the spot VM;
//
//  5. a claim whose release never came ends at its grace, and the spot VM
//     starts again.
//
//     sh tools/bench/bench.sh up && eval "$(sh tools/bench/bench.sh env)" && go test ./cmd/hangar -run BenchTheRoom -v
func TestBenchTheRoom(t *testing.T) {
	url := os.Getenv("HANGAR_BENCH_URL")
	if url == "" {
		t.Skip("no bench: sh tools/bench/bench.sh up, then eval its env")
	}
	sshArgs := []string{"-i", os.Getenv("HANGAR_BENCH_SSH_KEY"), "-p", os.Getenv("HANGAR_BENCH_SSH_PORT"),
		"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "root@127.0.0.1"}
	benchSSH := func(cmd string, stdin ...[]byte) (string, error) {
		c := exec.Command("ssh", append(sshArgs, cmd)...)
		if len(stdin) > 0 {
			c.Stdin = bytes.NewReader(stdin[0])
		}
		out, err := c.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(cmd string, stdin ...[]byte) string {
		t.Helper()
		out, err := benchSSH(cmd, stdin...)
		if err != nil {
			t.Fatalf("on the bench: %s: %v\n%s", cmd, err, out)
		}
		return out
	}
	archive := must(`ls /var/lib/vz/template/cache/ | grep "^debian-13-standard_.*_$(dpkg --print-architecture)\." | sort -V | tail -1`)
	if must("qm config 100 | grep -c '^name: priority'") != "1" {
		t.Fatal("no priority guest on the bench (setup.sh makes VM 100): sh tools/bench/bench.sh up")
	}

	dir, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	bin, hook := filepath.Join(dir, "hangar"), filepath.Join(dir, "hangar-hook")
	for out, pkg := range map[string]string{bin: ".", hook: "../hangar-hook"} {
		build := exec.Command("go", "build", "-o", out, pkg)
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cfg := filepath.Join(dir, "hangar.yaml")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, `
data_dir: %s/data
tiers:
  - name: hooks
    groups: [hooks]
    room: true
    zones: [bench]
    limits: {}
  - name: users
    groups: [users]
    zones: [bench]
    limits: {"machines.*": unlimited}
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
      vmids: 11020-11039
      ca_file: %s
      shutdown_timeout: "20"
    room:
      memory_gb: 6
      grace: 30s
      reservations:
        - {name: host, memory_gb: 1}
        - {name: priority, memory_gb: 3, while_running: "100"}
plugins:
  - name: machines
    builtin: machines
    zones: [bench]
    credentials:
      bench: {file: %s}
    settings:
      images:
        debian-13: {vm: debian-13, container: "local:vztmpl/%s"}
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
			t.Fatalf("hangar %v: %v: %s", args, err, errb.String())
		}
		return strings.TrimSpace(out.String())
	}
	run("check")
	alice := run("token", "create", "--subject", "alice", "--groups", "users", "--name", "bench", "--ttl", "2h")
	hookToken := run("token", "create", "--subject", "hook-pve-bench", "--groups", "hooks", "--name", "hook", "--ttl", "2h", "--scopes", "room")

	// the operator's preparation on the node, as root: the hook on the
	// template and on the priority guest, its file, its token
	hookBin, err := os.ReadFile(hook)
	if err != nil {
		t.Fatal(err)
	}
	must("mkdir -p /var/lib/vz/snippets /etc/hangar && cat > /var/lib/vz/snippets/hangar-hook.new && chmod 755 /var/lib/vz/snippets/hangar-hook.new && mv /var/lib/vz/snippets/hangar-hook.new /var/lib/vz/snippets/hangar-hook", hookBin)
	port := addr[strings.LastIndex(addr, ":")+1:]
	must(`cat > /etc/hangar/hook.json`, fmt.Appendf(nil,
		`{"brain": "http://10.0.2.2:%s", "token_file": "/etc/hangar/hook.token", "zone": "bench", "timeout": "60s", "shutdown_timeout": "20s", "grace": "30s"}`, port)) // no-environment: ok — QEMU user networking's own address for its host, the same on every machine
	must(`umask 077 && cat > /etc/hangar/hook.token`, []byte(hookToken+"\n"))
	must("qm stop 100 >/dev/null 2>&1; rm -rf /run/hangar-hook; qm set 9000 --hookscript local:snippets/hangar-hook >/dev/null && qm set 100 --hookscript local:snippets/hangar-hook >/dev/null")
	t.Cleanup(func() { _, _ = benchSSH("qm stop 100 >/dev/null 2>&1; true") })
	if out := must("/var/lib/vz/snippets/hangar-hook --help | head -1"); !strings.Contains(out, "hangar-hook") {
		t.Fatalf("the hook on the node: %q", out)
	}

	var logs bytes.Buffer
	var srv *exec.Cmd
	serve := func() {
		t.Helper()
		srv = exec.Command(bin, "serve", "--config", cfg)
		srv.Env = append(os.Environ(), "HANGAR_LISTEN="+addr)
		srv.Stdout, srv.Stderr = &logs, &logs
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(20 * time.Second); ; {
			if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("never healthy\n%s", logs.String())
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	stop := func() {
		_ = srv.Process.Signal(syscall.SIGTERM)
		_, _ = srv.Process.Wait()
	}
	serve()
	t.Cleanup(func() { _ = srv.Process.Kill() })

	call := func(method, path string, body any) (int, map[string]any) {
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		req, _ := http.NewRequest(method, "http://"+addr+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+alice)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, map[string]any{"error": err.Error()}
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	await := func(what string, code int, acc map[string]any) string {
		t.Helper()
		if code != 202 {
			t.Fatalf("%s: %d %v", what, code, acc)
		}
		op := acc["operation"].(map[string]any)["id"].(string)
		for {
			_, o := call("GET", "/v1/operations/"+op+"?wait=60", nil)
			switch o["state"] {
			case "succeeded":
				return acc["resource"].(map[string]any)["id"].(string)
			case "failed":
				t.Fatalf("%s failed: %v\n%s", what, o, logs.String())
			}
		}
	}
	get := func(id string) map[string]any { _, r := call("GET", "/v1/resources/"+id, nil); return r }
	vmidOf := func(id string) string {
		ref, _ := get(id)["observed"].(map[string]any)["engine_ref"].(string)
		return ref[strings.LastIndex(ref, "/")+1:]
	}
	machine := func(spec map[string]any) (int, map[string]any) {
		spec["image"] = "debian-13"
		return call("POST", "/v1/resources", map[string]any{"type": "machine", "zone": "bench", "spec": spec})
	}

	key, _ := os.ReadFile(os.Getenv("HANGAR_BENCH_SSH_KEY") + ".pub")
	code, acc := call("POST", "/v1/resources", map[string]any{"type": "keypair", "zone": "bench", "spec": map[string]any{"public_key": string(key)}})
	kp := await("the key pair", code, acc)
	code, acc = machine(map[string]any{"name": "spot-vm", "type": "t3.micro", "disk_gb": 4, "key_pairs": []string{kp}})
	vm := await("the spot VM", code, acc)
	t.Cleanup(func() { call("DELETE", "/v1/resources/"+vm, nil) })
	code, acc = machine(map[string]any{"name": "spot-once", "kind": "container", "type": "t3.micro", "disk_gb": 2, "resume": false})
	once := await("the spot container", code, acc)
	t.Cleanup(func() { call("DELETE", "/v1/resources/"+once, nil) })
	code, acc = machine(map[string]any{"name": "floor", "kind": "container", "class": "guaranteed+spot", "cores": 2, "memory_gb": 2,
		"floor_gb": 1, "cores_beside": 1, "disk_gb": 2})
	gs := await("the floor", code, acc)
	t.Cleanup(func() { call("DELETE", "/v1/resources/"+gs, nil) })
	vmID, onceID, gsID := vmidOf(vm), vmidOf(once), vmidOf(gs)
	t.Logf("spot VM %s = %s, spot container %s = %s, floor %s = %s", vm, vmID, once, onceID, gs, gsID)
	if out := must("qm config " + vmID); !strings.Contains(out, "hookscript: local:snippets/hangar-hook") || !strings.Contains(out, "admitted.1024") {
		t.Fatalf("the clone did not inherit the hook, or carries no admitted size:\n%s", out)
	}
	if out := must("pct config " + gsID + " | grep ^tags"); !strings.Contains(out, "class.guaranteed+spot") || !strings.Contains(out, "floor.1024") || !strings.Contains(out, "beside.1") {
		t.Fatalf("the floor's tags: %s", out)
	}

	// the arithmetic
	if code, r := machine(map[string]any{"kind": "container", "type": "t3.micro"}); code != 409 || !strings.Contains(fmt.Sprint(r["detail"]), "spot pool holds 3 GB; 3 GB in use") {
		t.Fatalf("a fourth borrower: %d %v", code, r)
	} else {
		t.Logf("refused: %s", r["detail"])
	}
	if code, r := machine(map[string]any{"kind": "container", "class": "guaranteed", "cores": 1, "memory_gb": 2}); code != 409 || !strings.Contains(fmt.Sprint(r["detail"]), "guaranteed pool holds 2 GB; 1 GB booked") {
		t.Fatalf("a promise beyond the pool: %d %v", code, r)
	} else {
		t.Logf("refused: %s", r["detail"])
	}

	initPID := must("lxc-info -n " + gsID + " -p -H")
	status := func(kind, id string) string { out, _ := benchSSH(kind + " status " + id); return out }
	cfgOf := func(kind, id, key string) string {
		out, _ := benchSSH(kind + " config " + id + " | awk -F': ' '$1 == \"" + key + "\" {print $2}'")
		return out
	}

	// 1. the priority guest starts, the way a wake daemon starts it
	t0 := time.Now()
	must("qm start 100")
	took := time.Since(t0)
	if status("qm", "100") != "status: running" {
		t.Fatal("the priority guest did not start")
	}
	t.Logf("qm start 100 took %s", took.Round(100*time.Millisecond))
	if s := status("qm", vmID); s != "status: stopped" {
		t.Fatalf("the spot VM beside it: %s", s)
	}
	if s := status("pct", onceID); s != "status: stopped" {
		t.Fatalf("the spot container beside it: %s", s)
	}
	if m, c := cfgOf("pct", gsID, "memory"), cfgOf("pct", gsID, "cpulimit"); m != "1024" || c != "1" {
		t.Fatalf("the floor beside it: memory %q, cpulimit %q", m, c)
	}
	if pid := must("lxc-info -n " + gsID + " -p -H"); pid != initPID {
		t.Fatalf("the floor's init went from %s to %s", initPID, pid)
	}
	if !strings.Contains(cfgOf("pct", gsID, "tags"), "held.100") {
		t.Fatal("the floor carries no hold")
	}
	hookLog := must("journalctl -t hangar-hook --no-pager -o cat | tail -5")
	if !strings.Contains(hookLog, "claim for guest 100: the brain answered (reservation priority)") {
		t.Fatalf("the hook's journal:\n%s", hookLog)
	}
	t.Logf("the hook said:\n%s", hookLog)
	if r := get(once); r["spec"].(map[string]any)["running"] != false {
		t.Fatalf("resume: false, and still meant to run: %v", r["spec"])
	}

	// 2. a spot machine started by hand beside it: refused by the hook
	if out, err := benchSSH("qm start " + vmID); err == nil || !strings.Contains(out, "is spot, and guest 100 of this node has its room") {
		t.Fatalf("a spot start by hand beside the priority guest: %v %s", err, out)
	} else {
		t.Logf("qm start %s: %s", vmID, out)
	}

	// 3. the priority guest stops: the room comes back
	must("qm stop 100")
	for deadline := time.Now().Add(90 * time.Second); status("qm", vmID) != "status: running" || cfgOf("pct", gsID, "memory") != "2048"; {
		if time.Now().After(deadline) {
			t.Fatalf("the room did not come back: VM %s, floor memory %s\n%s", status("qm", vmID), cfgOf("pct", gsID, "memory"), logs.String())
		}
		time.Sleep(time.Second)
	}
	if c := cfgOf("pct", gsID, "cpulimit"); c != "" {
		t.Fatalf("the cap outlived the hold: %q", c)
	}
	if s := status("pct", onceID); s != "status: stopped" {
		t.Fatalf("resume: false came back: %s", s)
	}
	if strings.Contains(cfgOf("qm", vmID, "tags")+cfgOf("pct", gsID, "tags"), "held.") {
		t.Fatal("a hold tag outlived the hold")
	}

	// 4. the brain down: the node acts alone
	stop()
	t0 = time.Now()
	must("qm start 100")
	t.Logf("qm start 100 with the brain down took %s", time.Since(t0).Round(100*time.Millisecond))
	if s := status("qm", vmID); s != "status: stopped" {
		t.Fatalf("the node alone left the spot VM %s", s)
	}
	if m, c := cfgOf("pct", gsID, "memory"), cfgOf("pct", gsID, "cpulimit"); m != "1024" || c != "1" {
		t.Fatalf("the node alone: floor memory %q, cpulimit %q", m, c)
	}
	if out := must("journalctl -t hangar-hook --no-pager -o cat | tail -8"); !strings.Contains(out, "the brain could not be reached") || !strings.Contains(out, "the node acts alone") {
		t.Fatalf("the hook's journal:\n%s", out)
	} else {
		t.Logf("the hook said:\n%s", out)
	}
	// an oversized start, by hand: refused by the hook for its size — the
	// first thing it reads, before whose room it is
	must("qm set " + vmID + " --memory 2048")
	if out, err := benchSSH("qm start " + vmID); err == nil || !strings.Contains(out, "is set to 2048 MB, and was admitted at 1024 MB") {
		t.Fatalf("an oversized start: %v %s", err, out)
	} else {
		t.Logf("qm start %s: %s", vmID, out)
	}
	must("qm set " + vmID + " --memory 1024")
	// the brain back reads the guest running (its token may read VM 100's
	// power), and keeps what the node held
	serve()
	time.Sleep(15 * time.Second) // a reconcile pass
	if r := get(vm); r["hold"] != "100" {
		t.Fatalf("the brain back does not hold the spot VM: %v", r["hold"])
	}
	if s := status("qm", vmID); s != "status: stopped" {
		t.Fatalf("the brain back started the spot VM beside the priority guest: %s", s)
	}
	if _, z := call("GET", "/v1/zones", nil); true {
		b, _ := json.Marshal(z["zones"].([]any)[0].(map[string]any)["room"])
		t.Logf("the zone, the brain back: %s", b)
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "survey") || strings.Contains(line, `"action":"room"`) {
			t.Logf("brain: %s", line)
		}
	}
	// down again for the stop: the node gives back alone; the brain back
	// reads the guest stopped and restarts the spot VM
	stop()
	must("qm stop 100")
	if m, c := cfgOf("pct", gsID, "memory"), cfgOf("pct", gsID, "cpulimit"); m != "2048" || c != "" {
		t.Fatalf("the node gave back: floor memory %q, cpulimit %q", m, c)
	}
	serve()
	waitRunning := func(why string) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Minute); status("qm", vmID) != "status: running"; {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the spot VM did not start again\n%s", why, logs.String())
			}
			time.Sleep(time.Second)
		}
	}
	waitRunning("the brain back after the node gave back")

	// 5. a release that never comes: claimed with the brain up, stopped with
	// it down — the claim outlives its guest, and ends at its grace
	must("qm start 100")
	if s := status("qm", vmID); s != "status: stopped" {
		t.Fatalf("claimed, and the spot VM runs: %s", s)
	}
	stop()
	must("qm stop 100")
	serve()
	waitRunning("a claim with no release")
	if !strings.Contains(logs.String(), `"result":"claim-expired"`) {
		t.Errorf("no claim-expired line in the audit")
	}
	for _, want := range []string{`"result":"claimed"`, `"result":"released"`, `"result":"held"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no %s in the audit", want)
		}
	}
	stop()
	if strings.Contains(logs.String(), strings.TrimSpace(readFile(os.Getenv("HANGAR_BENCH_TOKEN_FILE")))) {
		t.Fatal("the engine's token is in the brain's logs")
	}
	serve() // for the cleanups' deletes
}
