# starlinkd ⇄ plugin contract (FROZEN)

Ground truth: live Starlink Mini, hw `mini1_panda_prod1`, fw `2026.09.08.mr86422`,
API v43, gRPC `192.168.100.1:9200`, service `SpaceX.API.Device.Device/Handle`.
Captures in `testdata/*.real.json` are the authoritative field reference.

The daemon writes THREE files, all atomic (tmp+rename), dir `~/.local/state/starlinkd/`:

## status.json — every 2s (~1.5KB). Bar widget + panel read this.

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
  "gps": { "valid": true, "sats": 22 }
}
```

- `state` ∈ ONLINE | SEARCHING | NO_SIGNAL | THERMAL_SHUTDOWN | BOOTING | OFFLINE | NO_DATA
  (no OBSTRUCTED state — this firmware reports no live obstruction flag)
- `stateDetail`: `SOFTWARE_UPDATE_REBOOT_PENDING` | disablementCode when ≠ OKAY | `""`
- `ts` staleness: consumers treat data older than 10s as NO_DATA.
- `ok:false`: gRPC failing; `error` carries the last message. `state: OFFLINE`.

## hist.json — every 2s (~25KB). Panel graphs read this.

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
unreachable stays quiet until it has seen ONLINE once. notify-send app-name:
"Dishy".

## CLI controls (dial-on-demand, then exit)

`starlinkd stow|unstow|reboot [-t 192.168.100.1:9200]` — exit 0 on success,
nonzero + stderr message on failure. Reboot = `reboot` request.

## Daemon flags

`starlinkd [-t 192.168.100.1:9200] [-interval 2s] [-map-interval 60s] [-once] [-v]`
`-once`: single tick, write files, print status.json to stdout, exit.
`-v`: log transitions + errors to stderr (journald under systemd).

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
