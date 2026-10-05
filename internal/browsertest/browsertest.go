// Package browsertest drives a real browser in a test: Chromium (or its
// headless shell), spoken to over the DevTools protocol on a pipe — the
// standard library alone, nothing to install but the browser. The console's
// app is proved in it: the pages a person sees, clicked as a person clicks.
//
// $HANGAR_BROWSER names the browser's program; without it the tests that
// need one skip. $HANGAR_SHOTS names a directory to keep screenshots in.
package browsertest

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Browser is a running browser.
type Browser struct {
	t   testing.TB
	cmd *exec.Cmd
	to  *os.File

	mu      sync.Mutex
	next    int
	waiting map[int]chan reply
	pages   map[string]*Page // by session
}

type reply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type message struct {
	ID        int             `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId"`
	reply
}

// Start runs the browser for the test's lifetime, or skips the test when no
// browser is named.
func Start(t testing.TB) *Browser {
	t.Helper()
	bin := os.Getenv("HANGAR_BROWSER")
	if bin == "" {
		t.Skip("no browser: set HANGAR_BROWSER to a Chromium (or its headless shell)")
	}
	dir, err := os.MkdirTemp("", "hb")
	if err != nil {
		t.Fatal(err)
	}
	// the pipe: the browser reads commands on its descriptor 3, answers on 4
	toR, toW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	fromR, fromW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--headless", "--remote-debugging-pipe", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--no-first-run", "--hide-scrollbars", "--user-data-dir="+dir, "about:blank")
	cmd.ExtraFiles = []*os.File{toR, fromW}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("the browser %s: %v", bin, err)
	}
	_ = toR.Close()
	_ = fromW.Close()
	b := &Browser{t: t, cmd: cmd, to: toW, waiting: map[int]chan reply{}, pages: map[string]*Page{}}
	go b.read(fromR)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = toW.Close()
		_ = os.RemoveAll(dir)
	})
	return b
}

func (b *Browser) read(from *os.File) {
	rd := bufio.NewReaderSize(from, 1<<20)
	for {
		raw, err := rd.ReadBytes(0)
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(raw[:len(raw)-1], &m) != nil {
			continue
		}
		b.mu.Lock()
		if m.ID != 0 {
			if ch := b.waiting[m.ID]; ch != nil {
				delete(b.waiting, m.ID)
				ch <- m.reply
			}
		} else if p := b.pages[m.SessionID]; p != nil {
			p.event(m.Method, m.Params)
		}
		b.mu.Unlock()
	}
}

func (b *Browser) call(session, method string, params any, out any) error {
	b.mu.Lock()
	b.next++
	id := b.next
	ch := make(chan reply, 1)
	b.waiting[id] = ch
	b.mu.Unlock()
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	raw, _ := json.Marshal(msg)
	if _, err := b.to.Write(append(raw, 0)); err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return fmt.Errorf("%s: %s", method, r.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-time.After(60 * time.Second):
		return fmt.Errorf("%s: the browser did not answer", method)
	}
}

// Page is one tab.
type Page struct {
	b       *Browser
	t       testing.TB
	session string

	// Patience: how long a wait may last (default 20 s; a real engine takes
	// minutes to make a machine).
	Patience time.Duration

	mu     sync.Mutex
	errors []string
}

// Page opens a tab of the given size; phone: a touch screen's.
func (b *Browser) Page(width, height int, phone bool) *Page {
	b.t.Helper()
	var target struct {
		TargetID string `json:"targetId"`
	}
	// a browser context of its own: its cookies are its alone
	var ctx struct {
		ID string `json:"browserContextId"`
	}
	b.must(b.call("", "Target.createBrowserContext", nil, &ctx))
	b.must(b.call("", "Target.createTarget", map[string]any{"url": "about:blank", "browserContextId": ctx.ID}, &target))
	var att struct {
		SessionID string `json:"sessionId"`
	}
	b.must(b.call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &att))
	p := &Page{b: b, t: b.t, session: att.SessionID}
	b.mu.Lock()
	b.pages[p.session] = p
	b.mu.Unlock()
	b.must(p.call("Page.enable", nil, nil))
	b.must(p.call("Runtime.enable", nil, nil))
	b.must(p.call("Log.enable", nil, nil))
	b.must(p.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": height, "deviceScaleFactor": 1, "mobile": phone}, nil))
	if phone {
		// a finger, not a mouse: what a page shows only to a touch screen
		b.must(p.call("Emulation.setTouchEmulationEnabled", map[string]any{"enabled": true}, nil))
	}
	// no motion: what is drawn is there at once, and a picture shows it whole
	b.must(p.call("Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}}, nil))
	return p
}

func (b *Browser) must(err error) {
	b.t.Helper()
	if err != nil {
		b.t.Fatal(err)
	}
}

func (p *Page) call(method string, params any, out any) error {
	return p.b.call(p.session, method, params, out)
}

// event keeps what the page complains of: an exception, an error on its
// console, a load its policy refused.
func (p *Page) event(method string, params json.RawMessage) {
	var e struct {
		Type string `json:"type"`
		Args []struct {
			Value       any    `json:"value"`
			Description string `json:"description"`
		} `json:"args"`
		ExceptionDetails struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
		Entry struct {
			Level string `json:"level"`
			Text  string `json:"text"`
			URL   string `json:"url"`
		} `json:"entry"`
	}
	_ = json.Unmarshal(params, &e)
	var line string
	switch method {
	case "Runtime.exceptionThrown":
		line = "exception: " + e.ExceptionDetails.Text + " " + e.ExceptionDetails.Exception.Description
	case "Runtime.consoleAPICalled":
		if e.Type != "error" {
			return
		}
		for _, a := range e.Args {
			line += fmt.Sprint(a.Value, a.Description, " ")
		}
		line = "console.error: " + line
	case "Log.entryAdded":
		// a refusal the page asked for and shows is no complaint of its own
		if e.Entry.Level != "error" || strings.HasPrefix(e.Entry.Text, "Failed to load resource: the server responded with a status of 4") {
			return
		}
		line = "log: " + e.Entry.Text + " " + e.Entry.URL
	default:
		return
	}
	p.mu.Lock()
	p.errors = append(p.errors, strings.TrimSpace(line))
	p.mu.Unlock()
}

// Errors are what the page complained of since the last call, forgotten once read.
func (p *Page) Errors() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.errors
	p.errors = nil
	return out
}

// Goto loads an address.
func (p *Page) Goto(url string) {
	p.t.Helper()
	if err := p.call("Page.navigate", map[string]any{"url": url}, nil); err != nil {
		p.t.Fatal(err)
	}
}

// Eval runs an expression in the page (a promise is waited for) and reads
// its value into out.
func (p *Page) Eval(js string, out any) error {
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := p.call("Runtime.evaluate", map[string]any{"expression": js, "awaitPromise": true, "returnByValue": true}, &res); err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		return fmt.Errorf("in the page: %s %s", res.ExceptionDetails.Text, res.ExceptionDetails.Exception.Description)
	}
	if out != nil && len(res.Result.Value) > 0 {
		return json.Unmarshal(res.Result.Value, out)
	}
	return nil
}

// helpers the page is given: how a person finds things — by what they read.
const helpers = `(() => {
  if (window.__t) return;
  const norm = (s) => (s || '').replace(/\s+/g, ' ').trim().toLowerCase();
  const shown = (el) => !!(el && el.getClientRects().length && getComputedStyle(el).visibility !== 'hidden');
  window.__t = {
    norm, shown,
    // a button or a link, by its words
    press(words) {
      const want = norm(words);
      const all = [...document.querySelectorAll('button, a')].filter(shown);
      return all.find((el) => norm(el.textContent) === want) || all.find((el) => norm(el.textContent).startsWith(want)) || null;
    },
    // a field's control, by its label's words (within a root, when given)
    field(words, root) {
      const want = norm(words);
      const scope = root ? document.querySelector(root) : document;
      if (!scope) return null;
      for (const l of scope.querySelectorAll('label.lbl, legend.lbl')) {
        if (!shown(l)) continue;
        const text = norm(l.textContent).replace(/ \*$/, '').replace(/ → .*$/, '');
        if (text !== want) continue;
        if (l.tagName === 'LABEL' && l.htmlFor) return document.getElementById(l.htmlFor);
        return l.parentElement;
      }
      return null;
    },
    text() { return document.body.innerText; },
  };
})()`

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Wait waits until an expression is truthy in the page.
func (p *Page) Wait(what, js string) {
	p.t.Helper()
	var last error
	patience := p.Patience
	if patience == 0 {
		patience = 20 * time.Second
	}
	for deadline := time.Now().Add(patience); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var ok bool
		if last = p.Eval(helpers+"; !!("+js+")", &ok); last == nil && ok {
			return
		}
	}
	p.Shot("stuck")
	p.t.Fatalf("waiting for %s: never (%v)\nthe page reads:\n%s\nit complained of: %v", what, last, p.Text(), p.Errors())
}

// Text is what the page reads, as a person would select it all.
func (p *Page) Text() string {
	var s string
	_ = p.Eval(helpers+"; __t.text()", &s)
	return s
}

// Sees waits for words to be on the page.
func (p *Page) Sees(words string) {
	p.t.Helper()
	p.Wait("the words "+quote(words), "__t.text().toLowerCase().includes("+quote(strings.ToLower(words))+")")
}

// Lacks fails if the words are on the page.
func (p *Page) Lacks(words string) {
	p.t.Helper()
	if strings.Contains(strings.ToLower(p.Text()), strings.ToLower(words)) {
		p.t.Fatalf("the page reads %q:\n%s", words, p.Text())
	}
}

// Press clicks the button or link with these words.
func (p *Page) Press(words string) {
	p.t.Helper()
	p.Wait("a button "+quote(words), "__t.press("+quote(words)+")")
	if err := p.Eval("__t.press("+quote(words)+").click()", nil); err != nil {
		p.t.Fatal(err)
	}
}

// Fill types into the field with this label.
func (p *Page) Fill(label, value string) {
	p.t.Helper()
	p.Wait("a field "+quote(label), "__t.field("+quote(label)+")")
	js := `(() => { const el = __t.field(` + quote(label) + `);
	  if (!('value' in el)) throw new Error('the field ' + ` + quote(label) + ` + ' is not one to type in');
	  el.focus(); el.value = ` + quote(value) + `;
	  el.dispatchEvent(new Event('input', {bubbles: true})); el.dispatchEvent(new Event('change', {bubbles: true}));
	  if (el.value !== ` + quote(value) + `) throw new Error('the field ' + ` + quote(label) + ` + ' did not take ' + ` + quote(value) + ` + ': it offers ' + [...(el.options || [])].map((o) => o.value).join(', '));
	})()`
	if err := p.Eval(js, nil); err != nil {
		p.t.Fatal(err)
	}
}

// Choose ticks (or picks) the choice reading these words in the group with
// this label; on: whether it ends ticked.
func (p *Page) Choose(label, words string, on bool) {
	p.t.Helper()
	p.Wait("a choice "+quote(label), "__t.field("+quote(label)+")")
	js := `(() => { const g = __t.field(` + quote(label) + `);
	  const pick = [...g.querySelectorAll('label.pick')].find((l) => __t.norm(l.textContent).startsWith(__t.norm(` + quote(words) + `)));
	  if (!pick) throw new Error('no choice ' + ` + quote(words) + ` + ' in ' + ` + quote(label) + ` + ': ' + g.innerText);
	  const box = pick.querySelector('input');
	  if (box.checked !== ` + fmt.Sprint(on) + `) box.click();
	})()`
	if err := p.Eval(js, nil); err != nil {
		p.t.Fatal(err)
	}
}

// Options lists what a select with this label offers, by its words.
func (p *Page) Options(label string) []string {
	p.t.Helper()
	p.Wait("a field "+quote(label), "__t.field("+quote(label)+")")
	var out []string
	if err := p.Eval("[...__t.field("+quote(label)+").options].map((o) => o.textContent)", &out); err != nil {
		p.t.Fatal(err)
	}
	return out
}

// Pick chooses, in the select with this label, the option whose words begin so.
func (p *Page) Pick(label, words string) {
	p.t.Helper()
	p.Wait("a field "+quote(label), "__t.field("+quote(label)+")")
	js := `(() => { const el = __t.field(` + quote(label) + `);
	  const o = [...el.options].find((o) => __t.norm(o.textContent).startsWith(__t.norm(` + quote(words) + `)));
	  if (!o) throw new Error('no option ' + ` + quote(words) + ` + ' in ' + ` + quote(label) + ` + ': ' + [...el.options].map((o) => o.textContent).join(' | '));
	  el.value = o.value; el.dispatchEvent(new Event('change', {bubbles: true}));
	})()`
	if err := p.Eval(js, nil); err != nil {
		p.t.Fatal(err)
	}
}

// Shot keeps a picture of the page in $HANGAR_SHOTS, when set.
func (p *Page) Shot(name string) {
	dir := os.Getenv("HANGAR_SHOTS")
	if dir == "" {
		return
	}
	// a picture is of the page as it settles: its images and its faces loaded
	if name != "stuck" {
		p.Wait("the page's images and faces", "[...document.images].every((i) => i.complete && i.naturalWidth > 0) && document.fonts.status === 'loaded'")
	}
	// the whole page, not only what the window shows
	var m struct {
		Size struct {
			Width  float64 `json:"width"`
			Height float64 `json:"height"`
		} `json:"cssContentSize"`
	}
	if err := p.call("Page.getLayoutMetrics", nil, &m); err != nil {
		p.t.Logf("no picture %s: %v", name, err)
		return
	}
	var res struct {
		Data string `json:"data"`
	}
	if err := p.call("Page.captureScreenshot", map[string]any{"format": "png", "captureBeyondViewport": true,
		"clip": map[string]any{"x": 0, "y": 0, "width": m.Size.Width, "height": m.Size.Height, "scale": 1}}, &res); err != nil {
		p.t.Logf("no picture %s: %v", name, err)
		return
	}
	raw, err := base64.StdEncoding.DecodeString(res.Data)
	if err != nil {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, name+".png"), raw, 0o644)
}

// Submit presses the form's own button — the one that asks.
func (p *Page) Submit() {
	p.t.Helper()
	const find = `[...document.querySelectorAll('form button[type=submit]')].find(__t.shown)`
	p.Wait("a form to send", find)
	if err := p.Eval(find+".click()", nil); err != nil {
		p.t.Fatal(err)
	}
}

// Hash is where in the app the page is.
func (p *Page) Hash() string {
	var s string
	_ = p.Eval("location.hash", &s)
	return s
}

// Open goes to a place in the app.
func (p *Page) Open(hash string) {
	p.t.Helper()
	if err := p.Eval("location.hash = "+quote(hash), nil); err != nil {
		p.t.Fatal(err)
	}
}

// Quiet fails the test if the page complained of anything.
func (p *Page) Quiet() {
	p.t.Helper()
	if errs := p.Errors(); len(errs) > 0 {
		p.t.Fatalf("the page complained: %v", errs)
	}
}

// Reads waits until the element a selector names reads exactly these words
// (case and spacing aside) — where Sees would be content with the same
// letters anywhere on the page.
func (p *Page) Reads(selector, words string) {
	p.t.Helper()
	p.Wait(quote(selector)+" reading "+quote(words),
		"(() => { const el = document.querySelector("+quote(selector)+"); return el && __t.norm(el.textContent) === __t.norm("+quote(words)+"); })()")
}

// Type types into whatever holds the focus, as a keyboard does, then — with
// enter — presses Enter. For pages that are not the console's own: a
// provider's sign-in form.
func (p *Page) Type(text string, enter bool) {
	p.t.Helper()
	if err := p.call("Input.insertText", map[string]any{"text": text}, nil); err != nil {
		p.t.Fatal(err)
	}
	if !enter {
		return
	}
	for _, kind := range []string{"keyDown", "keyUp"} {
		if err := p.call("Input.dispatchKeyEvent", map[string]any{
			"type": kind, "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13, "text": "\r",
		}, nil); err != nil {
			p.t.Fatal(err)
		}
	}
}
