.pragma library

// Pure parsing and formatting helpers for the starlinkd status/hist files
// (backend/CONTRACT.md). Everything degrades to placeholders, never throws.

// Status colors that have no omarchy theme token (palette only defines
// accent/urgent/muted). Red/gray map to Color.urgent/Color.muted in QML.
var OK_HEX = "#7cb342"
var WARN_HEX = "#ffa000"

function parse(text) {
  try {
    var d = JSON.parse(String(text))
    return d && typeof d === "object" && !Array.isArray(d) ? d : null
  } catch (e) {
    return null
  }
}

function num(v, fallback) {
  // Number(null)/Number("") are 0 in JS — guard before coercion so nulls
  // stay null (graphs rely on that for gap preservation).
  if (v === null || v === undefined || v === "") return fallback === undefined ? null : fallback
  var n = Number(v)
  return isFinite(n) ? n : (fallback === undefined ? null : fallback)
}

function isStale(status, nowS) {
  if (!status) return true
  var ts = num(status.ts, 0)
  return ts <= 0 || nowS - ts > 10
}

function hasObstructionAlert(status) {
  var alerts = status && status.alerts
  if (!alerts || !alerts.length) return false
  for (var i = 0; i < alerts.length; i++) {
    var a = alerts[i] || {}
    if (String(a.id || "").indexOf("obstruct") >= 0
        || String(a.label || "").toLowerCase().indexOf("obstruct") >= 0) return true
  }
  return false
}

// good = ONLINE, warn = SEARCHING/BOOTING or obstruction alerts or
// reboot-required detail, bad = NO_SIGNAL/THERMAL/OFFLINE, none = NO_DATA.
function level(status) {
  if (!status) return "none"
  var st = String(status.state || "")
  var det = String(status.stateDetail || "")
  if (st === "ONLINE") {
    if (hasObstructionAlert(status)) return "warn"
    if (det.indexOf("REBOOT") >= 0) return "warn"
    return "good"
  }
  if (st === "SEARCHING" || st === "BOOTING") return "warn"
  if (st === "NO_SIGNAL" || st === "THERMAL_SHUTDOWN" || st === "OFFLINE") return "bad"
  return "none"
}

function titleCase(snake) {
  var s = String(snake || "").toLowerCase().replace(/_/g, " ").trim()
  return s.replace(/(^|\s)(\w)/g, function(m, p1, p2) { return p1 + p2.toUpperCase() })
}

function humanDur(s) {
  s = Math.max(0, Math.round(num(s, 0)))
  if (s < 90) return s + "s"
  if (s < 5400) return Math.floor(s / 60) + "m " + (s % 60) + "s"
  if (s < 172800) return Math.floor(s / 3600) + "h " + (Math.floor(s / 60) % 60) + "m"
  return Math.floor(s / 86400) + "d " + (Math.floor(s / 3600) % 24) + "h"
}

function agoText(startS, nowS) {
  return humanDur(num(nowS, Date.now() / 1000) - num(startS, 0)) + " ago"
}

function fmtMs(v) {
  var n = num(v)
  return n === null ? "—" : Math.round(n) + " ms"
}

function fmtMbps(v) {
  var n = num(v)
  return n === null ? "—" : (n >= 100 ? n.toFixed(0) : n.toFixed(1)) + " Mbps"
}

function fmtW(v) {
  var n = num(v)
  return n === null ? "—" : n.toFixed(1) + " W"
}

function fmtGB(v) {
  var n = num(v)
  if (n === null) return "—"
  return (n >= 100 ? n.toFixed(0) : n.toFixed(1)) + " GB"
}

function fmtPct(v, decimals) {
  var n = num(v)
  return n === null ? "—" : n.toFixed(decimals === undefined ? 1 : decimals) + "%"
}

// Nearest-rank percentile over non-null entries; null when no data.
function percentile(values, p) {
  var vals = []
  if (values) for (var i = 0; i < values.length; i++) {
    var n = num(values[i])
    if (n !== null) vals.push(n)
  }
  if (!vals.length) return null
  vals.sort(function(a, b) { return a - b })
  var idx = Math.min(vals.length - 1, Math.max(0, Math.ceil(p / 100 * vals.length) - 1))
  return vals[idx]
}

// Average per pixel bucket of a series (newest last), null buckets preserved
// as gaps so dropped pings keep breaking the line.
function decimate(values, buckets) {
  buckets = Math.max(1, Math.floor(num(buckets, 1)))
  if (!values || values.length <= buckets) return values || []
  var out = []
  var size = values.length / buckets
  for (var b = 0; b < buckets; b++) {
    var start = Math.floor(b * size)
    var end = Math.min(values.length, Math.floor((b + 1) * size))
    var sum = 0, count = 0
    for (var i = start; i < end; i++) {
      var n = num(values[i])
      if (n !== null) { sum += n; count++ }
    }
    out.push(count ? sum / count : null)
  }
  return out
}

// One-line bar tooltip summary.
function summary(status) {
  if (!status) return "Starlink — waiting for starlinkd"
  var st = String(status.state || "NO_DATA")
  if (st === "NO_DATA" || st === "") return "Starlink — waiting for starlinkd"
  var live = status.live || {}
  var parts = [titleCase(st)]
  if (live.latencyMs !== null && live.latencyMs !== undefined) parts.push(fmtMs(live.latencyMs))
  if (live.downMbps !== null && live.downMbps !== undefined)
    parts.push("↓" + fmtMbps(live.downMbps) + " ↑" + fmtMbps(live.upMbps))
  if (live.powerW !== null && live.powerW !== undefined) parts.push(fmtW(live.powerW))
  return parts.join("  ·  ")
}
