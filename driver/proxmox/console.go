package proxmox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/tomblancdev/hangar/driver"
)

// A guest's console (guest.console): a VM's serial port, the one every
// template here is made with (serial0, the display on it), opened through
// Proxmox's own terminal proxy — the call its web interface makes.
//
// Three things about it, read on a live node before this was written:
//
//   - The token needs VM.Console, and the pool's role is where it is given:
//     without it the call is a 403, and on a guest outside the pool too. The
//     driver advertises guest.console only where the token holds it on its
//     pool.
//   - POST …/termproxy answers a port and a ticket that is its asker's alone
//     (root's own token is refused with it), and starts a task that ends by
//     itself after ten seconds if nobody comes. GET …/vncwebsocket is the
//     socket; inside it the proxy speaks its own few words: "user:ticket\n"
//     and it says OK; then "0:<length>:<bytes>" is what is typed,
//     "1:<cols>:<rows>:" a window's size, "2" a ping.
//   - A serial port has one other end. A second socket on the same guest
//     opens and stays silent while the first holds the port; and a guest
//     stopped under an open socket says nothing — neither an end nor a
//     word. So whoever holds the driver keeps one console per guest, and
//     this looks at the guest itself, every few seconds: its power, and
//     which process it is — a guest stopped and started between two looks,
//     or rebooted by Proxmox (which ends its process and starts another),
//     runs both times, on a port this socket no longer reaches.
//
// A container has no console here: it boots no user data, so nothing could
// make its port sign anyone in, and a login prompt nobody holds a password
// for is no way in.

// consoleSerial is the port a VM's console is: the templates' serial0.
const consoleSerial = "serial0"

// consoleChatter is the proxy's own first line, before anything the guest
// says: not the guest's, and left out.
const consoleChatter = "starting serial terminal on interface " + consoleSerial + "\r\n"

// consoleLook is how often an open console looks at its guest's power
// (Driver.consoleLook, when set, says otherwise).
const consoleLook = 5 * time.Second

// consolePing is how often the proxy is told the other end is still there,
// as Proxmox's own page does.
const consolePing = 30 * time.Second

// process reads a guest's power as it is now, and which process it is: a
// guest that runs anew is another one.
func (d *Driver) process(ctx context.Context, r resource) (running bool, pid int, err error) {
	var st struct {
		Status string `json:"status"`
		PID    any    `json:"pid"`
	}
	err = d.c.call(ctx, http.MethodGet, r.path()+"/status/current", nil, &st)
	return st.Status == "running", num(st.PID), err
}

// consoles says whether the token may open a console in its pool.
func (d *Driver) consoles(perms map[string]map[string]int) bool {
	return perms["/pool/"+d.pool]["VM.Console"] == 1
}

// Console opens a running VM's console.
func (d *Driver) Console(ctx context.Context, id string, size driver.ConsoleSize) (driver.Console, error) {
	r, err := d.find(ctx, id)
	if err != nil {
		return nil, d.engine(err)
	}
	if !d.Traits(r.kind()).Console {
		return nil, fmt.Errorf("%w: a %s has no console here", driver.ErrRefused, r.kind())
	}
	on, pid, err := d.process(ctx, r)
	if err != nil {
		return nil, d.engine(err)
	}
	if !on {
		return nil, fmt.Errorf("%w: it does not run", driver.ErrRefused)
	}
	var t struct {
		User   string `json:"user"`
		Ticket string `json:"ticket"`
		Port   any    `json:"port"`
	}
	if err := d.c.call(ctx, http.MethodPost, r.path()+"/termproxy", url.Values{"serial": {consoleSerial}}, &t); err != nil {
		return nil, d.engine(err)
	}
	port := num(t.Port)
	if t.Ticket == "" || port == 0 {
		return nil, errors.New("proxmox: the terminal proxy answered no ticket")
	}
	u := c2ws(d.c.base) + r.path() + "/vncwebsocket?" + url.Values{"port": {strconv.Itoa(port)}, "vncticket": {t.Ticket}}.Encode()
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(dctx, u, &websocket.DialOptions{
		// the API's own client, without its one-minute bound: a console is held
		HTTPClient:   &http.Client{Transport: d.c.http.Transport},
		HTTPHeader:   http.Header{"Authorization": {d.c.auth}},
		Subprotocols: []string{"binary"},
	})
	if err != nil {
		if resp != nil && resp.StatusCode >= 400 {
			return nil, d.engine(&apiError{Status: resp.StatusCode, Message: strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))})
		}
		// the socket's address carries the ticket: an error that names where
		// it could not reach is told without it
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, &unreachable{fmt.Errorf("its terminal socket: %w", err)}
	}
	// the proxy's frames are whole reads of the port, up to 128 KiB each
	ws.SetReadLimit(1 << 20)
	c := &console{d: d, r: r, pid: pid, ws: ws, done: make(chan struct{}), skip: consoleChatter, look: consoleLook}
	if d.consoleLook > 0 {
		c.look = d.consoleLook
	}
	c.life, c.stop = context.WithCancel(context.Background())
	if err := ws.Write(dctx, websocket.MessageBinary, []byte(t.User+":"+t.Ticket+"\n")); err != nil {
		c.end(&unreachable{err})
		return nil, &unreachable{err}
	}
	// its answer: OK, then the port; anything else and the proxy lets go
	_, first, err := ws.Read(dctx)
	if err != nil || !strings.HasPrefix(string(first), "OK") {
		c.end(io.EOF)
		return nil, fmt.Errorf("%w: its terminal proxy did not take the ticket", driver.ErrRefused)
	}
	c.pending = c.heard(first[2:])
	if size.Cols > 0 && size.Rows > 0 {
		_ = c.Resize(size)
	}
	go c.watch()
	return c, nil
}

// c2ws is the API's address as a socket's: https → wss.
func c2ws(base string) string {
	if rest, ok := strings.CutPrefix(base, "https://"); ok {
		return "wss://" + rest
	}
	return "ws://" + strings.TrimPrefix(base, "http://")
}

type console struct {
	d    *Driver
	r    resource
	pid  int // the guest's process when this was opened; 0: the engine did not say
	ws   *websocket.Conn
	look time.Duration // how often it looks at its guest

	life context.Context // ends with the console
	stop context.CancelFunc

	rmu     sync.Mutex // one Read at a time
	pending []byte
	skip    string // what is left of the proxy's own first line to leave out

	wmu sync.Mutex // one frame at a time

	once sync.Once
	done chan struct{}
	why  error // why Read ends, once it does
}

// heard takes the proxy's own first line out of what the port says: it
// comes first, in one frame or several. Called by one reader at a time.
func (c *console) heard(p []byte) []byte {
	for len(p) > 0 && c.skip != "" {
		n := min(len(p), len(c.skip))
		if string(p[:n]) != c.skip[:n] {
			c.skip = "" // not that line after all: everything is the guest's
			break
		}
		p, c.skip = p[n:], c.skip[n:]
	}
	return p
}

func (c *console) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for len(c.pending) == 0 {
		_, msg, err := c.ws.Read(c.life)
		if err != nil {
			c.end(io.EOF) // the engine let go, unless something said why first
			return 0, c.why
		}
		c.pending = c.heard(msg)
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// consoleChunk is the most typed bytes one frame carries: a paste goes in
// several, each far under what the proxy takes in one.
const consoleChunk = 4096

func (c *console) Write(p []byte) (int, error) {
	sent := 0
	for len(p) > 0 {
		n := min(len(p), consoleChunk)
		if err := c.frame(append([]byte("0:"+strconv.Itoa(n)+":"), p[:n]...)); err != nil {
			return sent, err
		}
		sent += n
		p = p[n:]
	}
	return sent, nil
}

func (c *console) Resize(size driver.ConsoleSize) error {
	if size.Cols < 1 || size.Rows < 1 {
		return nil
	}
	return c.frame([]byte(fmt.Sprintf("1:%d:%d:", size.Cols, size.Rows)))
}

// frame sends one frame — whole: the proxy takes no fragment.
func (c *console) frame(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	ctx, cancel := context.WithTimeout(c.life, 10*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, b); err != nil {
		c.end(io.EOF)
		return io.ErrClosedPipe
	}
	return nil
}

// watch keeps the proxy told the other end is there, and ends the console
// once its guest no longer runs: Proxmox does not.
func (c *console) watch() {
	look := time.NewTicker(c.look)
	ping := time.NewTicker(consolePing)
	defer look.Stop()
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ping.C:
			_ = c.frame([]byte("2"))
		case <-look.C:
			ctx, cancel := context.WithTimeout(c.life, 20*time.Second)
			on, pid, err := c.d.process(ctx, c.r)
			cancel()
			var ae *apiError
			switch {
			case err == nil && !on:
				c.end(driver.ErrConsoleStopped)
			case err == nil && c.pid != 0 && pid != 0 && pid != c.pid:
				c.end(driver.ErrConsoleRestarted)
			case errors.As(err, &ae) && (ae.Status == http.StatusNotFound || strings.Contains(ae.Message, "does not exist")):
				c.end(driver.ErrConsoleStopped) // deleted under it
			}
			// an API that did not answer says nothing of the guest: the socket
			// itself ends if the node is gone
		}
	}
}

// end lets go of the socket, once, for the first reason given.
func (c *console) end(why error) {
	c.once.Do(func() {
		c.why = why
		close(c.done)
		c.stop()
		_ = c.ws.CloseNow()
	})
}

func (c *console) Close() error {
	c.end(io.EOF)
	return nil
}

var _ driver.Consoles = (*Driver)(nil)
