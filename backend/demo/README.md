# Dishy demo kit

A fake Starlink dish for developing and screenshotting Dishy on a machine that
is not on Starlink. `dishdemo` answers the gRPC requests `starlinkd` issues
(GetStatus, GetHistory, DishGetObstructionMap, DishStow, Reboot), serves a
local stand-in for the egress lookup, and can seed a state directory with a
day of history, so the widget shows a populated, moving dashboard from the
first second. Every number is a pure function of the wall clock, and every
value is invented.

**This is a development tool.** It is not installed, started or referenced by
`install.sh`, the service unit, or the plugin's QML, and it binds to loopback
only: it accepts numeric `127.0.0.1` / `::1` addresses and refuses anything
else before opening a socket.

## Files

- `dishdemo/` — the fake dish, a `package main` inside the `starlinkd` module
  (`go build ./demo/dishdemo` from `backend/`, once `gen/` exists).
- `demo.sh` — the runner: `up`, `status`, `down`.
- `README.md` — this file.

Nothing here writes into the repository. The runner's state, logs and pidfiles
live in one run directory, `$XDG_RUNTIME_DIR/dishy-demo` by default (`RUN=` to
change; an existing directory is only used if the kit created it).

## Run it

```sh
cd backend && bash build.sh            # once: generates gen/, which dishdemo imports
# LIVE_RESTORE is the Dish address the widget shows right now (the default is shown).
LIVE_ACK=yes LIVE_RESTORE=192.168.100.1:9200 DAEMON=$PWD/bin/starlinkd demo/demo.sh up
demo/demo.sh status
demo/demo.sh down
```

By hand, without the runner: `go run ./demo/dishdemo -scenario busy -v` serves
the fake dish, and `go run ./demo/dishdemo -seed <empty directory>` only
seeds one.

`up` builds `dishdemo` into the run directory if it is missing, seeds a
fresh sandbox state directory, starts the fake dish on `127.0.0.1:9210`
(egress answer on `127.0.0.1:9211/json`), starts the daemon against it, waits
for a fresh `status.json` and prints a one-line summary. `down` stops only the
two processes it recorded; once both are gone it prints the Dish address to
restore, if a live session left one, and the command that restarts the real
service without running it. A process that refuses to exit is named, keeps its
pidfile for a retry, and makes `down` exit non-zero. `status` says whether each
process is alive, how old `status.json` is, where the published state-dir link
points and, during a live session, the Dish address still to restore.

Variables:

| Variable | Meaning |
|---|---|
| `DAEMON` | The starlinkd binary to run. Required for `up`; the kit never picks one. |
| `DEMO` | An existing `dishdemo` binary. Default: built into the run directory when missing. |
| `SCENARIO` | `clean` (default), `busy` or `trouble`. |
| `MODE` | `new` or `legacy`; detected from the daemon's `-h` output when unset. |
| `RUN` | The run directory. Default `$XDG_RUNTIME_DIR/dishy-demo`; must be outside the repository. |
| `NOTIFY` | Default: the daemon runs with an empty `PATH` and finds no `notify-send`. `on` leaves your `PATH` for it. |
| `LIVE_ACK=yes` | "`up` may publish into the real runtime directory." Needs `LIVE_RESTORE`. |
| `LIVE_RESTORE` | The Dish address the widget shows right now, echoed in the notice and by `down`. |
| `SANDBOX=yes` | "`XDG_RUNTIME_DIR` is a sandbox" (not the real `/run/user/<uid>`, judged by directory identity: another name for that directory does not count): no live shell is involved, no notice. |
| `STATE_LINK` | Where a newer daemon publishes its state-dir link. Default `$XDG_RUNTIME_DIR/starlinkd`. |

`up` starts nothing while, among other things, any of these holds: no
acknowledgement, something already answering on
`$XDG_RUNTIME_DIR/starlinkd.sock` (the real daemon, or an earlier demo run —
matched as the socket file, whatever name its directory was reached
through), a demo port in use, no `DAEMON`, `RUN` inside the repository.

Daemon modes: a daemon that knows `-geo-url` (and with it the
`STARLINKD_STATE_DIR` override and the published state-dir link) runs in
`new` mode — state in `$RUN/state`, egress answered by the fake dish. An older
binary, such as the bundled v1.6.0, runs in `legacy` mode, for sandboxed
development only: its state lands in `$RUN/state/starlinkd` through
`XDG_STATE_HOME`, its egress lookup is pointed at a closed local port through
the proxy variables, and it publishes no state-dir link, so `up` refuses it
for the live shell. Development runs stay in a sandbox with

```sh
SANDBOX=yes XDG_RUNTIME_DIR=<a sandbox runtime directory> DAEMON=<binary> demo/demo.sh up
```

where the runtime directory must not be the real one, because a sandbox daemon
sharing it would reach the live daemon's socket.

## Scenarios

| Scenario | What the widget shows |
|---|---|
| `clean` (default) | ONLINE. Latency waves around 22–35 ms with occasional 1–2 s spikes to 60–90 ms, about 0.2 % dropped pings; downlink 40–150 Mbps in bursts, uplink 8–25 Mbps, 25–45 W; three dish-reported outages in the last 24 h; under 0.5 % obstruction. |
| `busy` | ONLINE with a sustained heavy download: 150–250 Mbps down, latency a little higher. |
| `trouble` | SEARCHING for 38 minutes: every ping dropped, alerts *Mast not near vertical*, *Install pending*, *Obstruction map reset*, a software update at 100 % waiting for a reboot, several recent outages. |

The obstruction map is the Mini's 123×123 grid: −1 outside the unit circle and
across a thin geostationary-exclusion band, a soft tree line of partial
clearance low on one side, clear sky elsewhere. Stow, Unstow and Reboot from
the panel are answered with success and logged; they do not change the
scenario. To switch, run `down` and `up` again with another `SCENARIO`.

The seed writes what a daemon that had been running all day would have by now:
today's minute-by-minute history (the daemon itself backfills the newest 15
minutes from the dish's ring), `daily.json`, `events.jsonl` for today's
outages, and a `config.json` whose target is the fake dish. It only ever
writes into an empty directory, with mode 0600 for files and 0700 for
directories.

## Privacy rule

- **Never real state.** The kit does not read or write the real
  `~/.local/state/starlinkd`; all of its state is synthetic and lives in the
  run directory.
- **Never a real IP or location.** The egress answer is an RFC 5737
  documentation address (`203.0.113.x`) and an invented town.
- **Never the real notifier.** Without `NOTIFY=on` the kit starts the daemon
  with an empty `PATH`, where it finds no `notify-send`, so demo transitions
  do not reach the desktop.
- **Never the real daemon.** `up` refuses while the real daemon is listening,
  stops only the pids it recorded, and never stops or restarts the real
  service.

## The live widget

Taking over the live widget — stopping the real service, pointing the shell at
the demo, restoring afterwards — is a separate, operator-run procedure that
the kit does not perform. One rule governs it: the Dishy widget's **Dish
address** setting is the source of truth for the dish target. The widget
pushes that setting to whichever daemon answers on the panel socket whenever
it differs from the `target` in the `config.json` of the state directory it
reads. That is why the seed writes `config.json` with the demo target, and why
the widget's setting must be exactly that value, `127.0.0.1:9210` — port
included — for as long as the demo runs. Left at its previous value, the
widget re-aims the demo daemon at the real dish within seconds and the demo
shows OFFLINE. The kit never changes the setting: on the real runtime
directory `up` stops unless `LIVE_ACK=yes` is given, prints both values (the
demo target, and the previous value you pass as `LIVE_RESTORE`) before
starting anything, and `down` repeats the value to restore. Change the
setting only while the real service is stopped — it is pushed to whichever
daemon answers, the real one included — i.e. stop the real service, run `up`,
set the address to the demo target, and afterwards run `down`, restore the
address and start the real service again. Until the real service starts and
republishes it, the state-dir link keeps pointing at the stopped demo state.
