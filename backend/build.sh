#!/usr/bin/env bash
# Build script for starlinkd.
#
# Proto codegen: the descriptors come from the dish itself (proto/dish.pb,
# captured via gRPC server reflection) — no hand-maintained .proto drift.
# Runtime: the built binary is static; only protoc + a Go toolchain are needed
# at build time (go via mise if not on PATH).
set -euo pipefail
cd "$(dirname "$0")"

# Go toolchain: prefer PATH, fall back to mise.
if command -v go >/dev/null 2>&1; then GO=go; else GO="mise x go -- go"; fi
export PATH="$HOME/.local/bin:$PATH"

GEN=gen
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT

# Generate Go stubs from the dish's own descriptor set. Every spacex_api /
# telemetron file is generated so cross-file imports resolve; well-known types
# (google/protobuf/*) come from the protobuf Go runtime instead.
#
# paths=import + per-file M mappings (one Go package per proto file, under
# starlinkd/gen/<proto-path>) — flat source_relative layout breaks: protos in
# the same directory carry conflicting Go package clauses. Output goes to a
# temp root (import paths embed the module prefix) and gen/ is moved into
# place afterwards.
MFLAGS=()
INPUTS=()
while read -r f; do
    imp="starlinkd/$GEN/${f%.proto}"
    MFLAGS+=("--go_opt=M$f=$imp" "--go-grpc_opt=M$f=$imp")
    INPUTS+=("$f")
done < <(strings proto/dish.pb | grep -oE '(spacex_api|spacex|telemetron)/[a-zA-Z0-9_/]+\.proto' | sort -u)

protoc --descriptor_set_in=proto/dish.pb \
    --go_out="$OUT" --go_opt=paths=import \
    --go-grpc_out="$OUT" --go-grpc_opt=paths=import \
    "${MFLAGS[@]}" "${INPUTS[@]}"

rm -rf "$GEN"
mkdir -p "$GEN"
mv "$OUT/starlinkd/$GEN"/* "$GEN"/

$GO mod tidy
CGO_ENABLED=0 $GO build -trimpath -ldflags="-s -w" -o bin/starlinkd .
echo "built bin/starlinkd"
