#!/usr/bin/env bash
# Installs the starlinkd systemd user service that feeds the Dishy plugin.
# Idempotent; no sudo required.
set -euo pipefail
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

mkdir -p "$(dirname "$UNIT_DST")"
if [ -f "$UNIT_DST" ] && ! cmp -s deploy/starlinkd.service "$UNIT_DST"; then
    cp "$UNIT_DST" "$UNIT_DST.bak.$(date +%s)"
    echo "note: existing starlinkd.service backed up"
fi
cp deploy/starlinkd.service "$UNIT_DST"
systemctl --user daemon-reload
systemctl --user enable starlinkd.service
# restart (not enable --now): a running service must re-exec to pick up an
# updated backend/bin/starlinkd; restart also starts a stopped one.
systemctl --user restart starlinkd.service
echo "--- starlinkd is installed and running:"
systemctl --user status starlinkd.service --no-pager -n 3
echo "The Dishy bar widget picks up live data within ~2 seconds."
echo "Off Starlink? The widget hides itself and keeps checking in the background."
