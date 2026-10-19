# Dishy

Starlink status for the [Omarchy](https://omarchy.org) shell: a bar widget with
your live latency (or throughput), a colored health dot, and a rich dashboard
popup — latency/throughput/power graphs, outage log, obstruction view, data
usage, availability, dish alerts, and stow/unstow/reboot controls.

![preview](preview.png)

```
omarchy plugin add https://github.com/nulluserid/omarchy-dishy.git --enable
```

Then run `./install.sh` once (from this directory) to install the `starlinkd`
backend service. The bar widget shows live data within ~2 seconds.

## What it shows

- **Bar:** satellite glyph + health dot (green online / amber searching or
  attention / red offline / gray no data) and live latency, down throughput, or
  icon only (click the widget's settings). Tooltip carries the full one-line
  summary.
- **Panel** (click the widget):
  - state chip, dish uptime, hardware/software versions, GPS + ethernet link
  - latency graph (15 min, 1 s resolution) with p50/p95 guides
  - down/up throughput and power draw graphs, per-sample drop strip
  - recent outages with cause and duration (dish-reported)
  - obstruction view (the dish's sky-coverage map) + obstructed fraction
  - data used today/all-time, availability today/session
  - active dish alert chips and software-update state
  - Stow / Unstow / Reboot — each behind an explicit click-to-confirm
- **Notifications:** desktop notification when an outage starts and ends, when
  a dish alert sets, and on sustained heavy obstruction. Notifications fire on
  transitions; a dish that is already unreachable when the daemon starts stays
  quiet on purpose.

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
To rebuild it yourself at tag `v1.0.0`: `backend/build.sh` needs `go` (≥ 1.27, matching backend/go.mod)
and `protoc`; the protobuf descriptors are captured from the dish itself via
gRPC reflection (`backend/proto/dish.pb`) — nothing hand-maintained.

## Install

1. `omarchy plugin add https://github.com/nulluserid/omarchy-dishy.git --enable`
2. From the plugin directory: `./install.sh`
   (installs + enables the `starlinkd` systemd user service)

Optional keybind — toggle the panel with `omarchy-shell dishy toggle`.

## Remove

```bash
systemctl --user disable --now starlinkd.service
rm ~/.config/systemd/user/starlinkd.service
systemctl --user daemon-reload
omarchy plugin remove io.github.nulluserid.dishy   # or delete the plugin directory
rm -rf ~/.local/state/starlinkd                   # optional: usage/outage history
```

## How it works

Two small pieces with one tiny file contract:

```
starlinkd (Go daemon, one static binary)          Dishy plugin (QML)
  gRPC 192.168.100.1:9200 — GetStatus +            bar widget + dashboard,
  GetHistory cursor-delta over the dish's          reads the state files,
  900-sample ring, derives stats once,             never talks to the dish
  writes atomic JSON every 2 s  ──────────────▶    (FileView + JSON.parse)
```

State lives in `~/.local/state/starlinkd/`: `status.json` (scalars, every 2 s),
`hist.json` (15-min time-ordered series + obstruction map), `daily.json`
(today + all-time usage/availability), `events.jsonl` (transition log). The
full contract is documented in [`backend/CONTRACT.md`](backend/CONTRACT.md).

Dish controls and the poller CLI: `backend/bin/starlinkd stow|unstow|reboot`.

## License

MIT — see [LICENSE](LICENSE).
