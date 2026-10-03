#!/bin/sh
# no-environment — this repo is a product; it must not describe a network.
#
# Anyone can run this thing. Their addresses, hostnames, domains, VLAN and
# group names and house word live in whatever configuration manages it, and
# the only thing crossing between that and this repo is a pinned tag. So the
# examples and the tests here describe nowhere: they use the reserves the
# RFCs set aside for documentation, which are guaranteed never to be anybody.
#
#   IPv4   192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24   RFC 5737
#   IPv6   2001:db8::/32                                   RFC 3849
#   MAC    00:00:5E:00:53:00-FF                            RFC 7042
#   names  example.com / .net / .org                       RFC 2606
#
# Note what this does NOT do: it carries no list of forbidden names. A
# denylist of somebody's hostnames would be that somebody's environment,
# published here, in the one file the check could never look at. Whoever runs
# this keeps that list in their own repo, where those names belong.
#
# Every MATCH is judged, not every line: a line that migrates 10.0.0.1 to
# 192.0.2.1 must not pass because the good address is standing next to the
# bad one. A value that genuinely has to be real marks its line with a
# trailing `no-environment: ok`.
#
# Usage: sh tools/no-environment.sh    # 0 = clean, 1 = the world leaked in
set -u

self='tools/no-environment.sh'
status=0
tmp=$(mktemp); hatch=$(mktemp)
trap 'rm -f "$tmp" "$hatch"' EXIT INT TERM

# Everything git tracks, minus this file — which is full of the shapes it
# forbids and would fail itself — and minus the licences of what the console
# vendors: their authors' own words, kept as they wrote them, their names and
# mailboxes with them (tools/editor/build.sh writes that file, and --check
# holds it to what the lock builds).
licences='ui/console/vendor/codemirror.licenses.txt'
files=$(git ls-files | grep -v -e "^$self$" -e "^$licences$")
# Prose, where a hostname is written plainly rather than quoted. Code and
# config are excluded from the unquoted rule: `p.local`, `s.home` and Jinja's
# `item.home` are field accesses, not houses. In those files a bare hostname
# is caught by the quoted rule instead, which is where a config puts one.
docs=$(printf '%s\n' "$files" | grep -E '\.(md|markdown|txt|rst|adoc)$')

# The lines that excused themselves, as "path:line:" prefixes.
printf '%s\n' "$files" | xargs -r grep -lIE 'no-environment: ok' 2>/dev/null \
	| xargs -r grep -HnIE 'no-environment: ok' 2>/dev/null \
	| cut -d: -f1,2 | sed 's/$/:/' > "$hatch"

scan() {
	# scan <file-list> <what> <extended-regex> <allowed-match-regex>
	printf '%s\n' "$1" | xargs -r grep -HonIE "$3" 2>/dev/null \
		| grep -vE ":($4)$" \
		| grep -vFf "$hatch" > "$tmp"
	[ -s "$tmp" ] || return 0
	echo
	echo "=== $2"
	sed 's/^/  /' "$tmp"
	status=1
}

# ---- addresses -----------------------------------------------------------
# Allowed beside the documentation reserves: the unspecified address, loopback,
# netmasks and the link-local block — none of them name anybody.
ip_ok='192\.0\.2\.[0-9]{1,3}|198\.51\.100\.[0-9]{1,3}|203\.0\.113\.[0-9]{1,3}|0\.0\.0\.0|127\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|169\.254\.[0-9]{1,3}\.[0-9]{1,3}|255\.(0|128|192|224|240|248|252|254|255)\.(0|128|192|224|240|248|252|254|255)\.(0|128|192|224|240|248|252|254|255)'
scan "$files" "an IPv4 address that is not documentation (RFC 5737: 192.0.2/24, 198.51.100/24, 203.0.113/24)" \
	'([0-9]{1,3}\.){3}[0-9]{1,3}' "$ip_ok"

# Only the two shapes a MAC cannot wear: eight groups, or a `::` run. Both
# must stand alone, or CSS's `::after` reads as the address `::af`.
edge='[^0-9a-zA-Z:]'
scan "$files" "an IPv6 address that is not documentation (RFC 3849: 2001:db8::/32)" \
	"(^|$edge)(([0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}|(([0-9a-fA-F]{1,4}:)*[0-9a-fA-F]{1,4})?::([0-9a-fA-F]{1,4}:)*[0-9a-fA-F]{1,4})($edge|\$)" \
	"$edge?(2001:[dD][bB]8:[0-9a-fA-F:]*|::1)$edge?"

# ---- hardware ------------------------------------------------------------
scan "$files" "a MAC address that is not documentation (RFC 7042: 00:00:5E:00:53:xx)" \
	'([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}' \
	'00:00:5[eE]:00:53:[0-9a-fA-F]{2}|00:00:00:00:00:00|[fF]{2}(:[fF]{2}){5}'

# ---- names ---------------------------------------------------------------
# Dependencies a build or a reader genuinely reaches for are named here, once.
# Anything else is somebody's domain.
dep='(([a-z0-9-]+\.)*(example\.(com|net|org)|github\.com|githubusercontent\.com|google\.com|ghcr\.io|docker\.io|golang\.org|go\.dev|gopkg\.in|npmjs\.org|w3\.org|opensource\.org|sil\.org|flathub\.org|freedesktop\.org|kernel\.org|ietf\.org|rfc-editor\.org|schema\.org|openapis\.org|htmx\.org|alpinelinux\.org|json-schema\.org|modernc\.org|buf\.build|gonum\.org|connectrpc\.com|pluginrpc\.com|go\.uber\.org|proxmox\.com|debian\.org|openssh\.com)|[a-z0-9-]+\.example|localhost)'
scan "$files" "a URL to somewhere real (documentation uses example.com — RFC 2606)" \
	'https?://[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?\.[a-zA-Z]{2,}' \
	"https?://$dep"

# A public TLD is a hostname wherever it appears. The private ones (.local,
# .lan, .home, .internal) are also ordinary field names — `p.local`, `s.home` —
# so in source they count only inside quotes; in prose and config, always.
# The trailing \b matters: without it `Array.from` reads as the domain `rray.fr`.
q=$(printf '["\047`]')
host='([a-z0-9][a-z0-9-]*\.)+'
scan "$files" "a hostname or mailbox that is neither documentation nor a declared dependency" \
	"$host(fr|com|net|org|arpa)\b|$q$host(lan|local|home|internal)\b" \
	"$q?$dep"
scan "$docs" "a private-network hostname (.local/.lan/.home/.internal names somebody's house)" \
	"$host(lan|local|home|internal)\b" "$dep"

if [ "$status" -ne 0 ]; then
	echo
	echo "^ an environment leaked into the product. Move it to the repo that"
	echo "  configures this, and use the documentation reserves here instead."
	echo "  A line that truly needs a real value marks itself: no-environment: ok"
	exit 1
fi
echo "clean: this repo describes no network"
