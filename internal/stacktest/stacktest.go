// Package stacktest runs a whole brain in a test: the registry, the core, the
// API over HTTP, and each plugin as its own process — the test binary itself,
// started again as "<test> hangar-test-plugin <name>", exactly as the core
// starts "hangar plugin <name>". A door (the command line) is then tested
// against the real API, on the fake engine.
//
// The test package's TestMain calls ServePlugins first.
package stacktest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/audit"
	"github.com/tomblancdev/hangar/internal/config"
	"github.com/tomblancdev/hangar/internal/core"
	"github.com/tomblancdev/hangar/internal/identity"
	"github.com/tomblancdev/hangar/internal/limits"
	"github.com/tomblancdev/hangar/internal/metrics"
	"github.com/tomblancdev/hangar/internal/plugins"
	"github.com/tomblancdev/hangar/internal/registry"
	"github.com/tomblancdev/hangar/internal/server"
	"github.com/tomblancdev/hangar/internal/testoidc"
	"github.com/tomblancdev/hangar/plugins/images"
	"github.com/tomblancdev/hangar/plugins/machines"
	"github.com/tomblancdev/hangar/plugins/toy"
	"github.com/tomblancdev/hangar/plugins/volumes"
	"github.com/tomblancdev/hangar/sdk"
)

// ServePlugins is the test binary as a plugin's process, when the core
// started it as one; otherwise it returns at once.
func ServePlugins() {
	if len(os.Args) != 3 || os.Args[1] != "hangar-test-plugin" {
		return
	}
	switch os.Args[2] {
	case "toy":
		sdk.Serve(toy.New())
	case "machines":
		sdk.Serve(machines.New())
	case "volumes":
		sdk.Serve(volumes.New())
	case "images":
		sdk.Serve(images.New())
	}
	os.Exit(0)
}

// Stack is a running brain.
type Stack struct {
	URL   string
	Dir   string
	Store *registry.Store
	Core  *core.Core
	Iss   *testoidc.Issuer
	Logs  *SyncBuf
}

// SyncBuf is a buffer the brain's goroutines write their log to.
type SyncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *SyncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *SyncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// New runs a brain on a config: %[1]s is its data directory, %[2]s the test
// identity provider, %[3]s the plugins' program (with args
// [hangar-test-plugin, <name>]). enabled names the plugins the tiers are
// held against.
func New(t *testing.T, cfgText string, enabled ...string) *Stack {
	t.Helper()
	dir, err := os.MkdirTemp("", "h") // short: the plugins' sockets live under it
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := &Stack{Dir: dir, Iss: testoidc.New(t), Logs: &SyncBuf{}}
	cfg, err := config.Parse(fmt.Appendf(nil, cfgText, dir, s.Iss.URL, os.Args[0]))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(s.Logs, nil))
	if s.Store, err = registry.Open(dir + "/hangar.db"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	host, err := plugins.Start(ctx, cfg, plugins.Options{DataDir: dir, Logs: io.Discard}, log)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := limits.Check(cfg.Tiers, host.Dimensions(), enabled); err != nil {
		t.Fatal(err)
	}
	if err := core.CheckSchedules(cfg, host); err != nil {
		t.Fatal(err)
	}
	m, a := metrics.New(), audit.New(log)
	s.Core = core.New(ctx, cfg, s.Store, host, a, log, m)
	s.Core.Retry = nil
	srv, err := server.New(cfg, s.Core, identity.New(cfg.Identity, s.Store, nil), s.Store, host, a, m, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	s.URL = hs.URL
	done := make(chan struct{})
	go func() { _ = s.Core.Run(ctx); close(done) }()
	t.Cleanup(func() {
		hs.Close()
		cancel()
		<-done
		host.Close()
		_ = s.Store.Close()
	})
	return s
}

// Token makes an API token for a subject in some groups.
func (s *Stack) Token(t *testing.T, subject string, groups ...string) string {
	t.Helper()
	secret, _, err := identity.Mint(context.Background(), s.Store, subject, "test", groups, nil, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}
