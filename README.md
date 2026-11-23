# Dishy

Starlink status for the [Omarchy](https://omarchy.org) shell: a bar widget with
your live latency (or throughput), a colored health dot, and a rich dashboard
popup — latency/throughput/power graphs, outage log, obstruction view, data
usage, availability, dish alerts, and stow/unstow/reboot controls.

![preview](preview.png)

```
omarchy plugin add https://github.com/nulluserid/omarchy-dishy.git --enable
```

Then run `./install.sh` once from
`~/.config/omarchy/plugins/io.github.nulluserid.dishy/` to install the `starlinkd`
backend service. The bar widget shows live data within ~2 seconds.

After a plugin update (`omarchy plugin update`), run `./install.sh`, then
restart the shell. The installer restarts the daemon with the new binary and
the shell caches QML until restart — and the daemon now exits after 15
minutes without a heartbeat from the widget: until the shell restarts, the
old widget sends none, so the daemon would stop and the widget show stale
data.

## What it shows

- **Bar:** satellite glyph + health dot (green online / amber searching or
  attention / red offline / gray no data) and live latency, down throughput,
  typical active throughput, or icon only (click the widget's settings).
  Amber covers every attention-worthy dish alert (stuck motors, water,
  slow ethernet, … — heating and power-save stay green), a pending
  update reboot, and any disablement code; the tooltip lists all active
  alerts plus the software-update state, so nothing the dish reports is
  easy to miss.
- **Auto-hide off-network (1.1):** when the machine is not on a Starlink
  network, the widget hides itself — the dish's exact gRPC target no longer
  answers *as a Starlink dish*, and your traffic provably exits through
  another carrier — while `starlinkd` keeps checking in the
  background (10 s ticks). Back on Starlink, the widget reappears by itself;
  no shell restart, no service restart. During connection drops, captive
  portals, or missing evidence the widget keeps showing for 10 minutes
  (grace) first. Turn it off with the widget's *Hide when not on Starlink*
  setting.
- **Bar placement (1.4):** Dishy sits in any bar section and moves like any
  Omarchy widget — `omarchy bar move io.github.nulluserid.dishy --section
  center` (or `right` / `left`); the dashboard follows the button.
- **Lives with the widget (1.7):** the backend is tied to the widget. The
  widget keeps `starlinkd` alive with a heartbeat; disable or remove the
  plugin and its daemon exits by itself within 15 minutes. The next shell
  start brings it back.
- **Panel** (click the widget; dense single-view grid, no scrolling):
  - state chip, dish uptime + short id, hardware/software versions, GPS +
    ethernet link
  - latency graph (15 min, 1 s resolution, p50/p95 guides) beside
    throughput (down fill / up line, typical-active readout) — drop strip
    and 15-min drop stats under the latency plot
  - Today chart (per-minute typical active throughput, 30-day rolling
    history in `<state dir>/dishes/<dish-id>/day-YYYYMMDD.json`; the state
    dir is `$XDG_STATE_HOME/starlinkd`, else `~/.local/state/starlinkd`)
    beside power draw
  - recent outages with cause and duration (dish-reported, latest three)
  - obstruction view (sky-coverage map + stats) beside the egress lookup:
    public IP + approximate exit city/region and carrier — one ifconfig.co
    query every 5 minutes, off Starlink too; after a failed lookup nothing
    is retried for 5 minutes; no API key. The IP check always uses IPv4, so
    the address and exit location it shows are the IPv4 ones; on an
    IPv6-only network there is no egress line.
  - one-line usage grid: today / all-time / availability
  - active dish alert chips + software-update state
  - Stow / Unstow / Reboot — each behind an explicit click-to-confirm
- **Multiple dishes (1.3):** stats are per dish. Move the machine between
  Starlinks (or change the dish address) and each dish keeps its own usage,
  availability, and day history in `<state dir>/dishes/<dish-id>/` (the
  state dir is resolved as under "How it works") — alternating between a
  couple of dishes in a week never blends their numbers. A dish you have
  not used before simply starts at zero. Housekeeping (1.5): a partition
  with no connection for 30 days is deleted whole at the next startup or
  day roll, and every partition rolls its day files through the same
  30-day retention as the active one. Old ≤ v1.2 top-level
  `daily.json`/`day-*.json` files are legacy (and may mix dishes); they
  share the 30-day retention and are safe to delete.
- **Notifications:** desktop notification when an outage starts and ends —
  including an outage that ends because a *different* dish took over, which
  announces "Starlink online (dish switched)" naming the new dish — when
  a dish alert sets, and on sustained heavy obstruction. Notifications fire on
  transitions; a dish that is already unreachable when the daemon starts stays
  quiet on purpose — and (1.1) they never fire for a machine parked on another
  network: off-network time is not counted as dish downtime. Rate-limited
  (burst 5, then one per 10 s); a refused `notify-send` is reported once in
  the journal at startup and disables notifications for that run.

## Requirements

- A Starlink dish reachable at `192.168.100.1` (default; any router setup that
  lets the Starlink app work also lets Dishy work)
- Omarchy's shell (Quickshell) — this is a `bar-widget` plugin
- `systemd` user session for the backend service
- `notify-send` for notifications (optional; already part of Omarchy)

`backend/bin/starlinkd` is a committed static Go binary (no runtime
dependencies, deterministically built with `CGO_ENABLED=0 -trimpath
-buildvcs=false` and version-stamped to the release tag — verify with
`go version -m backend/bin/starlinkd` and `backend/bin/starlinkd -version`).
To rebuild it yourself at a tag: `backend/build.sh` needs the exact Go version
in `backend/go.mod`, `jq`, `protoc`, `protoc-gen-go v1.36.12`, and
`protoc-gen-go-grpc v1.6.2`; the protobuf descriptors are captured from the dish itself via
gRPC reflection (`backend/proto/dish.pb`) — nothing hand-maintained.
The build refuses a different compiler or a version stamp that disagrees with
the manifest. Source and bundled-runtime vulnerability scans, tests, and a
byte-identical rebuild are required before release (see [RELEASING.md](RELEASING.md)).

## Install

1. `omarchy plugin add https://github.com/nulluserid/omarchy-dishy.git --enable`
2. `cd ~/.config/omarchy/plugins/io.github.nulluserid.dishy && ./install.sh`
   (installs + enables the `starlinkd` systemd user service)

The installer refuses other checkouts: the service always runs the canonical
plugin path above, so running it in a development clone must not silently
restart a different binary.

The installer trusts no caller `PATH`: it runs root-owned tools from a
closed `PATH`, refusing if one is missing; it refuses to write when a
directory up to `$HOME` is writable by group/others or owned by someone else
(the message names the fix); and it backs up an existing unit before
replacing it. `SYSTEMCTL` overrides the systemctl path (used by the hermetic
test).

Optional keybind — toggle the panel with `omarchy-shell dishy toggle`.

The dish address is a widget setting (default `192.168.100.1:9200`; changing
it is pushed to `starlinkd` live, no restart). There is deliberately no DNS
fallback: on a VPN or foreign network, a hostname must not silently redirect
dish traffic.

Upgrading from ≤ 1.1.1: run `./install.sh` once — the service unit no longer
pins the dish target, which is what makes the setting (and `starlinkd set
host`) effective.

Upgrading from ≤ 1.6.0: `omarchy plugin update --yes`, run `./install.sh`
once (it installs the updated service unit: more sandboxing, and the daemon
now exits when the widget is gone), then restart the shell
(`omarchy-restart-shell`). Until the shell restarts the old widget sends no
heartbeat, so the daemon exits after 15 minutes and the readout goes stale —
the restart brings both back.

## Remove

The daemon also exits by itself within 15 minutes of the widget being
disabled or removed; the manual steps below still remove the unit and state.

```bash
systemctl --user disable --now starlinkd.service
rm ~/.config/systemd/user/starlinkd.service
systemctl --user daemon-reload
omarchy plugin remove io.github.nulluserid.dishy   # or delete the plugin directory
rm -rf "${XDG_STATE_HOME:-$HOME/.local/state}"/starlinkd  # optional: usage/outage history
```

## Marketplace

Dishy's Omarchy plugin marketplace entry: [`io.github.nulluserid.dishy`](https://plugins.omarchy.org).
A listing is pinned to a validated commit — not a tag: once listed, changes to this repo
reach users via the marketplace's
[verification flow](https://github.com/omacom/omarchy-plugin-marketplace/blob/main/VERIFICATION.md).
Keep `main` shippable and tag releases as usual. The full release + deploy +
marketplace-update pipeline lives in [RELEASING.md](RELEASING.md).

## How it works

Two small pieces with one tiny file contract:

```
starlinkd (Go daemon, one static binary)          Dishy plugin (QML)
  gRPC 192.168.100.1:9200 — GetStatus +            bar widget + dashboard,
  GetHistory cursor-delta over the dish's          reads the state files,
  900-sample ring, derives stats once,             never talks to the dish
  writes atomic JSON  ──────────────────────▶      (FileView + JSON.parse)
```

The poll adapts to how it is actually used (v2):

| Situation                        | Status writes | History ingest | hist.json | Obstruction map |
|----------------------------------|--------------:|---------------:|----------:|----------------:|
| Dashboard open                   | every 2 s     | every 2 s      | every 2 s | every 60 s      |
| Dashboard closed                 | every 10 s    | every 60 s     | never     | never           |
| First failure / route change     | 2 s for 30 s  | 2 s            | —         | —               |

Opening the dashboard tells `starlinkd` to refresh immediately and poll fast
(`starlinkd panel open` over a small Unix socket); closing it returns to idle
at once. Off-network detection is evidence-based — the exact dish gRPC target
answering back *as a Starlink dish* (dish device info present; a cable modem
squatting on that address does not count) and the egress carrier (`AS14593`
via `ifconfig.co`; the dish LAN subnet is deliberately not a signal — router
mode LANs use 192.168.5.x/10.0.0.x while all of them route the dish address)
— with a 10-minute grace when there is no evidence at all
(captive portal, dish outage). The egress lookup asks `ifconfig.co` every 5
minutes — off Starlink too — and after a failed lookup nothing is retried
for 5 minutes. The IP check always uses IPv4, so the address and exit
location it shows are the IPv4 ones; on an IPv6-only network there is no
egress line. A parked machine costs ~1 KB of status JSON every 10 s and one
`/proc/net/route` read per tick: no per-tick process spawns, no polling
threads, no hist writes, no notification spam.

State lives in the daemon's state dir — `$XDG_STATE_HOME/starlinkd`, or
`~/.local/state/starlinkd/` when that variable is unset: `status.json`
(scalars + `link`/`geo`), `hist.json` (15-min time-ordered series +
obstruction map, written only while the dashboard is open), `day-YYYYMMDD.json`
(per-minute typical/ping/up/drop/power aggregates, 30-day rolling),
`daily.json` (today + all-time usage/availability), `events.jsonl`
(transition log). Since v1.3 the usage/day-history files live per dish under
`dishes/<dish-id>/` (see above). The daemon publishes the directory it
resolved as the symlink `$XDG_RUNTIME_DIR/starlinkd`; the widget and panel
read through that link first and follow the fresher of it and the default
path, so a service configured with a different state directory still feeds
the UI. `STARLINKD_STATE_DIR` overrides the state directory for every caller
(the widget passes it to its one-shot commands; the demo harness uses it).
The full contract is documented in
[`backend/CONTRACT.md`](backend/CONTRACT.md).

Dish controls and the poller CLI: `backend/bin/starlinkd stow|unstow|reboot`;
the dashboard also drives cadence via `backend/bin/starlinkd panel open|close`
and keeps the daemon alive with `panel alive`.

## Demo kit

`backend/demo/` is a development tool for working on Dishy off Starlink: a
loopback-only fake dish with scenarios (`clean`, `busy`, `trouble`), a seeded
sandbox state dir, and a local egress answer the daemon reads through
`-geo-url`. It is not installed, started or referenced by `install.sh`, the
service unit or the plugin's QML.

- **Privacy rule:** never real state, a real IP or location, the real
  notifier or the real daemon. Everything it shows is invented and lives in
  its own run directory.
- **Live-widget rule:** a live-shell demo needs the widget's *Dish address*
  setting to point at the demo target (`127.0.0.1:9210`) for the session.
  You set that setting and restore it afterwards — the kit never edits your
  shell settings.

See [`backend/demo/README.md`](backend/demo/README.md).

## License

Dishy's own code is MIT-licensed — see [LICENSE](LICENSE). The bundled
`starlinkd` executable also includes third-party components under their own
terms; their licenses, copyright notices, and patent grants are included in
[`backend/THIRD_PARTY_NOTICES.txt`](backend/THIRD_PARTY_NOTICES.txt).
