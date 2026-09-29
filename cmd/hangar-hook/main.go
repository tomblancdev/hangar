// hangar-hook — le hangar's hand on a Proxmox VE node: a guest's hookscript.
//
//	hangar-hook <vmid> <phase>                 as Proxmox VE calls it, as root
//	hangar-hook --config <file> <vmid> <phase> another file than /etc/hangar/hook.json
//	hangar-hook --help                         this text
//
// On a product machine (its tags carry hangar-id), before it starts: the
// node's own admission — a start above the size the brain admitted is
// refused, a spot machine's while a priority guest of the node has the room,
// a floor's above its floor then. Set it on the images' VM templates: every
// clone inherits it, and only root@pam can take it off.
//
// On a priority guest (the one a zone's reservation waits on), before it
// starts: the brain is asked to hold what the zone lends (claim), and when it
// cannot be reached the node does it alone from the machines' tags — never
// refusing the start. After the guest stops: release, or the node gives back
// alone. Set it on that guest, as root:
//
//	qm set <vmid> --hookscript local:snippets/hangar-hook
//
// /etc/hangar/hook.json (JSON):
//
//	{"brain": "https://hangar.example.com", "token_file": "/etc/hangar/hook.token",
//	 "zone": "lab", "timeout": "90s", "shutdown_timeout": "30s", "grace": "10m"}
//
// The token: hangar token create --subject hook-<node> --groups <a tier with
// room: true> --scopes room. It claims and releases, and nothing else.
//
// It logs to syslog as hangar-hook, and to the task's log.
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/syslog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tomblancdev/hangar/driver/proxmox"
)

const defaultConfig = "/etc/hangar/hook.json"

// its header is its help
//
//go:embed main.go
var source string

func usage() {
	for _, line := range strings.Split(strings.SplitN(source, "\npackage main", 2)[0], "\n") {
		fmt.Println(strings.TrimPrefix(strings.TrimPrefix(line, "//"), " "))
	}
}

func main() {
	args := os.Args[1:]
	cfgPath := defaultConfig
	if len(args) >= 2 && args[0] == "--config" {
		cfgPath, args = args[1], args[2:]
	}
	if len(args) != 2 || strings.HasPrefix(args[0], "-") {
		usage()
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
			os.Exit(0)
		}
		os.Exit(2)
	}
	vmid, phase := args[0], args[1]
	sys, _ := syslog.New(syslog.LOG_INFO|syslog.LOG_DAEMON, "hangar-hook")
	say := func(format string, a ...any) {
		msg := fmt.Sprintf("guest %s %s: ", vmid, phase) + fmt.Sprintf(format, a...)
		fmt.Fprintln(os.Stderr, "hangar-hook: "+msg)
		if sys != nil {
			_ = sys.Info(msg)
		}
	}
	// whatever goes wrong here, a start is refused only by an admission's
	// word: a hook that breaks never keeps a guest from starting
	defer func() {
		if r := recover(); r != nil {
			say("it broke (%v): nothing refused", r)
			os.Exit(0)
		}
	}()
	cfg, err := proxmox.ReadHookConfig(cfgPath)
	if err != nil {
		say("no config (%v): nothing done", err)
		return
	}
	host, _ := os.Hostname()
	node := &proxmox.LocalNode{Name: strings.SplitN(host, ".", 2)[0], Run: func(name string, a ...string) ([]byte, error) {
		out, err := exec.Command(name, a...).CombinedOutput()
		if err != nil {
			return out, fmt.Errorf("%s %s: %v: %s", name, strings.Join(a, " "), err, strings.TrimSpace(string(out)))
		}
		return out, nil
	}}
	h := &proxmox.Hook{Cfg: cfg, Node: node, Log: say}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := h.Run(ctx, vmid, phase); err != nil {
		if errors.Is(err, proxmox.ErrRefused) {
			msg := strings.TrimPrefix(err.Error(), proxmox.ErrRefused.Error()+": ")
			say("%s", msg)
			fmt.Fprintln(os.Stderr, msg)
			os.Exit(1)
		}
		say("%v", err)
	}
}
