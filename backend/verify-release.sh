#!/usr/bin/env bash
# Fail-closed source AND shipped-artifact gate. Run from any directory.
set -euo pipefail
cd "$(dirname "$0")"
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"
export GOOS=linux GOARCH=amd64 GOAMD64=v1 GOEXPERIMENT= GOFLAGS=-mod=readonly
want_go="go$(awk '$1 == "go" {print $2}' go.mod)"
test "$(go env GOVERSION)" = "$want_go" || { echo "Use $want_go" >&2; exit 1; }
version="v$(jq -er .version ../manifest.json)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

go mod verify
# Fresh code generation + rebuild, without replacing the committed artifact.
OUTPUT="$work/starlinkd" VERSION="$version" bash ./build.sh
cmp bin/starlinkd "$work/starlinkd" || { echo "Bundled binary does not match source/toolchain/manifest; rebuild it." >&2; exit 1; }
test "$(bin/starlinkd -version)" = "starlinkd $version"
metadata=$(go version -m bin/starlinkd)
printf '%s\n' "$metadata"
test "$(printf '%s\n' "$metadata" | head -n 1)" = "bin/starlinkd: $want_go"
for setting in 'CGO_ENABLED=0' 'GOOS=linux' 'GOARCH=amd64' 'GOAMD64=v1' '-trimpath=true'; do
    grep -Fq -- "$setting" <<< "$metadata"
done
test -z "$(gofmt -l . | grep -v '^gen/' || true)"
go mod tidy -diff
CGO_ENABLED=1 go test -race -count=1 ./...
CGO_ENABLED=0 go vet ./...

# Text mode is intentional: JSON/SARIF modes do not fail on findings.
# The override supports a frozen official DB snapshot in an offline sandbox.
db="${GOVULNDB:-https://vuln.go.dev}"
CGO_ENABLED=0 govulncheck -db "$db" ./...
govulncheck -db "$db" -mode=binary bin/starlinkd
bash ./test-install.sh
echo "Release gate passed: $version ($want_go)"
