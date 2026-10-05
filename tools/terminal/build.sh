#!/bin/sh
# terminal — the second thing in the console's app that the app did not
# write: a machine's terminal is drawn by xterm.js, vendored as ONE file (and
# its stylesheet beside it) and built here from the versions
# package-lock.json pins. What is in ui/console/vendor/ is what runs; this is
# how it is made again, and how it is checked.
#
#   sh tools/terminal/build.sh           build ui/console/vendor/xterm.js, its stylesheet and the
#                                        licences of what is in it beside it
#   sh tools/terminal/build.sh --check   build it again aside and compare: 0 = what is vendored
#                                        is what the lock builds, byte for byte
#   sh tools/terminal/build.sh --lock    write package-lock.json again from package.json (a
#                                        version moved there): then build, and read the diff
#   sh tools/terminal/build.sh --help    this text
#
# It needs a container engine (podman, else docker; $HANGAR_ENGINE names one)
# and the npm registry — nothing of Node on this machine. No package's
# install script runs.
set -eu

case "${1:-}" in
-h | --help | help)
	sed -n '2,/^set -eu$/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
	exit 0
	;;
"" | --check | --lock) ;;
*)
	echo "terminal: no such way: $1 (--help)" >&2
	exit 2
	;;
esac

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
vendor=$repo/ui/console/vendor
image=docker.io/library/node:24-alpine
engine=${HANGAR_ENGINE:-}
if [ -z "$engine" ]; then
	if command -v podman >/dev/null 2>&1; then engine=podman; else engine=docker; fi
fi

if [ "${1:-}" = "--lock" ]; then
	"$engine" run --rm -v "$here":/work -w /work "$image" \
		npm install --package-lock-only --ignore-scripts --no-audit --no-fund
	echo "terminal: package-lock.json written - build next"
	exit 0
fi

out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
chmod 755 "$out"
# in the container: the pinned packages, one bundle, the library's own
# stylesheet (its rules, as it ships them), and each package's own licence
# under its name and version
"$engine" run --rm -v "$here":/src:ro -v "$out":/out "$image" sh -euc '
	mkdir /work && cd /work
	cp /src/package.json /src/package-lock.json /src/entry.js .
	npm ci --ignore-scripts --no-audit --no-fund >/dev/null
	./node_modules/.bin/esbuild entry.js --bundle --format=esm --minify --target=es2020 --legal-comments=none \
		--banner:js="// xterm.js, vendored: built by tools/terminal/build.sh from the versions tools/terminal/package-lock.json pins. MIT - xterm.licenses.txt." \
		--outfile=/out/xterm.js --log-level=warning
	./node_modules/.bin/esbuild node_modules/@xterm/xterm/css/xterm.css --minify --legal-comments=none \
		--banner:css="/* xterm.js, vendored: its own stylesheet, built by tools/terminal/build.sh. MIT - xterm.licenses.txt. */" \
		--outfile=/out/xterm.css --log-level=warning
	{
		echo "What ui/console/vendor/xterm.js and xterm.css are made of, each under its own licence."
		echo "Built by tools/terminal/build.sh from tools/terminal/package-lock.json."
		for d in node_modules/@xterm/xterm node_modules/@xterm/addon-fit node_modules/@xterm/addon-webgl; do
			[ -f "$d/package.json" ] || continue
			echo
			echo "================================================================"
			node -e "const p = require(\"./$d/package.json\"); console.log(p.name + \" \" + p.version + \" - \" + p.license)"
			echo "================================================================"
			cat "$d/LICENSE" 2>/dev/null || echo "(no LICENSE file in the package: see its license field above)"
		done
	} >/out/xterm.licenses.txt
'

files="xterm.js xterm.css xterm.licenses.txt"
if [ "${1:-}" = "--check" ]; then
	for f in $files; do
		if ! cmp -s "$out/$f" "$vendor/$f"; then
			echo "terminal: ui/console/vendor/$f is not what the lock builds - sh tools/terminal/build.sh, and read the diff" >&2
			exit 1
		fi
	done
	echo "terminal: what is vendored is what the lock builds"
	exit 0
fi
mkdir -p "$vendor"
for f in $files; do cp "$out/$f" "$vendor/"; done
echo "terminal: $(wc -c <"$vendor/xterm.js") bytes - ui/console/vendor/xterm.js"
