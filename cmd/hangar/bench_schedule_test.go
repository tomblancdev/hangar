package main

import (
	"bytes"
	"context"
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

// A recipe baked again by the brain's own clock, through the binary on a
// real Proxmox VE: a schedule every minute, so its bakes follow one another
// and every run while one is still being made is skipped. The first image is
// asked for in the schedule's name, shared with everyone; a user's machine
// asks for "@debian" and is born from it, its address at birth; once the
// second is available, the first is retired — kept while the machine is born
// from it — and deleted, template and all, once the machine is gone. An
// image baked by hand from the same recipe is never touched. Without the
// bench's variables it skips:
//
//	sh tools/bench/bench.sh up && eval "$(sh tools/bench/bench.sh env)" && go test ./cmd/hangar -run BenchARecipe -v -timeout 60m
func TestBenchARecipeBakedAgainByItself(t *testing.T) {
	url, imgTok := os.Getenv("HANGAR_BENCH_URL"), os.Getenv("HANGAR_BENCH_IMAGES_TOKEN_FILE")
	if url == "" || imgTok == "" {
		t.Skip("no bench (or one set up before the images token): sh tools/bench/bench.sh up, then eval its env")
	}
	benchSSH := func(cmd string) (string, error) {
		// a bench out of breath fails the test, never hangs it
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ssh", "-i", os.Getenv("HANGAR_BENCH_SSH_KEY"), "-p", os.Getenv("HANGAR_BENCH_SSH_PORT"),
			"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
			"-o", "ConnectTimeout=30", benchRoot(), cmd).CombinedOutput()
		return strings.TrimSpace(string(out)), err
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
	stamp := time.Now().UnixNano()
	recipes := fmt.Sprintf("bench-recipes-%d", stamp)
	body := fmt.Sprintf(`
data_dir: %[1]s/data
tiers:
  - name: ops
    groups: [ops]
    operator: true
    zones: [bench]
    limits: {"*": unlimited, images.source: [recipe, machine], images.visibility: [private, shared, public],
             machines.kind: [vm], machines.class: [spot]}
  - name: recipes
    groups: [recipes]
    zones: [bench]
    limits: {images.count: 4, images.size_gb: 16, images.source: [recipe], images.visibility: [public]}
  - name: users
    groups: [users]
    zones: [bench]
    limits:
      machines.count: 1
      machines.vcpu_hours: 1000
      machines.vcpu: 2
      machines.memory_gb: 1
      machines.disk_gb: 8
      machines.kind: [vm]
      machines.class: [spot]
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
      vmids: 11080-11099
      ca_file: %[3]s
    room:
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
          memory_mb: 768
          timeout: 25m
          user_data: |
            #cloud-config
            package_update: true
            packages: [qemu-guest-agent]
reconcile: {every: 10s}
`, dir, url, os.Getenv("HANGAR_BENCH_CA_FILE"), os.Getenv("HANGAR_BENCH_TOKEN_FILE"), imgTok)
	schedule := fmt.Sprintf(`schedules:
  - name: debian
    cron: "* * * * *"
    as: {subject: %s, groups: [recipes]}
    create: {type: image, zone: bench, spec: {recipe: debian, shared_with: ["*"]}}
    keep: 1
    retire: retire
`, recipes)
	cfg, quiet := filepath.Join(dir, "hangar.yaml"), filepath.Join(dir, "quiet.yaml")
	if err := os.WriteFile(cfg, []byte(body+schedule), 0o600); err != nil {
		t.Fatal(err)
	}
	// the same brain without its schedule, for the clean-up
	if err := os.WriteFile(quiet, []byte(body), 0o600); err != nil {
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
	if out, err := run("check"); err != nil || !strings.Contains(out, "SCHEDULE") {
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
	ops := token(fmt.Sprintf("bench-ops-%d", stamp), "ops")
	user := token(fmt.Sprintf("bench-user-%d", stamp), "users")

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	var logs bytes.Buffer
	serve := func(config string) (*exec.Cmd, chan error) {
		t.Helper()
		srv := exec.Command(bin, "serve", "--config", config)
		srv.Env = append(os.Environ(), "HANGAR_LISTEN="+addr)
		srv.Stdout, srv.Stderr = &logs, &logs
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- srv.Wait() }()
		return srv, exited
	}
	srv, exited := serve(cfg)
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
	healthy := func() {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); ; {
			if code, _ := call(ops, "GET", "/healthz", nil); code == 200 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("never healthy\n%s", logs.String())
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	healthy()
	t0 := time.Now()
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
	field := func(id string, path ...string) any {
		_, r := call(ops, "GET", "/v1/resources/"+id, nil)
		var v any = r
		for _, p := range path {
			m, _ := v.(map[string]any)
			v = m[p]
		}
		return v
	}
	// what the schedule made and still holds something, oldest first
	made := func() []string {
		_, r := call(ops, "GET", "/v1/resources?type=image&tag=hangar:schedule=debian", nil)
		var ids []string
		rs, _ := r["resources"].([]any)
		for _, x := range rs {
			ids = append(ids, x.(map[string]any)["id"].(string))
		}
		return ids
	}
	until := func(what string, within time.Duration, cond func() bool) time.Duration {
		t.Helper()
		start := time.Now()
		for deadline := start.Add(within); !cond(); time.Sleep(10 * time.Second) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not within %s\n%s", what, within, logs.String())
			}
		}
		return time.Since(start).Round(time.Second)
	}
	available := func(id string) func() bool {
		return func() bool {
			st := field(id, "observed", "state")
			if st == "failed" {
				t.Fatalf("%s failed: %v", id, field(id, "observed", "detail"))
			}
			return st == "available"
		}
	}
	machine := func(image string) (int, map[string]any) {
		// unnamed: it is asked for more than once, and a name is one thing
		return call(user, "POST", "/v1/resources", map[string]any{"type": "machine", "zone": "bench",
			"spec": map[string]any{"image_id": image, "type": "t3.micro"}})
	}
	var m string
	t.Cleanup(func() {
		// the brain again, without its schedule: everything made here goes
		_ = srv.Process.Kill()
		srv, _ = serve(quiet)
		healthy()
		if m != "" {
			if code, acc := call(user, "DELETE", "/v1/resources/"+m, nil); code == 202 {
				end(user, "delete the machine", code, acc)
			}
		}
		_, r := call(ops, "GET", "/v1/resources?type=image", nil)
		rs, _ := r["resources"].([]any)
		for _, x := range rs {
			id := x.(map[string]any)["id"].(string)
			// an operation the first brain was stopped during is resumed by
			// this one: its end comes first
			for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline); time.Sleep(5 * time.Second) {
				if st := field(id, "state"); st != "creating" && st != "updating" && st != "deleting" {
					break
				}
			}
			code, acc := call(ops, "DELETE", "/v1/resources/"+id, nil)
			if code != 202 {
				// said, never skipped: what stays on the bench is named
				t.Errorf("cleaning up %s (%v): %d %v — destroy it on the bench by hand", id, field(id, "state"), code, acc["detail"])
				continue
			}
			if o := end(ops, "delete "+id, code, acc); o["state"] != "succeeded" {
				t.Errorf("cleaning up %s: %v", id, o)
			}
		}
	})

	// an image baked by hand from the same recipe: never the schedule's
	code, acc := call(ops, "POST", "/v1/resources", map[string]any{"type": "image", "zone": "bench", "name": "by-hand",
		"spec": map[string]any{"recipe": "debian", "shared_with": []string{"*"}}})
	hand := ok(ops, "the bake by hand", code, acc)

	// armed at start; asked at the next minute, in the schedule's name
	until("the schedule's first run", 3*time.Minute, func() bool { return len(made()) > 0 })
	img1 := made()[0]
	t.Logf("the first run asked for %s %s after the brain started", img1, time.Since(t0).Round(time.Second))
	// its create ends in a moment (the builder made on the engine): then the
	// plugin's say is written
	until("the first run's create", 3*time.Minute, func() bool { return field(img1, "state") == "ready" })
	if field(img1, "owner") != recipes || field(img1, "tags", "hangar:schedule") != "debian" || fmt.Sprint(field(img1, "shared_with")) != "[*]" {
		t.Fatalf("the schedule's, tagged, everyone's: %v %v %v", field(img1, "owner"), field(img1, "tags"), field(img1, "shared_with"))
	}
	if field(img1, "unusable") != "pending" || field(img1, "pending") != true {
		t.Fatalf("still being made: %v %v", field(img1, "unusable"), field(img1, "pending"))
	}
	if code, r := machine("@debian"); code != 422 || !strings.Contains(fmt.Sprint(r["detail"]), "has made no usable image") {
		t.Fatalf("the latest while the first still bakes: %d %v", code, r)
	}
	t.Logf("%s available %s later", img1, until("the first bake", 25*time.Minute, available(img1)))
	until("the bake by hand", 25*time.Minute, available(hand))

	// the user asks for the latest: born from the first, its address at birth
	t1 := time.Now()
	code, acc = machine("@debian")
	m = ok(user, "the machine born from the latest", code, acc)
	if got := field(m, "spec", "image_id"); got != img1 {
		t.Fatalf("born from %v, not the latest %s", got, img1)
	}
	until("the machine's address", 5*time.Minute, func() bool { as, _ := field(m, "observed", "addresses").([]any); return len(as) > 0 })
	t.Logf("%s born from @debian = %s answered %s after it was asked for", m, img1, time.Since(t1).Round(time.Second))

	// the next one: once it is available, the first is retired — and kept
	// while the machine is born from it
	until("the schedule's second run", 5*time.Minute, func() bool { return len(made()) > 1 })
	img2 := made()[1]
	t.Logf("%s available %s after it was asked for", img2, until("the second bake", 25*time.Minute, available(img2)))
	t.Logf("%s retired %s after", img1, until("the first retired", 2*time.Minute, func() bool { return field(img1, "unusable") == "retired" }))
	if field(img1, "state") != "ready" || field(m, "observed", "running") != true {
		t.Fatalf("the first kept while the machine is born from it: %v, the machine %v", field(img1, "state"), field(m, "observed", "running"))
	}
	// the latest is the second now: the audit line says what @debian stood
	// for, even for a request its tier refuses
	if code, r := machine("@debian"); code != 403 || !strings.Contains(logs.String(), `"resolved":"@debian=`+img2+`"`) {
		t.Fatalf("the latest is the second: %d %v", code, r)
	}

	// the machine gone, nothing is born from the first: deleted, template and all
	code, acc = call(user, "DELETE", "/v1/resources/"+m, nil)
	end(user, "delete the machine", code, acc)
	m = ""
	t.Logf("%s deleted %s after its machine", img1, until("the first deleted", 2*time.Minute, func() bool { return field(img1, "state") == "deleted" }))
	if out, err := benchSSH("qm list"); err != nil || strings.Contains(out, img1) {
		t.Fatalf("the engine still has %s (%v):\n%s", img1, err, out)
	}
	if field(hand, "state") != "ready" || field(hand, "unusable") != nil {
		t.Fatalf("the image baked by hand, touched: %v %v", field(hand, "state"), field(hand, "unusable"))
	}

	_ = srv.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatal("still running 20 s after SIGTERM")
	}
	// the run read in its own log
	count := func(s string) int { return strings.Count(logs.String(), s) }
	armed, asked, skipped := count(`"result":"armed"`), count(`"result":"asked"`), count(`"result":"skipped"`)
	if armed != 1 || asked < 2 || skipped < 1 || count(`"let_go":"retire"`) < 1 || count(`"let_go":"delete"`) < 1 {
		t.Fatalf("armed %d, asked %d, skipped %d, retire %d, delete %d\n%s", armed, asked, skipped,
			count(`"let_go":"retire"`), count(`"let_go":"delete"`), logs.String())
	}
	for _, never := range []string{`"result":"image.restarted"`, `"result":"image.bake_failed"`, `"result":"drifted"`,
		`"result":"lost"`, `"result":"repaired"`} {
		if n := count(never); n > 0 {
			t.Fatalf("%d %s in the brain's log\n%s", n, never, logs.String())
		}
	}
	t.Logf("the brain's log: armed %d, %d runs asked, %d skipped while one baked, %d image.baked, %d retire, %d delete",
		armed, asked, skipped, count(`"result":"image.baked"`), count(`"let_go":"retire"`), count(`"let_go":"delete"`))
	for _, f := range []string{os.Getenv("HANGAR_BENCH_TOKEN_FILE"), imgTok} {
		if strings.Contains(logs.String(), strings.TrimSpace(readFile(f))) {
			t.Fatalf("an engine token is in the brain's logs (%s)", f)
		}
	}
}
