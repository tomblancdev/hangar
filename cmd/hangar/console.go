package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tomblancdev/hangar/internal/console"
	"github.com/tomblancdev/hangar/ui"
)

// runConsole is the console on its own: a process in front of a brain,
// reaching it through its API alone. It reads no config file, opens no
// registry, starts no plugin and holds no engine's key — what it keeps is
// the sign-ins of the people using it, in memory. The process to put on a
// public door, the brain staying behind it.
func runConsole(args []string) error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "hangar-console")
	slog.SetDefault(log)
	env := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	fs := flag.NewFlagSet("console", flag.ContinueOnError)
	brain := fs.String("brain", env("HANGAR_CONSOLE_BRAIN", ""), "the brain's address, as this process reaches it (http://brain:8080)")
	listen := fs.String("listen", env("HANGAR_CONSOLE_LISTEN", ":8081"), "the address to serve on")
	public := fs.String("url", env("HANGAR_CONSOLE_URL", ""), "the address people open it at (https://hangar.example.org); empty: read from each request")
	house := fs.String("house", env("HANGAR_CONSOLE_HOUSE", ""), "your word for the mark; empty = the product's own")
	idleDef, err := time.ParseDuration(env("HANGAR_CONSOLE_IDLE", "12h"))
	if err != nil {
		return fmt.Errorf("console: $HANGAR_CONSOLE_IDLE: %w", err)
	}
	idle := fs.Duration("idle", idleDef, "a sign-in unused this long ends")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := url.Parse(strings.TrimRight(*brain, "/"))
	if *brain == "" || err != nil || (b.Scheme != "https" && b.Scheme != "http") || b.Host == "" || b.Path != "" {
		return errors.New("console: --brain URL (or $HANGAR_CONSOLE_BRAIN): the brain's address — http://host:port")
	}
	if *idle < time.Minute {
		return errors.New("console: --idle must be at least 1m")
	}
	if h := b.Hostname(); b.Scheme == "http" && h != "localhost" && h != "127.0.0.1" && h != "::1" {
		log.Warn("the brain is reached over plain http: people's tokens cross that link in clear — keep it a private one", "brain", b.String())
	}
	reach := &http.Client{Timeout: 90 * time.Second}
	c, err := console.New(console.Options{
		Brain: reach, BrainURL: b.String(), URL: *public, Idle: *idle,
		Static: ui.Console(), Mark: func() []byte { return ui.Still(*house) },
		Version: version, Log: log,
	})
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle(console.Prefix+"/", c.Handler())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, console.Prefix+"/", http.StatusFound)
	})
	// its own health: it serves, and its brain answers
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		body, code := map[string]any{"status": "ok", "version": version, "sessions": c.Sessions()}, 200
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.String()+"/healthz", nil)
		resp, err := reach.Do(req)
		switch {
		case err != nil:
			body["status"], body["why"], code = "degraded", []string{"the brain cannot be reached"}, 503
		default:
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body["status"], body["why"], code = "degraded", []string{"the brain answers " + resp.Status}, 503
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hs := &http.Server{
		Addr: *listen, Handler: mux,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 90 * time.Second, IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Info("listening", "addr", *listen, "version", version, "brain", b.String(), "url", *public)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
