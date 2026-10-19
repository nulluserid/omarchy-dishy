import QtQuick
import qs.Commons

// Line graph over a series where null entries are gaps (dropped samples).
// Optional dashed, labeled guide lines and an area fill under the line.
Canvas {
  id: root

  property var values: []
  property real maxValue: 100
  property bool autoScale: false
  property color lineColor: Color.accent
  property bool area: false
  property color fillColor: Qt.rgba(lineColor.r, lineColor.g, lineColor.b, 0.16)
  property var guides: []  // [{ value, label, color }]
  property real lineW: 1.5

  readonly property real effectiveMax: {
    var m = maxValue
    if (autoScale) for (var i = 0; i < values.length; i++) {
      var v = values[i]
      if (v !== null && v !== undefined && v > m) m = v
    }
    return m > 0 ? m : 1
  }

  implicitHeight: 28

  onValuesChanged: requestPaint()
  onLineColorChanged: requestPaint()
  onFillColorChanged: requestPaint()
  onGuidesChanged: requestPaint()
  onAutoScaleChanged: requestPaint()
  onWidthChanged: requestPaint()
  onHeightChanged: requestPaint()
  Component.onCompleted: requestPaint()

  function yFor(v) {
    var t = Math.max(0, Math.min(1, v / effectiveMax))
    return height - t * (height - 2) - 1
  }

  onPaint: {
    var ctx = getContext("2d")
    ctx.reset()
    if (width <= 0 || height <= 0) return
    var n = values.length
    if (n < 2) return
    var step = width / (n - 1)

    if (area) {
      ctx.beginPath()
      var runStart = -1
      var prevX = 0
      for (var a = 0; a < n; a++) {
        var av = values[a]
        if (av === null || av === undefined) {
          if (runStart >= 0) { ctx.lineTo(prevX, height); ctx.lineTo(runStart, height); runStart = -1 }
          continue
        }
        var ax = a * step
        var ay = yFor(av)
        if (runStart < 0) { ctx.moveTo(ax, height); ctx.lineTo(ax, ay); runStart = ax }
        else ctx.lineTo(ax, ay)
        prevX = ax
      }
      if (runStart >= 0) { ctx.lineTo(prevX, height); ctx.lineTo(runStart, height) }
      ctx.closePath()
      ctx.fillStyle = fillColor
      ctx.fill()
    }

    ctx.lineWidth = lineW
    ctx.strokeStyle = lineColor
    ctx.beginPath()
    var pen = false
    for (var i = 0; i < n; i++) {
      var v = values[i]
      if (v === null || v === undefined) { pen = false; continue }
      var x = i * step
      var y = yFor(v)
      if (!pen) { ctx.moveTo(x, y); pen = true }
      else ctx.lineTo(x, y)
    }
    ctx.stroke()

    for (var g = 0; g < guides.length; g++) {
      var guide = guides[g]
      if (guide.value === null || guide.value === undefined) continue
      var gy = yFor(guide.value)
      ctx.setLineDash([4, 3])
      ctx.lineWidth = 1
      ctx.strokeStyle = guide.color
      ctx.beginPath()
      ctx.moveTo(0, gy)
      ctx.lineTo(width, gy)
      ctx.stroke()
      ctx.setLineDash([])
      if (guide.label) {
        ctx.fillStyle = guide.color
        ctx.fillText(guide.label, width - ctx.measureText(guide.label).width - 2, Math.max(9, gy - 3))
      }
    }
  }

  function lastIndexBefore(n, from) {
    for (var i = n - 1; i >= from; i--) {
      var v = values[i]
      if (v !== null && v !== undefined) return i
    }
    return from
  }
}
