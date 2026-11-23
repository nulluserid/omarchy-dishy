# starlinkd ⇄ plugin contract (v4)

Ground truth: live Starlink Mini, hw `mini1_panda_prod1`, fw `2026.09.08.mr86422`,
API v43, gRPC `192.168.100.1:9200`, service `SpaceX.API.Device.Device/Handle`.
Captures in `testdata/*.real.json` are the authoritative field reference.

v2 (2026-10-23) amends v1.0.0: adaptive poll cadence (dashboard closed = 10 s
idle ticks, no hist writes), a `link` marker that hides the UI off-network, an
`geo` egress lookup (public IP + exit city), and a `panel open|close` control
socket. All additions are additive: consumers of the v1 fields work unchanged.

v3 (2026-10-27) amends v2: `day-YYYYMMDD.json` per-minute day history (30-day
rolling), `live.typicalDownMbps` (typical ACTIVE throughput), and
`starlinkd set host` — the dish target resolves as -t flag > config.json >
192.168.100.1:9200 (the service unit no longer pins -t; there is deliberately
no DNS fallback — a VPN or foreign network must not redirect dish traffic).
All additions are additive: v2 consumers work unchanged.

v4 (2026-10-30) amends v3: accumulated dish-derived state (daily.json,
day-*.json) lives in per-dish partitions, `dishes/<dish-id>/` under the state
dir, keyed by the identity-verified `device_info.id`. Joining another
Starlink's network (same 192.168.100.1, different dish) switches to that
dish's counters the moment it answers — and back, intact: usage, day history,
and session availability never blend across dishes. config.json gains
`lastDish` so restarts preload the right partition. Top-level daily.json /
day-*.json from ≤ v3 installs are legacy (pre-partition, potentially
mixed-dish) and are neither read nor migrated after the first identity is
seen; they are safe to delete. status.json, hist.json, events.jsonl and
config.json stay shared at the state root. New event type `dish_switch`
{from,to}. All additions are additive: v3 consumers work unchanged.

Retention (2026-10-30): a dish partition with no connection activity for
30 days is deleted whole (counters + day history) at the next startup or
local day roll. Connection evidence is daily.json's `lastSeen` (written
only by identity-verified good ticks) extended by the newest day-*.json
date; when no stamp exists (≤ v1.4 files, drive-bys) file mtime is used,
and `loadDaily` seeds the stamp from mtime so an adopted legacy dish ages
out from load time even though later writes rewrite the file. Kept
partitions roll their day files through the same 30-day window (see
day-*.json), and legacy root-level day files share it. No configuration;
the window is the day-history retention.

Amendment (2026-11-23, marketplace review of v1.6.0): the obstruction map is
bounded at the dish trust boundary — a coherent grid of at most 256x256
cells and 1 MiB per reply ("Security hardening") — because the shell
re-parses hist.json every 2 s while the dashboard is open. Strings the dish
and the egress provider send are clipped to rune caps, and the dish-reported
outage list is bounded at ingestion (same section). Every notification draws
from one rate bucket ("Notifications"); the control socket acts only on
newline-terminated lines and never acknowledges a dropped command ("Panel
control socket"). The daemon also publishes its resolved state directory as
the symlink `$XDG_RUNTIME_DIR/starlinkd` ("State directory"), so the QML
consumers follow a non-default `XDG_STATE_HOME`. status.json gains the
additive `dish.dir`. No other file shape changes.

The daemon writes these files, all atomic (tmp+rename), dir
`~/.local/state/starlinkd/` by default (see "State directory"; v4: daily.json
and day-*.json live per dish in `dishes/<dish-id>/` there):

## status.json — every tick: 2s with the dashboard open, 10s (idle) or 2s
(short probe burst) closed. (~1.5KB). Bar widget + panel read this.

```json
{
  "ts": 1760000000,
  "ok": true,
  "error": "",
  "dish": { "id": "ut31...", "hw": "mini1_panda_prod1", "sw": "2026.09.08.mr86422",
            "dir": "ut31...", "bootCount": 363, "uptimeS": 3310, "ethMbps": 1000 },
  "state": "ONLINE",
  "stateDetail": "",
  "live": { "latencyMs": 18.0, "downMbps": 14.2, "upMbps": 1.2, "powerW": 13.0,
            "typicalDownMbps": 11.4 },
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
- `dish.dir`: name of the per-dish partition actually being written (`dishes/<dir>/`), derived from the adopted `dish.id` — never from an id that is only pending in the identity debounce, and absent until an identity is known (the state root is written then). Consumers use it instead of re-deriving the name: the daemon's rule (lowercase, [a-z0-9._-], 64 bytes) and a re-implementation disagree on non-ASCII ids.
- `ts` staleness: consumers treat data older than 30s as NO_DATA (28s of
  headroom on the 10 s idle cadence; the bar timer runs at 10 s while hidden).
- `live.typicalDownMbps` (v3): last minute's typical ACTIVE downlink — samples
  ≥1 Mbps count as active, the fastest 10% of those are trimmed, and ≥4 active
  samples are required; null when idle or stale (>60 s since the newest
  sample). Same math as the day store (below), so the bar readout, panel
  stat, and Today chart agree. The bar's `typical` readout holds the last
  value for 5 minutes across brief idle stretches.
- `ok:false`: gRPC failing; `error` carries the last message. `state: OFFLINE`.
- `link` (v2): off-network UI marker. `onStarlink:false` ⇒ the machine is
  provably elsewhere: the QML bar widget hides (ModuleSlot → 0 width, layout
  position preserved), the panel closes, and daily counters freeze (see
  "Off-network accounting"). `reason` ∈ `dish` (GetStatus answered back as a
  Starlink dish — the response carries dish device info; a cable modem or
  router holding the target address that answers without it does not count) |
  `egress` (Starlink carrier on the
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
  unreachable or default route flipped; once a post-flip lookup is consumed,
  follow-ups return to the settled 5-min clock). No API key. Lookups run off
  the tick path, and results are stamped when the request is launched — the
  answer describes the network as of then, so a lookup in flight during a
  route change can never pass for a post-change answer.

## hist.json — ~60KB with a real dish's 123x123 map (map ~41KB, ring ~21KB),
## written ONLY on active (dashboard-open) ticks; the background cadence
## keeps the ring in memory and refreshes it every 60 s (also covering graph
## gaps across the idle stretch). Panel graphs read this. Conforming maximum
## ≈ 503 KB, measured as marshaled: map 65,536 cells × ≤ 6 chars = 393 KB,
## five 900-value ring arrays × ≤ 24 chars (value + separator) = 108 KB, and
## its ≤ 20 outages ≈ 2 KB. Consumers treat a hist.json larger than 768 KiB
## as invalid — headroom over that maximum.

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
           "snr": [0..1 per cell, -1 = no data] }   // row-major, refreshed every 60s;
                                                     // 1 ≤ rows,cols ≤ 256, exactly
                                                     // rows·cols cells; null only until
                                                     // the first accepted map
}
```

All five series arrays have exactly `n` entries, oldest→newest, 1s apart.

## daily.json — every tick. Persisted across restarts. Dish-scoped (v4):
## lives in `dishes/<dish-id>/` under the state dir, one per dish.

```json
{ "date": "2026-10-19", "downBytes": 1073741824, "upBytes": 52428800,
  "onlineS": 86400, "totalS": 86420, "outages": 1,
  "lastSeen": 1760000000,
  "allTime": { "downBytes": 8.72e11, "upBytes": 4.4e10 } }
```
On date change: roll counters into `allTime`, zero today's. `lastSeen`
(2026-10-30) is the unix s of the last identity-verified good tick for this
dish — the retention sweep's "connected" evidence (fail ticks never refresh
it, so an adopted-but-unreachable dish ages out despite its daily.json
being rewritten).

### Off-network accounting (v2)

Counters advance only on confirmed Starlink evidence (`link.reason` ∈
`dish`|`egress`; the grace hold counts nothing), using real elapsed
seconds per tick — capped at 30 s per tick, so suspend/resume hours are not
booked as dish downtime (timers freeze across sleep; the first tick after
waking books one capped tick). Failure ticks off-network add no time, open no
outages, send no notifications; a link flip off-network closes any open
outage with a silent `outage_end` event (cause "(left network)"). A machine
parked on another network does not pollute the dish's availability.

## day-YYYYMMDD.json (v3) — per-minute day history, 30-day rolling
## (v4: dish-scoped — lives in `dishes/<dish-id>/` under the state dir)

One file per local day, atomic writes, throttled to one per 30 s (a completed
minute forces a write; at most ~30 s of aggregates are lost to SIGKILL and
the watermark heals the overlap on restart). `minutes` is indexed by
minute-of-day, 0 = local midnight; a zero value = no data.

```json
{ "date": "2026-10-27", "lastTs": 1790000000,
  "minutes": [ { }, { }, { "downTyp": 11.4, "ping": 18.6, "up": 1.2,
                            "drop": 0.0008, "powerW": 13.4, "n": 60 } ] }
```

Per minute: `downTyp` — typical ACTIVE downlink, Mbps (samples ≥1 Mbps count
as active; fastest 10% trimmed; ≥10 active samples for a completed minute,
≥4 for the in-progress one), `ping` — median answered ping (ms), `up` — mean
active up (Mbps), `drop` — mean drop rate over all real samples, `powerW` —
mean input power, `n` — real samples (firmware phantoms excluded).

`lastTs` is the aggregation watermark (wall unix of the newest aggregated
sample): samples at or before it are skipped, so a restart's ring backfill
never double-counts. Sample wall times are derived from the ingest cursor —
the dish measures its ring even while this machine sleeps, so a wake's
backfill attributes those seconds correctly.

Off-network rule: identical to daily.json. The store is fed only from
identity-verified dish answers and only while `link.onStarlink`; parked
elsewhere, nothing is ingested (GetStatus fails or does not answer as a
dish), so nothing is written. Files older than 30 days are pruned by name.

Known limitations: a backfill that crosses local midnight (≤15 min) has a
tail belonging to the previous day's file; that tail is not backfilled into
it. A restart inside a minute completes that bucket from post-restart
samples only, so its `n` reflects the shorter window.

## events.jsonl — append on transitions; rotate >512KB → events.old.jsonl

```json
{"ts":1760000000,"type":"outage_start","cause":"BOOTING"}
{"ts":1760000060,"type":"outage_end","cause":"BOOTING","durS":60}
{"ts":1760000100,"type":"alert_set","id":"thermal_throttle","label":"Thermal throttle"}
{"ts":1760000200,"type":"obstruction_heavy","fraction":0.012}
{"ts":1760000300,"type":"state_change","from":"SEARCHING","to":"ONLINE"}
```
Types: outage_start, outage_end, alert_set, alert_cleared, obstruction_heavy,
state_change, dish_switch (v4: `{"type":"dish_switch","from":"<old dish id>",
"to":"<new dish id>"}` — emitted when a different dish answers; the event
itself is silent, but an open outage it closes ends with cause
`"<cause> (dish switched)"` and is announced as "Starlink online (dish
switched)" — the old dish never returned, yet its offline note was heard, so
the recovery names the dish that took over). `obstruction_heavy` hysteresis:
fire at fraction >0.01 sustained
60s; re-arm only after <0.005 for 300s.

## Notifications (notify-send, app-name "Dishy")

| event              | summary              | body                        | urgency |
|--------------------|----------------------|-----------------------------|---------|
| outage_start       | Starlink offline     | cause, humanized            | normal  |
| outage_end         | Starlink online      | "down Xm Ys"                | normal  |
| outage_end (switch)| Starlink online (dish switched) | "down Xm Ys — now on <hw or short id>" | normal |
| alert_set          | Starlink alert       | label                       | normal  |
| obstruction_heavy  | Starlink obstructed  | "X.X% obstruction, last 60s"| normal  |

Outage = ≥2 consecutive failed ticks or dish-reported outage beginning.
Notifications are warm-gated: a daemon that starts while the dish is already
unreachable stays quiet until it has seen ONLINE once — and (v2) they fire
only on CONFIRMED Starlink evidence (`link.reason` ∈ `dish`|`egress`), so a
grace hold or an evidence-free network (travel, captive portal before login)
never produces false "Starlink offline" pings. Egress-confirmed outages carry
one further gate: the geo answer must postdate the last default-route change
— a stale home answer describes the route the machine left, not the dish
failing on the route it is on. The gate arms either on a post-change geo
answer (lookups succeed) or on an identity-verified dish answer since the
change (`link.lastDishOK`), so a real at-home outage behind a transient
route change still notifies. notify-send app-name: "Dishy".

Rate (2026-11-23): every notification draws one token from a single bucket,
whatever fired it — capacity 5, one token back per 10 s on the monotonic
clock, full at daemon start. The dish decides which transitions fire (19
alert booleans can flip per tick; a good / fail / fail flap is an outage pair
every few ticks), so a gate per source never adds up to a bound. A
notification the bucket cannot pay for is dropped; its event is still
appended to events.jsonl.

## CLI controls (dial-on-demand, then exit)

`starlinkd stow|unstow|reboot [-t 192.168.100.1:9200]` — exit 0 on success,
nonzero + stderr message on failure. Reboot = `reboot` request.

`starlinkd set host <host[:port]>` (v3) — change the dish target. Validates
(default port 9200 appended when omitted; no whitespace/control chars; bare
IPv6 requires brackets), then pushes to the running daemon via the control
socket — live connection swap, config.json persisted. With no daemon
listening, config.json is written directly and used at the next start.
Target precedence for the daemon and `set host`: explicit `-t` flag >
config.json > 192.168.100.1:9200 (the one-shot `stow|unstow|reboot` controls
resolve config.json > default when `-t` is absent). Deliberately no DNS
fallback: a VPN or foreign network must not silently redirect dish traffic.

## State directory

The daemon resolves its state dir as `$XDG_STATE_HOME/starlinkd`, else
`~/.local/state/starlinkd` when `XDG_STATE_HOME` is unset. The QML consumers
see only the shell's environment, so at every daemon start — never for
`-once` or the CLI subcommands — the daemon publishes the directory it
resolved as the symlink `$XDG_RUNTIME_DIR/starlinkd` → that directory's
absolute path. The link is created under a temp name beside it and renamed
over the previous one, so consumers never see it missing or half-made. An
unset `XDG_RUNTIME_DIR` publishes nothing and is not an error; neither does
a runtime dir that is not this user's alone — missing, not owned by the
daemon's uid, or writable by group/others (publishing there would let another
account aim the consumers at a state dir of their choosing). That skip is
logged only in verbose mode, and the consumers fall back. A non-symlink
occupant of that path (a real directory or file) is never removed or
replaced: the daemon reports it and keeps running.

Consumers resolve the state dir as: `$XDG_RUNTIME_DIR/starlinkd` when the
link resolves, else `$XDG_STATE_HOME/starlinkd`, else
`~/.local/state/starlinkd`. The link is authoritative whenever it resolves —
it names the directory of the daemon's last start, so a user service
configured with a different state directory (e.g. an
`Environment=XDG_STATE_HOME=…` override on the unit) still feeds the widget
and panel.

## Panel control socket (v2)

`starlinkd panel open|close` — one line (`open\n` / `close\n`) over a Unix
socket at `$XDG_RUNTIME_DIR/starlinkd.sock` (0600, tmpfs; falls back to the
state dir). The dashboard QML sends `open` on show (immediate refresh + a
90 s fast-poll lease with a 60 s heartbeat) and `close` on hide (idle
immediately). (v3) the widget settings also send `set host <target>` through
the same socket; the listener validates the target and the tick loop applies
it (connection swap + config persist). The daemon never blocks on it; failure to connect just means
idle cadence. The lease covers a crashed shell: 90 s without a heartbeat and
the daemon drops back to idle by itself — no permanent fast polling. A 30 s
probe burst also runs once on the first failure after a good tick and on a
default-route change — one-shot, never while the failure persists.

A command counts only when a newline terminates it within 512 bytes,
however many writes it arrives in; anything else — a peer that closes or
stalls first, a line that overflows — is answered `bad` and never acted on.
The reply is `ok` only once the command is queued for the tick loop, and
`busy` when the queue is full and the command was dropped (`starlinkd set
host` reports that as rejected). The listener survives transient accept
failures (a descriptor-limit burst, an aborted client) and stops only with
the process.

## Daemon flags

`starlinkd [-t 192.168.100.1:9200] [-interval 2s] [-idle-interval 10s]
[-idle-hist-interval 60s] [-map-interval 60s] [-once] [-v]`
- `-interval`: poll cadence while the dashboard is open (default 2 s).
- `-idle-interval`: poll cadence while it is closed (default 10 s).
- `-idle-hist-interval`: background history refresh while closed (default 60 s).
- `-map-interval`: obstruction map refresh (dashboard open only).
- `-once`: single tick, write files, print status.json to stdout, exit.
- `-v`: log transitions + errors to stderr (journald under systemd).

## Security hardening (2026-10-31)

Response-trust hardening for the unauthenticated dish channel: GetHistory is
ingested only when **all four** ring arrays are exactly ringSize (short or
hostile responses are rejected whole); non-finite (NaN/Inf) samples are
skipped and `round()` neutralizes non-finite floats so no state file can
carry them (a marshal failure now keeps the last good file instead of writing
an empty object); obstruction maps (2026-11-23) are accepted only as a
coherent bounded grid — 1 ≤ rows,cols ≤ 256, exactly rows·cols delivered
cells, every raw cell finite and, once rounded, in [-1, 1] — and rejected
whole otherwise, keeping the last accepted map in place while still
advancing the refresh clock, as a reply that fails or exceeds its 1 MiB
per-call cap does (ObsMap.qml additionally refuses incoherent grids);
dish-reported outage starts are accepted only within the freshness window
(never future-dated) and announcements are rate-limited to one per 60 s,
with every notification drawing from one shared bucket ("Notifications");
dish-identity switches are debounced to 2 consecutive identical answers
(first sighting adopts immediately) and the partition set is capped at 64
with least-recently-active eviction; the control socket acts only on a
newline-terminated line of ≤ 512 bytes, acknowledges only a command it
queued, and keeps accepting after transient failures; state files are mode
0600 and state directories 0700; history-array values outside int range
clamp to a full
ring re-read; the systemd user unit sets NoNewPrivileges/ProtectSystem/
PrivateTmp/RestrictAddressFamilies/MemoryMax.

Strings the peers send are clipped, never copied whole (2026-11-23): the bar
re-parses status.json every 2 s even with the panel closed, and the dish's
device id / hardware / software strings, the RPC error text, and every egress
field are the peers' to size. Each is cut to a rune cap — `dish.id`, `hw`
and `sw` 64 (real values: 26, 17, 18); the RPC error, the whole
"code: message", 200, in `error` and in the control subcommands' stderr,
which the panel displays; `geo.ip` 64, `geo.city`/`region`/`org` 128,
`geo.country` 16, `geo.asn` 32 — after dropping control and format
characters (Unicode Cc, Cf: bidi overrides, zero-width marks) and invalid
UTF-8, never splitting a rune. Clipping truncates, it does not reject: the
surrounding answer stands. The clipped id is the only id the daemon ever
uses — the identity comparison, `dish.id`, config.json
`lastDish` (written and read), events.jsonl `dish_switch`, the partition key
and the notification text all derive from one conversion — so two ids that
differ only past the cap are one dish. Dish-reported outages are bounded at
ingestion to the newest 100 by start time, whatever order the dish delivered
them; what hist.json re-sorts per active tick never exceeds that.

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
- softwareUpdateState ∈ IDLE | FETCHING | PRE_CHECK | WRITING | POST_CHECK |
  REBOOT_REQUIRED | DISABLED | FAULTED (proto enum, "SOFTWARE_UPDATE_STATE_"
  prefix trimmed);
  progressPct from softwareUpdateProgress (0..1 fraction; multiply by 100
  for display).
- gpsStats: gpsValid, gpsSats.
- status real JSON uses camelCase grpcurl names; Go struct fields are canonical.

## Firmware note (2026.09.24.mr87636.5, observed 2026-10-19)

After the dish updated, the history ring alternates real samples with zeroed
phantoms (dropRate 0, latency 0 — impossible for a real ping). The daemon
forward-fills phantoms from the previous real sample (graphs, live powerW);
phantoms contribute no usage bytes.
