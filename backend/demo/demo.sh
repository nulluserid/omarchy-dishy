#!/bin/bash
# Dishy demo kit runner: a loopback-only fake dish plus a starlinkd sandboxed
# to its own state directory. See README.md next to this file.
#
#   LIVE_ACK=yes LIVE_RESTORE=<Dish address> DAEMON=/path/to/starlinkd demo.sh up
#   demo.sh status | demo.sh down
#
# Variables: DAEMON (required for up), SCENARIO=clean|busy|trouble,
# MODE=new|legacy (default: detected from the daemon), RUN (default
# $XDG_RUNTIME_DIR/dishy-demo), NOTIFY=on to leave the desktop notifier on PATH.
# `up` also needs LIVE_ACK=yes (real runtime directory, plus LIVE_RESTORE) or
# SANDBOX=yes (a sandbox XDG_RUNTIME_DIR); see README.md.
set -eu

KIT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
BACKEND=$(dirname "$KIT") # the Go module dishdemo builds in
GRPC=127.0.0.1:9210
GEO=127.0.0.1:9211
SCENARIO=${SCENARIO:-clean}
RUN=${RUN:-${XDG_RUNTIME_DIR:-}/dishy-demo}
SOCK=${XDG_RUNTIME_DIR:-}/starlinkd.sock                 # the daemon's panel control socket
STATE_LINK=${STATE_LINK:-${XDG_RUNTIME_DIR:-}/starlinkd} # the state-dir link a newer daemon publishes

die() { echo "demo.sh: $*" >&2; exit 1; }

# ---- listeners (read from the kernel's tables; nothing is ever dialed) ----
# tcp_listeners must be called as `out=$(...)`: a failing ss then stops the
# script instead of passing for "nothing is listening".

tcp_listeners() { # listening TCP sockets on port $1
    ss -H -ltn "sport = :$1" || die "ss failed"
}

sock_listening() { # is anything listening on the unix socket path $1?
    local out
    out=$(ss -H -xl) || die "ss failed"
    awk -v p="$1" '$5 == p { f = 1 } END { exit !f }' <<< "$out"
}

# ---- processes ----

# Field 22 of /proc/<pid>/stat is the start time in clock ticks since boot.
# It changes whenever the kernel reuses a pid, so a stale pidfile can never
# make this script signal a process it did not start.
proc_start() { sed 's/^.*) //' "/proc/$1/stat" 2> /dev/null | cut -d ' ' -f20; }

record_pid() { echo "$2 $(proc_start "$2")" > "$RUN/$1.pid"; } # record_pid <name> <pid>

# alive <name> says whether the recorded pid is still the process it was.
alive() {
    local pid start
    [ -f "$RUN/$1.pid" ] || return 1
    read -r pid start < "$RUN/$1.pid" || return 1
    [ -n "${start:-}" ] && [ "$(proc_start "${pid:-}")" = "$start" ]
}

stop_pid() { # stop only a pid this script recorded, and only while it is that process
    local name=$1 pid
    if alive "$name"; then
        read -r pid _ < "$RUN/$name.pid"
        kill "$pid" 2> /dev/null || true
        for _ in $(seq 1 50); do alive "$name" || break; sleep 0.1; done
        if alive "$name"; then echo "$name: pid $pid did not exit" >&2; return 1; fi
        echo "$name: stopped (pid $pid)"
    else
        echo "$name: not running"
    fi
    rm -f "$RUN/$name.pid"
}

# ---- what a run may take over ----

# ack_live_or_sandbox echoes 1 when the run takes the live shell over and 0
# for a sandbox runtime directory. The run must say which: nothing is started
# on a missing or inconsistent answer.
ack_live_or_sandbox() {
    local live
    case "${LIVE_ACK:-}:${SANDBOX:-}" in
        yes:) live=1 ;;
        :yes) live=0 ;;
        *) die "nothing was started: set LIVE_ACK=yes to take the live shell over, or SANDBOX=yes when XDG_RUNTIME_DIR is a sandbox" ;;
    esac
    if [ "$live" = 0 ]; then
        [ "$XDG_RUNTIME_DIR" != "/run/user/$(id -u)" ] \
            || die "SANDBOX=yes, but XDG_RUNTIME_DIR is the real runtime directory: use LIVE_ACK=yes"
        echo 0
        return 0
    fi
    [ -n "${LIVE_RESTORE:-}" ] || die "set LIVE_RESTORE to the Dish address the Dishy widget shows right now, so the notice can spell out what to restore"
    cat >&2 <<EOF
----------------------------------------------------------------------
 LIVE-SHELL DEMO: the Dishy widget's "Dish address" setting must follow it.
 The live shell's widget pushes that setting to whichever daemon answers the
 panel socket. This kit never changes the widget's settings; both steps are yours.
   For this session, set the Dish address to:      $GRPC
   Afterwards, restore it to its previous value:   $LIVE_RESTORE
----------------------------------------------------------------------
EOF
    echo 1
}

# refuse_if_busy stops the run while another daemon owns the panel socket or
# a demo port is taken: this kit never steals or stops anything.
refuse_if_busy() {
    local port out
    if [ -e "$SOCK" ] && sock_listening "$SOCK"; then
        die "something is listening on $SOCK: the real starlinkd owns the panel channel, or an earlier demo run is still up (demo.sh down). This kit never stops or displaces it."
    fi
    for port in "${GRPC##*:}" "${GEO##*:}"; do
        out=$(tcp_listeners "$port")
        [ -z "$out" ] || die "port $port is busy: nothing was started"
    done
}

# ---- daemon mode ----

# A daemon that knows -geo-url also honors STARLINKD_STATE_DIR and publishes
# the state-dir link; an older one needs the legacy treatment in cmd_up.
detect_mode() {
    if "$1" -h 2>&1 | grep -q -- '-geo-url'; then echo new; else echo legacy; fi
}

# ---- commands ----

cmd_up() {
    local daemon=${DAEMON:-} mode=${MODE:-} state path demo out state_name live
    local env_extra=() args=(-t "$GRPC" -v) i
    command -v jq > /dev/null || die "jq is required"
    command -v ss > /dev/null || die "ss (iproute2) is required"
    [ -n "$daemon" ] || die "set DAEMON=/path/to/starlinkd: the kit never picks a daemon for you"
    [ -x "$daemon" ] || die "DAEMON=$daemon is not an executable file"
    daemon=$(cd "$(dirname "$daemon")" && pwd)/$(basename "$daemon")
    case "$SCENARIO" in clean|busy|trouble) ;; *) die "SCENARIO must be clean, busy or trouble" ;; esac
    [ -n "${XDG_RUNTIME_DIR:-}" ] && [ -d "$XDG_RUNTIME_DIR" ] \
        || die "XDG_RUNTIME_DIR must be set: the daemon's panel socket and state-dir link live there"
    case "$RUN" in /*) ;; *) RUN=$PWD/$RUN ;; esac
    [ "$RUN" != / ] || die "RUN must not be /"
    case "$RUN" in "$BACKEND"|"$BACKEND"/*) die "RUN must be outside the repository" ;; esac
    # The run directory is wiped and reused, so it must be the kit's own: an
    # existing one counts only if it carries the stamp below.
    if [ -e "$RUN" ] && [ -n "$(ls -A "$RUN" 2> /dev/null)" ] && [ ! -e "$RUN/.dishy-demo" ]; then
        die "RUN=$RUN is not empty and was not created by this kit: nothing in it was touched"
    fi

    live=$(ack_live_or_sandbox)
    refuse_if_busy
    [ -z "$mode" ] && mode=$(detect_mode "$daemon")
    case "$mode" in new|legacy) ;; *) die "MODE must be new or legacy" ;; esac
    if [ "$mode" = legacy ] && [ "$live" = 1 ]; then
        die "MODE=legacy cannot take the live shell over: a daemon without -geo-url publishes no state-dir link. Use a newer daemon."
    fi

    mkdir -p "$RUN" "$RUN/emptybin"
    : > "$RUN/.dishy-demo"
    chmod 700 "$RUN"
    rm -rf "$RUN/state"
    rm -f "$RUN/daemon.log" "$RUN/dishdemo.log" "$RUN/restore-target"
    path=$PATH
    if [ "${NOTIFY:-}" != on ]; then path=$RUN/emptybin; fi # no notifier on PATH: demo transitions stay off the desktop

    if [ "$mode" = new ]; then
        state=$RUN/state
        env_extra+=("STARLINKD_STATE_DIR=$state")
        args+=(-geo-url "http://$GEO/json")
    else
        # Older daemons only know XDG_STATE_HOME (their state dir is then
        # $RUN/state/starlinkd) and always look up the egress on the public
        # internet: the proxy variables point that lookup at a closed local port.
        state=$RUN/state/starlinkd
        env_extra+=("XDG_STATE_HOME=$RUN/state" "HTTPS_PROXY=http://127.0.0.1:9" "HTTP_PROXY=http://127.0.0.1:9")
    fi

    demo=${DEMO:-$RUN/dishdemo}
    if [ ! -x "$demo" ]; then
        echo "building dishdemo in $BACKEND ..."
        (cd "$BACKEND" && go build -mod=readonly -o "$demo" ./demo/dishdemo) \
            || die "could not build dishdemo (backend/build.sh generates the gen/ it needs)"
    fi

    "$demo" -seed "$state" -scenario "$SCENARIO" -listen "$GRPC"
    "$demo" -listen "$GRPC" -geo-listen "$GEO" -scenario "$SCENARIO" -v >> "$RUN/dishdemo.log" 2>&1 < /dev/null &
    record_pid dishdemo $!
    for i in $(seq 1 50); do
        out=$(tcp_listeners "${GRPC##*:}")
        if alive dishdemo && [ -n "$out" ]; then break; fi
        sleep 0.1
    done
    out=$(tcp_listeners "${GRPC##*:}")
    alive dishdemo && [ -n "$out" ] || die "dishdemo did not start; see $RUN/dishdemo.log"

    env "${env_extra[@]}" PATH="$path" "$daemon" "${args[@]}" >> "$RUN/daemon.log" 2>&1 < /dev/null &
    record_pid daemon $!
    for i in $(seq 1 100); do
        if [ -f "$state/hist.json" ] \
            && state_name=$(jq -er 'select(.ok == true and now - .ts <= 5) | .state' "$state/status.json" 2> /dev/null); then
            break
        fi
        sleep 0.1
    done
    [ -n "$state_name" ] || die "no fresh status.json after 10 s; see $RUN/daemon.log"

    jq -r --arg s "$state_name" --slurpfile h "$state/hist.json" '
        "demo up (\($s)): \(.live.latencyMs) ms, map \($h[0].map // {} | "\(.rows // "?")x\(.cols // "?")"), today \(.usage.downGB) GB down / \(.usage.upGB) GB up"' \
        "$state/status.json"
    echo "scenario $SCENARIO, mode $mode, state $state"
    echo "logs $RUN/daemon.log and $RUN/dishdemo.log; demo.sh status | demo.sh down"
    local note=""
    if [ -L "$STATE_LINK" ]; then
        echo "state-dir link: $STATE_LINK -> $(readlink "$STATE_LINK")"
    else
        if [ "$mode" = legacy ]; then note=" (expected: a legacy daemon publishes none)"; fi
        echo "no state-dir link at $STATE_LINK: the live widget is not following this demo$note"
    fi
    if [ "$live" = 1 ]; then
        echo "$LIVE_RESTORE" > "$RUN/restore-target" # demo.sh down reminds with it
        echo "the widget's Dish address setting must be $GRPC for as long as this demo runs"
    fi
}

cmd_down() {
    local stuck=0
    stop_pid daemon || stuck=1
    stop_pid dishdemo || stuck=1
    [ "$stuck" = 0 ] || exit 1 # a kept pidfile lets a later run retry
    if [ -f "$RUN/restore-target" ]; then
        echo "Restore the Dishy widget's \"Dish address\" setting to: $(cat "$RUN/restore-target")"
        rm -f "$RUN/restore-target"
    fi
    echo "The real service is not restarted by this kit. When you are done, run: systemctl --user start starlinkd.service"
}

cmd_status() {
    local name state
    for name in dishdemo daemon; do
        if alive "$name"; then echo "$name: running (pid $(cut -d ' ' -f1 "$RUN/$name.pid"))"
        else echo "$name: not running"; fi
    done
    for state in "$RUN/state" "$RUN/state/starlinkd"; do
        if [ -f "$state/status.json" ]; then
            echo "status.json: $(jq -r '"\(.state), \(.live.latencyMs) ms, \(now - .ts | floor) s old"' "$state/status.json")"
            break
        fi
    done
    if [ -L "$STATE_LINK" ]; then echo "state-dir link: $STATE_LINK -> $(readlink "$STATE_LINK")"
    else echo "state-dir link: none at $STATE_LINK"; fi
    if [ -f "$RUN/restore-target" ]; then
        echo "Dish address setting to restore when done: $(cat "$RUN/restore-target")"
    fi
}

case "${1:-}" in
    up) cmd_up ;;
    down) cmd_down ;;
    status) cmd_status ;;
    *) echo "usage: DAEMON=/path/to/starlinkd demo.sh up | down | status (up needs LIVE_ACK=yes or SANDBOX=yes; see README.md)" >&2; exit 2 ;;
esac
