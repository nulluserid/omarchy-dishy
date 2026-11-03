#!/usr/bin/env bash
# Isolated negative cases; never modify the checkout's binary or notices.
set -euo pipefail
cd "$(dirname "$0")"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/backend" "$work/tools" "$work/grpc"
cp check-licenses.sh THIRD_PARTY_NOTICES.txt go.mod go.sum "$work/backend/"
go version -m bin/starlinkd > "$work/original-metadata"
export REAL_GO
REAL_GO=$(command -v go)
export LICENSE_TEST_WORK="$work"
cat > "$work/tools/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "$*" = 'version -m bin/starlinkd' ]; then
    cat "$LICENSE_TEST_WORK/metadata"
elif [ "$*" = 'list -m -json google.golang.org/grpc' ] && [ -f "$LICENSE_TEST_WORK/missing-upstream" ]; then
    "$REAL_GO" "$@" | jq --arg d "$LICENSE_TEST_WORK/grpc" '.Dir = $d'
else
    exec "$REAL_GO" "$@"
fi
EOF
chmod +x "$work/tools/go"
export PATH="$work/tools:$PATH"
reset_fixture() {
    cp "$work/original-metadata" "$work/metadata"
    cp THIRD_PARTY_NOTICES.txt "$work/backend/"
    rm -f "$work/missing-upstream"
}
reject() {
    if (cd "$work/backend"; bash check-licenses.sh) > "$work/rejected.log" 2>&1; then
        echo "License check accepted: $1" >&2; exit 1
    fi
    grep -Fq "$2" "$work/rejected.log"
    echo "Rejected: $1"
}
reset_fixture
(cd "$work/backend"; bash check-licenses.sh)
rm "$work/backend/THIRD_PARTY_NOTICES.txt"
reject 'missing distributed notices' 'THIRD_PARTY_NOTICES.txt missing/stale'
reset_fixture
sed -i '/Copyright 2014 gRPC authors./d' "$work/backend/THIRD_PARTY_NOTICES.txt"
reject 'removed grpc attribution' 'THIRD_PARTY_NOTICES.txt missing/stale'
reset_fixture
sed -i '/dep.*google.golang.org\/grpc/d' "$work/metadata"
reject 'missing linked component' 'Linked module inventory changed'
reset_fixture
printf '\tdep\texample.com/new\tv1.0.0\th1:test\n' >> "$work/metadata"
reject 'unreviewed dependency' 'Linked module inventory changed'
reset_fixture
sed -i 's/v1.84.0/v1.84.1/g' "$work/metadata"
reject 'unreviewed dependency version' 'Linked module inventory changed'
reset_fixture
printf '\t=>\texample.com/replacement\tv1.0.0\th1:test\n' >> "$work/metadata"
reject 'module replacement' 'Module replacements require notice review'
reset_fixture
sed -i '1s/go1.27.3/go1.27.2/' "$work/metadata"
reject 'different bundled compiler' 'Binary compiler differs'
reset_fixture
dir=$("$REAL_GO" list -m -f '{{.Dir}}' google.golang.org/grpc)
cp "$dir/LICENSE" "$work/grpc/"
touch "$work/missing-upstream"
reject 'missing required upstream NOTICE' 'Missing upstream text: google.golang.org/grpc@v1.84.0/NOTICE.txt'
before=$(sha256sum "$work/backend/THIRD_PARTY_NOTICES.txt")
if (cd "$work/backend"; bash check-licenses.sh --write) > "$work/write.log" 2>&1; then
    echo 'Generator accepted missing upstream NOTICE' >&2; exit 1
fi
test "$before" = "$(sha256sum "$work/backend/THIRD_PARTY_NOTICES.txt")"
reset_fixture
(cd "$work/backend"; bash check-licenses.sh --write; bash check-licenses.sh)
cmp THIRD_PARTY_NOTICES.txt "$work/backend/THIRD_PARTY_NOTICES.txt"
echo 'License inventory, notice integrity, and generation tests passed'
