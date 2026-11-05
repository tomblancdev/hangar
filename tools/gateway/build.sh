#!/bin/sh
# build — the archive a network's gateway is born from (the zone's
# `net_archive`): a Debian container archive made the one thing a gateway is.
# Run as root where the base archive is — a Proxmox VE node, or anywhere tar
# keeps owners. Nothing is fetched and nothing is run inside the archive: it
# is unpacked, a handful of files are written, it is packed again.
#
#   sh tools/gateway/build.sh BASE OUT
#       BASE   Proxmox's own Debian 13 archive
#              (/var/lib/vz/template/cache/debian-13-standard_…_amd64.tar.zst)
#       OUT    the gateway's archive (…/hangar-gateway-<version>.tar.zst):
#              name it after the recipe's version — a gateway born from
#              another name is made again
#   sh tools/gateway/build.sh --version   the recipe's version
#   sh tools/gateway/build.sh --help      this text
#
# What a gateway is — and keeps: nothing. Its two cards are Proxmox's to
# write at every start (eth0 on the zone's lane, eth1 on its network), its
# keys are the ones it was born with (root's authorized_keys, Proxmox's own
# file). The recipe adds:
#
#   - nftables: its network goes out through the lane, masqueraded; nothing
#     new comes in from the lane but ssh; and the gateway itself opens
#     connections toward its own network only — which is what makes a jump
#     through it reach that network and nothing else (sshd's PermitOpen
#     takes no range);
#   - sshd: one user, `jump`, let in by the keys the gateway was born with,
#     given no shell, no terminal, no session of any kind — forwarding only,
#     outward only;
#   - its endpoints (recipe 2): what its network may reach of the zone's
#     private side, read at each start from its own config — Proxmox VE's
#     `env` (9.1 and later), handed to its first process —
#
#       HANGAR_ENDPOINTS=tcp:192.0.2.35:3101 udp:192.0.2.35:514   let through
#       HANGAR_RESOLVERS=192.0.2.1                                port 53 let through
#       HANGAR_PRIVATE=192.0.2.0/24                               the rest refused, counted
#
#     by a unit (hangar-endpoints) that fills three sets of its rules after
#     nftables starts; FORWARDING STAYS OFF UNTIL IT HAS — a word it cannot
#     read leaves its network with no way out, never with an open one. With
#     none of them (a zone that publishes nothing) it lets out whatever the
#     lane lets out, as recipe 1 did;
#   - and off, everything a container archive runs that a gateway has no
#     use for (mail, cron, the package timers, a clock it cannot set).
set -eu

version=2

case "${1:-}" in
-h | --help | help | "")
	sed -n '2,/^set -eu$/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
	exit 0
	;;
--version)
	echo "$version"
	exit 0
	;;
esac

base=$1
out=${2:?the archive to write (see --help)}
say() { printf 'gateway: %s\n' "$*" >&2; }
[ -f "$base" ] || { say "no base archive at $base"; exit 1; }
[ "$(id -u)" = 0 ] || { say "run as root: an archive keeps its files' owners"; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
tar --numeric-owner -xpf "$base" -C "$work"
[ -x "$work/usr/sbin/nft" ] && [ -x "$work/usr/sbin/sshd" ] || {
	say "$base has no nft or no sshd: Proxmox's debian-13-standard archive has both"
	exit 1
}

# ---- the one user ---------------------------------------------------------
# no shell, no home, no password that could ever match ("*": not a locked
# account, which sshd would refuse outright)
uid=900
if grep -q "^[^:]*:[^:]*:$uid:" "$work/etc/passwd" || grep -q "^[^:]*:[^:]*:$uid:" "$work/etc/group"; then
	say "id $uid is taken in $base"
	exit 1
fi
echo "jump:x:$uid:$uid:a jump into this network:/nonexistent:/usr/sbin/nologin" >>"$work/etc/passwd"
echo "jump:x:$uid:" >>"$work/etc/group"
echo "jump:*:20000:0:99999:7:::" >>"$work/etc/shadow"

# ---- sshd: a jump, never a shell ------------------------------------------
cat >"$work/etc/ssh/sshd_config" <<'EOF'
# A hangar gateway (tools/gateway/build.sh): a jump into its network, never a
# shell on the gateway.
AddressFamily inet
AllowUsers jump
PermitRootLogin no
AuthenticationMethods publickey
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
# the keys it was born with: root's file, which Proxmox wrote at its birth
# and only root reads
AuthorizedKeysFile none
AuthorizedKeysCommand /usr/bin/cat /root/.ssh/authorized_keys
AuthorizedKeysCommandUser root
# forwarding outward, and nothing else: no session at all (MaxSessions 0
# refuses every shell, command and subsystem, and leaves forwarding)
MaxSessions 0
ForceCommand /usr/sbin/nologin
PermitTTY no
AllowTcpForwarding local
AllowAgentForwarding no
AllowStreamLocalForwarding no
GatewayPorts no
PermitListen none
PermitTunnel no
PermitUserRC no
PermitUserEnvironment no
X11Forwarding no
PrintMotd no
MaxAuthTries 3
LoginGraceTime 20
ClientAliveInterval 30
ClientAliveCountMax 4
# who came in, by which key
LogLevel VERBOSE
EOF

# ---- nftables: out, nothing in, a jump into its own network ----------------
cat >"$work/etc/nftables.conf" <<'EOF'
#!/usr/sbin/nft -f
# A hangar gateway (tools/gateway/build.sh). eth0: the zone's lane; eth1: its
# network.
flush ruleset

table inet gateway {
	chain input {
		type filter hook input priority filter; policy drop;
		ct state established,related accept
		icmp type echo-request accept
		iifname "eth0" tcp dport 22 accept comment "the jump: who may knock is the zone's wall's to say"
		counter drop comment "refused: in"
	}
	# filled at each start from the gateway's own config (hangar-endpoints):
	# what its network may reach of the zone's private side, and that side
	set endpoints { type ipv4_addr . inet_proto . inet_service; }
	set resolvers { type ipv4_addr; }
	set private { type ipv4_addr; flags interval; auto-merge; }
	chain forward {
		type filter hook forward priority filter; policy drop;
		ct state established,related accept
		iifname "eth1" oifname "eth0" ip daddr . meta l4proto . th dport @endpoints counter accept comment "its endpoints"
		iifname "eth1" oifname "eth0" ip daddr @resolvers meta l4proto { tcp, udp } th dport 53 counter accept comment "the zone's resolvers"
		iifname "eth1" oifname "eth0" ip daddr @private counter drop comment "refused: the private side, no endpoint"
		iifname "eth1" oifname "eth0" accept comment "its network, out"
		counter drop comment "refused: through"
	}
	chain output {
		type filter hook output priority filter; policy drop;
		ct state established,related accept
		oifname "eth1" accept comment "a jump opens its own network, and nothing else"
		counter reject with icmpx admin-prohibited comment "refused: a jump elsewhere"
	}
}

table ip nat {
	chain postrouting {
		type nat hook postrouting priority srcnat;
		oifname "eth0" masquerade
	}
}
EOF
chmod 755 "$work/etc/nftables.conf"
mkdir -p "$work/etc/systemd/system/sysinit.target.wants"
ln -sf /usr/lib/systemd/system/nftables.service "$work/etc/systemd/system/sysinit.target.wants/nftables.service"

# forwarding OFF at boot: hangar-endpoints turns it on once its rules stand
printf 'net.ipv4.ip_forward = 0\nnet.ipv6.conf.all.disable_ipv6 = 1\n' >"$work/etc/sysctl.d/90-hangar-gateway.conf"

# ---- its endpoints: read from its own config, at each start ----------------
mkdir -p "$work/usr/local/sbin"
cat >"$work/usr/local/sbin/hangar-endpoints" <<'EOS'
#!/bin/sh
# hangar-endpoints — a hangar gateway's endpoints (tools/gateway/build.sh,
# recipe 2): its first process's environment, which Proxmox VE writes from
# the container's `env`, into its rules' sets; then forwarding on. A word it
# cannot read stops it there: forwarding stays off.
set -euf
say() { echo "hangar-endpoints: $*"; }
no() { say "$1: forwarding stays off" >&2; exit 1; } # its words reach the journal, never the rules
env1=$(tr '\0' '\n' </proc/1/environ) || no "its first process's environment cannot be read"
get() { printf '%s\n' "$env1" | sed -n "s/^$1=//p" | tail -n 1; }
octet='([0-9]|[1-9][0-9]|1[0-9][0-9]|2[0-4][0-9]|25[0-5])'
addr="$octet(\.$octet){3}"
is() { printf '%s\n' "$2" | grep -Eqx "$1"; }
eps=$(get HANGAR_ENDPOINTS)
res=$(get HANGAR_RESOLVERS)
priv=$(get HANGAR_PRIVATE)
rules=$(mktemp)
{
	echo "flush set inet gateway endpoints"
	echo "flush set inet gateway resolvers"
	echo "flush set inet gateway private"
	for e in $eps; do # protocol:address:port
		is "(tcp|udp):$addr:[0-9]{1,5}" "$e" || no "an endpoint it cannot read ($e)"
		proto=${e%%:*} rest=${e#*:}
		port=${rest##*:}
		[ "$port" -ge 1 ] && [ "$port" -le 65535 ] || no "an endpoint it cannot read ($e)"
		echo "add element inet gateway endpoints { ${rest%:*} . $proto . $port }"
	done
	for r in $res; do
		is "$addr" "$r" || no "a resolver it cannot read ($r)"
		echo "add element inet gateway resolvers { $r }"
	done
	for p in $priv; do
		is "$addr(/([0-9]|[12][0-9]|3[0-2]))?" "$p" || no "a range of the private side it cannot read ($p)"
		echo "add element inet gateway private { $p }"
	done
} >"$rules"
nft -f "$rules" || no "its rules refused these sets"
rm -f "$rules"
sysctl -q -w net.ipv4.ip_forward=1
say "endpoints [$eps] resolvers [$res] private [$priv]: forwarding on"
EOS
chmod 755 "$work/usr/local/sbin/hangar-endpoints"
cat >"$work/etc/systemd/system/hangar-endpoints.service" <<'EOS'
[Unit]
Description=a hangar gateway's endpoints, from its own config
Requires=nftables.service
After=nftables.service systemd-sysctl.service
Before=network-online.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/hangar-endpoints
[Install]
WantedBy=multi-user.target
EOS
mkdir -p "$work/etc/systemd/system/multi-user.target.wants"
ln -sf /etc/systemd/system/hangar-endpoints.service "$work/etc/systemd/system/multi-user.target.wants/hangar-endpoints.service"

# ---- off: what a gateway has no use for -------------------------------------
for unit in postfix.service postfix@.service cron.service e2scrub_reap.service e2scrub_all.timer \
	apt-daily.timer apt-daily-upgrade.timer apt-listchanges.timer dpkg-db-backup.timer man-db.timer \
	fstrim.timer systemd-timesyncd.service getty@.service console-getty.service container-getty@.service; do
	ln -sf /dev/null "$work/etc/systemd/system/$unit"
done
find "$work/etc/systemd/system" -path '*.wants/*' -type l | while read -r link; do
	[ -L "$work/etc/systemd/system/$(basename "$link")" ] && [ "$(readlink "$work/etc/systemd/system/$(basename "$link")")" = /dev/null ] && rm -f "$link"
done
rm -f "$work"/etc/systemd/system/getty.target.wants/* "$work/etc/systemd/system/dbus-org.freedesktop.timesync1.service"
# the journal in memory, small: a gateway keeps nothing
mkdir -p "$work/etc/systemd/journald.conf.d"
printf '[Journal]\nStorage=volatile\nRuntimeMaxUse=8M\n' >"$work/etc/systemd/journald.conf.d/hangar-gateway.conf"
echo "$version" >"$work/etc/hangar-gateway"

mkdir -p "$(dirname "$out")"
tar --numeric-owner -cpf - -C "$work" . | zstd -q -T0 -o "$out.part" --force
mv "$out.part" "$out"
say "made $out (recipe $version) from $(basename "$base")"
