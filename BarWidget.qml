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

  // Auto-hide while away from Starlink (the local network claimed the dish's
  // exact gRPC target, the LAN subnet, or its egress carrier). The daemon
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

  readonly property string value: {
    if (!root.statusData || root.stale) return ""
    var live = root.statusData.live || {}
    var show = String(root.setting("show", "latency"))
    if (show === "down") return Model.fmtMbps(live.downMbps)
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
