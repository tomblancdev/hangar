#!/bin/sh
# bench — a throwaway Proxmox VE in a VM on this machine, for the Proxmox
# driver's integration tests. It needs podman and /dev/kvm with nested
# virtualisation on (the bench's own guests are VMs inside a VM); nothing else
# lands on the host.
#
#   sh tools/bench/bench.sh up        install (the first time), boot, set up — re-run freely
#   sh tools/bench/bench.sh env       the variables the bench tests read, as export lines
#   sh tools/bench/bench.sh ssh [cmd] root on the bench, with the bench's own key
#   sh tools/bench/bench.sh down      power it off; the disk stays
#   sh tools/bench/bench.sh reset     power off and go back to the disk as set up
#   sh tools/bench/bench.sh destroy   remove the bench's disks; the downloads stay cached
#   sh tools/bench/bench.sh --help    this text
#
# What it makes: Proxmox VE (the pinned ISO below) installed unattended on
# ZFS, then set up by setup.sh over ssh — a guest network with DHCP, a VM
# template and a container template, and the fence the machines plugin runs
# behind: pool `hangar`, pool `hangar-images`, a role, and one API token.
# The API answers on https://127.0.0.1:$HANGAR_BENCH_PORT (default 18006).
#
# State lives in $HANGAR_BENCH_DIR (default ~/.cache/hangar-bench): the ISO,
# the disks, the bench's ssh key, its root password and the plugin's token —
# the last two 0600 and never printed. The first `up` downloads about 2.3 GB.
#
# Sizes: HANGAR_BENCH_MEMORY (MiB, default 4096), HANGAR_BENCH_CPUS (default 4).
set -eu

case "${1:-}" in
-h | --help | help | "")
	sed -n '2,/^set -eu$/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
	exit 0
	;;
esac

here=$(cd "$(dirname "$0")" && pwd)
dir=${HANGAR_BENCH_DIR:-$HOME/.cache/hangar-bench}
port=${HANGAR_BENCH_PORT:-18006}
sshport=${HANGAR_BENCH_SSH_PORT:-18022}
mem=${HANGAR_BENCH_MEMORY:-4096}
cpus=${HANGAR_BENCH_CPUS:-4}
image=localhost/hangar-bench:latest
name=hangar-bench

iso=proxmox-ve_9.2-1.iso
iso_sha256=4e88fe416df9b527624a175f24c9aa07c714d3332afb1ee3dbf3879573ef2c6c
iso_url=https://enterprise.proxmox.com/iso/$iso

say() { printf 'bench: %s\n' "$*" >&2; }
die() {
	say "$*"
	exit 1
}

tool() { podman run --rm --network host -v "$dir:/bench" "$image" "$@"; }

running() { [ "$(podman inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" = true ]; }

bench_ssh() {
	ssh -i "$dir/ssh_ed25519" -p "$sshport" -o BatchMode=yes -o ConnectTimeout=5 \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
		root@127.0.0.1 "$@"
}

prepare() {
	[ -e /dev/kvm ] || die "no /dev/kvm here: the bench needs hardware virtualisation"
	mkdir -p "$dir"
	chmod 700 "$dir"
	if ! podman image exists "$image"; then
		say "building the bench's tool image"
		podman build -q -t "$image" -f "$here/Containerfile" "$here" >/dev/null
	fi
	if [ ! -f "$dir/$iso" ]; then
		say "downloading $iso (about 1.7 GB, resumable)"
		curl -fSL -C - -o "$dir/$iso.part" "$iso_url"
		mv "$dir/$iso.part" "$dir/$iso"
	fi
	echo "$iso_sha256  $dir/$iso" | sha256sum -c --quiet || die "$iso does not match its pinned hash"
	[ -f "$dir/ssh_ed25519" ] || ssh-keygen -q -t ed25519 -N '' -C hangar-bench -f "$dir/ssh_ed25519"
	if [ ! -f "$dir/root-password" ]; then
		(umask 077 && head -c 24 /dev/urandom | base64 | tr -d '/+=' >"$dir/root-password")
	fi
}

install() {
	[ -f "$dir/base.qcow2" ] && return 0
	say "preparing the unattended installer"
	(umask 077 && sed -e "s|@ROOT_PASSWORD@|$(cat "$dir/root-password")|" \
		-e "s|@SSH_KEY@|$(cat "$dir/ssh_ed25519.pub")|" "$here/answer.toml.in" >"$dir/answer.toml")
	tool proxmox-auto-install-assistant validate-answer /bench/answer.toml >/dev/null
	rm -f "$dir/auto.iso"
	tool proxmox-auto-install-assistant prepare-iso --fetch-from iso \
		--answer-file /bench/answer.toml --output /bench/auto.iso "/bench/$iso" >/dev/null
	rm -f "$dir/answer.toml"
	tool qemu-img create -q -f qcow2 /bench/install.qcow2 32G
	say "installing Proxmox VE (10 to 20 minutes; the VM powers off when done)"
	podman run --rm --name "$name-install" --device /dev/kvm -v "$dir:/bench" "$image" \
		timeout 3600 qemu-system-x86_64 -enable-kvm -cpu host -smp "$cpus" -m "$mem" \
		-drive file=/bench/install.qcow2,if=virtio,cache=unsafe,discard=unmap \
		-cdrom /bench/auto.iso -boot once=d \
		-netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
		-display none -no-reboot
	rm -f "$dir/auto.iso"
	mv "$dir/install.qcow2" "$dir/installed.qcow2"
}

boot() {
	disk=$1
	running && return 0
	podman rm -f "$name" >/dev/null 2>&1 || true
	say "booting the bench"
	podman run -d --name "$name" --network host --device /dev/kvm -v "$dir:/bench" "$image" \
		qemu-system-x86_64 -enable-kvm -cpu host -smp "$cpus" -m "$mem" \
		-drive "file=/bench/$disk,if=virtio,cache=unsafe,discard=unmap" \
		-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$port-:8006,hostfwd=tcp:127.0.0.1:$sshport-:22" \
		-device virtio-net-pci,netdev=n0 \
		-display none -serial file:/bench/console.log >/dev/null
	i=0
	until bench_ssh true 2>/dev/null; do
		i=$((i + 1))
		[ $i -lt 90 ] || die "no ssh after 6 minutes; the console is in $dir/console.log"
		sleep 4
	done
	i=0
	until bench_ssh pvesh get /version >/dev/null 2>&1; do
		i=$((i + 1))
		[ $i -lt 60 ] || die "the API did not come up"
		sleep 2
	done
}

halt() {
	running || { podman rm -f "$name" >/dev/null 2>&1 || true; return 0; }
	bench_ssh poweroff >/dev/null 2>&1 || true
	i=0
	while running; do
		i=$((i + 1))
		[ $i -lt 60 ] || { podman stop -t 5 "$name" >/dev/null; break; }
		sleep 2
	done
	podman rm -f "$name" >/dev/null 2>&1 || true
}

setup() {
	[ -f "$dir/base.qcow2" ] && return 0
	boot installed.qcow2
	say "setting the bench up (downloads a container template and a cloud image)"
	bench_ssh sh -s <"$here/setup.sh"
	bench_ssh cat /etc/pve/pve-root-ca.pem >"$dir/pve-root-ca.pem"
	halt
	mv "$dir/installed.qcow2" "$dir/base.qcow2"
}

overlay() {
	rm -f "$dir/disk.qcow2"
	tool qemu-img create -q -f qcow2 -b /bench/base.qcow2 -F qcow2 /bench/disk.qcow2
}

cmd=$1
shift
case "$cmd" in
up)
	prepare
	install
	setup
	[ -f "$dir/disk.qcow2" ] || overlay
	boot disk.qcow2
	bench_ssh sh -s <"$here/setup.sh"
	for t in hangar-token volumes-token images-token wide-token; do
		(umask 077 && bench_ssh cat "/root/$t" >"$dir/$t.tmp") && mv "$dir/$t.tmp" "$dir/${t%-token}.token"
	done
	say "up: https://127.0.0.1:$port — sh tools/bench/bench.sh env"
	;;
env)
	[ -f "$dir/hangar.token" ] || die "no bench yet: sh tools/bench/bench.sh up"
	cat <<EOF
export HANGAR_BENCH_URL=https://127.0.0.1:$port
export HANGAR_BENCH_CA_FILE=$dir/pve-root-ca.pem
export HANGAR_BENCH_TOKEN_FILE=$dir/hangar.token
export HANGAR_BENCH_VOLUMES_TOKEN_FILE=$dir/volumes.token
export HANGAR_BENCH_IMAGES_TOKEN_FILE=$dir/images.token
export HANGAR_BENCH_WIDE_TOKEN_FILE=$dir/wide.token
export HANGAR_BENCH_SSH_KEY=$dir/ssh_ed25519
export HANGAR_BENCH_SSH_PORT=$sshport
export HANGAR_BENCH_DIR=$dir
EOF
	;;
ssh)
	bench_ssh "$@"
	;;
down)
	halt
	;;
reset)
	halt
	overlay
	say "reset to the disk as set up; sh tools/bench/bench.sh up boots it"
	;;
destroy)
	halt
	rm -f "$dir/disk.qcow2" "$dir/base.qcow2" "$dir/installed.qcow2" "$dir/install.qcow2" \
		"$dir/hangar.token" "$dir/volumes.token" "$dir/images.token" "$dir/wide.token" "$dir/pve-root-ca.pem" "$dir/console.log"
	say "destroyed; the downloads stay in $dir"
	;;
*)
	die "no command $cmd (try --help)"
	;;
esac
