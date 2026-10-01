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

	"github.com/tomblancdev/hangar/internal/cron"
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
	// Schedules: requests the brain makes by itself, on a clock.
	Schedules []Schedule `yaml:"schedules"`
	// TimeZone: the calendar the meters' months are counted in (default UTC)
	// — a tier's hours a month begin anew at its first midnight.
	TimeZone string `yaml:"time_zone"`

	// Location: the time zone, read.
	Location *time.Location `yaml:"-"`
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
	// Scopes the command line asks for when it signs someone in (default
	// openid, profile, offline_access): whichever of the provider's scopes
	// put the groups claim in the token, and offline_access for a refresh
	// token. The command line signs in with the audience as its client id —
	// a public client, the device flow on (RFC 8628): the brain holds no
	// client secret, it only reads tokens.
	Scopes []string `yaml:"scopes"`
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
	// Room: claims and releases the reservations of the zones open to it —
	// the tier of the hooks on the guests they wait on (their tokens carry
	// the scope room alone).
	Room bool `yaml:"room"`
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
	// Room: the memory the zone's resources may count on, and the room it
	// keeps for others (ARCHITECTURE.md §6). Absent = the zone counts no
	// room: whatever the tiers allow fits.
	Room *Room `yaml:"room"`
	// Wake: a zone that sleeps is woken by this call before anything starts
	// in it.
	Wake *Wake `yaml:"wake"`
}

// Room is a zone's capacity and its reservations. The guaranteed pool is the
// memory less every reservation, as if all were in force; the spot pool is
// the room the conditional reservations keep while none of them is — when
// one is, it takes the whole spot pool back.
type Room struct {
	// MemoryGB: what the product's resources may count on in this zone —
	// the zone's memory less what is not theirs (or declare that as a
	// reservation with no condition).
	MemoryGB int `yaml:"memory_gb"`
	// Reservations: room kept for someone else, always or on a condition.
	Reservations []Reservation `yaml:"reservations"`
	// Grace: how long a hold is kept on the word of a claim — or of the
	// engine's node, acting alone — when its condition does not read true yet:
	// the time a priority guest takes to be seen running (default 10m).
	Grace time.Duration `yaml:"grace"`
}

// Reservation is room kept for someone else. With no condition it is always
// in force; while_running and while_down make it borrowable the rest of the
// time.
type Reservation struct {
	Name     string `yaml:"name"`
	MemoryGB int    `yaml:"memory_gb"`
	// WhileRunning: a guest outside the product, by the engine's own name
	// (a Proxmox VMID); its hook claims the room before it starts.
	WhileRunning string `yaml:"while_running"`
	// WhileDown: a node of the engine; its guests land here when it fails.
	WhileDown string `yaml:"while_down"`
}

// Key is the hold's key written on what the reservation holds: the guest's
// name, or down-<node>. A reservation with no condition holds nothing.
func (r Reservation) Key() string {
	switch {
	case r.WhileRunning != "":
		return r.WhileRunning
	case r.WhileDown != "":
		return "down-" + r.WhileDown
	}
	return ""
}

// Wake is the call that wakes a zone that sleeps: a webhook.
type Wake struct {
	URL string `yaml:"url"`
	// Method: default POST.
	Method string `yaml:"method"`
	// Body: sent as it is, as JSON ({"wait": true}).
	Body string `yaml:"body"`
	// Headers carry the call's credential, from a file or an environment
	// variable of the core's — never written here.
	Headers map[string]Secret `yaml:"headers"`
	// Timeout: how long the zone may take to answer after the call (default
	// 5m); a start still waiting then fails, and says so.
	Timeout time.Duration `yaml:"timeout"`
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

// Schedule is a create the brain asks for by itself, on a clock, in the name
// of the one it names and within their tier — a recipe baked again every
// week (AWS Image Builder's pipeline, a Kubernetes CronJob). What it made is
// tagged hangar:schedule=<name>; "@<name>" in a reference names the newest
// of them that is usable; once newer ones are usable, the older ones beyond
// Keep are retired, then deleted once nothing names them.
type Schedule struct {
	Name string `yaml:"name"`
	// Cron: when — minute hour day-of-month month day-of-week.
	Cron string `yaml:"cron"`
	// TimeZone the cron is read in (default UTC).
	TimeZone string `yaml:"time_zone"`
	// As: whose name the brain asks in — a subject, and the groups that give
	// it its tier. What it makes is theirs, and counts against their limits.
	As Principal `yaml:"as"`
	// Create: what it asks for, as a create through the API says it.
	Create Create `yaml:"create"`
	// Keep: how many of the newest usable ones stay as they are (default 2).
	Keep int `yaml:"keep"`
	// Retire: the action an older usable one is given first (an image's
	// retire: no machine is born from it any more); empty = none. An older
	// one is deleted once nothing names it.
	Retire string `yaml:"retire"`

	// Line and Location: the cron, read.
	Line     cron.Line      `yaml:"-"`
	Location *time.Location `yaml:"-"`
}

// Principal is whom the brain acts for.
type Principal struct {
	Subject string   `yaml:"subject"`
	Groups  []string `yaml:"groups"`
}

// Create is a create request, as the API's body says it.
type Create struct {
	Type string            `yaml:"type"`
	Zone string            `yaml:"zone"`
	Spec map[string]any    `yaml:"spec"`
	Tags map[string]string `yaml:"tags"`
}

// Schedule returns the named schedule.
func (c *Config) Schedule(name string) (*Schedule, bool) {
	for i := range c.Schedules {
		if c.Schedules[i].Name == name {
			return &c.Schedules[i], true
		}
	}
	return nil, false
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
		if len(o.Scopes) == 0 {
			o.Scopes = []string{"openid", "profile", "offline_access"}
		}
	}
	if c.Reconcile.Every == 0 {
		c.Reconcile.Every = time.Minute
	}
	if c.TimeZone == "" {
		c.TimeZone = "UTC"
	}
	for i := range c.Schedules {
		sc := &c.Schedules[i]
		if sc.TimeZone == "" {
			sc.TimeZone = "UTC"
		}
		if sc.Keep == 0 {
			sc.Keep = 2
		}
	}
	for i := range c.Zones {
		z := &c.Zones[i]
		if z.Room != nil && z.Room.Grace == 0 {
			z.Room.Grace = 10 * time.Minute
		}
		if w := z.Wake; w != nil {
			if w.Method == "" {
				w.Method = "POST"
			}
			if w.Timeout == 0 {
				w.Timeout = 5 * time.Minute
			}
		}
	}
}

// keyRe: what a hold's key may hold — it is written in engines' tags.
var keyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func (z Zone) validateRoom(bad func(string, ...any)) {
	if w := z.Wake; w != nil {
		if !strings.HasPrefix(w.URL, "https://") && !strings.HasPrefix(w.URL, "http://") {
			bad("zone %s: wake.url must be a URL", z.Name)
		}
		for h, s := range w.Headers {
			if (s.File == "") == (s.Env == "") {
				bad("zone %s: wake header %s: exactly one of file or env", z.Name, h)
			}
		}
	}
	r := z.Room
	if r == nil {
		return
	}
	if r.MemoryGB < 1 {
		bad("zone %s: room.memory_gb is the memory its resources may count on; at least 1", z.Name)
	}
	names, keys := map[string]bool{}, map[string]bool{}
	var sum int
	for i, rv := range r.Reservations {
		if !nameRe.MatchString(rv.Name) {
			bad("zone %s: reservations[%d]: name %q: lowercase letters, digits, - and _", z.Name, i, rv.Name)
		}
		if names[rv.Name] {
			bad("zone %s: reservation %q twice", z.Name, rv.Name)
		}
		names[rv.Name] = true
		if rv.MemoryGB < 1 {
			bad("zone %s: reservation %s keeps no memory", z.Name, rv.Name)
		}
		sum += rv.MemoryGB
		if rv.WhileRunning != "" && rv.WhileDown != "" {
			bad("zone %s: reservation %s: one condition, while_running or while_down", z.Name, rv.Name)
		}
		if k := rv.Key(); k != "" {
			if !keyRe.MatchString(k) {
				bad("zone %s: reservation %s: %q is written in engines' tags: lowercase letters, digits, - and _", z.Name, rv.Name, k)
			}
			if keys[k] {
				bad("zone %s: two reservations wait on %s", z.Name, k)
			}
			keys[k] = true
		}
	}
	if sum > r.MemoryGB {
		bad("zone %s: its reservations keep %d GB, more than the %d GB of room it has", z.Name, sum, r.MemoryGB)
	}
}

// Watch lists the guests a zone's reservations wait on, for the plugins'
// drivers to read (and nothing more).
func (z Zone) Watch() []string {
	var out []string
	if z.Room != nil {
		for _, r := range z.Room.Reservations {
			if r.WhileRunning != "" {
				out = append(out, r.WhileRunning)
			}
		}
	}
	return out
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
	var err error
	if c.Location, err = time.LoadLocation(c.TimeZone); err != nil {
		c.Location = time.UTC
		bad("time_zone %q: %v", c.TimeZone, err)
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
		z.validateRoom(bad)
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
	c.validateSchedules(zones, bad)
	return errors.Join(errs...)
}

func (c *Config) validateSchedules(zones map[string]bool, bad func(string, ...any)) {
	names := map[string]bool{}
	for i := range c.Schedules {
		sc := &c.Schedules[i]
		if !nameRe.MatchString(sc.Name) {
			bad("schedules[%d]: name %q: lowercase letters, digits, - and _", i, sc.Name)
		}
		if names[sc.Name] {
			bad("schedules: %q twice", sc.Name)
		}
		names[sc.Name] = true
		var err error
		if sc.Line, err = cron.Parse(sc.Cron); err != nil {
			bad("schedule %s: %v", sc.Name, err)
		}
		if sc.Location, err = time.LoadLocation(sc.TimeZone); err != nil {
			bad("schedule %s: time_zone %q: %v", sc.Name, sc.TimeZone, err)
		}
		if sc.As.Subject == "" || len(sc.As.Groups) == 0 {
			bad("schedule %s: as: the subject it asks in the name of, and the groups that give it its tier", sc.Name)
		} else if t := c.tierFor(sc.As.Groups); t == nil {
			bad("schedule %s: none of the groups %s is in a tier", sc.Name, strings.Join(sc.As.Groups, ", "))
		} else if !slices.Contains(t.Zones, "*") && !slices.Contains(t.Zones, sc.Create.Zone) {
			bad("schedule %s: tier %s is not open to zone %s", sc.Name, t.Name, sc.Create.Zone)
		}
		if sc.Create.Type == "" {
			bad("schedule %s: create.type: what it asks for", sc.Name)
		}
		if !zones[sc.Create.Zone] {
			bad("schedule %s: create.zone %q is not declared", sc.Name, sc.Create.Zone)
		}
		for k := range sc.Create.Tags {
			if strings.HasPrefix(strings.ToLower(k), "hangar") {
				bad("schedule %s: tag %s: keys starting with hangar are the core's", sc.Name, k)
			}
		}
		if sc.Keep < 1 {
			bad("schedule %s: keep: at least 1 — the newest usable one stays", sc.Name)
		}
	}
}

// Tier returns the named tier.
func (c *Config) Tier(name string) (*Tier, bool) {
	for i := range c.Tiers {
		if c.Tiers[i].Name == name {
			return &c.Tiers[i], true
		}
	}
	return nil, false
}

// tierFor is the first tier, in the file's order, one of the groups reaches.
func (c *Config) tierFor(groups []string) *Tier {
	for i := range c.Tiers {
		for _, g := range c.Tiers[i].Groups {
			if slices.Contains(groups, g) {
				return &c.Tiers[i]
			}
		}
	}
	return nil
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
