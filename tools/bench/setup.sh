#!/bin/sh
# setup — run as root ON the bench by bench.sh (over ssh), once, after the
# install: what an operator does before hangar may drive a Proxmox VE.
#
#   1. the no-subscription repository (a bench has no subscription) + dnsmasq
#   2. a guest network: an SDN simple zone with DHCP and SNAT (198.51.100.0/24)
#      and a storage of its own for the VMs' seed discs (hangar-seeds)
#   3. images: a container template archive and a VM template made from
#      Debian's cloud image, in pool hangar-images
#   4. the fence: pool hangar, a role that acts only there, a read-and-clone
#      role on the images, one user and its API token (privilege-separated)
#      for the machines plugin — and a narrower one for the volumes plugin:
#      disks and its own shelf guests in the same pool, no network, no seed
#      store, no watched guest — and one for the images plugin: templates and
#      the builders that bake them in pool hangar-images, a stopped machine
#      of pool hangar read and cloned (a save), nothing else of the machines
#   5. a priority guest: VM 100, the operator's own, outside the pools — the
#      guest a zone keeps room for while it runs; the token may read its
#      power (VM.Audit on it) and nothing else there. The hook itself
#      (cmd/hangar-hook) is a build of this repo: the room's bench test puts
#      it in place, on this guest and on the VM template.
#   6. the wall: the cluster's firewall on — a guest's own is enforced only
#      then — the node's own left off (a bench is reached from wherever its
#      tests run), and one security group as an operator's, `hangar-floor`:
#      what every guest of a walled zone hears — the node, on ssh, which is
#      how the tests enter a guest. No token of the product's can do any of
#      it (Sys.Modify on /).
#
# The token's secret is written to /root/hangar-token (0600) as
# `user@realm!name=secret`, for bench.sh to copy out; it is never printed —
# the volumes plugin's to /root/volumes-token, the images plugin's to
# /root/images-token, the same way.
# So is a second one, /root/wide-token: root's, unfenced — the tests'
# control that the driver tells a fenced token from one that is not.
# Idempotent: a step already done is skipped — bench.sh runs it at every
# `up`, so a change here reaches a bench already made.
set -eu

node=$(hostname)
say() { printf 'setup: %s\n' "$*" >&2; }

# ---- 1. packages ------------------------------------------------------------
for f in /etc/apt/sources.list.d/pve-enterprise.sources /etc/apt/sources.list.d/ceph.sources; do
	if [ -f "$f" ] && ! grep -q '^Enabled: no' "$f"; then echo 'Enabled: no' >>"$f"; fi
done
cat >/etc/apt/sources.list.d/pve-no-subscription.sources <<'EOF'
Types: deb
URIs: http://download.proxmox.com/debian/pve
Suites: trixie
Components: pve-no-subscription
Signed-By: /usr/share/keyrings/proxmox-archive-keyring.gpg
EOF
if ! command -v dnsmasq >/dev/null; then
	say "installing dnsmasq (the SDN's DHCP)"
	apt-get update -q >/dev/null
	DEBIAN_FRONTEND=noninteractive apt-get install -y -q dnsmasq >/dev/null
	systemctl disable --now dnsmasq >/dev/null 2>&1 || true
fi

# ---- 2. the guests' network -------------------------------------------------
pvesm set local --content iso,vztmpl,backup,import,snippets
# The VMs' seed discs get a storage of their own: deleting an uploaded ISO
# takes Datastore.Allocate, which on a shared storage would reach every
# template and backup on it. Here it reaches seed discs and nothing else.
if ! pvesm status --storage hangar-seeds >/dev/null 2>&1; then
	mkdir -p /var/lib/hangar-seeds
	pvesm add dir hangar-seeds --path /var/lib/hangar-seeds --content iso
fi
if ! pvesh get /cluster/sdn/zones/hbench >/dev/null 2>&1; then
	say "a guest network: zone hbench, vnet hbnet, 198.51.100.0/24 with DHCP and SNAT"
	pvesh create /cluster/sdn/zones --zone hbench --type simple --dhcp dnsmasq --ipam pve
	pvesh create /cluster/sdn/vnets --vnet hbnet --zone hbench
	pvesh create /cluster/sdn/vnets/hbnet/subnets --subnet 198.51.100.0/24 --type subnet \
		--gateway 198.51.100.1 --snat 1 \
		--dhcp-range start-address=198.51.100.100,end-address=198.51.100.199
	pvesh set /cluster/sdn
fi

# ---- 3. images --------------------------------------------------------------
pveum pool add hangar-images --comment "templates hangar clones from" 2>/dev/null || true
# the index lists every architecture Proxmox VE runs on: take the node's own
arch=$(dpkg --print-architecture)
for f in /var/lib/vz/template/cache/debian-13-standard_*; do
	case $f in *_"$arch".tar.*) ;; *) rm -f "$f" ;; esac
done
if ! ls /var/lib/vz/template/cache/debian-13-standard_*_"$arch".tar.* >/dev/null 2>&1; then
	say "the container template"
	pveam update >/dev/null
	tmpl=$(pveam available --section system | awk -v a="_$arch." '$2 ~ /^debian-13-standard_/ && index($2, a) {print $2}' | sort -V | tail -1)
	[ -n "$tmpl" ] || { say "no debian-13-standard template in the appliance index"; exit 1; }
	pveam download local "$tmpl" >/dev/null
fi
cloud=debian-13-genericcloud-amd64.qcow2
base=https://cloud.debian.org/images/cloud/trixie/latest
if [ ! -f "/var/lib/vz/import/$cloud" ]; then
	say "Debian's cloud image"
	mkdir -p /var/lib/vz/import
	curl -fsSL -o "/var/lib/vz/import/$cloud.part" "$base/$cloud"
	want=$(curl -fsSL "$base/SHA512SUMS" | awk -v f="$cloud" '$2 == f {print $1}')
	got=$(sha512sum "/var/lib/vz/import/$cloud.part" | cut -d' ' -f1)
	[ -n "$want" ] && [ "$want" = "$got" ] || { say "the cloud image does not match SHA512SUMS"; exit 1; }
	mv "/var/lib/vz/import/$cloud.part" "/var/lib/vz/import/$cloud"
fi
if ! qm config 9000 >/dev/null 2>&1; then
	say "VM template 9000 (debian-13) from the cloud image"
	qm create 9000 --name debian-13 --pool hangar-images --ostype l26 --memory 1024 --cores 1 \
		--scsihw virtio-scsi-single --scsi0 "local-zfs:0,import-from=local:import/$cloud" \
		--boot order=scsi0 --serial0 socket --vga serial0 --agent enabled=1 \
		--net0 virtio,bridge=hbnet >/dev/null
	qm template 9000 >/dev/null
fi

# ---- 4. the fence -----------------------------------------------------------
pveum pool add hangar --comment "guests made by hangar" 2>/dev/null || true
privs="VM.Allocate,VM.Audit,VM.Clone,VM.Config.CDROM,VM.Config.CPU,VM.Config.Cloudinit"
privs="$privs,VM.Config.Disk,VM.Config.HWType,VM.Config.Memory,VM.Config.Network"
privs="$privs,VM.Config.Options,VM.PowerMgmt,VM.GuestAgent.Audit,Datastore.AllocateSpace,Datastore.Audit"
# Pool.Audit: without it /cluster/resources leaves out a guest's pool
# (API2/Cluster.pm), and the fence could not tell its own guests apart
privs="$privs,Pool.Audit"
pveum role add HangarMachines --privs "$privs" 2>/dev/null || pveum role modify HangarMachines --privs "$privs"
pveum role add HangarImages --privs VM.Audit,VM.Clone,Pool.Audit 2>/dev/null ||
	pveum role modify HangarImages --privs VM.Audit,VM.Clone,Pool.Audit
pveum role add HangarTemplates --privs Datastore.Audit 2>/dev/null || pveum role modify HangarTemplates --privs Datastore.Audit
seeds=Datastore.Allocate,Datastore.AllocateTemplate,Datastore.Audit
pveum role add HangarSeeds --privs "$seeds" 2>/dev/null || pveum role modify HangarSeeds --privs "$seeds"
pveum role add HangarWatch --privs VM.Audit 2>/dev/null || pveum role modify HangarWatch --privs VM.Audit
# the volumes plugin: a volume's disk on a machine of the pool (Config.Disk),
# the line that says whose it is (Config.Options: the description), and the
# stopped « shelf » guests that keep a volume no machine holds (Allocate)
vprivs="VM.Allocate,VM.Audit,VM.Config.Disk,VM.Config.Options,Datastore.AllocateSpace,Datastore.Audit,Pool.Audit"
pveum role add HangarVolumes --privs "$vprivs" 2>/dev/null || pveum role modify HangarVolumes --privs "$vprivs"
if ! qm config 100 >/dev/null 2>&1; then
	say "VM 100, a priority guest (no disk: it only has to start and stop)"
	qm create 100 --name priority --memory 512 --cores 1 --net0 virtio,bridge=hbnet >/dev/null
fi
pveum user add hangar-machines@pve --comment "hangar's machines plugin" 2>/dev/null || true
if [ ! -s /root/hangar-token ]; then
	pveum user token remove hangar-machines@pve bench 2>/dev/null || true
	secret=$(pveum user token add hangar-machines@pve bench --privsep 1 --output-format json |
		sed -n 's/.*"value":"\([^"]*\)".*/\1/p')
	[ -n "$secret" ] || { say "the token was not made"; exit 1; }
	(umask 077 && printf 'hangar-machines@pve!bench=%s\n' "$secret" >/root/hangar-token)
fi
for who in "--users hangar-machines@pve" "--tokens hangar-machines@pve!bench"; do
	# shellcheck disable=SC2086 # two words on purpose
	{
		pveum acl modify /pool/hangar --roles HangarMachines $who
		pveum acl modify /pool/hangar-images --roles HangarImages $who
		pveum acl modify /storage/local-zfs --roles PVEDatastoreUser $who
		pveum acl delete /storage/local --roles HangarSeeds $who 2>/dev/null || true
		pveum acl modify /storage/local --roles HangarTemplates $who # read the container templates
		pveum acl modify /storage/hangar-seeds --roles HangarSeeds $who
		pveum acl modify /sdn/zones/hbench/hbnet --roles PVESDNUser $who
		pveum acl modify /vms/100 --roles HangarWatch $who # its power, read: the zone's reservation waits on it
	}
done
pveum user add hangar-volumes@pve --comment "hangar's volumes plugin" 2>/dev/null || true
if [ ! -s /root/volumes-token ]; then
	pveum user token remove hangar-volumes@pve bench 2>/dev/null || true
	secret=$(pveum user token add hangar-volumes@pve bench --privsep 1 --output-format json |
		sed -n 's/.*"value":"\([^"]*\)".*/\1/p')
	[ -n "$secret" ] || { say "the volumes token was not made"; exit 1; }
	(umask 077 && printf 'hangar-volumes@pve!bench=%s\n' "$secret" >/root/volumes-token)
fi
for who in "--users hangar-volumes@pve" "--tokens hangar-volumes@pve!bench"; do
	# shellcheck disable=SC2086 # two words on purpose
	{
		pveum acl modify /pool/hangar --roles HangarVolumes $who
		pveum acl modify /storage/local-zfs --roles PVEDatastoreUser $who
		pveum acl modify /storage/local --roles HangarTemplates $who # a container shelf's archive
	}
done
# the images plugin: in the images pool, its builders (a VM made from a disk
# image, or a template copied whole; its first boot on a seed disc; its host
# name and one log file read through the guest agent) and the templates they
# become; in the machines' pool, a stopped machine read and cloned (a save)
# and the machines' disks read (which were born from a template), nothing
# of their power or config
iprivs="VM.Allocate,VM.Audit,VM.Clone,VM.Config.CDROM,VM.Config.CPU,VM.Config.Disk,VM.Config.HWType"
iprivs="$iprivs,VM.Config.Memory,VM.Config.Network,VM.Config.Options,VM.PowerMgmt,VM.GuestAgent.Audit"
iprivs="$iprivs,VM.GuestAgent.FileRead,Datastore.AllocateSpace,Datastore.Audit,Pool.Audit"
pveum role add HangarImagesMake --privs "$iprivs" 2>/dev/null || pveum role modify HangarImagesMake --privs "$iprivs"
pveum role add HangarImagesSave --privs VM.Audit,VM.Clone,Pool.Audit 2>/dev/null ||
	pveum role modify HangarImagesSave --privs VM.Audit,VM.Clone,Pool.Audit
pveum user add hangar-images@pve --comment "hangar's images plugin" 2>/dev/null || true
if [ ! -s /root/images-token ]; then
	pveum user token remove hangar-images@pve bench 2>/dev/null || true
	secret=$(pveum user token add hangar-images@pve bench --privsep 1 --output-format json |
		sed -n 's/.*"value":"\([^"]*\)".*/\1/p')
	[ -n "$secret" ] || { say "the images token was not made"; exit 1; }
	(umask 077 && printf 'hangar-images@pve!bench=%s\n' "$secret" >/root/images-token)
fi
for who in "--users hangar-images@pve" "--tokens hangar-images@pve!bench"; do
	# shellcheck disable=SC2086 # two words on purpose
	{
		pveum acl modify /pool/hangar-images --roles HangarImagesMake $who
		pveum acl modify /pool/hangar --roles HangarImagesSave $who
		pveum acl modify /storage/local-zfs --roles PVEDatastoreUser $who
		pveum acl modify /storage/local --roles HangarTemplates $who # a base disk image to import
		pveum acl modify /storage/hangar-seeds --roles HangarSeeds $who
		pveum acl modify /sdn/zones/hbench/hbnet --roles PVESDNUser $who
	}
done
# A control for the fence: a token that reaches everything (root's, not
# separated). The driver must open with it and refuse to call it fenced.
if [ ! -s /root/wide-token ]; then
	pveum user token remove root@pam wide 2>/dev/null || true
	secret=$(pveum user token add root@pam wide --privsep 0 --output-format json |
		sed -n 's/.*"value":"\([^"]*\)".*/\1/p')
	(umask 077 && printf 'root@pam!wide=%s\n' "$secret" >/root/wide-token)
fi
# ---- 6. the wall ------------------------------------------------------------
# The node's own firewall is said off BEFORE the cluster's is turned on: with
# both on, the node would hear its own subnet only, and the tests come from
# elsewhere.
if ! grep -q '^\[group hangar-floor\]' /etc/pve/firewall/cluster.fw 2>/dev/null; then
	say "the cluster's firewall on, the node's own off, the group hangar-floor"
	mkdir -p /etc/pve/firewall
	printf '[OPTIONS]\nenable: 0\n' >"/etc/pve/nodes/$node/host.fw"
	cat >/etc/pve/firewall/cluster.fw <<'FW'
[OPTIONS]
enable: 1

[group hangar-floor] # what every guest of a walled zone hears

IN ACCEPT -source 198.51.100.1 -p tcp -dport 22 -log nolog # the node, on ssh
FW
fi
say "done on $node"
