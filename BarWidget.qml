import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui
import "Model.js" as Model

// Bar readout (state dot + latency/down/icon) and host for the dashboard panel.
// Reads ONLY ~/.local/state/starlinkd/status.json (CONTRACT.md).
BarWidget {
  id: root
  moduleName: "io.github.nulluserid.dishy"

  readonly property string glyph: "\udb86\udc80"  // nf-md-satellite_uplink
  readonly property string stateDir: Quickshell.env("HOME") + "/.local/state/starlinkd"

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
  property var typicalHold: null
  readonly property real typicalHoldS: 300

  function noteTypical(parsed) {
    var live = parsed && parsed.live ? parsed.live : null
    var v = live ? live.typicalDownMbps : null
    if (v !== null && v !== undefined) root.typicalHold = { v: v, ts: Date.now() / 1000 }
  }

  readonly property string typicalText: {
    if (!root.typicalHold) return ""
    if (Date.now() / 1000 - root.typicalHold.ts > root.typicalHoldS) return ""
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
    root.noteTypical(parsed)
    root.pushHostSetting()
  }

  // ---- dishHost setting → starlinkd ----
  // The manifest's dishHost setting is pushed to the daemon: valid values
  // that differ from the last-pushed one run `starlinkd set host`, which
  // swaps the live target or (daemon not running) persists config.json for
  // the next start. The daemon re-validates and rejects bad targets — a
  // rejection is remembered (no retry loop) until the setting changes again.
  readonly property string binPath: decodeURIComponent(
    Qt.resolvedUrl("backend/bin/starlinkd").toString().replace(/^file:\/\//, ""))
  property string appliedHost: ""
  function pushHostSetting() {
    var want = String(root.setting("dishHost", "192.168.100.1:9200")).trim()
    if (want === root.appliedHost) return
    if (!/^[A-Za-z0-9._\-\[\]]+(:[0-9]{1,5})?$/.test(want)) return // not (yet) valid; keep current target
    hostSet.target = want
    hostSet.command = [root.binPath, "set", "host", want]
    hostSet.running = true
  }

  Process {
    id: hostSet
    property string target: ""
    command: []
    onExited: function(exitCode) {
      // Even on rejection, stop retrying the same value; the next edit retries.
      root.appliedHost = target
    }
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

  // Colored state dot: green ONLINE, amber SEARCHING/OBSTRUCTION/REBOOT,
  // red NO_SIGNAL/THERMAL/OFFLINE/stale, gray NO_DATA.
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
