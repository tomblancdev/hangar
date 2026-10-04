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
#   - forwarding on; and off, everything a container archive runs that a
#     gateway has no use for (mail, cron, the package timers, a clock it
#     cannot set).
set -eu

version=1

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
	chain forward {
		type filter hook forward priority filter; policy drop;
		ct state established,related accept
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

printf 'net.ipv4.ip_forward = 1\nnet.ipv6.conf.all.disable_ipv6 = 1\n' >"$work/etc/sysctl.d/90-hangar-gateway.conf"

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
