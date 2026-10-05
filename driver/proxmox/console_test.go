package proxmox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tomblancdev/hangar/driver"
)

// proxy is Proxmox's terminal proxy as a live node answered it: a ticket
// that is its asker's, a socket that takes « user:ticket » and says OK, then
// the proxy's own first line, then the guest's port; what is typed arrives
// as "0:<length>:<bytes>", a window as "1:<cols>:<rows>:", a ping as "2".
type proxy struct {
	mu     sync.Mutex
	status string   // the guest's power
	pid    int      // its process: another one once it runs anew
	cut    bool     // the socket's opening is dropped on the floor
	heard  []string // every frame after the ticket
	conns  int
	// hold: what the port says after its first line, a frame each
	says []string
}

func (p *proxy) frames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.heard...)
}

func (p *proxy) mount(a *api, vmid int) {
	base := fmt.Sprintf("/nodes/node-a/qemu/%d", vmid)
	a.h["GET "+base+"/status/current"] = func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		data(w, map[string]any{"status": p.status, "pid": p.pid})
	}
	a.h["POST "+base+"/termproxy"] = func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("serial") != "serial0" {
			http.Error(w, "", http.StatusBadRequest)
			return
		}
		data(w, map[string]any{"user": "tok@pve!t", "ticket": "PVEVNC:6A:ticket==", "port": 5900, "upid": "UPID:node-a:0:0:0:vncproxy:1:tok@pve!t:"})
	}
	a.h["GET "+base+"/vncwebsocket"] = func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		cut := p.cut
		p.mu.Unlock()
		if cut {
			if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = c.Close() // no answer at all: the transport's own failure
			}
			return
		}
		if r.URL.Query().Get("port") != "5900" || r.URL.Query().Get("vncticket") != "PVEVNC:6A:ticket==" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		p.mu.Lock()
		p.conns++
		p.mu.Unlock()
		ctx := r.Context()
		_, first, err := c.Read(ctx)
		if err != nil || string(first) != "tok@pve!t:PVEVNC:6A:ticket==\n" {
			return // the proxy lets go of whoever does not say the ticket
		}
		// OK and the start of its own line in one frame, the rest in the next
		_ = c.Write(ctx, websocket.MessageBinary, []byte("OKstarting serial term"))
		_ = c.Write(ctx, websocket.MessageBinary, []byte("inal on interface serial0\r\n"))
		for _, s := range p.says {
			_ = c.Write(ctx, websocket.MessageBinary, []byte(s))
		}
		for {
			_, msg, err := c.Read(ctx)
			if err != nil {
				return
			}
			p.mu.Lock()
			p.heard = append(p.heard, string(msg))
			p.mu.Unlock()
			// what is typed comes back, as a getty's echo
			if rest, ok := strings.CutPrefix(string(msg), "0:"); ok {
				_, keys, _ := strings.Cut(rest, ":")
				_ = c.Write(ctx, websocket.MessageBinary, []byte(keys))
			}
		}
	}
}

func consoleAPI(t *testing.T) (*api, *proxy) {
	t.Helper()
	a := newAPI()
	a.perms["/pool/hangar"]["VM.Console"] = 1
	a.res = []resource{
		{VMID: 11000, Node: "node-a", Type: "qemu", Pool: "hangar", Tags: "hangar-id.m-1"},
		{VMID: 11001, Node: "node-a", Type: "lxc", Pool: "hangar", Tags: "hangar-id.m-2"},
	}
	p := &proxy{status: "running", pid: 4242, says: []string{"\r\nbox login: "}}
	p.mount(a, 11000)
	return a, p
}

func readFor(t *testing.T, c driver.Console, want string) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		var all strings.Builder
		buf := make([]byte, 4096)
		for !strings.Contains(all.String(), want) {
			n, err := c.Read(buf)
			all.Write(buf[:n])
			if err != nil {
				break
			}
		}
		got <- all.String()
	}()
	select {
	case s := <-got:
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("the console never said %q", want)
		return ""
	}
}

// A console is advertised only where the token may open one — VM.Console on
// its pool — and a VM alone has one.
func TestAConsoleIsOfferedWhereTheTokenMayOpenIt(t *testing.T) {
	a, _ := consoleAPI(t)
	d := open(t, a, nil)
	if !has(d.Capabilities(), driver.GuestConsole) || !d.Traits("vm").Console || d.Traits("container").Console {
		t.Fatalf("a token with VM.Console on its pool: %v %+v", d.Capabilities(), d.Traits("vm"))
	}
	// the control: the same token without it — and with it elsewhere only
	delete(a.perms["/pool/hangar"], "VM.Console")
	a.perms["/pool/hangar-images"] = map[string]int{"VM.Console": 1}
	d = open(t, a, nil)
	if has(d.Capabilities(), driver.GuestConsole) || d.Traits("vm").Console {
		t.Fatalf("a token without VM.Console on its pool offers a console: %v", d.Capabilities())
	}
	if _, err := d.Console(context.Background(), "m-1", driver.ConsoleSize{}); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("a console where the token may open none: %v", err)
	}
}

// The driver speaks the proxy's own words: the ticket first, the proxy's
// line left out of what the guest says, what is typed framed as the proxy
// reads it — a paste in pieces —, a window's size, and nothing else.
func TestAConsoleSpeaksTheProxysWords(t *testing.T) {
	a, p := consoleAPI(t)
	d := open(t, a, nil)
	ctx := context.Background()
	c, err := d.Console(ctx, "m-1", driver.ConsoleSize{Cols: 132, Rows: 41})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := readFor(t, c, "login: "); got != "\r\nbox login: " {
		t.Fatalf("what the guest says, and none of the proxy's own line: %q", got)
	}
	if _, err := c.Write([]byte("root\r")); err != nil {
		t.Fatal(err)
	}
	if got := readFor(t, c, "root\r"); got != "root\r" {
		t.Fatalf("what is typed comes back: %q", got)
	}
	paste := strings.Repeat("x", consoleChunk+10)
	if n, err := c.Write([]byte(paste)); err != nil || n != len(paste) {
		t.Fatalf("a paste: %d %v", n, err)
	}
	readFor(t, c, strings.Repeat("x", 10))
	if err := c.Resize(driver.ConsoleSize{Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(p.frames()) < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	want := []string{"1:132:41:", "0:5:root\r", fmt.Sprintf("0:%d:%s", consoleChunk, paste[:consoleChunk]), "0:10:xxxxxxxxxx", "1:80:24:"}
	if got := p.frames(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the frames the proxy heard:\n%q\nwant\n%q", got, want)
	}
	// the calls: the guest's power read first, the ticket asked, the socket
	var calls []string
	a.mu.Lock()
	for _, call := range a.calls {
		if strings.Contains(call, "/qemu/11000/") {
			calls = append(calls, strings.TrimPrefix(call, "/api2/json"))
		}
	}
	a.mu.Unlock()
	if fmt.Sprint(calls) != "[GET /api2/json/nodes/node-a/qemu/11000/status/current POST /api2/json/nodes/node-a/qemu/11000/termproxy GET /api2/json/nodes/node-a/qemu/11000/vncwebsocket]" {
		t.Fatalf("the calls a console makes: %v", calls)
	}
}

// Proxmox says nothing when a guest stops under an open socket: the driver
// looks at its power itself, and ends the console with the reason. A
// stopped guest opens none, a container has none, and a guest outside the
// pool is not found.
func TestAConsoleEndsWithItsGuest(t *testing.T) {
	a, p := consoleAPI(t)
	a.res = append(a.res, resource{VMID: 100, Node: "node-a", Type: "qemu", Tags: "hangar-id.m-9"})
	d := open(t, a, nil)
	d.consoleLook = 20 * time.Millisecond
	ctx := context.Background()

	c, err := d.Console(ctx, "m-1", driver.ConsoleSize{})
	if err != nil {
		t.Fatal(err)
	}
	readFor(t, c, "login: ")
	p.mu.Lock()
	p.status = "stopped"
	p.mu.Unlock()
	ended := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := c.Read(buf); err != nil {
				ended <- err
				return
			}
		}
	}()
	select {
	case err := <-ended:
		if !errors.Is(err, driver.ErrConsoleStopped) {
			t.Fatalf("a guest stopped under its console: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guest stopped, and its console stayed open")
	}
	if _, err := c.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("typing into a console that ended: %v", err)
	}

	// the same, started again between two looks: it runs, as another process
	p.mu.Lock()
	p.status, p.pid = "running", 4343
	p.mu.Unlock()
	again, err := d.Console(ctx, "m-1", driver.ConsoleSize{})
	if err != nil {
		t.Fatal(err)
	}
	readFor(t, again, "login: ")
	p.mu.Lock()
	p.pid = 4444
	p.mu.Unlock()
	restarted := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := again.Read(buf); err != nil {
				restarted <- err
				return
			}
		}
	}()
	select {
	case err := <-restarted:
		if !errors.Is(err, driver.ErrConsoleRestarted) {
			t.Fatalf("a guest that runs anew under its console: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guest ran anew, and its console stayed open on the old one")
	}
	p.mu.Lock()
	p.status = "stopped"
	p.mu.Unlock()

	if _, err := d.Console(ctx, "m-1", driver.ConsoleSize{}); !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), "does not run") {
		t.Fatalf("a stopped guest's console: %v", err)
	}
	if _, err := d.Console(ctx, "m-2", driver.ConsoleSize{}); !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), "container") {
		t.Fatalf("a container's console: %v", err)
	}
	if _, err := d.Console(ctx, "m-9", driver.ConsoleSize{}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("the console of a guest outside the pool: %v", err)
	}
	p.mu.Lock()
	n := p.conns
	p.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d sockets opened, of two consoles", n)
	}
}

// A socket that cannot be opened is said without its address: the ticket
// rides in it, and an error is read by whoever asked and kept in the audit.
func TestAConsolesErrorCarriesNoTicket(t *testing.T) {
	a, p := consoleAPI(t)
	d := open(t, a, nil)
	p.mu.Lock()
	p.cut = true
	p.mu.Unlock()
	_, err := d.Console(context.Background(), "m-1", driver.ConsoleSize{})
	if err == nil {
		t.Fatal("a socket dropped at its opening opened a console")
	}
	for _, never := range []string{"vncticket", "PVEVNC", "ticket==", "127.0.0.1"} {
		if strings.Contains(err.Error(), never) {
			t.Fatalf("the error of a socket that did not open holds %q: %v", never, err)
		}
	}
	if !strings.Contains(err.Error(), "its terminal socket") {
		t.Fatalf("the error says what could not be opened: %v", err)
	}
}

// A guest born signed in: the step rides on its seed disc beside its
// owner's user data; one that is not carries its user data as it is.
func TestASignedInGuestsSeedCarriesTheStep(t *testing.T) {
	own := []byte("#cloud-config\npackages: [htop]\n")
	plain := nocloudSeed("m-1", "box", nil, own, nil, time.Unix(0, 0))
	signed := nocloudSeed("m-1", "box", nil, driver.FirstBoot(own, driver.SignedInConsole), nil, time.Unix(0, 0))
	if !strings.Contains(string(plain), string(own)) || strings.Contains(string(plain), "multipart") {
		t.Fatal("a guest that is not born signed in: its user data, as it is")
	}
	if !strings.Contains(string(signed), "multipart/mixed") || !strings.Contains(string(signed), "text/cloud-boothook") ||
		!strings.Contains(string(signed), `filename="hangar-terminal"`) {
		t.Fatal("a guest born signed in: a multipart, the step in it")
	}
}
