package fake

import (
	"context"
	"errors"
	"strings"
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
	_ = c.Resize(driver.ConsoleSize{Cols: 80, Rows: 24})
	_, _ = c.Write([]byte("stty size\rexit\r"))
	if got := hear(t, c, "(automatic login)"); !strings.Contains(got, "24 80") || !strings.Contains(got, "logout") {
		t.Fatalf("a window that changed, then exit: signed in again: %q", got)
	}

	asks, err := e.Console(ctx, "m-asks", driver.ConsoleSize{})
	if err != nil {
		t.Fatal(err)
	}
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
