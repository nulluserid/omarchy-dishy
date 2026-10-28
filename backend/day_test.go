package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkDayDaemon returns a daemon with the day store initialized for t0's date
// and a fresh state dir.
func mkDayDaemon(t *testing.T) *daemon {
	t.Helper()
	d := newDaemon(config{dir: t.TempDir()}, nil)
	d.initDay(t0)
	return d
}

// feedAt feeds one synthetic sample at the given wall time.
func feedAt(d *daemon, ts time.Time, downMbps float64, ping *float64, drop float64) {
	power := 13.5
	var pow *float64
	if downMbps >= 0 {
		pow = &power
	}
	d.feedSample(ts, downMbps, downMbps/10, ping, drop, pow)
}

func pf32(v float64) *float64 { return &v }

// TestDayMinuteAggregation checks one completed minute's math: idle floor,
// fastest-10% trim, ping median, drop and power means, and the sample count.
func TestDayMinuteAggregation(t *testing.T) {
	d := mkDayDaemon(t)
	start := time.Date(t0.Year(), t0.Month(), t0.Day(), 10, 0, 0, 0, t0.Location())

	// 20 samples: 5 idle (0.5 Mbps, dropped ping), 15 active (10×10, 5×20).
	for k := 0; k < 20; k++ {
		ts := start.Add(time.Duration(k) * time.Second)
		switch {
		case k < 5:
			feedAt(d, ts, 0.5, nil, 1.0) // offline second: no answered ping
		case k < 15:
			feedAt(d, ts, 10.0, pf32(20.0), 0)
		default:
			feedAt(d, ts, 20.0, pf32(30.0), 0)
		}
	}
	// Minute completes with the first sample of the next minute.
	d.feedSample(start.Add(60*time.Second), 0.5, 0.05, pf32(25), 0, nil)

	got := d.day.file.Minutes[600] // 10:00 local
	if got.N != 20 {
		t.Fatalf("N = %d, want 20", got.N)
	}
	// Active = 15; trim fastest 10% (1 sample): sorted actives keep 10×10 + 4×20.
	if got.DownTyp != 12.86 { // (10*10 + 4*20) / 14
		t.Errorf("DownTyp = %v, want 12.86", got.DownTyp)
	}
	// Up mean over the active set (untrimmed): (10×1.0 + 5×2.0) / 15.
	if got.Up != 1.33 {
		t.Errorf("Up = %v, want 1.33", got.Up)
	}
	// Ping median over answered pings only: ten 20s, five 30s → 20.
	if got.Ping != 20 {
		t.Errorf("Ping = %v, want 20", got.Ping)
	}
	// Drop mean over ALL samples: 5 dropped seconds out of 20.
	if got.Drop != 0.25 {
		t.Errorf("Drop = %v, want 0.25", got.Drop)
	}
	// Power mean: every sample carries 13.5 (a dropped ping still draws power).
	if got.PowerW != 13.5 {
		t.Errorf("PowerW = %v, want 13.5", got.PowerW)
	}
}

// TestDayPartialMinuteSnapshot: the in-progress minute is written with the
// lower partial threshold so the Today chart moves before :seconds elapse.
func TestDayPartialMinuteSnapshot(t *testing.T) {
	d := mkDayDaemon(t)
	start := time.Date(t0.Year(), t0.Month(), t0.Day(), 10, 1, 0, 0, t0.Location())
	for k := 0; k < 5; k++ {
		feedAt(d, start.Add(time.Duration(k)*time.Second), 12.0, pf32(21), 0)
	}
	d.persistDay(start.Add(5*time.Second), false) // unforced: first write always passes the throttle

	b, err := os.ReadFile(filepath.Join(d.cfg.dir, dayFileName(d.day.file.Date)))
	if err != nil {
		t.Fatalf("day file not written: %v", err)
	}
	var df dayFile
	if err := json.Unmarshal(b, &df); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := df.Minutes[601]
	if got.DownTyp != 12 {
		t.Errorf("partial DownTyp = %v, want 12", got.DownTyp)
	}
	if df.LastTs != start.Add(4*time.Second).Unix() {
		t.Errorf("LastTs = %d, want %d", df.LastTs, start.Add(4*time.Second).Unix())
	}
}

// TestDayWatermarkRestart: a restart reloads today's file and re-ingests the
// same ring without double-counting (file.lastTs is the watermark).
func TestDayWatermarkRestart(t *testing.T) {
	d := mkDayDaemon(t)
	start := time.Date(t0.Year(), t0.Month(), t0.Day(), 10, 0, 0, 0, t0.Location())
	for k := 0; k < 30; k++ {
		feedAt(d, start.Add(time.Duration(k)*time.Second), 10.0, pf32(20), 0)
	}
	d.persistDay(start.Add(30*time.Second), true) // production entry: partial snapshot + forced write

	d2 := newDaemon(config{dir: d.cfg.dir}, nil)
	d2.initDay(start.Add(30 * time.Second))
	// Overlap: same samples again.
	for k := 0; k < 30; k++ {
		feedAt(d2, start.Add(time.Duration(k)*time.Second), 10.0, pf32(20), 0)
	}
	if got := d2.day.file.Minutes[600]; got.N != 30 {
		t.Fatalf("N after overlap re-feed = %d, want 30", got.N)
	}
	// Fresh sample advances.
	feedAt(d2, start.Add(30*time.Second), 10.0, pf32(20), 0)
	if d2.day.file.LastTs != start.Add(30*time.Second).Unix() {
		t.Errorf("LastTs = %d, want %d", d2.day.file.LastTs, start.Add(30*time.Second).Unix())
	}
}

// TestDayRollAndPrune: a next-day sample rolls the file, writes the old day,
// and prunes beyond the 30-day window.
func TestDayRollAndPrune(t *testing.T) {
	d := mkDayDaemon(t)
	start := time.Date(t0.Year(), t0.Month(), t0.Day(), 10, 0, 0, 0, t0.Location())
	feedAt(d, start, 10.0, pf32(20), 0)

	// A 35-day-old file must fall outside the retention window.
	old := filepath.Join(d.cfg.dir, dayFileName(t0.AddDate(0, 0, -35).Format("2006-01-02")))
	if err := os.WriteFile(old, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	tomorrow := start.AddDate(0, 0, 1)
	d.feedSample(tomorrow, 10.0, 1.0, pf32(20), 0, nil)

	if _, err := os.Stat(filepath.Join(d.cfg.dir, dayFileName(start.Format("2006-01-02")))); err != nil {
		t.Errorf("old day file not written on roll: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("stale day file survived pruning")
	}
	if d.day.file.Date != tomorrow.Format("2006-01-02") {
		t.Errorf("rolled file date = %s", d.day.file.Date)
	}
}

// TestDayOffNetworkGate: consume() must not feed the day store while the
// link marker says off-network (contract parity with daily.json).
func TestDayOffNetworkGate(t *testing.T) {
	d := newDaemon(config{dir: t.TempDir()}, nil)
	d.initDay(t0)
	d.startWall = t0.Add(-time.Hour)
	d.link.OnStarlink = false

	h := mkHist(ringSize-1, []float32{20}, []float32{0})
	d.ingest(h, 100, true, t0)
	if d.day.file.LastTs != 0 {
		t.Fatalf("off-network samples reached the day store (LastTs=%d)", d.day.file.LastTs)
	}

	d.link.OnStarlink = true
	d.ingest(mkHist(ringSize, []float32{20}, []float32{0}), 101, true, t0.Add(time.Second))
	if d.day.file.LastTs == 0 {
		t.Fatalf("on-network delta did not reach the day store")
	}
}

// TestTypicalDownNow: ring-tail typical mirrors the day-store math; stale or
// too-idle windows yield nil.
func TestTypicalDownNow(t *testing.T) {
	d := newDaemon(config{}, nil)
	d.resetArrays()
	// Newest 60 slots: 30 idle (0.5), 25 at 10, 5 at 50 (newest last).
	for k := 0; k < 60; k++ {
		slot := (ringSize - 60 + k) % ringSize
		var v float64
		switch {
		case k < 30:
			v = 0.5
		case k < 55:
			v = 10.0
		default:
			v = 50.0
		}
		d.arrays[aDown][slot] = pf(v, 2)
		d.arrays[aUp][slot] = pf(v/10, 2)
	}
	d.lastNewest = ringSize - 1
	d.lastSampleWall = t0

	got := typicalDownNow(&d.arrays, d.lastNewest, d.lastSampleWall, t0)
	if got == nil {
		t.Fatal("typical = nil, want a value")
	}
	// Active = 30, trim 3 fastest: 25×10 + 2×50 over 27 = 12.96.
	if *got != 12.96 {
		t.Errorf("typical = %v, want 12.96", *got)
	}

	// Stale anchor: no ingest for 2 minutes → nil.
	if got := typicalDownNow(&d.arrays, d.lastNewest, t0.Add(-2*time.Minute), t0); got != nil {
		t.Errorf("stale typical = %v, want nil", *got)
	}

	// All idle → nil.
	for k := 0; k < 60; k++ {
		slot := (ringSize - 60 + k) % ringSize
		d.arrays[aDown][slot] = pf(0.5, 2)
	}
	if got := typicalDownNow(&d.arrays, d.lastNewest, d.lastSampleWall, t0); got != nil {
		t.Errorf("idle typical = %v, want nil", *got)
	}
}

// TestNormalizeTarget covers the `set host` acceptance rules.
func TestNormalizeTarget(t *testing.T) {
	ok := map[string]string{
		"192.168.100.1":      "192.168.100.1:9200",
		"192.168.100.1:9200": "192.168.100.1:9200",
		" dish.local ":       "dish.local:9200",
		"[::1]:1234":         "[::1]:1234",
	}
	for in, want := range ok {
		got, err := normalizeTarget(in)
		if err != nil || got != want {
			t.Errorf("normalizeTarget(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"", "   ", ":9200", "h:", "h:0", "h:99999", "h:abc", "h x", "h\tx", string(make([]byte, 254))}
	for _, in := range bad {
		if got, err := normalizeTarget(in); err == nil {
			t.Errorf("normalizeTarget(%q) = %q, want error", in, got)
		}
	}
}

// TestTargetConfigRoundTrip: persisted config survives a load.
func TestTargetConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := loadTargetConfig(dir); ok {
		t.Fatal("missing config reported present")
	}
	if err := writeTargetConfig(dir, "192.168.100.7:9200"); err != nil {
		t.Fatal(err)
	}
	got, ok := loadTargetConfig(dir)
	if !ok || got != "192.168.100.7:9200" {
		t.Fatalf("round trip = %q, %v", got, ok)
	}
	// Corrupt config reads as absent.
	if err := os.WriteFile(filepath.Join(dir, configName), []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadTargetConfig(dir); ok {
		t.Fatal("corrupt config reported present")
	}
}
