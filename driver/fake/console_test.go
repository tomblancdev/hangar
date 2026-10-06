package fake

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
)

// hear reads a console until it has said want, and returns all of it.
func hear(t *testing.T, c driver.Console, want string) string {
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
		if !strings.Contains(s, want) {
			t.Fatalf("the console ended before saying %q: %q", want, s)
		}
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("the console never said %q", want)
		return ""
	}
}

// ear listens to a console for as long as it is open, and keeps what it said:
// what a test needs to hear that a port says nothing.
type ear struct {
	mu   sync.Mutex
	said strings.Builder
}

func listen(c driver.Console) *ear {
	e := &ear{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := c.Read(buf)
			e.mu.Lock()
			e.said.Write(buf[:n])
			e.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return e
}

func (e *ear) heard() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.said.String()
}

// until waits for the console to have said want, and returns all it said.
func (e *ear) until(t *testing.T, want string) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if strings.Contains(e.heard(), want) {
			return e.heard()
		}
	}
	t.Fatalf("the console never said %q; it said %q", want, e.heard())
	return ""
}

// The port's other end is the guest's: a shell left there is there when the
// port is opened again, and says nothing by itself — neither a greeting nor a
// prompt, until something is typed. What was typed and not entered is still
// on its line; Ctrl-L draws it again. A guest that asks a login asked it at
// boot, of nobody: its port is silent too, until Enter. A new boot is a new
// port, and its getty greets whoever comes.
func TestAPortOutlivesItsConsole(t *testing.T) {
	ctx := context.Background()
	d, _ := Open(ctx, driver.Params{Zone: "z"})
	e := d.(*Engine)
	for id, s := range map[string]driver.GuestSpec{
		"m-open": {Kind: "vm", Name: "box", SignedIn: true},
		"m-asks": {Kind: "vm", Name: "keyed"},
	} {
		s.ID, s.Cores, s.MemoryMB = id, 1, 512
		if _, err := e.CreateGuest(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	open := func(id string) (driver.Console, *ear) {
		t.Helper()
		c, err := e.Console(ctx, id, driver.ConsoleSize{Cols: 80, Rows: 24})
		if err != nil {
			t.Fatal(err)
		}
		return c, listen(c)
	}
	// silent: nothing more is said for a while (the fake answers in
	// microseconds: a tenth of a second is a long silence here)
	silent := func(what string, l *ear, since string) {
		t.Helper()
		time.Sleep(100 * time.Millisecond)
		if now := l.heard(); now != since {
			t.Fatalf("%s: the port spoke by itself: %q", what, strings.TrimPrefix(now, since))
		}
	}

	// the first terminal of a boot: the getty was waiting for one
	c, l := open("m-open")
	l.until(t, "(automatic login)")
	l.until(t, "user@box:~$ ")
	_, _ = c.Write([]byte("echo ha"))
	l.until(t, "echo ha")
	_ = c.Close()

	// opened again: not a byte, and the shell is where it was left
	c, l = open("m-open")
	silent("a port opened again", l, "")
	// Ctrl-L: the screen cleared, the prompt and the line as it was left
	_, _ = c.Write([]byte{0x0c})
	if got := l.until(t, "user@box:~$ echo ha"); !strings.HasPrefix(got, "\x1b[H\x1b[2J") {
		t.Fatalf("Ctrl-L draws the screen again: %q", got)
	}
	_, _ = c.Write([]byte("lf\r"))
	l.until(t, "\r\nhalf\r\nuser@box:~$ ")
	_ = c.Close()
	// and again, at an empty prompt: Enter is answered by a prompt alone
	c, l = open("m-open")
	silent("a port opened a third time", l, "")
	_, _ = c.Write([]byte("\r"))
	if got := l.until(t, "user@box:~$ "); strings.Contains(got, "login") {
		t.Fatalf("Enter at a shell left open signed someone in again: %q", got)
	}
	// exit: the getty again — and a terminal is there, so it signs in
	_, _ = c.Write([]byte("exit\r"))
	l.until(t, "logout")
	l.until(t, "(automatic login)\r\n\r\nuser@box:~$ ")
	_ = c.Close()

	// a new boot: another port, a getty that greets whoever comes
	if _, err := e.Reboot(ctx, "m-open"); err != nil {
		t.Fatal(err)
	}
	c, l = open("m-open")
	l.until(t, "(automatic login)")
	_ = c.Close()

	// a login asked at boot was asked of nobody: silent, until Enter
	k, kl := open("m-asks")
	silent("a port that asks a login", kl, "")
	_, _ = k.Write([]byte{0x0c}) // a getty takes no notice of Ctrl-L
	silent("Ctrl-L at a login", kl, "")
	_, _ = k.Write([]byte("\r"))
	if got := kl.until(t, "keyed login: "); strings.Contains(got, "automatic") {
		t.Fatalf("a console that asks: %q", got)
	}
	_, _ = k.Write([]byte("roo"))
	kl.until(t, "roo")
	_ = k.Close()
	// opened again in the middle of a name: it is still being typed
	k, kl = open("m-asks")
	silent("a login half typed, opened again", kl, "")
	_, _ = k.Write([]byte("t\r"))
	kl.until(t, "Password: ")
	_ = k.Close()
}

// The fake's console: a guest born signed in greets with a shell, another
// asks a name; a container has none, and a stopped guest opens none.
func TestAConsole(t *testing.T) {
	ctx := context.Background()
	d, _ := Open(ctx, driver.Params{Zone: "z"})
	e := d.(*Engine)
	if !e.Traits("vm").Console || e.Traits("container").Console {
		t.Fatalf("a VM has a console, a container none: %+v %+v", e.Traits("vm"), e.Traits("container"))
	}
	for id, s := range map[string]driver.GuestSpec{
		"m-open":  {Kind: "vm", Name: "box", SignedIn: true},
		"m-asks":  {Kind: "vm", Name: "keyed"},
		"m-ct":    {Kind: "container", Name: "ct"},
		"m-still": {Kind: "vm", Name: "still", Stopped: true},
	} {
		s.ID, s.Cores, s.MemoryMB = id, 1, 512
		if _, err := e.CreateGuest(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	c, err := e.Console(ctx, "m-open", driver.ConsoleSize{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	if got := hear(t, c, "user@box:~$ "); !strings.Contains(got, "(automatic login)") {
		t.Fatalf("a console born signed in: %q", got)
	}
	_, _ = c.Write([]byte("whoami\rstty size\r"))
	if got := hear(t, c, "30 100\r\nuser@box:~$ "); !strings.Contains(got, "whoami\r\nuser\r\n") {
		t.Fatalf("what is typed comes back, and a command answers: %q", got)
	}
	// a window that changed: the shell still has the size it was signed in
	// at — a port carries none — until exit signs in again, at the new one
	_ = c.Resize(driver.ConsoleSize{Cols: 80, Rows: 24})
	_, _ = c.Write([]byte("stty size\rexit\r"))
	if got := hear(t, c, "(automatic login)"); !strings.Contains(got, "30 100") || strings.Contains(got, "24 80") || !strings.Contains(got, "logout") {
		t.Fatalf("a window that changed tells the shell nothing; then exit: %q", got)
	}
	_, _ = c.Write([]byte("stty size\r"))
	if got := hear(t, c, "24 80\r\nuser@box:~$ "); strings.Contains(got, "30 100") {
		t.Fatalf("signed in again, at the window's size by then: %q", got)
	}
	// echo -e: how a test puts a terminal in a mode
	_, _ = c.Write([]byte(`echo -e \e[?1004h` + "\r"))
	hear(t, c, "\x1b[?1004h\r\nuser@box:~$ ")

	asks, err := e.Console(ctx, "m-asks", driver.ConsoleSize{})
	if err != nil {
		t.Fatal(err)
	}
	// (its login was asked at boot, of nobody: Enter asks again)
	_, _ = asks.Write([]byte("\r"))
	if got := hear(t, asks, "keyed login: "); strings.Contains(got, "automatic") {
		t.Fatalf("a console that asks: %q", got)
	}
	_, _ = asks.Write([]byte("root\r"))
	hear(t, asks, "Password: ")
	_, _ = asks.Write([]byte("guess\r"))
	if got := hear(t, asks, "keyed login: "); !strings.Contains(got, "Login incorrect") || strings.Contains(got, "guess") {
		t.Fatalf("nobody gets in, and a password is not shown: %q", got)
	}
	_ = asks.Close()

	if _, err := e.Console(ctx, "m-ct", driver.ConsoleSize{}); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("a container's console: %v", err)
	}
	if _, err := e.Console(ctx, "m-still", driver.ConsoleSize{}); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("a stopped guest's console: %v", err)
	}
	if _, err := e.Console(ctx, "m-none", driver.ConsoleSize{}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("no such guest's console: %v", err)
	}

	// ends reads a console to its end, and says how it ended
	ends := func(c driver.Console) error {
		t.Helper()
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
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("its console stayed open")
			return nil
		}
	}
	// the guest rebooted under it: the port's other end is another boot, and
	// this console is to be opened again
	if _, err := e.Reboot(ctx, "m-open"); err != nil {
		t.Fatal(err)
	}
	if err := ends(c); !errors.Is(err, driver.ErrConsoleRestarted) {
		t.Fatalf("a guest rebooted under its console: %v", err)
	}
	if c, err = e.Console(ctx, "m-open", driver.ConsoleSize{}); err != nil {
		t.Fatal(err)
	}
	hear(t, c, "user@box:~$ ")
	// the guest stops under it: the console ends, and says why
	if _, err := e.SetPower(ctx, "m-open", false); err != nil {
		t.Fatal(err)
	}
	if err := ends(c); !errors.Is(err, driver.ErrConsoleStopped) {
		t.Fatalf("a guest stopped under its console: %v", err)
	}

	// a zone that opens none
	q, _ := Open(ctx, driver.Params{Zone: "q", Options: map[string]string{"capabilities": "kind.vm,guest.tags"}})
	qe := q.(*Engine)
	if _, err := qe.CreateGuest(ctx, driver.GuestSpec{ID: "m-q", Kind: "vm", Cores: 1, MemoryMB: 512}); err != nil {
		t.Fatal(err)
	}
	if _, err := qe.Console(ctx, "m-q", driver.ConsoleSize{}); !errors.Is(err, driver.ErrRefused) || qe.Traits("vm").Console {
		t.Fatalf("a console in a zone that opens none: %v", err)
	}
}
