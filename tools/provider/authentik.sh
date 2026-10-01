#!/bin/sh
# authentik — a throwaway identity provider on this machine, for the sign-in
# tests against a real one: authentik in a podman pod, from zero, set up by
# one blueprint — a public client `hangar` that answers the authorization
# code flow (the console's) and lists the device grant (the command line's:
# its brand's device page is one more setting, docs/cli.md), two people in
# two groups. Nothing else lands on the host.
#
#   sh tools/provider/authentik.sh up        start it and wait until its client answers — re-run freely
#   sh tools/provider/authentik.sh env       the variables the provider tests read, as export lines
#   sh tools/provider/authentik.sh down      remove it and everything it kept
#   sh tools/provider/authentik.sh --help    this text
#
# It answers on http://127.0.0.1:$HANGAR_PROVIDER_PORT (default 19000); the
# console under test is expected at http://127.0.0.1:$HANGAR_PROVIDER_CONSOLE_PORT
# (default 18081) — the one redirect address its client knows. State lives in
# $HANGAR_PROVIDER_DIR (default ~/.cache/hangar-provider): the database and
# the generated passwords, 0600 and never printed. The first `up` takes ten
# minutes or so: authentik's migrations.
set -eu

case "${1:-}" in
-h | --help | help | "")
	sed -n '2,/^set -eu$/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
	exit 0
	;;
esac

here=$(cd "$(dirname "$0")" && pwd)
dir=${HANGAR_PROVIDER_DIR:-$HOME/.cache/hangar-provider}
port=${HANGAR_PROVIDER_PORT:-19000}
console=${HANGAR_PROVIDER_CONSOLE_PORT:-18081}
image=${HANGAR_PROVIDER_IMAGE:-ghcr.io/goauthentik/server:2026.8.3}
db=${HANGAR_PROVIDER_DB_IMAGE:-docker.io/library/postgres:17-alpine}
pod=hangar-provider

say() { printf 'provider: %s\n' "$*" >&2; }
die() {
	say "$*"
	exit 1
}
secret() { # secret <file>: made once, kept 0600
	[ -s "$dir/$1" ] || (umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$dir/$1")
}

case "$1" in
up)
	mkdir -p "$dir/db" "$dir/blueprints"
	chmod 700 "$dir"
	for s in db-password secret-key admin-password alice-password; do secret "$s"; done
	# the blueprint is read by authentik's own user: world-readable, and it holds no secret (they come from the environment)
	sed "s|@CONSOLE@|http://127.0.0.1:$console|g" "$here/authentik-blueprint.yaml" >"$dir/blueprints/hangar.yaml"
	chmod 755 "$dir/blueprints" && chmod 644 "$dir/blueprints/hangar.yaml"
	(umask 077 && cat >"$dir/env" <<ENV
AUTHENTIK_SECRET_KEY=$(cat "$dir/secret-key")
AUTHENTIK_POSTGRESQL__HOST=127.0.0.1
AUTHENTIK_POSTGRESQL__USER=authentik
AUTHENTIK_POSTGRESQL__NAME=authentik
AUTHENTIK_POSTGRESQL__PASSWORD=$(cat "$dir/db-password")
AUTHENTIK_BOOTSTRAP_PASSWORD=$(cat "$dir/admin-password")
AUTHENTIK_ERROR_REPORTING__ENABLED=false
AUTHENTIK_DISABLE_UPDATE_CHECK=true
AUTHENTIK_DISABLE_STARTUP_ANALYTICS=true
HANGAR_ALICE_PASSWORD=$(cat "$dir/alice-password")
POSTGRES_USER=authentik
POSTGRES_DB=authentik
POSTGRES_PASSWORD=$(cat "$dir/db-password")
ENV
	)
	if ! podman pod exists "$pod"; then
		podman pod create --name "$pod" -p "127.0.0.1:$port:9000" >/dev/null
		podman run -d --pod "$pod" --name "$pod-db" --env-file "$dir/env" -v "$dir/db:/var/lib/postgresql/data:Z" "$db" >/dev/null
		podman run -d --pod "$pod" --name "$pod-server" --env-file "$dir/env" -v "$dir/blueprints:/blueprints/hangar:ro,Z" "$image" server >/dev/null
		# in one pod the worker needs ports of its own
		podman run -d --pod "$pod" --name "$pod-worker" --env-file "$dir/env" -v "$dir/blueprints:/blueprints/hangar:ro,Z" \
			-e AUTHENTIK_LISTEN__HTTP=0.0.0.0:9010 -e AUTHENTIK_LISTEN__HTTPS=0.0.0.0:9453 -e AUTHENTIK_LISTEN__METRICS=0.0.0.0:9301 "$image" worker >/dev/null
	else
		podman pod start "$pod" >/dev/null
	fi
	say "waiting for the client to answer (the first start runs authentik's migrations: ten minutes or so)"
	i=0
	# asked from inside its own container: nothing on the host but podman
	until podman exec "$pod-server" python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:9000/application/o/hangar/.well-known/openid-configuration', timeout=5)" >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -lt 240 ] || die "no answer after 20 minutes: podman logs $pod-server"
		# the server can lose a port race at its first start, beside the worker
		[ "$(podman inspect -f '{{.State.Running}}' "$pod-server" 2>/dev/null)" = true ] || podman start "$pod-server" >/dev/null
		sleep 5
	done
	say "up: http://127.0.0.1:$port — sh tools/provider/authentik.sh env"
	;;
env)
	[ -s "$dir/alice-password" ] || die "not set up: sh tools/provider/authentik.sh up"
	cat <<ENV
export HANGAR_PROVIDER_ISSUER=http://127.0.0.1:$port/application/o/hangar/
export HANGAR_PROVIDER_CLIENT=hangar
export HANGAR_PROVIDER_CONSOLE=127.0.0.1:$console
export HANGAR_PROVIDER_USER=alice
export HANGAR_PROVIDER_PASSWORD_FILE=$dir/alice-password
export HANGAR_PROVIDER_DIR=$dir
ENV
	;;
down)
	podman pod rm -f "$pod" >/dev/null 2>&1 || true
	[ -d "$dir" ] && podman unshare rm -rf "$dir"
	say "removed"
	;;
*)
	die "up, env or down (--help)"
	;;
esac
