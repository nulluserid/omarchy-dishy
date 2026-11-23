import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
// The palette is reached as Commons.Color: Qt 6.12 ships its own `Color`
// type, which the bare name resolves to.
import qs.Commons as Commons
import qs.Ui
import "Model.js" as Model

// Bar readout (state dot + latency/down/icon) and host for the dashboard panel.
// Reads ONLY the daemon's status.json (CONTRACT.md), from whichever of its two
// possible state dirs answers: the $XDG_RUNTIME_DIR/starlinkd symlink it
// publishes at start, or $XDG_STATE_HOME/starlinkd (default
// ~/.local/state/starlinkd). Decided per tick (Model.pickSource, in tick()).
// It also holds the daemon up: the service's starlinkd exits after 15 minutes
// without a panel command or an `alive` ping, and this widget sends one at
// start and every 5 minutes (see "daemon lifetime"), so the backend lives and
// dies with the plugin.
BarWidget {
  id: root
  moduleName: "io.github.nulluserid.dishy"

  readonly property string glyph: "\udb86\udc80"  // nf-md-satellite_uplink

  // The daemon honors XDG_STATE_HOME: a service on another state dir updates
  // fine while a hardcoded ~/.local/state path shows nothing. It also
  // publishes its real state dir as a symlink at $XDG_RUNTIME_DIR/starlinkd
  // at start. So there are two places to read it from:
  readonly property string runtimeStateDir: Model.linkedStateDirFor(Quickshell.env("XDG_RUNTIME_DIR"))
  readonly property string mirrorStateDir: Model.stateDirFor(Quickshell.env("XDG_STATE_HOME"), Quickshell.env("HOME"))

  // ...and `source` is the one in use. Each tick reads it; when its status is
  // missing or stale it reads the other once and moves there if that one is
  // fresh — so the bar follows the daemon both ways (one that starts later,
  // runs on another state dir, or moves back) with no shell restart, and
  // shows the new data in the same tick. stateDir follows `source`, and with
  // it everything bound to stateDir.
  property string source: Model.SOURCE_MIRROR
  readonly property string stateDir: root.source === Model.SOURCE_RUNTIME ? root.runtimeStateDir : root.mirrorStateDir

  // Parsed status.json (null = missing/corrupt/oversize) and 30s staleness flag.
  // (Not named `data`: that would shadow Item.data.)
  property var statusData: null
  property bool stale: true

  // Auto-hide while away from Starlink (the dish's gRPC target stopped
  // answering as a Starlink dish, or its egress carrier changed). The daemon
  // keeps checking at its own cadence and the widget returns by itself;
  // `visible: false` collapses the ModuleSlot to zero width, keeping layout
  // position. Setting: autoHide on/off.
  readonly property bool autoHide: String(root.setting("autoHide", "on")) === "on"
  readonly property bool offNetwork: Model.isOffNetwork(root.statusData)
  visible: !root.autoHide || !root.offNetwork
  onOffNetworkChanged: {
    if (root.offNetwork) root.close() // dashboard never stays open when hidden
  }

  // Bar read cadence. While visible, the user's interval (default 2 s — the
  // daemon refreshes status at the same rate with the dashboard open). While
  // hidden the read slows to 10 s: it only has to catch the daemon flipping
  // link.onStarlink back to true, so it costs almost nothing to return.
  readonly property int readInterval: {
    var n = Number(root.setting("interval", 2000))
    return n > 0 ? n : 2000
  }

  readonly property string level: Model.level(root.statusData)
  readonly property color stateColor: level === "good" ? Model.OK_HEX
    : level === "warn" ? Model.WARN_HEX
    : level === "bad" ? Commons.Color.urgent : Commons.Color.muted

  // Typical-active hold: live.typicalDownMbps is null when the last minute
  // had too few active samples (idle link). Hold the last value for five
  // minutes so brief idle stretches don't blank the readout, then clear.
  // nowS is refreshed by tick() so the hold's expiry binding re-evaluates.
  // The hold is per dish: a different dish id clears it immediately.
  property var typicalHold: null
  property string holdDishID: ""
  property real nowS: 0
  readonly property real typicalHoldS: 300

  function noteTypical(parsed) {
    if (!parsed) return // transient unreadable file: keep the current hold
    var id = parsed.dish ? String(parsed.dish.id || "") : ""
    if (id !== root.holdDishID) {
      root.holdDishID = id
      root.typicalHold = null
    }
    var live = parsed.live || null
    var v = live ? live.typicalDownMbps : null
    if (v !== null && v !== undefined) root.typicalHold = { v: v, ts: Date.now() / 1000 }
  }

  readonly property string typicalText: {
    if (!root.typicalHold) return ""
    if (root.nowS - root.typicalHold.ts > root.typicalHoldS) return ""
    return Model.fmtMbps(root.typicalHold.v)
  }

  readonly property string value: {
    if (!root.statusData || root.stale) return ""
    var live = root.statusData.live || {}
    var show = String(root.setting("show", "latency"))
    if (show === "down") return Model.fmtMbps(live.downMbps)
    if (show === "typical") {
      // Prefer a fresh value; fall back to the hold window.
      var fresh = live.typicalDownMbps !== null && live.typicalDownMbps !== undefined
      return fresh ? Model.fmtMbps(live.typicalDownMbps) : root.typicalText
    }
    if (show === "icon") return ""
    return Model.fmtMs(live.latencyMs)
  }

  readonly property string label: root.glyph + (root.value !== "" ? " " + root.value : "")

  function tick() {
    var nowS = Date.now() / 1000
    var pick = Model.pickSource(root.source, root.readStatus, nowS, root.runtimeStateDir !== "")
    root.applyPick(pick)
    root.statusData = pick.status
    root.stale = Model.isStale(pick.status, nowS)
    root.nowS = nowS
    root.noteTypical(pick.status)
    root.pushHostSetting()
  }

  // The reader Model.pickSource decides over: one bounded read, of the file as
  // it is now (the readers below never serve a previous read).
  function readStatus(source) {
    var file = source === Model.SOURCE_RUNTIME ? runStatus : mirrorStatus
    try {
      file.reload()
      // Bounded: this runs on every tick, so a status.json past the
      // daemon's contract size is dropped as unreadable, never parsed.
      return Model.parseBounded(file.text(), Model.MAX_STATUS_CHARS)
    } catch (e) {
      return null
    }
  }

  // Takes a pickSource() result's source. A switch rebinds stateDir, and with
  // it config.json, so the applied host is re-seeded from the new path.
  function applyPick(pick) {
    if (!pick.switched) return
    root.source = pick.source
    root.seedAppliedHost()
  }

  // ---- dishHost setting → starlinkd ----
  // The manifest's dishHost setting is pushed to the daemon when it differs
  // from the persisted config target (read at start, and again when the state
  // dir switches): a live `set host` swap if the daemon runs, else
  // config.json for the next start. Edits are debounced (1.5 s stable) so
  // typing prefixes never become live swaps, and the daemon re-validates — a
  // rejected value is remembered (no retry loop) until the setting changes
  // again.
  readonly property string binPath: decodeURIComponent(
    Qt.resolvedUrl("backend/bin/starlinkd").toString().replace(/^file:\/\//, ""))
  property string appliedHost: ""
  property string pendingHost: ""
  function pushHostSetting() {
    var want = String(root.setting("dishHost", "192.168.100.1:9200")).trim()
    if (want === root.appliedHost) {
      hostDebounce.stop()
      root.pendingHost = ""
      return
    }
    if (!/^[A-Za-z0-9._\-\[\]]+(:[0-9]{1,5})?$/.test(want)) return // not (yet) valid; keep current target
    if (want === root.pendingHost && hostDebounce.running) return
    root.pendingHost = want
    hostDebounce.restart()
  }

  function pushHostNow(v) {
    hostSet.target = v
    hostSet.command = [root.binPath, "set", "host", v]
    hostSet.running = true
  }

  Timer {
    id: hostDebounce
    interval: 1500
    onTriggered: root.pushHostNow(root.pendingHost)
  }

  // No FileView in this widget logs a failed read: a missing path is normal
  // for each of them — config.json until the first `set host`, the runtime
  // dir's status.json until the daemon has published its link, the mirror's
  // on a daemon that uses another state dir — and Quickshell would log a
  // load error on every such tick.
  // blockAllReads, not blockLoading: after a state dir switch FileView keeps
  // the previous file's text until the new read lands, and the seed has to
  // see the config.json at the new path.
  FileView {
    id: configFile
    path: root.stateDir + "/config.json"
    blockAllReads: true
    printErrors: false
  }

  // STARLINKD_STATE_DIR keeps the CLI on the directory this widget reads.
  // With no daemon listening, `set host` writes config.json itself, and the
  // service's own environment may resolve another dir than the widget did;
  // the daemon gives the override highest precedence. clearEnvironment stays
  // off, so the child keeps the rest of its inherited environment.
  Process {
    id: hostSet
    property string target: ""
    command: []
    environment: ({ STARLINKD_STATE_DIR: root.stateDir })
    onExited: function(exitCode) {
      // Even on rejection, stop retrying the same value; the next edit retries.
      root.appliedHost = target
    }
  }

  // Seed the applied target from config.json: an unchanged setting is not
  // re-pushed at every shell start; when it differs, the setting is the
  // source of truth and pushes (manifest dishHost documents this). Repeated
  // after a state dir switch, for the config.json at the new path — nothing
  // readable there means nothing is applied yet.
  function seedAppliedHost() {
    try {
      configFile.reload()
      var c = Model.parseBounded(configFile.text(), Model.MAX_CONFIG_CHARS)
      root.appliedHost = c && c.target ? String(c.target) : ""
    } catch (e) { }
  }

  // Startup probe: start on the daemon's own dir whenever it has published
  // one (Model.startSource) — the seed below then reads the config.json that
  // daemon actually uses — and let the ticks sort out freshness from there.
  function probeStateDir() {
    if (root.runtimeStateDir === "") return
    var published = false
    try {
      runStatus.reload()
      published = runStatus.text() !== ""
    } catch (e) { }
    root.source = Model.startSource(published)
  }

  Component.onCompleted: {
    // Resolve the state dir first: the seed must read the config.json at
    // the path the widget ends up on.
    root.probeStateDir()
    root.seedAppliedHost()
    root.sendAlive(true)
  }

  readonly property bool opened: panelLoader.item ? panelLoader.item.opened === true : false

  function open() { if (panelLoader.item) panelLoader.item.open() }
  function close() { if (panelLoader.item) panelLoader.item.close() }
  function togglePanel() { if (panelLoader.item) panelLoader.item.toggle() }

  readonly property bool popoutSwitchClosing: panelLoader.item ? panelLoader.item.popoutSwitchClosing === true : false
  function closeForPopoutSwitch() { if (panelLoader.item) panelLoader.item.closeForPopoutSwitch() }

  function injectPanel() {
    var target = panelLoader.item
    if (!target) return
    if ("bar" in target) target.bar = root.bar
    if ("settings" in target) target.settings = root.settings
    if ("anchorItem" in target) target.anchorItem = button
    if ("hostWidget" in target) target.hostWidget = root
    if ("status" in target) target.status = root
  }

  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  onBarChanged: injectPanel()
  onSettingsChanged: injectPanel()

  Timer {
    interval: root.visible ? root.readInterval : 10000
    running: true
    repeat: true
    triggeredOnStart: true
    onTriggered: root.tick()
  }

  // One status reader per source, read straight from disk when asked. With
  // preload off and blockAllReads on, a reload()+text() pair is a single
  // synchronous read of the ~1KB file — no thread-pool hop per tick — and it
  // is always the file as it is now. Plain blockLoading keeps serving the
  // previous read until an async one lands: that put the bar one tick behind
  // and would make the freshness choice on stale text.
  FileView {
    id: runStatus
    path: root.runtimeStateDir + "/status.json"
    preload: false
    blockAllReads: true
    printErrors: false
  }
  FileView {
    id: mirrorStatus
    path: root.mirrorStateDir + "/status.json"
    preload: false
    blockAllReads: true
    printErrors: false
  }

  // ---- daemon lifetime ----
  // The service runs starlinkd with -idle-exit 15m: it exits cleanly once
  // neither a panel command nor an `alive` has reached it for that long. This
  // widget is what keeps it up — a ping at start and then every 5 minutes,
  // whatever the bar's visibility or the panel — so disabling or removing the
  // plugin takes its backend down within 15 minutes, and the next shell start
  // brings it back. `starlinkd alive` goes through the runtime socket and
  // needs no state dir. One reusable Process; a ping still running is not
  // stacked on.
  function sendAlive(mayStart) {
    if (alivePing.running) return
    alivePing.mayStart = mayStart
    alivePing.running = true
  }

  Process {
    id: alivePing
    property bool mayStart: false
    command: [root.binPath, "alive"]
    onExited: function(exitCode) {
      // Non-zero: nothing is listening. Only the start-of-session ping acts
      // on it, by starting the service once and ignoring the result — a
      // no-op when it already runs or is not installed.
      if (exitCode !== 0 && mayStart) daemonStart.running = true
      mayStart = false
    }
  }

  Process { id: daemonStart; command: ["systemctl", "--user", "start", "starlinkd.service"] }

  Timer {
    // The keepalive: always on, and well inside the daemon's 15 minutes.
    interval: 300000
    running: true
    repeat: true
    onTriggered: root.sendAlive(false)
  }

  Loader {
    id: panelLoader
    active: true
    source: Qt.resolvedUrl("Panel.qml")
    visible: false
    onLoaded: {
      root.injectPanel()
      Qt.callLater(root.injectPanel)
    }
  }

  IpcHandler {
    target: "dishy"

    function open(): void { root.open() }
    function close(): void { root.close() }
    function toggle(): void { root.togglePanel() }
  }

  WidgetButton {
    id: button
    anchors.fill: parent
    bar: root.bar
    text: root.label
    tooltipText: root.statusData && !root.stale ? Model.summary(root.statusData) : "Starlink — NO_DATA"
    horizontalMargin: 8.75
    onPressed: function(b) {
      if (b === Qt.LeftButton) root.togglePanel()
    }
  }

  // Colored state dot: green ONLINE with nothing needing attention, amber
  // SEARCHING/BOOTING or any action-worthy dish alert or update/detail
  // marker, red NO_SIGNAL/THERMAL/OFFLINE/stale, gray NO_DATA.
  Rectangle {
    id: stateDot
    visible: true
    anchors.top: button.top
    anchors.right: button.right
    anchors.margins: 2
    z: 10

    implicitWidth: Style.space(8)
    implicitHeight: Style.space(8)
    radius: height / 2
    color: root.stateColor
    border.width: 1
    border.color: root.bar ? root.bar.background : Commons.Color.background
  }
}
