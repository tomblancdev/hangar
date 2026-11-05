#!/bin/sh
# NUAGE-3's proof, on a throwaway Proxmox VE: a gateway carries its endpoints in
# its own config (`env`, written by the networks plugin's fenced token), its
# archive's unit reads them at each start, and its network reaches its
# endpoints, the resolver and the web - and nothing else of the private side.
# Run as root on the bench (hangar's tools/bench/setup.sh laid it).
set -u
say() { printf 'proof %s: %s\n' "$(date +%H:%M:%S.%N | cut -c1-12)" "$*"; }
node=$(hostname)
api=https://127.0.0.1:8006/api2/json
call() { tok=$1; shift; curl -sk -H "Authorization: PVEAPIToken=$(cat "$tok")" "$@"; }
NT=/root/networks-token MT=/root/machines-nets-token
cache=/var/lib/vz/template/cache
gw=201 m=301
envfile=/root/proof-env

wait_task() { # $1 = token file, $2 = UPID
	i=0
	while [ $i -lt 120 ]; do
		st=$(call "$1" "$api/nodes/$node/tasks/$(printf %s "$2" | jq -sRr @uri)/status")
		case "$st" in *'"status":"stopped"'*) printf '%s\n' "$st" | jq -r '.data.exitstatus'; return ;; esac
		sleep 0.5; i=$((i + 1))
	done
	echo timeout
}
probe() { # from the machine: $1 = label, $2 = host, $3 = port
	if pct exec $m -- timeout 3 bash -c "exec 3<>/dev/tcp/$2/$3" 2>/dev/null; then say "  $1 ($2:$3): REACHED"; else say "  $1 ($2:$3): refused or silent"; fi
}
probes() {
	probe "endpoint lab-logs" 198.51.100.1 3101
	probe "private, no endpoint" 198.51.100.1 3102
	probe "private, the node's ssh" 198.51.100.1 22
	if pct exec $m -- timeout 5 getent hosts deb.debian.org >/dev/null 2>&1; then say "  the resolver: deb.debian.org RESOLVED"; else say "  the resolver: no answer"; fi
	probe "the web" deb.debian.org 80
}

pct destroy $m --purge >/dev/null 2>&1 || true
call $NT -X DELETE "$api/nodes/$node/lxc/$gw?purge=1&force=1" >/dev/null 2>&1; sleep 3

say "1. the archive: recipe 2-draft, from Proxmox's own Debian 13 archive"
base=$(ls $cache/debian-13-standard_*_amd64.tar.* | sort -V | tail -1)
[ -f $cache/hangar-gateway-2-draft.tar.zst ] || sh /root/build-v2-draft.sh "$base" $cache/hangar-gateway-2-draft.tar.zst

say "2. listeners on the lane, at the node's 198.51.100.1: 3101 (published) and 3102 (not)"
for p in 3101 3102; do (cd /tmp && setsid python3 -m http.server $p --bind 198.51.100.1 >/dev/null 2>&1 &) ; done
sleep 1

say "3. the gateway made by the NETWORKS token, its endpoints in env"
printf 'HANGAR_ENDPOINTS=tcp:198.51.100.1:3101\0HANGAR_RESOLVERS=198.51.100.1\0HANGAR_PRIVATE=10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 198.51.100.0/24' >$envfile
t0=$(date +%s.%N)
r=$(call $NT -X POST "$api/nodes/$node/lxc" \
	--data-urlencode "vmid=$gw" --data-urlencode "hostname=gw-proof" --data-urlencode "pool=hangar-nets" \
	--data-urlencode "ostemplate=local:vztmpl/hangar-gateway-2-draft.tar.zst" --data-urlencode "unprivileged=1" \
	--data-urlencode "memory=128" --data-urlencode "swap=0" --data-urlencode "cores=1" --data-urlencode "rootfs=local-zfs:2" \
	--data-urlencode "net0=name=eth0,bridge=hbnet,ip=198.51.100.201/24,gw=198.51.100.1" \
	--data-urlencode "net1=name=eth1,bridge=hgnets,tag=101,ip=10.20.0.1/24" \
	--data-urlencode "env@$envfile")
say "  create: $(printf %s "$r" | head -c 300)"
upid=$(printf %s "$r" | jq -r '.data // empty')
[ -n "$upid" ] && say "  create task: $(wait_task $NT "$upid")"
say "  the config Proxmox keeps:"; pct config $gw | grep -E '^(env|lxc|net)' | sed 's/^/    /'
grep -E '^lxc.environment' /var/lib/lxc/$gw/config 2>/dev/null | sed 's/^/    (lxc) /'

say "4. the control: the machines plugin's token (a gateway's power, nothing of its config) writes env"
printf 'HANGAR_ENDPOINTS=tcp:198.51.100.1:22' >/root/proof-env-bad
say "  $(call $MT -X PUT "$api/nodes/$node/lxc/$gw/config" --data-urlencode "env@/root/proof-env-bad" | head -c 300)"
say "  and a newline inside the value, by the networks token:"
printf 'HANGAR_ENDPOINTS=tcp:1.2.3.4:5\nX=1' >/root/proof-env-nl
say "  $(call $NT -X PUT "$api/nodes/$node/lxc/$gw/config" --data-urlencode "env@/root/proof-env-nl" | head -c 300)"

say "5. a machine on the network (tag 101), its gateway 10.20.0.1, its resolver the zone's"
pct create $m local:vztmpl/$(basename "$base") --hostname m-proof --unprivileged 1 --memory 256 --swap 0 --cores 1 \
	--rootfs local-zfs:2 --net0 name=eth0,bridge=hgnets,tag=101,ip=10.20.0.10/24,gw=10.20.0.1 --nameserver 198.51.100.1 >/dev/null
say "6. the gateway started by the networks token"
r=$(call $NT -X POST "$api/nodes/$node/lxc/$gw/status/start"); upid=$(printf %s "$r" | jq -r '.data // empty')
say "  start task: $(wait_task $NT "$upid")"
pct start $m; sleep 6
t=0; while [ $t -lt 40 ]; do pct exec $gw -- systemctl is-active hangar-endpoints >/dev/null 2>&1 && break; sleep 0.5; t=$((t + 1)); done
say "  inside the gateway, its first process's environment:"
pct exec $gw -- sh -c "tr '\0' '\n' </proc/1/environ" | grep -E '^(HANGAR|container)' | sed 's/^/    /'
say "  its unit: $(pct exec $gw -- systemctl is-active hangar-endpoints) - $(pct exec $gw -- journalctl -u hangar-endpoints -o cat --no-pager | grep hangar-endpoints: | tail -1)"
say "  forwarding: $(pct exec $gw -- sysctl -n net.ipv4.ip_forward)"
say "7. from the machine:"
probes
say "  the gateway's counters:"
pct exec $gw -- nft list chain inet gateway forward | grep counter | sed 's/^\s*/    /'

say "8. a change: lab-logs AND 3102 published to this network - env written again by the networks token, the gateway restarted"
printf 'HANGAR_ENDPOINTS=tcp:198.51.100.1:3101 tcp:198.51.100.1:3102\0HANGAR_RESOLVERS=198.51.100.1\0HANGAR_PRIVATE=10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 198.51.100.0/24' >$envfile
say "  $(call $NT -X PUT "$api/nodes/$node/lxc/$gw/config" --data-urlencode "env@$envfile" | head -c 200)"
say "  pending? $(call $NT "$api/nodes/$node/lxc/$gw/pending" | jq -c '[.data[] | select(.key=="env") | {pending: (.pending != null), value: (.value|length)}]')"
say "  before the restart, from the machine:"
probe "3102, written but not yet restarted" 198.51.100.1 3102
# a connection every 0.2 s to the endpoint, across the restart
pct exec $m -- bash -c 'end=$((SECONDS+40)); while [ $SECONDS -lt $end ]; do if timeout 0.5 bash -c "exec 3<>/dev/tcp/198.51.100.1/3101" 2>/dev/null; then echo "$(date +%s.%N) ok"; else echo "$(date +%s.%N) cut"; fi; sleep 0.2; done' >/root/proof-cut.log 2>&1 &
loop=$!
sleep 3
r=$(call $NT -X POST "$api/nodes/$node/lxc/$gw/status/reboot"); upid=$(printf %s "$r" | jq -r '.data // empty')
say "  reboot task: $(wait_task $NT "$upid")"
wait $loop
awk '$2=="cut" && !a {a=$1} $2=="ok" && a && !b {b=$1} END {if (a) printf "proof: the cut: from %.1f to %.1f = %.1f s\n", a, b, b-a; else print "proof: no cut seen"}' /root/proof-cut.log
say "  after the restart:"
probes
say "  its unit says: $(pct exec $gw -- journalctl -u hangar-endpoints -o cat --no-pager -b | grep hangar-endpoints: | tail -1)"

say "9. a gateway with no endpoints at all (env removed: a zone that publishes nothing)"
say "  $(call $NT -X PUT "$api/nodes/$node/lxc/$gw/config" --data-urlencode "delete=env" | head -c 200)"
r=$(call $NT -X POST "$api/nodes/$node/lxc/$gw/status/reboot"); upid=$(printf %s "$r" | jq -r '.data // empty'); say "  reboot task: $(wait_task $NT "$upid")"
t=0; while [ $t -lt 40 ]; do pct exec $gw -- systemctl is-active hangar-endpoints >/dev/null 2>&1 && break; sleep 0.5; t=$((t + 1)); done
probes
say "done"
