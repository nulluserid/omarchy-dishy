import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui
import "Model.js" as Model

// Bar readout (state dot + latency/down/icon) and host for the dashboard panel.
// Reads ONLY the daemon's status.json (CONTRACT.md), from the daemon's state
// dir: $XDG_STATE_HOME/starlinkd (default ~/.local/state/starlinkd) until the
// daemon's $XDG_RUNTIME_DIR/starlinkd symlink is seen, then that.
BarWidget {
  id: root
  moduleName: "io.github.nulluserid.dishy"

  readonly property string glyph: "\udb86\udc80"  // nf-md-satellite_uplink

  // The daemon honors XDG_STATE_HOME: a service on another state dir updates
  // fine while a hardcoded ~/.local/state path shows nothing. It also
  // publishes its real state dir as a symlink at $XDG_RUNTIME_DIR/starlinkd
  // at start; the probe below flips `linked` once that file answers.
  property bool linked: false
  readonly property string stateDir: root.linked ? Model.linkedStateDirFor(Quickshell.env("XDG_RUNTIME_DIR"))
    : Model.stateDirFor(Quickshell.env("XDG_STATE_HOME"), Quickshell.env("HOME"))

  // Parsed status.json (null = missing/corrupt) and 30s staleness flag.
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
    : level === "bad" ? Color.urgent : Color.muted

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
    var parsed = null
    try {
      statusFile.reload()
      parsed = Model.parse(statusFile.text())
    } catch (e) {
      parsed = null
    }
    root.statusData = parsed
    root.stale = Model.isStale(parsed, Date.now() / 1000)
    if (root.stale) root.refreshStateDir() // missing counts as stale
    root.nowS = Date.now() / 1000
    root.noteTypical(parsed)
    root.pushHostSetting()
  }

  // ---- dishHost setting → starlinkd ----
  // The manifest's dishHost setting is pushed to the daemon when it differs
  // from the persisted config target (read at start, and again when the state
  // dir switches): a live `set host`
  // swap if the daemon runs, else config.json for the next start. Edits are
  // debounced (1.5 s stable) so typing prefixes never become live swaps, and
  // the daemon re-validates — a rejected value is remembered (no retry loop)
  // until the setting changes again.
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

  // blockAllReads, not blockLoading: after a state dir switch FileView keeps
  // the previous file's text until the new read lands, and the seed has to
  // see the config.json at the new path.
  FileView { id: configFile; path: root.stateDir + "/config.json"; blockAllReads: true }

  Process {
    id: hostSet
    property string target: ""
    command: []
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
      var c = Model.parse(configFile.text())
      root.appliedHost = c && c.target ? String(c.target) : ""
    } catch (e) { }
  }

  // State dir probe: one small read, only while unlinked, so it can ride the
  // tick — a daemon that starts after the shell, or runs on another state
  // dir, is picked up without a shell restart.
  function refreshStateDir() {
    if (root.linked || Quickshell.env("XDG_RUNTIME_DIR") === "") return
    try {
      linkProbe.reload()
      if (linkProbe.text() !== "") {
        root.linked = true
        root.seedAppliedHost()
      }
    } catch (e) { }
  }

  Component.onCompleted: {
    // Resolve the state dir first: the seed must read the config.json at
    // the path the widget ends up on.
    root.refreshStateDir()
    root.seedAppliedHost()
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

  FileView { id: statusFile; path: root.stateDir + "/status.json"; blockLoading: true }

  // The probe's "not found" is the normal state until the updated daemon has
  // run this session, so it is not logged (the other FileViews' failures are).
  FileView {
    id: linkProbe
    path: Model.linkedStateDirFor(Quickshell.env("XDG_RUNTIME_DIR")) + "/status.json"
    blockLoading: true
    printErrors: false
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
    border.color: root.bar ? root.bar.background : Color.background
  }
}
