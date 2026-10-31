#!/usr/bin/env bash
# Exercise install.sh without ever touching the real user service/config.
set -euo pipefail
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
export HOME="$work/home" PATH="$work/tools:$PATH"
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
# A symlink to the canonical checkout is still the same physical directory.
ln -s "$plugin" "$work/plugin-link"
bash "$work/plugin-link/install.sh" > "$work/accepted.log" 2>&1
cmp "$plugin/deploy/starlinkd.service" "$HOME/.config/systemd/user/starlinkd.service"
test "$(wc -l < "$HOME/systemctl-calls")" -eq 4
grep -Fxq -- '--user restart starlinkd.service' "$HOME/systemctl-calls"
echo 'Installer location/side-effect tests passed'
