# Changelog

## v0.6.1 — 2026-10-06

- **Le terminal rouvert — a terminal opened again draws.** A machine keeps
  no picture of its screen, and nothing on the way to the page does: a shell
  left on its port is there at the next opening, and says nothing until a
  key is pressed. `v0.6.0`'s page opened a terminal anew every time it was
  shown — the whole window and back included — and showed an empty screen
  and a cursor (read in a real browser on a real Proxmox VE: `open`,
  47 × 31, not a character after six seconds). Three things answer it
  ([docs/console.md](docs/console.md#a-machines-terminal)):
  **Carried, not opened again.** *Full screen* and *Leave full screen* move
  the same terminal — its screen, the line being typed, its socket — and so
  does any other way back to the machine's page (the name in the terminal's
  bar, the browser's own). The brain's audit holds one opening where it held
  three; left for anywhere else, a terminal is let go of at once, as before.
  **Said, not typed.** A terminal that *is* opened again — the page loaded
  anew, a phone, `Close` then `terminal` — and hears nothing from its
  machine for four seconds says so on a slip over its screen, with
  a **Redraw** key: it types Ctrl-L, which a shell, an editor or a pager
  answers by drawing its screen, the line begun on it too. The page types
  nothing by itself: a key pressed for someone lands in whatever reads the
  keyboard then — a password being asked, a file being written. A first
  opening never shows the slip: a machine about to sign someone in asks the
  terminal its size every two seconds, and the four are counted from the
  brain's own opening, which the console now says in the socket
  (`{"opened":true}`, before anything the machine says) — the page said
  « open » at the console's accept, before the brain had been asked. And a
  screen the page kept across an ending forgets a program's asking for
  focus reports before its machine is opened again: taking the keyboard
  back typed `ESC [ I` into whatever read it.
  **A window's size no longer costs the screen.** The line the cursor is on
  is wrapped and unwrapped with the others: no shell on a serial port is
  told its window changed, so none draws its line again, and a window
  narrowed under a long line cut it at the edge for good. And a window with
  no room for a terminal — under 16 columns or 4 rows, a moment on its way
  to another size — is not fitted to: a window of one pixel fitted the grid
  to two columns, and the line being typed was left with two characters.
  The bar's « resized: exit signs you in at this size » is measured from the
  machine's own asking, and leaves when it asks again — after that `exit`.
  **A page that was left opens nothing**: a machine's page whose first
  answer came after it was left went on to open the terminal its address
  asked for, on a screen nobody could see or close.
- **The fake engine's port has the real one's form.** Its other end is its
  guest's and its boot's, not a console's: a shell left there is there at
  the next opening, with what was typed and not entered; a second opening
  hears nothing; a login asked at boot was asked of nobody, and asks again
  at Enter; Ctrl-L draws a shell's line again; a shell keeps the size of its
  sign-in, whatever its window does afterwards. It greeted at every opening
  before, and took every window's size — which is why every test of `v0.6.0`
  was green over an empty screen. Held by `TestAPortOutlivesItsConsole`, and
  by the page's test in a real browser, which now carries a half-typed line
  to the whole window and ends it there, comes back by three ways, narrows
  the window under a long line, and reads the slip and its key on a phone —
  where Redraw draws a line left half typed and does not enter it;
  `TestBenchATerminalInThePage` does the same on a real Proxmox VE, with a
  real shell.
- **On a cluster, the terminal's proxy runs where the API is asked**
  ([docs/proxmox.md](docs/proxmox.md#a-machines-terminal)): neither call is
  passed on to the guest's node, and the proxy's last leg is the cluster's
  own ssh from the one to the other. A zone's `endpoint` need not be the
  node its machines run on; what a terminal needs between the nodes is what
  a migration needs. Read on a three-node cluster — the bench has one node.
- **The bench: what a container was told, waited for.**
  `TestBenchAMachineBornBehindItsWall` asked a container its address once,
  right after its start, and was red twice in four runs: its init writes it
  a moment later on a bench that has run other tests.
  `TestBenchTwoNetworksAndAJump` asked the same way. Both ask until it is
  there (`toldInside`), and their logs say at which read it was; the wait
  has a test of its own, which needs no bench
  (`TestToldInsideAsksUntilItIsThere`).

Nothing to do at an upgrade: no setting, no migration, nothing in the API or
in a plugin's contract. A page left open keeps the scripts it loaded: load
it again.

## v0.6.0 — 2026-10-05

- **Le terminal — a machine entered from the page.** A machine's own screen
  and keyboard, in the console: no key, no client, nothing installed in the
  machine or on anyone's computer
  ([docs/console.md](docs/console.md#a-machines-terminal),
  [docs/proxmox.md](docs/proxmox.md#a-machines-terminal),
  [ARCHITECTURE.md](ARCHITECTURE.md) §4 « Streams »). **A VM that names no
  key pair is born with its terminal open**: its owner opens it and lands in
  a shell as the machine's user, nothing asked — the gateway and its second
  factor already said who they are. One that names a key asks a login,
  unless its form says `terminal: open`; it is set at a machine's birth
  (`terminal`, `open` or `login`). **Its owner alone**: an operator sees a
  machine, stops it, deletes it, and is refused its terminal — as is anyone
  it is shared with, and a token that only reads. **One place at a time**:
  opened again on another tab or a phone, it moves there and the first is
  told. **Held on a credential, and ended with it**: what its opening asked
  is asked again every half minute — a token revoked, a person out of every
  group, a sign-in the provider no longer renews, each closes it, with the
  reason. **Audited at both ends, never in between**: one line when it
  opens, one when it closes — how long, how many bytes each way, why — and
  not a word of what passed.
  **In the page** the `terminal` key sits among a machine's keys on its
  owner's page and unfolds a screen under them; *Full screen* is the same
  screen alone, at an address of its own, and *Leave full screen* comes back
  with it still shown; a notice over the screen says why it ended, in the
  brain's words; a phone gets the keys its keyboard lacks (Esc, Tab, Ctrl,
  the arrows). The screen is **xterm.js, vendored** (`tools/terminal/`,
  checked byte for byte by CI as the editor is) — and **the page's policy
  is as it was**: the three `<style>` the library writes become sheets the
  document adopts; the policy names one thing more, the console's own
  socket.
  **In the contract**, the first call that is not a request and its answer:
  **a stream** — `Open`, bytes both ways for as long as both ends hold it. A
  type declares its streams beside its actions; the API serves one as a
  WebSocket, `GET /v1/resources/{id}/streams/{stream}`, with the same bearer
  token as everything else; a refusal is an ordinary problem document before
  anything is switched, an end the socket's last words (4000 with the
  reason, 4001 taken elsewhere); a client whose token is short-lived hands
  the next one over the socket (`{"token":"…"}`). The rules — its owner
  alone, one at a time, held on its credential, audited at both ends — hold
  for any plugin's stream; the toy plugin's `echo` is the one to copy. The console passes a stream on as
  it passes every call, with the person's own token; it takes a socket only
  from its own pages, says a refusal inside it, and ends what is open on a
  sign-in when that sign-in ends.
  **On Proxmox VE** a terminal is a VM's serial port, through the node's own
  terminal proxy — and **one privilege more in the machines pool's role,
  the operator's to give: `VM.Console`**. Whoever takes the brain can then
  type in every running machine whose terminal was born open; a zone whose
  role lacks it offers no terminal, and nothing else changes (the new flag
  is **`guest.console`**, advertised only where the token holds the
  privilege on its pool). A machine born open is handed **one step at its
  first boot, beside its owner's user data and never in it**: its port then
  waits for a terminal at the other end, asks it its size — a serial port
  carries none —, and signs the machine's user in. No shell sits open on a
  port nobody holds, and the one a person gets fits their window, with its
  colours. The step is that machine's alone: an image saved from it does not
  open the machines born from it. A container has no terminal.
  **Proved on a real Proxmox VE**, through the plugin's own fenced token
  (`TestBenchAVMsConsole`, `TestBenchATerminalInThePage`): a VM made with no
  key from the page and entered 8 to 16 s after its terminal was opened (five
  runs) — as its user, with sudo, at the window's size; a shell on its port
  20 to 28 s after its create, its owner's own user data run beside the
  step; `exit` signed in again at the size the terminal had by then; a VM born with a key asked a
  login; another person found neither the machine nor its terminal, an
  operator the machine and no way in; opened again on a phone it moved
  there and the desk was told, then took it back; a machine stopped under
  its terminal ended it with the reason, which Proxmox itself does not say,
  and one rebooted from its page came back on the same screen, signed in
  again, in a new shell; nothing left running on the node; the audit held the opening and the closing, and
  nothing typed was in the brain's log.
  **To upgrade:** nothing changes until the operator adds `VM.Console` to
  the machines pool's role and restarts the brain. Machines made before keep
  the port they were born with (a login asked); `apply` leaves a machine
  whose file names no `terminal` where it is. A plugin built against the
  earlier SDK is unchanged: a stream is a call it does not answer.
  **Named, not hidden:** root on the engine's node can open any guest's
  console — that is the operator's own machine; an open terminal is not what
  `idle_after` counts; a window resized after the sign-in is taken at the
  next one (`exit`); with no canvas to draw on, a 24-bit colour reads as the
  default colour; the step is for images that run cloud-init under systemd.

## v0.5.0 — 2026-10-04

- **Les réseaux — a network of one's own.** A new plugin, **`networks`**, and
  its type **`network`** (`net-…`): a private network only its machines are
  on, each given its address there, out through **a gateway the cloud
  makes**, in by its owner's jump alone
  ([ARCHITECTURE.md](ARCHITECTURE.md) §7, [docs/proxmox.md](docs/proxmox.md#networks)).
  Two owners are apart because no wire joins them, not because a rule says
  so; the zone's shared lane then carries the cloud's own gateways, and no
  machine of a person's. **A machine names its network at its birth**
  (`network`, by name or id: one of its owner's, or one shared with them) and
  holds one card, there, for its life. **One that names none is put on its
  owner's network called `default`**, made by the brain at their first
  machine — an ordinary create, in their name, within their tier
  (`networks.count`), audited as theirs: a household never learns the word.
  **The way in is a jump**: `ssh -J jump@<the gateway> user@<the machine>`,
  with a key pair the network names (`key_pairs`; `set_key_pairs` changes
  them) — it opens that network and nothing else, and gives no shell, no
  command and no file on the gateway. A network may be **shared**
  (`shared_with`, `share`: its people put machines on it, only its owner
  changes it; `networks.visibility`), and **is not deleted while a machine
  stands on it** (409 `members`, naming them). Its page says its range,
  where its keys jump through, and whether its gateway is up.
  **On Proxmox VE** a zone that says `net_bridge` (with `net_tag`,
  `net_block`, `net_size`, `net_vmids`, `net_pool`, `net_address`,
  `net_archive`) cuts networks on a bridge of its own — no port, VLAN-aware —
  one tag each. **A network is its gateway guest**, and every number of it is
  *derived from that guest's own id, never counted*: its tag, its range, its
  gateway's two addresses — and a machine's address from the machine's own
  id. Nothing is allocated, nothing kept, nothing made on the cluster at a
  request's time. **A gateway keeps nothing and is never patched**: born from
  the operator's archive (a recipe the product ships,
  `tools/gateway/build.sh`, and `/gateway-build.sh` in the image: nftables
  and sshd, three static files and a user), with the keys its network names; one born with others, or from
  another archive, or gone, **is made again at the same id**. **It runs only
  while a machine of its network runs** — started before the first, stopped
  after the last, put back at every look — so a node with nothing running
  sleeps. Two keys, kept apart: the networks plugin's token makes gateways
  and reads the machines; the machines plugin's gains the power of a gateway
  and nothing of its config. The new flag is **`net.private`**.
  **In the core**, two marks a schema may put on a reference:
  `x-hangar-member` (what it names is not deleted while it has members; the
  member goes freely, and may name what is shared) and `x-hangar-default`
  (left out by a create, it names the owner's own of that name, made first).
  **Proved on a real Proxmox VE**, through the plugins' own fenced tokens
  (`TestBenchTwoNetworksAndAJump`, `TestBenchANetworkThroughTheAPI`): two
  networks never saw each other and one saw itself; a machine fetched the
  web through its gateway while the lane saw the gateway's address only;
  nothing came in, even from a node given a route to the network; the
  owner's key jumped to its machines — a container, a VM told its address on
  its first-boot disc — and to nothing else — the lane's node,
  the other gateway, a machine of the other network, the gateway itself and
  the web each refused —, got no shell, no command, no file, no root; a key
  the network does not name was denied; a gateway was made again on a key
  change in 23 s, rested when its last machine stopped, and was started
  again by the brain's look after a hand stopped it. **What it costs:** a
  small container per network (128 MB while it runs, booked in the zone's
  guaranteed pool), a network's birth 18 s, a person's first machine accepted
  18 s later than the next ones. *Not in this version: rules inside a
  network, two networks joined, a network with no way out, one across two
  nodes.*
- **`hangar check` reads a zone's options** — and so does a start: each
  zone's options are held against its driver's own reading of them, the
  engine not reached. A file whose zone could never open — an address past
  its range, a security group that is no name — came back « sound », the
  driver refusing it only when the zone was opened; it is refused now, in
  the driver's words, with no network at all. **An option the Proxmox driver
  does not know is refused**: a word mistyped was a lever left off, and
  nobody told. *(A config that carried an unknown zone option no longer
  starts: `hangar check` names it.)*

## v0.4.0 — 2026-10-04

- **La carte et le mur — a machine is given its address, and is born behind
  a wall.** Two things a Proxmox zone may now say, each off unless said
  ([docs/proxmox.md](docs/proxmox.md#addresses-and-the-wall)).
  **`subnet`**: every guest is given its address at its birth, and none asks
  a DHCP. The address is *derived, never counted* — the first address of the
  zone plus the guest's own VMID less the first of the zone's: the one
  number Proxmox hands out with no race, so there is no allocator, no state
  and nothing two creates can be given twice. A container is told on its
  card, a VM on its seed disc (`network-config`, its card matched by the MAC
  it keeps), a bake's builder as a VM; a machine's page says it as
  `address`, running or not. **`firewall: on`**: every guest the driver makes
  stands behind Proxmox's firewall *before its first start*, alone — nothing
  comes in but what the operator's own security groups let
  (`firewall_groups`), and it sends only as itself: its card's MAC and the
  one address it was given, which Proxmox makes an ARP filter too. The
  driver owns the guest's whole firewall file (a clone is born with its
  template's — a door left there does not come with it), and **every look
  puts back what a hand changed**, naming it: `repaired`, « put back: [wall
  (options, rules)] ». A guest born before, with a lease, is walled as it
  runs — its open connections go on — and pinned to the zone's subnet until
  it is made again (`wall: range`; a guest given its address: `wall: exact`).
  A zone that turned its wall on advertises the new flag `net.firewall`; the
  tokens need nothing they did not hold. **What it costs:** Proxmox applies a
  guest's firewall on its own pass, every ten seconds, and a start does not
  ask it to — read on a bench, a guest started right after its wall was
  written answered its neighbour, and went on answering once its rules were
  in. So a guest whose wall has just been written starts **fifteen seconds
  later**: at a birth, and at no other start. Proved on a real Proxmox VE
  (`TestBenchAMachineBornBehindItsWall`, `TestBenchAVMBornBehindItsWall`): a
  neighbour that pinged a machine's address from before its birth was never
  answered; as another address, as the gateway and from another MAC it was
  dropped on its own card, where the same lie from a guest with no wall
  passed; a wall opened by hand let the neighbour in, and the next look shut
  it. The fake engine keeps a wall too, so the plugin's own tests hold it.
  *No rule is written by anyone through the product in this version: the
  wall is the zone's.*

## v0.3.1 — 2026-10-03

- **Le JSON lisible — the console's JSON.** A value that does not read on
  one line — an object, a list that holds one, a text of several lines — was
  one raw line (an image's `from`: its recipe and its first-boot script, two
  and a half thousand characters of it). It is **listed as JSON a person
  reads**: indented, what is short kept on one line, a script as its own
  lines, an id inside still a link, the rest folded past twelve lines,
  *copy* for the whole of it — on a resource's page, in « Now: » and in an
  operation's line, where plain params read as the command line writes them
  (`cores=4 · memory_gb=8`). And where JSON is typed, **an editor** instead
  of three blank lines — CodeMirror 6: numbered lines, colours, pairs,
  folds, a search, several cursors —, with the console's own verdict under
  it as it is typed: it reads, or the line and column it breaks at and what
  was expected there (the same answer as the browser's own reader on
  whether it is JSON), that line lit; and a refusal that points inside what
  was typed (`/labels/stage`) said in words, its line lit too. **It is the
  app's first dependency, and it is vendored**: one file, built from pinned
  versions by `tools/editor/build.sh` and held to them by CI, fetched only
  by a form that has a JSON field, and set in a root of its own so the
  page's policy stays as strict as it was
  ([docs/console.md](docs/console.md#the-one-file-the-app-did-not-write)).
  The toy plugin gains what proves it: `labels`, a field that is an object,
  and `label`, an action that takes one.

- **The bench as machines** (`tools/bench/shards.sh`, `tools/bench/pve.sh` —
  tools: the image is `0.3.0`'s). Where a zone has cores to spare, a bench
  is a machine of it: a Debian 13 made a Proxmox VE as Proxmox documents
  it, saved once as an image by its owner (no operator, no recipe), born
  again in under a minute for each shard of the tests; the shards, balanced
  by the tests' measured times, run at once
  ([docs/proxmox.md](docs/proxmox.md#the-bench-as-machines)). Proved on a
  real zone, the benches made with `0.3.0`'s own `cpu: host` and
  `virtualization`: **the twenty tests in 18 min 23 s on three of them, eighty on
  a laptop's one.** What a machine made a Proxmox VE needs that an
  installer's disk has by itself is written beside each line of `pve.sh` —
  its guests' way out among them: the node told to forward, to answer
  their names, and to answer their clock.

## v0.3.0 — 2026-10-03

**A machine's processor.** What the first heavy runs on a real zone found: a
VM the product made saw a 2003 processor. The image:
`ghcr.io/tomblancdev/hangar:0.3.0`.

- **The zone's own model is no longer an accident.** The Proxmox VE driver
  never wrote a VM's processor, and through the API that is `kvm64`: no AES,
  no SSE4.2, no AVX. It now writes it on every VM — the zone's `cpu_model`,
  **`x86-64-v2-AES` unless said** (what Proxmox's own form picks) — and on
  every bake's builder. A machine made before keeps the processor it has.
- **`cpu: host`** — a VM that asks for it sees its host's own processor,
  every instruction of it. Measured on a real zone: 2 to 4 % on a Go suite,
  **a fifth on one that runs a database and an object store**. It then runs
  on that kind of host only. Set at its birth.
- **`virtualization: true`** — a VM that may run VMs of its own. It goes
  with `cpu: host`, and it is a field of its own because it is a risk of
  its own: **a VM that does not ask is given none, whatever its processor**
  (Proxmox's `nested-virt` flag, written either way on every VM). Set at its
  birth.
- **Each is a tier's to open, nobody's by default**: two choices,
  `machines.cpu: [host]` and `machines.virtualization: [nested]`, asked of
  a tier only by a machine that wants them. A tier that names neither gives
  neither, in words — and goes on making every other machine: **no tier
  needs a line changed** (one that says `"*": unlimited` opens both, as it
  opens everything).
- **`cpu_weight`** (1 to 100) — a machine's share of the cores when others
  want them too, a container's as a VM's: 25 yields to the others, and loses
  nothing while cores are free. It only yields, so no tier is asked. Changed
  while the machine runs — `set_cpu_weight`, and `apply` asks for that step —
  and kept true on the engine at every look.
- **A VM's disks give space back** (`discard`): what is deleted inside a
  machine's system disk or a block volume returns to the storage — a thin
  disk no longer only grows. On every new machine (whatever its image's
  age), every bake, every new volume and every volume that moves.
- **A volume's `in_guest` says the truth.** It named a path under
  `/dev/disk/by-id` carrying the volume's serial; no such path exists — udev
  names a QEMU disk after the slot it is plugged in. It is now that path
  (`…_drive-scsi1`), and the docs say what follows a volume from guest to
  guest: its serial (`lsblk -o NAME,SERIAL`).
- **The command line and the console** draw the three fields and the action
  from the schema, as everything else; a list says `host CPU`, `runs VMs`,
  `CPU weight 25` where they are.
- **Three capability flags** for a driver: `cpu.host`, `cpu.nested`,
  `cpu.weight` — the plugin refuses, in words, what a zone's engine lacks —
  and `SetCPUWeight` on its guests facet. **Proxmox VE 9.1 or newer** (the
  `nested-virt` flag, qemu-server 9.0.27).
- Proved on the bench, inside real guests: the processor each of the three
  sees and whether `/dev/kvm` is there; a share moved on a running guest's
  own cgroup; 300 MB written then deleted, back on the storage; a volume's
  path read where its guest finds it.
- **The bench's tests name a bench that is not on their own machine**
  (`HANGAR_BENCH_SSH_HOST`), and the one whose bench calls the brain back
  (the room's hook) holds a tunnel open over the ssh it already has — what
  a bench that is a machine of a zone will need.
- **What running them twice in a row found, in the tests themselves.** The
  room's asked for its three machines' deletes and never waited for them:
  every run left a VM and two containers running on the bench, and the next
  ran beside them. The two that count quiet minutes went red whenever they
  ran across the top of an hour: a fresh Debian 13 guest fetches its
  packages' changelogs once, at the first hour of its life
  (`apt-listchanges.timer`) — a minute that is rightly not idle. Their
  guests are told not to. And the hours' asked that a machine be stopped
  no sooner than five minutes after its last packet, where the engine
  counts in minutes' averages: it now allows one sample.

## v0.2.0 — 2026-10-02

**Names.** A resource a person can read, and call: what the first look at a
real console asked for — a list showed an id, `ready` on a stopped machine,
a 64-character owner and the raw spec. The image:
`ghcr.io/tomblancdev/hangar:0.2.0`.

- **A name and a description on every type.** Every resource has its id —
  its identity: never changed, never used again, what the audit and the
  engine carry — and, when its owner gives one, a `name` (a host's label)
  and a line of `description`: the core's own, beside `tags` in a create,
  never a spec's (Hetzner's and OpenStack's model; AWS's name is a tag no
  command takes, Google Cloud's is fixed at birth). A name is **one thing**
  among its owner's live resources of one type — a second is refused, 409,
  with the id that holds it — so it goes **wherever an id goes**: `hangar
  machine start dev`, `--machine dev`, a reference in a spec or an action's
  params (the core writes the id before anything reads it; the audit says
  what the name stood for). It resolves among one's **own** only: what
  someone shares is named by its id — anyone may call theirs anything, and
  a request by name would be handed a look-alike. `PATCH
  /v1/resources/{id}` renames and describes; the id stays, and so does the
  host name a machine was born with.
- **A resource is served as a person reads it.** `status` and `light` — the
  one word it wears: the core's while something moves or went wrong, its
  plugin's say on whether it may be named, then its type's own (`running`
  or `stopped`, `attached` or `parked`, `available`) —, `summary` — its
  type's own sentence, what it names read by name: « 64 GB · on dev at
  /home · backed up » —, `owner_name` — the name its owner signs in under,
  which the brain now remembers —, `names`. A type says its sentence and
  its word at its schema's root (`x-hangar-summary`, `x-hangar-status`,
  ARCHITECTURE.md §4); the core fills them in, so the command line and the
  console print the same words and know no type.
- **The command line.** A list is `NAME STATE WHAT ZONE AGE ID` (the owner,
  by name, when a row is not yours; `-o wide` adds the set, the place on
  the engine and the description); `get` is a card — what it uses and what
  uses it, by name — and `-o yaml` the whole record; `rename`, `describe`;
  `--name` and `--description` on a create; and **`hangar list`**:
  everything you hold on one screen, each machine with what hangs on it.
- **`apply`.** An entry's key is what its resource is called, a
  `description:` may sit beside its `type:`, and apply keeps both true.
- **The console.** The same words in its lists; name and description first
  in every type's form, refused beside the field; a rename on every
  resource's page; where you are, by name; home drawn as `hangar list`.
- **On the engine** (Proxmox VE). A guest's notes say what it is called and
  whose, and under each volume's line what the volume is called — lines for
  people, of their own: nothing is found by one, and an older driver reads
  past them. The notes' two writers each hold the config's digest: a write
  on notes that changed since they were read is refused by Proxmox itself.
  Proved on the bench, three times in a row.
- **What moves for whoever writes requests by hand.** A machine's and an
  image's `name` left their spec: it is the request's own `name`. The
  registry converts itself at the first start (schema 6): each resource is
  called by the entry a spec file made it from, else by the name its spec
  carried; among live ones of an owner and a type the oldest keeps a name
  two shared; nothing is made again. A plugin's `Resource` carries `name`,
  `description` and `owner_name` (protocol fields 11 to 13), and a driver
  `Relabel` and `RelabelVolume`.

## v0.1.1 — 2026-10-02

What the first deployment on a real cluster found, the same day: a zone that
sleeps, and an image that trusted nobody. The image:
`ghcr.io/tomblancdev/hangar:0.1.1`.

- **A sleeping zone is known at once** (Proxmox VE driver). A call the API
  hands to a node that is powered off comes back `595` only after 30 s —
  the survey's whole time. So every pass over a sleeping zone logged
  `survey: the plugin could not answer`, went on to judge the zone's
  resources one 30 s call at a time, and a start asked of a sleeping zone
  waited behind both before its wake was called: 1 min 34 s to a running
  container, of which the wake itself was 44 s. Now the cluster's own list
  is read first — `offline` there is asleep, said in milliseconds, the node
  not asked; a watched guest of an offline node does not run — and a node
  that is asked is given 5 s of its own. Awake is still the node's own
  answer, never its line in the list
  ([docs/proxmox.md](docs/proxmox.md), « A zone that sleeps, known at
  once »). Proved against an API that answers as the cluster did (a node
  that says nothing, then `595`), nine ways of breaking it each caught.
- **The image carries the certificate authorities.** It is `scratch`, and
  carried no trust store: the brain could not verify its identity provider's
  certificate, and a deployment had to mount its host's bundle. The bundle
  is in the image now; a mounted one, or `SSL_CERT_FILE`, still wins.
  `tools/image-test.sh` builds the image and asks the binary inside it — and
  builds the same image without the bundle, which must be refused.
- **`hangar check` refuses what it could never verify.** On a system that
  trusts no certificate authority, a config that reaches its identity
  provider or a zone's wake over https is refused, naming them — it used to
  be found sound, and fail at the first sign-in. `hangar serve` says the
  same in a warning, and serves: API tokens need no provider.

## v0.1.0 — 2026-10-02

The first release. It gathers everything built since the product was born,
one entry a piece, newest first: the console, the hours, the command line,
the recipes baked again by themselves, the images, the volumes, the room and
the node's hook, the machines and the Proxmox VE driver, the core. Nothing
moved between the last entry and the tag — a release is what a deployment
pins: the image `ghcr.io/tomblancdev/hangar:0.1.0`.

### la console

The web console: the catalogue in a browser, signed in at the provider —
proved by tests that run each of its server's guards on the console inside
the brain and on its own (twenty-one of them broken on purpose, each caught),
and by a real browser clicking through every action of the three plugins: on
the fake engine in CI, and on the repo's throwaway Proxmox VE through the
binary ([docs/console.md](docs/console.md)).

- **`/console/`** on the brain (its own address now leads there): an app of
  hand-written modules — no build step, no dependency — drawn from `GET
  /v1/types`. Every type a list, a form (a field per top-level property of
  its schema: what a reference may name, `@<schedule>` — the newest, the
  groups a share may reach, chips, boxes, numbers with their bounds), a page
  with what it names and what names it, **its actions as buttons**, its
  delete asked twice; your limits as gauges, the zones' room, operations,
  API tokens. A refusal lands beside the field it names, or on a notice with
  the numbers.
- **Sign-in kept by the console's server.** The authorization code flow with
  PKCE at the provider, with the brain's own client id — the command line's
  public client, one more redirect address (`<console>/console/callback`).
  The tokens stay in the console's memory, renewed there (one renewal at a
  time), never on disk and never in the page; the browser holds a cookie
  (`HttpOnly`, `SameSite=Strict`, `__Host-`/`Secure` behind TLS). What
  changes something carries a second token in a header and is refused from
  another site's page (`Sec-Fetch-Site`, `Origin`). A sign-in finishes only
  in the browser that began it, once, and comes back inside the console. A
  sign-in unused for `console.idle` (12h) ends; one the provider stops
  renewing ends at its next call; sign-out revokes its refresh token. Without
  a provider: an API token, pasted.
- **The console is a client of the API**: each call of the page is passed on
  (`/console/api/v1/…`) with the person's own token — the audit names the
  person, `via` their own sign-in. **`hangar console --brain URL`** is the
  same console as a process of its own in front of a brain: no config file,
  no registry, no plugin's key — the one for a public door; `console:
  {enabled: false}` turns the brain's own off.
- **Its look**: a console as consoles are laid out (a side listing the
  catalogue's types, a bar that says where you are as a prompt, lists as
  tables), an old one's screen (what is reported behind glass — lit numbers,
  gauges of lit segments, a lamp and a word for a state, a status line last)
  and the street on top (the stencil, a taped paper notice for what is said
  to you, a stamp on a resource's own page, one sprayed word, a sticker for
  the button that asks). Two stylesheets: `theme.css` the tokens, `app.css`
  the rest.
- Config: `console: {enabled, url, idle}`. New: `internal/console`,
  `internal/provider` (a door's side of the identity provider — the command
  line moved onto it), `internal/browsertest` (a real browser in a test, over
  the DevTools protocol on a pipe; the standard library alone), `ui/console`.
- **The images plugin's `share` action** marks its param `x-hangar-share`: a
  door offers the person's groups.
- **Fixed: the mark was never an image.** A comment in its stylesheet named
  two tags literally, which made the file ill-formed XML — a browser draws
  such an SVG nowhere it is used as an image (the brain's front page, a
  README). Held by a test now. And where it is an image, **the mark is
  served at rest** (`ui.Still`, `/mark.svg` too): the one that draws itself
  plays whole as a document of its own, but as an image the browser the
  tests drive drew its ring and its bolt and never its words.
- CI runs the suite with a browser (`HANGAR_BROWSER`).

### les heures

Power: an idle machine stopped by itself, the hours machines run counted
against a tier's month, keep awake — proved on the fake engine by a clock the
test moves (controls red), and on the repo's throwaway Proxmox VE through the
binary, in real minutes.

- **`idle_after`** on a machine (`30m`, `2h` — 5m to 12h; absent or `never`:
  never stopped for idleness). At every reconcile the plugin asks its engine
  how long the guest has stayed quiet — its CPU and what it sends, from the
  engine's own history, nothing installed in the guest; once that reaches its
  `idle_after` the machine is stopped and **stays stopped** until its owner
  starts it (`machine.idle` in the audit, `observed.quiet_for` on the way). A
  machine whose room is held, one kept awake, one whose history cannot be
  read are not judged. `set_idle_after` changes it (and `apply` asks for it).
  What idle is: the plugin's `idle` settings (`cpu`, cores' worth, default
  0.05; `sent_bps`, default 20).
- **Keep awake, both ways**: `keep_awake` with `for: 8h` holds the idle stop
  off until then and ends by itself; with nothing, until `let_sleep`. A stop
  ends it.
- **Meters**, a third kind of dimension (`DIMENSION_KIND_METER`): what is
  *consumed* as time passes, summed per owner over the calendar month — in
  the config's new `time_zone` (default UTC). A plan names the meters a
  request would leave the resource drawing on (`PlanResponse.meters`); every
  action, delete and reconcile says what it consumed (`Consumed`), added to
  the month in the same write as the observed state it came with. A month
  spent refuses what would draw on it — beside every other limit the request
  is over, all named at once — 403 `limit`, reason `meter`, « 10.1
  of 10 vCPU-hours (machines.vcpu_hours) used in October 2026: it is back on
  1 November » — and at the next reconcile the core tells the plugin
  (`Resource.spent`), under the tier the resource's last request was admitted
  in (`tier` on a resource). `GET /v1/limits` shows a meter's month (`used`
  is a number, `period`, `resets`); `hangar limits` reads « 4.2 of 80 a
  month, back on 1 November »; `hangar_metered_total{dimension}`.
- **`machines.vcpu_hours`**: the hours machines run, **once per core** — read
  from the engine (since when a guest runs), counted at every look and before
  every action as the machine was. The month spent, **what still runs is shut
  down** (`machine.spent`) and starts are refused until it is back. **A tier
  that lists its dimensions one by one must now name it** — a dimension a
  tier does not name is allowed nothing; `machines.*: unlimited` covers it.
- **Drivers**: a guest says since when it runs (`Guest.StartedAt`); the
  **activity facet** (`driver.Activity`, capability `guest.activity`):
  `QuietFor`. Proxmox VE reads the node's own statistics of each guest
  (`rrddata`: a sample a minute, kept a day — read on 9.2; `status/current`'s
  own `cpu` is a per-worker rate, not used), with the `VM.Audit` the token
  already has ([docs/proxmox.md](docs/proxmox.md#idleness-and-hours)). The
  fake engine has a clock of its own (`now` in its file), `started_at` on a
  running guest and `busy_until`.
- **Found on the bench, the second time the tests ran**: Proxmox VE keeps a
  guest's history by its VMID and keeps it when the guest is deleted — a
  container made on the number of a guest deleted a minute before read that
  guest's quiet minutes as its own, and was stopped 22 seconds after it was
  made. Nothing older than a guest's own start is read (the driver), and a
  machine is never quiet for longer than it has run (the plugin); the bench
  test now makes a guest on a dead one's number on purpose.
- Registry schema 5: `meters`, `resources.tier`.

### la ligne de commande

The command line, drawn from what the brain serves — proved on the fake
engine (controls red), through the binary, on the repo's throwaway Proxmox VE
(a dev box made true from its file) and against a throwaway authentik (the
device flow, the refresh, the revocation).

- **`hangar` is also the command line** ([docs/cli.md](docs/cli.md)): `login`
  (the **device flow**, RFC 8628, at the brain's identity provider with its
  own client id — a public client; or an API token read from stdin), `logout`
  (the refresh token revoked, RFC 7009), `whoami`, `types`, `zones`, `limits`,
  `operations`, `wait`, and **every type the brain offers**:
  `hangar <type> create|list|get|delete|<action>`, its flags the type's
  schema's top-level properties, drawn at run time from `GET /v1/types`. The
  sign-ins in `$HANGAR_HOME/credentials.json` (`0600`); `HANGAR_URL`,
  `HANGAR_TOKEN`, `HANGAR_ZONE`.
- **`hangar apply FILE`**: a spec file made true — each entry one resource in
  its type's words; a reference an id, `@<schedule>` or another entry, made in
  that order; the brain its state (tags `apply:set`, `apply:name`; the
  caller's own resources only — an operator's listing of everyone's never
  becomes their plan). Created, changed by the steps each plugin names,
  deleted after asking (`--yes`; refused with no one at stdin), a field set at
  birth stopping everything before anything changes; `--plan`.
- **A change's plan** (`POST /v1/resources/{id}/plan`, the protocol's
  `PlanChange`): what brings a resource to a whole spec — its plugin's steps,
  in order, or the fields set at its birth. Changes nothing. Only what the
  spec names anew must be usable; `@<schedule>` is where the resource already
  is when that schedule made what it names. The machines (resize), volumes
  (resize, set_backup, attach/detach/move), images (share) and toy plugins
  answer it; the SDK's `Step` and `Fixed`.
- **`GET /v1/signin`** (public): the issuer, the client id and the scopes a
  door signs people in with; `identity.oidc.scopes` (default `openid profile
  offline_access`).
- `internal/testoidc` signs a command line in by the device flow, refreshes
  and revokes; `internal/stacktest` runs a whole brain in another package's
  tests.

### les recettes qui se refont

Recipes baked again by themselves — proved on the fake engine by a clock
the test turns, and on the repo's throwaway Proxmox VE through the binary.

- **Schedules** in the core (`schedules:` in the config): a create on a cron
  line (five fields, names, steps; in a time zone, default UTC), **in the
  name of a subject and groups the file names, within that tier's limits**.
  What a schedule makes is tagged `hangar:schedule=<name>`. One at a time: a
  run while the last is still being made is skipped; a run the brain missed
  comes once; a new schedule waits for its first time; a run's client token
  is its own. **What it keeps:** its `keep` newest usable ones (default 2);
  an older usable one is given its `retire` action — only once newer ones
  are usable —, and one that is not usable is deleted once a newer one is
  usable and nothing names it any more. `hangar_schedule_runs_total`, an
  audit line per run and per letting-go, and `hangar check` prints each
  schedule's next run.
- **`@<schedule>`** in any reference: the newest usable resource that
  schedule made which the owner may name — the publisher's pointer, never a
  search by name. The request keeps the id; the audit says what it stood for.
- **Usability**, the plugin's say (`Usability` in `CreateResponse`,
  `ActResponse`, `ReconcileResponse`): why a new request may not name a
  resource (`pending`, `waiting`, `failed`, `retired`) and whether it is
  still being made. The core refuses a reference to one that is not usable,
  in that word, and shows it (`unusable`, `pending`). Registry schema 4.
- **A plugin may report that a resource holds less** (`ReconcileResponse.usage`):
  the core keeps the smaller amount — **a failed bake holds nothing**, and a
  rebake is admitted anew. A failure streak no longer fills a tier.
- The fake engine: `builders_fail` in its file makes every bake fail.
- **The Proxmox VE driver, under a listing a pvestatd pass behind:** a
  machine's create addresses its new guest by the VMID it took (it was looked
  up, and a machine asked for a second after a bake began was not found by
  its own create — and left behind, holding its image's disk); a delete that
  finds nothing looks once more after that pass; a VMID another plugin took
  at the same moment is passed over for the next. `tools/bench`: 6 GB by
  default (4 GB hung under a builder, a machine and an import at once).

### les images

System disks machines are born from — proved on the repo's throwaway Proxmox
VE, through the binary's API, the machines and images plugins each on its
own token.

- **The images plugin** (`images`): `image` (`img-…`) — **baked** from a
  recipe of the operator's (a base on the engine per kind, a disk size, the
  builder's cores, memory and time, a `#cloud-config` or `#!` first boot), or
  **saved** from a stopped machine of the owner's (its system disk alone: a
  machine with a volume plugged in is refused). Actions: `share`, `retire`
  (no machine born from it any more; those born from it run on), `rebake` (a
  failed bake again, from the recipe as it is now). Limits: `images.count`,
  `images.size_gb`, and two choices — `images.source` (recipe, machine) and
  `images.visibility` (private, shared, public). A recipe is copied into each
  image it bakes (`spec.from`).
- **A bake is not one call**: the create answers `pending`, the brain's
  reconcile moves it forward — `available`, or `failed` with the words of the
  builder's first boot. The builder **borrows spot room** (it wakes a sleeping
  zone, is refused while the room is held) and **is let go at once** when the
  room is needed: its bake **starts over by itself** when the room is back,
  never counted as a failure. A failure of the recipe is never retried
  blindly; a builder that stops unasked three times fails its bake.
- **Machines born from an image** (`image_id`, beside the operator's names in
  `image`): one's own or one shared with one, available, not retired; the
  disk at least the image's.
- **Shares** in the core (`"x-hangar-share": true` on a spec's array of
  groups, `*` = everyone): a person in one of the groups sees the resource
  (get, and the listing now holds one's own and what is shared with one's
  groups) and names it in their requests (a reference, never an attachment);
  only its owner or an operator changes or deletes it (403 `shared`). A
  person shares only with groups they are in, or with everyone as their tier
  allows. Shares follow the spec, like relations (registry schema 3).
- **A plugin sees a resource's room** (`Resource.room` in the protocol), and a
  resource whose room changes on its own returns it from reconcile.
- **A reference to a type no enabled plugin declares** no longer stops the
  start: it is logged, and refused at the request.
- **The drivers' images facet** (`driver.Images`), on the fake engine and on
  Proxmox VE: VM templates in the images pool, named after their id and
  tagged with nothing (read on the bench: a clone copies its template's
  tags, and a machine born with an image's id was not found as itself). A
  bake's builder is imported from a disk image (`import-from`, which a fenced
  token may do with `Datastore.Audit` on its storage) or copied from a
  template, boots the recipe then the driver's last step (a unit after
  `cloud-final`: cloud-init's verdict, the disk made ready to be cloned —
  cloud-init's memory, the machine id, the host keys cleared — the verdict
  said by the host name), read through the guest agent; a failure's log read
  with `VM.GuestAgent.FileRead`. A template machines were born from as
  linked clones refuses its delete, naming them. **A new guest's name and a
  template's flag are read in its config** — `/cluster/resources` gives them
  a pvestatd pass late (read on the bench: a save lost its own clone, a
  machine asked for right after it found no template).
- **The bench** gains the images plugin's user, roles and token
  (`HANGAR_BENCH_IMAGES_TOKEN_FILE`).

### les volumes

Disks that belong to their owner rather than to a machine — proved on the
repo's throwaway Proxmox VE, through the binary's API, with both plugins
running, each on its own token.

- **The volumes plugin** (`volumes`): `volume` (`vol-…`) — a size, a content
  fixed at birth (`block`: a disk its VM formats itself; `filesystem`: a
  directory its container mounts at a path), a backup flag, and the machine
  it is plugged into, or none (parked). Actions: `attach`, `detach`, `move`
  (to another machine of the same owner), `resize` (it only grows),
  `set_backup`. Limits: `volumes.count`, `volumes.size_gb`, and
  **`volumes.backup_gb`** — backups are a size budget: a tier that names none
  backs up nothing.
- **Attachments** (`"x-hangar-attached": true` beside `x-hangar-ref`): the
  resource lives inside the one it names on the engine; **neither end is
  deleted while it is** (409 `attached`, naming what to detach). A lost
  resource binds nothing.
- **Relations follow the spec**: added from an action's planned spec at its
  admission, set anew from the spec as it ends (and when reconcile moves a
  spec). **A plan sees what the request names** (`refs` in `PlanRequest`),
  so a disk for a container is refused before anything is admitted.
- **The drivers' volumes facet** (`driver.Volumes`), on the fake engine and
  on Proxmox VE (`volume.move_between_guests`): a volume is always a line of
  some guest's config — a machine's, or a **shelf**'s (a stopped guest per
  owner and kind the driver makes and never starts; containers' from the
  zone's new `shelf_archive`), so a parked volume keeps its backup. Which
  disk is which volume is written in the guest's description, target first
  and source last, so a move cut anywhere is found and finished (a disk is
  renamed after each guest it moves to). A block volume shows its guest the
  serial `vol0123…` (AWS's form). Read on the bench: a disk leaves a running
  VM only unplugged, its options dropped — so it rests on its shelf, where
  they are written back, and reaches the next VM hot-plugged with them; a
  running container lets go of none (refused, in words); a shelf container
  is made without a host name (pve-container counts it as network).
- **A guest holding a volume refuses its delete** in both drivers, under the
  core's own refusal.
- Two processes pick guest ids from one range: a create whose id was just
  taken takes the next.
- **The bench** gains the volumes plugin's user, role and token
  (`HANGAR_BENCH_VOLUMES_TOKEN_FILE`), and its command runs one package at
  a time (`go test -p 1`): two packages' tests drive the bench's one
  priority guest.

### la place

A zone's room: what can be promised, what is only lent, and the guests that
matter more than yours — proved on the repo's throwaway Proxmox VE, with the
hook running on its node.

- **Zones count room** (`room` in a zone): its memory, and reservations kept
  for someone else — always, `while_running` a guest of the operator's, or
  `while_down` a node. Two pools follow: **guaranteed** (the memory less
  every reservation — booked while a resource lives) and **spot** (the room
  the conditional reservations keep while none is in force — lent while it
  runs). Admission is in the limits' transaction: a request beyond a pool is
  refused (409 `room`) with the arithmetic; while the spot pool is held a
  resource runs on what it booked only.
- **Holds**: while a conditional reservation is in force, the core holds
  every room-taking resource of the zone (`hold` in the protocol) and the
  plugin brings each to what that means for it; lifted, it brings them back.
- **The claim**: `POST /v1/zones/{zone}/claim` and `/release` — a guest's
  hook, before it starts and after it stops, with a token of the new scope
  `room` (claims and releases, nothing else) for a tier with `room: true`.
  The brain holds before it answers. A claim stands for the zone's `grace`,
  then only while its guest reads running — or cannot be read: a hiccup is
  not a guest gone; a release that never came ends there. A hold the node
  placed alone is adopted. One room decision at a time per zone: a pass's
  survey never reads a release's own tags mid-way (read on the bench).
- **Surveys** (the protocol's `Survey`, the drivers' `Watcher` facet): what
  a reservation waits on, which resources carry a hold, whether the zone is
  awake — at start and at every reconcile pass.
- **Waking**: a zone's `wake` webhook, called before anything starts in it
  when it reads asleep, then waited for; a reconcile never wakes a zone, and
  does not judge its resources while it sleeps.
- **The machines plugin**: classes `guaranteed`, `spot`, `guaranteed+spot`
  (`floor_gb`: it shrinks to its floor instead of stopping, never below what
  it holds, and says what it could not give back — only where a running
  guest of its kind gives memory back); `cores_beside` (the CPU cap while
  held); `resume` (a spot machine starts again when the room returns, or
  stays stopped); start and stop are planned (they change the room). The
  class, admitted size, floor and cap are written on the guest as tags.
- **The Proxmox VE driver**: `resize.live.cpu_cap` (`cpulimit`) and
  `hook.pre_start`; the fence accepts `VM.Audit` — alone — on the guests a
  zone watches; a node reads down only when the cluster's membership says
  `offline` (`unknown` cannot tell), and a zone awake when its node answers.
- **`hangar-hook`**, a second binary (standard library only): a guest's
  hookscript on the node. On a product machine, the node's own admission
  (set on the VM templates, inherited by every clone); on a priority guest,
  the claim — the brain first, the node alone from the tags when it cannot be
  reached — never refusing that guest's start.
- **The bench** gains a priority guest (VM 100) and the watch grant on it.

### les machines

The first plugin that makes real machines, and the first real engine:
proved on a throwaway Proxmox VE installed by the repo's own bench.

- **The machines plugin** (`machines`): `machine` (`m-…`) — a container or a
  VM, sized by AWS's type names (`t3.micro` … `r5.2xlarge`), the operator's
  aliases, or cores and memory; started from an image the operator names per
  kind; a class (`guaranteed`, `spot`) written on the guest as a tag;
  cloud-init user data where the kind boots it; start, stop, reboot, resize
  (a running machine changes only what its kind can change live), delete,
  reconcile. `keypair` (`kp-…`) — a public key, imported, never generated
  (the brain holds no private key). Dimensions: count, vCPU, memory, disk,
  key pairs; kinds and classes as choices. It requires `fence.pool`.
- **The Proxmox VE driver** (`proxmox`), its own small client on the
  standard library: one API token fenced to one pool (the fence read from the
  token's own permissions — a token that reaches further opens, and is not
  called fenced); containers from a template archive, VMs cloned from a
  template and fed their first boot by a NoCloud seed disc the driver writes
  (a small ISO 9660 + Joliet writer) and uploads; every long call waits for
  its task, so a start a hook refused returns the hook's own words. The least
  the token needs, and why each grant: [docs/proxmox.md](docs/proxmox.md).
- **References between resources:** a schema property marked
  `"x-hangar-ref": "<type>"` names other resources by id; the core checks each
  is the owner's own, in the same zone and ready (someone else's reads as one
  that does not exist), records the relation, and hands the plugin the
  resources with the create or the action (`refs` in the protocol).
- **The driver contract grows:** a guest's name, image, root disk, public
  keys, user data, node and addresses; `Reboot`; `Traits` (what a guest of a
  kind can take, and change while it runs — finer than a capability flag).
- **The bench** (`tools/bench/`): `bench.sh up` installs Proxmox VE unattended
  in a VM (podman + `/dev/kvm`, nothing else on the host) and prepares it as
  an operator would; the driver's tests and the binary's end-to-end test run
  against it when its variables are set, and skip otherwise.

### la naissance

The product is born: the core's skeleton, proved end to end on a fake engine.
No release is cut; the first tag comes with the first plugin that makes a real
machine.

- **The core:** identity (OIDC bearer tokens; API tokens `hgr_…`, hashed,
  expiring, read-only or read-write, unable to make tokens), tiers and limits
  (quantities and choices, the first matching tier wins, an unnamed dimension
  allows nothing, every refusal with its numbers), the registry (SQLite,
  AWS-style ids, tags, usage, relations), operations (client tokens, waiting,
  resumed after a crash), reconcile (in sync, repaired, drifted, lost and
  found), the audit (one line per call), the plugin host.
- **The contracts:** the API as OpenAPI 3.1 (`api/openapi.yaml`, served at
  `/openapi.json`, held to the routes by a test); the plugin protocol
  (`proto/`, gRPC over HashiCorp's go-plugin) and its SDK.
- **The walls, proved:** a plugin starts with an empty environment and
  receives only its own credential, per zone — tests run a probe plugin and
  read what it was given (and fail when the wall is removed).
- **The fake driver** (in memory, or a JSON file that *is* the engine) and
  **the toy plugin** (`box`: create, delete, start, stop, resize, suspend,
  reconcile) — the example to copy.
- One static binary: `hangar serve | check | token | plugin | version`.
