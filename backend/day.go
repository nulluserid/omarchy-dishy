// Day history store: per-minute per-day aggregates over the dish's 1 Hz
// samples, one JSON file per local day (day-YYYYMMDD.json, CONTRACT.md v3).
//
// Design notes:
//   - Aggregation happens inside consume() as real samples are applied; every
//     sample's wall time is derived from the ingest cursor (newest - offset),
//     so a daemon restart backfills the ring idempotently: file.lastTs skips
//     anything already aggregated.
//   - "Typical" down/up follow the incumbent's published approach: samples
//     below the idle floor are not "using the link", and the fastest 10% of
//     the active ones are trimmed so a short spike cannot define a minute.
//   - Writes are atomic and throttled (>= dayWriteInterval apart, or forced
//     on minute roll / -once); at most dayWriteInterval of samples are lost
//     to a SIGKILL, and the watermark heals the overlap on restart.
//   - Files outside the retention window are pruned by name at load and roll.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	dayKeep             = 30               // days retained, rolling (owner decision)
	dayIdleFloorMbps    = 1.0              // below this the link is idle, not "in use"
	dayMinActive        = 10               // active samples for a completed minute
	dayMinActivePartial = 4                // active samples for the in-progress minute
	dayWriteInterval    = 30 * time.Second // max write cadence for day-*.json
	dayMinutesPerDay    = 1440
)

// dayMinute is one minute-of-day bucket. Zero value = no data; omitempty
// keeps the sparse daytime-hours file small.
type dayMinute struct {
	DownTyp float64 `json:"downTyp,omitempty"` // typical active downlink, Mbps
	Ping    float64 `json:"ping,omitempty"`    // median latency of answered pings, ms
	Up      float64 `json:"up,omitempty"`      // mean uplink over active samples, Mbps
	Drop    float64 `json:"drop,omitempty"`    // mean drop rate, 0..1
	PowerW  float64 `json:"powerW,omitempty"`  // mean input power, W
	N       int     `json:"n,omitempty"`       // real (non-phantom) samples in the minute
}

// dayFile is one local day, column-light and QML-friendly: index by
// minute-of-day (0 = local midnight). LastTs is the aggregation watermark.
type dayFile struct {
	Date    string      `json:"date"`    // 2006-01-02, local
	LastTs  int64       `json:"lastTs"`  // wall unix of the newest aggregated sample
	Minutes []dayMinute `json:"minutes"` // len 1440, zero value = no data
}

func newDayFile(date string) dayFile {
	return dayFile{Date: date, Minutes: make([]dayMinute, dayMinutesPerDay)}
}

// dayAccum carries the in-memory aggregation state. Touched only from the
// tick loop, like every other daemon field.
type dayAccum struct {
	file      dayFile
	minute    int       // minute-of-day of the in-progress bucket, -1 = none
	downs     []float64 // parallel per-sample slices of the open bucket
	ups       []float64
	pings     []float64 // answered pings only
	pingIdx   []int     // index into the parallel sample set, for shared "active" selection
	drops     []float64
	pows      []float64
	dirty     bool // new samples since the last write
	rolled    bool // a minute completed since the last write (force write)
	lastWrite time.Time
}

func (d *daemon) initDay(now time.Time) {
	date := now.Format("2006-01-02")
	d.day.file = newDayFile(date)
	d.day.minute = -1
	d.loadDay(now)
	d.pruneDays(now)
}

// loadDay restores today's aggregates; a file dated other than today is
// ignored for content (but still seen by pruneDays). Corrupt starts fresh.
func (d *daemon) loadDay(now time.Time) {
	date := now.Format("2006-01-02")
	b, err := os.ReadFile(filepath.Join(d.cfg.dir, dayFileName(date)))
	if err != nil {
		return
	}
	var df dayFile
	if err := json.Unmarshal(b, &df); err != nil || df.Date != date || len(df.Minutes) != dayMinutesPerDay {
		d.vlog("day %s: unreadable (starting fresh)", date)
		return
	}
	d.day.file = df
}

func dayFileName(date string) string {
	return "day-" + strings.ReplaceAll(date, "-", "") + ".json"
}

// pruneDays removes day files older than the retention window.
func (d *daemon) pruneDays(now time.Time) {
	entries, err := os.ReadDir(d.cfg.dir)
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -(dayKeep - 1)).Format("2006-01-02")
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "day-") || !strings.HasSuffix(name, ".json") || len(name) != len("day-20060102.json") {
			continue
		}
		raw := name[len("day-") : len(name)-len(".json")]
		date, err := time.Parse("20060102", raw)
		if err != nil {
			continue
		}
		if date.Format("2006-01-02") < cutoff {
			if err := os.Remove(filepath.Join(d.cfg.dir, name)); err != nil {
				d.vlog("prune %s: %v", name, err)
			}
		}
	}
}

// feedSample aggregates one real (non-phantom) sample with wall time ts.
// Order is chronological (consume applies oldest first), which is what the
// minute-rollover logic relies on. ts may predate the daemon start: the dish
// kept measuring while we were off, and file.lastTs keeps restart backfill
// from double-counting.
func (d *daemon) feedSample(ts time.Time, downMbps, upMbps float64, pingMs *float64, dropRate float64, powerW *float64) {
	if len(d.day.file.Minutes) != dayMinutesPerDay {
		d.day.file = newDayFile(ts.Format("2006-01-02")) // self-heal (tests, corrupt state)
	}
	date := ts.Format("2006-01-02")
	if date < d.day.file.Date {
		return // stale sample across a clock step; never rewrite history
	}
	if date > d.day.file.Date {
		d.rollDay(ts)
	}
	unix := ts.Unix()
	if unix <= d.day.file.LastTs {
		return // already aggregated (restart backfill overlap)
	}
	d.day.file.LastTs = unix

	m := ts.Hour()*60 + ts.Minute()
	if m != d.day.minute {
		if d.day.minute >= 0 {
			d.finalizeMinute(false) // completes the previous bucket
		}
		d.day.minute = m
		d.day.downs = d.day.downs[:0]
		d.day.ups = d.day.ups[:0]
		d.day.pings = d.day.pings[:0]
		d.day.pingIdx = d.day.pingIdx[:0]
		d.day.drops = d.day.drops[:0]
		d.day.pows = d.day.pows[:0]
	}

	d.day.downs = append(d.day.downs, downMbps)
	d.day.ups = append(d.day.ups, upMbps)
	d.day.drops = append(d.day.drops, dropRate)
	if pingMs != nil {
		d.day.pings = append(d.day.pings, *pingMs)
		d.day.pingIdx = append(d.day.pingIdx, len(d.day.downs)-1)
	}
	if powerW != nil {
		d.day.pows = append(d.day.pows, *powerW)
	}
	d.day.dirty = true
}

// finalizeMinute folds the open bucket into file.Minutes. partial marks the
// in-progress minute (lower active-sample bar, so the live chart keeps up).
func (d *daemon) finalizeMinute(partial bool) {
	m, acc := d.day.minute, &d.day
	if m < 0 || m >= dayMinutesPerDay {
		return
	}
	n := len(acc.downs)
	if n == 0 {
		return
	}
	need := dayMinActive
	if partial {
		need = dayMinActivePartial
	}

	out := dayMinute{N: n}

	// Active = samples whose downlink clears the idle floor. Typical down is
	// their mean after trimming the fastest 10% (ceil), the incumbent's
	// "typical active throughput" approach; up is their plain mean.
	var actDowns, actUps []float64
	for i, v := range acc.downs {
		if v >= dayIdleFloorMbps {
			actDowns = append(actDowns, v)
			actUps = append(actUps, acc.ups[i])
		}
	}
	if len(actDowns) >= need {
		sort.Float64s(actDowns)
		trim := len(actDowns) / 10
		kept := actDowns[:len(actDowns)-trim]
		var sum float64
		for _, v := range kept {
			sum += v
		}
		out.DownTyp = round(sum/float64(len(kept)), 2)
		var usum float64
		for _, v := range actUps {
			usum += v
		}
		out.Up = round(usum/float64(len(actUps)), 2)
	}

	if len(acc.pings) > 0 {
		sorted := append([]float64(nil), acc.pings...)
		sort.Float64s(sorted)
		out.Ping = round(percentile(sorted, 50), 1)
	}

	var dsum float64
	for _, v := range acc.drops {
		dsum += v
	}
	out.Drop = round(dsum/float64(n), 4)

	if len(acc.pows) > 0 {
		var psum float64
		for _, v := range acc.pows {
			psum += v
		}
		out.PowerW = round(psum/float64(len(acc.pows)), 1)
	}

	d.day.file.Minutes[m] = out
	d.day.rolled = !partial // only a completed minute forces the next write
}

// rollDay closes out the previous local day and starts a new file.
func (d *daemon) rollDay(now time.Time) {
	if d.day.minute >= 0 {
		d.finalizeMinute(true)
		d.day.minute = -1
	}
	if d.day.dirty {
		d.writeDay(now, true)
	}
	d.day.file = newDayFile(now.Format("2006-01-02"))
	d.day.dirty = false
	d.pruneDays(now)
}

// writeDay persists the aggregates (atomic, throttled). force skips the
// throttle: minute rolls and -once runs always write.
func (d *daemon) writeDay(now time.Time, force bool) {
	if !d.day.dirty {
		return
	}
	if !force && now.Sub(d.day.lastWrite) < dayWriteInterval {
		return
	}
	d.day.lastWrite = now
	d.day.dirty = false
	d.day.rolled = false
	if err := atomicWrite(filepath.Join(d.cfg.dir, dayFileName(d.day.file.Date)), marshalCompact(d.day.file)); err != nil {
		d.vlog("write %s: %v", dayFileName(d.day.file.Date), err)
	}
}

// persistDay is called from persist(): snapshot the in-progress minute into
// the file (finalizeMinute recomputes the same bucket from the still-open
// accumulators until the minute completes) and write when due. The 30 s
// throttle bounds the cost; a completed minute forces an immediate write.
func (d *daemon) persistDay(now time.Time, force bool) {
	if d.day.minute >= 0 {
		d.finalizeMinute(true)
	}
	d.writeDay(now, force)
}

// typicalDownNow computes the last-minute typical active downlink straight
// from the ring: the same idle floor and fastest-10% trim as the day store,
// so the bar readout, the panel stat, and the day chart agree. Returns nil
// when fewer than dayMinActivePartial active samples exist in the window.
func typicalDownNow(arrays *[arrayCount][]*float64, newest int, lastSampleWall time.Time, now time.Time) *float64 {
	if lastSampleWall.IsZero() {
		return nil
	}
	var actDowns, actUps []float64
	for k := 0; k < 60 && k <= newest; k++ {
		if lastSampleWall.Add(-time.Duration(k) * time.Second).Before(now.Add(-time.Minute)) {
			break
		}
		slot := ((newest-k)%ringSize + ringSize) % ringSize
		if v := arrays[aDown][slot]; v != nil && *v >= dayIdleFloorMbps {
			actDowns = append(actDowns, *v)
			if u := arrays[aUp][slot]; u != nil {
				actUps = append(actUps, *u)
			}
		}
	}
	if len(actDowns) < dayMinActivePartial {
		return nil
	}
	sort.Float64s(actDowns)
	trim := len(actDowns) / 10
	kept := actDowns[:len(actDowns)-trim]
	var sum float64
	for _, v := range kept {
		sum += v
	}
	v := round(sum/float64(len(kept)), 2)
	return &v
}
