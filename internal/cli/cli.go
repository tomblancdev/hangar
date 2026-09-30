// Package cli is the command line: a door on the brain's API, like the
// console, generated from what the brain serves. Every type the enabled
// plugins declare is a command (hangar machine create --cores 2 …), its
// flags drawn from the type's JSON Schema, its actions verbs — a new plugin
// appears here without a line of this package changing. `hangar login`
// signs a person in by the device flow (RFC 8628) against the operator's
// identity provider; `hangar apply` makes a spec file true.
//
// The brain checks everything: the command line only turns words into
// JSON, and prints the brain's refusals as they come, field by field, with
// the numbers.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"
)

// Env is what a run of the command line reads and writes — the process's
// own, or a test's.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	// Home keeps whom the command line signed in to which brain.
	Home string
	HTTP *http.Client
	// Terminal: a person at stdin — apply asks before it deletes.
	Terminal bool
	Now      func() time.Time
	Sleep    func(time.Duration)
	// Poll: how often a wait asks again when the brain answers at once
	// (a test's brain waits for nothing).
	Poll time.Duration
}

// DefaultEnv is the process's own.
func DefaultEnv() *Env {
	home := os.Getenv("HANGAR_HOME")
	if home == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			base = "."
		}
		home = filepath.Join(base, "hangar")
	}
	return &Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, Home: home,
		// a terminal, not any character device: /dev/null is one too (a
		// script's, a timer's stdin), and nobody answers there
		HTTP: &http.Client{Timeout: 90 * time.Second}, Terminal: term.IsTerminal(int(os.Stdin.Fd())), Now: time.Now, Sleep: time.Sleep,
		Poll: 2 * time.Second,
	}
}

// Words are the command line's own commands; a type of the same name is
// reached as `hangar type <name> …`.
var Words = []string{"login", "logout", "whoami", "types", "zones", "limits", "operations", "wait", "apply", "type"}

// Usage is the command line's part of `hangar help`.
const Usage = `
The command line (a door on a brain's API):
  hangar login [URL] [--with-token]       sign in: the device flow at the brain's identity
                                          provider, or an API token read from stdin
  hangar logout                           revoke the sign-in and forget it
  hangar whoami | types | zones | limits  who you are; what may be asked for, where, how much
  hangar <type> create|list|get|delete|<action> …
                                          every type the brain offers, its flags drawn from
                                          its schema: hangar <type> --help
  hangar apply FILE [--plan] [--yes]      make a spec file true: create what is missing,
                                          change what differs, delete what left the file
  hangar operations [--resource ID] | wait OP
The brain: $HANGAR_URL, else the last one signed in to. $HANGAR_TOKEN (hgr_…) signs in
without a provider. $HANGAR_HOME keeps the sign-ins (default ~/.config/hangar).
`

// errUsage is a command asked wrongly: exit 2.
type errUsage struct{ msg string }

func (e *errUsage) Error() string { return e.msg }

func usagef(format string, a ...any) error { return &errUsage{fmt.Sprintf(format, a...)} }

// Main runs the command line and returns its exit code.
func Main(args []string, env *Env) int {
	err := run(args, env)
	if err == nil {
		return 0
	}
	var u *errUsage
	if errors.As(err, &u) {
		fmt.Fprintln(env.Stderr, "hangar:", u.msg)
		return 2
	}
	fmt.Fprintln(env.Stderr, "hangar:", err)
	var p *Problem
	if errors.As(err, &p) {
		for _, line := range p.lines() {
			fmt.Fprintln(env.Stderr, "  "+line)
		}
	}
	return 1
}

func run(args []string, env *Env) error {
	if len(args) == 0 {
		return usagef("a command: hangar help")
	}
	switch args[0] {
	case "login":
		return login(env, args[1:])
	case "logout":
		return logout(env, args[1:])
	case "whoami":
		return whoami(env, args[1:])
	case "types":
		return types(env, args[1:])
	case "zones":
		return zones(env, args[1:])
	case "limits":
		return limitsCmd(env, args[1:])
	case "operations":
		return operations(env, args[1:])
	case "wait":
		return waitCmd(env, args[1:])
	case "apply":
		return apply(env, args[1:])
	case "type":
		if len(args) < 2 {
			return usagef("type NAME …: a type whose name is one of the command line's own words")
		}
		return typeCmd(env, args[1], args[2:])
	}
	return typeCmd(env, args[0], args[1:])
}

// ---- Flags: parsed by hand, since most are drawn from schemas -------------

// opt is a flag.
type opt struct {
	names []string // without dashes; the first is the one shown
	value bool     // takes a value
	set   func(string) error
	help  string
	arg   string // the value's word in help
}

// parse reads flags anywhere among the arguments ("--x v", "--x=v", "-x v");
// "--" ends them. A flag's name reads alike with - or _.
func parse(args []string, opts []*opt) ([]string, error) {
	find := func(name string) *opt {
		name = strings.ReplaceAll(name, "_", "-")
		for _, o := range opts {
			for _, n := range o.names {
				if strings.ReplaceAll(n, "_", "-") == name {
					return o
				}
			}
		}
		return nil
	}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(pos, args[i+1:]...), nil
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		o := find(name)
		if o == nil {
			if name == "h" || name == "help" {
				return nil, errHelp
			}
			return nil, usagef("no flag --%s here (--help lists them)", name)
		}
		switch {
		case o.value && !hasVal:
			if i+1 >= len(args) {
				return nil, usagef("--%s takes a value", name)
			}
			i++
			val = args[i]
		case !o.value && !hasVal:
			val = "true"
		}
		if err := o.set(val); err != nil {
			return nil, usagef("--%s: %v", name, err)
		}
	}
	return pos, nil
}

var errHelp = errors.New("help")

func flagHelp(w io.Writer, opts []*opt) {
	for _, o := range opts {
		name := "--" + strings.ReplaceAll(o.names[0], "_", "-")
		if len(o.names[0]) == 1 {
			name = "-" + o.names[0]
		}
		for _, n := range o.names[1:] {
			if len(n) == 1 {
				name += ", -" + n
			}
		}
		if o.value {
			arg := o.arg
			if arg == "" {
				arg = "VALUE"
			}
			name += " " + arg
		}
		fmt.Fprintf(w, "  %-28s %s\n", name, o.help)
	}
}

func boolOpt(p *bool, help string, names ...string) *opt {
	return &opt{names: names, help: help, set: func(v string) error {
		switch strings.ToLower(v) {
		case "true", "1", "yes":
			*p = true
		case "false", "0", "no":
			*p = false
		default:
			return fmt.Errorf("true or false, not %q", v)
		}
		return nil
	}}
}

func strOpt(p *string, arg, help string, names ...string) *opt {
	return &opt{names: names, value: true, arg: arg, help: help, set: func(v string) error { *p = v; return nil }}
}

func listOpt(p *[]string, arg, help string, names ...string) *opt {
	return &opt{names: names, value: true, arg: arg, help: help, set: func(v string) error { *p = append(*p, v); return nil }}
}

// confirm asks a person a yes or no; anything but yes is no.
func confirm(env *Env, question string) bool {
	fmt.Fprintf(env.Stderr, "%s [y/N] ", question)
	line, _ := bufio.NewReader(env.Stdin).ReadString('\n')
	return slices.Contains([]string{"y", "yes"}, strings.ToLower(strings.TrimSpace(line)))
}
