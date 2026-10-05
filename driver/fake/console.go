package fake

import (
	"context"
	"fmt"
	"io"
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
// password. The shell knows whoami, hostname, stty size, echo, exit.

// consoleUser is who a signed-in console is: the fake's one account.
const consoleUser = "user"

type console struct {
	e    *Engine
	id   string
	host string
	auto bool // born signed in
	boot int  // which of its guest's boots it was opened on

	mu     sync.Mutex
	wake   *sync.Cond
	out    []byte
	err    error // why Read ends, once it does
	line   []byte
	mode   int
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
	c := &console{e: e, id: id, host: g.Name, auto: e.state.Specs[id].SignedIn, size: size, boot: e.state.Boots[id], closed: make(chan struct{})}
	c.wake = sync.NewCond(&c.mu)
	c.say("\r\n" + c.host + " ttyS0\r\n\r\n")
	c.greet()
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
// console is handed out.
func (c *console) say(s string) {
	c.out = append(c.out, s...)
	c.wake.Broadcast()
}

func (c *console) prompt() { c.say(consoleUser + "@" + c.host + ":~$ ") }

// greet is the getty's start: a shell at once for a guest born signed in, a
// name asked otherwise.
func (c *console) greet() {
	c.line = c.line[:0]
	if c.auto {
		c.mode = atShell
		c.say(c.host + " login: " + consoleUser + " (automatic login)\r\n\r\n")
		c.prompt()
		return
	}
	c.mode = atLogin
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
	for _, b := range p {
		switch {
		case b == '\r' || b == '\n':
			c.enter()
		case b == 0x7f || b == 0x08:
			if len(c.line) > 0 {
				c.line = c.line[:len(c.line)-1]
				if c.mode != atPassword {
					c.say("\b \b")
				}
			}
		case b == 0x03:
			c.line = c.line[:0]
			c.say("^C\r\n")
			if c.mode == atShell {
				c.prompt()
			} else {
				c.greet()
			}
		case b == 0x04 && len(c.line) == 0 && c.mode == atShell:
			c.say("logout\r\n\r\n")
			c.greet()
		case b >= 0x20:
			c.line = append(c.line, b)
			if c.mode != atPassword {
				c.say(string(b))
			}
		}
	}
	return len(p), nil
}

// enter is a line ended. Called with c.mu held.
func (c *console) enter() {
	line := strings.TrimSpace(string(c.line))
	c.line = c.line[:0]
	c.say("\r\n")
	switch c.mode {
	case atLogin:
		if line == "" {
			c.greet()
			return
		}
		c.mode = atPassword
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
			c.say(fmt.Sprintf("%d %d\r\n", c.size.Rows, c.size.Cols))
		case cmd == "echo":
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
