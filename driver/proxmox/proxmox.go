// Package proxmox drives Proxmox VE through its HTTP API, with one API token
// fenced to one pool.
//
// Everything the driver makes lives in the zone's POOL and carries the
// core's id as a tag (hangar-id.<id>); a guest outside the pool, or in it
// without that tag, does not exist for it. It creates containers from a
// template archive (`pct create`, keys given at birth) and VMs by cloning a
// template VM found by name in the IMAGES POOL, then hands the VM its first
// boot through a NoCloud seed disc it uploads (seed.go). Each call that
// starts a task waits for the task and reads how it ended: a start refused by
// a hook answers 200 and fails inside its task.
//
// Zone options:
//
//	node            the node guests are made on                    (required)
//	pool            the pool they are made in — the fence         (required)
//	images_pool     where VM templates are found by name           (default: pool)
//	storage         where their disks go                           (required)
//	seed_storage    a storage of its own for VMs' seed discs (iso) (required)
//	bridge          the bridge or SDN vnet their network joins     (required)
//	vlan            a VLAN tag on it                               (optional)
//	vmids           the ids this driver may take, "11000-11099"    (required)
//	full_clone      "true": full clones even beside the template
//	shelf_archive   a container archive to make volumes' container shelves
//	                from (volumes.go); without one, a filesystem volume
//	                lives on a container only
//	shutdown_timeout how long a guest is asked to shut down before it is
//	                made to, in seconds                            (default 60)
//	ca_file         a CA bundle to verify the API's certificate
//	fingerprint     or the certificate's SHA-256, pinned
//	tls_server_name the name to verify, when it is not the endpoint's
//
// The credential is the token: `user@realm!name=secret`. What it must be
// allowed to do, and nothing more, is written in docs/proxmox.md. The one
// thing it may do outside its pools is read the power of the guests the
// zone's reservations watch (VM.Audit on each, and nothing else there).
//
// The tags a guest carries are the contract with the node's hook
// (cmd/hangar-hook), which acts on them when the brain cannot be reached:
// hangar-id.<id>, the plugin's key.value tags (class, floor, beside,
// admitted), and held.<key> for each reservation holding its room back.
package proxmox

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tomblancdev/hangar/driver"
)

// Name is the name the driver registers under.
const Name = "proxmox"

func init() { driver.Register(Name, Open) }

// idTag is the tag that carries the core's id on a guest.
const idTag = "hangar-id"

// HeldTag is the key of a hold's tag: held.<reservation key>.
const HeldTag = "held"

// Driver is one Proxmox zone.
type Driver struct {
	c        *client
	zone     string
	node     string
	pool     string
	images   string
	storage  string
	seeds    string
	bridge   string
	vlan     int
	lo, hi   int
	full     bool
	shutdown int // seconds a shutdown is waited for before a stop
	watch    []string
	caps     []driver.Capability
	fenceErr string // why fence.pool is not advertised, when it is not

	mu sync.Mutex // one create at a time: a VMID is picked, then taken
	// volumes: one volume change at a time (a free key is picked, then taken)
	vmu          sync.Mutex
	shelfArchive string
}

// Open opens a zone: it checks the options, reaches the API with the token,
// and reads what the token may do (fence.pool is advertised only when it can
// act on nothing outside its pools).
func Open(ctx context.Context, p driver.Params) (driver.Driver, error) {
	o := p.Options
	d := &Driver{
		zone: p.Zone, node: o["node"], pool: o["pool"], images: o["images_pool"], storage: o["storage"],
		seeds: o["seed_storage"], bridge: o["bridge"], full: o["full_clone"] == "true", shutdown: 60,
		shelfArchive: o["shelf_archive"],
	}
	for _, ref := range p.Watch {
		if n, err := strconv.Atoi(ref); err != nil || n < 100 {
			return nil, fmt.Errorf("proxmox zone %s: a watched guest is named by its VMID, not %q", p.Zone, ref)
		}
		d.watch = append(d.watch, ref)
	}
	if v := o["shutdown_timeout"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 3600 {
			return nil, fmt.Errorf("proxmox zone %s: shutdown_timeout %q: 1 to 3600 seconds", p.Zone, v)
		}
		d.shutdown = n
	}
	var missing []string
	for _, k := range []string{"node", "pool", "storage", "seed_storage", "bridge", "vmids"} {
		if o[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("proxmox zone %s: options %s are required", p.Zone, strings.Join(missing, ", "))
	}
	if d.images == "" {
		d.images = d.pool
	}
	if v := o["vlan"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 4094 {
			return nil, fmt.Errorf("proxmox zone %s: vlan %q: 1 to 4094", p.Zone, v)
		}
		d.vlan = n
	}
	lo, hi, ok := strings.Cut(o["vmids"], "-")
	d.lo, _ = strconv.Atoi(strings.TrimSpace(lo))
	d.hi, _ = strconv.Atoi(strings.TrimSpace(hi))
	if !ok || d.lo < 100 || d.hi < d.lo || d.hi > 999999999 {
		return nil, fmt.Errorf("proxmox zone %s: vmids %q: a range such as 11000-11099", p.Zone, o["vmids"])
	}
	c, err := newClient(p.Endpoint, p.Credential, o)
	if err != nil {
		return nil, fmt.Errorf("proxmox zone %s: %w", p.Zone, err)
	}
	d.c = c
	var ver struct {
		Version string `json:"version"`
	}
	if err := c.call(ctx, http.MethodGet, "/version", nil, &ver); err != nil {
		return nil, fmt.Errorf("proxmox zone %s: %w", p.Zone, err)
	}
	d.caps = []driver.Capability{driver.KindContainer, driver.KindVM, driver.GuestTags, driver.ResizeLiveMemoryDown,
		driver.ResizeLiveCPUCap, driver.HookPreStart, driver.VolumeMoveBetweenGuests}
	why, err := d.fence(ctx)
	if err != nil {
		return nil, fmt.Errorf("proxmox zone %s: reading what the token may do: %w", p.Zone, err)
	}
	if why == "" {
		d.caps = append(d.caps, driver.FencePool)
	}
	d.fenceErr = why
	return d, nil
}

func (d *Driver) Capabilities() []driver.Capability { return slices.Clone(d.caps) }
func (d *Driver) Close() error                      { return nil }

// FenceReport says why fence.pool is not advertised ("" when it is).
func (d *Driver) FenceReport() string { return d.fenceErr }

// fence reads the token's own permissions and returns why it is NOT fenced
// to its pools: a privilege on a guest path outside them — but VM.Audit on a
// guest the zone watches, which reads its power and nothing more — or one
// that could widen its own rights.
func (d *Driver) fence(ctx context.Context) (string, error) {
	var perms map[string]map[string]int
	if err := d.c.call(ctx, http.MethodGet, "/access/permissions", nil, &perms); err != nil {
		return "", err
	}
	members, err := d.poolMembers(ctx)
	if err != nil {
		return "", err
	}
	var why []string
	paths := make([]string, 0, len(perms))
	for p := range perms {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		for priv := range perms[path] {
			switch {
			case priv == "Permissions.Modify" || priv == "Sys.Modify" || priv == "User.Modify" || priv == "Realm.Allocate":
				why = append(why, fmt.Sprintf("%s on %s", priv, path))
			case strings.HasPrefix(priv, "VM."):
				if path == "/pool/"+d.pool || path == "/pool/"+d.images {
					continue
				}
				if v, ok := strings.CutPrefix(path, "/vms/"); ok {
					if id, err := strconv.Atoi(v); err == nil && members[id] {
						continue
					}
					if priv == "VM.Audit" && slices.Contains(d.watch, v) {
						continue
					}
				}
				why = append(why, fmt.Sprintf("%s on %s", priv, path))
			}
		}
	}
	if len(why) == 0 {
		return "", nil
	}
	sort.Strings(why)
	if len(why) > 4 {
		why = append(why[:4], fmt.Sprintf("and %d more", len(why)-4))
	}
	return "the token reaches beyond pools " + d.pool + " and " + d.images + ": " + strings.Join(why, ", "), nil
}

// poolMembers are the VMIDs in the fence's two pools.
func (d *Driver) poolMembers(ctx context.Context) (map[int]bool, error) {
	rs, err := d.resources(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for _, r := range rs {
		if r.Pool == d.pool || r.Pool == d.images {
			out[r.VMID] = true
		}
	}
	return out, nil
}

// ---- The guests, as the cluster lists them ----------------------------------

type resource struct {
	VMID     int     `json:"vmid"`
	Node     string  `json:"node"`
	Type     string  `json:"type"` // qemu, lxc
	Name     string  `json:"name"`
	Status   string  `json:"status"` // pvestatd's, seconds behind: never decide on it
	Pool     string  `json:"pool"`
	Tags     string  `json:"tags"`
	Template int     `json:"template"`
	MaxMem   int64   `json:"maxmem"`
	MaxCPU   float64 `json:"maxcpu"`
	MaxDisk  int64   `json:"maxdisk"`
}

func (d *Driver) resources(ctx context.Context) ([]resource, error) {
	var rs []resource
	err := d.c.call(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {"vm"}}, &rs)
	return rs, err
}

func (r resource) path() string {
	return "/nodes/" + url.PathEscape(r.Node) + "/" + r.Type + "/" + strconv.Itoa(r.VMID)
}

func (r resource) kind() string {
	if r.Type == "lxc" {
		return "container"
	}
	return "vm"
}

// tagList splits Proxmox's tag string (it answers with ";", accepts more).
func tagList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' || r == ' ' })
}

// guestID is the core's id a guest carries, or "".
func guestID(tags string) string {
	for _, t := range tagList(tags) {
		if v, ok := strings.CutPrefix(t, idTag+"."); ok {
			return v
		}
	}
	return ""
}

// find locates a guest by the core's id: its tag, or — for a guest whose
// create stopped before its tags were written — the marker its description
// carries from birth (a pool-fenced token cannot tag a guest in the call that
// makes it: Proxmox checks tags on /vms/<id>, which is not in the pool yet).
func (d *Driver) find(ctx context.Context, id string) (resource, error) {
	if id == "" {
		return resource{}, driver.ErrNotFound // an untagged guest carries "" too
	}
	rs, err := d.resources(ctx)
	if err != nil {
		return resource{}, err
	}
	var untagged []resource
	for _, r := range rs {
		if r.Pool != d.pool || r.Template != 0 {
			continue
		}
		switch guestID(r.Tags) {
		case id:
			return r, nil
		case "":
			untagged = append(untagged, r)
		}
	}
	for _, r := range untagged {
		var cfg map[string]any
		if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err == nil && strings.TrimSpace(str(cfg["description"])) == marker(id) {
			return r, nil
		}
	}
	return resource{}, driver.ErrNotFound
}

// marker is a guest's description from birth: the id, before its tag.
func marker(id string) string { return "made by hangar: " + id }

// ---- Tags -------------------------------------------------------------------

var tagPart = regexp.MustCompile(`^[a-z0-9_+-]+$`)
var tagValue = regexp.MustCompile(`^[a-z0-9_+.-]+$`)

// tags writes a guest's tags: the id's, then key.value for each of the
// plugin's (a key has no dot; the value may), then held.<key> per hold.
func tags(id string, m map[string]string, holds []string) (string, error) {
	out := []string{idTag + "." + id}
	for k, v := range m {
		if k == idTag || k == HeldTag || !tagPart.MatchString(k) || !tagValue.MatchString(v) {
			return "", fmt.Errorf("%w: tag %s=%s: Proxmox tags hold a-z, 0-9, _ + - and a dot in the value (and %s, %s are the driver's)",
				driver.ErrRefused, k, v, idTag, HeldTag)
		}
		out = append(out, k+"."+v)
	}
	for _, h := range holds {
		if !tagPart.MatchString(h) {
			return "", fmt.Errorf("%w: hold %q: a-z, 0-9, _ + -", driver.ErrRefused, h)
		}
		out = append(out, HeldTag+"."+h)
	}
	sort.Strings(out)
	return strings.Join(slices.Compact(out), ";"), nil
}

func tagMap(s string) map[string]string {
	m := map[string]string{}
	for _, t := range tagList(s) {
		if k, v, ok := strings.Cut(t, "."); ok && k != idTag && k != HeldTag {
			m[k] = v
		}
	}
	return m
}

// holdsOf reads the holds a guest's tags carry, sorted.
func holdsOf(s string) []string {
	var out []string
	for _, t := range tagList(s) {
		if v, ok := strings.CutPrefix(t, HeldTag+"."); ok {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// ---- Reading one guest ------------------------------------------------------

// read builds the Guest from its config and status.
func (d *Driver) read(ctx context.Context, r resource, id string) (driver.Guest, error) {
	var cfg map[string]any
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
		return driver.Guest{}, d.engine(err)
	}
	var st struct {
		Status string `json:"status"`
		Mem    int64  `json:"mem"`
	}
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/status/current", nil, &st); err != nil {
		return driver.Guest{}, d.engine(err)
	}
	g := driver.Guest{
		ID: guestID(str(cfg["tags"])), EngineRef: fmt.Sprintf("%s/%s/%d", r.Node, r.Type, r.VMID),
		Kind: r.kind(), Node: r.Node, Running: st.Status == "running", Tags: tagMap(str(cfg["tags"])),
		Holds: holdsOf(str(cfg["tags"])), Cores: num(cfg["cores"]), MemoryMB: memoryMB(cfg["memory"]),
		CPULimit: int(math.Ceil(fnum(cfg["cpulimit"]))),
	}
	if g.Running {
		g.MemoryUsedMB = int((st.Mem + (1 << 20) - 1) >> 20)
	}
	if g.ID == "" {
		g.ID = id // found by its marker, before its tags
	}
	if g.Cores == 0 {
		g.Cores = 1 // Proxmox's default, not written in the config
	}
	if r.Type == "lxc" {
		g.Name = str(cfg["hostname"])
		g.DiskGB = sizeGB(str(cfg["rootfs"]))
	} else {
		g.Name = r.Name
		if disk := bootDisk(cfg); disk != "" {
			g.DiskGB = sizeGB(str(cfg[disk]))
		}
	}
	if g.Running {
		g.Addresses = d.addresses(ctx, r)
	}
	return g, nil
}

// addresses are the guest's own addresses where the engine can read them: a
// container's interfaces, a VM's through its guest agent (none without one).
func (d *Driver) addresses(ctx context.Context, r resource) []string {
	var out []string
	keep := func(s string) {
		ip := net.ParseIP(strings.Split(s, "/")[0])
		if ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !slices.Contains(out, ip.String()) {
			out = append(out, ip.String())
		}
	}
	if r.Type == "lxc" {
		var ifs []struct {
			Name  string `json:"name"`
			Inet  string `json:"inet"`
			Inet6 string `json:"inet6"`
		}
		if d.c.call(ctx, http.MethodGet, r.path()+"/interfaces", nil, &ifs) == nil {
			for _, i := range ifs {
				for _, a := range []string{i.Inet, i.Inet6} {
					if a != "" {
						keep(a)
					}
				}
			}
		}
		return out
	}
	var ans struct {
		Result []struct {
			Name string `json:"name"`
			IPs  []struct {
				Address string `json:"ip-address"`
			} `json:"ip-addresses"`
		} `json:"result"`
	}
	if d.c.call(ctx, http.MethodGet, r.path()+"/agent/network-get-interfaces", nil, &ans) == nil {
		for _, i := range ans.Result {
			for _, a := range i.IPs {
				keep(a.Address)
			}
		}
	}
	return out
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func num(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func fnum(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		n, _ := strconv.ParseFloat(x, 64)
		return n
	}
	return 0
}

// memoryMB reads "memory": a number, or "current=…,…" on a VM.
func memoryMB(v any) int {
	if s, ok := v.(string); ok {
		for _, part := range strings.Split(s, ",") {
			k, val, found := strings.Cut(part, "=")
			if !found {
				val = k
			} else if k != "current" {
				continue
			}
			n, _ := strconv.Atoi(val)
			return n
		}
	}
	return num(v)
}

// sizeGB reads the size= of a disk line ("local-zfs:vm-1-disk-0,size=8G").
func sizeGB(line string) int {
	for _, part := range strings.Split(line, ",") {
		if v, ok := strings.CutPrefix(part, "size="); ok {
			unit := v[len(v)-1]
			n, err := strconv.ParseFloat(v[:len(v)-1], 64)
			if err != nil {
				return 0
			}
			switch unit {
			case 'T':
				n *= 1024
			case 'M':
				n /= 1024
			case 'K':
				n /= 1024 * 1024
			}
			return int(n + 0.5)
		}
	}
	return 0
}

// bootDisk is the key of a VM's first boot disk.
func bootDisk(cfg map[string]any) string {
	boot := str(cfg["boot"])
	if order, ok := strings.CutPrefix(boot, "order="); ok {
		for _, dev := range strings.Split(order, ";") {
			if l := str(cfg[dev]); l != "" && !strings.Contains(l, "media=cdrom") {
				return dev
			}
		}
	}
	for _, bus := range []string{"scsi", "virtio", "sata", "ide"} {
		for i := range 31 {
			k := bus + strconv.Itoa(i)
			if l := str(cfg[k]); l != "" && !strings.Contains(l, "media=cdrom") {
				return k
			}
		}
	}
	return ""
}

// ---- The guests facet -------------------------------------------------------

func (d *Driver) Guest(ctx context.Context, id string) (driver.Guest, error) {
	r, err := d.find(ctx, id)
	if err != nil {
		return driver.Guest{}, d.engine(err)
	}
	return d.read(ctx, r, id)
}

func (d *Driver) Guests(ctx context.Context) ([]driver.Guest, error) {
	rs, err := d.resources(ctx)
	if err != nil {
		return nil, d.engine(err)
	}
	var out []driver.Guest
	for _, r := range rs {
		if r.Pool != d.pool || r.Template != 0 || guestID(r.Tags) == "" {
			continue
		}
		g, err := d.read(ctx, r, "")
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Traits: a container changes everything live (its memory down only above
// what it holds — Resize checks) and takes no user data; a VM grows its
// memory live (hot-plugged), changes its cores at a cold start, and boots
// its user data from the seed disc.
func (d *Driver) Traits(kind string) driver.Traits {
	if kind == "container" {
		return driver.Traits{LiveCores: true, LiveMemoryUp: true, LiveMemoryDown: true}
	}
	return driver.Traits{UserData: true, LiveMemoryUp: true}
}

func (d *Driver) CreateGuest(ctx context.Context, s driver.GuestSpec) (driver.Guest, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s.Name == "" {
		s.Name = s.ID
	}
	tagStr, err := tags(s.ID, s.Tags, s.Holds)
	if err != nil {
		return driver.Guest{}, err
	}
	r, err := d.find(ctx, s.ID)
	switch {
	case err == nil: // a retry: finish what the first call began
	case errors.Is(err, driver.ErrNotFound):
		vmid, err := d.freeVMID(ctx)
		if err != nil {
			return driver.Guest{}, d.engine(err)
		}
		switch s.Kind {
		case "container":
			err = d.createContainer(ctx, vmid, s)
		case "vm":
			err = d.cloneVM(ctx, vmid, s)
		default:
			err = fmt.Errorf("%w: no kind %q", driver.ErrRefused, s.Kind)
		}
		if err != nil {
			return driver.Guest{}, d.engine(err)
		}
		if r, err = d.find(ctx, s.ID); err != nil {
			return driver.Guest{}, d.engine(err)
		}
	default:
		return driver.Guest{}, d.engine(err)
	}
	if r.Type == "qemu" {
		err = d.configureVM(ctx, r, s, tagStr)
	} else if r.Tags != tagStr {
		err = d.c.call(ctx, http.MethodPut, r.path()+"/config", url.Values{"tags": {tagStr}}, nil)
	}
	if err == nil {
		// capped before its first start: a machine born under a hold never
		// runs a second uncapped
		err = d.cpuLimit(ctx, r, s.CPULimit)
	}
	if err != nil {
		return driver.Guest{}, d.engine(err)
	}
	if !s.Stopped {
		if err := d.power(ctx, r, true); err != nil {
			return driver.Guest{}, d.engine(err)
		}
	}
	return d.read(ctx, r, s.ID)
}

// freeVMID is the lowest id of the range that no guest holds — those the
// fence hides included: the cluster is asked about each candidate.
func (d *Driver) freeVMID(ctx context.Context) (int, error) {
	rs, err := d.resources(ctx)
	if err != nil {
		return 0, err
	}
	seen := map[int]bool{}
	for _, r := range rs {
		seen[r.VMID] = true
	}
	for id := d.lo; id <= d.hi; id++ {
		if seen[id] {
			continue
		}
		var got any
		err := d.c.call(ctx, http.MethodGet, "/cluster/nextid", url.Values{"vmid": {strconv.Itoa(id)}}, &got)
		if err == nil {
			return id, nil
		}
		var ae *apiError
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
			return 0, err
		}
	}
	return 0, fmt.Errorf("%w: every id of %d-%d is taken", driver.ErrRefused, d.lo, d.hi)
}

func (d *Driver) net0(container bool) string {
	var b strings.Builder
	if container {
		b.WriteString("name=eth0,ip=dhcp,")
	} else {
		b.WriteString("virtio,")
	}
	b.WriteString("bridge=" + d.bridge)
	if d.vlan > 0 {
		fmt.Fprintf(&b, ",tag=%d", d.vlan)
	}
	return b.String()
}

func (d *Driver) createContainer(ctx context.Context, vmid int, s driver.GuestSpec) error {
	if s.Image == "" {
		return fmt.Errorf("%w: a container starts from a template archive; none named", driver.ErrRefused)
	}
	if len(s.UserData) > 0 {
		return fmt.Errorf("%w: a container here takes no user data", driver.ErrRefused)
	}
	disk := s.DiskGB
	if disk == 0 {
		disk = 8
	}
	p := url.Values{
		"vmid":         {strconv.Itoa(vmid)},
		"ostemplate":   {s.Image},
		"hostname":     {s.Name},
		"cores":        {strconv.Itoa(s.Cores)},
		"memory":       {strconv.Itoa(s.MemoryMB)},
		"swap":         {"0"},
		"rootfs":       {fmt.Sprintf("%s:%d", d.storage, disk)},
		"net0":         {d.net0(true)},
		"pool":         {d.pool},
		"unprivileged": {"1"},
		"features":     {"nesting=1"},
		"onboot":       {"0"},
		"description":  {marker(s.ID)},
	}
	if len(s.SSHKeys) > 0 {
		p.Set("ssh-public-keys", strings.Join(s.SSHKeys, "\n"))
	}
	return d.c.run(ctx, http.MethodPost, "/nodes/"+url.PathEscape(d.node)+"/lxc", p)
}

// template finds a VM template by name in the images pool.
func (d *Driver) template(ctx context.Context, name string) (resource, error) {
	rs, err := d.resources(ctx)
	if err != nil {
		return resource{}, err
	}
	for _, r := range rs {
		if r.Pool == d.images && r.Template == 1 && r.Type == "qemu" && r.Name == name {
			return r, nil
		}
	}
	return resource{}, fmt.Errorf("%w: no VM template named %q in pool %s", driver.ErrRefused, name, d.images)
}

func (d *Driver) cloneVM(ctx context.Context, vmid int, s driver.GuestSpec) error {
	t, err := d.template(ctx, s.Image)
	if err != nil {
		return err
	}
	p := url.Values{
		"newid":       {strconv.Itoa(vmid)},
		"name":        {s.Name},
		"description": {marker(s.ID)},
		"pool":        {d.pool},
		"target":      {d.node},
	}
	if d.full || t.Node != d.node {
		p.Set("full", "1")
		p.Set("storage", d.storage)
	}
	return d.c.run(ctx, http.MethodPost, t.path()+"/clone", p)
}

func seedName(id string) string { return "hangar-seed-" + id + ".iso" }

// configureVM makes a clone the machine asked for — idempotent, so a retry
// finishes a create that stopped half-way: its seed disc, size, network and
// tags, then its disk grown to the size asked.
func (d *Driver) configureVM(ctx context.Context, r resource, s driver.GuestSpec, tagStr string) error {
	volid := d.seeds + ":iso/" + seedName(s.ID)
	if err := d.upload(ctx, s, volid); err != nil {
		return err
	}
	p := url.Values{
		"cores":   {strconv.Itoa(s.Cores)},
		"sockets": {"1"},
		"memory":  {strconv.Itoa(s.MemoryMB)},
		"numa":    {"1"},
		"hotplug": {"disk,network,usb,memory"},
		"net0":    {d.net0(false)},
		"ide2":    {volid + ",media=cdrom"},
		"tags":    {tagStr},
		"agent":   {"enabled=1"},
		"onboot":  {"0"},
	}
	if err := d.c.run(ctx, http.MethodPost, r.path()+"/config", p); err != nil {
		return err
	}
	if s.DiskGB == 0 {
		return nil
	}
	var cfg map[string]any
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
		return err
	}
	disk := bootDisk(cfg)
	if disk == "" || sizeGB(str(cfg[disk])) >= s.DiskGB {
		return nil
	}
	return d.c.run(ctx, http.MethodPut, r.path()+"/resize", url.Values{"disk": {disk}, "size": {fmt.Sprintf("%dG", s.DiskGB)}})
}

// upload puts the VM's seed disc on the seed storage, unless it is there.
func (d *Driver) upload(ctx context.Context, s driver.GuestSpec, volid string) error {
	have, err := d.seedExists(ctx, volid)
	if err != nil || have {
		return err
	}
	iso := nocloudSeed(s.ID, s.Name, s.SSHKeys, s.UserData, time.Now())
	var upid string
	path := "/nodes/" + url.PathEscape(d.node) + "/storage/" + url.PathEscape(d.seeds) + "/upload"
	if err := d.c.upload(ctx, path, map[string]string{"content": "iso"}, seedName(s.ID), iso, &upid); err != nil {
		return err
	}
	return d.c.wait(ctx, upid)
}

func (d *Driver) seedExists(ctx context.Context, volid string) (bool, error) {
	var items []struct {
		VolID string `json:"volid"`
	}
	path := "/nodes/" + url.PathEscape(d.node) + "/storage/" + url.PathEscape(d.seeds) + "/content"
	if err := d.c.call(ctx, http.MethodGet, path, url.Values{"content": {"iso"}}, &items); err != nil {
		return false, err
	}
	for _, it := range items {
		if it.VolID == volid {
			return true, nil
		}
	}
	return false, nil
}

func (d *Driver) DeleteGuest(ctx context.Context, id string) error {
	r, err := d.find(ctx, id)
	if errors.Is(err, driver.ErrNotFound) {
		return d.deleteSeed(ctx, id)
	}
	if err != nil {
		return d.engine(err)
	}
	// its disks go with it: one that is a volume (volumes.go) keeps it here
	if held, err := d.heldBy(ctx, r); err != nil {
		return d.engine(err)
	} else if len(held) > 0 {
		return fmt.Errorf("%w: it holds %s: detach them first — they keep their data", driver.ErrRefused, strings.Join(held, ", "))
	}
	// /cluster/resources is pvestatd's view, seconds behind: decide on the live one
	if on, err := d.running(ctx, r); err != nil {
		return d.engine(err)
	} else if on {
		if err := d.c.run(ctx, http.MethodPost, r.path()+"/status/stop", nil); err != nil {
			return d.engine(err)
		}
	}
	if err := d.c.run(ctx, http.MethodDelete, r.path(), url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}); err != nil {
		return d.engine(err)
	}
	return d.deleteSeed(ctx, id)
}

func (d *Driver) deleteSeed(ctx context.Context, id string) error {
	volid := d.seeds + ":iso/" + seedName(id)
	have, err := d.seedExists(ctx, volid)
	if err != nil || !have {
		return d.engine(err)
	}
	path := "/nodes/" + url.PathEscape(d.node) + "/storage/" + url.PathEscape(d.seeds) + "/content/" + url.PathEscape(volid)
	return d.engine(d.c.run(ctx, http.MethodDelete, path, nil))
}

func (d *Driver) SetPower(ctx context.Context, id string, on bool) (driver.Guest, error) {
	r, err := d.find(ctx, id)
	if err != nil {
		return driver.Guest{}, d.engine(err)
	}
	if err := d.power(ctx, r, on); err != nil {
		return driver.Guest{}, d.engine(err)
	}
	return d.read(ctx, r, id)
}

// running reads a guest's power as it is now (never from /cluster/resources,
// which trails it by a pvestatd pass).
func (d *Driver) running(ctx context.Context, r resource) (bool, error) {
	var st struct {
		Status string `json:"status"`
	}
	err := d.c.call(ctx, http.MethodGet, r.path()+"/status/current", nil, &st)
	return st.Status == "running", err
}

// power starts a guest, or shuts it down (asked, then made to after 60 s).
func (d *Driver) power(ctx context.Context, r resource, on bool) error {
	now, err := d.running(ctx, r)
	if err != nil {
		return err
	}
	if now == on {
		return nil
	}
	if on {
		return d.c.run(ctx, http.MethodPost, r.path()+"/status/start", nil)
	}
	return d.c.run(ctx, http.MethodPost, r.path()+"/status/shutdown",
		url.Values{"timeout": {strconv.Itoa(d.shutdown)}, "forceStop": {"1"}})
}

// cpuLimit writes a guest's cap (0 lifts it), unless it already reads so.
func (d *Driver) cpuLimit(ctx context.Context, r resource, cores int) error {
	var cfg map[string]any
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
		return err
	}
	if int(math.Ceil(fnum(cfg["cpulimit"]))) == cores {
		return nil
	}
	p := url.Values{"cpulimit": {strconv.Itoa(cores)}}
	if cores == 0 {
		p = url.Values{"delete": {"cpulimit"}}
	}
	if r.Type == "lxc" {
		return d.c.call(ctx, http.MethodPut, r.path()+"/config", p, nil)
	}
	return d.c.run(ctx, http.MethodPost, r.path()+"/config", p)
}

// SetCPULimit caps a guest's CPU (cpulimit: live on both kinds — S2 read
// the cgroup's cpu.max move at once).
func (d *Driver) SetCPULimit(ctx context.Context, id string, cores int) (driver.Guest, error) {
	if cores < 0 {
		return driver.Guest{}, fmt.Errorf("%w: a cap of %d cores", driver.ErrRefused, cores)
	}
	r, err := d.find(ctx, id)
	if err != nil {
		return driver.Guest{}, d.engine(err)
	}
	if err := d.cpuLimit(ctx, r, cores); err != nil {
		return driver.Guest{}, d.engine(err)
	}
	return d.read(ctx, r, id)
}

// Retag writes a guest's tags and holds anew; a tag change is live.
func (d *Driver) Retag(ctx context.Context, id string, m map[string]string, holds []string) (driver.Guest, error) {
	tagStr, err := tags(id, m, holds)
	if err != nil {
		return driver.Guest{}, err
	}
	r, err := d.find(ctx, id)
	if err != nil {
		return driver.Guest{}, d.engine(err)
	}
	var cfg map[string]any
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
		return driver.Guest{}, d.engine(err)
	}
	if strings.Join(tagList(str(cfg["tags"])), ";") != tagStr {
		p := url.Values{"tags": {tagStr}}
		if r.Type == "lxc" {
			err = d.c.call(ctx, http.MethodPut, r.path()+"/config", p, nil)
		} else {
			err = d.c.run(ctx, http.MethodPost, r.path()+"/config", p)
		}
		if err != nil {
			return driver.Guest{}, d.engine(err)
		}
	}
	return d.read(ctx, r, id)
}

// ---- The watcher facet ------------------------------------------------------

// GuestRunning reads a watched guest's power, live. The token sees it through
// VM.Audit on it alone (a guest it was not granted reads as not found).
func (d *Driver) GuestRunning(ctx context.Context, ref string) (bool, error) {
	if !slices.Contains(d.watch, ref) {
		return false, fmt.Errorf("%w: guest %s is not one this zone watches", driver.ErrRefused, ref)
	}
	rs, err := d.resources(ctx)
	if err != nil {
		return false, d.engine(err)
	}
	for _, r := range rs {
		if strconv.Itoa(r.VMID) == ref {
			on, err := d.running(ctx, r)
			return on, d.engine(err)
		}
	}
	return false, fmt.Errorf("%w: guest %s is not visible to the token (VM.Audit on /vms/%s)", driver.ErrNotFound, ref, ref)
}

type nodeEntry struct {
	Node   string `json:"node"`
	Status string `json:"status"`
}

func (d *Driver) nodes(ctx context.Context) ([]nodeEntry, error) {
	var ns []nodeEntry
	err := d.c.call(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {"node"}}, &ns)
	return ns, err
}

// NodeDown reads a node's state from the cluster's own list — any token sees
// it, with no grant on the node. Proxmox says "offline" only when the
// cluster's membership does (API2Tools.pm, extract_node_stats), "online"
// while pvestatd's stats are fresh, and "unknown" otherwise — a lone node, or
// one too busy to report: that cannot tell, and says so.
func (d *Driver) NodeDown(ctx context.Context, node string) (bool, error) {
	ns, err := d.nodes(ctx)
	if err != nil {
		return false, d.engine(err)
	}
	for _, n := range ns {
		if n.Node != node {
			continue
		}
		switch n.Status {
		case "offline":
			return true, nil
		case "online":
			return false, nil
		}
		return false, fmt.Errorf("node %s reads %q: the cluster cannot tell whether it is up", node, n.Status)
	}
	return false, fmt.Errorf("%w: the cluster has no node %s", driver.ErrNotFound, node)
}

// Awake: the zone's node answers a call the API hands to it — a node asleep
// does not, whichever node the endpoint is. (Its state in the cluster's list
// is no answer: "unknown" there is also a node too busy to report.)
func (d *Driver) Awake(ctx context.Context) (bool, error) {
	var v map[string]any
	if err := d.c.call(ctx, http.MethodGet, "/nodes/"+url.PathEscape(d.node)+"/version", nil, &v); err != nil {
		return false, nil
	}
	return true, nil
}

func (d *Driver) Reboot(ctx context.Context, id string) (driver.Guest, error) {
	r, err := d.find(ctx, id)
	if err != nil {
		return driver.Guest{}, d.engine(err)
	}
	if on, err := d.running(ctx, r); err != nil {
		return driver.Guest{}, d.engine(err)
	} else if !on {
		return driver.Guest{}, fmt.Errorf("%w: %s is stopped; start it instead", driver.ErrRefused, id)
	}
	if err := d.c.run(ctx, http.MethodPost, r.path()+"/status/reboot", url.Values{"timeout": {"60"}}); err != nil {
		return driver.Guest{}, d.engine(err)
	}
	// a VM's reboot task ends once it is down; qmeventd starts it again
	// after — wait for that, or say it did not come back
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var st struct {
			Status string `json:"status"`
		}
		if err := d.c.call(ctx, http.MethodGet, r.path()+"/status/current", nil, &st); err != nil {
			return driver.Guest{}, d.engine(err)
		}
		if st.Status == "running" {
			break
		}
		if time.Now().After(deadline) {
			return driver.Guest{}, fmt.Errorf("%w: %s went down for its reboot and did not come back within 2 minutes", driver.ErrRefused, id)
		}
		select {
		case <-ctx.Done():
			return driver.Guest{}, ctx.Err()
		case <-time.After(d.c.poll):
		}
	}
	return d.read(ctx, r, id)
}

// shrinkMargin is what a container's memory must stay above its use by: a
// limit written below what the group holds makes the kernel kill inside it,
// init included.
const shrinkMargin = 64 << 20

func (d *Driver) ResizeGuest(ctx context.Context, id string, cores, memoryMB int) (driver.Guest, error) {
	r, err := d.find(ctx, id)
	if err != nil {
		return driver.Guest{}, d.engine(err)
	}
	g, err := d.read(ctx, r, id)
	if err != nil {
		return driver.Guest{}, err
	}
	p := url.Values{}
	if cores != g.Cores {
		p.Set("cores", strconv.Itoa(cores))
	}
	if memoryMB != g.MemoryMB {
		p.Set("memory", strconv.Itoa(memoryMB))
	}
	if len(p) == 0 {
		return g, nil
	}
	if g.Running {
		t := d.Traits(g.Kind)
		switch {
		case cores != g.Cores && !t.LiveCores:
			return driver.Guest{}, fmt.Errorf("%w: a running VM's cores change at its next cold start here: stop it first", driver.ErrRefused)
		case memoryMB < g.MemoryMB && !t.LiveMemoryDown:
			return driver.Guest{}, fmt.Errorf("%w: a running VM's memory only grows: stop it first", driver.ErrRefused)
		case memoryMB < g.MemoryMB:
			var st struct {
				Mem int64 `json:"mem"`
			}
			if err := d.c.call(ctx, http.MethodGet, r.path()+"/status/current", nil, &st); err != nil {
				return driver.Guest{}, d.engine(err)
			}
			if int64(memoryMB)<<20 < st.Mem+shrinkMargin {
				return driver.Guest{}, fmt.Errorf("%w: it holds %d MB now; its memory goes no lower than %d MB while it runs — stop it first",
					driver.ErrRefused, st.Mem>>20, (st.Mem+shrinkMargin+(1<<20)-1)>>20)
			}
		}
	}
	if r.Type == "lxc" {
		err = d.c.call(ctx, http.MethodPut, r.path()+"/config", p, nil)
	} else {
		err = d.c.run(ctx, http.MethodPost, r.path()+"/config", p)
	}
	if err != nil {
		return driver.Guest{}, d.engine(err)
	}
	if g.Running && r.Type == "qemu" {
		// a change Proxmox could not make live is left pending: undo it and say so
		var pending []struct {
			Key     string `json:"key"`
			Pending any    `json:"pending"`
		}
		if err := d.c.call(ctx, http.MethodGet, r.path()+"/pending", nil, &pending); err == nil {
			for _, pc := range pending {
				if pc.Pending != nil && (pc.Key == "memory" || pc.Key == "cores") {
					_ = d.c.run(ctx, http.MethodPost, r.path()+"/config", url.Values{"revert": {pc.Key}})
					return driver.Guest{}, fmt.Errorf("%w: Proxmox could not change %s live; nothing was changed", driver.ErrRefused, pc.Key)
				}
			}
		}
	}
	return d.read(ctx, r, id)
}

// engine maps the API's answers to the driver's errors: a refusal stays a
// refusal (its words kept), a guest the fence hides or that is gone is not
// found, an API that did not answer is left as it is (the plugin retries).
func (d *Driver) engine(err error) error {
	if err == nil || errors.Is(err, driver.ErrNotFound) || errors.Is(err, driver.ErrRefused) {
		return err
	}
	var te *taskError
	if errors.As(err, &te) {
		return fmt.Errorf("%w: %s", driver.ErrRefused, te.Error())
	}
	var ae *apiError
	if errors.As(err, &ae) {
		if ae.Status == http.StatusNotFound || strings.Contains(ae.Message, "does not exist") {
			return fmt.Errorf("%w: %s", driver.ErrNotFound, ae.Message)
		}
		if ae.Status < 500 || ae.Status == http.StatusInternalServerError {
			return fmt.Errorf("%w: %s", driver.ErrRefused, ae.Error())
		}
	}
	return err
}

var (
	_ driver.Guests  = (*Driver)(nil)
	_ driver.Watcher = (*Driver)(nil)
	_ driver.Volumes = (*Driver)(nil)
)
