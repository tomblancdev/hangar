// Le Hangar — a small cloud's control plane: people ask for machines,
// volumes and images (and, through plugins, anything else) and get them
// within their tier's limits, on the engines the operator already owns.
//
// One static binary. It is the brain (serve), its own checker (check), the
// hand that makes the first API token on the brain's host (token), —
// started by itself as separate processes — its built-in plugins (plugin),
// the web console on its own in front of a brain (console), and the command
// line people ask a brain with (login, apply, and every type the brain
// offers: internal/cli).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	_ "time/tzdata"

	"github.com/tomblancdev/hangar/internal/audit"
	"github.com/tomblancdev/hangar/internal/cli"
	"github.com/tomblancdev/hangar/internal/config"
	"github.com/tomblancdev/hangar/internal/core"
	"github.com/tomblancdev/hangar/internal/identity"
	"github.com/tomblancdev/hangar/internal/limits"
	"github.com/tomblancdev/hangar/internal/metrics"
	"github.com/tomblancdev/hangar/internal/plugins"
	"github.com/tomblancdev/hangar/internal/registry"
	"github.com/tomblancdev/hangar/internal/server"
	"github.com/tomblancdev/hangar/sdk"
)

// set by -ldflags "-X main.version=..."
var version = "dev"

const usage = `hangar — a small cloud's control plane.

On the brain's host:
  hangar serve   [--config FILE]          run the brain
  hangar check   [--config FILE]          start every plugin, check the config against
                                          what they declare — and that what it reaches over
                                          https could be verified here —, print it, stop
                                          (0 = sound)
  hangar token create --subject SUB --name NAME [--groups a,b] [--ttl 24h] [--scopes read,write|room]
                 [--config FILE]          make an API token in the registry; the secret
                                          is printed once, on stdout
  hangar token list   [--subject SUB] [--config FILE]
  hangar token revoke ID [--config FILE]
  hangar version

The config is --config, else $HANGAR_CONFIG, else /etc/hangar/hangar.yaml.
$HANGAR_LISTEN and $HANGAR_DATA_DIR override the file's listen and data_dir.

The brain serves the web console at /console/ (the config's console block). Or,
in front of a brain, a process of its own that holds none of the plugins' keys:
  hangar console --brain URL [--listen :8081] [--url URL] [--house WORD] [--idle 12h]
                                          each also $HANGAR_CONSOLE_BRAIN, _LISTEN, _URL,
                                          _HOUSE, _IDLE; it reads no config file
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage+cli.Usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "check":
		err = check(os.Args[2:], os.Stdout)
	case "token":
		err = token(os.Args[2:], os.Stdout)
	case "plugin":
		err = runPlugin(os.Args[2:])
	case "console":
		err = runConsole(os.Args[2:])
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage + cli.Usage)
	default:
		// the command line: its own words, and every type the brain offers
		os.Exit(cli.Main(os.Args[1:], cli.DefaultEnv()))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hangar:", err)
		os.Exit(1)
	}
}

func loadConfig(fs *flag.FlagSet, args []string) (*config.Config, error) {
	def := os.Getenv("HANGAR_CONFIG")
	if def == "" {
		def = "/etc/hangar/hangar.yaml"
	}
	path := fs.String("config", def, "the config file")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return nil, err
	}
	if v := os.Getenv("HANGAR_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("HANGAR_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	// The image has a read-only root and no /tmp: anything that needs a
	// temporary file (SQLite's sorts) gets one under the data directory.
	if os.Getenv("TMPDIR") == "" {
		tmp := filepath.Join(cfg.DataDir, "tmp")
		if err := os.MkdirAll(tmp, 0o700); err != nil {
			return nil, err
		}
		_ = os.Setenv("TMPDIR", tmp)
	}
	return cfg, nil
}

func openStore(cfg *config.Config) (*registry.Store, error) {
	return registry.Open(filepath.Join(cfg.DataDir, "hangar.db"))
}

// startPlugins starts the plugins and holds the tiers against what they
// declared.
func startPlugins(ctx context.Context, cfg *config.Config, log *slog.Logger, logs io.Writer) (*plugins.Host, error) {
	host, err := plugins.Start(ctx, cfg, plugins.Options{DataDir: cfg.DataDir, Logs: logs}, log)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, p := range host.Plugins() {
		names = append(names, p.Name)
	}
	if err := limits.Check(cfg.Tiers, host.Dimensions(), names); err != nil {
		host.Close()
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := core.CheckSchedules(cfg, host); err != nil {
		host.Close()
		return nil, fmt.Errorf("config: %w", err)
	}
	return host, nil
}

func serve(args []string) error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "hangar")
	slog.SetDefault(log)
	cfg, err := loadConfig(flag.NewFlagSet("serve", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	host, err := startPlugins(ctx, cfg, log, os.Stdout)
	if err != nil {
		return err
	}
	defer host.Close()
	if cfg.Identity.OIDC == nil {
		log.Warn("no identity provider configured: API tokens only (hangar token create)")
	}
	if what := untrusted(cfg); len(what) > 0 {
		log.Warn("this system trusts no certificate authority: what is reached over https will not be verified, and will fail", "what", what)
	}

	m := metrics.New()
	a := audit.New(log)
	c := core.New(ctx, cfg, store, host, a, log, m)
	srv, err := server.New(cfg, c, identity.New(cfg.Identity, store, nil), store, host, a, m, log, version)
	if err != nil {
		return err
	}
	hs := &http.Server{
		Addr: cfg.Listen, Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 90 * time.Second, IdleTimeout: 120 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Info("listening", "addr", cfg.Listen, "version", version, "plugins", len(host.Plugins()), "zones", len(cfg.Zones))
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		stop()
		<-done
		return err
	}
	return <-done
}

func check(args []string, out io.Writer) error {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg, err := loadConfig(flag.NewFlagSet("check", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if what := untrusted(cfg); len(what) > 0 {
		return noTrust(what)
	}
	host, err := startPlugins(context.Background(), cfg, log, io.Discard)
	if err != nil {
		return err
	}
	defer host.Close()
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PLUGIN\tZONE\tSTATE\tCAPABILITIES")
	for _, p := range host.Plugins() {
		zones := make([]string, 0, len(p.Zones))
		for z := range p.Zones {
			zones = append(zones, z)
		}
		sort.Strings(zones)
		for _, z := range zones {
			st, state := p.Zones[z], "ok"
			if st.Error != "" {
				state = "unusable: " + st.Error
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Name, z, state, strings.Join(st.Capabilities, " "))
		}
	}
	fmt.Fprintln(w, "\nTYPE\tID\tPLUGIN\tACTIONS")
	for _, t := range host.Types() {
		acts := []string{}
		for _, a := range t.Actions {
			acts = append(acts, a.Name)
		}
		fmt.Fprintf(w, "%s\t%s-…\t%s\t%s\n", t.Name, t.Prefix, t.Plugin, strings.Join(acts, " "))
	}
	fmt.Fprintln(w, "\nTIER\tGROUPS\tZONES\tOPERATOR")
	for _, t := range cfg.Tiers {
		fmt.Fprintf(w, "%s\t%s\t%s\t%v\n", t.Name, strings.Join(t.Groups, " "), strings.Join(t.Zones, " "), t.Operator)
	}
	if len(cfg.Schedules) > 0 {
		fmt.Fprintln(w, "\nSCHEDULE\tCRON\tMAKES\tAS\tKEEP\tNEXT")
		for _, sc := range cfg.Schedules {
			fmt.Fprintf(w, "%s\t%s (%s)\t%s in %s\t%s\t%d\t%s\n", sc.Name, sc.Line, sc.TimeZone, sc.Create.Type, sc.Create.Zone,
				sc.As.Subject, sc.Keep, sc.Line.Next(time.Now(), sc.Location).Format(time.RFC3339))
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nsound: every plugin started, every limit names a declared dimension, every schedule makes a declared type")
	return nil
}

func token(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("token: create, list or revoke")
	}
	fs := flag.NewFlagSet("token "+args[0], flag.ContinueOnError)
	subject := fs.String("subject", "", "the owner's subject, as the identity provider names them")
	name := fs.String("name", "", "what the token is for")
	groups := fs.String("groups", "", "comma-separated: the groups the token acts with")
	ttl := fs.Duration("ttl", 24*time.Hour, "how long it lives")
	scopes := fs.String("scopes", "read,write", "read; read,write; or room (a guest's hook: it claims and releases, nothing else)")
	rest := args[1:]
	var id string
	if args[0] == "revoke" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New("token revoke ID")
		}
		id, rest = rest[0], rest[1:]
	}
	cfg, err := loadConfig(fs, rest)
	if err != nil {
		return err
	}
	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx := context.Background()
	switch args[0] {
	case "create":
		if *subject == "" || *name == "" {
			return errors.New("token create needs --subject and --name")
		}
		secret, tok, err := identity.Mint(ctx, store, *subject, *name, split(*groups), split(*scopes), *ttl, cfg.Identity.Tokens.MaxTTL)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "made %s for %s, expires %s\n", tok.ID, tok.Owner, tok.ExpiresAt.Format(time.RFC3339))
		fmt.Fprintln(out, secret)
	case "list":
		toks, err := store.Tokens(ctx, *subject)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tOWNER\tNAME\tSCOPES\tEXPIRES\tSTATE")
		now := time.Now()
		for _, t := range toks {
			state := "live"
			switch {
			case t.RevokedAt != nil:
				state = "revoked"
			case !now.Before(t.ExpiresAt):
				state = "expired"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Owner, t.Name, strings.Join(t.Scopes, ","), t.ExpiresAt.Format(time.RFC3339), state)
		}
		return w.Flush()
	case "revoke":
		if err := store.RevokeToken(ctx, "", id); err != nil {
			return fmt.Errorf("revoke %s: %w", id, err)
		}
		fmt.Fprintf(os.Stderr, "revoked %s\n", id)
	default:
		return fmt.Errorf("token: no command %q", args[0])
	}
	return nil
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// runPlugin is a built-in plugin's process. The core starts it; by hand,
// go-plugin refuses to run and says so.
func runPlugin(args []string) error {
	if len(args) != 1 {
		return errors.New("plugin NAME (the core starts these itself)")
	}
	ctor, ok := builtins[args[0]]
	if !ok {
		return fmt.Errorf("no built-in plugin %q", args[0])
	}
	sdk.Serve(ctor())
	return nil
}
