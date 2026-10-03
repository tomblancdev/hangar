package console_test

import (
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/browsertest"
	"github.com/tomblancdev/hangar/internal/stacktest"
	"github.com/tomblancdev/hangar/internal/testoidc"
)

// lab: a brain with the three plugins on the fake engine, a zone that counts
// room, two tiers — what a person finds behind the console.
const lab = `
data_dir: %[1]s
identity:
  oidc: {issuer: %[2]s, audience: hangar}
tiers:
  - name: operators
    groups: [ops]
    operator: true
    zones: ["*"]
    limits: {"*": unlimited, "images.source": [recipe, machine], "images.visibility": [private, shared, public],
             "machines.kind": [vm, container], "machines.class": [spot, guaranteed, guaranteed+spot]}
  - name: users
    groups: [users]
    zones: [lab]
    limits:
      machines.count: 2
      machines.vcpu_hours: 80
      machines.vcpu: 4
      machines.memory_gb: 8
      machines.disk_gb: 32
      machines.key_pairs: 2
      machines.kind: [vm, container]
      machines.class: [spot]
      volumes.count: 2
      volumes.size_gb: 20
      volumes.backup_gb: 5
      images.count: 2
      images.size_gb: 40
      images.source: [machine]
      images.visibility: [private, shared]
zones:
  - name: lab
    driver: fake
    endpoint: "%[1]s/zone-lab.json"
    room:
      memory_gb: 14
      grace: 1s
      reservations:
        - {name: priority, memory_gb: 8, while_running: "4100"}
plugins:
  - name: machines
    path: %[3]s
    args: [hangar-test-plugin, machines]
    zones: [lab]
    settings:
      images:
        debian-13: {vm: debian-13, container: "store:vztmpl/debian-13.tar.zst"}
  - name: volumes
    path: %[3]s
    args: [hangar-test-plugin, volumes]
    zones: [lab]
  - name: images
    path: %[3]s
    args: [hangar-test-plugin, images]
    zones: [lab]
    settings:
      recipes:
        debian:
          base: {vm: "store:import/debian-13.qcow2"}
          disk_gb: 10
          memory_mb: 2048
          user_data: "#cloud-config\npackages: [qemu-guest-agent]\n"
        broken:
          base: {vm: "store:import/debian-13.qcow2"}
          disk_gb: 4
          user_data: "#!/bin/sh\n# hangar-fake: fail this bake\n"
reconcile:
  every: 5s
`

// onAClock: the lab, and a recipe baked again by the brain itself, every
// minute — what "@<schedule>" names.
const onAClock = lab + `
schedules:
  - name: fresh
    cron: "* * * * *"
    as: {subject: recipes, groups: [ops]}
    create: {type: image, zone: lab, spec: {recipe: debian, shared_with: ["*"]}}
    keep: 2
    retire: retire
`

// stamp: the one word a resource's own page stamps it with.
const stamp = ".under .stamp"

const aliceKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGXj2Dq6dbWAg2wXCN9pDWc3cS/cJEHWIr0sRd4b3M5V alice@example.com"

// Every action the three plugins declare, asked from a real browser: the
// pages drawn from the catalogue, clicked as a person clicks them, under the
// page's own policy (no inline code, nothing loaded from elsewhere) — against
// a whole brain on the fake engine. What the brain refuses is read on the
// page as it says it: field by field, with the numbers.
func TestEveryActionFromABrowser(t *testing.T) {
	br := browsertest.Start(t)
	s := stacktest.New(t, lab, "machines", "volumes", "images")
	s.Iss.SignedIn(&testoidc.Claims{Subject: "alice", Name: "alice", Groups: []string{"users"}})
	p := br.Page(1280, 900, false)
	made := func() string { // the page of what was just asked for: its id
		t.Helper()
		p.Wait("the new resource's page", "location.hash.startsWith('#/r/')")
		return strings.TrimPrefix(p.Hash(), "#/r/")
	}
	done := func(action string) { t.Helper(); p.Sees(action + ": done") }

	// ---- signing in, at the provider
	p.Goto(s.URL + "/")
	p.Sees("COME IN")
	p.Shot("01-signin")
	p.Press("SIGN IN")
	p.Sees("YOUR LIMITS")
	p.Sees("machines vcpu hours")
	p.Sees("kept for priority")
	p.Shot("02-home-empty")
	p.Quiet()

	// ---- a key pair
	p.Press("+ Key pair")
	p.Sees("ASK FOR A KEY PAIR")
	p.Fill("name", "laptop") // every type has a name: the brain's own, never a spec's
	p.Fill("public key", aliceKey)
	p.Submit()
	kp := made()
	p.Sees("LAPTOP")
	p.Reads(stamp, "READY")
	p.Sees("SHA256:") // what the plugin read of it

	// ---- a machine: refused field by field, refused with the numbers, then made
	p.Open("#/t/machine/new")
	p.Sees("ASK FOR A MACHINE")
	p.Fill("name", "Not A Name")
	p.Fill("image", "debian-13")
	p.Submit()
	p.Sees("REFUSED")
	p.Wait("the refusal beside the field it names", `__t.field('name').closest('.field').querySelector('.field-err').textContent.length > 0`)
	p.Fill("name", "too-big")
	p.Fill("type", "t3.2xlarge")
	p.Submit()
	p.Sees("machines.vcpu")
	p.Sees("this asks for")
	p.Shot("03-refused")
	p.Fill("name", "dev-box")
	p.Fill("type", "t3.small")
	p.Choose("key pairs", "laptop", true)
	p.Fill("idle after", "30m")
	p.Fill("tags", "purpose=dev\nnote=<b>bold</b><img src=x onerror=document.title='in'>")
	p.Shot("04-new-machine")
	p.Submit()
	box := made()
	p.Sees("DEV-BOX")
	p.Reads(stamp, "RUNNING")
	p.Sees("vm · 2 cores · 2 GB · spot · debian-13") // its type's own sentence, beside the word it wears
	p.Wait("its key pair, by what it is called", `[...document.querySelectorAll('.kv a')].some((a) => a.getAttribute('href') === '#/r/`+kp+`' && a.textContent === 'laptop')`)
	p.Sees("purpose=dev")
	// what a person typed comes back as the letters they typed, never as markup
	p.Sees("note=<b>bold</b><img src=x")
	p.Wait("the tag shown as text", `!document.querySelector('.kv-val b, .kv-val img') && !document.title.startsWith('in')`)
	p.Shot("05-machine")

	// ---- its actions
	p.Press("stop")
	done("stop")
	p.Reads(stamp, "STOPPED")
	p.Press("resize")
	p.Sees("It can change what you hold")
	p.Fill("type", "")
	p.Fill("cores", "2")
	p.Fill("memory gb", "3")
	p.Shot("06-action-form")
	p.Submit()
	done("resize")
	// in its history: what the action was asked with, as the command line writes it
	p.Sees("cores=2 · memory_gb=3")
	p.Lacks(`{"cores"`)
	p.Press("start")
	done("start")
	p.Reads(stamp, "RUNNING")
	p.Press("reboot")
	done("reboot")
	p.Press("set idle after")
	p.Fill("idle after", "1h")
	p.Submit()
	done("set idle after")
	p.Press("keep awake")
	p.Fill("for", "8h")
	p.Submit()
	done("keep awake")
	p.Press("let sleep")
	done("let sleep")
	// over the limit, from an action's form
	p.Press("resize")
	p.Fill("memory gb", "64")
	p.Submit()
	p.Sees("REFUSED")
	p.Sees("machines.memory_gb")

	// ---- a volume, plugged into it
	p.Open("#/t/volume/new")
	p.Sees("ASK FOR A VOLUME")
	p.Fill("name", "data")
	p.Fill("description", "what the box keeps")
	p.Fill("size gb", "4")
	p.Pick("machine", "dev-box")
	p.Submit()
	vol := made()
	p.Sees("DATA")
	p.Reads(stamp, "ATTACHED") // the word its type wears, not the registry's "ready"
	p.Sees("4 GB · on dev-box")
	p.Sees("what the box keeps")
	// a second one of that name is refused beside the field, with the one that holds it
	p.Open("#/t/volume/new")
	p.Fill("name", "data")
	p.Fill("size gb", "1")
	p.Submit()
	p.Sees("REFUSED")
	p.Wait("the name already held, said beside the name", `__t.field('name').closest('.field').querySelector('.field-err').textContent.includes('`+vol+`')`)
	// renamed from its page: its id stays, the lists follow
	p.Open("#/r/" + vol)
	p.Press("rename")
	p.Fill("name", "Not A Name")
	p.Submit()
	p.Sees("REFUSED")
	p.Wait("the refusal beside the name", `__t.field('name').closest('.field').querySelector('.field-err').textContent.includes('a name is a-z')`)
	p.Fill("name", "store")
	p.Fill("description", "")
	p.Shot("06a-rename")
	p.Submit()
	p.Sees("renamed")
	p.Sees("STORE")
	p.Lacks("what the box keeps")
	p.Wait("where you are, by what it is called", `document.getElementById('here').textContent === 'store' && document.title.startsWith('store')`)
	// everything held, on the home page: the machine, and under it what hangs on it
	p.Open("#/")
	p.Wait("the volume and the key pair under their machine", `(() => { const rows = [...document.querySelectorAll('.section .rows a.row')].map((a) => [a.getAttribute('href'), a.className]);
	  const at = (id) => rows.findIndex((r) => r[0] === '#/r/' + id);
	  return at('`+box+`') >= 0 && !rows[at('`+box+`')][1].includes('hangs') && at('`+vol+`') > at('`+box+`') && rows[at('`+vol+`')][1].includes('hangs') &&
	    at('`+kp+`') > at('`+vol+`') && rows[at('`+kp+`')][1].includes('hangs last'); })()`)
	p.Sees("store")
	p.Shot("06b-home-held")
	p.Open("#/r/" + vol)
	p.Press("set backup")
	p.Choose("backup", "yes", true)
	p.Submit()
	done("set backup")
	// its backups are a size: growing it past the tier's is refused, with the numbers
	p.Press("resize")
	p.Fill("size gb", "6")
	p.Submit()
	p.Sees("volumes.backup_gb")
	p.Press("set backup")
	p.Choose("backup", "yes", false)
	p.Submit()
	done("set backup")
	p.Press("resize")
	p.Fill("size gb", "6")
	p.Submit()
	done("resize")

	// the machine's page names what is plugged into it — and is not deleted while it is
	p.Open("#/r/" + box)
	p.Wait("the volume under what names it", `[...document.querySelectorAll('a.row')].some((a) => a.getAttribute('href') === '#/r/`+vol+`')`)
	p.Press("DELETE…")
	p.Sees("DELETE?")
	p.Shot("07-delete-asked")
	p.Press("YES, DELETE IT")
	p.Sees("REFUSED")
	p.Sees("detach it first")

	// a second machine, and the volume moved to it, unplugged, plugged back
	p.Open("#/t/machine/new")
	p.Fill("name", "other")
	p.Fill("type", "t3.micro")
	p.Fill("image", "debian-13")
	p.Submit()
	other := made()
	p.Reads(stamp, "RUNNING")
	p.Open("#/r/" + vol)
	p.Press("move")
	p.Pick("machine", "other")
	p.Submit()
	done("move")
	p.Press("detach")
	done("detach")
	p.Press("attach")
	p.Pick("machine", "other")
	p.Submit()
	done("attach")
	p.Press("detach")
	done("detach")

	// ---- an image, saved from the stopped machine; shared; a machine born from it
	p.Open("#/r/" + box)
	p.Press("stop")
	done("stop")
	p.Open("#/t/image/new")
	p.Sees("ASK FOR AN IMAGE")
	p.Fill("name", "my-base")
	p.Pick("machine", "dev-box")
	p.Submit()
	img := made()
	p.Reads(stamp, "AVAILABLE") // made by the next looks of the brain: the page follows by itself
	p.Press("share")
	p.Choose("shared with", "users", true)
	p.Submit()
	done("share")
	p.Sees("shared with")

	// the volume and the second machine go; a machine is born from the image
	p.Open("#/r/" + vol)
	p.Press("DELETE…")
	p.Press("YES, DELETE IT")
	done("delete")
	p.Open("#/r/" + other)
	p.Press("DELETE…")
	p.Press("YES, DELETE IT")
	done("delete")
	p.Reads(stamp, "DELETED")
	p.Open("#/t/machine/new")
	p.Fill("name", "from-image")
	p.Fill("type", "t3.micro")
	p.Pick("image id", "my-base")
	p.Submit()
	born := made()
	p.Reads(stamp, "RUNNING")
	p.Wait("the image it was born from, as a link", `[...document.querySelectorAll('.kv a')].some((a) => a.getAttribute('href') === '#/r/`+img+`')`)

	// retired: no machine is born from it any more — the form offers it no longer
	p.Open("#/r/" + img)
	p.Press("retire")
	done("retire")
	p.Reads(stamp, "RETIRED")
	p.Open("#/t/machine/new")
	p.Sees("ASK FOR A MACHINE")
	for _, o := range p.Options("image id") {
		if strings.HasPrefix(o, "my-base") && !strings.Contains(o, "retired") {
			t.Fatalf("a retired image is offered as any other: %q", o)
		}
	}

	// ---- the lists, the operations
	p.Open("#/t/machine")
	p.Sees("MACHINES")
	p.Sees("dev-box")
	p.Sees("from-image")
	p.Lacks("other")
	p.Shot("08-machines")
	p.Press("deleted")
	p.Sees("other")
	p.Open("#/operations")
	p.Sees("OPERATIONS")
	p.Sees("keep awake")
	p.Open("#/")
	p.Sees("4 things") // the key pair, two machines, the image
	p.Shot("09-home")

	// ---- an API token: shown once
	p.Open("#/tokens")
	p.Fill("name", "ci")
	p.Submit()
	p.Sees("KEEP IT")
	p.Wait("the secret", `document.querySelector('.secret').textContent.startsWith('hgr_')`)
	p.Shot("10-token")
	p.Press("Revoke")
	p.Sees("REVOKED")

	// ---- everything let go of
	for _, id := range []string{born, box, img, kp} {
		p.Open("#/r/" + id)
		p.Press("DELETE…")
		p.Press("YES, DELETE IT")
		done("delete")
	}
	p.Open("#/")
	p.Sees("Nothing yet")

	// ---- signing out
	p.Press("Sign out")
	p.Sees("COME IN")
	p.Quiet()
	if n := s.Iss.Revoked(); n != 1 {
		t.Fatalf("the provider was told of %d sign-outs", n)
	}
}

// Two people, two browsers: an operator bakes an image and shares it with
// everyone, and sees everyone's resources; a user sees it, has a machine
// born from it, and may not change it. A bake that failed is baked again
// from its page. And the user's pages on a phone.
func TestTwoPeopleAndAPhone(t *testing.T) {
	br := browsertest.Start(t)
	s := stacktest.New(t, onAClock, "machines", "volumes", "images")
	made := func(p *browsertest.Page) string {
		t.Helper()
		p.Wait("the new resource's page", "location.hash.startsWith('#/r/')")
		return strings.TrimPrefix(p.Hash(), "#/r/")
	}

	// ---- the operator
	s.Iss.SignedIn(&testoidc.Claims{Subject: "olive", Name: "olive", Groups: []string{"ops"}})
	op := br.Page(1280, 900, false)
	op.Goto(s.URL + "/console/#/t/image/new")
	op.Press("SIGN IN")
	// back where they were going: the form, not the home page
	op.Sees("ASK FOR AN IMAGE")
	op.Fill("name", "debian-baked")
	op.Fill("recipe", "debian")
	op.Choose("shared with", "everyone", true)
	op.Submit()
	baked := made(op)
	op.Reads(stamp, "AVAILABLE")
	op.Sees("shared with")
	// what it was baked from is an object, a script in it: listed as JSON a
	// person reads, the script as its own lines — never one line of \n
	op.Wait("its recipe, listed", `(() => { const l = [...document.querySelectorAll('.kv .json-line')].map((x) => x.textContent);
	  return l.includes('"disk_gb": 10,') && l.includes('#cloud-config') && l.includes('packages: [qemu-guest-agent]'); })()`)
	op.Lacks(`\npackages`)
	op.Shot("19-image-recipe")
	// a bake that fails on its own waits to be asked again
	op.Open("#/t/image/new")
	op.Fill("recipe", "broken")
	op.Submit()
	made(op)
	op.Reads(stamp, "FAILED")
	op.Press("rebake")
	op.Sees("rebake: done")
	op.Reads(stamp, "FAILED")
	op.Quiet()

	// ---- the user, on a phone
	s.Iss.SignedIn(&testoidc.Claims{Subject: "alice", Name: "alice", Groups: []string{"users"}})
	ph := br.Page(390, 844, true)
	ph.Goto(s.URL + "/console/")
	ph.Sees("COME IN")
	ph.Shot("20-phone-signin")
	ph.Press("SIGN IN")
	ph.Sees("YOUR LIMITS")
	// the operator's image is hers to see and to name — not to change
	ph.Open("#/r/" + baked)
	ph.Sees("DEBIAN-BAKED")
	ph.Sees("only its owner changes it")
	ph.Lacks("DELETE…")
	ph.Wait("no action offered on what is someone else's", `!__t.press('retire') && !__t.press('share')`)
	ph.Open("#/t/machine/new")
	ph.Fill("name", "pocket")
	ph.Fill("type", "t3.micro")
	ph.Pick("image id", "debian-baked")
	// on a phone a form's fields come one under the other, never squeezed side by side
	ph.Wait("the fields one per line", `(() => { const f = [...document.querySelectorAll('.form > .field')].map((e) => e.getBoundingClientRect());
	  return f.length > 3 && f.every((r, i) => i === 0 || r.top >= f[i - 1].bottom) && f.every((r) => r.width > 300); })()`)
	ph.Shot("21-phone-form")
	ph.Submit()
	pocket := made(ph)
	ph.Reads(stamp, "RUNNING")
	ph.Shot("22-phone-machine")
	ph.Open("#/t/machine")
	ph.Sees("pocket")
	ph.Shot("23-phone-list")
	ph.Open("#/")
	ph.Sees("1 thing")
	ph.Shot("24-phone-home")
	// nothing runs off the side of a phone
	var wide int
	if err := ph.Eval("document.documentElement.scrollWidth - window.innerWidth", &wide); err != nil || wide > 0 {
		t.Fatalf("the page is %d px wider than the phone (%v)", wide, err)
	}
	ph.Quiet()

	// ---- the newest a schedule made: offered by name once the brain's clock
	// has baked one (within its minute), and the machine born keeps the id
	ph.Patience = 100 * time.Second
	ph.Wait("the schedule's first image offered as the newest", `(() => {
	  if (!location.hash.endsWith('/new')) { location.hash = '#/t/machine/new'; return false; }
	  const sel = __t.field('image id');
	  if (sel && [...sel.options].some((o) => o.value === '@fresh')) return true;
	  location.hash = '#/t/machine'; return false; })()`)
	ph.Fill("name", "newest")
	ph.Fill("type", "t3.micro")
	ph.Pick("image id", "@fresh")
	ph.Submit()
	made(ph)
	ph.Reads(stamp, "RUNNING")
	ph.Wait("the image it was born from, by its id", `[...document.querySelectorAll('.kv-row')].some((r) => r.firstChild.textContent === 'image id' && /^img-[0-9a-f]{17}$/.test(r.lastChild.textContent))`)
	ph.Quiet()

	// ---- the operator sees everyone's, and whose
	op.Open("#/t/machine")
	op.Sees("pocket")
	op.Sees("owner alice")
	op.Open("#/operations")
	op.Sees("everyone's")
	op.Sees("alice")
	// and acts on it: an operator's reach, in their own name in the audit
	op.Open("#/r/" + pocket)
	op.Press("stop")
	op.Sees("stop: done")
	if !strings.Contains(s.Logs.String(), `"actor":"olive"`) || !strings.Contains(s.Logs.String(), pocket) {
		t.Error("the audit does not name the operator for what she did to alice's machine")
	}
	op.Quiet()
}

// A field that is an object, from a real browser, under the page's own
// policy: typed in an editor — a bracket brings its pair, a new line its
// indentation — that says where it breaks as it is typed; refused by the
// brain inside what was typed, the place said in words and its line lit;
// read back as JSON a person reads — on its page, in an action's form, in
// its history.
func TestAnObjectTypedAndRead(t *testing.T) {
	br := browsertest.Start(t)
	s := stacktest.New(t, tokensOnly, "toy")
	p := br.Page(1280, 900, false)
	p.Goto(s.URL + "/console/")
	p.Sees("COME IN")
	p.Fill("API token", s.Token(t, "alice", "users"))
	p.Press("SIGN IN")
	p.Sees("YOUR LIMITS")
	// the editor's own code is fetched by a form that has such a field, never before
	fetched := `performance.getEntriesByType('resource').some((e) => e.name.endsWith('/vendor/codemirror.js'))`
	p.Wait("a page with no JSON to type, and no editor fetched for it", "!("+fetched+")")
	p.Open("#/t/box/new")
	p.Sees("ASK FOR A BOX")
	slot := `__t.field('labels').closest('.field').querySelector('.field-err').textContent`
	// the line the editor lights: its number, in the root the editor sits in
	lit := func(n string) string {
		return `(() => { const b = document.querySelector('.code-box'); const l = b && b.shadowRoot ? [...b.shadowRoot.querySelectorAll('.cm-lineNumbers .cm-lit-n')].map((x) => x.textContent).join() : 'no editor'; return l === '` + n + `'; })()`
	}

	// ---- it is an editor: typed as a keyboard types, a brace brings its pair, Enter its indentation
	p.Wait("the editor, in the box", `(() => { const b = document.querySelector('.code-box'); return !!b && !!b.shadowRoot && !document.querySelector('.code textarea'); })()`)
	p.Wait("fetched by the form that needs it", fetched)
	// reached as a person reaches a field: by its label
	if err := p.Eval(`[...document.querySelectorAll('label.lbl')].find((l) => l.textContent === 'labels').click()`, nil); err != nil {
		t.Fatal(err)
	}
	p.Type("{", true)
	p.Wait("a pair, and the line between them pushed in", `__t.field('labels').value === '{\n  \n}'`)
	p.Type(`"team": "infra"`, false)
	p.Reads(".code-says", "reads as JSON · 3 lines")

	// ---- what is not JSON is said as it is typed, and asks nothing
	p.Fill("name", "labelled")
	p.Fill("labels", "{\n  \"team\": \"infra\"\n  \"stage\": 3\n}")
	p.Reads(".code-says", "line 3, column 3: a comma or a } is expected")
	p.Wait("the line it breaks at, lit", lit("3"))
	p.Shot("30-json-broken")
	p.Submit()
	p.Wait("the same, beside the field", slot+` === 'line 3, column 3: a comma or a } is expected'`)
	if h := p.Hash(); h != "#/t/box/new" {
		t.Fatalf("what does not read was asked for: the page is %s", h)
	}

	// ---- what the brain refuses inside it: where, in words, and its line lit
	p.Fill("labels", "{\n  \"team\": \"infra\",\n  \"stage\": 3\n}")
	p.Reads(".code-says", "reads as JSON · 4 lines")
	p.Submit()
	p.Sees("REFUSED")
	p.Wait("the place inside, in words", slot+`.startsWith('stage: ')`)
	p.Wait("the line the brain points at, lit", lit("3"))
	p.Shot("31-json-refused")
	p.Fill("labels", "{\n  \"team\": \"infra\",\n  \"stage\": \"prod\"\n}")
	p.Wait("a line no longer lit once it is typed over", lit(""))
	p.Submit()
	p.Wait("the new resource's page", "location.hash.startsWith('#/r/')")
	p.Sees("LABELLED")
	p.Reads(stamp, "RUNNING")
	p.Wait("its labels, short enough for one line", `[...document.querySelectorAll('.kv .json-line')].some((l) => l.textContent === '{ "stage": "prod", "team": "infra" }')`)

	// ---- an action that takes an object: what it is now listed, the box, tidy
	p.Press("label")
	p.Wait("what it is now, listed under the field", `(() => { const n = document.querySelector('.action-panel .now .json'); return !!n && n.textContent.includes('"team": "infra"'); })()`)
	p.Fill("labels", `{"team": "infra", "stage": "prod", "owner": "alice", "since": "2026", "why": "a label for every question"}`)
	p.Press("tidy")
	p.Wait("written again, indented", `__t.field('labels').value.split('\n').length === 7`)
	p.Shot("32-json-action")
	p.Submit()
	p.Sees("label: done")
	// more than a line holds: listed, on its page and under its line in the history
	p.Wait("its labels, listed", `[...document.querySelectorAll('.kv .json-line')].some((l) => l.textContent === '"why": "a label for every question"')`)
	p.Wait("what the action was asked with, listed under its line", `[...document.querySelectorAll('.ops .json-line')].some((l) => l.textContent === '"owner": "alice",')`)
	p.Lacks(`{"labels"`)
	p.Shot("33-json-read")
	p.Quiet()
}
