import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui
import "Model.js" as Model

// Starlink dashboard. status.json flows in from the bar widget (injected as
// `status`); hist.json is read ONLY while the panel is open (on open + 2s).
Panel {
  id: root
  moduleName: "io.github.nulluserid.dishy"
  ipcTarget: "dishy"
  manageIpc: false

  property var anchorItem: null
  property var hostWidget: null
  property var status: null
  readonly property var barIdentity: hostWidget || root

  readonly property color fg: root.barForeground
  readonly property color dim: Color.muted
  readonly property string fontFamily: bar ? bar.fontFamily : Style.font.family
  readonly property string stateDir: Quickshell.env("HOME") + "/.local/state/starlinkd"
  readonly property string binPath: decodeURIComponent(
    Qt.resolvedUrl("backend/bin/starlinkd").toString().replace(/^file:\/\//, ""))

  // status.json mirror (kept fresh by the bar widget's own timer).
  readonly property var s: status ? status.statusData : null
  readonly property bool sStale: status ? status.stale : true
  readonly property string level: status ? status.level : "none"
  readonly property color stateColor: level === "good" ? Model.OK_HEX
    : level === "warn" ? Model.WARN_HEX
    : level === "bad" ? Color.urgent : Color.muted
  readonly property bool hasData: s !== null && !sStale

  // hist.json: parsed only while open; null = unreadable/missing.
  property var h: null
  function reloadHist() {
    try {
      histFile.reload()
      root.h = Model.parse(histFile.text())
    } catch (e) {
      root.h = null
    }
  }

  // day-YYYYMMDD.json: per-minute day aggregates, read only while open.
  // Lives in the current dish's partition (CONTRACT.md v4); the stamp
  // re-binds at local midnight and the path follows the dish identity.
  property var dayData: null
  property string dayStamp: Qt.formatDateTime(new Date(), "yyyyMMdd")
  readonly property string dayPath: {
    var id = s && s.dish ? String(s.dish.id || "") : ""
    var dir = id !== "" ? Model.dishDirName(id) : ""
    return root.stateDir + (dir !== "" ? "/dishes/" + dir : "")
      + "/day-" + root.dayStamp + ".json"
  }
  function reloadDay() {
    root.dayStamp = Qt.formatDateTime(new Date(), "yyyyMMdd")
    try {
      dayFile.reload()
      root.dayData = Model.parse(dayFile.text())
    } catch (e) {
      root.dayData = null
    }
  }

  // Today's typical-down series so far (minute-of-day order, null gaps).
  readonly property var daySeries: {
    if (!root.dayData || !root.dayData.minutes) return []
    var now = new Date()
    var cur = now.getHours() * 60 + now.getMinutes()
    var m = root.dayData.minutes
    var out = []
    for (var i = 0; i <= cur && i < m.length; i++) {
      var v = m[i] ? m[i].downTyp : null
      out.push(v ? v : null)
    }
    return out
  }

  // Today's peak typical, for the section value line.
  readonly property var dayPeak: {
    var peak = null
    var a = root.daySeries
    for (var i = 0; i < a.length; i++) {
      if (a[i] !== null && (peak === null || a[i] > peak)) peak = a[i]
    }
    return peak
  }

  // Dish controls (dial-on-demand CLI). Each button needs an inline
  // two-step confirm before anything is spawned; nothing auto-fires.
  property string armedAction: ""
  property bool ctlBusy: false
  property bool ctlFailed: false
  property string ctlResult: ""
  property string ctlOutput: ""
  property string ctlErrText: ""

  // Panel <-> daemon cadence link: opening the dashboard asks starlinkd for
  // an immediate refresh and opens the fast-poll lease; a 60 s heartbeat
  // keeps the lease alive while open; close returns it to idle at once. The
  // lease itself (90 s) covers a crashed shell (CONTRACT.md "Panel control
  // socket"). Best-effort: errors only mean the daemon runs at idle cadence.
  function notifyPanel(mode) {
    if (ctlLink.running) return
    ctlLink.command = [root.binPath, "panel", mode]
    ctlLink.running = true
  }

  onOpenedChanged: root.notifyPanel(root.opened ? "open" : "close")
  Component.onDestruction: {
    if (root.opened) root.notifyPanel("close")
  }

  function runCtl(action) {
    if (root.ctlBusy) return
    root.armedAction = ""
    root.ctlFailed = false
    root.ctlResult = "Running " + action + "…"
    root.ctlOutput = ""
    root.ctlErrText = ""
    ctl.action = action
    ctl.command = [root.binPath, action]
    root.ctlBusy = true
    ctl.running = true
  }

  function open() { root.controller.show() }
  function close() {
    root.controller.hide()
    root.h = null
    root.ctlResult = ""
    root.armedAction = ""
  }
  function toggle() { root.opened ? root.close() : root.open() }

  function scrollBy(dy) {
    var max = Math.max(0, dashScroll.contentHeight - dashScroll.height)
    dashScroll.contentY = Math.max(0, Math.min(max, dashScroll.contentY + dy * 80))
  }

  // p50/p95 guide lines: lat5m when fresh, else derived from the series.
  readonly property var latencyGuides: {
    var lat = s && s.lat5m ? s.lat5m : {}
    var series = h ? h.latencyMs : []
    var p50 = lat.p50 !== undefined && lat.p50 !== null ? lat.p50 : Model.percentile(series, 50)
    var p95 = lat.p95 !== undefined && lat.p95 !== null ? lat.p95 : Model.percentile(series, 95)
    var g = []
    if (p50 !== null) g.push({ value: p50, label: "p50 " + Math.round(p50), color: String(Color.muted) })
    if (p95 !== null) g.push({ value: p95, label: "p95 " + Math.round(p95), color: Model.WARN_HEX })
    return g
  }

  // One shared y-scale for down+up so the two series are comparable.
  readonly property real throughputMax: {
    var m = 1
    var down = h ? h.downMbps : []
    var up = h ? h.upMbps : []
    for (var i = 0; i < down.length; i++) if (down[i] > m) m = down[i]
    for (var j = 0; j < up.length; j++) if (up[j] > m) m = up[j]
    return m
  }

  readonly property bool hasPower: {
    var p = h ? h.powerW : []
    for (var i = 0; i < p.length; i++) if (p[i] !== null && p[i] !== undefined) return true
    return false
  }

  KeyboardPanel {
    id: panel
    anchorItem: root.anchorItem
    owner: root.barIdentity
    bar: root.bar
    open: root.opened
    centerOnBar: true
    focusTarget: keyCatcher
    contentWidth: panel.fittedContentWidth(Style.space(720))
    contentHeight: panel.fittedContentHeight(Style.space(480))

    PanelKeyCatcher {
      id: keyCatcher
      anchors.fill: parent
      onMoveRequested: function(dx, dy) { if (dy !== 0) root.scrollBy(dy) }
      onCloseRequested: root.close()
      onTabRequested: function(direction) { if (root.bar) root.bar.switchPanelFrom(root.barIdentity, direction) }
      onTextKey: function(t) { if (t === "q" || t === "Q") root.close() }

      // Waiting for the daemon.
      Column {
        anchors.centerIn: parent
        visible: !root.hasData
        spacing: Style.space(8)

        Text {
          anchors.horizontalCenter: parent.horizontalCenter
          text: "Waiting for starlinkd data…"
          textFormat: Text.PlainText
          color: root.dim
          font.family: root.fontFamily
          font.pixelSize: Style.font.subtitle
        }

        Text {
          anchors.horizontalCenter: parent.horizontalCenter
          text: "systemctl --user start starlinkd"
          textFormat: Text.PlainText
          color: root.dim
          font.family: root.fontFamily
          font.pixelSize: Style.font.bodySmall
        }
      }

      // Dashboard.
      Flickable {
        id: dashScroll
        anchors.fill: parent
        visible: root.hasData
        contentWidth: width
        contentHeight: dashColumn.implicitHeight
        clip: true
        boundsBehavior: Flickable.StopAtBounds
        interactive: contentHeight > height

        Column {
          id: dashColumn
          width: dashScroll.width
          spacing: Style.space(6)

          // ---- Header: state chip + identity (80s-readout dense) ----
          Row {
            id: headerRow
            width: parent.width
            spacing: Style.space(10)

            Rectangle {
              id: stateChip
              implicitWidth: stateRow.implicitWidth + Style.space(16)
              implicitHeight: Style.space(30)
              radius: Style.space(5)
              color: Qt.rgba(root.stateColor.r, root.stateColor.g, root.stateColor.b, 0.12)
              border.width: 1
              border.color: root.stateColor

              Row {
                id: stateRow
                anchors.centerIn: parent
                spacing: Style.space(7)

                Rectangle {
                  anchors.verticalCenter: parent.verticalCenter
                  width: Style.space(8)
                  height: width
                  radius: height / 2
                  color: root.stateColor
                }

                Text {
                  anchors.verticalCenter: parent.verticalCenter
                  text: root.hasData ? Model.titleCase(s.state) : "NO DATA"
                  textFormat: Text.PlainText
                  color: root.fg
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.body
                  font.bold: true
                }
              }
            }

            Column {
              anchors.verticalCenter: parent.verticalCenter
              width: headerRow.width - stateChip.implicitWidth - headerRow.spacing
              spacing: 0

              Text {
                text: root.hasData
                  ? "up " + Model.humanDur(s.dish ? s.dish.uptimeS : null)
                    + " · GPS " + (s.gps && s.gps.valid ? s.gps.sats : "—")
                    + " · eth " + Model.fmtMbps(s.dish ? s.dish.ethMbps : null)
                  : ""
                textFormat: Text.PlainText
                color: root.fg
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
              }

              Text {
                text: root.hasData && s.dish
                  ? String(s.dish.id || "").substring(0, 8) + " · " + (s.dish.hw || "—")
                    + " · " + String(s.dish.sw || "—").split(".").slice(0, 3).join(".")
                  : ""
                textFormat: Text.PlainText
                elide: Text.ElideRight
                width: Math.min(implicitWidth, headerRow.width - stateChip.implicitWidth - headerRow.spacing)
                color: root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
              }
            }
          }

          // ---- Alert + update chips ----
          Flow {
            width: parent.width
            spacing: Style.space(5)

            Repeater {
              model: root.hasData && s.alerts ? s.alerts : []

              Rectangle {
                required property var modelData
                implicitWidth: chipLabel.implicitWidth + Style.space(12)
                implicitHeight: chipLabel.implicitHeight + Style.space(6)
                radius: height / 2
                color: "transparent"
                border.width: 1
                border.color: Model.WARN_HEX

                Text {
                  id: chipLabel
                  anchors.centerIn: parent
                  text: modelData && modelData.label ? String(modelData.label) : "alert"
                  textFormat: Text.PlainText
                  color: Model.WARN_HEX
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.bodySmall
                }
              }
            }

            // Software update / state detail chip.
            Rectangle {
              visible: root.hasData && updateText !== ""
              implicitWidth: updateLabel.implicitWidth + Style.space(12)
              implicitHeight: updateLabel.implicitHeight + Style.space(6)
              radius: height / 2
              color: "transparent"
              border.width: 1
              border.color: Model.WARN_HEX

              readonly property string updateText: {
                if (!root.hasData) return ""
                var u = s.update || {}
                if (u.state === "REBOOT_REQUIRED") return "Software update — reboot required"
                if (u.state === "FETCHING" || u.state === "WRITING")
                  return "Software update — " + Model.fmtPct(u.progressPct * 100, 0)
                if (u.state === "FAULTED") return "Software update — faulted"
                var det = String(s.stateDetail || "")
                if (det !== "" && det !== "SOFTWARE_UPDATE_REBOOT_PENDING")
                  return Model.titleCase(det)
                return ""
              }

              Text {
                id: updateLabel
                anchors.centerIn: parent
                text: parent.updateText
                textFormat: Text.PlainText
                color: Model.WARN_HEX
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
              }
            }
          }

          // ---- Graph grid (two columns, 80s-readout dense) ----
          Row {
            width: parent.width
            spacing: Style.space(10)

            // Latency: plot, drop strip, drops line.
            Column {
              width: (parent.width - parent.spacing) / 2
              spacing: Style.space(3)

              Item {
                width: parent.width
                height: Math.max(latTitle.implicitHeight, latValue.implicitHeight)

                Text {
                  id: latTitle
                  text: "LATENCY"
                  textFormat: Text.PlainText
                  color: root.dim
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.bodySmall
                  font.bold: true
                }

                Text {
                  id: latValue
                  anchors.right: parent.right
                  text: Model.fmtMs(root.hasData && s.live ? s.live.latencyMs : null)
                  textFormat: Text.PlainText
                  color: root.fg
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.body
                  font.bold: true
                }
              }

              Sparkline {
                width: parent.width
                height: Style.space(46)
                values: Model.decimate(h ? h.latencyMs : [], width)
                guides: root.latencyGuides
                autoScale: true
                lineColor: Color.accent
              }

              DropStrip {
                width: parent.width
                drops: h ? h.dropRate : []
              }

              Text {
                width: parent.width
                text: root.hasData && s.drop15m
                  ? "drops " + Model.fmtPct(s.drop15m.ratio * 100, 2) + " · "
                    + (s.drop15m.count15m || 0) + " ev · " + Model.humanDur(s.drop15m.totalOutageS)
                  : ""
                textFormat: Text.PlainText
                elide: Text.ElideRight
                color: root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
              }
            }

            // Throughput: down (fill) + up (line), legend line under.
            Column {
              width: (parent.width - parent.spacing) / 2
              spacing: Style.space(3)

              Item {
                width: parent.width
                height: Math.max(thrTitle.implicitHeight, thrValue.implicitHeight)

                Text {
                  id: thrTitle
                  text: "THROUGHPUT"
                  textFormat: Text.PlainText
                  color: root.dim
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.bodySmall
                  font.bold: true
                }

                Text {
                  id: thrValue
                  anchors.right: parent.right
                  text: root.hasData && s.live
                    ? "↓" + Model.fmtMbps(s.live.downMbps) + " ↑" + Model.fmtMbps(s.live.upMbps)
                    : ""
                  textFormat: Text.PlainText
                  color: root.fg
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.body
                  font.bold: true
                }
              }

              Item {
                width: parent.width
                height: Style.space(46)

                Sparkline {
                  anchors.fill: parent
                  values: Model.decimate(h ? h.downMbps : [], width)
                  maxValue: root.throughputMax
                  lineColor: Color.accent
                  area: true
                }

                Sparkline {
                  anchors.fill: parent
                  values: Model.decimate(h ? h.upMbps : [], width)
                  maxValue: root.throughputMax
                  lineColor: Model.WARN_HEX
                }
              }

              Text {
                width: parent.width
                text: root.hasData && s.live && s.live.typicalDownMbps !== null && s.live.typicalDownMbps !== undefined
                  ? "typ ↓ " + Model.fmtMbps(s.live.typicalDownMbps)
                  : "↓ down (fill) · ↑ up (line)"
                textFormat: Text.PlainText
                elide: Text.ElideRight
                color: root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
              }
            }
          }

          // ---- Today | Power grid ----
          Row {
            width: parent.width
            spacing: Style.space(10)

            GraphSection {
              width: root.hasPower ? (parent.width - parent.spacing) / 2 : parent.width
              title: "TODAY"
              valueText: root.hasData && s.live && s.live.typicalDownMbps !== null && s.live.typicalDownMbps !== undefined
                ? Model.fmtMbps(s.live.typicalDownMbps)
                : (root.dayPeak !== null ? "peak " + Model.fmtMbps(root.dayPeak) : "—")
              series: root.daySeries
              autoScale: true
              plotHeight: 38
            }

            GraphSection {
              visible: root.hasPower
              width: (parent.width - parent.spacing) / 2
              title: "POWER"
              valueText: Model.fmtW(root.hasData && s.live ? s.live.powerW : null)
              series: Model.decimate(h ? h.powerW : [], width)
              autoScale: true
              plotHeight: 38
            }
          }

          // ---- Outages (compact; newest first) ----
          Column {
            width: parent.width
            spacing: Style.space(2)
            visible: !!(h && h.outages && h.outages.length > 0)

            Row {
              width: parent.width
              spacing: Style.space(6)

              Text {
                text: "OUTAGES"
                textFormat: Text.PlainText
                color: root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                font.bold: true
              }

              Text {
                text: h && h.outages ? h.outages.length + " recent" : ""
                textFormat: Text.PlainText
                color: root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
              }
            }

            Repeater {
              model: h && h.outages ? h.outages.slice(0, 3) : []

              Text {
                required property var modelData
                // The dish reports some old boot outages with a stale epoch
                // (e.g. 2016); suppress the "ago" segment for those.
                readonly property bool plausibleStart: modelData.start > 1609459200
                width: parent.width
                text: (modelData.cause ? Model.titleCase(modelData.cause) : "Unknown")
                  + (plausibleStart ? " · " + Model.agoText(modelData.start, Date.now() / 1000) : "")
                  + " · " + Model.humanDur(modelData.durS)
                textFormat: Text.PlainText
                elide: Text.ElideRight
                color: root.fg
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
              }
            }
          }

          // ---- Obstruction | Connection side by side ----
          Row {
            width: parent.width
            spacing: Style.space(10)

            Column {
              width: parent.width * 0.55 - parent.spacing / 2
              spacing: Style.space(3)

              Text {
                text: "OBSTRUCTION"
                textFormat: Text.PlainText
                color: root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                font.bold: true
              }

              Row {
                width: parent.width
                spacing: Style.space(8)

                ObsMap {
                  id: obsMapPlot
                  anchors.verticalCenter: parent.verticalCenter
                  width: Style.space(84)
                  height: Style.space(84)
                  snr: h && h.map ? h.map.snr : []
                  rows: h && h.map ? h.map.rows : 0
                  cols: h && h.map ? h.map.cols : 0
                  maxThetaDeg: h && h.map ? h.map.maxThetaDeg : 90
                  baseColor: root.dim
                }

                Column {
                  anchors.verticalCenter: parent.verticalCenter
                  width: parent.width - obsMapPlot.width - parent.spacing
                  spacing: 0

                  Repeater {
                    model: root.hasData && s.obs ? [
                      { label: "Obstructed", value: Model.fmtPct(s.obs.fraction * 100, 2) },
                      { label: "Time", value: Model.humanDur(s.obs.timeObstructedS) },
                      { label: "Valid", value: Model.humanDur(s.obs.validS) },
                      { label: "FoV", value: "θ ≤ " + (h && h.map ? h.map.maxThetaDeg : 90) + "°" }
                    ] : []

                    KVRow {
                      required property var modelData
                      small: true
                      label: modelData.label
                      value: modelData.value
                    }
                  }
                }
              }
            }

            Column {
              width: parent.width * 0.45 - parent.spacing / 2
              spacing: Style.space(3)

              Text {
                text: "CONNECTION"
                textFormat: Text.PlainText
                color: root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                font.bold: true
              }

              Column {
                width: parent.width
                spacing: 0
                visible: !!(root.hasData && s.geo)

                KVRow {
                  small: true
                  label: "IP"
                  value: root.hasData && s.geo ? String(s.geo.ip || "—") : ""
                }
                KVRow {
                  small: true
                  label: "Exit"
                  value: root.hasData && s.geo ? (Model.geoPlace(s.geo) || "—") : ""
                }
                KVRow {
                  small: true
                  label: "Net"
                  value: root.hasData && s.geo
                    ? (s.geo.org ? String(s.geo.org) : "—") + (s.geo.asn ? " (" + s.geo.asn + ")" : "")
                    : ""
                }

                Text {
                  width: parent.width
                  text: "approximate egress"
                  textFormat: Text.PlainText
                  elide: Text.ElideRight
                  color: root.dim
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.bodySmall
                }
              }

              Text {
                visible: !(root.hasData && s.geo)
                width: parent.width
                text: "no egress data yet"
                textFormat: Text.PlainText
                color: root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
              }
            }
          }

          // ---- Usage / availability grid ----
          PanelSeparator { foreground: root.fg }

          Row {
            width: parent.width
            spacing: Style.space(12)

            Text {
              width: (parent.width - 2 * parent.spacing) * 0.32
              text: root.hasData && s.usage
                ? "TODAY ↓ " + Model.fmtGB(s.usage.downGB) + " ↑ " + Model.fmtGB(s.usage.upGB)
                : ""
              textFormat: Text.PlainText
              elide: Text.ElideRight
              color: root.fg
              font.family: root.fontFamily
              font.pixelSize: Style.font.bodySmall
              font.bold: true
            }

            Text {
              width: (parent.width - 2 * parent.spacing) * 0.40
              text: root.hasData && s.usage
                ? "ALL-TIME ↓ " + Model.fmtGB(s.usage.allTimeDownGB) + " ↑ " + Model.fmtGB(s.usage.allTimeUpGB)
                : ""
              textFormat: Text.PlainText
              elide: Text.ElideRight
              color: root.fg
              font.family: root.fontFamily
              font.pixelSize: Style.font.bodySmall
              font.bold: true
            }

            Text {
              width: (parent.width - 2 * parent.spacing) * 0.28
              text: root.hasData && s.avail
                ? "AVAIL " + Model.fmtPct(s.avail.todayPct, 1) + " / " + Model.fmtPct(s.avail.sessionPct, 1)
                : ""
              textFormat: Text.PlainText
              elide: Text.ElideRight
              color: root.fg
              font.family: root.fontFamily
              font.pixelSize: Style.font.bodySmall
              font.bold: true
            }
          }

          // ---- Controls (owner-approved): inline title + two-step confirm ----
          Column {
            width: parent.width
            spacing: Style.space(3)

            Row {
              spacing: Style.space(8)

              Text {
                anchors.verticalCenter: parent.verticalCenter
                text: "CONTROLS"
                textFormat: Text.PlainText
                color: root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.bodySmall
                font.bold: true
              }

              Repeater {
                model: [
                  { action: "stow", title: "Stow" },
                  { action: "unstow", title: "Unstow" },
                  { action: "reboot", title: "Reboot" }
                ]

                ConfirmButton {
                  required property var modelData
                  action: modelData.action
                  title: modelData.title
                  onFired: function(a) { root.runCtl(a) }
                }
              }
            }

            Text {
              visible: root.ctlResult !== ""
              width: parent.width
              text: root.ctlResult
              textFormat: Text.PlainText
              elide: Text.ElideRight
              color: root.ctlBusy ? root.dim : (root.ctlFailed ? Color.urgent : Model.OK_HEX)
              font.family: root.fontFamily
              font.pixelSize: Style.font.bodySmall
            }
          }

          Text {
            width: parent.width
            horizontalAlignment: Text.AlignHCenter
            text: "Esc  close   ·   ↑ ↓  scroll"
            textFormat: Text.PlainText
            color: root.dim
            font.family: root.fontFamily
            font.pixelSize: Style.font.bodySmall
          }
        }
      }
    }
  }

  Timer {
    interval: 4000
    running: root.armedAction !== ""
    repeat: false
    onTriggered: root.armedAction = ""
  }

  Timer {
    interval: 2000
    running: root.opened
    repeat: true
    triggeredOnStart: true
    onTriggered: {
      root.reloadHist()
      root.reloadDay()
    }
  }

  Timer {
    interval: 60000
    running: root.opened
    repeat: true
    onTriggered: root.notifyPanel("open") // heartbeat: keep the fast lease
  }

  Process {
    id: ctlLink
    command: []
  }

  FileView { id: histFile; path: root.stateDir + "/hist.json"; blockLoading: true }
  FileView { id: dayFile; path: root.dayPath; blockLoading: true }

  Process {
    id: ctl
    property string action: ""
    command: []
    stdout: StdioCollector { onStreamFinished: root.ctlOutput = text.trim() }
    stderr: StdioCollector { onStreamFinished: root.ctlErrText = text.trim() }
    onExited: function(exitCode) {
      root.ctlBusy = false
      root.ctlFailed = exitCode !== 0
      root.ctlResult = exitCode === 0
        ? (root.ctlOutput !== "" ? root.ctlOutput : "Done")
        : (root.ctlErrText !== "" ? root.ctlErrText
                                  : "Failed (exit " + exitCode + ")")
    }
  }

  // Section title + current value on one line, graph below.
  component GraphSection: Column {
    id: gs
    property string title: ""
    property string valueText: ""
    property var series: []
    property var guides: []
    property bool autoScale: false
    property real maxValue: 100
    property int plotHeight: 40

    width: parent ? parent.width : 0
    spacing: Style.space(3)

    Item {
      width: parent.width
      height: Math.max(gsTitle.implicitHeight, gsValue.implicitHeight)

      Text {
        id: gsTitle
        text: gs.title
        textFormat: Text.PlainText
        color: root.dim
        font.family: root.fontFamily
        font.pixelSize: Style.font.bodySmall
        font.bold: true
      }

      Text {
        id: gsValue
        anchors.right: parent.right
        text: gs.valueText
        textFormat: Text.PlainText
        color: root.fg
        font.family: root.fontFamily
        font.pixelSize: Style.font.body
        font.bold: true
      }
    }

    Sparkline {
      width: parent.width
      height: Style.space(gs.plotHeight)
      values: gs.series
      guides: gs.guides
      autoScale: gs.autoScale
      maxValue: gs.maxValue
      lineColor: Color.accent
    }
  }

  // Label / value row (wrapper Item: Row children may not use anchors).
  component KVRow: Item {
    id: kv
    property string label: ""
    property string value: ""
    property bool bold: false
    property bool small: false

    width: parent ? parent.width : 0
    implicitHeight: Math.max(kvLabel.implicitHeight, kvValue.implicitHeight)

    Text {
      id: kvLabel
      width: parent.width - kvValue.implicitWidth - Style.space(8)
      text: kv.label
      textFormat: Text.PlainText
      elide: Text.ElideRight
      color: root.dim
      font.family: root.fontFamily
      font.pixelSize: kv.small ? Style.font.bodySmall : Style.font.body
      font.bold: kv.bold
    }

    Text {
      id: kvValue
      anchors.right: parent.right
      text: kv.value
      textFormat: Text.PlainText
      color: root.fg
      font.family: root.fontFamily
      font.pixelSize: kv.small ? Style.font.bodySmall : Style.font.body
      font.bold: kv.bold
    }
  }

  component ConfirmButton: Rectangle {
    id: cb
    property string action: ""
    property string title: ""
    signal fired(string action)

    readonly property bool armed: root.armedAction === action

    implicitWidth: cbLabel.implicitWidth + Style.space(12)
    implicitHeight: Style.space(26)
    radius: Style.space(6)
    color: armed ? Qt.rgba(Color.urgent.r, Color.urgent.g, Color.urgent.b, 0.18) : "transparent"
    border.width: 1
    border.color: armed ? Color.urgent : Qt.rgba(root.fg.r, root.fg.g, root.fg.b, 0.35)

    Text {
      id: cbLabel
      anchors.centerIn: parent
      text: cb.armed ? "Confirm?" : cb.title
      textFormat: Text.PlainText
      color: cb.armed ? Color.urgent : root.fg
      font.family: root.fontFamily
      font.pixelSize: Style.font.body
    }

    MouseArea {
      anchors.fill: parent
      cursorShape: Qt.PointingHandCursor
      onClicked: {
        if (root.ctlBusy) return
        if (!cb.armed) root.armedAction = cb.action
        else cb.fired(cb.action)
      }
    }
  }

  // Thin strip: one tick per sample, red where the ping dropped.
  component DropStrip: Canvas {
    id: strip
    property var drops: []
    implicitHeight: Style.space(10)

    onDropsChanged: requestPaint()
    onWidthChanged: requestPaint()
    Component.onCompleted: requestPaint()

    onPaint: {
      var ctx = getContext("2d")
      ctx.reset()
      if (width <= 0 || height <= 0 || !drops.length) return
      ctx.fillStyle = Qt.rgba(root.fg.r, root.fg.g, root.fg.b, 0.12)
      ctx.fillRect(0, 0, width, height)
      var step = width / drops.length
      ctx.fillStyle = String(Color.urgent)
      for (var i = 0; i < drops.length; i++) {
        if (drops[i] === 1) ctx.fillRect(i * step, 0, Math.max(1, step), height)
      }
    }
  }
}
