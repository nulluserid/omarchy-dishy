package main

import (
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	dish "starlinkd/gen/spacex_api/device/dish"
	udc "starlinkd/gen/spacex_api/satellites/network/ut_disablement_codes"
)

// Rune caps for strings that arrive from an untrusted wire — the dish's
// answers and the egress lookup — and land in a state file the QML parses on
// the GUI thread (the bar re-parses status.json every 2 s, panel closed), an
// events.jsonl line, or a notify-send argument. For scale: a real device id
// is 26 runes, hw "mini1_panda_prod1" (17), sw "2026.09.08.mr86422" (18).
const (
	maxDishStrRunes    = 64 // device id, hardware and software version
	maxRPCErrorRunes   = 200
	maxGeoIPRunes      = 64  // a refused address, quoted in the lookup's error; a published one is a parsed IPv4
	maxGeoTextRunes    = 128 // city, region, carrier org
	maxGeoCountryRunes = 16
	maxGeoASNRunes     = 32
)

// clip bounds such a string at max runes and drops characters that render as
// nothing and only change how the text around them is read — control
// characters (Unicode Cc: newlines, ESC, DEL, …) and format characters (Cf:
// the bidi overrides U+202A–U+202E and U+2066–U+2069 that can make a label
// read backwards, zero-width marks) — plus invalid UTF-8, never splitting a
// rune. It truncates rather than rejects — the surrounding answer still
// stands, the string is just hostile-sized. (The obstruction map and the
// egress address are the exceptions that are rejected whole: a truncated sky
// would be a lie, and so would a clipped address.)
func clip(s string, max int) string {
	invisible := func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }
	if len(s) <= max && utf8.ValidString(s) && !strings.ContainsFunc(s, invisible) {
		return s // short and clean, like every real value
	}
	var b strings.Builder
	b.Grow(min(len(s), max*utf8.UTFMax))
	n := 0
	for i, r := range s {
		if n == max {
			break
		}
		if r == utf8.RuneError {
			if _, size := utf8.DecodeRuneInString(s[i:]); size == 1 {
				continue // an invalid byte, not an encoded U+FFFD
			}
		}
		if invisible(r) {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// round rounds v to n decimal places. Non-finite values (NaN/Inf can enter
// through hostile dish responses) become 0 so no marshaled document can
// carry them — encoding/json refuses the whole document otherwise.
func round(v float64, n int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	p := math.Pow10(n)
	r := math.Round(v*p) / p
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return 0
	}
	return r
}

// percentile returns the nearest-rank p-th percentile of an ascending slice.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// latStats computes p50/p95/mean (0.1 precision) over non-dropped samples.
func latStats(series []*float64, max int) (p50, p95, mean float64) {
	var s []float64
	for i := len(series) - 1; i >= 0 && len(s) < max; i-- {
		if series[i] != nil {
			s = append(s, *series[i])
		}
	}
	if len(s) == 0 {
		return 0, 0, 0
	}
	sort.Float64s(s)
	var sum float64
	for _, v := range s {
		sum += v
	}
	return round(percentile(s, 50), 1), round(percentile(s, 95), 1), round(sum/float64(len(s)), 1)
}

// dropMean is the mean drop rate over the newest n samples (0.0001 precision).
func dropMean(series []*float64, n int) float64 {
	var sum float64
	var cnt int
	for i := len(series) - 1; i >= 0 && cnt < n; i-- {
		if series[i] != nil {
			sum += *series[i]
			cnt++
		}
	}
	if cnt == 0 {
		return 0
	}
	return round(sum/float64(cnt), 4)
}

// snakeCase converts a protobuf Go field name to its snake_case id:
// "InstallPending" -> "install_pending".
func snakeCase(name string) string {
	var b strings.Builder
	rs := []rune(name)
	for i, r := range rs {
		if unicode.IsUpper(r) {
			if i > 0 && !unicode.IsUpper(rs[i-1]) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// titleCase renders a snake_case id as a human label with the first letter
// capitalized: "install_pending" -> "Install pending" (per CONTRACT.md).
func titleCase(id string) string {
	rs := []rune(id)
	for i, r := range rs {
		if r == '_' {
			rs[i] = ' '
			continue
		}
		if i == 0 {
			rs[i] = unicode.ToUpper(r)
		}
	}
	return string(rs)
}

// alert is one entry of status.json "alerts".
type alert struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// decodeAlerts returns every TRUE dish alert, in a stable order.
func decodeAlerts(a *dish.DishAlerts) []alert {
	out := []alert{}
	if a == nil {
		return out
	}
	fields := []struct {
		name string
		v    bool
	}{
		{"motorsStuck", a.GetMotorsStuck()},
		{"thermalShutdown", a.GetThermalShutdown()},
		{"thermalThrottle", a.GetThermalThrottle()},
		{"unexpectedLocation", a.GetUnexpectedLocation()},
		{"mastNotNearVertical", a.GetMastNotNearVertical()},
		{"slowEthernetSpeeds", a.GetSlowEthernetSpeeds()},
		{"SlowEthernetSpeeds_100", a.GetSlowEthernetSpeeds_100()},
		{"installPending", a.GetInstallPending()},
		{"isHeating", a.GetIsHeating()},
		{"powerSupplyThermalThrottle", a.GetPowerSupplyThermalThrottle()},
		{"isPowerSaveIdle", a.GetIsPowerSaveIdle()},
		{"dbfTelemStale", a.GetDbfTelemStale()},
		{"lowMotorCurrent", a.GetLowMotorCurrent()},
		{"lowerSignalThanPredicted", a.GetLowerSignalThanPredicted()},
		{"obstructionMapReset", a.GetObstructionMapReset()},
		{"dishWaterDetected", a.GetDishWaterDetected()},
		{"routerWaterDetected", a.GetRouterWaterDetected()},
		{"upsuRouterPortSlow", a.GetUpsuRouterPortSlow()},
		{"noEthernetLink", a.GetNoEthernetLink()},
	}
	for _, f := range fields {
		if f.v {
			id := snakeCase(f.name)
			out = append(out, alert{ID: id, Label: titleCase(id)})
		}
	}
	return out
}

// deriveState maps a status response onto the contract state enum.
// The captures only document ONLINE; the remaining branches follow the
// fields this firmware reports in each situation (see CONTRACT.md).
func deriveState(st *dish.DishGetStatusResponse) (state, detail string) {
	if st == nil || st.GetDeviceInfo() == nil {
		return "NO_DATA", ""
	}
	detail = ""
	if st.GetSoftwareUpdateState() == dish.SoftwareUpdateState_REBOOT_REQUIRED || st.GetSwupdateRebootReady() {
		detail = "SOFTWARE_UPDATE_REBOOT_PENDING"
	} else if code := st.GetDisablementCode(); code != udc.UtDisablementCode_OKAY &&
		code != udc.UtDisablementCode_UNKNOWN_STATE {
		detail = code.String()
	}
	uptime := st.GetDeviceState().GetUptimeS()
	switch {
	case st.GetAlerts().GetThermalShutdown():
		return "THERMAL_SHUTDOWN", detail
	case st.GetPopPingLatencyMs() > 0:
		return "ONLINE", detail
	case uptime > 0 && uptime < 120:
		return "BOOTING", detail
	case !st.GetIsSnrAboveNoiseFloor():
		return "SEARCHING", detail
	default:
		return "NO_SIGNAL", detail
	}
}

// humanizeCause renders an outage cause enum name ("BOOTING") as prose.
func humanizeCause(cause string) string {
	return titleCase(strings.ToLower(cause))
}

// outageTrack is an open local outage.
type outageTrack struct {
	cause string
	start int64 // unix seconds
}

// hyst implements the obstruction_heavy notification hysteresis:
// fire at fraction > 0.01 sustained 60s; re-arm only after < 0.005 for 300s.
type hyst struct {
	fired  bool
	overS  float64
	underS float64
	alarmS float64
	rearmS float64
}

func newHyst(alarmS, rearmS float64) hyst {
	return hyst{alarmS: alarmS, rearmS: rearmS}
}

// step advances the hysteresis by dt seconds at the given fraction and
// reports whether obstruction_heavy should fire now.
func (h *hyst) step(fraction, dt float64) bool {
	switch {
	case fraction > 0.01:
		h.overS += dt
		h.underS = 0
		if !h.fired && h.overS >= h.alarmS {
			h.fired = true
			return true
		}
	case fraction < 0.005:
		h.underS += dt
		if h.fired && h.underS >= h.rearmS {
			h.fired = false
			h.overS = 0
		}
	}
	return false
}
