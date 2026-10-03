package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/browsertest"
	"github.com/tomblancdev/hangar/internal/testoidc"
)

// Every action the three plugins declare, asked from a real browser of a
// real Proxmox VE: the binary's brain serving its console, a person signed in
// at an identity provider, the pages drawn from the catalogue and clicked —
// a key pair, a container with all its actions, a volume plugged in, grown,
// moved and parked, a VM saved as an image, shared, a machine born from it,
// the image retired; an operator's bake that fails, asked again from its
// page; and everything let go of. What the engine itself refuses is read on
// the page in the engine's words. Without the bench's variables, or without
// a browser, it skips:
//
//	sh tools/bench/bench.sh up && eval "$(sh tools/bench/bench.sh env)" && HANGAR_BROWSER=… go test ./cmd/hangar -run BenchTheConsole -v -timeout 40m
func TestBenchTheConsole(t *testing.T) {
	url, volTok, imgTok := os.Getenv("HANGAR_BENCH_URL"), os.Getenv("HANGAR_BENCH_VOLUMES_TOKEN_FILE"), os.Getenv("HANGAR_BENCH_IMAGES_TOKEN_FILE")
	if url == "" || volTok == "" || imgTok == "" {
		t.Skip("no bench: sh tools/bench/bench.sh up, then eval its env")
	}
	br := browsertest.Start(t)
	benchSSH := func(cmd string) (string, error) {
		out, err := exec.Command("ssh", "-i", os.Getenv("HANGAR_BENCH_SSH_KEY"), "-p", os.Getenv("HANGAR_BENCH_SSH_PORT"),
			"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
			benchRoot(), cmd).CombinedOutput()
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
	iss := testoidc.New(t)
	run := time.Now().UnixNano()
	alice, olive := fmt.Sprintf("alice-%d", run), fmt.Sprintf("olive-%d", run) // their own shelves, removed at the end
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cfg := filepath.Join(dir, "hangar.yaml")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, `
data_dir: %[1]s/data
console: {url: "http://%[8]s"}
identity:
  oidc: {issuer: %[7]s, audience: hangar}
tiers:
  - name: ops
    groups: [ops]
    operator: true
    zones: [bench]
    limits: {"*": unlimited, images.source: [recipe, machine], images.visibility: [private, shared, public],
             machines.kind: [vm, container], machines.class: [spot]}
  - name: users
    groups: [users]
    zones: [bench]
    limits:
      machines.count: 2
      machines.vcpu_hours: 1000
      machines.vcpu: 3
      machines.memory_gb: 3
      machines.disk_gb: 12
      machines.key_pairs: 1
      machines.kind: [vm, container]
      machines.class: [spot]
      volumes.count: 1
      volumes.size_gb: 3
      volumes.backup_gb: 2
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
      vmids: 11120-11139
      ca_file: %[3]s
      shelf_archive: local:vztmpl/%[9]s
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
        debian-13: {vm: debian-13, container: "local:vztmpl/%[9]s"}
  - name: volumes
    builtin: volumes
    zones: [bench]
    credentials:
      bench: {file: %[5]s}
  - name: images
    builtin: images
    zones: [bench]
    credentials:
      bench: {file: %[6]s}
    settings:
      recipes:
        fails:                # no agent in its builder, so no verdict ever comes: it fails at its timeout, with no network asked of anyone
          base: {vm: "local:import/debian-13-genericcloud-amd64.qcow2"}
          disk_gb: 4
          memory_mb: 1024
          timeout: 1m
          user_data: "#!/bin/sh\nexit 1\n"
reconcile: {every: 10s}
`, dir, url, os.Getenv("HANGAR_BENCH_CA_FILE"), os.Getenv("HANGAR_BENCH_TOKEN_FILE"), volTok, imgTok, iss.URL, addr, archive), 0o600); err != nil {
		t.Fatal(err)
	}
	host := func(args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		cmd := exec.Command(bin, append(args, "--config", cfg)...)
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("hangar %s: %v %s", args[0], err, errb.String())
		}
		return out.String()
	}
	host("check")
	sweeper := strings.TrimSpace(host("token", "create", "--subject", "sweeper", "--groups", "ops", "--name", "bench", "--ttl", "2h"))

	var logs logBuffer
	serve := func() *exec.Cmd {
		srv := exec.Command(bin, "serve", "--config", cfg)
		srv.Env = append(os.Environ(), "HANGAR_LISTEN="+addr)
		srv.Stdout, srv.Stderr = &logs, &logs
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
			if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return srv
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("never healthy\n%s", logs.String())
			}
		}
	}
	srv := serve()
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	t.Cleanup(func() { _ = srv.Process.Kill() })

	// the API, for what the test reads beside the pages and for the clean-up
	api := func(method, path string, body any) (int, map[string]any) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, "http://"+addr+path, rd)
		req.Header.Set("Authorization", "Bearer "+sweeper)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	// ask: a call that changes something, asked again while what it names is
	// busy (an operation in flight, a bake being made), then waited for.
	ask := func(method, path string) {
		for range 40 {
			code, acc := api(method, path, nil)
			if code == 409 && acc["kind"] == "busy" {
				time.Sleep(3 * time.Second)
				continue
			}
			if op, ok := acc["operation"].(map[string]any); ok {
				for range 30 {
					if _, o := api("GET", "/v1/operations/"+fmt.Sprint(op["id"])+"?wait=20", nil); o["state"] != "running" {
						break
					}
				}
			}
			return
		}
	}
	vmidOf := func(id string) string {
		t.Helper()
		_, r := api("GET", "/v1/resources/"+id, nil)
		obs, _ := r["observed"].(map[string]any)
		ref := fmt.Sprint(obs["engine_ref"])
		return ref[strings.LastIndex(ref, "/")+1:]
	}
	// mine lists the engine's guests in this test's range — the owners'
	// shelves aside (a guest of the volumes plugin's own, known by its
	// description, removed with its owner at the end).
	mine := func() []string {
		var out []string
		for _, tool := range []string{"pct", "qm"} {
			list, _ := benchSSH(tool + " list")
			for _, line := range strings.Split(list, "\n") {
				f := strings.Fields(line)
				if len(f) == 0 || len(f[0]) != 5 || f[0] < "11120" || f[0] > "11139" {
					continue
				}
				if desc, _ := benchSSH(tool + " config " + f[0] + " | grep '^description:'"); strings.Contains(desc, "hangar%3A shelf") {
					continue
				}
				out = append(out, tool+" "+strings.Join(f, " "))
			}
		}
		return out
	}
	t.Cleanup(func() {
		// whatever is left, let go of: machines stopped, volumes unplugged, then everything, then the shelves
		for _, who := range []string{alice, olive} {
			_, res := api("GET", "/v1/resources?limit=500&owner="+who, nil)
			list, _ := res["resources"].([]any)
			for _, kind := range []string{"machine", "volume"} {
				for _, x := range list {
					if r := x.(map[string]any); r["type"] == kind {
						ask("POST", "/v1/resources/"+fmt.Sprint(r["id"])+"/actions/"+map[string]string{"machine": "stop", "volume": "detach"}[kind])
					}
				}
			}
			for _, kind := range []string{"volume", "machine", "image", "keypair"} {
				for _, x := range list {
					if r := x.(map[string]any); r["type"] == kind {
						ask("DELETE", "/v1/resources/"+fmt.Sprint(r["id"]))
					}
				}
			}
			sum := sha256.Sum256([]byte(who)) // the shelf's name: its owner's, hashed
			_, _ = benchSSH("for id in $(pct list | awk 'NR>1 {print $1}'); do pct config $id | grep -q '^description: made by hangar%3A shelf " +
				hex.EncodeToString(sum[:6]) + "' && pct destroy $id --purge >/dev/null; done; true")
		}
		// what the brain could not let go of is said — and removed, so the next run starts from nothing
		for _, left := range mine() {
			t.Errorf("left on the engine after the clean-up: %s", left)
			f := strings.Fields(left)
			_, _ = benchSSH(f[0] + " stop " + f[1] + " >/dev/null 2>&1; " + f[0] + " destroy " + f[1] + " --purge >/dev/null 2>&1; true")
		}
	})

	base := "http://" + addr
	const stamp = ".under .stamp" // the one word a resource's own page stamps it with
	made := func(p *browsertest.Page) string {
		t.Helper()
		p.Wait("the new resource's page", "location.hash.startsWith('#/r/')")
		return strings.TrimPrefix(p.Hash(), "#/r/")
	}
	step := func(p *browsertest.Page, what string, f func()) {
		t.Helper()
		start := time.Now()
		f()
		p.Quiet()
		t.Logf("%-46s %s", what, time.Since(start).Round(time.Second))
	}
	del := func(p *browsertest.Page, id string) {
		t.Helper()
		p.Open("#/r/" + id)
		p.Press("DELETE…")
		p.Press("YES, DELETE IT")
		p.Sees("delete: done")
	}
	key, _ := os.ReadFile(os.Getenv("HANGAR_BENCH_SSH_KEY") + ".pub")

	// ---- the operator begins a bake that will fail: it takes its minute beside everything else
	iss.SignedIn(&testoidc.Claims{Subject: olive, Name: "olive", Groups: []string{"ops"}})
	op := br.Page(1280, 900, false)
	op.Patience = 6 * time.Minute
	var failing string
	step(op, "the operator signs in, asks for a bake", func() {
		op.Goto(base + "/console/#/t/image/new")
		op.Press("SIGN IN")
		op.Sees("ASK FOR AN IMAGE")
		op.Fill("name", "never")
		op.Fill("recipe", "fails")
		op.Submit()
		failing = made(op)
	})

	// ---- a person signs in
	iss.SignedIn(&testoidc.Claims{Subject: alice, Name: "alice", Groups: []string{"users"}})
	p := br.Page(1280, 900, false)
	p.Patience = 6 * time.Minute
	var kp, box, vol, other, vm, img, born string
	step(p, "sign in at the provider", func() {
		p.Goto(base + "/")
		p.Press("SIGN IN")
		p.Sees("YOUR LIMITS")
		p.Sees("kept for priority")
	})
	step(p, "a key pair", func() {
		p.Press("+ Key pair")
		p.Fill("public key", strings.TrimSpace(string(key)))
		p.Submit()
		kp = made(p)
		p.Reads(stamp, "READY")
	})

	// ---- a container, and every action a machine has
	step(p, "a container made", func() {
		p.Open("#/t/machine/new")
		p.Fill("name", "box")
		p.Choose("kind", "container", true)
		p.Fill("cores", "1")
		p.Fill("memory gb", "1")
		p.Fill("disk gb", "2")
		p.Fill("image", "debian-13")
		p.Choose("key pairs", kp, true)
		p.Submit()
		box = made(p)
		p.Reads(stamp, "RUNNING")
	})
	boxID := vmidOf(box)
	if got := must("pct exec " + boxID + " -- sh -c 'hostname; grep -c ^ssh- /root/.ssh/authorized_keys'"); got != "box\n1" {
		t.Fatalf("the container on the engine: %q", got)
	}
	act := func(p *browsertest.Page, action string, fill func()) {
		t.Helper()
		step(p, action, func() {
			p.Press(action)
			if fill != nil {
				fill()
				p.Submit()
			}
			p.Sees(action + ": done")
		})
	}
	act(p, "reboot", nil)
	act(p, "set idle after", func() { p.Fill("idle after", "30m") })
	act(p, "keep awake", func() { p.Fill("for", "1h") })
	act(p, "let sleep", nil)

	// ---- a volume: plugged into the running container, grown; its backup flag refused while it runs, in the engine's words
	step(p, "a volume made, plugged in", func() {
		p.Open("#/t/volume/new")
		p.Fill("size gb", "1")
		p.Pick("machine", "box")
		p.Fill("mount", "/home/dev")
		p.Submit()
		vol = made(p)
		p.Reads(stamp, "ATTACHED") // the word a volume wears, plugged in
	})
	must("pct exec " + boxID + " -- sh -c 'mountpoint -q /home/dev && echo le-hangar > /home/dev/proof'")
	act(p, "resize", func() { p.Fill("size gb", "2") })
	step(p, "set backup, refused by the engine while it runs", func() {
		p.Press("set backup")
		p.Choose("backup", "yes", true)
		p.Submit()
		p.Sees("FAILED") // the operation, on a notice — the volume itself stays as it was
		p.Sees("a volume's backup flag changes only while it is stopped")
		p.Reads(stamp, "ATTACHED")
	})
	step(p, "the machine's page names its volume; its delete is refused", func() {
		p.Open("#/r/" + box)
		p.Wait("the volume under what names it", `[...document.querySelectorAll('a.row')].some((a) => a.getAttribute('href') === '#/r/`+vol+`')`)
		p.Press("DELETE…")
		p.Press("YES, DELETE IT")
		p.Sees("REFUSED")
		p.Sees("detach it first")
	})
	act(p, "stop", nil)
	p.Reads(stamp, "STOPPED")
	act(p, "resize", func() { p.Fill("memory gb", "2") })
	if got := must("pct config " + boxID + " | grep '^memory:'"); got != "memory: 2048" {
		t.Fatalf("the container's memory on the engine: %q", got)
	}
	p.Open("#/r/" + vol)
	act(p, "set backup", func() { p.Choose("backup", "yes", true) })
	act(p, "detach", nil)
	act(p, "attach", func() { p.Pick("machine", "box"); p.Fill("mount", "/home/dev") })

	// a second container; the volume moved to it, its data with it
	step(p, "a second container", func() {
		p.Open("#/t/machine/new")
		p.Fill("name", "other")
		p.Choose("kind", "container", true)
		p.Fill("cores", "1")
		p.Fill("memory gb", "1")
		p.Fill("disk gb", "2")
		p.Fill("image", "debian-13")
		p.Submit()
		other = made(p)
		p.Reads(stamp, "RUNNING")
	})
	otherID := vmidOf(other)
	p.Open("#/r/" + other)
	act(p, "stop", nil)
	p.Open("#/r/" + vol)
	act(p, "move", func() { p.Pick("machine", "other"); p.Fill("mount", "/srv/moved") })
	p.Open("#/r/" + other)
	act(p, "start", nil)
	p.Reads(stamp, "RUNNING")
	if got := must("pct exec " + otherID + " -- cat /srv/moved/proof"); got != "le-hangar" {
		t.Fatalf("the volume's data after its move: %q", got)
	}
	act(p, "stop", nil)
	p.Open("#/r/" + vol)
	act(p, "detach", nil)
	step(p, "the volume and the second container deleted", func() {
		del(p, vol)
		del(p, other)
	})
	if out := must("pct list"); strings.Contains(out, otherID+" ") {
		t.Fatalf("the second container is still on the engine:\n%s", out)
	}

	// ---- a VM, saved as an image; shared; a machine born from it; retired
	step(p, "a VM made", func() {
		p.Open("#/t/machine/new")
		p.Fill("name", "vm")
		p.Fill("cores", "1")
		p.Fill("memory gb", "1")
		p.Fill("image", "debian-13")
		p.Submit()
		vm = made(p)
		p.Reads(stamp, "RUNNING")
	})
	act(p, "stop", nil)
	step(p, "an image saved from it", func() {
		p.Open("#/t/image/new")
		p.Fill("name", "saved")
		p.Pick("machine", "vm")
		p.Submit()
		img = made(p)
		p.Reads(stamp, "AVAILABLE")
	})
	act(p, "share", func() { p.Choose("shared with", "users", true) })
	step(p, "the VM deleted, a machine born from the image", func() {
		del(p, vm)
		p.Open("#/t/machine/new")
		p.Fill("name", "born")
		p.Fill("cores", "1")
		p.Fill("memory gb", "1")
		p.Pick("image id", "saved")
		p.Submit()
		born = made(p)
		p.Reads(stamp, "RUNNING")
	})
	p.Open("#/r/" + img)
	act(p, "retire", nil)
	p.Reads(stamp, "RETIRED")

	// ---- the operator: the bake failed on its own, and is asked again from its page; she sees whose is what
	step(op, "the failed bake, asked again", func() {
		op.Open("#/r/" + failing)
		op.Reads(stamp, "FAILED")
		op.Press("rebake")
		op.Sees("rebake: done")
		op.Wait("the image being baked again", `__t.norm(document.querySelector('.under .stamp').textContent) !== 'failed'`)
		op.Reads(stamp, "FAILED") // …and failing again, a minute on: its recipe did not change
	})
	step(op, "the operator sees everyone's", func() {
		op.Open("#/t/machine")
		op.Sees("born")
		op.Sees("owner alice")
		del(op, failing)
	})

	// ---- everything let go of
	step(p, "everything deleted", func() {
		del(p, born)
		del(p, img)
		del(p, box)
		del(p, kp)
		p.Open("#/")
		p.Sees("Nothing yet")
		p.Press("Sign out")
		p.Sees("COME IN")
	})
	for _, left := range mine() {
		t.Errorf("left on the engine: %s", left)
	}

	_ = srv.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatal("still running 20 s after SIGTERM")
	}
	// the run read in its own log: who asked, through which door — and nothing that must not be there
	count := func(s string) int { return strings.Count(logs.String(), s) }
	if count(`"event":"signin","result":"ok","subject":"`+alice+`"`) != 1 || count(`"event":"signout","subject":"`+alice+`"`) != 1 {
		t.Errorf("the console's own lines for alice's sign-in and sign-out:\n%s", logs.String())
	}
	if count(`"actor":"`+alice+`","name":"alice","tier":"users","via":"oidc"`) == 0 {
		t.Error("the audit does not name alice, signed in at the provider, for what the console asked")
	}
	for _, never := range []string{`"result":"lost"`, `"result":"drifted"`, "eyJ"} {
		if n := count(never); n > 0 {
			t.Errorf("%d %s in the brain's log", n, never)
		}
	}
	for _, f := range []string{os.Getenv("HANGAR_BENCH_TOKEN_FILE"), volTok, imgTok} {
		if strings.Contains(logs.String(), strings.TrimSpace(readFile(f))) {
			t.Fatalf("an engine token is in the brain's logs (%s)", f)
		}
	}
	t.Logf("the brain's log: %d audit lines, %d through the console's sign-ins", count(`"kind":"audit"`), count(`"via":"oidc"`))
	// the brain again, for the clean-up's calls
	srv = serve()
	go func() { _ = srv.Wait() }()
}
