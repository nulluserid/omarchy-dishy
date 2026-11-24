#!/usr/bin/env bash
# Exercise install.sh without ever touching the real user service/config.
set -euo pipefail
# install.sh refuses group/world-writable ancestors, so the fake HOME must not
# inherit a permissive umask (002 on some CI runners).
umask 022
cd "$(dirname "$0")/.."
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/home" "$work/tools" "$work/clone"
cp install.sh "$work/clone/"
cat > "$work/tools/systemctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$HOME/systemctl-calls"
EOF
chmod +x "$work/tools/systemctl"
# install.sh closes its PATH and takes systemctl from SYSTEMCTL (its one
# documented override): the decoy stat placed first on PATH must never run.
printf '#!/usr/bin/env bash\necho stat >> "$HOME/decoy-calls"\n' > "$work/tools/stat"
chmod +x "$work/tools/stat"
export HOME="$work/home" PATH="$work/tools:$PATH" SYSTEMCTL="$work/tools/systemctl"
if bash "$work/clone/install.sh" > "$work/rejected.log" 2>&1; then
    echo "Installer accepted a non-canonical checkout" >&2
    exit 1
fi
test ! -e "$HOME/systemctl-calls"
test ! -e "$HOME/.config/systemd/user"
plugin="$HOME/.config/omarchy/plugins/io.github.nulluserid.dishy"
mkdir -p "$plugin/backend/bin" "$plugin/deploy"
cp install.sh "$plugin/"
cp deploy/starlinkd.service "$plugin/deploy/"
printf '#!/usr/bin/env bash\nexit 0\n' > "$plugin/backend/bin/starlinkd"
chmod +x "$plugin/backend/bin/starlinkd"
# A missing tool (systemctl here) is refused by name before anything changes.
missing="$work/tools/no-such-systemctl"
if SYSTEMCTL="$missing" bash "$plugin/install.sh" > "$work/no-tool.log" 2>&1; then
    echo "Installer ran with a missing tool" >&2
    exit 1
fi
grep -Fq -- "$missing" "$work/no-tool.log"
test ! -e "$HOME/systemctl-calls"
test ! -e "$HOME/.config/systemd/user"
# A unit directory (or an ancestor) writable by others is refused and named;
# nothing is written and systemctl is never called.
mkdir -p "$HOME/.config/systemd/user"
for unsafe in "$HOME/.config/systemd/user" "$HOME/.config"; do
    chmod 0777 "$unsafe"
    if bash "$plugin/install.sh" > "$work/unsafe.log" 2>&1; then
        echo "Installer accepted an unsafe directory: $unsafe" >&2
        exit 1
    fi
    grep -Fq -- "$unsafe" "$work/unsafe.log"
    test ! -e "$HOME/systemctl-calls"
    test -z "$(ls -A "$HOME/.config/systemd/user")"
    chmod go-w "$unsafe"
done
# A symlink to the canonical checkout is still the same physical directory.
ln -s "$plugin" "$work/plugin-link"
bash "$work/plugin-link/install.sh" > "$work/accepted.log" 2>&1
cmp "$plugin/deploy/starlinkd.service" "$HOME/.config/systemd/user/starlinkd.service"
test "$(wc -l < "$HOME/systemctl-calls")" -eq 4
grep -Fxq -- '--user restart starlinkd.service' "$HOME/systemctl-calls"
test ! -e "$HOME/decoy-calls"
echo 'Installer location/side-effect tests passed'
