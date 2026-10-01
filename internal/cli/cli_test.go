package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/registry"
	"github.com/tomblancdev/hangar/internal/stacktest"
	"github.com/tomblancdev/hangar/internal/testoidc"
)

func testoidcClaims(sub, name string, groups ...string) testoidc.Claims {
	return testoidc.Claims{Subject: sub, Name: name, Groups: groups}
}

func TestMain(m *testing.M) {
	stacktest.ServePlugins()
	os.Exit(m.Run())
}

const labConfig = `
data_dir: %[1]s
identity:
  oidc: {issuer: %[2]s, audience: hangar-cli}
tiers:
  - name: ops
    groups: [ops]
    operator: true
    zones: ["*"]
    limits: {"*": unlimited, "machines.kind": [vm, container], "machines.class": [spot, guaranteed, guaranteed+spot]}
  - name: users
    groups: [users]
    zones: [lab]
    limits:
      machines.count: 4
      machines.vcpu_hours: 1000
      machines.vcpu: 16
      machines.memory_gb: 32
      machines.disk_gb: 64
      machines.key_pairs: 4
      machines.kind: [vm, container]
      machines.class: [spot, guaranteed+spot]
      volumes.count: 4
      volumes.size_gb: 64
      volumes.backup_gb: 16
zones:
  - {name: lab, driver: fake, endpoint: "%[1]s/zone-lab.json"}
plugins:
  - name: machines
    path: %[3]s
    args: [hangar-test-plugin, machines]
    zones: [lab]
    settings:
      images:
        debian-13: {vm: debian-13, container: "store:vztmpl/debian-13.tar.zst"}
  - name: volumes
    path: %[3]s
    args: [hangar-test-plugin, volumes]
    zones: [lab]
reconcile:
  every: 1h
`

// cmd is one run of the command line against a brain.
type cmd struct {
	t     *testing.T
	home  string
	vars  map[string]string
	stdin string
	tty   bool
}

func newCmd(t *testing.T, url, token string) *cmd {
	return &cmd{t: t, home: t.TempDir(), vars: map[string]string{"HANGAR_URL": url, "HANGAR_TOKEN": token}}
}

func (c *cmd) env() (*Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &Env{
		Stdin: strings.NewReader(c.stdin), Stdout: &out, Stderr: &errb, Home: c.home,
		Getenv: func(k string) string { return c.vars[k] }, HTTP: &http.Client{Timeout: time.Minute},
		Terminal: c.tty, Now: time.Now, Sleep: func(time.Duration) { time.Sleep(5 * time.Millisecond) }, Poll: 10 * time.Millisecond,
	}, &out, &errb
}

// run returns stdout, stderr and the exit code.
func (c *cmd) run(args ...string) (string, string, int) {
	c.t.Helper()
	env, out, errb := c.env()
	code := Main(args, env)
	return out.String(), errb.String(), code
}

// ok runs and fails the test on a non-zero exit.
func (c *cmd) ok(args ...string) (string, string) {
	c.t.Helper()
	out, errs, code := c.run(args...)
	if code != 0 {
		c.t.Fatalf("hangar %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errs)
	}
	return out, errs
}

// publicKey is an ed25519 public key in OpenSSH's form.
func publicKey(comment string) string {
	var blob bytes.Buffer
	for _, part := range [][]byte{[]byte("ssh-ed25519"), bytes.Repeat([]byte{7}, 32)} {
		_ = binary.Write(&blob, binary.BigEndian, uint32(len(part)))
		blob.Write(part)
	}
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob.Bytes()) + " " + comment
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// devBox is the file of a dev box: a key pair, a container with a floor that
// stays and a top-up that goes, and two volumes plugged in at their paths.
func devBox(memory, cacheBackup, cache bool, kind string) string {
	mem := 8
	if memory {
		mem = 12
	}
	// written the other way round from the order they are made in: the
	// volumes name the box, the box names the key pair
	s := "set: dev\nzone: lab\nresources:\n"
	if cache {
		s += fmt.Sprintf(`  cache:
    type: volume
    spec: {size_gb: 8, mount: /srv/cache, backup: %v, machine: box}
`, cacheBackup)
	}
	s += fmt.Sprintf(`  home:
    type: volume
    spec: {size_gb: 4, mount: /home, backup: true, machine: box}
  box:
    type: machine
    spec:
      kind: %s
      class: guaranteed+spot
      cores: 4
      memory_gb: %d
      floor_gb: 2
      cores_beside: 1
      image: debian-13
      key_pairs: [me]
  me:
    type: keypair
    spec: {public_key: "%s"}
`, kind, mem, publicKey("alice@laptop"))
	return s
}

type engine struct {
	Guests  map[string]map[string]any `json:"guests"`
	Volumes map[string]map[string]any `json:"volumes"`
}

func readEngine(t *testing.T, path string) engine {
	t.Helper()
	var e engine
	b, err := os.ReadFile(path)
	if err != nil {
		return e
	}
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func live(t *testing.T, s *stacktest.Stack, owner string) map[string]*registry.Resource {
	t.Helper()
	rs, _, err := s.Store.Resources(context.Background(), registry.Filter{Owner: owner, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*registry.Resource{}
	for _, r := range rs {
		if r.State != registry.Deleted && r.State != registry.Failed {
			out[r.Tags[TagName]] = r
		}
	}
	return out
}

func spec(r *registry.Resource) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.Spec, &m)
	return m
}

// The dev box made from its file on the fake engine, through the brain's API:
// planned without a change, created in the order its references say, found
// in sync the second time, changed by the steps its plugins name, a field
// set at birth refused before anything moves, a volume that left the file
// deleted only once asked (and unplugged first) — and only the caller's own
// resources ever counted, an operator's listing of everyone's included.
func TestApplyTheDevBox(t *testing.T) {
	s := stacktest.New(t, labConfig, "machines", "volumes")
	alice := newCmd(t, s.URL, s.Token(t, "alice", "users"))
	dir := t.TempDir()
	file := writeFile(t, dir, "dev.yaml", devBox(false, false, true, "container"))

	// the plan changes nothing
	_, errs := alice.ok("apply", file, "--plan")
	for _, want := range []string{"+ me", "+ box", "create (after me)", "+ home", "create (after box)", "+ cache", "4 to create", "nothing changed (--plan)"} {
		if !strings.Contains(errs, want) {
			t.Fatalf("plan: no %q in\n%s", want, errs)
		}
	}
	if got := live(t, s, "alice"); len(got) != 0 {
		t.Fatalf("a plan made %v", got)
	}

	// made, each after what it names
	_, errs = alice.ok("apply", file)
	got := live(t, s, "alice")
	if len(got) != 4 {
		t.Fatalf("made %d: %v\n%s", len(got), got, errs)
	}
	box, home, cache, me := got["box"], got["home"], got["cache"], got["me"]
	if spec(home)["machine"] != box.ID || spec(cache)["machine"] != box.ID {
		t.Fatalf("volumes on the box: home %v cache %v", spec(home), spec(cache))
	}
	if kp := spec(box)["key_pairs"].([]any); len(kp) != 1 || kp[0] != me.ID {
		t.Fatalf("the box's key pairs: %v", spec(box))
	}
	if box.Tags[TagSet] != "dev" || spec(box)["class"] != "guaranteed+spot" || spec(box)["floor_gb"] != float64(2) {
		t.Fatalf("the box: %v %v", box.Tags, spec(box))
	}
	eng := readEngine(t, s.Dir+"/zone-lab.json")
	if v := eng.Volumes[home.ID]; v == nil || v["guest"] != box.ID || v["mount"] != "/home" {
		t.Fatalf("home on the engine: %v", eng.Volumes)
	}
	if !strings.Contains(errs, "Applied") || !strings.Contains(errs, "4 created") {
		t.Fatal(errs)
	}

	// the second time: nothing to do
	if _, errs = alice.ok("apply", file); !strings.Contains(errs, "Nothing to do") || strings.Count(errs, "in sync") != 4 {
		t.Fatalf("the second apply:\n%s", errs)
	}

	// memory 8 → 12 and the cache backed up: two steps, the plugins' own
	writeFile(t, dir, "dev.yaml", devBox(true, true, true, "container"))
	_, errs = alice.ok("apply", file)
	if !strings.Contains(errs, "~ box") || !strings.Contains(errs, "resize {cores: 4, memory_gb: 12}") || !strings.Contains(errs, "set-backup {backup: true}") {
		t.Fatalf("the change:\n%s", errs)
	}
	got = live(t, s, "alice")
	if spec(got["box"])["memory_gb"] != float64(12) || spec(got["cache"])["backup"] != true || got["box"].ID != box.ID {
		t.Fatalf("after the change: %v %v", spec(got["box"]), spec(got["cache"]))
	}

	// a field set at birth: refused, named, nothing moved
	writeFile(t, dir, "dev.yaml", devBox(true, true, true, "vm"))
	_, errs, code := alice.run("apply", file)
	if code != 1 || !strings.Contains(errs, "! box") || !strings.Contains(errs, "/kind: it is a container, and a machine's kind is set at its birth") || !strings.Contains(errs, "nothing changed") {
		t.Fatalf("a kind changed: %d\n%s", code, errs)
	}
	if now := live(t, s, "alice"); now["box"].ID != box.ID || spec(now["box"])["kind"] != "container" {
		t.Fatal("the box moved")
	}

	// the cache leaves the file: not deleted without a yes
	writeFile(t, dir, "dev.yaml", devBox(true, true, false, "container"))
	if _, errs, code = alice.run("apply", file); code != 1 || !strings.Contains(errs, "- cache") || !strings.Contains(errs, "run again with --yes") {
		t.Fatalf("a delete without --yes: %d\n%s", code, errs)
	}
	alice.tty, alice.stdin = true, "n\n"
	if _, errs, code = alice.run("apply", file); code != 1 || !strings.Contains(errs, "not confirmed") {
		t.Fatalf("a delete refused at the prompt: %d\n%s", code, errs)
	}
	if live(t, s, "alice")["cache"] == nil {
		t.Fatal("deleted unasked")
	}
	// yes: it must be unplugged first — a running container lets go of
	// nothing, and the plugin says so in its words
	alice.stdin = "y\n"
	if _, errs, code = alice.run("apply", file); code != 1 || !strings.Contains(errs, "running container") || !strings.Contains(errs, "stopped:") {
		t.Fatalf("a volume on a running container: %d\n%s", code, errs)
	}
	alice.tty, alice.stdin = false, ""
	alice.ok("machine", "stop", box.ID)
	_, errs = alice.ok("apply", file, "--yes")
	if !strings.Contains(errs, "~ cache: detach done") || !strings.Contains(errs, "- cache: "+cache.ID+" deleted") {
		t.Fatalf("the cache unplugged, then deleted:\n%s", errs)
	}
	if got = live(t, s, "alice"); got["cache"] != nil || len(got) != 3 {
		t.Fatalf("after the delete: %v", got)
	}
	alice.ok("machine", "start", box.ID)

	// someone else's set of the same name is never theirs: bob's resources
	// are not in alice's plan, nor an operator's — whose listing is everyone's
	bob := newCmd(t, s.URL, s.Token(t, "bob", "users"))
	bobFile := writeFile(t, dir, "bob.yaml", "set: dev\nzone: lab\nresources:\n  me:\n    type: keypair\n    spec: {public_key: \""+publicKey("bob")+"\"}\n  spare:\n    type: volume\n    spec: {size_gb: 1}\n")
	bob.ok("apply", bobFile)
	if _, errs = alice.ok("apply", file); !strings.Contains(errs, "Nothing to do") || strings.Contains(errs, "spare") {
		t.Fatalf("bob's set in alice's plan:\n%s", errs)
	}
	root := newCmd(t, s.URL, s.Token(t, "root", "ops"))
	rootFile := writeFile(t, dir, "root.yaml", "set: dev\nzone: lab\nresources:\n  me:\n    type: keypair\n    spec: {public_key: \""+publicKey("root")+"\"}\n")
	_, errs = root.ok("apply", rootFile, "--plan")
	if strings.Contains(errs, "spare") || strings.Contains(errs, "- ") || !strings.Contains(errs, "+ me") {
		t.Fatalf("an operator's plan counts only their own:\n%s", errs)
	}
}

// A file that cannot be applied says why before anything is asked.
func TestAFileRefusedBeforeAnything(t *testing.T) {
	s := stacktest.New(t, labConfig, "machines", "volumes")
	alice := newCmd(t, s.URL, s.Token(t, "alice", "users"))
	dir := t.TempDir()
	for body, words := range map[string]string{
		"set: dev\nresources:\n  a:\n    type: volume\n    spec: {size_gb: 1, machine: box}\n":                        `a: machine names "box", which the file does not declare`,
		"set: dev\nresources:\n  a:\n    type: volume\n    spec: {size_gb: 1, machine: k}\n  k:\n    type: keypair\n": "a: machine names k, a keypair — it takes a machine",
		"set: dev\nresources:\n  a:\n    type: widget\n":                                                              `the brain offers no type "widget"`,
		"set: Dev\nresources: {}\n":                                        "set names what apply keeps",
		"set: dev\nresources:\n  m-0123456789abcdef0:\n    type: volume\n": "not shaped like an id",
		"set: dev\nresources:\n  a:\n    type: volume\n    colour: red\n":  `no key "colour"`,
		"set: dev\nresources:\n  a:\n    type: machine\n    spec: {image: debian-13, key_pairs: [b]}\n  b:\n    type: keypair\n    spec: {public_key: x}\n": "",
	} {
		p := writeFile(t, dir, "f.yaml", body)
		_, errs, code := alice.run("apply", p)
		if words == "" {
			// b comes first, and its plugin refuses its key in its own words
			if code != 1 || !strings.Contains(errs, "stopped: b: one public key, in OpenSSH's form") {
				t.Fatalf("a bad key: %d\n%s", code, errs)
			}
			continue
		}
		if code != 1 || !strings.Contains(errs, words) {
			t.Fatalf("%q: want %q, got %d\n%s", body, words, code, errs)
		}
	}
	if got := live(t, s, "alice"); len(got) != 0 {
		t.Fatalf("made: %v", got)
	}
}

// Signed in by the device flow at the brain's own provider: the code shown,
// approved by the person (here, the test), the token the brain reads kept
// and refreshed when it ends, the refresh token revoked at logout — and a
// brain with no provider takes an API token instead.
func TestSignInByTheDeviceFlow(t *testing.T) {
	s := stacktest.New(t, labConfig, "machines", "volumes")
	// a directory it makes itself is the person's alone (one that exists is
	// left as it is: HANGAR_HOME may be somewhere shared; the file is 0600)
	carol := &cmd{t: t, home: filepath.Join(t.TempDir(), "hangar"), vars: map[string]string{}}
	s.Iss.TokenTTL = 45 * time.Second

	// login waits for the person; the person approves the code it shows
	signin := func(approve bool) (string, int) {
		t.Helper()
		type result struct {
			errs string
			code int
		}
		done := make(chan result, 1)
		go func() {
			env, _, errb := carol.env()
			code := Main([]string{"login", s.URL}, env)
			done <- result{errb.String(), code}
		}()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			for code, scope := range s.Iss.Waiting() {
				if scope != "openid profile offline_access" {
					t.Errorf("the scopes asked: %q", scope)
				}
				if approve {
					s.Iss.Approve(code, testoidcClaims("carol", "Carol", "users"))
				} else {
					s.Iss.Deny(code)
				}
				r := <-done
				if !strings.Contains(r.errs, "and check the code there reads  "+code) || !strings.Contains(r.errs, s.Iss.URL+"/activate?code="+code) {
					t.Errorf("the code and where to approve it are not shown:\n%s", r.errs)
				}
				return r.errs, r.code
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("login asked the provider for no code")
		return "", 0
	}

	if errs, code := signin(false); code != 1 || !strings.Contains(errs, "the sign-in was refused at the identity provider") {
		t.Fatalf("denied: %d\n%s", code, errs)
	}
	errs, code := signin(true)
	if code != 0 || !strings.Contains(errs, "Signed in to "+s.URL+" as carol (Carol) — tier users") {
		t.Fatalf("login: %d\n%s", code, errs)
	}
	// kept for the person alone
	fi, err := os.Stat(filepath.Join(carol.home, "credentials.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the sign-in file: %v %v", fi.Mode(), err)
	}
	if di, _ := os.Stat(carol.home); di.Mode().Perm() != 0o700 {
		t.Fatalf("its directory: %v", di.Mode())
	}
	// no HANGAR_URL, no token: the brain signed in to last
	if out, _ := carol.ok("whoami"); !strings.Contains(out, "carol (Carol) on "+s.URL+" — tier users, via oidc") {
		t.Fatalf("whoami: %s", out)
	}
	if s.Iss.Refreshes() != 0 {
		t.Fatal("refreshed a token that still lived")
	}
	// a minute later the token has ended: refreshed, once, and the new
	// refresh token kept (each is good once)
	later := func(d time.Duration) func(*Env) {
		return func(e *Env) { e.Now = func() time.Time { return time.Now().Add(d) } }
	}
	for i := 1; i <= 2; i++ {
		env, out, errb := carol.env()
		later(time.Duration(i) * time.Minute)(env)
		if Main([]string{"whoami"}, env) != 0 || !strings.Contains(out.String(), "carol") {
			t.Fatalf("whoami %d after the token ended: %s%s", i, out, errb)
		}
		if s.Iss.Refreshes() != i {
			t.Fatalf("refreshes after %d: %d", i, s.Iss.Refreshes())
		}
	}
	// signed in, she asks: a machine, from the flags its schema draws
	out, _ := carol.ok("machine", "create", "--kind", "container", "--image", "debian-13", "--cores", "2", "--memory-gb", "2", "-o", "id")
	if !strings.HasPrefix(out, "m-") {
		t.Fatalf("create: %q", out)
	}
	// logout: revoked at the provider, forgotten here
	carol.ok("logout")
	if s.Iss.Revoked() != 1 {
		t.Fatalf("revoked: %d", s.Iss.Revoked())
	}
	if _, errs, code := carol.run("whoami"); code != 1 || !strings.Contains(errs, "no brain: hangar login URL") {
		t.Fatalf("after logout: %d %s", code, errs)
	}

	// a brain with no provider: an API token, read from stdin
	s2 := stacktest.New(t, strings.Replace(labConfig, "  oidc: {issuer: %[2]s, audience: hangar-cli}\n", "  tokens: {max_ttl: 24h}\n  # %[2]s\n", 1), "machines", "volumes")
	dave := &cmd{t: t, home: t.TempDir(), vars: map[string]string{}}
	if _, errs, code := dave.run("login", s2.URL); code != 1 || !strings.Contains(errs, "has no identity provider: sign in with an API token") {
		t.Fatalf("no provider: %d %s", code, errs)
	}
	dave.stdin = s2.Token(t, "dave", "users") + "\n"
	if _, errs := dave.ok("login", s2.URL, "--with-token"); !strings.Contains(errs, "Signed in to "+s2.URL+" as dave") {
		t.Fatal(errs)
	}
	dave.stdin = ""
	if out, _ := dave.ok("whoami"); !strings.Contains(out, "dave") || !strings.Contains(out, "via token:") {
		t.Fatalf("whoami: %s", out)
	}
}

// The hours at the command line: idle_after is a flag of the type, its
// actions are commands drawn from the plugin's schema, and `limits` shows the
// month's meter — what was consumed, of how much a month, and when it is back.
func TestTheHoursAtTheCommandLine(t *testing.T) {
	s := stacktest.New(t, labConfig, "machines", "volumes")
	alice := newCmd(t, s.URL, s.Token(t, "alice", "users"))
	id, _ := alice.ok("machine", "create", "--kind", "container", "--image", "debian-13", "--cores", "2", "--memory-gb", "1", "--idle-after", "45m", "-o", "id")
	id = strings.TrimSpace(id)
	if out, _ := alice.ok("machine", "get", id, "-o", "json"); !strings.Contains(out, `"idle_after": "45m"`) {
		t.Fatalf("born with its idle_after: %s", out)
	}
	if _, errs, code := alice.run("machine", "create", "--image", "debian-13", "--idle-after", "1m"); code == 0 || !strings.Contains(errs, "between 5m and 12h") {
		t.Fatalf("an idle_after of a minute: %d %s", code, errs)
	}
	alice.ok("machine", "keep-awake", id, "--for", "2h")
	if out, _ := alice.ok("machine", "get", id, "-o", "json"); !strings.Contains(out, `"awake": "20`) {
		t.Fatalf("kept awake until a time: %s", out)
	}
	alice.ok("machine", "keep-awake", id)
	if out, _ := alice.ok("machine", "get", id, "-o", "json"); !strings.Contains(out, `"awake": "always"`) {
		t.Fatalf("kept awake until let-sleep: %s", out)
	}
	alice.ok("machine", "let-sleep", id)
	alice.ok("machine", "set-idle-after", id, "--idle-after", "never")
	if out, _ := alice.ok("machine", "get", id, "-o", "json"); strings.Contains(out, "idle_after") || strings.Contains(out, `"awake"`) {
		t.Fatalf("let sleep, never idle: %s", out)
	}
	if out, _ := alice.ok("machine", "--help"); !strings.Contains(out, "keep-awake") || !strings.Contains(out, "--idle-after") {
		t.Fatalf("the type's help: %s", out)
	}
	out, _ := alice.ok("limits")
	var meter string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "machines.vcpu_hours") {
			meter = strings.Join(strings.Fields(line), " ")
		}
	}
	next := time.Now().UTC().AddDate(0, 1, -time.Now().UTC().Day()+1).Format("2 January")
	if want := "machines.vcpu_hours 0 1000 a month, back on " + next + " vCPU-hours"; meter != want {
		t.Fatalf("the month's line reads %q, want %q\n%s", meter, want, out)
	}
}
