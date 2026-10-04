#!/bin/sh
# shards — the bench as machines of a hangar zone: a Proxmox VE born from an
# image in seconds, one for each shard of the bench's tests, the shards run at
# once. For a zone with cores to spare, where bench.sh's one VM on this
# machine runs the tests one after the other.
#
#   sh tools/bench/shards.sh seed             once — and again when Proxmox VE or setup.sh moved:
#                                             a machine made a Proxmox VE (pve.sh, setup.sh), saved
#                                             as the image `pve-bench`, then deleted
#   sh tools/bench/shards.sh key              the public key the tests come in with (made if absent)
#   sh tools/bench/shards.sh up [N] [--key 'ssh-ed25519 …']
#                                             N benches born from the image (default 3); their addresses
#   sh tools/bench/shards.sh test [ADDRESS…]  each bench's node set to its address, then the tests in as
#                                             many shards, one bench each, at once (default: up's addresses)
#   sh tools/bench/shards.sh list [N]         which test goes to which of N shards, and why
#   sh tools/bench/shards.sh down             the benches deleted (the image stays)
#   sh tools/bench/shards.sh --help           this text
#
# Who does what. `seed`, `up` and `down` ask a brain for machines: they need
# `hangar` signed in (hangar login) as someone whose tier opens a VM's host
# processor and virtualisation (machines.cpu, machines.virtualization) and
# lets them save an image of their machine (images.source: machine). `test`
# needs `go`, `ssh` and a way to each bench's ports 22 and 8006 — it may run
# elsewhere than `up` did (nearer the benches): take its key first (`key`
# there, `up --key` here), and hand it the addresses.
#
# A bench holds nothing: its key is made for it, its node's host key is never
# learnt (as bench.sh's), its tokens are the image's own.
#
# State lives in $HANGAR_BENCH_DIR/shards (default ~/.cache/hangar-bench): the
# key, the addresses, each shard's tokens and its log.
#
#   HANGAR_BENCH_FROM       what the seed is born from, as `hangar machine create` takes it
#                           (default: --image debian-13 — a Debian 13 with cloud-init)
#   HANGAR_BENCH_ZONE       the zone (default: the brain's own choice)
#   HANGAR_BENCH_USER       the image's own user, who may sudo (default debian)
#   HANGAR_BENCH_CPUS, HANGAR_BENCH_MEMORY_GB, HANGAR_BENCH_DISK_GB   a bench's size (4, 6, 32)
#   HANGAR_BENCH_RUN        only the bench tests this expression matches (default: all of them)
#   HANGAR_BENCH_WAIT       seconds a machine is given to say its address and to answer ssh (300)
#   HANGAR_BROWSER          a Chromium for the console's test (named and missing, it fails;
#                           unnamed, it skips)
set -eu

case "${1:-}" in
-h | --help | help | "")
	sed -n '2,/^set -eu$/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
	exit 0
	;;
esac

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
dir=${HANGAR_BENCH_DIR:-$HOME/.cache/hangar-bench}/shards
from=${HANGAR_BENCH_FROM:---image debian-13}
user=${HANGAR_BENCH_USER:-debian}
cpus=${HANGAR_BENCH_CPUS:-4}
mem=${HANGAR_BENCH_MEMORY_GB:-6}
disk=${HANGAR_BENCH_DISK_GB:-32}
image=pve-bench
keypair=bench-shards
zone=${HANGAR_BENCH_ZONE:+--zone $HANGAR_BENCH_ZONE}
patience=${HANGAR_BENCH_WAIT:-300} # seconds a machine is given to say its address, and to answer ssh

say() { printf 'shards: %s\n' "$*" >&2; }
die() {
	say "$*"
	exit 1
}

mkdir -p "$dir"
chmod 700 "$dir"

key() {
	[ -f "$dir/key" ] || ssh-keygen -q -t ed25519 -N '' -C hangar-bench-shards -f "$dir/key"
	cat "$dir/key.pub"
}

# as someone on a bench, with the bench's own key; the node's host key is
# never learnt — a bench is born a minute ago and holds nothing
on() {
	who=$1
	shift
	ssh -i "$dir/key" -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=no \
		-o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "$who" "$@"
}

answers() {
	until=$(($(date +%s) + patience))
	until on "$1" true 2>/dev/null; do
		if [ "$(date +%s)" -ge "$until" ]; then
			say "$1 does not answer ssh after $patience s"
			return 1
		fi
		sleep 4
	done
}

# a machine's address, as the brain reads it from its guest agent
# — the first that is not the bench's own guests' network (198.51.100.0/24,
# setup.sh's: the node answers there too, and nobody else reaches it)
address() {
	hangar machine get "$1" -o json 2>/dev/null | tr -d ' \n' | grep -o '"addresses":\[[^]]*\]' |
		grep -o '[0-9]*\.[0-9]*\.[0-9]*\.[0-9]*' | grep -v '^198\.51\.100\.' | head -1
}

await_address() {
	until=$(($(date +%s) + patience))
	a=
	while [ -z "$a" ]; do
		if [ "$(date +%s)" -ge "$until" ]; then
			say "$1 says no address after $patience s (its image runs no guest agent?)"
			return 1
		fi
		sleep 4
		a=$(address "$1")
	done
	echo "$a"
}

# The tests, and about how long each takes alone (seconds, read on benches
# that are machines of a zone: what the shards are balanced by — the first
# two are mostly waits no machine shortens, and they bound a shard). A test
# this list does not know weighs 300: it is still run, in the shard that is
# lightest when its turn comes — nothing a package holds is ever left out.
weights='
hangar TestBenchTheHours 840
proxmox TestBenchAGuestsActivity 652
hangar TestBenchARecipeBakedAgainByItself 276
hangar TestBenchTheConsole 239
proxmox TestBenchAnImagesLife 179
proxmox TestBenchAMachineBornBehindItsWall 149
hangar TestBenchTheRoom 149
proxmox TestBenchAVMsLife 147
proxmox TestBenchAVMsVolume 136
proxmox TestBenchTwoNetworksAndAJump 281
hangar TestBenchAnImageThroughTheAPI 115
hangar TestBenchANetworkThroughTheAPI 87
proxmox TestBenchAVMsProcessor 81
proxmox TestBenchAVMBornBehindItsWall 75
proxmox TestBenchAContainersLife 40
proxmox TestBenchAContainersVolume 39
hangar TestBenchAVolumeThroughTheAPI 37
hangar TestBenchTheDevBoxApplied 33
hangar TestBenchThroughTheAPI 37
proxmox TestBenchNamesInAGuestsNotes 17
proxmox TestBenchAContainersWeight 9
proxmox TestBenchAHookRefusesAStart 6
proxmox TestBenchTheWatcher 4
proxmox TestBenchTheFence 1
'

# plan N: every bench test of the two packages, one line each —
# "<shard> <package> <test> <seconds>" — the heaviest first, each given to
# the shard that is lightest so far.
plan() {
	n=$1
	(cd "$repo" && go test -list '^TestBench' ./driver/proxmox/ ./cmd/hangar) | WEIGHTS=$weights awk -v n="$n" -v only="${HANGAR_BENCH_RUN:-.}" '
		BEGIN {
			split(ENVIRON["WEIGHTS"], lines, "\n")
			for (i in lines) { split(lines[i], f, " "); if (f[2] != "") w[f[1] " " f[2]] = f[3] }
		}
		/^TestBench/ { if ($1 ~ only) pending[++count] = $1; next }
		/^ok/ {
			pkg = ($2 ~ /driver\/proxmox$/) ? "proxmox" : "hangar"
			for (i = 1; i <= count; i++) {
				t = pending[i]; k = pkg " " t
				tests[++total] = k; secs[k] = (k in w) ? w[k] : 300
			}
			count = 0
		}
		END {
			# heaviest first (a stable selection sort: the list is short)
			for (i = 1; i <= total; i++) {
				best = i
				for (j = i + 1; j <= total; j++) if (secs[tests[j]] > secs[tests[best]]) best = j
				t = tests[best]; for (j = best; j > i; j--) tests[j] = tests[j - 1]; tests[i] = t
			}
			for (i = 1; i <= total; i++) {
				light = 1
				for (s = 2; s <= n; s++) if (load[s] + 0 < load[light] + 0) light = s
				load[light] += secs[tests[i]]
				print light, tests[i], secs[tests[i]]
			}
		}'
}

# a bench made ready: its node set to the address it has, its fence checked
# (setup.sh, a second time: every step says it is done), its tokens and its
# authority fetched. Each step is asked for its own answer: one that fails
# ends it there.
ready() {
	addr=$1
	sd=$2
	answers "$user@$addr" || return 1
	on "$user@$addr" sudo sh -s node <"$here/pve.sh" || return 1
	answers "root@$addr" || return 1
	on "root@$addr" sh -s <"$here/setup.sh" || return 1
	gateway "root@$addr" || return 1
	for t in hangar-token volumes-token images-token wide-token networks-token machines-nets-token; do
		(umask 077 && on "root@$addr" cat "/root/$t" >"$sd/${t%-token}.token") || return 1
		[ -s "$sd/${t%-token}.token" ] || return 1
	done
	on "root@$addr" cat /etc/pve/pve-root-ca.pem >"$sd/pve-root-ca.pem" || return 1
	[ -s "$sd/pve-root-ca.pem" ]
}

# the gateway's archive, built on the bench from its own Debian archive by
# the product's recipe — once: a bench that has it keeps it
gateway() {
	v=$(sh "$repo/tools/gateway/build.sh" --version)
	on "$1" "f=/var/lib/vz/template/cache/hangar-gateway-$v.tar.zst; [ -f \$f ] || sh -s -- \"\$(ls /var/lib/vz/template/cache/debian-13-standard_*_\$(dpkg --print-architecture).tar.* | sort -V | tail -1)\" \$f" <"$repo/tools/gateway/build.sh"
}

# one bench: made ready, then its share of the tests
shard() {
	n=$1
	addr=$2
	sd=$dir/shard-$n
	log=$dir/shard-$n.log
	rm -rf "$sd"
	mkdir -p "$sd"
	: >"$log"
	if ! ready "$addr" "$sd" >>"$log" 2>&1; then
		echo "shard $n ($addr): NOT RUN — the bench could not be made ready: $log"
		return 1
	fi
	export HANGAR_BENCH_URL="https://$addr:8006" HANGAR_BENCH_CA_FILE="$sd/pve-root-ca.pem"
	export HANGAR_BENCH_TOKEN_FILE="$sd/hangar.token" HANGAR_BENCH_VOLUMES_TOKEN_FILE="$sd/volumes.token"
	export HANGAR_BENCH_IMAGES_TOKEN_FILE="$sd/images.token" HANGAR_BENCH_WIDE_TOKEN_FILE="$sd/wide.token"
	export HANGAR_BENCH_NETWORKS_TOKEN_FILE="$sd/networks.token" HANGAR_BENCH_MACHINES_NETS_TOKEN_FILE="$sd/machines-nets.token"
	export HANGAR_BENCH_SSH_KEY="$dir/key" HANGAR_BENCH_SSH_PORT=22 HANGAR_BENCH_SSH_HOST="$addr"
	status=0
	began=$(date +%s)
	for pkg in proxmox hangar; do
		tests=$(awk -v s="$n" -v p="$pkg" '$1 == s && $2 == p {printf "%s%s", sep, $3; sep = "|"}' "$dir/plan")
		[ -n "$tests" ] || continue
		path=./driver/proxmox/
		[ "$pkg" = proxmox ] || path=./cmd/hangar
		(cd "$repo" && go test "$path" -run "^($tests)\$" -count=1 -v -timeout 3h) >>"$log" 2>&1 || status=1
	done
	took=$(($(date +%s) - began))
	passed=$(grep -c '^--- PASS' "$log" || true)
	failed=$(grep -c '^--- FAIL' "$log" || true)
	skipped=$(grep -c '^--- SKIP' "$log" || true)
	asked=$(awk -v s="$n" '$1 == s' "$dir/plan" | wc -l)
	verdict=green
	# a test that never said PASS is not green, whatever go test's exit says
	if [ "$status" != 0 ] || [ "$failed" != 0 ] || [ "$((passed + skipped))" != "$asked" ]; then
		verdict=RED
	fi
	echo "shard $n ($addr): $verdict — $passed passed, $failed failed, $skipped skipped of $asked, in $((took / 60)) min $((took % 60)) s: $log"
	[ "$verdict" = green ]
}

cmd=$1
shift
case "$cmd" in
key)
	key
	;;
list)
	plan "${1:-3}"
	;;
seed)
	command -v hangar >/dev/null || die "no hangar here: the command line, signed in (hangar login)"
	key >/dev/null
	hangar machine delete bench-seed >/dev/null 2>&1 || true
	hangar keypair delete "$keypair" >/dev/null 2>&1 || true
	hangar keypair create --name "$keypair" --public-key "$(cat "$dir/key.pub")" $zone >/dev/null
	say "a machine to make a Proxmox VE of ($cpus cores, $mem GB, $disk GB)"
	# shellcheck disable=SC2086 # words on purpose
	hangar machine create --name bench-seed --description "the bench's Proxmox VE, being made" $zone --kind vm $from \
		--cores "$cpus" --memory-gb "$mem" --disk-gb "$disk" --cpu host --virtualization --key-pairs "$keypair" >/dev/null
	addr=$(await_address bench-seed) || exit 1
	answers "$user@$addr" || exit 1
	say "at $addr: Proxmox's kernel"
	on "$user@$addr" sudo sh -s kernel <"$here/pve.sh"
	on "$user@$addr" sudo reboot >/dev/null 2>&1 || true
	sleep 20
	answers "$user@$addr" || exit 1
	say "Proxmox VE, on its own kernel"
	on "$user@$addr" sudo sh -s packages <"$here/pve.sh"
	on "$user@$addr" sudo sh -s node <"$here/pve.sh"
	say "the bench's own setup"
	on "$user@$addr" sudo sh -s <"$here/setup.sh"
	on "$user@$addr" sudo sh -s seal <"$here/pve.sh"
	hangar machine stop bench-seed >/dev/null
	say "saved as the image $image"
	hangar image delete "$image" >/dev/null 2>&1 || true
	hangar image create --name "$image" --description "a Proxmox VE the bench's tests run against" $zone --machine bench-seed >/dev/null
	hangar machine delete bench-seed >/dev/null
	say "done: sh tools/bench/shards.sh up"
	;;
up)
	command -v hangar >/dev/null || die "no hangar here: the command line, signed in (hangar login)"
	n=3
	pub=
	while [ $# -gt 0 ]; do
		case "$1" in
		--key)
			pub=$2
			shift
			;;
		*) n=$1 ;;
		esac
		shift
	done
	[ -n "$pub" ] || pub=$(key)
	hangar keypair delete "$keypair" >/dev/null 2>&1 || true
	hangar keypair create --name "$keypair" --public-key "$pub" $zone >/dev/null
	i=1
	pids=
	while [ "$i" -le "$n" ]; do
		hangar machine delete "bench-$i" >/dev/null 2>&1 || true
		# shellcheck disable=SC2086 # words on purpose
		hangar machine create --name "bench-$i" --description "a bench: a Proxmox VE for one shard of the tests" $zone --kind vm \
			--image-id "$image" --cores "$cpus" --memory-gb "$mem" --disk-gb "$disk" --cpu host --virtualization \
			--key-pairs "$keypair" >"$dir/up-$i.log" 2>&1 &
		pids="$pids $!"
		i=$((i + 1))
	done
	i=1
	for p in $pids; do
		# a bench the brain refuses is said in the brain's own words
		wait "$p" || die "bench-$i was not made: $(cat "$dir/up-$i.log")"
		i=$((i + 1))
	done
	: >"$dir/addrs"
	i=1
	while [ "$i" -le "$n" ]; do
		a=$(await_address "bench-$i") || exit 1
		echo "$a" >>"$dir/addrs"
		i=$((i + 1))
	done
	say "$n benches: sh tools/bench/shards.sh test $(tr '\n' ' ' <"$dir/addrs")"
	cat "$dir/addrs"
	;;
test)
	key >/dev/null
	[ $# -gt 0 ] || { [ -s "$dir/addrs" ] && set -- $(cat "$dir/addrs"); }
	[ $# -gt 0 ] || die "no bench: sh tools/bench/shards.sh up, or name their addresses"
	command -v go >/dev/null || die "no go here"
	plan $# >"$dir/plan"
	say "$(wc -l <"$dir/plan") tests in $# shards (sh tools/bench/shards.sh list $#)"
	began=$(date +%s)
	# built once, before the shards start together
	(cd "$repo" && go test -count=1 -run '^$' ./driver/proxmox/ ./cmd/hangar >/dev/null)
	n=0
	pids=
	for addr in "$@"; do
		n=$((n + 1))
		shard "$n" "$addr" &
		pids="$pids $!"
	done
	red=0
	for p in $pids; do
		wait "$p" || red=$((red + 1))
	done
	took=$(($(date +%s) - began))
	if [ "$red" = 0 ]; then
		say "green: every shard, in $((took / 60)) min $((took % 60)) s"
	else
		die "RED: $red of $# shards, in $((took / 60)) min $((took % 60)) s"
	fi
	;;
down)
	command -v hangar >/dev/null || die "no hangar here: the command line, signed in (hangar login)"
	for m in $(hangar machine list -o json 2>/dev/null | tr -d ' \n' | grep -o '"name":"bench-[0-9]*"' | sed 's/.*"\(bench-[0-9]*\)"/\1/' | sort -u); do
		hangar machine delete "$m" >/dev/null && say "deleted $m"
	done
	hangar keypair delete "$keypair" >/dev/null 2>&1 || true
	rm -f "$dir/addrs"
	;;
*)
	die "no command $cmd (try --help)"
	;;
esac
