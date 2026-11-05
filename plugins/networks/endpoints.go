package networks

// Endpoints: what a network reaches of the operator's own side.
//
// A network goes out to the web through its gateway; the operator's own side
// — their private addresses, the services the cloud's machines may use — it
// reaches through its endpoints and nothing else (AWS's gateway endpoints:
// a machine is never put on the people's VPN to reach one service).
//
// The operator publishes services per zone, in the plugin's settings:
//
//	zones:
//	  lab:
//	    services:
//	      - {name: logs, description: "the log store's push door", to: "192.0.2.35:3101"}
//	      - {name: syslog, to: "192.0.2.35:514/udp"}
//	    private: [192.0.2.0/24]   # their side; unsaid, the private ranges
//
// `private` is the operator's side: what a network of the zone reaches only
// by an endpoint. A service outside it is refused when the zone is opened —
// every network would reach it. A zone that publishes no service is as it
// always was: its gateways let out whatever its lane lets out.
//
// An endpoint (ep-…) is an owner's: one network of their OWN ("x-hangar-own":
// a network someone shares holds their machines, not their endpoints) may
// reach one service. Both are set at its birth. The network is not deleted
// while it holds one ("x-hangar-member"). Two dimensions bound it:
// networks.endpoints (how many) and the choice networks.services (which).
//
// The gateway enforces it, its archive's own doing: its network out to its
// endpoints, to the zone's resolvers and to the web; the rest of the private
// side refused, and counted on it. What a gateway lets through is carried in
// its engine's own record of it (Proxmox VE: its container's env), written at
// its birth and again at a change — a running gateway then started again,
// its network's way out cut for seconds. An endpoint is a PART of its
// network: the core hands a network's endpoints with it (Resource.members),
// and an endpoint's create and delete hold their network meanwhile — so what
// is written is always the whole of them, one writer at a time.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/tomblancdev/hangar/driver"
	"github.com/tomblancdev/hangar/sdk"
	"github.com/tomblancdev/hangar/sdk/pluginpb"
)

// Endpoint is an endpoint's desired state: set at its birth, both.
type Endpoint struct {
	Network string `json:"network"`
	Service string `json:"service"`
}

// EndpointObserved is an endpoint as its gateway carries it.
type EndpointObserved struct {
	// To: what it reaches, "192.0.2.35:3101/tcp".
	To string `json:"to,omitempty"`
	// From: what the service sees it as — its network's gateway on the lane,
	// the source of everything the network sends.
	From string `json:"from,omitempty"`
	// Applied: its gateway runs with it (a stopped one: will, at its start).
	Applied bool `json:"applied"`
}

const endpointSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["network", "service"],
  "x-hangar-summary": ["{service} from {network}", "to {to}", "as {from}"],
  "x-hangar-status": { "field": "applied", "on": "applied", "off": "pending" },
  "properties": {
    "network": { "type": "string", "x-hangar-ref": "network", "x-hangar-member": true, "x-hangar-own": true,
                 "description": "Your network that may reach the service: by name or id — one of your own, not one shared with you. Set at its birth." },
    "service": { "type": "string", "minLength": 1, "maxLength": 63,
                 "description": "A service the zone's operator publishes, by its name: hangar limits says which your tier opens. Set at its birth." }
  }
}`

// The private ranges: what a zone's private side is when its operator says
// none — RFC 1918, RFC 6598 (carrier-grade NAT) and link-local (a cloud's
// metadata door is there).
var defaultPrivate = []string{"10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16"} // no-environment: ok — the private ranges themselves, the product's default

// Settings are the operator's: per zone, what its networks may reach of
// their side.
type Settings struct {
	Zones map[string]ZoneSettings `json:"zones"`
}

// ZoneSettings: a zone's published services, and its private side.
type ZoneSettings struct {
	Services []Service `json:"services"`
	Private  []string  `json:"private"`
}

// Service is one the operator publishes.
type Service struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// To: address:port, and /udp for a datagram service (tcp unless said).
	To string `json:"to"`
}

// published is a zone's settings, read: its services by name, its side.
type published struct {
	services map[string]target
	private  []netip.Prefix
}

// target is where a service is.
type target struct {
	proto string
	addr  netip.Addr
	port  int
}

// line is a target as a gateway takes it: "tcp:192.0.2.35:3101".
func (t target) line() string { return fmt.Sprintf("%s:%s:%d", t.proto, t.addr, t.port) }

// words is a target as a person reads it: "192.0.2.35:3101/tcp".
func (t target) words() string { return fmt.Sprintf("%s:%d/%s", t.addr, t.port, t.proto) }

var serviceName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// parseTo reads a service's `to`.
func parseTo(v string) (target, error) {
	hostPort, proto, _ := strings.Cut(v, "/")
	if proto == "" {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return target{}, fmt.Errorf("%q: the protocol is tcp or udp", v)
	}
	ap, err := netip.ParseAddrPort(hostPort)
	if err != nil || !ap.Addr().Is4() || ap.Port() == 0 {
		return target{}, fmt.Errorf("%q: an IPv4 address and a port, such as 192.0.2.35:3101, then /udp for a datagram service", v)
	}
	return target{proto: proto, addr: ap.Addr(), port: int(ap.Port())}, nil
}

// parsePrivate reads a zone's private side: ranges, or single addresses.
func parsePrivate(list []string) ([]netip.Prefix, error) {
	if list == nil {
		list = defaultPrivate
	}
	var out []netip.Prefix
	for _, v := range list {
		p, err := netip.ParsePrefix(v)
		if err != nil {
			a, aerr := netip.ParseAddr(v)
			if aerr != nil {
				return nil, fmt.Errorf("private: %q: an IPv4 range, such as 192.0.2.0/24, or one address", v)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if !p.Addr().Is4() {
			return nil, fmt.Errorf("private: %q: an IPv4 range (a gateway carries IPv4 alone)", v)
		}
		out = append(out, p.Masked())
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(out), nil
}

// readZone checks a zone's settings, in the words the zone's refusal says.
func readZone(z ZoneSettings) (published, error) {
	pub := published{services: map[string]target{}}
	var err error
	if pub.private, err = parsePrivate(z.Private); err != nil {
		return pub, err
	}
	for i, s := range z.Services {
		switch {
		case !serviceName.MatchString(s.Name):
			return pub, fmt.Errorf("services[%d]: %q: a service's name — a-z, 0-9 and -", i, s.Name)
		case pub.services[s.Name] != (target{}):
			return pub, fmt.Errorf("services: %s twice", s.Name)
		case strings.ContainsAny(s.Description, "\n\r"):
			return pub, fmt.Errorf("services: %s: a description is one line", s.Name)
		}
		t, err := parseTo(s.To)
		if err != nil {
			return pub, fmt.Errorf("services: %s: %v", s.Name, err)
		}
		if !slices.ContainsFunc(pub.private, func(p netip.Prefix) bool { return p.Contains(t.addr) }) {
			return pub, fmt.Errorf("services: %s at %s is outside the zone's private side: every network would reach it", s.Name, t.addr)
		}
		pub.services[s.Name] = t
	}
	return pub, nil
}

// publishes: the zone publishes services — its gateways carry endpoints.
func (z published) publishes() bool { return len(z.services) > 0 }

// names are the zone's services, sorted.
func (z published) names() []string {
	out := make([]string, 0, len(z.services))
	for n := range z.services {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// endpointsFor is what a network's gateway lets through, from its endpoints
// as the core hands them (Resource.members): nil where the zone publishes
// nothing. An endpoint to a service the zone no longer publishes reaches
// nothing.
func (p *Plugin) endpointsFor(zone string, members []*pluginpb.Resource) *driver.Endpoints {
	p.mu.RLock()
	z, ok := p.published[zone]
	p.mu.RUnlock()
	if !ok || !z.publishes() {
		return nil
	}
	e := &driver.Endpoints{To: []string{}}
	for _, pr := range z.private {
		e.Private = append(e.Private, pr.String())
	}
	for _, m := range members {
		if m.GetType() != "endpoint" {
			continue
		}
		var s Endpoint
		if sdk.Decode(m.GetSpec(), &s) != nil {
			continue
		}
		if t, ok := z.services[s.Service]; ok {
			e.To = append(e.To, t.line())
		}
	}
	slices.Sort(e.To)
	e.To = slices.Compact(e.To)
	return e
}

// zoneOf is a zone's settings as read when it was opened.
func (p *Plugin) zoneOf(zone string) published {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.published[zone]
}

// ---- The endpoint type --------------------------------------------------------

func (p *Plugin) planEndpoint(req *pluginpb.PlanRequest) (*pluginpb.PlanResponse, error) {
	if req.GetAction() != "" {
		return nil, sdk.Refuse("an endpoint has no action %q: it is made, and deleted", req.GetAction())
	}
	if _, err := p.networks(req.GetZone()); err != nil {
		return nil, err
	}
	var s Endpoint
	if err := sdk.Decode(req.GetSpec(), &s); err != nil {
		return nil, err
	}
	resp := &pluginpb.PlanResponse{Spec: sdk.JSON(s), Usage: map[string]int64{"networks.endpoints": 1},
		Choices: map[string]string{"networks.services": s.Service}}
	z := p.zoneOf(req.GetZone())
	if _, ok := z.services[s.Service]; !ok {
		why := fmt.Sprintf("zone %s publishes no service", req.GetZone())
		if z.publishes() {
			why = fmt.Sprintf("zone %s publishes no service %s; it publishes %s", req.GetZone(), s.Service, strings.Join(z.names(), ", "))
		}
		resp.Refusals = append(resp.Refusals, &pluginpb.Refusal{Field: "/service", Reason: why})
	}
	return resp, nil
}

// planEndpointChange: an endpoint is its network and its service, both set
// at its birth — another service is another endpoint.
func planEndpointChange(req *pluginpb.PlanChangeRequest) (*pluginpb.PlanChangeResponse, error) {
	var was, want Endpoint
	if err := sdk.Decode(req.GetCurrent().GetSpec(), &was); err != nil {
		return nil, err
	}
	if err := sdk.Decode(req.GetSpec(), &want); err != nil {
		return nil, err
	}
	out := &pluginpb.PlanChangeResponse{}
	if want.Network != was.Network {
		out.Fixed = append(out.Fixed, sdk.Fixed("/network", was.Network, "an endpoint's network"))
	}
	if want.Service != was.Service {
		out.Fixed = append(out.Fixed, sdk.Fixed("/service", was.Service, "an endpoint's service (another service is another endpoint)"))
	}
	return out, nil
}

// itsNetwork is the network an endpoint's create or delete is handed, with
// its endpoints; nil: none (gone).
func itsNetwork(s Endpoint, refs []*pluginpb.Resource) *pluginpb.Resource {
	for _, r := range refs {
		if r.GetId() == s.Network && r.GetType() == "network" {
			return r
		}
	}
	return nil
}

// write brings a network's gateway to what its endpoints are now: written,
// and a running gateway started again.
func (p *Plugin) write(ctx context.Context, n driver.Networks, network *pluginpb.Resource) (driver.Network, []string, error) {
	var ns Spec
	if err := sdk.Decode(network.GetSpec(), &ns); err != nil {
		return driver.Network{}, nil, err
	}
	return n.TendNetwork(ctx, p.specOf(network, ns), wasAt(network))
}

// observeEndpoint reads what an endpoint's gateway carries of it.
func (p *Plugin) observeEndpoint(zone string, s Endpoint, got driver.Network) EndpointObserved {
	o := EndpointObserved{From: got.Lane}
	if t, ok := p.zoneOf(zone).services[s.Service]; ok {
		o.To = t.words()
		o.Applied = slices.Contains(got.Endpoints, t.line())
	}
	return o
}

func (p *Plugin) createEndpoint(ctx context.Context, req *pluginpb.CreateRequest) (*pluginpb.CreateResponse, error) {
	r := req.GetResource()
	n, err := p.networks(r.GetZone())
	if err != nil {
		return nil, err
	}
	var s Endpoint
	if err := sdk.Decode(r.GetSpec(), &s); err != nil {
		return nil, err
	}
	network := itsNetwork(s, req.GetRefs())
	if network == nil {
		return nil, sdk.NotNow("its network %s is gone", s.Network)
	}
	got, _, err := p.write(ctx, n, network)
	if err != nil {
		return nil, engineErr(err)
	}
	o := p.observeEndpoint(r.GetZone(), s, got)
	return &pluginpb.CreateResponse{Observed: sdk.JSON(o),
		Events: []*pluginpb.Event{sdk.Event("endpoint.created", "", map[string]string{"network": s.Network, "service": s.Service, "to": o.To})}}, nil
}

func (p *Plugin) deleteEndpoint(ctx context.Context, req *pluginpb.DeleteRequest) (*pluginpb.DeleteResponse, error) {
	r := req.GetResource()
	n, err := p.networks(r.GetZone())
	if err != nil {
		return nil, err
	}
	var s Endpoint
	if err := sdk.Decode(r.GetSpec(), &s); err != nil {
		return nil, err
	}
	// handed without it: what its gateway carries is the rest of them
	if network := itsNetwork(s, req.GetRefs()); network != nil {
		if _, _, err := p.write(ctx, n, network); err != nil && !errors.Is(err, driver.ErrNotFound) {
			return nil, engineErr(err)
		}
	}
	return &pluginpb.DeleteResponse{Events: []*pluginpb.Event{sdk.Event("endpoint.deleted", "", map[string]string{"network": s.Network, "service": s.Service})}}, nil
}

// reconcileEndpoint reads what its gateway carries of it — the network's own
// look writes it again when it differs; an endpoint keeps nothing of its own
// on the engine.
func (p *Plugin) reconcileEndpoint(ctx context.Context, req *pluginpb.ReconcileRequest) (*pluginpb.ReconcileResponse, error) {
	r := req.GetResource()
	n, err := p.networks(r.GetZone())
	if err != nil {
		return nil, err
	}
	var s Endpoint
	if err := sdk.Decode(r.GetSpec(), &s); err != nil {
		return nil, err
	}
	got, err := n.Network(ctx, s.Network)
	if errors.Is(err, driver.ErrNotFound) {
		return &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_DRIFTED, Observed: sdk.JSON(EndpointObserved{}),
			Detail: "its network's gateway is not on the engine: its network's look makes it again"}, nil
	}
	if err != nil {
		return nil, engineErr(err)
	}
	o := p.observeEndpoint(r.GetZone(), s, got)
	if o.To == "" {
		return &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_DRIFTED, Observed: sdk.JSON(o),
			Detail: fmt.Sprintf("zone %s publishes no service %s any more: it reaches nothing", r.GetZone(), s.Service)}, nil
	}
	return &pluginpb.ReconcileResponse{Drift: pluginpb.Drift_DRIFT_IN_SYNC, Observed: sdk.JSON(o)}, nil
}

// whyNoEndpoints says why a zone that publishes services cannot carry them.
func whyNoEndpoints(d driver.Driver) string {
	if r, ok := d.(interface{ EndpointsReport() string }); ok && r.EndpointsReport() != "" {
		return r.EndpointsReport()
	}
	return "its driver cannot carry endpoints there (" + driver.NetEndpoints + ")"
}
