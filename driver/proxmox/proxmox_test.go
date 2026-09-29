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
	s, err := tags("m-0123456789abcdef0", map[string]string{"class": "spot", "type": "t3.medium"})
	if err != nil || s != "class.spot;hangar-id.m-0123456789abcdef0;type.t3.medium" {
		t.Fatalf("%q %v", s, err)
	}
	if guestID(s) != "m-0123456789abcdef0" {
		t.Fatal("the id did not read back")
	}
	if m := tagMap(s); m["class"] != "spot" || m["type"] != "t3.medium" || len(m) != 2 {
		t.Fatalf("%v", m)
	}
	if _, err := tags("m-1", map[string]string{"owner": "alice@example.com"}); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("an @ in a tag was written: %v", err)
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
