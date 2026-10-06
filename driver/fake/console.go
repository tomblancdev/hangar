package fake

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tomblancdev/hangar/driver"
)

// A guest's console on the fake engine (guest.console): a serial port with a
// getty behind it and a toy of a shell, enough to prove every layer above —
// what is typed comes back, a command answers, the guest's stop ends it, and
// so does its start anew (a reboot: the port's other end is another boot).
//
// A guest born signed in (GuestSpec.SignedIn) greets with a shell as "user";
// another asks a name and a password, and lets nobody in: the fake holds no
// password. The shell knows whoami, hostname, stty size, echo (and echo -e,
// whose \e is an escape: how a test puts a terminal in a mode), exit — and
// Ctrl-L, at which it draws its prompt again, and what was typed on it.
//
// The port's other end is the guest's, not the console's, as on the engines
// this stands in for: what sits there stays there when a console closes. A
// shell left at its prompt is at its prompt when the port is opened again —
// what was typed and not entered still on its line — and says nothing by
// itself. Only a getty that signs in speaks at an opening: it waits for a
// terminal, and one has come. One that asks a login asked it at boot, of
// nobody; it asks again at Enter.
//
// And a port carries no window size: a shell has the size its terminal had
// when it was signed in — a window that changes afterwards changes nothing
// there, until exit signs in again. The getty that signs in asks the
// terminal (« report your text area's size », CSI 18 t), as the real one
// does, and takes what a terminal answers; the fake does not wait for the
// answer — a test's hand client gives none — and starts from the size the
// console was opened with.

// consoleUser is who a signed-in console is: the fake's one account.
const consoleUser = "user"

// sizeAnswer is a terminal's answer to the getty's question: its text area,
// rows then columns.
var sizeAnswer = regexp.MustCompile("\x1b\\[8;(\\d{1,4});(\\d{1,4})t")

// port is the other end of a guest's serial port, for one of its boots: who
// is there — a getty, a shell — and the line being typed. It outlives the
// consoles opened on it.
type port struct {
	mu   sync.Mutex
	boot int
	mode int
	line []byte
	size driver.ConsoleSize // the terminal's, when its shell was signed in
}

type console struct {
	e    *Engine
	id   string
	host string
	auto bool  // born signed in
	boot int   // which of its guest's boots it was opened on
	p    *port // that boot's port

	mu     sync.Mutex
	wake   *sync.Cond
	out    []byte
	err    error // why Read ends, once it does
	size   driver.ConsoleSize
	closed chan struct{}
}

const (
	atLogin = iota
	atPassword
	atShell
)

// Console opens a running guest's console. VMs have one; a container has
// none here, as on the engines this stands in for.
func (e *Engine) Console(_ context.Context, id string, size driver.ConsoleSize) (driver.Console, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return nil, err
	}
	g, ok := e.state.Guests[id]
	switch {
	case !ok:
		return nil, driver.ErrNotFound
	case !e.has(driver.GuestConsole) || !e.Traits(g.Kind).Console:
		return nil, fmt.Errorf("%w: a %s has no console here", driver.ErrRefused, g.Kind)
	case !g.Running:
		return nil, fmt.Errorf("%w: it does not run", driver.ErrRefused)
	}
	boot := e.state.Boots[id]
	p := e.ports[id]
	if p == nil || p.boot != boot {
		// a new boot: a getty at the port, as every boot leaves one
		p = &port{boot: boot, mode: atLogin}
		if e.ports == nil {
			e.ports = map[string]*port{}
		}
		e.ports[id] = p
	}
	c := &console{e: e, id: id, host: g.Name, auto: e.state.Specs[id].SignedIn, size: size, boot: boot, p: p, closed: make(chan struct{})}
	c.wake = sync.NewCond(&c.mu)
	// the getty that signs in waited for a terminal, and one has come; a
	// shell already there, or a login asked at boot, says nothing more
	p.mu.Lock()
	if c.auto && p.mode != atShell {
		c.greet()
	}
	p.mu.Unlock()
	go c.watch()
	return c, nil
}

// watch ends the console once its guest no longer runs: the engine is its
// file, and a guest stopped there — by the brain or by a hand — is stopped.
func (c *console) watch() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
		}
		c.e.mu.Lock()
		_ = c.e.load()
		g, ok := c.e.state.Guests[c.id]
		runs, boot := ok && g.Running, c.e.state.Boots[c.id]
		c.e.mu.Unlock()
		if !runs {
			c.end(driver.ErrConsoleStopped)
			return
		}
		if boot != c.boot {
			c.end(driver.ErrConsoleRestarted)
			return
		}
	}
}

func (c *console) end(why error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = why
	close(c.closed)
	c.wake.Broadcast()
}

// say queues what the port says. Called with c.mu held, or before the
// console is handed out. What follows it — prompt, greet, enter — reads and
// moves the port too: called with c.p.mu held as well.
func (c *console) say(s string) {
	c.out = append(c.out, s...)
	c.wake.Broadcast()
}

func (c *console) prompt() { c.say(consoleUser + "@" + c.host + ":~$ ") }

// greet is the getty's start: a shell at once for a guest born signed in, a
// name asked otherwise.
func (c *console) greet() {
	c.p.line = c.p.line[:0]
	c.say("\r\n" + c.host + " ttyS0\r\n\r\n")
	// the port's own size, unless a getty that signs in asked the terminal
	c.p.size = driver.ConsoleSize{Cols: 80, Rows: 24}
	if c.auto {
		c.p.mode = atShell
		if c.size.Cols > 0 && c.size.Rows > 0 {
			c.p.size = c.size
		}
		c.say("\x1b[18t")
		c.say(c.host + " login: " + consoleUser + " (automatic login)\r\n\r\n")
		c.prompt()
		return
	}
	c.p.mode = atLogin
	c.say(c.host + " login: ")
}

func (c *console) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.out) == 0 && c.err == nil {
		c.wake.Wait()
	}
	if len(c.out) == 0 {
		return 0, c.err
	}
	n := copy(p, c.out)
	c.out = c.out[n:]
	return n, nil
}

func (c *console) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, io.ErrClosedPipe
	}
	at := c.p
	at.mu.Lock()
	defer at.mu.Unlock()
	// a terminal's answer to the getty's question is the getty's, not the
	// shell's: taken, and not typed
	typed := p
	if m := sizeAnswer.FindSubmatchIndex(p); m != nil && at.mode == atShell {
		rows, _ := strconv.Atoi(string(p[m[2]:m[3]]))
		cols, _ := strconv.Atoi(string(p[m[4]:m[5]]))
		if rows > 0 && cols > 0 {
			at.size = driver.ConsoleSize{Cols: cols, Rows: rows}
		}
		typed = append(append([]byte{}, p[:m[0]]...), p[m[1]:]...)
	}
	for _, b := range typed {
		switch {
		case b == '\r' || b == '\n':
			c.enter()
		case b == 0x7f || b == 0x08:
			if len(at.line) > 0 {
				at.line = at.line[:len(at.line)-1]
				if at.mode != atPassword {
					c.say("\b \b")
				}
			}
		case b == 0x03:
			at.line = at.line[:0]
			c.say("^C\r\n")
			if at.mode == atShell {
				c.prompt()
			} else {
				c.greet()
			}
		case b == 0x04 && len(at.line) == 0 && at.mode == atShell:
			c.say("logout\r\n\r\n")
			c.greet()
		case b == 0x0c:
			// Ctrl-L: a shell clears the screen and draws its line again; a
			// getty takes no notice
			if at.mode == atShell {
				c.say("\x1b[H\x1b[2J")
				c.prompt()
				c.say(string(at.line))
			}
		case b >= 0x20:
			at.line = append(at.line, b)
			if at.mode != atPassword {
				c.say(string(b))
			}
		}
	}
	return len(p), nil
}

// enter is a line ended. Called with c.mu and c.p.mu held.
func (c *console) enter() {
	line := strings.TrimSpace(string(c.p.line))
	c.p.line = c.p.line[:0]
	c.say("\r\n")
	switch c.p.mode {
	case atLogin:
		if line == "" {
			c.greet()
			return
		}
		c.p.mode = atPassword
		c.say("Password: ")
	case atPassword:
		c.say("\r\nLogin incorrect\r\n")
		c.greet()
	case atShell:
		cmd, rest, _ := strings.Cut(line, " ")
		switch {
		case line == "":
		case line == "whoami":
			c.say(consoleUser + "\r\n")
		case line == "hostname":
			c.say(c.host + "\r\n")
		case line == "stty size":
			c.say(fmt.Sprintf("%d %d\r\n", c.p.size.Rows, c.p.size.Cols))
		case cmd == "echo":
			if seq, ok := strings.CutPrefix(rest, "-e "); ok {
				rest = strings.ReplaceAll(seq, `\e`, "\x1b")
			}
			c.say(rest + "\r\n")
		case line == "exit" || line == "logout":
			c.say("logout\r\n\r\n")
			c.greet()
			return
		default:
			c.say("sh: 1: " + cmd + ": not found\r\n")
		}
		c.prompt()
	}
}

func (c *console) Resize(size driver.ConsoleSize) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.size = size
	return nil
}

func (c *console) Close() error {
	c.end(io.EOF)
	return nil
}

var _ driver.Consoles = (*Engine)(nil)
