package proxmox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/driver"
)

// api imitates the few answers of Proxmox VE's API these tests need, shaped
// as a live cluster answered them: data in {"data": …}, a refusal's words in
// the status line, a long call a 200 with a task id whose end is read later.
type api struct {
	mu    sync.Mutex
	perms map[string]map[string]int
	res   []resource
	nodes []nodeEntry          // the cluster's list of its nodes
	tasks map[string][2]string // upid -> exit status, log line
	calls []string
	h     map[string]http.HandlerFunc
}

func newAPI() *api {
	return &api{
		perms: map[string]map[string]int{"/pool/hangar": {"VM.Allocate": 1, "VM.PowerMgmt": 1}},
		tasks: map[string][2]string{},
		h:     map[string]http.HandlerFunc{},
	}
}

func (a *api) task(exit, log string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	upid := fmt.Sprintf("UPID:node-a:%08X:00000000:00000000:qmstart:1:tok@pve!t:", len(a.tasks))
	a.tasks[upid] = [2]string{exit, log}
	return upid
}

func data(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.calls = append(a.calls, r.Method+" "+r.URL.Path)
	a.mu.Unlock()
	if r.Header.Get("Authorization") != "PVEAPIToken=tok@pve!t=s3cret" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/api2/json")
	if h, ok := a.h[r.Method+" "+p]; ok {
		h(w, r)
		return
	}
	switch {
	case p == "/version":
		data(w, map[string]string{"version": "9.2"})
	case p == "/access/permissions":
		data(w, a.perms)
	case p == "/cluster/resources" && r.URL.Query().Get("type") == "node":
		data(w, a.nodes)
	case p == "/cluster/resources":
		data(w, a.res)
	case strings.HasSuffix(p, "/status") && strings.Contains(p, "/tasks/"):
		upid := strings.TrimSuffix(strings.SplitN(p, "/tasks/", 2)[1], "/status")
		data(w, map[string]string{"status": "stopped", "exitstatus": a.tasks[upid][0]})
	case strings.HasSuffix(p, "/log") && strings.Contains(p, "/tasks/"):
		upid := strings.TrimSuffix(strings.SplitN(p, "/tasks/", 2)[1], "/log")
		data(w, []map[string]any{{"n": 1, "t": a.tasks[upid][1]}, {"n": 2, "t": "TASK ERROR: " + a.tasks[upid][0]}})
	default:
		http.Error(w, "", http.StatusNotImplemented)
	}
}

func open(t *testing.T, a *api, opts map[string]string) *Driver {
	t.Helper()
	srv := httptest.NewTLSServer(a)
	t.Cleanup(srv.Close)
	o := map[string]string{"node": "node-a", "pool": "hangar", "storage": "local-zfs", "seed_storage": "seeds", "bridge": "vmbr0", "vmids": "11000-11009"}
	for k, v := range opts {
		o[k] = v
	}
	sum := srv.Certificate().Raw
	o["fingerprint"] = fmt.Sprintf("%x", sha(sum))
	d, err := Open(context.Background(), driver.Params{Zone: "z", Endpoint: srv.URL, Options: o, Credential: []byte("tok@pve!t=s3cret\n")})
	if err != nil {
		t.Fatal(err)
	}
	pd := d.(*Driver)
	pd.c.poll = time.Millisecond
	return pd
}

func TestOpenRefusesWhatItCannotUse(t *testing.T) {
	ctx := context.Background()
	for name, p := range map[string]driver.Params{
		"no options":      {Endpoint: "https://192.0.2.1:8006", Credential: []byte("a@pve!b=c")},
		"a bad range":     {Endpoint: "https://192.0.2.1:8006", Credential: []byte("a@pve!b=c"), Options: map[string]string{"node": "n", "pool": "p", "storage": "s", "seed_storage": "i", "bridge": "b", "vmids": "9-1"}},
		"not a token":     {Endpoint: "https://192.0.2.1:8006", Credential: []byte("hunter2"), Options: map[string]string{"node": "n", "pool": "p", "storage": "s", "seed_storage": "i", "bridge": "b", "vmids": "200-300"}},
		"both TLS pins":   {Endpoint: "https://192.0.2.1:8006", Credential: []byte("a@pve!b=c"), Options: map[string]string{"node": "n", "pool": "p", "storage": "s", "seed_storage": "i", "bridge": "b", "vmids": "200-300", "ca_file": "x", "fingerprint": "y"}},
		"not an endpoint": {Endpoint: "node-a:8006", Credential: []byte("a@pve!b=c"), Options: map[string]string{"node": "n", "pool": "p", "storage": "s", "seed_storage": "i", "bridge": "b", "vmids": "200-300"}},
	} {
		if _, err := Open(ctx, p); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}

func TestTheFenceIsReadFromTheTokensOwnPermissions(t *testing.T) {
	a := newAPI()
	a.res = []resource{{VMID: 11000, Node: "node-a", Type: "lxc", Pool: "hangar"}, {VMID: 100, Node: "node-a", Type: "qemu"}}
	a.perms["/vms/11000"] = map[string]int{"VM.PowerMgmt": 1}
	d := open(t, a, nil)
	if !has(d.Capabilities(), driver.FencePool) {
		t.Fatalf("a token that acts only on its pool is not fenced: %s", d.FenceReport())
	}
	// the control: the same token also reaching a guest outside its pool
	a.perms["/vms/100"] = map[string]int{"VM.PowerMgmt": 1}
	d = open(t, a, nil)
	if has(d.Capabilities(), driver.FencePool) || !strings.Contains(d.FenceReport(), "VM.PowerMgmt on /vms/100") {
		t.Fatalf("a token reaching /vms/100 still fenced: %v %q", d.Capabilities(), d.FenceReport())
	}
	delete(a.perms, "/vms/100")
	a.perms["/"] = map[string]int{"Permissions.Modify": 1}
	d = open(t, a, nil)
	if has(d.Capabilities(), driver.FencePool) {
		t.Fatal("a token that can grant itself more is fenced")
	}
}

// A reservation's guest, outside the pool, may be READ — VM.Audit on it, and
// only on a guest the zone watches. Anything more there, or the same read on
// a guest it does not watch, and the token is not fenced.
func TestTheFenceLetsAWatchedGuestBeRead(t *testing.T) {
	a := newAPI()
	a.res = []resource{{VMID: 11000, Node: "node-a", Type: "lxc", Pool: "hangar"}, {VMID: 100, Node: "node-a", Type: "qemu"}}
	a.perms["/vms/100"] = map[string]int{"VM.Audit": 0}
	watching := func(watch ...string) *Driver {
		t.Helper()
		srv := httptest.NewTLSServer(a)
		t.Cleanup(srv.Close)
		d, err := Open(context.Background(), driver.Params{Zone: "z", Endpoint: srv.URL, Credential: []byte("tok@pve!t=s3cret"), Watch: watch,
			Options: map[string]string{"node": "node-a", "pool": "hangar", "storage": "s", "seed_storage": "i", "bridge": "b",
				"vmids": "11000-11009", "fingerprint": fmt.Sprintf("%x", sha(srv.Certificate().Raw))}})
		if err != nil {
			t.Fatal(err)
		}
		return d.(*Driver)
	}
	if d := watching("100"); !has(d.Capabilities(), driver.FencePool) {
		t.Fatalf("reading a watched guest is outside the fence: %s", d.FenceReport())
	}
	if d := watching(); has(d.Capabilities(), driver.FencePool) || !strings.Contains(d.FenceReport(), "VM.Audit on /vms/100") {
		t.Fatalf("reading a guest the zone does not watch is fenced: %q", d.FenceReport())
	}
	a.perms["/vms/100"]["VM.Console"] = 0
	if d := watching("100"); has(d.Capabilities(), driver.FencePool) || !strings.Contains(d.FenceReport(), "VM.Console on /vms/100") {
		t.Fatalf("more than a read on a watched guest is fenced: %q", d.FenceReport())
	}
	if _, err := Open(context.Background(), driver.Params{Zone: "z", Endpoint: "https://192.0.2.1:8006", Credential: []byte("a@pve!b=c"),
		Watch: []string{"priority"}, Options: map[string]string{"node": "n", "pool": "p", "storage": "s", "seed_storage": "i", "bridge": "b", "vmids": "200-300"}}); err == nil {
		t.Fatal("a watched guest named otherwise than by its VMID was accepted")
	}
}

// A hook that refuses a start: the API answers 200 and a task id, and the
// refusal lives only in the task. The driver must wait for it and say why.
func TestARefusedStartIsReadFromItsTask(t *testing.T) {
	a := newAPI()
	a.res = []resource{{VMID: 11001, Node: "node-a", Type: "lxc", Pool: "hangar", Tags: "hangar-id.m-0123456789abcdef0", Status: "stopped"}}
	upid := a.task("hookscript error for 11001 on pre-start: command '/var/lib/vz/snippets/hook' failed: exit code 1",
		"room: the zone's spot pool is full")
	a.h["GET /nodes/node-a/lxc/11001/status/current"] = func(w http.ResponseWriter, _ *http.Request) {
		data(w, map[string]any{"status": "stopped"})
	}
	a.h["POST /nodes/node-a/lxc/11001/status/start"] = func(w http.ResponseWriter, _ *http.Request) { data(w, upid) }
	d := open(t, a, nil)
	_, err := d.SetPower(context.Background(), "m-0123456789abcdef0", true)
	if !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("a failed task was not a refusal: %v", err)
	}
	for _, want := range []string{"hookscript error for 11001 on pre-start", "room: the zone's spot pool is full"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lost %q: %v", want, err)
		}
	}
}

// The same refusal comes back 403 from one kind of guest and 500 from the
// other; its words are in the status line.
func TestARefusalsWordsAreKept(t *testing.T) {
	a := newAPI()
	a.res = []resource{{VMID: 11002, Node: "node-a", Type: "lxc", Pool: "hangar", Tags: "hangar-id.m-0000000000000000a"}}
	a.h["GET /nodes/node-a/lxc/11002/status/current"] = func(w http.ResponseWriter, _ *http.Request) {
		data(w, map[string]any{"status": "stopped"})
	}
	a.h["POST /nodes/node-a/lxc/11002/status/start"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"data":null}`))
	}
	d := open(t, a, nil)
	// net/http's test server writes its own reason phrase; Proxmox writes
	// its message there. Read what the client makes of a message body too.
	_, err := d.SetPower(context.Background(), "m-0000000000000000a", true)
	if !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a 403 is not a refusal: %v", err)
	}
	a.h["POST /nodes/node-a/lxc/11002/status/start"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"data":null,"message":"only root can set 'hookscript' config\n"}`))
	}
	_, err = d.SetPower(context.Background(), "m-0000000000000000a", true)
	if !errors.Is(err, driver.ErrRefused) || !strings.Contains(err.Error(), "only root can set 'hookscript' config") {
		t.Fatalf("a 500's words were lost: %v", err)
	}
}

func TestAGuestOutsideThePoolOrUntaggedDoesNotExist(t *testing.T) {
	a := newAPI()
	a.res = []resource{
		{VMID: 100, Node: "node-a", Type: "lxc", Tags: "hangar-id.m-1"},                       // not in the pool
		{VMID: 11003, Node: "node-a", Type: "lxc", Pool: "hangar"},                            // no id
		{VMID: 11004, Node: "node-a", Type: "qemu", Pool: "hangar", Name: "m-2", Template: 1}, // a template
	}
	d := open(t, a, nil)
	for _, id := range []string{"m-1", "m-2", ""} {
		if _, err := d.find(context.Background(), id); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("%q found: %v", id, err)
		}
	}
}

// A create cut between the guest's birth and its tags: found again by the
// marker its description carries from birth — and only in its own pool.
func TestAGuestIsFoundByItsMarkerBeforeItsTags(t *testing.T) {
	a := newAPI()
	a.res = []resource{
		{VMID: 11005, Node: "node-a", Type: "lxc", Pool: "hangar"},
		{VMID: 11006, Node: "node-a", Type: "lxc", Pool: "elsewhere"},
	}
	for _, v := range []string{"11005", "11006"} {
		a.h["GET /nodes/node-a/lxc/"+v+"/config"] = func(w http.ResponseWriter, _ *http.Request) {
			data(w, map[string]any{"description": "made by hangar: m-" + v + "\n", "hostname": "x"})
		}
	}
	d := open(t, a, nil)
	r, err := d.find(context.Background(), "m-11005")
	if err != nil || r.VMID != 11005 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := d.find(context.Background(), "m-11006"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("a marked guest outside the pool was found: %v", err)
	}
}

func TestFreeVMIDAsksTheClusterAboutWhatTheFenceHides(t *testing.T) {
	a := newAPI()
	a.res = []resource{{VMID: 11000, Node: "node-a", Type: "lxc", Pool: "hangar"}}
	taken := map[string]bool{"11001": true, "11002": true} // guests the token cannot see
	a.h["GET /cluster/nextid"] = func(w http.ResponseWriter, r *http.Request) {
		if taken[r.URL.Query().Get("vmid")] {
			http.Error(w, `{"data":null,"errors":{"vmid":"VM 11001 already exists"}}`, http.StatusBadRequest)
			return
		}
		data(w, r.URL.Query().Get("vmid"))
	}
	d := open(t, a, nil)
	id, err := d.freeVMID(context.Background())
	if err != nil || id != 11003 {
		t.Fatalf("free id %d, %v; want 11003", id, err)
	}
}

func TestTags(t *testing.T) {
	s, err := tags("m-0123456789abcdef0", map[string]string{"class": "guaranteed+spot", "type": "t3.medium"}, []string{"4100"})
	if err != nil || s != "class.guaranteed+spot;hangar-id.m-0123456789abcdef0;held.4100;type.t3.medium" {
		t.Fatalf("%q %v", s, err)
	}
	if guestID(s) != "m-0123456789abcdef0" {
		t.Fatal("the id did not read back")
	}
	if m := tagMap(s); m["class"] != "guaranteed+spot" || m["type"] != "t3.medium" || len(m) != 2 {
		t.Fatalf("%v", m)
	}
	if h := holdsOf(s + ";held.down-node-b"); len(h) != 2 || h[0] != "4100" || h[1] != "down-node-b" {
		t.Fatalf("holds %v", h)
	}
	if _, err := tags("m-1", map[string]string{"owner": "alice@example.com"}, nil); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("an @ in a tag was written: %v", err)
	}
	if _, err := tags("m-1", map[string]string{"held": "4100"}, nil); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("a plugin wrote the driver's own held tag: %v", err)
	}
}

func TestReadingTheConfig(t *testing.T) {
	for line, want := range map[string]int{"local-zfs:vm-1-disk-0,size=8G": 8, "x:y,iothread=1,size=3584M": 4, "x,size=1T": 1024, "x": 0} {
		if got := sizeGB(line); got != want {
			t.Errorf("sizeGB(%q) = %d, want %d", line, got, want)
		}
	}
	if memoryMB("current=2048") != 2048 || memoryMB(float64(512)) != 512 || memoryMB("1024") != 1024 {
		t.Error("memory")
	}
	cfg := map[string]any{"boot": "order=ide2;scsi0", "ide2": "local:iso/x.iso,media=cdrom", "scsi0": "local-zfs:d,size=3G"}
	if bootDisk(cfg) != "scsi0" {
		t.Errorf("boot disk %q", bootDisk(cfg))
	}
	delete(cfg, "boot")
	if bootDisk(cfg) != "scsi0" {
		t.Errorf("boot disk without an order %q", bootDisk(cfg))
	}
}

// The seed disc, read back the way a guest would find its files: the Joliet
// tree's names, the label, the data at each extent.
func TestTheSeedDisc(t *testing.T) {
	img := nocloudSeed("m-0123456789abcdef0", "dev", []string{"ssh-ed25519 AAAAC3Nza key"}, []byte("#cloud-config\n"), time.Unix(0, 0))
	if len(img)%sector != 0 || len(img) < 26*sector {
		t.Fatalf("size %d", len(img))
	}
	if string(img[16*sector+1:16*sector+6]) != "CD001" || img[17*sector] != 2 || img[18*sector] != 255 {
		t.Fatal("descriptors")
	}
	svd := img[17*sector:]
	if string(svd[88:91]) != "%/E" {
		t.Fatal("not Joliet")
	}
	if label := fromUCS2(svd[40:72]); strings.TrimSpace(label) != "cidata" {
		t.Fatalf("label %q", label)
	}
	root := binary.LittleEndian.Uint32(svd[156+2:])
	files := map[string][]byte{}
	dir := img[int(root)*sector:]
	for off := 0; dir[off] != 0; off += int(dir[off]) {
		rec := dir[off:]
		n := int(rec[32])
		id := rec[33 : 33+n]
		if n == 1 && id[0] <= 1 {
			continue
		}
		at := binary.LittleEndian.Uint32(rec[2:])
		size := binary.LittleEndian.Uint32(rec[10:])
		files[fromUCS2(id)] = img[int(at)*sector : int(at)*sector+int(size)]
	}
	if string(files["user-data"]) != "#cloud-config\n" {
		t.Fatalf("user-data %q (files %v)", files["user-data"], keys(files))
	}
	md := string(files["meta-data"])
	for _, want := range []string{"instance-id: m-0123456789abcdef0", "local-hostname: dev", `- "ssh-ed25519 AAAAC3Nza key"`} {
		if !strings.Contains(md, want) {
			t.Errorf("meta-data lacks %q:\n%s", want, md)
		}
	}
	// the same inputs make the same disc
	if !bytes.Equal(img, nocloudSeed("m-0123456789abcdef0", "dev", []string{"ssh-ed25519 AAAAC3Nza key"}, []byte("#cloud-config\n"), time.Unix(0, 0))) {
		t.Error("not deterministic")
	}
}

func fromUCS2(b []byte) string {
	var r []rune
	for i := 0; i+1 < len(b); i += 2 {
		r = append(r, rune(b[i])<<8|rune(b[i+1]))
	}
	return strings.TrimRight(string(r), " ")
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sha(b []byte) [32]byte { return sha256.Sum256(b) }

// A VM's processor, as it is written for Proxmox and read back: the zone's
// model or the host's, and virtualisation said either way — a VM that did
// not ask is given none, whatever its model carries.
func TestAVMsProcessorLine(t *testing.T) {
	a := newAPI()
	d := open(t, a, nil)
	for name, c := range map[string]struct {
		s    driver.GuestSpec
		want string
	}{
		"nothing asked":       {driver.GuestSpec{}, "x86-64-v2-AES,flags=-nested-virt"},
		"its host's":          {driver.GuestSpec{CPU: driver.CPUModelHost}, "host,flags=-nested-virt"},
		"its host's, VMs too": {driver.GuestSpec{CPU: driver.CPUModelHost, Virtualization: true}, "host,flags=+nested-virt"},
	} {
		if got := d.cpuLine(c.s); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
		// and it reads back as it was asked
		model, virt := cpuOf(c.want)
		if model != c.s.CPU || virt != c.s.Virtualization {
			t.Errorf("%s: %q reads back as %q %v", name, c.want, model, virt)
		}
	}
	// the operator's own model for the zone: every VM that asks nothing
	e := open(t, a, map[string]string{"cpu_model": "EPYC-v4"})
	if got := e.cpuLine(driver.GuestSpec{}); got != "EPYC-v4,flags=-nested-virt" {
		t.Errorf("the zone's model: %q", got)
	}
	if got := e.cpuLine(driver.GuestSpec{CPU: driver.CPUModelHost}); got != "host,flags=-nested-virt" {
		t.Errorf("its host's, in a zone with a model of its own: %q", got)
	}
	// lines no driver wrote: a hand's `host` carries virtualisation, the
	// API's own default and a generic model carry none
	for line, want := range map[string][2]any{
		"host":                                  {"host", true},
		"cputype=host":                          {"host", true},
		"host,flags=+aes;-nested-virt":          {"host", false},
		"":                                      {"", false},
		"kvm64":                                 {"", false},
		"x86-64-v2-AES":                         {"", false},
		"x86-64-v2-AES,flags=+aes;+nested-virt": {"", true},
		"host,hidden=1,flags=+pcid":             {"host", true},
	} {
		if model, virt := cpuOf(line); model != want[0] || virt != want[1] {
			t.Errorf("%q reads as %q %v, want %v", line, model, virt, want)
		}
	}
	for name, s := range map[string]driver.GuestSpec{
		"another model":           {Kind: "vm", CPU: "EPYC"},
		"a container's processor": {Kind: "container", CPU: driver.CPUModelHost},
		"VMs inside a container":  {Kind: "container", Virtualization: true},
		"VMs on a generic model":  {Kind: "vm", Virtualization: true},
		"more than a full share":  {Kind: "vm", CPUWeight: 101},
	} {
		if err := cpuRefusal(s); !errors.Is(err, driver.ErrRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, s := range map[string]driver.GuestSpec{
		"nothing asked":      {Kind: "vm"},
		"a container":        {Kind: "container", CPUWeight: 25},
		"host, VMs, a share": {Kind: "vm", CPU: driver.CPUModelHost, Virtualization: true, CPUWeight: 1},
	} {
		if err := cpuRefusal(s); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, bad := range []string{"host,flags=+nested-virt", "x86 64", "-v2", "a;b"} {
		srv := httptest.NewTLSServer(a)
		_, err := Open(context.Background(), driver.Params{Zone: "z", Endpoint: srv.URL, Credential: []byte("tok@pve!t=s3cret"),
			Options: map[string]string{"node": "n", "pool": "hangar", "storage": "s", "seed_storage": "i", "bridge": "b", "vmids": "11000-11009",
				"fingerprint": fmt.Sprintf("%x", sha(srv.Certificate().Raw)), "cpu_model": bad}})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "cpu_model") {
			t.Errorf("cpu_model %q: %v", bad, err)
		}
	}
	for _, c := range []driver.Capability{driver.CPUHost, driver.CPUNested, driver.CPUWeight} {
		if !has(d.Capabilities(), c) {
			t.Errorf("the driver does not advertise %s", c)
		}
	}
}

// A guest's share of the cores: read from its config (none written is a
// full one), written only when it differs — a VM's through its task, a
// container's at once.
func TestAGuestsWeight(t *testing.T) {
	a := newAPI()
	a.res = []resource{
		{VMID: 11001, Node: "node-a", Type: "qemu", Pool: "hangar", Tags: "hangar-id.m-000000000000000aa", Name: "vm"},
		{VMID: 11002, Node: "node-a", Type: "lxc", Pool: "hangar", Tags: "hangar-id.m-000000000000000bb"},
	}
	cfg := map[string]map[string]any{
		"qemu/11001": {"tags": "hangar-id.m-000000000000000aa", "cores": 2, "memory": "1024", "cpu": "host,flags=-nested-virt"},
		"lxc/11002":  {"tags": "hangar-id.m-000000000000000bb", "cores": 1, "memory": 512, "cpuunits": 25, "hostname": "ct"},
	}
	var wrote []string
	for k := range cfg {
		a.h["GET /nodes/node-a/"+k+"/config"] = func(w http.ResponseWriter, _ *http.Request) { data(w, cfg[k]) }
		a.h["GET /nodes/node-a/"+k+"/status/current"] = func(w http.ResponseWriter, _ *http.Request) { data(w, map[string]any{"status": "stopped"}) }
		write := func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			wrote = append(wrote, r.Method+" "+k+" cpuunits="+r.Form.Get("cpuunits"))
			n := 0
			_, _ = fmt.Sscan(r.Form.Get("cpuunits"), &n)
			cfg[k]["cpuunits"] = n
			if r.Method == http.MethodPost {
				data(w, a.task("OK", ""))
				return
			}
			data(w, nil)
		}
		a.h["POST /nodes/node-a/"+k+"/config"] = write
		a.h["PUT /nodes/node-a/"+k+"/config"] = write
	}
	d := open(t, a, nil)
	ctx := context.Background()
	vm, err := d.Guest(ctx, "m-000000000000000aa")
	if err != nil || vm.CPUWeight != driver.FullWeight || vm.CPU != driver.CPUModelHost || vm.Virtualization {
		t.Fatalf("a VM with no weight written: %+v %v", vm, err)
	}
	ct, err := d.Guest(ctx, "m-000000000000000bb")
	if err != nil || ct.CPUWeight != 25 || ct.CPU != "" {
		t.Fatalf("a container at 25: %+v %v", ct, err)
	}
	// already so: nothing written
	if _, err := d.SetCPUWeight(ctx, "m-000000000000000aa", 100); err != nil || len(wrote) != 0 {
		t.Fatalf("a full share written on one that has it: %v %v", wrote, err)
	}
	if _, err := d.SetCPUWeight(ctx, "m-000000000000000bb", 25); err != nil || len(wrote) != 0 {
		t.Fatalf("25 written on one at 25: %v %v", wrote, err)
	}
	if vm, err = d.SetCPUWeight(ctx, "m-000000000000000aa", 40); err != nil || vm.CPUWeight != 40 {
		t.Fatalf("%+v %v", vm, err)
	}
	if ct, err = d.SetCPUWeight(ctx, "m-000000000000000bb", 0); err != nil || ct.CPUWeight != driver.FullWeight {
		t.Fatalf("%+v %v", ct, err)
	}
	if got := strings.Join(wrote, "; "); got != "POST qemu/11001 cpuunits=40; PUT lxc/11002 cpuunits=100" {
		t.Fatalf("written: %s", got)
	}
	if _, err := d.SetCPUWeight(ctx, "m-000000000000000aa", 101); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("more than a full share: %v", err)
	}
}

// A VM's system disk is made to give back what is deleted inside it, once:
// a line that says so already is left alone, and its other options stay.
func TestAVMsDiskGivesSpaceBack(t *testing.T) {
	a := newAPI()
	var wrote []string
	a.h["POST /nodes/node-a/qemu/11001/config"] = func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		wrote = append(wrote, r.Form.Encode())
		data(w, a.task("OK", ""))
	}
	d := open(t, a, nil)
	r := resource{VMID: 11001, Node: "node-a", Type: "qemu"}
	ctx := context.Background()
	if err := d.trim(ctx, r, map[string]any{"boot": "order=scsi0", "scsi0": "local-zfs:base-9000-disk-0/vm-11001-disk-0,size=3G",
		"ide2": "seeds:iso/x.iso,media=cdrom"}); err != nil {
		t.Fatal(err)
	}
	if len(wrote) != 1 || wrote[0] != "scsi0=local-zfs%3Abase-9000-disk-0%2Fvm-11001-disk-0%2Csize%3D3G%2Cdiscard%3Don" {
		t.Fatalf("written: %v", wrote)
	}
	if err := d.trim(ctx, r, map[string]any{"boot": "order=scsi0", "scsi0": "local-zfs:vm-11001-disk-0,discard=on,size=3G"}); err != nil || len(wrote) != 1 {
		t.Fatalf("a disk that says so already was written again: %v %v", wrote, err)
	}
	// one that says `ignore` is made to
	if err := d.trim(ctx, r, map[string]any{"scsi0": "local-zfs:vm-11001-disk-0,discard=ignore,iothread=1,size=3G"}); err != nil || len(wrote) != 2 ||
		wrote[1] != "scsi0=local-zfs%3Avm-11001-disk-0%2Ciothread%3D1%2Csize%3D3G%2Cdiscard%3Don" {
		t.Fatalf("written: %v %v", wrote, err)
	}
	// a VM with no disk of its own (a shelf): nothing to write
	if err := d.trim(ctx, r, map[string]any{"ide2": "seeds:iso/x.iso,media=cdrom"}); err != nil || len(wrote) != 2 {
		t.Fatalf("a VM with no disk: %v %v", wrote, err)
	}
}
