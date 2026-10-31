import QtQuick
import qs.Commons
import "Model.js" as Model

// Sky-coverage reticle over hist.map. On current Mini firmware the map is
// effectively a binary per-patch coverage mask (valid cells ≈ 1, no-data ≈ -1),
// so a heat ramp just makes a blob: render the mapped sky as a soft mask with
// elevation rings instead, and let the numeric stats carry the signal.
// snr is row-major rows x cols; -1 = no data; cells outside the unit circle
// are masked (hemisphere).
Canvas {
  id: root

  property var snr: []
  property int rows: 0
  property int cols: 0
  property real maxThetaDeg: 90
  property color baseColor: Color.muted

  implicitHeight: 140

  onSnrChanged: requestPaint()
  onRowsChanged: requestPaint()
  onColsChanged: requestPaint()
  onWidthChanged: requestPaint()
  onHeightChanged: requestPaint()
  Component.onCompleted: requestPaint()

  onPaint: {
    var ctx = getContext("2d")
    ctx.reset()
    if (width <= 0 || height <= 0 || rows <= 0 || cols <= 0 || !snr.length) return
    // Defense in depth: rows/cols come from a daemon-parsed dish response.
    // A hostile or incoherent grid (declared cells exceeding the delivered
    // data, or absurd dims) must never drive this GUI-thread loop.
    if (rows * cols > snr.length || rows > 1024 || cols > 1024) return
    var size = Math.min(width, height)
    var ox = (width - size) / 2
    var oy = (height - size) / 2
    var cxp = ox + size / 2
    var cyp = oy + size / 2
    var r = size / 2 - 2
    var cw = size / cols
    var ch = size / rows
    var ccx = (cols - 1) / 2
    var ccy = (rows - 1) / 2
    var radius = Math.min(ccx, ccy)

    // Mapped sky: soft neutral mask per valid cell.
    for (var row = 0; row < rows; row++) {
      for (var col = 0; col < cols; col++) {
        var v = snr[row * cols + col]
        if (v === undefined || v === null || v < 0) continue
        var dx = (col - ccx) / radius
        var dy = (row - ccy) / radius
        if (dx * dx + dy * dy > 1) continue
        ctx.fillStyle = Qt.rgba(baseColor.r, baseColor.g, baseColor.b, 0.30)
        ctx.fillRect(ox + col * cw, oy + row * ch, cw + 0.5, ch + 0.5)
      }
    }

    // Elevation rings: 1/3 and 2/3 of the field of view (approx 30°/60°).
    ctx.lineWidth = 1
    ctx.strokeStyle = Qt.rgba(baseColor.r, baseColor.g, baseColor.b, 0.22)
    for (var band = 1; band <= 2; band++) {
      ctx.beginPath()
      ctx.arc(cxp, cyp, r * band / 3, 0, 2 * Math.PI)
      ctx.stroke()
    }
    // Crosshair.
    ctx.beginPath()
    ctx.moveTo(cxp - r, cyp); ctx.lineTo(cxp + r, cyp)
    ctx.moveTo(cxp, cyp - r); ctx.lineTo(cxp, cyp + r)
    ctx.stroke()

    // Disc outline.
    ctx.lineWidth = 1.5
    ctx.strokeStyle = Qt.rgba(baseColor.r, baseColor.g, baseColor.b, 0.45)
    ctx.beginPath()
    ctx.arc(cxp, cyp, r, 0, 2 * Math.PI)
    ctx.stroke()
  }
}
