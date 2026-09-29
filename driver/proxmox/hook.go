package proxmox

// The hook — what runs on a Proxmox VE node as a guest's hookscript
// (cmd/hangar-hook), as root, with the guest's id and phase:
//
//   - On a product machine (its tags carry hangar-id), before it starts: the
//     node's own admission. It refuses a start whose memory is above what the
//     brain admitted, a spot machine's while a priority guest of the node has
//     the room, and a floor's above its floor then — whoever started it (the
//     brain, the GUI, qm). The operator sets the hook on the images' VM
//     templates; every clone inherits it, and the product's token cannot
//     remove it (only root@pam sets a hookscript).
//   - On a priority guest (no hangar-id; the operator sets the hook on it),
//     before it starts: it phones the brain (claim) and waits while the brain
//     holds what the zone lends. When the brain cannot be reached it acts
//     alone, on the tags: every spot machine of the node stopped, every floor
//     shrunk to it, every cap applied, each tagged held.<vmid> first. It never
//     refuses the start. After the guest stops: release, or — the brain out
//     of reach — the node gives back what it took (floors regrown, caps
//     lifted, tags removed); the brain restarts the spot machines.
//
// A marker in the run directory says a priority guest of this node is
// starting or running, for the admission to read.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HookConfig is the hook's file on the node (JSON; /etc/hangar/hook.json).
type HookConfig struct {
	// Brain is the brain's URL; TokenFile holds an API token of the scope
	// room (hangar token create --scopes room), for a tier with room: true.
	Brain     string `json:"brain"`
	TokenFile string `json:"token_file"`
	// CAFile verifies the brain's certificate, when it is not in the node's
	// own bundle.
	CAFile string `json:"ca_file,omitempty"`
	// Zone is the brain's name for the zone this node's guests are in.
	Zone string `json:"zone"`
	// Timeout: how long a priority guest's start waits for the brain before
	// the node acts alone (default 90s).
	Timeout Duration `json:"timeout,omitempty"`
	// ShutdownTimeout: how long a spot machine the node stops alone is asked
	// before it is made to (default 30s).
	ShutdownTimeout Duration `json:"shutdown_timeout,omitempty"`
	// Grace: how long a priority guest's marker counts before its guest is
	// seen running (default 10m, the brain's own default).
	Grace Duration `json:"grace,omitempty"`
	// RunDir holds the markers (default /run/hangar-hook: gone at a reboot,
	// like the guests that ran).
	RunDir string `json:"run_dir,omitempty"`
}

// Duration reads "90s" in JSON.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}

// ReadHookConfig reads and defaults the hook's file.
func ReadHookConfig(path string) (HookConfig, error) {
	var c HookConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.Timeout == 0 {
		c.Timeout = Duration(90 * time.Second)
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = Duration(30 * time.Second)
	}
	if c.Grace == 0 {
		c.Grace = Duration(10 * time.Minute)
	}
	if c.RunDir == "" {
		c.RunDir = "/run/hangar-hook"
	}
	return c, nil
}

// NodeGuest is a guest as the node's own files say.
type NodeGuest struct {
	VMID    string
	Kind    string // "qemu" or "lxc"
	Running bool
	Config  map[string]string // the current section of its config
	// MemoryUsedMB: what a running container holds (its cgroup's).
	MemoryUsedMB int
}

// Tags is the guest's tag list.
func (g NodeGuest) Tags() []string { return tagList(g.Config["tags"]) }

// Tag returns the value of a key.value tag.
func (g NodeGuest) Tag(key string) string {
	for _, t := range g.Tags() {
		if v, ok := strings.CutPrefix(t, key+"."); ok {
			return v
		}
	}
	return ""
}

func (g NodeGuest) tagInt(key string) int { n, _ := strconv.Atoi(g.Tag(key)); return n }

// Product reports whether hangar made it.
func (g NodeGuest) Product() bool { return g.Tag(idTag) != "" }

// Node is what the hook reads and changes on its node.
type Node interface {
	Guest(vmid string) (NodeGuest, error)
	Guests() ([]NodeGuest, error)
	// Shutdown asks a guest to stop, then makes it after timeout.
	Shutdown(g NodeGuest, timeout time.Duration) error
	// Set changes a guest's config: key → value, "" deletes the key.
	Set(g NodeGuest, changes map[string]string) error
}

// Hook runs one hookscript call.
type Hook struct {
	Cfg  HookConfig
	Node Node
	// Log receives what the hook says (syslog and the task's log).
	Log func(format string, a ...any)
	// Client reaches the brain (default: one built from the config).
	Client *http.Client
	Now    func() time.Time
}

// ErrRefused is an admission's refusal: the start does not happen.
var ErrRefused = errors.New("refused")

// Run handles one phase; an error wrapping ErrRefused refuses the start,
// anything else is logged and never does.
func (h *Hook) Run(ctx context.Context, vmid, phase string) error {
	if h.Now == nil {
		h.Now = time.Now
	}
	g, err := h.Node.Guest(vmid)
	if err != nil {
		h.Log("guest %s: %v (nothing done)", vmid, err)
		return nil
	}
	if g.Product() {
		if phase == "pre-start" {
			return h.admit(g)
		}
		return nil
	}
	switch phase {
	case "pre-start":
		h.mark(vmid)
		h.claim(ctx, vmid, true)
	case "post-stop":
		// the marker goes first: the brain's release starts spot machines
		// again, and their own admission reads it
		h.unmark(vmid)
		h.claim(ctx, vmid, false)
	}
	return nil
}

// ---- The admission ----------------------------------------------------------

func (h *Hook) admit(g NodeGuest) error {
	id := g.Tag(idTag)
	mem := memoryMB(g.Config["memory"])
	if a := g.tagInt("admitted"); a > 0 && mem > a {
		return fmt.Errorf("%w: hangar: %s is set to %d MB, and was admitted at %d MB: change its size through hangar", ErrRefused, id, mem, a)
	}
	holder := h.holder()
	if holder == "" {
		return nil
	}
	switch g.Tag("class") {
	case "spot":
		return fmt.Errorf("%w: hangar: %s is spot, and guest %s of this node has its room: it starts when the room is back", ErrRefused, id, holder)
	case "guaranteed+spot":
		if f := g.tagInt("floor"); f > 0 && mem > f {
			return fmt.Errorf("%w: hangar: %s is set to %d MB while guest %s has the room: it starts on its floor, %d MB (hangar does that)",
				ErrRefused, id, mem, holder, f)
		}
	}
	return nil
}

// holder is a priority guest of this node holding the room now: one whose
// marker is fresh, or whose guest runs.
func (h *Hook) holder() string {
	ents, _ := os.ReadDir(h.Cfg.RunDir)
	for _, e := range ents {
		vmid := e.Name()
		b, err := os.ReadFile(filepath.Join(h.Cfg.RunDir, vmid))
		if err != nil {
			continue
		}
		at, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if h.Now().Sub(time.Unix(at, 0)) < time.Duration(h.Cfg.Grace) {
			return vmid
		}
		if g, err := h.Node.Guest(vmid); err == nil && g.Running {
			return vmid
		}
	}
	return ""
}

func (h *Hook) mark(vmid string) {
	if err := os.MkdirAll(h.Cfg.RunDir, 0o755); err == nil {
		err = os.WriteFile(filepath.Join(h.Cfg.RunDir, vmid), []byte(strconv.FormatInt(h.Now().Unix(), 10)), 0o644)
		if err == nil {
			return
		}
		h.Log("the marker for %s could not be written: %v", vmid, err)
	}
}

func (h *Hook) unmark(vmid string) { _ = os.Remove(filepath.Join(h.Cfg.RunDir, vmid)) }

// ---- The claim: the brain first ---------------------------------------------

type claimAnswer struct {
	Reservation string `json:"reservation"`
	HeldBy      string `json:"held_by"`
	Detail      string `json:"detail"`
	Resources   []struct {
		Resource string `json:"resource"`
		Result   string `json:"result"`
		Detail   string `json:"detail"`
	} `json:"resources"`
}

// claim phones the brain; when it cannot be reached — or does not answer in
// time, or refuses the hook's token — the node acts alone. A brain that
// answers "no room for this guest" is the operator's word: nothing is held.
func (h *Hook) claim(ctx context.Context, vmid string, claim bool) {
	verb := map[bool]string{true: "claim", false: "release"}[claim]
	ans, status, err := h.phone(ctx, vmid, verb)
	switch {
	case err == nil && status == http.StatusOK:
		h.Log("%s for guest %s: the brain answered (reservation %s): %s", verb, vmid, ans.Reservation, summary(ans))
		return
	case err == nil && status == http.StatusNotFound:
		h.Log("%s for guest %s: the brain keeps no room for it (%s): nothing held", verb, vmid, ans.Detail)
		return
	case err == nil:
		h.Log("%s for guest %s: the brain answered %d (%s): the node acts alone", verb, vmid, status, ans.Detail)
	default:
		h.Log("%s for guest %s: the brain could not be reached (%v): the node acts alone", verb, vmid, err)
	}
	if claim {
		h.holdAlone(vmid)
	} else {
		h.releaseAlone(vmid)
	}
}

func summary(a claimAnswer) string {
	if len(a.Resources) == 0 {
		return "nothing to change"
	}
	var parts []string
	for _, r := range a.Resources {
		p := r.Resource + " " + r.Result
		if r.Detail != "" {
			p += " (" + r.Detail + ")"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

func (h *Hook) phone(ctx context.Context, vmid, verb string) (claimAnswer, int, error) {
	var ans claimAnswer
	tok, err := os.ReadFile(h.Cfg.TokenFile)
	if err != nil {
		return ans, 0, fmt.Errorf("its token: %w", err)
	}
	client := h.Client
	if client == nil {
		if client, err = brainClient(h.Cfg.CAFile); err != nil {
			return ans, 0, err
		}
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(h.Cfg.Timeout))
	defer cancel()
	body, _ := json.Marshal(map[string]string{"guest": vmid})
	url := strings.TrimSuffix(h.Cfg.Brain, "/") + "/v1/zones/" + h.Cfg.Zone + "/" + verb
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ans, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ans, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(b, &ans)
	return ans, resp.StatusCode, nil
}

func brainClient(caFile string) (*http.Client, error) {
	if caFile == "" {
		return &http.Client{}, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no certificate", caFile)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, nil
}

// ---- The node alone ---------------------------------------------------------

func retagged(g NodeGuest, add, remove string) string {
	var out []string
	for _, t := range g.Tags() {
		if t != remove {
			out = append(out, t)
		}
	}
	if add != "" {
		out = append(out, add)
	}
	sort.Strings(out)
	return strings.Join(slices.Compact(out), ";")
}

// holdAlone does what the brain would: for each product machine of the node
// that takes borrowed room, the tag first, then stopped (spot) or shrunk to
// its floor, and capped.
func (h *Hook) holdAlone(p string) {
	gs, err := h.Node.Guests()
	if err != nil {
		h.Log("hold for guest %s: the node's guests cannot be read: %v", p, err)
		return
	}
	var wg sync.WaitGroup
	for _, g := range gs {
		class, beside := g.Tag("class"), g.tagInt("beside")
		if !g.Product() || g.VMID == p || (class != "spot" && class != "guaranteed+spot" && beside == 0) {
			continue
		}
		id := g.Tag(idTag)
		if err := h.Node.Set(g, map[string]string{"tags": retagged(g, HeldTag+"."+p, "")}); err != nil {
			h.Log("hold: %s (%s): its tag could not be written: %v", id, g.VMID, err)
		}
		switch {
		case class == "spot" && g.Running:
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := h.Node.Shutdown(g, time.Duration(h.Cfg.ShutdownTimeout)); err != nil {
					h.Log("hold: %s (%s) could not be stopped: %v", id, g.VMID, err)
					return
				}
				h.Log("hold: %s (%s) stopped for guest %s", id, g.VMID, p)
			}()
			continue
		case class == "spot":
			continue
		}
		changes := map[string]string{}
		if beside > 0 {
			changes["cpulimit"] = strconv.Itoa(beside)
		}
		if floor := g.tagInt("floor"); class == "guaranteed+spot" && floor > 0 {
			mem, target := memoryMB(g.Config["memory"]), floor
			if g.Running && g.MemoryUsedMB+usedMargin > target {
				target = g.MemoryUsedMB + usedMargin
				h.Log("hold: %s (%s) holds %d MB, above its floor of %d MB: %d MB stay lent", id, g.VMID, g.MemoryUsedMB, floor, min(target, mem)-floor)
			}
			if target < mem {
				changes["memory"] = strconv.Itoa(target)
			}
		}
		if len(changes) > 0 {
			if err := h.Node.Set(g, changes); err != nil {
				h.Log("hold: %s (%s) could not be shrunk or capped: %v", id, g.VMID, err)
				continue
			}
			h.Log("hold: %s (%s) %s for guest %s", id, g.VMID, describe(changes), p)
		}
	}
	wg.Wait()
}

// releaseAlone gives back what holdAlone — or the brain — took for p, where
// nothing else holds it.
func (h *Hook) releaseAlone(p string) {
	gs, err := h.Node.Guests()
	if err != nil {
		h.Log("release for guest %s: the node's guests cannot be read: %v", p, err)
		return
	}
	for _, g := range gs {
		if !g.Product() || !slices.Contains(g.Tags(), HeldTag+"."+p) {
			continue
		}
		id := g.Tag(idTag)
		changes := map[string]string{"tags": retagged(g, "", HeldTag+"."+p)}
		if len(holdsOf(changes["tags"])) == 0 {
			if a := g.tagInt("admitted"); g.Tag("class") == "guaranteed+spot" && a > memoryMB(g.Config["memory"]) {
				changes["memory"] = strconv.Itoa(a)
			}
			if g.Config["cpulimit"] != "" {
				changes["cpulimit"] = ""
			}
		}
		if err := h.Node.Set(g, changes); err != nil {
			h.Log("release: %s (%s): %v", id, g.VMID, err)
			continue
		}
		h.Log("release: %s (%s) %s", id, g.VMID, describe(changes))
	}
}

func describe(changes map[string]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(changes)) {
		switch v := changes[k]; {
		case k == "tags":
			parts = append(parts, "tags "+v)
		case v == "":
			parts = append(parts, k+" lifted")
		case k == "memory":
			parts = append(parts, "memory "+v+" MB")
		case k == "cpulimit":
			parts = append(parts, "capped to "+v+" cores")
		default:
			parts = append(parts, k+" "+v)
		}
	}
	return strings.Join(parts, ", ")
}

// usedMargin: what a running container's memory stays above its use by (a
// limit below it makes the kernel kill inside, init included).
const usedMargin = shrinkMargin >> 20

// ---- The node, as Proxmox VE lays it out ------------------------------------

// LocalNode reads the node's own files and changes guests with qm and pct.
type LocalNode struct {
	Name string // the node's name (its host name)
	// Root: where /etc/pve, /sys and /usr/sbin are (tests); "" = /.
	Root string
	// Run runs a command (default: os/exec).
	Run func(name string, args ...string) ([]byte, error)
}

func (n *LocalNode) path(p string) string { return filepath.Join(n.Root, p) }

func (n *LocalNode) confPath(kind, vmid string) string {
	dir := map[string]string{"qemu": "qemu-server", "lxc": "lxc"}[kind]
	return n.path(filepath.Join("/etc/pve/nodes", n.Name, dir, vmid+".conf"))
}

// ParseGuestConfig reads a guest config's current section: key: value lines
// up to the first [snapshot]; description comments are skipped.
func ParseGuestConfig(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			break
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

func (n *LocalNode) read(kind, vmid string) (NodeGuest, error) {
	b, err := os.ReadFile(n.confPath(kind, vmid))
	if err != nil {
		return NodeGuest{}, err
	}
	g := NodeGuest{VMID: vmid, Kind: kind, Config: ParseGuestConfig(b)}
	cg := n.path("/sys/fs/cgroup/qemu.slice/" + vmid + ".scope")
	if kind == "lxc" {
		cg = n.path("/sys/fs/cgroup/lxc/" + vmid)
	}
	if _, err := os.Stat(cg); err == nil {
		g.Running = true
		if kind == "lxc" {
			if b, err := os.ReadFile(filepath.Join(cg, "memory.current")); err == nil {
				v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
				g.MemoryUsedMB = int((v + (1 << 20) - 1) >> 20)
			}
		}
	}
	return g, nil
}

func (n *LocalNode) Guest(vmid string) (NodeGuest, error) {
	if g, err := n.read("qemu", vmid); err == nil {
		return g, nil
	}
	g, err := n.read("lxc", vmid)
	if err != nil {
		return NodeGuest{}, fmt.Errorf("no guest %s on node %s", vmid, n.Name)
	}
	return g, nil
}

func (n *LocalNode) Guests() ([]NodeGuest, error) {
	var out []NodeGuest
	for _, kind := range []string{"qemu", "lxc"} {
		files, err := filepath.Glob(n.confPath(kind, "*"))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if g, err := n.read(kind, strings.TrimSuffix(filepath.Base(f), ".conf")); err == nil {
				out = append(out, g)
			}
		}
	}
	return out, nil
}

func (n *LocalNode) cli(kind string) string {
	if kind == "lxc" {
		return n.path("/usr/sbin/pct")
	}
	return n.path("/usr/sbin/qm")
}

func (n *LocalNode) Shutdown(g NodeGuest, timeout time.Duration) error {
	_, err := n.Run(n.cli(g.Kind), "shutdown", g.VMID, "--timeout", strconv.Itoa(int(timeout.Seconds())), "--forceStop", "1")
	return err
}

func (n *LocalNode) Set(g NodeGuest, changes map[string]string) error {
	args := []string{"set", g.VMID}
	var del []string
	for _, k := range slices.Sorted(maps.Keys(changes)) {
		if v := changes[k]; v == "" {
			del = append(del, k)
		} else {
			args = append(args, "--"+k, v)
		}
	}
	if len(del) > 0 {
		args = append(args, "--delete", strings.Join(del, ","))
	}
	_, err := n.Run(n.cli(g.Kind), args...)
	return err
}
