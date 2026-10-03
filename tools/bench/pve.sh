#!/bin/sh
# pve — run as root ON a Debian 13 machine: make it the Proxmox VE the bench's
# setup.sh takes, so that a bench is a machine of a hangar zone — born from an
# image in seconds, one per shard of the tests — and not only a VM on the
# machine that runs them (bench.sh). shards.sh drives it; by hand:
#
#   sh pve.sh kernel      Proxmox's repository and its kernel — then REBOOT
#   sh pve.sh packages    Proxmox VE itself, on its own kernel; a pool for its guests' disks
#   sh pve.sh node        the node answers at the address the machine has NOW:
#                         its name, its certificate, its guests' way out —
#                         at every birth of a machine born from the saved image
#   sh pve.sh seal        before the machine is saved as an image: what its clones must not share
#   sh pve.sh --help      this text
#
# The machine needs its host's processor and virtualisation (a machine's
# `cpu: host` and `virtualization: true`): its guests are VMs inside a VM.
# Installed as Proxmox documents it for Debian 13 (its packages on a stock
# system), not by its installer: an installer's disk cannot be born from an
# image of the product. What differs from bench.sh's own bench, and why:
#
#   - the root is the image's own (ext4): the guests' disks go on a ZFS pool
#     made of one sparse file, named as the installer names its own (`rpool`,
#     storage `local-zfs`) — the tests name them —, its cache capped as the
#     installer's answers cap it;
#   - the node keeps the name `pve-bench` whatever the machine is called
#     (cloud-init is told to leave the host name and /etc/hosts alone): a
#     Proxmox node is its name, and the tests name it;
#   - the address is the machine's lease, another at every birth: `node`
#     writes it where Proxmox reads its own (/etc/hosts), makes the
#     certificate anew for it, and applies the guests' network again — their
#     way out is written with the node's address in it;
#   - the node is told to forward (the installer's own network does it by
#     itself): without it a guest's answers stop at the node;
#   - the node's resolvers are the real ones, not the image's own stub: the
#     guests ask the node, and the node will not ask a stub of its own;
#   - the guests' clock is answered by the node, whatever server they name:
#     a zone's network may let only the web out.
#
# Idempotent: a step already done is skipped.
set -eu

case "${1:-}" in
-h | --help | help | "")
	sed -n '2,/^set -eu$/p' "$0" 2>/dev/null | sed '$d' | sed 's/^# \{0,1\}//'
	exit 0
	;;
esac

name=pve-bench
fqdn=$name.example.com
pool_gb=${HANGAR_BENCH_POOL_GB:-24}
say() { printf 'pve: %s\n' "$*" >&2; }
die() {
	say "$*"
	exit 1
}
[ "$(id -u)" = 0 ] || die "as root"
# a long command's output is kept, and shown only when it fails
quiet() {
	if ! "$@" >/tmp/pve-bench.log 2>&1; then
		tail -40 /tmp/pve-bench.log >&2
		die "failed: $*"
	fi
}
export DEBIAN_FRONTEND=noninteractive

# the address the machine's default route leaves by (192.0.2.1: a
# documentation address — nothing is sent, the kernel only says which way;
# and not one of the guests' own network, which the node itself answers on)
address() { ip -4 -o route get 192.0.2.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p'; }

# root is who the bench's tests come in as. The machine's keys were given to
# the image's own user, and root's own file tells that same key to go there
# (cloud-init's line, a command in place of a shell) — a line Proxmox then
# keeps in its own copy of root's keys, where the first line that matches a
# key wins. So: Proxmox's own key for root, the user's keys, and no such line.
root_keys() {
	keys=/root/.ssh/authorized_keys
	install -d -m 700 /root/.ssh
	if [ -d /etc/pve/priv ]; then
		keys=/etc/pve/priv/authorized_keys
		ln -sfn "$keys" /root/.ssh/authorized_keys
	fi
	{
		grep -v 'command="' "$keys" 2>/dev/null || true
		cat /home/*/.ssh/authorized_keys 2>/dev/null || true
	} | awk 'NF && !seen[$0]++' >/tmp/pve-bench.keys
	cat /tmp/pve-bench.keys >"$keys"
	rm -f /tmp/pve-bench.keys
}

# The names its guests ask for are answered by the node's own DHCP server
# (dnsmasq), which passes them on to the resolvers the node's file names.
# On the image two things stood in the way, and every question a guest asked
# came back refused — a guest asked again and again (read: an idle VM
# sending 63 bytes a second for ever, and no package fetched):
#   - the image's file names only its own stub (127.0.0.53), an address of
#     the node's that the server will not ask: the node's file is made the
#     one that names the real resolvers (what a container made here
#     inherits, too);
#   - the image's resolver brings a `resolvconf` of its own, and Debian's
#     start of that server, seeing one, reads a file nothing writes: it is
#     told to read the node's.
resolvers() {
	if [ -s /run/systemd/resolve/resolv.conf ] &&
		[ "$(readlink /etc/resolv.conf 2>/dev/null)" != /run/systemd/resolve/resolv.conf ]; then
		ln -sfn /run/systemd/resolve/resolv.conf /etc/resolv.conf
	fi
	mkdir -p /etc/systemd/system/dnsmasq@.service.d
	if [ ! -f /etc/systemd/system/dnsmasq@.service.d/pve-bench.conf ]; then
		printf '[Service]\nEnvironment=IGNORE_RESOLVCONF=yes\n' >/etc/systemd/system/dnsmasq@.service.d/pve-bench.conf
		systemctl daemon-reload
	fi
}

# Proxmox reads its own address by its name: a loopback one is refused.
hosts() {
	ip=$(address)
	[ -n "$ip" ] || die "no address yet: the machine has no default route"
	sed -i "/[[:space:]]$name\$/d; /[[:space:]]$name[[:space:]]/d; /^127\.0\.1\.1[[:space:]]/d" /etc/hosts
	printf '%s %s %s\n' "$ip" "$fqdn" "$name" >>/etc/hosts
	say "the node is $name at $ip"
}

# the guests' own resolver (the node, on their bridge) answers a name
host_answers() {
	if command -v host >/dev/null; then
		host -W 2 deb.debian.org 198.51.100.1 >/dev/null 2>&1
	else
		# no tool to ask it with: the node's own file names a real resolver
		! grep -q '^nameserver 127\.' /etc/resolv.conf
	fi
}

case "$1" in
kernel)
	# cloud-init gives a machine its name and writes /etc/hosts at every boot
	# (the cloud image's own settings): not this one's, from here on. And it
	# goes on writing the machine's own network where the image reads it
	# (netplan): with Proxmox's ifupdown installed it would choose that
	# instead, at a clone's first boot — and the clone's interface, written
	# twice and read by neither, came up with no address (read on a first try)
	mkdir -p /etc/cloud/cloud.cfg.d
	cat >/etc/cloud/cloud.cfg.d/99-pve-bench.cfg <<EOF
preserve_hostname: true
manage_etc_hosts: false
system_info:
  network:
    renderers: [netplan]
EOF
	hostnamectl set-hostname "$name"
	hosts
	# Proxmox's archive key, checked against the hash published beside its
	# release notes (the same as the bench's own tool image)
	key=/usr/share/keyrings/proxmox-archive-keyring.gpg
	want=136673be77aba35dcce385b28737689ad64fd785a797e57897589aed08db6e45
	if ! echo "$want  $key" | sha256sum -c --quiet >/dev/null 2>&1; then
		command -v curl >/dev/null || { apt-get update -q >/dev/null && apt-get install -y -q curl ca-certificates >/dev/null; }
		curl -fsSL -o "$key.part" https://enterprise.proxmox.com/debian/proxmox-archive-keyring-trixie.gpg
		echo "$want  $key.part" | sha256sum -c --quiet || die "Proxmox's archive key does not match its pinned hash"
		mv "$key.part" "$key"
	fi
	cat >/etc/apt/sources.list.d/pve-no-subscription.sources <<EOF
Types: deb
URIs: http://download.proxmox.com/debian/pve
Suites: trixie
Components: pve-no-subscription
Signed-By: $key
EOF
	# the cloud image never told its boot loader's package which disk it boots
	# from; Proxmox's own build of it asks, and with nobody to answer its
	# upgrade fails (read: « You must correct your GRUB install devices »)
	boot=/dev/$(lsblk -no PKNAME "$(findmnt -no SOURCE /)")
	[ -b "$boot" ] || die "the disk this machine boots from was not found ($boot)"
	echo "grub-pc grub-pc/install_devices multiselect $boot" | debconf-set-selections
	say "the system brought up to date, then Proxmox's kernel"
	apt-get update -q >/dev/null
	quiet apt-get full-upgrade -y -q
	quiet apt-get install -y -q proxmox-default-kernel
	say "done: reboot, then sh pve.sh packages"
	;;
packages)
	case "$(uname -r)" in
	*-pve) ;;
	*) die "this is kernel $(uname -r), not Proxmox's: sh pve.sh kernel, then reboot" ;;
	esac
	hosts
	resolvers
	if ! command -v pvesh >/dev/null; then
		say "Proxmox VE (about 1 GB of packages)"
		echo "postfix postfix/main_mailer_type select Local only" | debconf-set-selections
		echo "postfix postfix/mailname string $fqdn" | debconf-set-selections
		quiet apt-get install -y -q proxmox-ve postfix open-iscsi chrony qemu-guest-agent
		apt-get remove -y -q os-prober >/dev/null 2>&1 || true
	fi
	# a subscription's repositories come with the packages: a bench has none
	for f in /etc/apt/sources.list.d/pve-enterprise.sources /etc/apt/sources.list.d/ceph.sources; do
		if [ -f "$f" ] && ! grep -q '^Enabled: no' "$f"; then echo 'Enabled: no' >>"$f"; fi
	done
	# the guests' network is written under interfaces.d, which ifupdown reads
	# only when its own file says so — and the image's network is not
	# ifupdown's: it has no such file. Proxmox's packages leave one beside it,
	# `interfaces.new`, that the first network reload moves into place — over
	# anything written before (read: the line was lost, and the guests'
	# bridge never made). Moved here first, then the line.
	[ ! -f /etc/network/interfaces.new ] || mv /etc/network/interfaces.new /etc/network/interfaces
	[ -f /etc/network/interfaces ] || printf 'auto lo\niface lo inet loopback\n' >/etc/network/interfaces
	grep -q '^source /etc/network/interfaces.d/\*' /etc/network/interfaces ||
		printf '\nsource /etc/network/interfaces.d/*\n' >>/etc/network/interfaces
	# ifupdown's own service, started once here: the first reload after its
	# install fails (read twice: « ifreload -a failed: exit code 89 », the
	# second one fine) — that first one is this, not the guests' network's
	systemctl enable --now networking >/dev/null 2>&1 || true
	ifreload -a >/dev/null 2>&1 || true
	# the guests' disks: a pool named as the installer names its own, on one
	# sparse file of the root disk
	# ZFS's cache capped as the bench's own installer caps it (256 MB): left
	# to itself it takes up to half the machine's memory, from under the guests
	echo 'options zfs zfs_arc_max=268435456' >/etc/modprobe.d/zfs.conf
	if ! zpool list rpool >/dev/null 2>&1; then
		say "a ZFS pool for the guests' disks ($pool_gb G, sparse)"
		modprobe zfs
		echo 268435456 >/sys/module/zfs/parameters/zfs_arc_max
		truncate -s "${pool_gb}G" /var/lib/hangar-bench-rpool.img
		zpool create -o ashift=12 -O compression=lz4 -O atime=off rpool /var/lib/hangar-bench-rpool.img
		zfs create rpool/data
	fi
	i=0
	until pvesh get /version >/dev/null 2>&1; do
		i=$((i + 1))
		[ $i -lt 60 ] || die "the API did not come up"
		sleep 2
	done
	pvesm status --storage local-zfs >/dev/null 2>&1 ||
		pvesm add zfspool local-zfs --pool rpool/data --content images,rootdir --sparse 1
	say "done on $(hostname): sh pve.sh node, then setup.sh"
	;;
node)
	command -v pvesh >/dev/null || die "no Proxmox VE here: sh pve.sh kernel, reboot, sh pve.sh packages"
	resolvers
	was=$(sed -n "s/^\([0-9.]*\)[[:space:]].*[[:space:]]$name\$/\1/p" /etc/hosts | tail -1)
	[ "$(hostname)" = "$name" ] || hostnamectl set-hostname "$name"
	hosts
	if [ "$was" != "$(address)" ] || ! [ -s "/etc/pve/nodes/$name/pve-ssl.pem" ]; then
		# another address than the image's: Proxmox learns its own again
		systemctl restart pve-cluster
		pvecm updatecerts --force >/dev/null 2>&1
		systemctl restart pvedaemon pveproxy pvestatd
	fi
	i=0
	until pvesh get /version >/dev/null 2>&1; do
		i=$((i + 1))
		[ $i -lt 60 ] || die "the API did not come up"
		sleep 2
	done
	root_keys
	zpool list rpool >/dev/null 2>&1 || zpool import -d /var/lib rpool
	# the cache's cap, said again where the module reads it now (loaded
	# before the root's own options are read, it may have missed them)
	[ ! -w /sys/module/zfs/parameters/zfs_arc_max ] || echo 268435456 >/sys/module/zfs/parameters/zfs_arc_max
	# the guests' way out passes through the node: it forwards. Proxmox's own
	# installer leaves that to ifupdown, which holds every interface there;
	# here the machine's own interface is the image's network's, and nothing
	# said so — a guest's packets left, and their answers were dropped at the
	# node (read: every guest that fetched a package waited for ever)
	echo 'net.ipv4.ip_forward = 1' >/etc/sysctl.d/90-pve-bench.conf
	sysctl -q -w net.ipv4.ip_forward=1
	out=$(ip -4 -o route get 192.0.2.1 | sed -n 's/.* dev \([^ ]*\).*/\1/p')
	[ -z "$out" ] || sysctl -q -w "net.ipv4.conf.$out.forwarding=1"
	# the guests leave by the node's own address, written in their network's
	# files: applied again, it is the one the node has now — and the rule a
	# clone booted with, which names the image's address, is taken off first
	# (it comes first, and what leaves by another address than the machine's
	# is dropped on its way)
	if pvesh get /cluster/sdn/zones/hbench >/dev/null 2>&1; then
		ip=$(address)
		iptables -t nat -S POSTROUTING 2>/dev/null | grep -- '--to-source' | grep -v -- "--to-source $ip\$" | sed 's/^-A/-D/' |
			while read -r rule; do
				# shellcheck disable=SC2086 # a rule is words
				iptables -t nat $rule
			done
		pvesh set /cluster/sdn >/dev/null
		i=0
		until ip link show hbnet >/dev/null 2>&1 && iptables -t nat -S POSTROUTING | grep -q -- "--to-source $ip\$"; do
			i=$((i + 1))
			[ $i -lt 30 ] || die "the guests' network did not come up at $ip"
			sleep 2
		done
		# a reload may set the interface's own forwarding anew
		[ -z "$out" ] || sysctl -q -w "net.ipv4.conf.$out.forwarding=1"
		[ "$(sysctl -n "net.ipv4.conf.${out:-all}.forwarding")" = 1 ] || die "the node does not forward: its guests have no way out"
		# their clock is asked of the node itself, whatever server a guest
		# names: a zone's network may let only the web out, and a guest
		# asking the world's time servers, never answered, asks again every
		# ten seconds — it is never read as idle (read on such a zone)
		if [ -d /etc/chrony/conf.d ]; then
			printf 'allow 198.51.100.0/24\nlocal stratum 10\n' >/etc/chrony/conf.d/pve-bench.conf
			systemctl restart chrony
			iptables -t nat -C PREROUTING -i hbnet -p udp --dport 123 -j REDIRECT 2>/dev/null ||
				iptables -t nat -A PREROUTING -i hbnet -p udp --dport 123 -j REDIRECT
		fi
		# and their names are answered: asked of the server they are handed
		systemctl restart "dnsmasq@hbench" 2>/dev/null || true
		i=0
		until getent hosts deb.debian.org >/dev/null 2>&1 && host_answers; do
			i=$((i + 1))
			[ $i -lt 20 ] || die "the guests' names are not answered at $(sed -n 's/^nameserver //p' /etc/resolv.conf | tr '\n' ' ')"
			sleep 2
		done
	fi
	say "ready"
	;;
seal)
	# what clones must not share, and what makes the next boot a first one:
	# cloud-init's own state (each clone is given its keys and its disk's
	# size anew), the machine id, the ssh host keys
	sync
	# nobody's key stays in the image but Proxmox's own for root
	if [ -f /etc/pve/priv/authorized_keys ]; then
		grep " root@$name\$" /etc/pve/priv/authorized_keys >/tmp/pve-bench.keys || true
		cat /tmp/pve-bench.keys >/etc/pve/priv/authorized_keys
		rm -f /tmp/pve-bench.keys
	fi
	rm -f /home/*/.ssh/authorized_keys
	rm -f /etc/ssh/ssh_host_*
	apt-get clean
	cloud-init clean --logs --seed --machine-id >/dev/null 2>&1 || die "cloud-init clean failed"
	journalctl --rotate >/dev/null 2>&1 || true
	journalctl --vacuum-time=1s >/dev/null 2>&1 || true
	say "sealed: stop the machine, then save it"
	;;
*)
	die "no step $1 (try --help)"
	;;
esac
