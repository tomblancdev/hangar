package proxmox

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/internal/ids"
)

// screen is a terminal at the other end of a console, as the page's is: it
// keeps what the guest says, and answers the one question a terminal is
// asked — its size.
type screen struct {
	mu   sync.Mutex
	said strings.Builder
	end  error
}

func watchConsole(c driver.Console, rows, cols string) *screen {
	s := &screen{}
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := c.Read(buf)
			s.mu.Lock()
			s.said.Write(buf[:n])
			if err != nil {
				s.end = err
			}
			s.mu.Unlock()
			if err != nil {
				return
			}
			if rows != "" && strings.Contains(string(buf[:n]), "\x1b[18t") {
				_, _ = c.Write([]byte("\x1b[8;" + rows + ";" + cols + "t"))
			}
		}
	}()
	return s
}

func (s *screen) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.said.String()
}

func (s *screen) ended() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.end
}

// until waits for the guest to say something that matches, and returns
// everything it said.
func (s *screen) until(t *testing.T, what, pattern string, within time.Duration) string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if re.MatchString(s.text()) {
			return s.text()
		}
	}
	tail := s.text()
	if len(tail) > 1500 {
		tail = tail[len(tail)-1500:]
	}
	t.Fatalf("%s: the console never said /%s/ in %s; its last words:\n%q", what, pattern, within, tail)
	return ""
}

// A VM's console on a real Proxmox VE, opened by the plugin's own token —
// fenced to its pool, VM.Console in its role. A VM born signed in, with no
// key: its owner's own user data runs untouched beside the step, and the
// port signs its user in once a terminal is there — at that terminal's
// size, with its colours, and nothing asked. A VM that is not born so asks
// a login. A guest stopped under its console ends it, with the reason —
// Proxmox itself says nothing. And nothing is left running on the node.
func TestBenchAVMsConsole(t *testing.T) {
	b := onBench(t)
	d := b.open(t, b.token)
	d.consoleLook = 2 * time.Second
	ctx := context.Background()
	if !has(d.Capabilities(), driver.GuestConsole) || !d.Traits("vm").Console {
		t.Fatalf("the machines token opens no console: is VM.Console in its pool's role? %v", d.Capabilities())
	}
	// the control: a token of the same pool whose role has none
	if tok := os.Getenv("HANGAR_BENCH_VOLUMES_TOKEN_FILE"); tok != "" {
		if v := b.open(t, tok); has(v.Capabilities(), driver.GuestConsole) {
			t.Fatal("a token without VM.Console offers a console")
		}
	}

	open, asks := ids.New("m"), ids.New("m")
	for _, id := range []string{open, asks} {
		t.Cleanup(func() {
			if err := d.DeleteGuest(context.Background(), id); err != nil {
				t.Errorf("cleaning up %s: %v", id, err)
			}
		})
	}
	// its owner's own first boot: a cloud-config, as they wrote it
	own := "#cloud-config\nruncmd:\n  - [sh, -c, 'echo the-owners-own > /var/tmp/mine']\n"
	began := time.Now()
	if _, err := d.CreateGuest(ctx, driver.GuestSpec{ID: open, Kind: "vm", Name: "box", Cores: 1, MemoryMB: 1024, Image: "debian-13",
		UserData: []byte(own), SignedIn: true, Tags: map[string]string{"class": "guaranteed"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateGuest(ctx, driver.GuestSpec{ID: asks, Kind: "vm", Name: "keyed", Cores: 1, MemoryMB: 1024, Image: "debian-13",
		SSHKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGXj2Dq6dbWAg2wXCN9pDWc3cS/cJEHWIr0sRd4b3M5V someone@example.com"},
		Tags:    map[string]string{"class": "guaranteed"}}); err != nil {
		t.Fatal(err)
	}

	// ---- born signed in: a terminal of 132 by 41 at the other end
	c, err := d.Console(ctx, open, driver.ConsoleSize{Cols: 132, Rows: 41})
	if err != nil {
		t.Fatal(err)
	}
	s := watchConsole(c, "41", "132")
	s.until(t, "signed in, nothing asked", `debian \(automatic login\)[\s\S]*\$ $`, 6*time.Minute)
	t.Logf("a shell on its console %s after its create", time.Since(began).Round(time.Second))
	if strings.Contains(s.text(), "starting serial terminal") {
		t.Error("the proxy's own line is in what the guest said")
	}
	ask := func(cmd, pattern string) {
		t.Helper()
		if _, err := c.Write([]byte(cmd + "\r")); err != nil {
			t.Fatal(err)
		}
		s.until(t, cmd, pattern, 30*time.Second)
	}
	// (an answer follows its command's echo and the shell's own « paste
	// mode off », which ends in a bare return)
	ask("whoami", `[\r\n]debian\r\n`)
	ask("sudo -n id -u", `[\r\n]0\r\n`)
	ask("stty size", `[\r\n]41 132\r\n`)
	ask("echo $TERM", `[\r\n]xterm-256color\r\n`)
	// its owner's own user data ran, beside the step
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(5 * time.Second) {
		_, _ = c.Write([]byte("cat /var/tmp/mine\r"))
		time.Sleep(time.Second)
		if regexp.MustCompile(`[\r\n]the-owners-own\r\n`).MatchString(s.text()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the owner's own user data did not run beside the step")
		}
	}
	// exit: signed in again, at the size the terminal has now
	_ = c.Close()
	if c, err = d.Console(ctx, open, driver.ConsoleSize{Cols: 100, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	s = watchConsole(c, "30", "100")
	if _, err := c.Write([]byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	s.until(t, "signed in again after exit", `logout[\s\S]*automatic login[\s\S]*\$ $`, time.Minute)
	if _, err := c.Write([]byte("stty size\r")); err != nil {
		t.Fatal(err)
	}
	s.until(t, "the size taken at the new sign-in", `[\r\n]30 100\r\n`, 30*time.Second)

	// the step is this machine's alone. Its mark changed — as on a machine
	// born from an image of this one, which carries its files and did not ask
	// for them — the port asks a login, and signs nobody in
	before := len(s.text())
	if _, err := c.Write([]byte("sudo sh -c 'echo m-another > /etc/hangar-terminal.instance'; exit\r")); err != nil {
		t.Fatal(err)
	}
	s.until(t, "a login asked where the step was written for another machine", `logout[\s\S]*login: $`, time.Minute)
	if strings.Contains(s.text()[before:], "automatic login") {
		t.Fatalf("a machine carrying another's step signed someone in: %q", s.text()[before:])
	}

	// ---- rebooted under its console: Proxmox ends its process and starts
	// another, and says nothing on the socket — the driver sees it runs anew,
	// and the console is opened again on the new boot. (The step is written
	// again at that boot, for this machine: its port is its own once more.)
	rebooted := time.Now()
	if _, err := d.Reboot(ctx, open); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); s.ended() == nil; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the guest was rebooted, and its console stayed open on the process before")
		}
	}
	// caught down on its way, or seen running anew: either way this console
	// is over, and the one who holds it knows a reboot was asked
	if !errors.Is(s.ended(), driver.ErrConsoleRestarted) && !errors.Is(s.ended(), driver.ErrConsoleStopped) {
		t.Fatalf("a guest rebooted under its console: %v", s.ended())
	}
	t.Logf("its console ended as: %v", s.ended())
	if c, err = d.Console(ctx, open, driver.ConsoleSize{Cols: 132, Rows: 41}); err != nil {
		t.Fatal(err)
	}
	s = watchConsole(c, "41", "132")
	s.until(t, "signed in again on the new boot", `automatic login[\s\S]*\$ $`, 4*time.Minute)
	t.Logf("a shell on its console again %s after its reboot was asked", time.Since(rebooted).Round(time.Second))

	// ---- not born so: a login asked, and nobody signed in
	k, err := d.Console(ctx, asks, driver.ConsoleSize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	ks := watchConsole(k, "24", "80")
	for deadline := time.Now().Add(4 * time.Minute); !strings.Contains(ks.text(), "login: "); time.Sleep(3 * time.Second) {
		_, _ = k.Write([]byte("\r"))
		if time.Now().After(deadline) {
			t.Fatalf("a VM that is not born signed in never asked a login: %q", ks.text())
		}
	}
	if strings.Contains(ks.text(), "automatic login") {
		t.Fatal("a VM that was not born signed in signed someone in")
	}
	_ = k.Close()

	// ---- stopped under its console: ended, with the reason
	if _, err := d.SetPower(ctx, open, false); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); s.ended() == nil; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the guest stopped, and its console stayed open")
		}
	}
	if !errors.Is(s.ended(), driver.ErrConsoleStopped) {
		t.Fatalf("a guest stopped under its console: %v", s.ended())
	}
	if _, err := d.Console(ctx, open, driver.ConsoleSize{}); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("a stopped guest's console: %v", err)
	}
	// nothing of a console is left running on the node
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(time.Second) {
		left, _ := b.ssh(t, "pgrep -c termproxy")
		if left == "0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s terminal proxies still running on the node", left)
		}
	}
}
