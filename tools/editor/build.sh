#!/bin/sh
# editor — the one thing in the console's app that the app did not write: the
# box JSON is typed in is CodeMirror 6, vendored as ONE file and built here
# from the versions package-lock.json pins. What is in ui/console/vendor/ is
# what runs; this is how it is made again, and how it is checked.
#
#   sh tools/editor/build.sh           build ui/console/vendor/codemirror.js, and the licences
#                                      of what is in it beside it
#   sh tools/editor/build.sh --check   build it again aside and compare: 0 = what is vendored
#                                      is what the lock builds, byte for byte
#   sh tools/editor/build.sh --lock    write package-lock.json again from package.json (a
#                                      version moved there): then build, and read the diff
#   sh tools/editor/build.sh --help    this text
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
	echo "editor: no such way: $1 (--help)" >&2
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
	echo "editor: package-lock.json written - build next"
	exit 0
fi

out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
chmod 755 "$out"
# in the container: the pinned packages, one bundle, and each package's own
# licence under its name and version
"$engine" run --rm -v "$here":/src:ro -v "$out":/out "$image" sh -euc '
	mkdir /work && cd /work
	cp /src/package.json /src/package-lock.json /src/entry.js .
	npm ci --ignore-scripts --no-audit --no-fund >/dev/null
	./node_modules/.bin/esbuild entry.js --bundle --format=esm --minify --target=es2020 --legal-comments=none \
		--banner:js="// CodeMirror 6, vendored: built by tools/editor/build.sh from the versions tools/editor/package-lock.json pins. MIT - codemirror.licenses.txt." \
		--outfile=/out/codemirror.js --log-level=warning
	{
		echo "What ui/console/vendor/codemirror.js is made of, each under its own licence."
		echo "Built by tools/editor/build.sh from tools/editor/package-lock.json."
		for d in node_modules/@codemirror/* node_modules/@lezer/* node_modules/style-mod node_modules/w3c-keyname node_modules/crelt node_modules/@marijn/find-cluster-break; do
			[ -f "$d/package.json" ] || continue
			echo
			echo "================================================================"
			node -e "const p = require(\"./$d/package.json\"); console.log(p.name + \" \" + p.version + \" - \" + p.license)"
			echo "================================================================"
			cat "$d/LICENSE" 2>/dev/null || echo "(no LICENSE file in the package: see its license field above)"
		done
	} >/out/codemirror.licenses.txt
'

if [ "${1:-}" = "--check" ]; then
	for f in codemirror.js codemirror.licenses.txt; do
		if ! cmp -s "$out/$f" "$vendor/$f"; then
			echo "editor: ui/console/vendor/$f is not what the lock builds - sh tools/editor/build.sh, and read the diff" >&2
			exit 1
		fi
	done
	echo "editor: what is vendored is what the lock builds"
	exit 0
fi
mkdir -p "$vendor"
cp "$out/codemirror.js" "$out/codemirror.licenses.txt" "$vendor/"
echo "editor: $(wc -c <"$vendor/codemirror.js") bytes - ui/console/vendor/codemirror.js"
