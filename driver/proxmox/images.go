package proxmox

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tomblancdev/hangar/driver"
)

// Images, on Proxmox VE, are VM templates in the zone's images pool, each
// named after the core's id (a machine's clone finds its template by name)
// and carrying its marker in its description — and NO tag: a clone copies
// its template's tags, and a machine born with another's hangar-id would not
// be found as itself (read on the bench). A builder is tagged while it works
// (hangar-id, class spot: a node making room without the brain stops it).
//
// A BAKE is a builder: a VM made in the images pool from the recipe's base —
// a disk image imported from an import storage, or a template cloned whole —
// that boots once with the recipe as its first boot (the NoCloud seed disc
// the machines' VMs get, seed.go). Its user data is the recipe's, then the
// driver's own last step (bakeEnd): once cloud-init has finished it makes the
// disk ready to be cloned (cloud-init's memory and the machine-id cleared,
// the ssh host keys removed) and says so by the builder's host name —
// hangar-bake-done — or, when cloud-init reported errors, hangar-bake-failed,
// its log kept in /run/hangar-bake.log. The driver reads the host name
// through the guest agent (VM.GuestAgent.Audit), the log with
// VM.GuestAgent.FileRead: a bake needs the recipe to install the QEMU guest
// agent — which is also how the machines born from it report their address.
// Done, the builder is shut down, its seed disc taken off, and it becomes the
// template.
//
// What a builder is at lives in its description, beside the marker every
// guest of the driver carries: "hangar bake <attempt> started <unix time>",
// then "hangar bake <attempt> done" — a brain that stopped half-way finds its
// bake where it was. A builder that stopped before it finished (a person, or
// a node that made room for a priority guest without the brain — it tags it
// held.<key> first) is destroyed and made again: its first boot cannot resume.
//
// A SAVE is a full clone of a stopped machine into the images pool, made a
// template: its system disk only — a machine holding a volume is refused.
//
// A template machines were born from as linked clones (beside it, on the
// same storage) cannot be deleted while they live: Proxmox VE refuses it, and
// the driver names them.

// Builders' host names: what the last step of a bake says.
const (
	bakeDoneName   = "hangar-bake-done"
	bakeFailedName = "hangar-bake-failed"
	bakeLog        = "/run/hangar-bake.log"
)

var (
	bakeStarted = regexp.MustCompile(`(?m)^hangar bake (\d+) started (\d+)$`)
	bakeDone    = regexp.MustCompile(`(?m)^hangar bake (\d+) done$`)
)

func (d *Driver) ImageKinds() []string { return []string{"vm"} }

// findImage locates an image's template, or its builder, in the images pool:
// by its tag, or by the marker its description carries from birth.
func (d *Driver) findImage(ctx context.Context, id string) (resource, map[string]any, error) {
	if id == "" {
		return resource{}, nil, driver.ErrNotFound
	}
	rs, err := d.resources(ctx)
	if err != nil {
		return resource{}, nil, err
	}
	for _, r := range rs {
		if r.Pool != d.images || r.Type != "qemu" {
			continue
		}
		tagged := guestID(r.Tags) == id
		if !tagged && guestID(r.Tags) != "" && r.nameKnown() && r.Name != id {
			// another's builder — a save's clone carries its machine's tag until
			// it is finished, and /cluster/resources may not say its name yet
			// (a pvestatd pass behind): read then
			continue
		}
		var cfg map[string]any
		if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
			if tagged {
				return resource{}, nil, err
			}
			continue
		}
		if tagged || strings.SplitN(strings.TrimSpace(str(cfg["description"])), "\n", 2)[0] == marker(id) {
			return r, cfg, nil
		}
	}
	return resource{}, nil, driver.ErrNotFound
}

// imageOf reads an image (a template) or its bake (a builder).
func (d *Driver) imageOf(ctx context.Context, r resource, cfg map[string]any, id string) (driver.Image, error) {
	im := driver.Image{ID: id, EngineRef: fmt.Sprintf("%s/%s/%d", r.Node, r.Type, r.VMID), Kind: "vm", Node: r.Node}
	if disk := bootDisk(cfg); disk != "" {
		im.SizeGB = sizeGB(str(cfg[disk]))
	}
	desc := str(cfg["description"])
	if m := bakeStarted.FindStringSubmatch(desc); m != nil {
		im.Attempt, _ = strconv.Atoi(m[1])
	}
	if num(cfg["template"]) == 1 {
		im.State, im.Ref = driver.ImageAvailable, str(cfg["name"]) // the config's: the list's may lag
		return im, nil
	}
	on, err := d.running(ctx, r)
	if err != nil {
		return im, err
	}
	im.State = driver.ImageWaiting
	if on {
		im.State = driver.ImagePending
	}
	return im, nil
}

func (d *Driver) Image(ctx context.Context, id string) (driver.Image, error) {
	r, cfg, err := d.findImage(ctx, id)
	if err != nil {
		return driver.Image{}, d.engine(err)
	}
	im, err := d.imageOf(ctx, r, cfg, id)
	return im, d.engine(err)
}

// ---- A bake -----------------------------------------------------------------

func (d *Driver) Bake(ctx context.Context, s driver.BakeSpec, held bool) (driver.Image, error) {
	if s.Kind != "vm" {
		return driver.Image{}, fmt.Errorf("%w: this zone bakes images for VMs, not for a %s", driver.ErrRefused, s.Kind)
	}
	im, err := d.bake(ctx, s, held)
	return im, d.engine(err)
}

func (d *Driver) bake(ctx context.Context, s driver.BakeSpec, held bool) (driver.Image, error) {
	r, cfg, err := d.findImage(ctx, s.ID)
	switch {
	case errors.Is(err, driver.ErrNotFound):
	case err != nil:
		return driver.Image{}, err
	case num(cfg["template"]) == 1:
		return d.imageOf(ctx, r, cfg, s.ID)
	default:
		im, next, err := d.builder(ctx, r, cfg, s, held)
		if err != nil || !next {
			return im, err
		}
	}
	// no builder at work: one is made, unless the room is needed
	if held {
		return driver.Image{ID: s.ID, Kind: s.Kind, State: driver.ImageWaiting, Attempt: s.Attempt, SizeGB: s.DiskGB}, nil
	}
	return d.makeBuilder(ctx, s)
}

// builder moves a bake whose builder exists: next=true when it is gone and a
// new one is to be made.
func (d *Driver) builder(ctx context.Context, r resource, cfg map[string]any, s driver.BakeSpec, held bool) (driver.Image, bool, error) {
	desc := str(cfg["description"])
	attempt, started := 0, int64(0)
	if m := bakeStarted.FindStringSubmatch(desc); m != nil {
		attempt, _ = strconv.Atoi(m[1])
		started, _ = strconv.ParseInt(m[2], 10, 64)
	}
	done := false
	if m := bakeDone.FindStringSubmatch(desc); m != nil && m[1] == strconv.Itoa(attempt) {
		done = true
	}
	im, err := d.imageOf(ctx, r, cfg, s.ID)
	if err != nil {
		return im, false, err
	}
	switch {
	case attempt != s.Attempt:
		// another attempt's builder: not this one's
		return im, true, d.destroy(ctx, r, s.ID)
	case done:
		// its first boot finished clean: an image, whatever else is asked
		im, err := d.finish(ctx, r, s.ID)
		return im, false, err
	case held:
		if err := d.destroy(ctx, r, s.ID); err != nil {
			return im, false, err
		}
		im.State = driver.ImageWaiting
		return im, false, nil
	case im.State == driver.ImageWaiting:
		// stopped before it finished — by a node making room without the
		// brain (it tags the builder held.<key> first), or by someone: its
		// first boot cannot resume, so it starts over
		if err := d.destroy(ctx, r, s.ID); err != nil {
			return im, false, err
		}
		if len(holdsOf(str(cfg["tags"]))) > 0 {
			return im, true, nil
		}
		again, err := d.makeBuilder(ctx, s)
		again.Detail = "its builder stopped before its first boot finished: started over"
		return again, false, err
	}
	// at work: what does it say?
	var host struct {
		Result struct {
			HostName string `json:"host-name"`
		} `json:"result"`
	}
	agentErr := d.c.call(ctx, http.MethodGet, r.path()+"/agent/get-host-name", nil, &host)
	switch host.Result.HostName {
	case bakeDoneName:
		if err := d.c.call(ctx, http.MethodPut, r.path()+"/config", url.Values{
			"description": {strings.TrimRight(desc, "\n") + fmt.Sprintf("\nhangar bake %d done", attempt)},
		}, nil); err != nil {
			return im, false, err
		}
		im, err := d.finish(ctx, r, s.ID)
		return im, false, err
	case bakeFailedName:
		why := d.bakeLog(ctx, r)
		if err := d.destroy(ctx, r, s.ID); err != nil {
			return im, false, err
		}
		im.State, im.Detail = driver.ImageFailed, "the builder's first boot reported errors"+why
		return im, false, nil
	}
	if s.Timeout > 0 && time.Since(time.Unix(started, 0)) > s.Timeout {
		why := ""
		if agentErr != nil {
			why = " — its guest agent never answered: does its recipe install qemu-guest-agent?"
		} else {
			why = d.bakeLog(ctx, r)
		}
		if err := d.destroy(ctx, r, s.ID); err != nil {
			return im, false, err
		}
		im.State, im.Detail = driver.ImageFailed, fmt.Sprintf("no word from its builder within %s%s", s.Timeout, why)
		return im, false, nil
	}
	return im, false, nil
}

// bakeLog reads what the builder's last step kept of its first boot: the
// end of it, as ": …" — or nothing, when it cannot be read.
func (d *Driver) bakeLog(ctx context.Context, r resource) string {
	var got struct {
		Content string `json:"content"`
	}
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/agent/file-read", url.Values{"file": {bakeLog}}, &got); err != nil {
		return ""
	}
	// cloud-init's verdict first, then the end of its log: both kept, the
	// middle cut when it is long
	text := strings.TrimSpace(got.Content)
	if len(text) > 4000 {
		text = text[:1500] + "\n…\n" + text[len(text)-2400:]
	}
	if text == "" {
		return ""
	}
	return ":\n" + text
}

// makeBuilder makes and starts a bake's builder.
func (d *Driver) makeBuilder(ctx context.Context, s driver.BakeSpec) (driver.Image, error) {
	tagStr, err := tags(s.ID, s.Tags, nil)
	if err != nil {
		return driver.Image{}, err
	}
	seed := d.seeds + ":iso/" + seedName(s.ID)
	desc := marker(s.ID) + fmt.Sprintf("\nhangar bake %d started %d", s.Attempt, time.Now().Unix())
	cores, mem := max(s.Cores, 1), max(s.MemoryMB, 512)
	d.mu.Lock()
	err = d.withFreeVMID(ctx, func(vmid int) error {
		if strings.Contains(s.Base, ":") {
			// a disk image on an import storage: the builder's disk is a copy
			return d.c.run(ctx, http.MethodPost, "/nodes/"+url.PathEscape(d.node)+"/qemu", url.Values{
				"vmid": {strconv.Itoa(vmid)}, "name": {s.ID}, "pool": {d.images}, "description": {desc},
				"ostype": {"l26"}, "cores": {strconv.Itoa(cores)}, "memory": {strconv.Itoa(mem)},
				"scsihw": {"virtio-scsi-single"}, "scsi0": {d.storage + ":0,import-from=" + s.Base + ",discard=on"},
				"boot": {"order=scsi0"}, "serial0": {"socket"}, "vga": {"serial0"}, "agent": {"enabled=1"},
				"net0": {d.card(false, d.onLane(vmid), "")}, "onboot": {"0"},
			})
		}
		// a template: copied whole, into the images pool
		t, err := d.template(ctx, s.Base)
		if err != nil {
			return err
		}
		return d.c.run(ctx, http.MethodPost, t.path()+"/clone", url.Values{
			"newid": {strconv.Itoa(vmid)}, "name": {s.ID}, "description": {desc}, "pool": {d.images},
			"target": {d.node}, "full": {"1"}, "storage": {d.storage},
		})
	})
	d.mu.Unlock()
	if err != nil {
		return driver.Image{}, err
	}
	r, cfg, err := d.findImage(ctx, s.ID)
	if err != nil {
		return driver.Image{}, err
	}
	// its card, then its first boot's disc: where the zone gives addresses
	// the disc names the card's MAC — and a builder is given its own, as a
	// machine is
	p, sn, err := d.vmNet(ctx, r, d.onLane(r.VMID)) // a builder stands on the zone's lane
	if err != nil {
		return driver.Image{}, err
	}
	if err := d.upload(ctx, driver.GuestSpec{ID: s.ID, Name: s.ID, UserData: bakeUserData(s.UserData)}, sn, seed); err != nil {
		return driver.Image{}, err
	}
	// its processor is the zone's own model: what a machine born from the
	// image sees, unless it asks otherwise (a clone begins with its
	// template's line)
	for k, v := range map[string]string{
		"cores": strconv.Itoa(cores), "memory": strconv.Itoa(mem), "cpu": d.cpuLine(driver.GuestSpec{}),
		"ide2": seed + ",media=cdrom", "agent": "enabled=1", "tags": tagStr, "onboot": "0",
	} {
		p.Set(k, v)
	}
	if err := d.c.run(ctx, http.MethodPost, r.path()+"/config", p); err != nil {
		return driver.Image{}, err
	}
	if err := d.trim(ctx, r, cfg); err != nil {
		return driver.Image{}, err
	}
	if disk := bootDisk(cfg); disk != "" && sizeGB(str(cfg[disk])) < s.DiskGB {
		if err := d.c.run(ctx, http.MethodPut, r.path()+"/resize", url.Values{"disk": {disk}, "size": {fmt.Sprintf("%dG", s.DiskGB)}}); err != nil {
			return driver.Image{}, err
		}
	}
	// a builder too is born behind the wall: it fetches its packages, and
	// hears nobody
	if _, err := d.wallUp(ctx, r); err != nil {
		return driver.Image{}, err
	}
	if err := d.settled(ctx, r); err != nil {
		return driver.Image{}, err
	}
	if err := d.c.run(ctx, http.MethodPost, r.path()+"/status/start", nil); err != nil {
		return driver.Image{}, err
	}
	if r, cfg, err = d.findImage(ctx, s.ID); err != nil {
		return driver.Image{}, err
	}
	return d.imageOf(ctx, r, cfg, s.ID)
}

// finish makes a builder, or a machine's clone, the image: stopped, its seed
// disc off, no tag left, a template.
func (d *Driver) finish(ctx context.Context, r resource, id string) (driver.Image, error) {
	if err := d.power(ctx, r, false); err != nil {
		return driver.Image{}, err
	}
	var cfg map[string]any
	if err := d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg); err != nil {
		return driver.Image{}, err
	}
	if num(cfg["template"]) != 1 {
		// no tags: its clones would carry them (tags are copied), a machine
		// born with this image's hangar-id
		drop := []string{"tags"}
		for k, v := range cfg {
			// the seed disc of its first boot (a clone's is its machine's); a
			// disk it no longer uses
			if strings.Contains(str(v), "media=cdrom") || strings.HasPrefix(k, "unused") {
				drop = append(drop, k)
			}
		}
		slices.Sort(drop)
		if err := d.c.run(ctx, http.MethodPost, r.path()+"/config", url.Values{"delete": {strings.Join(drop, ",")}}); err != nil {
			return driver.Image{}, err
		}
		if err := d.c.run(ctx, http.MethodPost, r.path()+"/template", nil); err != nil {
			return driver.Image{}, err
		}
	}
	if err := d.deleteSeed(ctx, id); err != nil {
		return driver.Image{}, err
	}
	r, cfg, err := d.findImage(ctx, id)
	if err != nil {
		return driver.Image{}, err
	}
	return d.imageOf(ctx, r, cfg, id)
}

// destroy removes a builder (or a template), and its seed disc.
func (d *Driver) destroy(ctx context.Context, r resource, id string) error {
	if on, err := d.running(ctx, r); err != nil {
		return err
	} else if on {
		if err := d.c.run(ctx, http.MethodPost, r.path()+"/status/stop", nil); err != nil {
			return err
		}
	}
	if err := d.c.run(ctx, http.MethodDelete, r.path(), url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}); err != nil {
		return err
	}
	return d.deleteSeed(ctx, id)
}

// ---- A save -----------------------------------------------------------------

func (d *Driver) SaveImage(ctx context.Context, id, guest string) (driver.Image, error) {
	im, err := d.save(ctx, id, guest)
	return im, d.engine(err)
}

func (d *Driver) save(ctx context.Context, id, guest string) (driver.Image, error) {
	r, cfg, err := d.findImage(ctx, id)
	switch {
	case err == nil && num(cfg["template"]) == 1:
		return d.imageOf(ctx, r, cfg, id)
	case err == nil:
		// a clone made, not yet the image: finish it
		return d.finish(ctx, r, id)
	case !errors.Is(err, driver.ErrNotFound):
		return driver.Image{}, err
	}
	src, err := d.find(ctx, guest)
	if errors.Is(err, driver.ErrNotFound) {
		return driver.Image{}, fmt.Errorf("%w: no machine %s here", driver.ErrRefused, guest)
	}
	if err != nil {
		return driver.Image{}, err
	}
	if src.Type != "qemu" {
		return driver.Image{}, fmt.Errorf("%w: %s is a container: this zone saves images of VMs", driver.ErrRefused, guest)
	}
	if on, err := d.running(ctx, src); err != nil {
		return driver.Image{}, err
	} else if on {
		return driver.Image{}, fmt.Errorf("%w: %s runs: an image is saved from a stopped machine", driver.ErrRefused, guest)
	}
	if held, err := d.heldBy(ctx, src); err != nil {
		return driver.Image{}, err
	} else if len(held) > 0 {
		return driver.Image{}, fmt.Errorf("%w: %s holds %s: an image is its system disk alone — detach them first", driver.ErrRefused,
			guest, strings.Join(held, ", "))
	}
	var made int
	d.mu.Lock()
	err = d.withFreeVMID(ctx, func(vmid int) error {
		made = vmid
		return d.c.run(ctx, http.MethodPost, src.path()+"/clone", url.Values{
			"newid": {strconv.Itoa(vmid)}, "name": {id}, "description": {marker(id)}, "pool": {d.images},
			"target": {d.node}, "full": {"1"}, "storage": {d.storage},
		})
	})
	d.mu.Unlock()
	if err != nil {
		return driver.Image{}, err
	}
	// the clone is the id just taken: /cluster/resources may not list it as
	// it is yet (read on the bench — a save found nothing it had just made)
	return d.finish(ctx, resource{VMID: made, Node: d.node, Type: "qemu", Pool: d.images, Name: id}, id)
}

// ---- Delete -----------------------------------------------------------------

var baseVolume = regexp.MustCompile(`base-(\d+)-disk-\d+/`)

func (d *Driver) DeleteImage(ctx context.Context, id string) error {
	r, cfg, err := d.findImage(ctx, id)
	if errors.Is(err, driver.ErrNotFound) {
		return d.engine(d.deleteSeed(ctx, id))
	}
	if err != nil {
		return d.engine(err)
	}
	err = d.destroy(ctx, r, id)
	if err != nil && num(cfg["template"]) == 1 && strings.Contains(err.Error(), "linked clone") {
		born := d.bornFrom(ctx, r.VMID)
		who := "machines born from it"
		if len(born) > 0 {
			who = strings.Join(born, ", ")
		}
		return fmt.Errorf("%w: %s still share its disk: delete them first — retire it meanwhile", driver.ErrRefused, who)
	}
	return d.engine(err)
}

// bornFrom lists the machines of the pool whose disk is a linked clone of a
// template's.
func (d *Driver) bornFrom(ctx context.Context, vmid int) []string {
	rs, err := d.resources(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range rs {
		if r.Pool != d.pool || r.Type != "qemu" || r.Template != 0 {
			continue
		}
		var cfg map[string]any
		if d.c.call(ctx, http.MethodGet, r.path()+"/config", nil, &cfg) != nil {
			continue
		}
		for _, v := range cfg {
			if m := baseVolume.FindStringSubmatch(str(v)); m != nil && m[1] == strconv.Itoa(vmid) {
				id := guestID(str(cfg["tags"]))
				if id == "" {
					id = "guest " + strconv.Itoa(r.VMID)
				}
				out = append(out, id)
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

// ---- The builder's first boot -----------------------------------------------

// bakeEnd is the driver's last step of every bake, run once cloud-init has
// finished (a unit ordered after cloud-final): cloud-init's verdict read, the
// disk made ready to be cloned — cloud-init's memory, the machine id, the ssh
// host keys and the package cache cleared, so each machine born from it is a
// first boot of its own — and the verdict said by the host name, which the
// driver reads through the guest agent.
const bakeEnd = `#!/bin/sh
# hangar: the last step of a bake — see the driver's images.go.
cat >/usr/local/sbin/hangar-bake-end <<'EOF'
#!/bin/sh
cloud-init status --wait >/dev/null 2>&1
rc=$?
{
	cloud-init status --long
	echo "--- the end of /var/log/cloud-init-output.log:"
	grep -vE '^[|+]|randomart|key fingerprint|^SHA256:|has been saved in|^Generating public/private' /var/log/cloud-init-output.log | tail -n 25
} >` + bakeLog + ` 2>&1
systemctl start qemu-guest-agent >/dev/null 2>&1
if [ "$rc" -eq 1 ]; then
	hostname ` + bakeFailedName + `
	exit 0
fi
rm -f /etc/systemd/system/hangar-bake-end.service /usr/local/sbin/hangar-bake-end
systemctl daemon-reload
command -v apt-get >/dev/null && apt-get clean
cloud-init clean --logs --seed --machine-id >/dev/null 2>&1 || cloud-init clean --logs
rm -f /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub /var/lib/systemd/random-seed
journalctl --rotate --vacuum-time=1s >/dev/null 2>&1
sync
hostname ` + bakeDoneName + `
EOF
chmod 755 /usr/local/sbin/hangar-bake-end
cat >/etc/systemd/system/hangar-bake-end.service <<'EOF'
[Unit]
Description=hangar: the end of a bake
After=cloud-final.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/hangar-bake-end
EOF
systemctl daemon-reload
systemctl start --no-block hangar-bake-end.service
`

// bakeUserData is a builder's user data: the recipe's, then bakeEnd, as a
// MIME multipart cloud-init reads part by part — the recipe's cloud-config
// (or script) untouched.
func bakeUserData(recipe []byte) []byte {
	const boundary = "==hangar-bake-7f3a2c=="
	kind := "text/cloud-config"
	if bytes.HasPrefix(bytes.TrimLeft(recipe, " \t\r\n"), []byte("#!")) {
		kind = "text/x-shellscript"
	}
	var b bytes.Buffer
	part := func(typ, name string, body []byte) {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; charset=\"utf-8\"\r\nMIME-Version: 1.0\r\nContent-Transfer-Encoding: base64\r\n"+
			"Content-Disposition: attachment; filename=\"%s\"\r\n\r\n", boundary, typ, name)
		enc := base64.StdEncoding.EncodeToString(body)
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc + "\r\n")
	}
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"%s\"\r\nMIME-Version: 1.0\r\n\r\n", boundary)
	part(kind, "recipe", recipe)
	part("text/x-shellscript", "zz-hangar-bake-end", []byte(bakeEnd))
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

var _ driver.Images = (*Driver)(nil)
