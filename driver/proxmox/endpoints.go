package proxmox

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/tomblancdev/hangar/driver"
)

// Endpoints (net.endpoints): what a network's gateway lets through to the
// zone's private side, carried in the gateway's own config — a container's
// `env` (Proxmox VE 9.1 and later), which Proxmox keeps as
// lxc.environment.runtime lines and hands to the container's first process
// at every start. Three variables, read by the archive's own unit
// (tools/gateway, recipe 2) after nftables starts; the gateway forwards
// nothing until they stand:
//
//	HANGAR_ENDPOINTS=tcp:192.0.2.35:3101 udp:192.0.2.35:514   its endpoints
//	HANGAR_RESOLVERS=192.0.2.1                                the zone's resolvers: port 53
//	HANGAR_PRIVATE=192.0.2.0/24                               the zone's private side, refused
//
// A zone that publishes nothing writes none, and its gateways are as they
// were. A gateway is born with them; a change is written again and a running
// gateway restarted — its network's way out cut for seconds (7.4 s on the
// bench): a line written to a running container waits in its PENDING config
// until its next start, so the driver reads both, what is written and what
// it runs with. The field falls to VM.Config.Options, which the networks
// role holds already: no privilege more.

// Env's variables, as recipe 2 reads them.
const (
	envEndpoints = "HANGAR_ENDPOINTS"
	envResolvers = "HANGAR_RESOLVERS"
	envPrivate   = "HANGAR_PRIVATE"
)

// endpointsRecipe is the gateway archive's first recipe that reads them.
const endpointsRecipe = 2

// recipeOf reads the recipe an archive is named after, as tools/gateway
// advises (hangar-gateway-<version>.tar.zst); 0: a name that says none.
var recipeName = regexp.MustCompile(`^hangar-gateway-([0-9]+)(?:[.-]|$)`)

func recipeOf(archive string) int {
	_, file, _ := strings.Cut(archive, ":")
	if file == "" {
		file = archive
	}
	m := recipeName.FindStringSubmatch(path.Base(file))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// atLeast reads a Proxmox VE version ("9.2.25") against major.minor.
func atLeast(version string, major, minor int) bool {
	f := strings.SplitN(version, ".", 3)
	if len(f) < 2 {
		return false
	}
	a, err1 := strconv.Atoi(f[0])
	b, err2 := strconv.Atoi(f[1])
	return err1 == nil && err2 == nil && (a > major || a == major && b >= minor)
}

// endpointsWhy says why a zone that cuts networks carries no endpoints ("":
// it does, or cuts none).
func (d *Driver) endpointsWhy(version string) string {
	switch {
	case !d.nets.on():
		return ""
	case !atLeast(version, 9, 1):
		return fmt.Sprintf("its Proxmox VE is %s: a container's env, where a gateway carries its endpoints, is 9.1's", version)
	case recipeOf(d.nets.archive) < endpointsRecipe:
		return fmt.Sprintf("its gateways' archive %s is not named after a recipe that reads endpoints (hangar-gateway-<version>, %d or later: tools/gateway/build.sh)",
			d.nets.archive, endpointsRecipe)
	}
	return ""
}

// EndpointsReport says why net.endpoints is not advertised ("" when it is,
// or the zone cuts no networks).
func (d *Driver) EndpointsReport() string { return d.endpointsErr }

// envOf is what a gateway's env holds for its endpoints: nil for none.
func (d *Driver) envOf(e *driver.Endpoints) map[string]string {
	if e == nil {
		return nil
	}
	env := map[string]string{envPrivate: strings.Join(e.Private, " ")}
	if len(e.To) > 0 {
		env[envEndpoints] = strings.Join(e.To, " ")
	}
	var res []string
	for _, a := range d.lane.resolvers {
		if a.Is4() {
			res = append(res, a.String())
		}
	}
	if len(res) > 0 {
		env[envResolvers] = strings.Join(res, " ")
	}
	return env
}

// envLine is an env as the API takes it: NUL-separated, in a fixed order.
func envLine(env map[string]string) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return strings.Join(out, "\x00")
}

// parseEnv reads an env as the API gives it back (NUL-separated).
func parseEnv(v string) map[string]string {
	if v == "" {
		return nil
	}
	out := map[string]string{}
	for _, kv := range strings.Split(v, "\x00") {
		if k, val, ok := strings.Cut(kv, "="); ok && k != "" {
			out[k] = val
		}
	}
	return out
}

// envs reads a gateway's env twice: what it runs with, and what is written
// — the same but where a line written to it while it ran waits for its next
// start.
func (d *Driver) envs(ctx context.Context, r resource) (running, written map[string]string, err error) {
	var rows []struct {
		Key     string `json:"key"`
		Value   any    `json:"value"`
		Pending any    `json:"pending"`
		Delete  any    `json:"delete"`
	}
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/pending", nil, &rows); err != nil {
		return nil, nil, err
	}
	for _, row := range rows {
		if row.Key != "env" {
			continue
		}
		running = parseEnv(str(row.Value))
		written = running
		switch {
		case row.Pending != nil:
			written = parseEnv(str(row.Pending))
		case row.Delete != nil && str(row.Delete) != "0":
			written = nil
		}
	}
	return running, written, nil
}

// endpointsOf reads, from an env, the endpoints it lets through.
func endpointsOf(env map[string]string) []string {
	return strings.Fields(env[envEndpoints])
}

// endpointsReady brings a gateway's env to its network's endpoints: written
// when it differs, and a running gateway started again so that it runs with
// them. It says what it did.
func (d *Driver) endpointsReady(ctx context.Context, r resource, s driver.NetworkSpec) ([]string, error) {
	want := d.envOf(s.Endpoints)
	if !slices.Contains(d.caps, driver.NetEndpoints) {
		if want != nil {
			return nil, fmt.Errorf("%w: zone %s's gateways carry no endpoints: %s", driver.ErrRefused, d.zone, d.endpointsErr)
		}
		return nil, nil // nothing is written where nothing can be
	}
	running, written, err := d.envs(ctx, r)
	if err != nil {
		return nil, err
	}
	var fixed []string
	if !maps.Equal(written, want) {
		p := url.Values{"env": {envLine(want)}}
		if want == nil {
			p = url.Values{"delete": {"env"}}
		}
		if err := d.c.call(ctx, http.MethodPut, r.path()+"/config", p, nil); err != nil {
			return nil, fmt.Errorf("writing its endpoints: %w", err)
		}
		fixed = append(fixed, "endpoints")
		if running, _, err = d.envs(ctx, r); err != nil {
			return fixed, err
		}
	}
	if maps.Equal(running, want) {
		return fixed, nil
	}
	// written, and not what it runs with: started again, if it runs
	on, err := d.running(ctx, r)
	if err != nil || !on {
		return fixed, err
	}
	if err := d.c.run(ctx, http.MethodPost, r.path()+"/status/reboot", url.Values{"timeout": {"30"}}); err != nil {
		if on, rerr := d.running(ctx, r); rerr == nil && !on {
			return fixed, nil // stopped meanwhile: it takes them at its next start
		}
		return fixed, fmt.Errorf("starting it again with its endpoints: %w", err)
	}
	if len(fixed) == 0 {
		fixed = append(fixed, "endpoints (started again: it ran without them)")
	}
	return fixed, nil
}

// runsWith reads the endpoints a gateway runs with; none where the zone
// carries none.
func (d *Driver) runsWith(ctx context.Context, r resource) ([]string, error) {
	if !slices.Contains(d.caps, driver.NetEndpoints) {
		return nil, nil
	}
	running, _, err := d.envs(ctx, r)
	if err != nil {
		return nil, err
	}
	return endpointsOf(running), nil
}
