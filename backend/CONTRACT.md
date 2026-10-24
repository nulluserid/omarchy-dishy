# starlinkd ⇄ plugin contract (v2)

Ground truth: live Starlink Mini, hw `mini1_panda_prod1`, fw `2026.09.08.mr86422`,
API v43, gRPC `192.168.100.1:9200`, service `SpaceX.API.Device.Device/Handle`.
Captures in `testdata/*.real.json` are the authoritative field reference.

v2 (2026-10-23) amends v1.0.0: adaptive poll cadence (dashboard closed = 10 s
idle ticks, no hist writes), a `link` marker that hides the UI off-network, an
`geo` egress lookup (public IP + exit city), and a `panel open|close` control
socket. All additions are additive: consumers of the v1 fields work unchanged.

The daemon writes THREE files, all atomic (tmp+rename), dir `~/.local/state/starlinkd/`:

## status.json — every tick: 2s with the dashboard open, 10s (idle) or 2s
(short probe burst) closed. (~1.5KB). Bar widget + panel read this.

```json
{
  "ts": 1760000000,
  "ok": true,
  "error": "",
  "dish": { "id": "ut31...", "hw": "mini1_panda_prod1", "sw": "2026.09.08.mr86422",
            "bootCount": 363, "uptimeS": 3310, "ethMbps": 1000 },
  "state": "ONLINE",
  "stateDetail": "",
  "live": { "latencyMs": 18.0, "downMbps": 14.2, "upMbps": 1.2, "powerW": 13.0 },
  "lat5m": { "p50": 18.5, "p95": 24.0, "mean": 19.1 },
  "drop15m": { "ratio": 0.0021, "totalOutageS": 112, "count15m": 2 },
  "usage": { "downGB": 12.34, "upGB": 0.56, "allTimeDownGB": 812.4, "allTimeUpGB": 41.0 },
  "avail": { "todayPct": 99.98, "sessionPct": 100.0 },
  "obs":  { "fraction": 0.00213, "timeObstructedS": 12, "validS": 3177 },
  "alerts": [ { "id": "install_pending", "label": "Install pending" } ],
  "update": { "state": "REBOOT_REQUIRED", "progressPct": 1.0 },
  "gps": { "valid": true, "sats": 22 },
  "link": { "onStarlink": true, "reason": "dish", "since": 1760000000,
            "lastDishOK": 1760000000 },
  "geo":  { "ts": 1759999000, "ip": "100.67.10.4", "city": "Fremont",
            "region": "California", "country": "US", "asn": "AS14593",
            "org": "Starlink", "isStarlink": true }
}
```

- `state` ∈ ONLINE | SEARCHING | NO_SIGNAL | THERMAL_SHUTDOWN | BOOTING | OFFLINE | NO_DATA
  (no OBSTRUCTED state — this firmware reports no live obstruction flag)
- `stateDetail`: `SOFTWARE_UPDATE_REBOOT_PENDING` | disablementCode when ≠ OKAY | `""`
- `ts` staleness: consumers treat data older than 30s as NO_DATA (28s of
  headroom on the 10 s idle cadence; the bar timer runs at 10 s while hidden).
- `ok:false`: gRPC failing; `error` carries the last message. `state: OFFLINE`.
- `link` (v2): off-network UI marker. `onStarlink:false` ⇒ the machine is
  provably elsewhere: the QML bar widget hides (ModuleSlot → 0 width, layout
  position preserved), the panel closes, and daily counters freeze (see
  "Off-network accounting"). `reason` ∈ `dish` (GetStatus answered) | `lan`
  (local address inside the target's /24) | `egress` (Starlink carrier on the
  exit path) | `egress-other` (fresh geo with a non-Starlink carrier) | `grace`
  (no evidence at all: hold the last belief for 10 min from the last evidence,
  then hide; outage notifications and downtime booking require a confirmed
  reason, never the grace hold). `since` = unix s of the last `onStarlink`
  flip; `lastDishOK` = unix s of the last successful GetStatus. Missing
  `link` block (v1 daemon): consumers must default to visible.
- `geo` (v2, omitted until the first lookup): egress info from
  `https://ifconfig.co/json` — public IP, approximate exit city/region/country,
  carrier (`asn`, `asn_org`), and `isStarlink` (AS14593 or SpaceX/Starlink org
  string). Refresh: 5 min settled on-network (with the dashboard open, forced
  on open + every heartbeat whenever the answer is over 60 s old — so at most
  ≈1/min, the provider's courtesy limit), 1 min while classifying (dish
  unreachable or default route flipped). No API key. Lookups run off the tick
  path.

## hist.json — 25KB, written ONLY on active (dashboard-open) ticks; the
background cadence keeps the ring in memory and refreshes it every 60 s (also
covering graph gaps across the idle stretch). Panel graphs read this.

```json
{
  "ts": 1760000000,
  "n": 900, "intervalS": 1,
  "latencyMs":  [18.1, null, 18.3],   // newest LAST; null = dropped ping (sample dropRate==1)
  "downMbps":   [14.2, 13.9, 15.0],
  "upMbps":     [1.1, 0.9, 1.3],
  "powerW":     [13.0, 16.8, 12.9],
  "dropRate":   [0, 0, 1],
  "outages": [ { "cause": "BOOTING", "start": 1476462774, "durS": 112 } ],  // newest first, max 20
  "map": { "rows": 123, "cols": 123, "maxThetaDeg": 90.0,
           "snr": [0..1 per cell, -1 = no data] }   // row-major, refreshed every 60s
}
```

All five series arrays have exactly `n` entries, oldest→newest, 1s apart.

## daily.json — every tick. Persisted across restarts.

```json
{ "date": "2026-10-19", "downBytes": 1073741824, "upBytes": 52428800,
  "onlineS": 86400, "totalS": 86420, "outages": 1,
  "allTime": { "downBytes": 8.72e11, "upBytes": 4.4e10 } }
```
On date change: roll counters into `allTime`, zero today's.

### Off-network accounting (v2)

Counters advance only on confirmed Starlink evidence (`link.reason` ∈
`dish`|`lan`|`egress`; the grace hold counts nothing), using real elapsed
seconds per tick — capped at 30 s per tick, so suspend/resume hours are not
booked as dish downtime (timers freeze across sleep; the first tick after
waking books one capped tick). Failure ticks off-network add no time, open no
outages, send no notifications; a link flip off-network closes any open
outage with a silent `outage_end` event (cause "(left network)"). A machine
parked on another network does not pollute the dish's availability.

## events.jsonl — append on transitions; rotate >512KB → events.old.jsonl

```json
{"ts":1760000000,"type":"outage_start","cause":"BOOTING"}
{"ts":1760000060,"type":"outage_end","cause":"BOOTING","durS":60}
{"ts":1760000100,"type":"alert_set","id":"thermal_throttle","label":"Thermal throttle"}
{"ts":1760000200,"type":"obstruction_heavy","fraction":0.012}
{"ts":1760000300,"type":"state_change","from":"SEARCHING","to":"ONLINE"}
```
Types: outage_start, outage_end, alert_set, alert_cleared, obstruction_heavy,
state_change. `obstruction_heavy` hysteresis: fire at fraction >0.01 sustained
60s; re-arm only after <0.005 for 300s.

## Notifications (notify-send, app-name "Dishy")

| event              | summary              | body                        | urgency |
|--------------------|----------------------|-----------------------------|---------|
| outage_start       | Starlink offline     | cause, humanized            | normal  |
| outage_end         | Starlink online      | "down Xm Ys"                | normal  |
| alert_set          | Starlink alert       | label                       | normal  |
| obstruction_heavy  | Starlink obstructed  | "X.X% obstruction, last 60s"| normal  |

Outage = ≥2 consecutive failed ticks or dish-reported outage beginning.
Notifications are warm-gated: a daemon that starts while the dish is already
unreachable stays quiet until it has seen ONLINE once — and (v2) they fire
only on CONFIRMED Starlink evidence (`link.reason` ∈ `dish`|`lan`|`egress`),
so a grace hold or an evidence-free network (travel, captive portal before
login) never produces false "Starlink offline" pings. notify-send app-name:
"Dishy".

## CLI controls (dial-on-demand, then exit)

`starlinkd stow|unstow|reboot [-t 192.168.100.1:9200]` — exit 0 on success,
nonzero + stderr message on failure. Reboot = `reboot` request.

## Panel control socket (v2)

`starlinkd panel open|close` — one line (`open\n` / `close\n`) over a Unix
socket at `$XDG_RUNTIME_DIR/starlinkd.sock` (0600, tmpfs; falls back to the
state dir). The dashboard QML sends `open` on show (immediate refresh + a
90 s fast-poll lease with a 60 s heartbeat) and `close` on hide (idle
immediately). The daemon never blocks on it; failure to connect just means
idle cadence. The lease covers a crashed shell: 90 s without a heartbeat and
the daemon drops back to idle by itself — no permanent fast polling. A 30 s
probe burst also runs once on the first failure after a good tick and on a
default-route change — one-shot, never while the failure persists.

## Daemon flags

`starlinkd [-t 192.168.100.1:9200] [-interval 2s] [-idle-interval 10s]
[-idle-hist-interval 60s] [-map-interval 60s] [-once] [-v]`
- `-interval`: poll cadence while the dashboard is open (default 2 s).
- `-idle-interval`: poll cadence while it is closed (default 10 s).
- `-idle-hist-interval`: background history refresh while closed (default 60 s).
- `-map-interval`: obstruction map refresh (dashboard open only).
- `-once`: single tick, write files, print status.json to stdout, exit.
- `-v`: log transitions + errors to stderr (journald under systemd).

## Field mapping ground truth (from testdata)

- History cursor `current` (uint64): index of newest sample; ring of 900.
  Delta = (cur - prev + 900*2) % 900 samples; on backwards jump → dish
  rebooted: clear arrays, re-derive.
- `popPingDropRate` per-sample 0|1. latency for dropped samples → null.
- `downlinkThroughputBps`/`uplinkThroughputBps` → ÷1e6 for Mbps.
- `powerIn` watts (Mini-specific; may be absent on other hw → omit powerW arrays
  keys? NO — always emit, fill nulls if RPC lacks them).
- `outages[].startTimestampNs` string ns → unix seconds (÷1e9), `durationNs` → s.
- `obstructionStats`: fractionObstructed, timeObstructed (s), validS, patchesValid.
- `alerts` (DishAlerts): ~19 named booleans; emit every TRUE field.
  id = snake_case of proto name, label = Title Case.
- softwareUpdateState ∈ IDLE | CHECKING | DOWNLOADING | WRITING | REBOOT_REQUIRED | DISABLED;
  progressPct from softwareUpdateProgress (0..1).
- gpsStats: gpsValid, gpsSats.
- status real JSON uses camelCase grpcurl names; Go struct fields are canonical.

## Firmware note (2026.09.24.mr87636.5, observed 2026-10-19)

After the dish updated, the history ring alternates real samples with zeroed
phantoms (dropRate 0, latency 0 — impossible for a real ping). The daemon
forward-fills phantoms from the previous real sample (graphs, live powerW);
phantoms contribute no usage bytes.
