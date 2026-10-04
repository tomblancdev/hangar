#!/bin/sh
# image-test — what the image carries beside the binary, asked of the binary
# inside it.
#
# The image is `scratch`: nothing is in it that the Dockerfile did not put
# there, a trust store included. This builds it and runs its own checker on a
# config that reaches an identity provider over https — sound only where
# certificate authorities are trusted, which in the image means its bundle.
#
# The control builds the same image without the bundle, and the same check
# must refuse it, naming the provider: a test that passed both images would
# be reading nothing.
#
# And the image carries the recipe a network's gateway archive is built from
# (/gateway-build.sh): read back out of it, the repository's own file.
#
# Usage: sh tools/image-test.sh            # docker, or podman
#        CONTAINER=podman sh tools/image-test.sh
set -eu
case "${1:-}" in
-h | --help)
	sed -n '2,/^set -eu/{/^set -eu/d;s/^# \{0,1\}//;p}' "$0"
	exit 0
	;;
esac
cd "$(dirname "$0")/.."

engine=${CONTAINER:-}
if [ -z "$engine" ]; then
	for e in docker podman; do
		command -v "$e" >/dev/null 2>&1 && engine=$e && break
	done
fi
[ -n "$engine" ] || { echo "image-test: needs docker or podman" >&2; exit 2; }

tmp=$(mktemp -d)
tag="hangar-image-test:$$"
trap 'rm -rf "$tmp"; "$engine" rmi -f "$tag" "$tag-bare" >/dev/null 2>&1 || true' EXIT INT TERM

cat > "$tmp/hangar.yaml" <<'YAML'
identity:
  oidc: {issuer: "https://id.example.com/application/o/hangar/", audience: hangar}
tiers:
  - {name: users, groups: [users], zones: [], limits: {}}
YAML
chmod 644 "$tmp/hangar.yaml"

# the control's Dockerfile: the same, the bundle left out
bundle='/etc/ssl/certs/ca-certificates.crt'
grep -v "$bundle" Dockerfile > "$tmp/Dockerfile.bare"
if cmp -s Dockerfile "$tmp/Dockerfile.bare"; then
	echo "image-test: the Dockerfile copies no $bundle" >&2
	exit 1
fi

check() { # check <image> — the image's own checker on the config
	"$engine" run --rm --tmpfs /data:rw,mode=1777 \
		-v "$tmp/hangar.yaml:/etc/hangar/hangar.yaml:ro" "$1" check 2>&1
}

echo "image-test: building the image, and the same without its bundle"
"$engine" build -q -t "$tag" . >/dev/null
"$engine" build -q -t "$tag-bare" -f "$tmp/Dockerfile.bare" . >/dev/null

status=0
if out=$(check "$tag") && printf '%s\n' "$out" | grep -q '^sound:'; then
	echo "ok    the image verifies https: its checker is sound on a config with an https provider"
else
	echo "FAIL  the image's checker refused a config with an https provider:"
	printf '%s\n' "$out" | sed 's/^/        /'
	status=1
fi
if out=$(check "$tag-bare"); then
	echo "FAIL  the control — the image without its bundle — was found sound: this test reads nothing"
	status=1
elif printf '%s\n' "$out" | grep -q 'trusts no certificate authority.*id\.example\.com'; then
	echo "ok    the control: without the bundle, the same check refuses and names the provider"
else
	echo "FAIL  the control failed for another reason:"
	printf '%s\n' "$out" | sed 's/^/        /'
	status=1
fi
# the recipe a network's gateway archive is built from: the repository's own
# file, at the image's version — what an operator who holds the image runs
c=$("$engine" create "$tag")
if "$engine" cp "$c:/gateway-build.sh" "$tmp/gateway-build.sh" 2>/dev/null && cmp -s "$tmp/gateway-build.sh" tools/gateway/build.sh &&
	[ -n "$(sh "$tmp/gateway-build.sh" --version)" ]; then
	echo "ok    the image carries the gateway's recipe, and it is the repository's"
else
	echo "FAIL  the image does not carry the gateway's recipe (/gateway-build.sh)"
	status=1
fi
"$engine" rm "$c" >/dev/null 2>&1 || true
exit $status
