#!/usr/bin/env bash
# Build script for starlinkd.
#
# Proto codegen: the descriptors come from the dish itself (proto/dish.pb,
# captured via gRPC server reflection) — no hand-maintained .proto drift.
# Runtime: the built binary is static. Build tools: the exact Go in go.mod,
# protoc, protoc-gen-go v1.36.12, protoc-gen-go-grpc v1.6.2, and jq.
set -euo pipefail
cd "$(dirname "$0")"

# Go toolchain: prefer PATH, fall back to mise.
if command -v go >/dev/null 2>&1; then GO=go; else GO="mise x go -- go"; fi
export PATH="$HOME/.local/bin:$PATH"
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"
WANT_GO="go$(awk '$1 == "go" {print $2}' go.mod)"
if [ "$($GO env GOVERSION)" != "$WANT_GO" ]; then
    echo "Build requires $WANT_GO; select it explicitly (GOTOOLCHAIN=$WANT_GO)." >&2
    exit 1
fi
VERSION="${VERSION:-v$(jq -er .version ../manifest.json)}"
if [ "$VERSION" != "v$(jq -er .version ../manifest.json)" ]; then
    echo "VERSION must match manifest.json" >&2
    exit 1
fi
export GOOS=linux GOARCH=amd64 GOAMD64=v1 GOEXPERIMENT= GOFLAGS=-mod=readonly
test "$(protoc-gen-go --version)" = 'protoc-gen-go v1.36.12'
test "$(protoc-gen-go-grpc --version)" = 'protoc-gen-go-grpc 1.6.2'

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

# Deterministic: CGO off (fully static), no VCS stamping (amend-proof);
# version comes from the manifest, not the previous tag on a release branch.
OUTPUT="${OUTPUT:-bin/starlinkd}"
CGO_ENABLED=0 $GO build -trimpath -buildvcs=false -ldflags="-s -w -X main.version=$VERSION" -o "$OUTPUT" .
echo "built $OUTPUT ($VERSION)" >&2
