package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const dayMinutesPerDay = 1440

// The file shapes below mirror starlinkd's (backend/day.go,
// backend/persist.go), so the daemon loads the seed as its own earlier work;
// its first tick rewrites every file in its own words.

type dayMinute struct {
	DownTyp float64 `json:"downTyp,omitempty"` // typical active downlink, Mbps
	Ping    float64 `json:"ping,omitempty"`    // median latency of answered pings, ms
	Up      float64 `json:"up,omitempty"`      // mean uplink over active samples, Mbps
	Drop    float64 `json:"drop,omitempty"`    // mean drop rate, 0..1
	PowerW  float64 `json:"powerW,omitempty"`  // mean input power, W
	N       int     `json:"n,omitempty"`       // real samples in the minute
}

type dayFile struct {
	Date    string      `json:"date"`
	LastTs  int64       `json:"lastTs"` // wall unix of the newest aggregated sample
	Minutes []dayMinute `json:"minutes"`
}

type dailyAllTime struct {
	DownBytes int64 `json:"downBytes"`
	UpBytes   int64 `json:"upBytes"`
}

type dailyFile struct {
	Date      string       `json:"date"`
	DownBytes int64        `json:"downBytes"`
	UpBytes   int64        `json:"upBytes"`
	OnlineS   int64        `json:"onlineS"`
	TotalS    int64        `json:"totalS"`
	Outages   int64        `json:"outages"`
	LastSeen  int64        `json:"lastSeen,omitempty"`
	AllTime   dailyAllTime `json:"allTime"`
}

// daemonConfig is the daemon's config.json (backend/control.go).
type daemonConfig struct {
	Target string `json:"target"`
}

type event struct {
	Ts    int64  `json:"ts"`
	Type  string `json:"type"`
	Cause string `json:"cause,omitempty"`
	DurS  int64  `json:"durS,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
}

// runSeed writes what a starlinkd that had been running all day would have by
// now: today's minute history, the dish's counters, the events of today's
// outages and a config.json whose target is this fake dish. dir must be empty
// — a real daemon's state directory never is.
func runSeed(dir, name, target string) error {
	d, err := newFakeDish(name, time.Now())
	if err != nil {
		return err
	}
	switch entries, err := os.ReadDir(dir); {
	case err == nil && len(entries) > 0:
		return fmt.Errorf("%s is not empty: the seed never writes over existing state", dir)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}

	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	date := now.Format("2006-01-02")
	// The daemon's first tick backfills the newest 900 s from the dish's ring
	// and skips samples at or before lastTs (CONTRACT.md "day-*.json"), and
	// it books no bytes for samples older than itself. So the seed stops one
	// ring-length short of now and leaves that stretch to the backfill — but
	// counts its bytes, which the daemon will not.
	m0 := now.Truncate(time.Minute).Add(-14 * time.Minute)
	df := dayFile{
		Date:    date,
		LastTs:  max(m0.Unix()-1, midnight.Unix()-1),
		Minutes: make([]dayMinute, dayMinutesPerDay),
	}

	var downBytes, upBytes, populated int64
	for min := midnight; min.Before(m0); min = min.Add(time.Minute) {
		idle := !active(min)
		var downs, ups, pings, drops, pows []float64
		for sec := min.Unix(); sec < min.Unix()+60; sec++ {
			s := d.sample(sec, idle)
			downBytes += int64(s.downBps / 8)
			upBytes += int64(s.upBps / 8)
			// The daemon rounds each sample before it aggregates; same here.
			downs = append(downs, round(float64(s.downBps)/1e6, 2))
			ups = append(ups, round(float64(s.upBps)/1e6, 2))
			drops = append(drops, float64(s.drop))
			pows = append(pows, round(float64(s.powerW), 1))
			if s.latMs > 0 {
				pings = append(pings, round(float64(s.latMs), 2))
			}
		}
		df.Minutes[min.Hour()*60+min.Minute()] = aggregate(downs, ups, pings, drops, pows)
		populated++
	}
	for sec := max(m0.Unix(), midnight.Unix()); sec <= now.Unix(); sec++ {
		s := d.sample(sec, false)
		downBytes += int64(s.downBps / 8)
		upBytes += int64(s.upBps / 8)
	}

	var outages, offline int64
	var events []event
	today := func(start, end time.Time) int64 { // seconds of [start, end) inside today so far
		if start.Before(midnight) {
			start = midnight
		}
		if end.After(now) {
			end = now
		}
		return max(0, int64(end.Sub(start).Seconds()))
	}
	for _, o := range d.sc.outages {
		start, end := d.anchor.Add(-o.ago), d.anchor.Add(-o.ago).Add(o.dur)
		if secs := today(start, end); secs > 0 {
			outages++
			offline += secs
			if !start.Before(midnight) {
				events = append(events, event{Ts: start.Unix(), Type: "outage_start", Cause: o.cause.String()})
			}
			if !end.After(now) {
				events = append(events, event{Ts: end.Unix(), Type: "outage_end", Cause: o.cause.String(), DurS: int64(o.dur / time.Second)})
			}
		}
	}
	if !d.downSince.IsZero() {
		// Searching is not an outage in the daemon's book, only time offline.
		offline += today(d.downSince, now)
		events = append(events, event{Ts: d.downSince.Unix(), Type: "state_change", From: "ONLINE", To: "SEARCHING"})
	}
	var buf bytes.Buffer
	for _, ev := range events {
		if err := json.NewEncoder(&buf).Encode(ev); err != nil {
			return err
		}
	}

	totalS := int64(now.Sub(midnight).Seconds())
	daily := dailyFile{
		Date: date, DownBytes: downBytes, UpBytes: upBytes,
		OnlineS: totalS - offline, TotalS: totalS, Outages: outages, LastSeen: now.Unix(),
		AllTime: dailyAllTime{DownBytes: 1_931_274_000_000, UpBytes: 121_330_000_000},
	}

	part := filepath.Join(dir, "dishes", dishDirName(dishID))
	for _, p := range []string{dir, filepath.Join(dir, "dishes"), part} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return err
		}
		// MkdirAll leaves an existing directory's mode alone.
		if err := os.Chmod(p, 0o700); err != nil {
			return err
		}
	}
	put := func(path string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return writePrivate(path, b)
	}
	if err := put(filepath.Join(part, "daily.json"), daily); err != nil {
		return err
	}
	if err := put(filepath.Join(part, dayFileName(date)), df); err != nil {
		return err
	}
	// The target in config.json is what the Dishy widget compares its "Dish
	// address" setting with: once the two match the widget stops pushing the
	// real dish's address to the demo daemon.
	if err := put(filepath.Join(dir, "config.json"), daemonConfig{Target: target}); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(dir, "events.jsonl"), buf.Bytes()); err != nil {
		return err
	}

	avail := 100.0
	if totalS > 0 {
		avail = 100 * float64(daily.OnlineS) / float64(totalS)
	}
	_, err = fmt.Printf("seeded %s (%s): %d minutes of today, down %.1f GB up %.1f GB, availability %.2f%%, %d outages\n",
		part, name, populated, float64(downBytes)/1e9, float64(upBytes)/1e9, avail, outages)
	return err
}

// writePrivate creates path with mode 0600 whatever the umask.
func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// activityByHour is the share of a day's four-minute slots that carry real
// traffic, by hour: a house asleep at night, busy around noon and in the
// evening. Idle slots still record the connection's background traffic.
var activityByHour = [24]float64{
	0.007, 0.007, 0.007, 0.007, 0.007, 0.013, 0.04, 0.09, 0.10, 0.065, 0.05, 0.05,
	0.12, 0.06, 0.05, 0.05, 0.065, 0.09, 0.10, 0.12, 0.12, 0.10, 0.065, 0.026,
}

func active(min time.Time) bool {
	m := min.Hour()*60 + min.Minute()
	if unit(mix(int64(m/4), saltActive)) < activityByHour[min.Hour()] {
		return true
	}
	// A busy stretch may run past its slot, so the bumps are not all four
	// minutes long.
	return unit(mix(int64(m/4-1), saltActive)) < activityByHour[min.Hour()] &&
		unit(mix(int64(m), saltRun)) < 0.5
}

// aggregate folds one minute the way the daemon's finalizeMinute does: an
// "active" sample clears 1 Mbps, the fastest 10 % of those are trimmed, and a
// typical figure needs ten of them.
func aggregate(downs, ups, pings, drops, pows []float64) dayMinute {
	out := dayMinute{N: len(downs)}
	var actDowns, actUps []float64
	for i, v := range downs {
		if v >= 1 {
			actDowns = append(actDowns, v)
			actUps = append(actUps, ups[i])
		}
	}
	if len(actDowns) >= 10 {
		slices.Sort(actDowns)
		kept := actDowns[:len(actDowns)-len(actDowns)/10]
		out.DownTyp = round(mean(kept), 2)
		out.Up = round(mean(actUps), 2)
	}
	if len(pings) > 0 {
		slices.Sort(pings)
		out.Ping = round(percentile(pings, 50), 1)
	}
	out.Drop = round(mean(drops), 4)
	out.PowerW = round(mean(pows), 1)
	return out
}

func mean(v []float64) float64 {
	var sum float64
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

// dishDirName mirrors starlinkd's partition name rule (backend/dish.go).
func dishDirName(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			r = '-'
		}
		if b.Len() >= 64 {
			break
		}
		b.WriteRune(r)
	}
	if name := b.String(); name != "" && name != "." && name != ".." {
		return name
	}
	return "unknown"
}

func dayFileName(date string) string {
	return "day-" + strings.ReplaceAll(date, "-", "") + ".json"
}

// round and percentile mirror the daemon's (backend/stats.go).

func round(v float64, n int) float64 {
	p := math.Pow10(n)
	return math.Round(v*p) / p
}

// percentile is the nearest-rank p-th percentile of an ascending slice.
func percentile(sorted []float64, p float64) float64 {
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}
