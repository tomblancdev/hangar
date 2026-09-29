// Package config reads the operator's one file: who signs in and how, the
// tiers and their limits, the zones, and the plugins enabled on them.
//
// Everything that describes the operator's world lives here and nowhere in
// the product: addresses, group names, zone names, the house word.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole file.
type Config struct {
	// Listen is the API's address (":8080"). Plain HTTP: the operator's
	// gateway terminates TLS in front of it.
	Listen string `yaml:"listen"`
	// DataDir holds the registry (hangar.db) and the plugins' sockets.
	DataDir string `yaml:"data_dir"`
	// House is the operator's word for the mark; empty = the product's own.
	House     string    `yaml:"house"`
	Identity  Identity  `yaml:"identity"`
	Tiers     []Tier    `yaml:"tiers"`
	Zones     []Zone    `yaml:"zones"`
	Plugins   []Plugin  `yaml:"plugins"`
	Reconcile Reconcile `yaml:"reconcile"`
}

// Identity is how a caller proves who they are.
type Identity struct {
	// OIDC: bearer tokens signed by the operator's identity provider. Absent
	// = API tokens only (made on the brain's host with `hangar token create`).
	OIDC   *OIDC  `yaml:"oidc"`
	Tokens Tokens `yaml:"tokens"`
}

// OIDC names the identity provider and the claims read from its tokens.
type OIDC struct {
	Issuer string `yaml:"issuer"`
	// Audience is the client id the tokens must be issued to.
	Audience string `yaml:"audience"`
	// GroupsClaim holds the person's groups (default "groups").
	GroupsClaim string `yaml:"groups_claim"`
	// NameClaim is shown in the audit beside the subject (default
	// "preferred_username").
	NameClaim string `yaml:"name_claim"`
}

// Tokens bounds the API tokens people make for automation.
type Tokens struct {
	// MaxTTL is the longest a token may live (default 720h).
	MaxTTL time.Duration `yaml:"max_ttl"`
}

// Tier is a line of limits, reached through groups.
type Tier struct {
	Name string `yaml:"name"`
	// Groups: a person in any of these is in this tier. The FIRST tier in the
	// file that matches wins, so order them from the most to the least.
	Groups []string `yaml:"groups"`
	// Operator: sees and acts on every resource, not only its own.
	Operator bool `yaml:"operator"`
	// Zones open to the tier; "*" = all.
	Zones []string `yaml:"zones"`
	// Limits per dimension ("toy.boxes": 2). A dimension the tier does not
	// name is allowed NOTHING. "<plugin>.*" and "*" name many at once; the
	// most precise name wins.
	Limits map[string]Limit `yaml:"limits"`
}

// Limit is a number, the word "unlimited", or — for a choice — the list of
// values allowed.
type Limit struct {
	Unlimited bool
	Max       int64
	Allowed   []string
	IsChoice  bool
}

// UnmarshalYAML reads the three spellings of a limit.
func (l *Limit) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Value == "unlimited" {
			l.Unlimited = true
			return nil
		}
		var v int64
		if err := n.Decode(&v); err != nil || v < 0 {
			return fmt.Errorf("line %d: a limit is a number >= 0, \"unlimited\" or a list of allowed values, not %q", n.Line, n.Value)
		}
		l.Max = v
		return nil
	case yaml.SequenceNode:
		l.IsChoice = true
		return n.Decode(&l.Allowed)
	}
	return fmt.Errorf("line %d: a limit is a number, \"unlimited\" or a list", n.Line)
}

// Zone is one engine connection.
type Zone struct {
	Name     string            `yaml:"name"`
	Driver   string            `yaml:"driver"`
	Endpoint string            `yaml:"endpoint"`
	Options  map[string]string `yaml:"options"`
}

// Plugin enables one plugin on some zones.
type Plugin struct {
	Name string `yaml:"name"`
	// Builtin names a plugin compiled into this binary; Path a program of
	// its own. Exactly one.
	Builtin string   `yaml:"builtin"`
	Path    string   `yaml:"path"`
	Args    []string `yaml:"args"`
	// SHA256 of the program at Path: the core refuses to start one that
	// does not match.
	SHA256 string   `yaml:"sha256"`
	Zones  []string `yaml:"zones"`
	// Settings are handed to the plugin as they are (a JSON object).
	Settings map[string]any `yaml:"settings"`
	// Credentials per zone: the plugin gets these and nothing else.
	Credentials map[string]Secret `yaml:"credentials"`
}

// Secret is read from a file or an environment variable of the core's — never
// written in this file.
type Secret struct {
	File string `yaml:"file"`
	Env  string `yaml:"env"`
}

// Read returns the secret's value, a file's trailing newline removed.
func (s Secret) Read() ([]byte, error) {
	switch {
	case s.File != "":
		b, err := os.ReadFile(s.File)
		if err != nil {
			return nil, err
		}
		return bytes.TrimRight(b, "\r\n"), nil
	case s.Env != "":
		v, ok := os.LookupEnv(s.Env)
		if !ok {
			return nil, fmt.Errorf("environment variable %s is not set", s.Env)
		}
		return []byte(v), nil
	}
	return nil, nil
}

// Reconcile paces the loop that compares the registry with the engines.
type Reconcile struct {
	Every time.Duration `yaml:"every"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Load reads, defaults and validates the file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse reads, defaults and validates a file's bytes.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = "/data"
	}
	if c.Identity.Tokens.MaxTTL == 0 {
		c.Identity.Tokens.MaxTTL = 720 * time.Hour
	}
	if o := c.Identity.OIDC; o != nil {
		if o.GroupsClaim == "" {
			o.GroupsClaim = "groups"
		}
		if o.NameClaim == "" {
			o.NameClaim = "preferred_username"
		}
	}
	if c.Reconcile.Every == 0 {
		c.Reconcile.Every = time.Minute
	}
}

func (c *Config) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if o := c.Identity.OIDC; o != nil {
		if !strings.HasPrefix(o.Issuer, "https://") && !strings.HasPrefix(o.Issuer, "http://") {
			bad("identity.oidc.issuer must be a URL")
		}
		if o.Audience == "" {
			bad("identity.oidc.audience is the client id tokens are issued to; it is required")
		}
	}
	if c.Reconcile.Every < 5*time.Second {
		bad("reconcile.every must be at least 5s")
	}

	zones := map[string]bool{}
	for i, z := range c.Zones {
		if !nameRe.MatchString(z.Name) {
			bad("zones[%d]: name %q: lowercase letters, digits, - and _", i, z.Name)
		}
		if zones[z.Name] {
			bad("zones: %q twice", z.Name)
		}
		zones[z.Name] = true
		if z.Driver == "" {
			bad("zone %s: no driver", z.Name)
		}
	}

	if len(c.Tiers) == 0 {
		bad("tiers: none — nobody could ask for anything")
	}
	tiers := map[string]bool{}
	for i, t := range c.Tiers {
		if !nameRe.MatchString(t.Name) {
			bad("tiers[%d]: name %q: lowercase letters, digits, - and _", i, t.Name)
		}
		if tiers[t.Name] {
			bad("tiers: %q twice", t.Name)
		}
		tiers[t.Name] = true
		if len(t.Groups) == 0 {
			bad("tier %s: no groups — nobody could be in it", t.Name)
		}
		for _, z := range t.Zones {
			if z != "*" && !zones[z] {
				bad("tier %s: zone %q is not declared", t.Name, z)
			}
		}
	}

	plugins := map[string]bool{}
	for i, p := range c.Plugins {
		if !nameRe.MatchString(p.Name) {
			bad("plugins[%d]: name %q: lowercase letters, digits, - and _", i, p.Name)
		}
		if plugins[p.Name] {
			bad("plugins: %q twice", p.Name)
		}
		plugins[p.Name] = true
		if (p.Builtin == "") == (p.Path == "") {
			bad("plugin %s: exactly one of builtin or path", p.Name)
		}
		if p.SHA256 != "" && p.Path == "" {
			bad("plugin %s: sha256 checks a program at path; a builtin is this binary", p.Name)
		}
		if len(p.Zones) == 0 {
			bad("plugin %s: enabled on no zone", p.Name)
		}
		for _, z := range p.Zones {
			if !zones[z] {
				bad("plugin %s: zone %q is not declared", p.Name, z)
			}
		}
		for z, s := range p.Credentials {
			if !slices.Contains(p.Zones, z) {
				bad("plugin %s: a credential for zone %q, where it is not enabled", p.Name, z)
			}
			if (s.File == "") == (s.Env == "") {
				bad("plugin %s: credential for %s: exactly one of file or env", p.Name, z)
			}
		}
	}
	return errors.Join(errs...)
}

// Zone returns the named zone.
func (c *Config) Zone(name string) (Zone, bool) {
	for _, z := range c.Zones {
		if z.Name == name {
			return z, true
		}
	}
	return Zone{}, false
}
