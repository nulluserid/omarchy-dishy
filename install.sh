#!/bin/bash
# Installs the starlinkd systemd user service that feeds the Dishy plugin.
# Idempotent; no sudo required.
set -euo pipefail

# Trust no caller PATH: read SYSTEMCTL first (the one supported override,
# SYSTEMCTL=/path/to/systemctl), then run root-owned tools only, from a
# closed PATH, refusing if any is missing.
SYSTEMCTL="${SYSTEMCTL:-/usr/bin/systemctl}"
export PATH=/usr/local/bin:/usr/bin:/bin
for tool in cp cmp mkdir dirname date stat "$SYSTEMCTL"; do
    if ! command -v "$tool" > /dev/null; then
        echo "Refusing to install: required tool not found: $tool" >&2
        exit 1
    fi
done

cd "$(dirname "$0")"

# The unit deliberately runs the marketplace checkout, not an arbitrary clone.
# Refuse before changing any service/config if that is not what we checked.
PLUGIN_DIR="$HOME/.config/omarchy/plugins/io.github.nulluserid.dishy"
if [ ! -d "$PLUGIN_DIR" ] || [ "$(pwd -P)" != "$(cd "$PLUGIN_DIR" && pwd -P)" ]; then
    echo "Run install.sh from $PLUGIN_DIR (install/update the Omarchy plugin first)." >&2
    exit 1
fi

UNIT_DST="$HOME/.config/systemd/user/starlinkd.service"

if [ ! -x backend/bin/starlinkd ]; then
    echo "backend/bin/starlinkd is missing — build it with: backend/build.sh (needs go + protoc)" >&2
    exit 1
fi

# A directory another account can write to could plant a unit that runs as
# this user at desktop start: nothing is written until every directory from
# the unit's own up to $HOME is ours (or root's) and closed to group/other
# writes.
mkdir -p "$(dirname "$UNIT_DST")"
dir="$(dirname "$UNIT_DST")"
while :; do
    if ! meta="$(stat -Lc '%u %a' "$dir")"; then
        echo "Refusing to install: cannot inspect $dir" >&2
        exit 1
    fi
    owner="${meta%% *}"
    mode="${meta##* }"
    if [[ $owner != "0" && $owner != "$EUID" ]]; then
        remedy="chown \"${USER:-$EUID}\" \"$dir\" (as the administrator)"
    elif (( 8#$mode & 022 )); then
        remedy="chmod go-w \"$dir\""
    else
        remedy=
    fi
    if [ -n "$remedy" ]; then
        echo "Refusing to install: $dir (owner uid $owner, mode $mode) must be owned by you or root and not writable by group/others — fix with: $remedy" >&2
        exit 1
    fi
    if [ "$dir" = "${HOME%/}" ] || [ "$dir" = / ]; then
        break
    fi
    dir="$(dirname "$dir")"
done
if [ -f "$UNIT_DST" ] && ! cmp -s deploy/starlinkd.service "$UNIT_DST"; then
    cp "$UNIT_DST" "$UNIT_DST.bak.$(date +%s)"
    echo "note: existing starlinkd.service backed up"
fi
cp deploy/starlinkd.service "$UNIT_DST"
"$SYSTEMCTL" --user daemon-reload
"$SYSTEMCTL" --user enable starlinkd.service
# restart (not enable --now): a running service must re-exec to pick up an
# updated backend/bin/starlinkd; restart also starts a stopped one.
"$SYSTEMCTL" --user restart starlinkd.service
echo "--- starlinkd is installed and running:"
"$SYSTEMCTL" --user status starlinkd.service --no-pager -n 3
echo "The Dishy bar widget picks up live data within ~2 seconds."
echo "Off Starlink? The widget hides itself and keeps checking in the background."
