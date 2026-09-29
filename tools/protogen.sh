#!/bin/sh
# protogen — regenerate the plugin protocol's Go code from proto/.
#
# The tools are pinned in tools/go.mod (buf, protoc-gen-go,
# protoc-gen-go-grpc), so every machine and CI generate the same bytes.
# `--check` regenerates and fails when the committed code differs.
#
# Usage: sh tools/protogen.sh [--check]
set -eu
cd "$(dirname "$0")/.."
buf() { go tool -modfile=tools/go.mod buf "$@"; }
buf lint
buf generate
if [ "${1:-}" = "--check" ]; then
	if ! git diff --exit-code -- sdk/pluginpb; then
		echo "the generated code is not what proto/ makes: run sh tools/protogen.sh" >&2
		exit 1
	fi
fi
