package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/browsertest"
	"github.com/tomblancdev/hangar/internal/testoidc"
)

// A machine with no key, entered from the page — on a real Proxmox VE: the
// binary's brain serving its console, a person signed in at an identity
// provider. She asks for a VM and names no key pair; its page offers its
// terminal; she opens it and is in, as the machine's user, nothing asked —
// at her window's size, with its colours. The whole window and back is the
// same terminal, carried: nothing is opened again, so nothing has to be
// drawn again. Opened again on her phone, her shell is where she left it and
// says nothing by itself: the page says so over an empty screen, and its key
// asks the machine to draw. Another person finds neither the machine nor its
// terminal. An operator finds the machine, no terminal on its page, and is
// refused one by its address. The brain's audit holds her opening and her
// closing — how long, how many bytes — and not a word of what she typed.
// Without the bench's variables, or without a browser, it skips:
//
//	sh tools/bench/bench.sh up && eval "$(sh tools/bench/bench.sh env)" && HANGAR_BROWSER=… go test ./cmd/hangar -run BenchATerminalInThePage -v -timeout 30m
func TestBenchATerminalInThePage(t *testing.T) {
	url := os.Getenv("HANGAR_BENCH_URL")
	if url == "" {
		t.Skip("no bench: sh tools/bench/bench.sh up, then eval its env")
	}
	br := browsertest.Start(t)
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
	alice, bob, olive := fmt.Sprintf("alice-%d", run), fmt.Sprintf("bob-%d", run), fmt.Sprintf("olive-%d", run)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cfg := filepath.Join(dir, "hangar.yaml")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, `
data_dir: %[1]s/data
console: {url: "http://%[6]s"}
identity:
  oidc: {issuer: %[5]s, audience: hangar}
tiers:
  - name: ops
    groups: [ops]
    operator: true
    zones: [bench]
    limits: {"*": unlimited, machines.kind: [vm, container], machines.class: [spot]}
  - name: users
    groups: [users]
    zones: [bench]
    limits:
      machines.count: 1
      machines.vcpu_hours: 1000
      machines.vcpu: 2
      machines.memory_gb: 2
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
      vmids: 11140-11149
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
    settings:
      images:
        debian-13: {vm: debian-13}
reconcile: {every: 10s}
`, dir, url, os.Getenv("HANGAR_BENCH_CA_FILE"), os.Getenv("HANGAR_BENCH_TOKEN_FILE"), iss.URL, addr), 0o600); err != nil {
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
	srv := exec.Command(bin, "serve", "--config", cfg)
	srv.Env = append(os.Environ(), "HANGAR_LISTEN="+addr)
	srv.Stdout, srv.Stderr = &logs, &logs
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill() })
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("never healthy\n%s", logs.String())
		}
	}
	api := func(method, path string) (int, map[string]any) {
		req, _ := http.NewRequest(method, "http://"+addr+path, nil)
		req.Header.Set("Authorization", "Bearer "+sweeper)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil
		}
		defer resp.Body.Close()
		out := map[string]any{}
		b, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}
	var id string
	// sweep lets go of the machine: the bench is left as it was found
	sweep := func() {
		if id == "" {
			return
		}
		for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
			if code, r := api("GET", "/v1/resources/"+id); code == 200 && r["state"] == "deleted" {
				return
			}
			api("DELETE", fmt.Sprintf("/v1/resources/%s?client_token=sweep-%d", id, time.Now().UnixNano()))
		}
		t.Errorf("%s was not let go of", id)
	}
	t.Cleanup(sweep) // whatever the test's end, while its brain still runs

	const (
		read   = `(document.querySelector('.term') && document.querySelector('.term').read ? document.querySelector('.term').read() : '')`
		noKeys = `!document.querySelector('.actions [data-stream]')`
		// the slip a terminal lays over its screen while its machine says nothing
		quiet = `document.querySelector('.term[data-quiet] .slip')`
		// the keyboard is the terminal's: what is typed goes to the machine
		keys = `document.activeElement && document.activeElement.closest('.term')`
	)
	state := func(p *browsertest.Page, word string) {
		t.Helper()
		p.Wait("the terminal "+word, `document.querySelector('.term[data-state="`+word+`"]')`)
	}
	says := func(p *browsertest.Page, what, pattern string) {
		t.Helper()
		p.Wait(what, "new RegExp("+fmt.Sprintf("%q", pattern)+").test("+read+")")
	}

	// ---- alice: a VM that names no key, asked from the page
	iss.SignedIn(&testoidc.Claims{Subject: alice, Name: "alice", Groups: []string{"users"}})
	p := br.Page(1280, 900, false)
	p.Goto("http://" + addr + "/console/#/t/machine/new")
	p.Press("SIGN IN")
	p.Sees("ASK FOR A MACHINE")
	p.Fill("name", "box")
	p.Fill("cores", "1")
	p.Fill("memory gb", "1")
	p.Fill("image", "debian-13")
	p.Submit()
	p.Wait("the new machine's page", "location.hash.startsWith('#/r/')")
	id = strings.TrimPrefix(p.Hash(), "#/r/")
	p.Patience = 4 * time.Minute
	p.Reads(".under .stamp", "RUNNING")
	p.Wait("its terminal, born open", `[...document.querySelectorAll('.kv-row')].some((r) => r.firstChild.textContent === 'terminal' && r.lastChild.textContent === 'open')`)

	// ---- in, with no key: her window's size, its colours, nothing asked
	began := time.Now()
	p.Press("terminal")
	state(p, "open")
	p.Patience = 6 * time.Minute
	says(p, "a shell as the machine's user", `debian \(automatic login\)[\s\S]*debian@box:~\$$`)
	t.Logf("a shell in the page %s after the terminal was opened", time.Since(began).Round(time.Second))
	p.Patience = 30 * time.Second
	var grid string
	if err := p.Eval(`document.querySelector('.term-grid').textContent`, &grid); err != nil {
		t.Fatal(err)
	}
	var cols, rows int
	if _, err := fmt.Sscanf(grid, "%d × %d", &cols, &rows); err != nil || cols < 80 || rows < 20 {
		t.Fatalf("the grid the bar shows: %q (%v)", grid, err)
	}
	p.Type("whoami; sudo -n id -u; stty size; echo $TERM; echo a-typed-secret", true)
	says(p, "who she is there, and the size the machine took", fmt.Sprintf(`\ndebian\n0\n%d %d\nxterm-256color\na-typed-secret\n`, rows, cols))
	p.Shot("50-bench-terminal")
	// under the page's own policy: nothing it had to block
	p.Quiet()

	// ---- the whole window, and back: the same terminal, carried — on a real
	// machine nothing else would draw its screen again
	p.Type("echo carried-over", true)
	says(p, "a mark on her screen", `\ncarried-over\n[^\n]*\$$`)
	p.Press("Full screen")
	p.Wait("the terminal's own address", "location.hash.endsWith('/terminal')")
	state(p, "open")
	says(p, "the same screen, in the whole window", `\ncarried-over\n[^\n]*\$$`)
	p.Wait("the keyboard its own again, in the whole window", keys)
	p.Type("echo in-the-whole-window", true)
	says(p, "typed in the whole window", `\nin-the-whole-window\n[^\n]*\$$`)
	p.Shot("50a-bench-terminal-whole-window")
	// a window that changed tells her shell nothing — its port carries no
	// size: it keeps its sign-in's, the bar says so, and exit signs her in
	// again at the size the window has now, which the machine asks for
	if err := p.Eval(`document.querySelector('.term-grid').textContent`, &grid); err != nil {
		t.Fatal(err)
	}
	var wcols, wrows int
	if _, err := fmt.Sscanf(grid, "%d × %d", &wcols, &wrows); err != nil || wcols <= cols || wrows <= rows {
		t.Fatalf("the whole window's grid: %q, no larger than %d × %d (%v)", grid, cols, rows, err)
	}
	p.Sees("resized: exit signs you in at this size")
	p.Type("stty size", true)
	says(p, "her shell's size, still its sign-in's", fmt.Sprintf(`stty size\n%d %d\n[^\n]*\$$`, rows, cols))
	p.Patience = time.Minute
	p.Type("exit", true)
	says(p, "signed in again, in the whole window", `logout[\s\S]*automatic login[\s\S]*debian@box:~\$$`)
	p.Type("stty size", true)
	says(p, "the whole window's size, taken at the new sign-in", fmt.Sprintf(`stty size\n%d %d\n[^\n]*\$$`, wrows, wcols))
	p.Wait("the bar no longer says « resized »: the machine asked the size again", `!document.querySelector('.term-grid .term-hint')`)
	p.Patience = 30 * time.Second
	p.Type("echo in-the-whole-window", true)
	says(p, "typed after the new sign-in", `\nin-the-whole-window\n[^\n]*\$$`)
	p.Press("Leave full screen")
	p.Wait("its page again", `location.hash.startsWith("#/r/`+id+`") && !location.hash.includes("/terminal")`)
	p.Wait("the terminal back under the keys", `document.querySelector('.term-panel .term[data-state="open"]')`)
	says(p, "the same screen, back in its page", `\nin-the-whole-window\n[^\n]*\$$`)
	p.Wait("the keyboard its own again, under the keys", keys)
	p.Type("echo back-under-the-keys", true)
	says(p, "typed under the keys again", `\nback-under-the-keys\n[^\n]*\$$`)
	if n := strings.Count(logs.String(), `"result":"opened"`); n != 1 {
		t.Fatalf("the whole window and back is the same terminal: the brain's audit holds %d openings", n)
	}
	// a line begun at the desk and not entered, left there
	p.Type("echo left-ha", false)
	says(p, "a line begun at the desk", `\$ echo left-ha$`)

	// ---- opened again elsewhere, by her: it moves there, and the first is
	// told. One port, one terminal: the engine serves the second only once
	// the first has let go, so the brain ends the first before it asks
	iss.SignedIn(&testoidc.Claims{Subject: alice, Name: "alice", Groups: []string{"users"}})
	phone := br.Page(390, 780, true)
	phone.Goto("http://" + addr + "/console/#/r/" + id + "/terminal")
	phone.Press("SIGN IN")
	state(phone, "open")
	state(p, "taken")
	// opened again: her shell is where she left it, and says nothing by
	// itself — an empty screen, which the page explains, with the key that
	// asks the machine to draw. Nothing is typed for her meanwhile
	phone.Wait("the page says her machine is quiet", quiet)
	var held string
	if err := phone.Eval(read, &held); err != nil || held != "" {
		t.Fatalf("a terminal opened again holds %q before anything is asked (%v)", held, err)
	}
	phone.Shot("52a-bench-terminal-opened-again")
	// Redraw: her shell draws its screen again, the line she had begun on it
	// — drawn, not entered: nothing it would have said is there
	phone.Press("Redraw")
	says(phone, "her shell, drawn at her asking: the line she left, not run", `^debian@box:~\$ echo left-ha$`)
	phone.Wait("the slip gone at the machine's first word", `!document.querySelector('.term[data-quiet]')`)
	phone.Shot("52b-bench-terminal-drawn-again")
	phone.Patience = time.Minute
	phone.Type("lf", true)
	says(phone, "the line left at the desk, ended on the phone", `\$ echo left-half\nleft-half\n[^\n]*\$$`)
	phone.Type("echo on-the-phone", true)
	says(phone, "the same machine, on the phone", `\non-the-phone\n`)
	phone.Shot("52-bench-terminal-phone")
	phone.Quiet()
	// …and back, by the notice's own key: the shell she left is still there
	p.Press("OPEN IT HERE")
	state(p, "open")
	state(phone, "taken")
	p.Type("echo back-at-the-desk", true)
	says(p, "the same machine, back at the desk", `\nback-at-the-desk\n`)

	// ---- rebooted from its page, the terminal open under the keys: Proxmox
	// ends the machine's process and starts another, and says nothing on the
	// socket — the terminal is opened again by itself, and she watches her
	// machine come back and sign her in
	p.Type("MARK=set-before-the-reboot", true)
	says(p, "a mark in the shell she has now", `MARK=set-before-the-reboot\n[^\n]*\$$`)
	p.Patience = 4 * time.Minute
	p.Press("reboot")
	p.Sees("reboot: done")
	// a sign-in AFTER the mark — or the mark gone with the screen, which a
	// machine's boot resets: either way, what is read is the new boot's
	p.Wait("the machine back on the same screen, signed in again", `(() => { const t = `+read+`;
	  const at = t.lastIndexOf('MARK=set-before-the-reboot');
	  return /automatic login/.test(at < 0 ? t : t.slice(at)) && /debian@box:~\$$/.test(t); })()`)
	state(p, "open")
	// a new shell: what the one before held is gone
	p.Type(`echo "after-the-reboot:[$MARK]"`, true)
	says(p, "her shell on the new boot", `\nafter-the-reboot:\[\]\n`)
	p.Patience = 30 * time.Second
	p.Shot("53-bench-terminal-rebooted")

	// ---- another person: neither the machine nor its terminal
	iss.SignedIn(&testoidc.Claims{Subject: bob, Name: "bob", Groups: []string{"users"}})
	other := br.Page(1280, 900, false)
	other.Goto("http://" + addr + "/console/#/r/" + id)
	other.Press("SIGN IN")
	other.Sees("not one you may see")
	other.Open("#/r/" + id + "/terminal")
	state(other, "refused")
	other.Sees("no resource " + id)

	// ---- an operator: the machine, and no way into it
	iss.SignedIn(&testoidc.Claims{Subject: olive, Name: "olive", Groups: []string{"ops"}})
	op := br.Page(1280, 900, false)
	op.Goto("http://" + addr + "/console/#/r/" + id)
	op.Press("SIGN IN")
	op.Sees("owner alice")
	op.Wait("the operator's keys on it", `__t.press('stop')`)
	op.Wait("no terminal offered to an operator", noKeys)
	op.Open("#/r/" + id + "/terminal")
	state(op, "refused")
	op.Sees("its terminal is its owner's alone")
	op.Shot("51-bench-terminal-refused")
	// hers was not touched by either attempt
	state(p, "open")
	p.Type("echo still-here", true)
	says(p, "her terminal, still hers", `\nstill-here\n`)

	// ---- she puts it away: the brain lets go, and the audit holds both ends
	p.Press("Close")
	p.Wait("the terminal put away", `!document.querySelector('.term')`)
	closed := regexp.MustCompile(`"result":"closed"[^\n]*"detail":"closed by its owner"`)
	for deadline := time.Now().Add(15 * time.Second); !closed.MatchString(logs.String()) && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
	}
	var opened, ended, refused []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, `"kind":"audit"`) || !strings.Contains(line, "/streams/{stream}") {
			continue
		}
		switch {
		case strings.Contains(line, `"result":"opened"`) && strings.Contains(line, `"actor":"`+alice+`"`) && strings.Contains(line, id):
			opened = append(opened, line)
		case strings.Contains(line, `"result":"closed"`) && strings.Contains(line, `"actor":"`+alice+`"`) && strings.Contains(line, id):
			ended = append(ended, line)
		case strings.Contains(line, `"result":"refused"`):
			refused = append(refused, line)
		}
	}
	// three openings — the desk, the phone, the desk again — each with its
	// closing: two taken, the last closed by her
	if len(opened) != 3 || len(ended) != 3 {
		t.Fatalf("the audit holds three openings and three closings of alice's terminal: %d and %d\n%s", len(opened), len(ended), strings.Join(append(opened, ended...), "\n"))
	}
	last := ended[len(ended)-1]
	if !strings.Contains(last, `"detail":"closed by its owner"`) || !regexp.MustCompile(`"bytes_in":"[1-9]\d*"`).MatchString(last) ||
		!regexp.MustCompile(`"bytes_out":"[1-9]\d*"`).MatchString(last) || !regexp.MustCompile(`"seconds":"\d+\.\d"`).MatchString(last) {
		t.Errorf("the closing says how long, how much and why: %s", last)
	}
	if n := strings.Count(strings.Join(ended, "\n"), `"reason":"taken"`); n != 2 {
		t.Errorf("%d of the closings say « taken », of two", n)
	}
	if len(refused) != 2 || !strings.Contains(strings.Join(refused, "\n"), `"actor":"`+olive+`"`) || !strings.Contains(strings.Join(refused, "\n"), `"reason":"owner"`) {
		t.Errorf("the audit holds the two refusals, the operator's by its reason:\n%s", strings.Join(refused, "\n"))
	}
	for _, typed := range []string{"a-typed-secret", "still-here", "stty size", "on-the-phone", "back-at-the-desk", "after-the-reboot", "set-before-the-reboot",
		"carried-over", "in-the-whole-window", "back-under-the-keys", "left-ha"} {
		if strings.Contains(logs.String(), typed) {
			t.Errorf("%q, typed in a terminal, is in the brain's log", typed)
		}
	}
	if strings.Contains(logs.String(), strings.TrimSpace(readFile(os.Getenv("HANGAR_BENCH_TOKEN_FILE")))) {
		t.Fatal("the engine token is in the brain's logs")
	}
	op.Quiet()
	other.Quiet()

	// the machine let go of, and the brain stopped: an open terminal or not,
	// it ends when asked
	sweep()
	id = ""
	_ = srv.Process.Signal(syscall.SIGTERM)
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatal("still running 20 s after SIGTERM")
	}
}
