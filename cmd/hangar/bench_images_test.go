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

// An image through the binary's API on a real Proxmox VE, three plugins
// running, each with its own token: an operator bakes Debian with the guest
// agent (a user's tier may not), the brain's reconcile carrying the bake to
// its end; private, then shared with everyone; a user's machine born from it
// is usable at birth — its address read at once, the user's key letting them
// in, the recipe's work there; the user saves it, stopped, as an image of
// their own, shares it with their group and not with one they are not in;
// the operator's delete is refused while the user's machine shares the
// image's disk, and the image retired. Without the bench's variables it
// skips:
//
//	sh tools/bench/bench.sh up && eval "$(sh tools/bench/bench.sh env)" && go test ./cmd/hangar -run BenchAnImage -v -timeout 60m
func TestBenchAnImageThroughTheAPI(t *testing.T) {
	url, imgTok := os.Getenv("HANGAR_BENCH_URL"), os.Getenv("HANGAR_BENCH_IMAGES_TOKEN_FILE")
	if url == "" || imgTok == "" {
		t.Skip("no bench (or one set up before the images token): sh tools/bench/bench.sh up, then eval its env")
	}
	benchSSH := func(cmd string, stdin ...string) (string, error) {
		c := exec.Command("ssh", "-i", os.Getenv("HANGAR_BENCH_SSH_KEY"), "-p", os.Getenv("HANGAR_BENCH_SSH_PORT"),
			"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
			benchRoot(), cmd)
		if len(stdin) > 0 {
			c.Stdin = strings.NewReader(stdin[0])
		}
		out, err := c.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(cmd string, stdin ...string) string {
		t.Helper()
		out, err := benchSSH(cmd, stdin...)
		if err != nil {
			t.Fatalf("on the bench: %s: %v %s", cmd, err, out)
		}
		return out
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
data_dir: %[1]s/data
tiers:
  - name: ops
    groups: [ops]
    operator: true
    zones: [bench]
    limits: {"*": unlimited, images.source: [recipe, machine], images.visibility: [private, shared, public],
             machines.kind: [vm], machines.class: [spot]}
  - name: users
    groups: [users]
    zones: [bench]
    limits:
      machines.count: 1
      machines.vcpu_hours: 1000
      machines.vcpu: 2
      machines.memory_gb: 1
      machines.disk_gb: 8
      machines.key_pairs: 1
      machines.kind: [vm]
      machines.class: [spot]
      images.count: 1
      images.size_gb: 8
      images.source: [machine]
      images.visibility: [private, shared]
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
    room:                     # the bench's priority guest, VM 100: the machines' token reads its power
      memory_gb: 6
      reservations: [{name: priority, memory_gb: 3, while_running: "100"}]
plugins:
  - name: machines
    builtin: machines
    zones: [bench]
    credentials:
      bench: {file: %[4]s}
  - name: images
    builtin: images
    zones: [bench]
    credentials:
      bench: {file: %[5]s}
    settings:
      recipes:
        debian:
          base: {vm: "local:import/debian-13-genericcloud-amd64.qcow2"}
          disk_gb: 4
          memory_mb: 1024
          timeout: 25m
          user_data: |
            #cloud-config
            package_update: true
            packages: [qemu-guest-agent]
            write_files:
              - {path: /etc/hangar-baked, content: "baked through the API\n"}
reconcile: {every: 10s}
`, dir, url, os.Getenv("HANGAR_BENCH_CA_FILE"), os.Getenv("HANGAR_BENCH_TOKEN_FILE"), imgTok), 0o600); err != nil {
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
	if out, err := run("check"); err != nil || !strings.Contains(out, "image") {
		t.Fatalf("check: %v\n%s", err, out)
	} else {
		t.Logf("hangar check:\n%s", out)
	}
	token := func(subject, groups string) string {
		t.Helper()
		s, err := run("token", "create", "--subject", subject, "--groups", groups, "--name", "bench", "--ttl", "2h")
		if err != nil {
			t.Fatalf("token: %v %s", err, s)
		}
		return strings.TrimSpace(s)
	}
	stamp := time.Now().UnixNano()
	ops := token(fmt.Sprintf("bench-ops-%d", stamp), "ops")
	user := token(fmt.Sprintf("bench-user-%d", stamp), "users,family")

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
		if code, _ := call(ops, "GET", "/healthz", nil); code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never healthy\n%s", logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	end := func(who, what string, code int, acc map[string]any) map[string]any {
		t.Helper()
		if code != 202 {
			t.Fatalf("%s: %d %v", what, code, acc)
		}
		op := acc["operation"].(map[string]any)["id"].(string)
		for {
			_, o := call(who, "GET", "/v1/operations/"+op+"?wait=60", nil)
			if o["state"] != "running" {
				return o
			}
		}
	}
	ok := func(who, what string, code int, acc map[string]any) string {
		t.Helper()
		if o := end(who, what, code, acc); o["state"] != "succeeded" {
			t.Fatalf("%s failed: %v\n%s", what, o, logs.String())
		}
		return acc["resource"].(map[string]any)["id"].(string)
	}
	get := func(who, id string) (int, map[string]any) { return call(who, "GET", "/v1/resources/"+id, nil) }
	field := func(who, id string, path ...string) any {
		_, r := get(who, id)
		var v any = r
		for _, p := range path {
			m, _ := v.(map[string]any)
			v = m[p]
		}
		return v
	}
	create := func(who, typ string, spec map[string]any) (int, map[string]any) {
		// what it is called is the request's own, never the spec's
		body := map[string]any{"type": typ, "zone": "bench", "spec": spec}
		if n, named := spec["name"]; named {
			delete(spec, "name")
			body["name"] = n
		}
		return call(who, "POST", "/v1/resources", body)
	}
	act := func(who, id, action string, params map[string]any) (int, map[string]any) {
		return call(who, "POST", "/v1/resources/"+id+"/actions/"+action, map[string]any{"params": params})
	}
	var img, mine, m string
	t.Cleanup(func() {
		if m != "" {
			if code, acc := call(user, "DELETE", "/v1/resources/"+m, nil); code == 202 {
				end(user, "delete the machine", code, acc)
			}
		}
		for who, id := range map[string]string{user: mine, ops: img} {
			if id == "" {
				continue
			}
			if code, acc := call(who, "DELETE", "/v1/resources/"+id, nil); code == 202 {
				if o := end(who, "delete "+id, code, acc); o["state"] != "succeeded" {
					t.Errorf("cleaning up %s: %v", id, o)
				}
			}
		}
	})

	// the bake: a user's tier may not; the operator's does, and the brain's
	// reconcile carries it to its end
	if code, r := create(user, "image", map[string]any{"recipe": "debian"}); code != 403 || !strings.Contains(fmt.Sprint(r["detail"]), "images.source") {
		t.Fatalf("a user bakes: %d %v", code, r)
	}
	t0 := time.Now()
	code, acc := create(ops, "image", map[string]any{"recipe": "debian", "name": "debian"})
	img = ok(ops, "the bake", code, acc)
	if st := field(ops, img, "observed", "state"); st != "pending" {
		t.Fatalf("a bake begun is pending: %v", st)
	}
	if spot := field(ops, img, "room", "spot_mb"); spot != float64(1024) {
		t.Fatalf("a bake borrows its builder's room: %v", spot)
	}
	for deadline := time.Now().Add(25 * time.Minute); ; time.Sleep(15 * time.Second) {
		st := field(ops, img, "observed", "state")
		if st == "available" {
			break
		}
		if st == "failed" || time.Now().After(deadline) {
			t.Fatalf("the bake: %v — %v\n%s", st, field(ops, img, "observed", "detail"), logs.String())
		}
	}
	t.Logf("baked through the API in %s", time.Since(t0).Round(time.Second))
	if spot := field(ops, img, "room", "spot_mb"); spot != float64(0) {
		t.Fatalf("a finished bake gives its room back: %v", spot)
	}

	// private, then everyone's
	if code, _ := get(user, img); code != 404 {
		t.Fatalf("the user sees a private image: %d", code)
	}
	code, acc = act(ops, img, "share", map[string]any{"shared_with": []string{"*"}})
	end(ops, "share", code, acc)
	if code, _ := get(user, img); code != 200 {
		t.Fatalf("the user does not see what is everyone's: %d", code)
	}

	// the user's machine, born from it, usable at birth
	kdir := t.TempDir()
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "bench-api-img", "-f", filepath.Join(kdir, "k")).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	pub, _ := os.ReadFile(filepath.Join(kdir, "k.pub"))
	priv, _ := os.ReadFile(filepath.Join(kdir, "k"))
	must("umask 077 && cat > /root/bench-api-img-key", string(priv))
	code, acc = create(user, "keypair", map[string]any{"public_key": strings.TrimSpace(string(pub))})
	kp := ok(user, "the key pair", code, acc)
	t1 := time.Now()
	code, acc = create(user, "machine", map[string]any{"name": "born", "image_id": img, "type": "t3.micro", "key_pairs": []string{kp}})
	m = ok(user, "the machine", code, acc)
	var ip string
	for deadline := time.Now().Add(5 * time.Minute); ip == ""; time.Sleep(5 * time.Second) {
		if as, _ := field(user, m, "observed", "addresses").([]any); len(as) > 0 {
			ip = fmt.Sprint(as[0])
		}
		if ip == "" && time.Now().After(deadline) {
			t.Fatalf("no address 5 minutes after its birth: %v", field(user, m, "observed"))
		}
	}
	t.Logf("%s born from %s answered at %s, %s after it was asked for", m, img, ip, time.Since(t1).Round(time.Second))
	in := must("ssh -i /root/bench-api-img-key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=10 debian@" + ip +
		" 'hostname; cat /etc/hangar-baked; systemctl is-active qemu-guest-agent'")
	if in != "born\nbaked through the API\nactive" {
		t.Fatalf("inside the machine: %q", in)
	}
	if field(user, m, "spec", "disk_gb") != float64(8) {
		t.Fatalf("its disk: %v", field(user, m, "spec"))
	}

	// the user's own image, saved stopped, shared with their own group only
	code, acc = act(user, m, "stop", nil)
	end(user, "stop", code, acc)
	code, acc = create(user, "image", map[string]any{"machine": m, "name": "mine"})
	mine = ok(user, "the save", code, acc)
	if st := field(user, mine, "observed", "state"); st != "available" {
		t.Fatalf("a save is available at once: %v", st)
	}
	if code, r := act(user, mine, "share", map[string]any{"shared_with": []string{"ops"}}); code != 403 {
		t.Fatalf("shared with a group the user is not in: %d %v", code, r)
	}
	code, acc = act(user, mine, "share", map[string]any{"shared_with": []string{"family"}})
	end(user, "share with family", code, acc)

	// the operator's image shares its disk with the user's machine: no delete
	code, acc = call(ops, "DELETE", "/v1/resources/"+img, nil)
	if o := end(ops, "the image's delete", code, acc); o["state"] != "failed" || !strings.Contains(fmt.Sprint(o["error"]), m) {
		t.Fatalf("the delete under a linked clone: %v", o)
	} else {
		t.Logf("refused: %v", o["error"])
	}
	code, acc = act(ops, img, "retire", nil)
	end(ops, "retire", code, acc)
	if code, r := create(user, "machine", map[string]any{"image_id": img}); code != 422 || !strings.Contains(fmt.Sprint(r["detail"]), "retired") {
		t.Fatalf("a machine from a retired image: %d %v", code, r)
	}

	_ = srv.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatal("still running 20 s after SIGTERM")
	}
	// the run read in its own log: one bake, and nothing that must not be
	// there — a restart, a drift, a loss, a repair
	count := func(s string) int { return strings.Count(logs.String(), s) }
	if n := count(`"result":"image.baked"`); n != 1 {
		t.Fatalf("%d image.baked in the brain's log, want 1\n%s", n, logs.String())
	}
	for _, never := range []string{`"result":"image.restarted"`, `"result":"image.bake_failed"`, `"result":"drifted"`,
		`"result":"lost"`, `"result":"repaired"`, `"result":"image.waiting"`} {
		if n := count(never); n > 0 {
			t.Fatalf("%d %s in the brain's log\n%s", n, never, logs.String())
		}
	}
	t.Logf("the brain's log: %d audit lines, 1 image.baked, %d image.shared, %d machine.created, %d reconcile verdicts",
		count(`"kind":"audit"`), count(`"result":"image.shared"`), count(`"result":"machine.created"`), count(`"action":"reconcile"`))
	for _, f := range []string{os.Getenv("HANGAR_BENCH_TOKEN_FILE"), imgTok} {
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
		if code, _ := call(ops, "GET", "/healthz", nil); code == 200 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
}
