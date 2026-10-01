package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/browsertest"
)

// The console's sign-in at a real identity provider — authentik, a throwaway
// one (tools/provider/authentik.sh) — in a real browser, through the
// provider's own pages: the binary's brain serving its console, a person
// typing their name and password at the provider, coming back signed in;
// their token renewed as it ends, twice (each refresh token is good once);
// someone whose groups reach no tier told so in the brain's words; the
// sign-out. Without the provider's variables, or a browser, it skips:
//
//	sh tools/provider/authentik.sh up && eval "$(sh tools/provider/authentik.sh env)" && HANGAR_BROWSER=… go test ./cmd/hangar -run ProviderTheConsole -v
func TestProviderTheConsole(t *testing.T) {
	issuer, at := os.Getenv("HANGAR_PROVIDER_ISSUER"), os.Getenv("HANGAR_PROVIDER_CONSOLE")
	if issuer == "" || at == "" {
		t.Skip("no provider: sh tools/provider/authentik.sh up, then eval its env")
	}
	br := browsertest.Start(t)
	password := strings.TrimSpace(readFile(os.Getenv("HANGAR_PROVIDER_PASSWORD_FILE")))
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
console: {url: "http://%[3]s"}
identity:
  oidc: {issuer: "%[2]s", audience: %[4]s}
tiers:
  - name: users
    groups: [users]
    zones: [z]
    limits: {toy.boxes: 2, toy.cores: 2, toy.memory_gb: 2, toy.kind: [container]}
zones:
  - {name: z, driver: fake, endpoint: %[1]s/zone.json}
plugins:
  - {name: toy, builtin: toy, zones: [z]}
reconcile: {every: 5s}
`, dir, issuer, at, os.Getenv("HANGAR_PROVIDER_CLIENT")), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs logBuffer
	srv := exec.Command(bin, "serve", "--config", cfg)
	srv.Env = append(os.Environ(), "HANGAR_LISTEN="+at)
	srv.Stdout, srv.Stderr = &logs, &logs
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	t.Cleanup(func() { _ = srv.Process.Kill() })
	base := "http://" + at
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("never healthy\n%s", logs.String())
		}
	}

	// the provider's own page: its fields live inside its components, found
	// as a person finds them — the one shown — and typed into as a keyboard does
	const shown = `((sel) => { const walk = (root) => { for (const el of root.querySelectorAll(sel)) { if (el.getClientRects().length) return el; }
	  for (const e of root.querySelectorAll('*')) { if (e.shadowRoot) { const r = walk(e.shadowRoot); if (r) return r; } } return null; }; return walk(document); })`
	typeAt := func(p *browsertest.Page, sel, value string) {
		t.Helper()
		q := fmt.Sprintf("%q", sel)
		p.Wait("the provider's field "+sel, shown+`(`+q+`)`)
		// the provider's page draws its field again as it settles: typed until
		// the field holds what was typed, then its own button pressed
		for try := 0; ; try++ {
			if err := p.Eval(`(() => { const el = `+shown+`(`+q+`); el.focus(); el.select(); })()`, nil); err != nil {
				t.Fatal(err)
			}
			p.Type(value, false)
			var held bool
			if err := p.Eval(`(() => { const el = `+shown+`(`+q+`); return !!el && el.value === `+fmt.Sprintf("%q", value)+`; })()`, &held); err == nil && held {
				break
			}
			if try == 20 {
				t.Fatalf("the provider's field %s never held what was typed", sel)
			}
			time.Sleep(250 * time.Millisecond)
		}
		if err := p.Eval(shown+`('button[type="submit"]').click()`, nil); err != nil {
			t.Fatal(err)
		}
		// gone: the provider took it and moved on
		p.Wait("the provider to take "+sel, `!`+shown+`(`+q+`)`)
	}
	signIn := func(p *browsertest.Page, user string) {
		t.Helper()
		p.Goto(base + "/")
		p.Sees("COME IN")
		p.Sees("You sign in at 127.0.0.1")
		p.Press("SIGN IN")
		typeAt(p, `input[name="uidField"]`, user)
		typeAt(p, `input[name="password"]`, password)
	}

	// ---- alice: in at the provider, back signed in
	p := br.Page(1280, 900, false)
	p.Patience = 60 * time.Second
	start := time.Now()
	signIn(p, os.Getenv("HANGAR_PROVIDER_USER"))
	p.Sees("YOUR LIMITS")
	p.Sees("tier users")
	t.Logf("signed in through the provider's pages in %s", time.Since(start).Round(time.Second))
	p.Shot("30-authentik-home")
	var me struct {
		Subject string   `json:"subject"`
		Name    string   `json:"name"`
		Groups  []string `json:"groups"`
		Via     string   `json:"via"`
	}
	if err := p.Eval(`fetch('api/v1/whoami').then((r) => r.json())`, &me); err != nil || me.Via != "oidc" || me.Name != "alice" {
		t.Fatalf("whoami: %+v %v", me, err)
	}
	t.Logf("the brain reads: subject %s, name %s, groups %v", me.Subject, me.Name, me.Groups)
	p.Press("+ Box")
	p.Fill("cores", "1")
	p.Submit()
	p.Wait("the box's page", "location.hash.startsWith('#/r/')")
	p.Reads(".under .stamp", "RUNNING")

	// ---- its token ends (a minute, at this provider): renewed, twice — the
	// second renewal with the refresh token the first one brought
	for i := 1; i <= 2; i++ {
		time.Sleep(70 * time.Second)
		var code int
		if err := p.Eval(`fetch('api/v1/whoami').then((r) => r.status)`, &code); err != nil || code != 200 {
			t.Fatalf("a call %d s after the sign-in: %d %v\n%s", i*70, code, err, logs.String())
		}
	}
	t.Log("two calls, each past the token's own minute: answered — the token was renewed twice")

	// ---- signing out; then someone whose groups reach no tier
	p.Press("Sign out")
	p.Sees("COME IN")
	if strings.Contains(logs.String(), "could not be told of a sign-out") {
		t.Errorf("the provider refused the revocation:\n%s", logs.String())
	}
	e := br.Page(1280, 900, false)
	e.Patience = 60 * time.Second
	signIn(e, "eve")
	e.Sees("your groups reach no tier")
	e.Shot("31-authentik-no-tier")
	var signed bool
	if err := e.Eval(`fetch('session').then((r) => r.json()).then((s) => s.signed_in)`, &signed); err != nil || signed {
		t.Fatalf("someone with no tier holds a sign-in: %v %v", signed, err)
	}

	_ = srv.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatal("still running 20 s after SIGTERM")
	}
	for what, secret := range map[string]string{"a token": "eyJ", "the password": password} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the brain's log holds %s", what)
		}
	}
	if n := strings.Count(logs.String(), `"event":"signin","result":"ok"`); n != 1 {
		t.Errorf("%d sign-ins logged, want alice's alone\n%s", n, logs.String())
	}
}
