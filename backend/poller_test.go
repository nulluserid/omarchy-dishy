package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dish "starlinkd/gen/spacex_api/device/dish"
)

// t0 is a fixed reference "now" for ingest tests (usage gating compares
// sample wall times against the daemon's startWall).
var t0 = time.Unix(1790000000, 0)

// mkHist builds a full 900-sample history response whose slot values cycle
// through the given latencies/drop rates; throughput is a constant 8000 B/s
// per sample (1000 bytes/s).
func mkHist(cur uint64, lat []float32, drop []float32) *dish.DishGetHistoryResponse {
	h := &dish.DishGetHistoryResponse{Current: cur}
	h.PopPingLatencyMs = make([]float32, ringSize)
	h.PopPingDropRate = make([]float32, ringSize)
	h.DownlinkThroughputBps = make([]float32, ringSize)
	h.UplinkThroughputBps = make([]float32, ringSize)
	h.PowerIn = make([]float32, ringSize)
	for i := 0; i < ringSize; i++ {
		h.PopPingLatencyMs[i] = lat[i%len(lat)]
		h.PopPingDropRate[i] = drop[i%len(drop)]
		h.DownlinkThroughputBps[i] = 8000
		h.UplinkThroughputBps[i] = 800
		h.PowerIn[i] = 13.5
	}
	return h
}

func TestIngestFirstTick(t *testing.T) {
	d := newDaemon(config{}, nil)
	d.startWall = t0.Add(-time.Hour)
	h := mkHist(5, []float32{20}, []float32{0})
	res := d.ingest(h, 100, true, t0)
	if res.samples != ringSize {
		t.Fatalf("first ingest samples = %d, want %d", res.samples, ringSize)
	}
	for _, slot := range []int{0, 5, 899} {
		if d.arrays[aLat][slot] == nil || *d.arrays[aLat][slot] != 20 {
			t.Errorf("latency slot %d = %v, want 20", slot, d.arrays[aLat][slot])
		}
	}
	// 900 samples * 8000 B/s / 8 = 900000 bytes.
	if res.downBytes != 900000 || res.upBytes != 90000 {
		t.Errorf("bytes = %d/%d, want 900000/90000", res.downBytes, res.upBytes)
	}
	if d.prevCur != 5 || !d.sampled {
		t.Errorf("cursor state = %d, %v", d.prevCur, d.sampled)
	}
	// Same cursor again: no new samples.
	if res := d.ingest(h, 101, true, t0); res.samples != 0 {
		t.Errorf("re-ingest samples = %d, want 0", res.samples)
	}
}

func TestIngestWrap(t *testing.T) {
	d := newDaemon(config{}, nil)
	d.startWall = t0.Add(-time.Hour)
	d.ingest(mkHist(ringSize-1, []float32{20}, []float32{0}), 100, true, t0)
	// Cursor wraps 899 -> 0: exactly one new sample in slot 0.
	res := d.ingest(mkHist(0, []float32{30}, []float32{0}), 110, true, t0)
	if res.samples != 1 {
		t.Fatalf("wrap samples = %d, want 1", res.samples)
	}
	if v := d.arrays[aLat][0]; v == nil || *v != 30 {
		t.Errorf("slot 0 latency = %v, want 30", v)
	}
	if v := d.arrays[aLat][1]; v == nil || *v != 20 {
		t.Errorf("slot 1 latency = %v, want untouched 20", v)
	}
}

func TestIngestForwardDelta(t *testing.T) {
	d := newDaemon(config{}, nil)
	d.startWall = t0.Add(-time.Hour)
	d.ingest(mkHist(1, []float32{20}, []float32{0}), 100, true, t0)
	res := d.ingest(mkHist(3, []float32{40}, []float32{0}), 102, true, t0)
	if res.samples != 2 {
		t.Fatalf("delta samples = %d, want 2", res.samples)
	}
	for _, slot := range []int{2, 3} {
		if v := d.arrays[aLat][slot]; v == nil || *v != 40 {
			t.Errorf("slot %d latency = %v, want 40", slot, v)
		}
	}
	if v := d.arrays[aLat][1]; v == nil || *v != 20 {
		t.Errorf("slot 1 latency = %v, want untouched 20", v)
	}
}

func TestIngestRebootReset(t *testing.T) {
	d := newDaemon(config{}, nil)
	d.startWall = t0.Add(-time.Hour)
	d.ingest(mkHist(850, []float32{20}, []float32{0}), 1000, true, t0)
	// Cursor goes backwards AND dish uptime regressed: reboot.
	res := d.ingest(mkHist(100, []float32{50}, []float32{0}), 500, true, t0)
	if res.samples != ringSize {
		t.Fatalf("reboot samples = %d, want %d", res.samples, ringSize)
	}
	if d.prevCur != 100 {
		t.Errorf("prevCur = %d, want 100", d.prevCur)
	}
	// The old ring contents must be gone: slot 850 now holds new data.
	if v := d.arrays[aLat][850]; v == nil || *v != 50 {
		t.Errorf("slot 850 after reboot = %v, want 50 (ring was reset)", v)
	}
	if v := d.arrays[aLat][100]; v == nil || *v != 50 {
		t.Errorf("slot 100 after reboot = %v, want 50", v)
	}
}

func TestIngestBackwardsWithoutUptimeRegression(t *testing.T) {
	// cur < prev but uptime increased: normal ring wrap, not a reboot.
	d := newDaemon(config{}, nil)
	d.startWall = t0.Add(-time.Hour)
	d.ingest(mkHist(850, []float32{20}, []float32{0}), 1000, true, t0)
	res := d.ingest(mkHist(100, []float32{50}, []float32{0}), 2000, true, t0)
	if res.samples != ringSize-850+100 {
		t.Errorf("wrap-after-850 samples = %d, want %d", res.samples, ringSize-850+100)
	}
}

func TestIngestClamp(t *testing.T) {
	d := newDaemon(config{}, nil)
	d.startWall = t0.Add(-time.Hour)
	d.ingest(mkHist(0, []float32{20}, []float32{0}), 100, true, t0)
	// Jumped 950 samples ahead: clamp to a full ring re-read.
	res := d.ingest(mkHist(950, []float32{60}, []float32{0}), 1200, true, t0)
	if res.samples != ringSize {
		t.Fatalf("clamped samples = %d, want %d", res.samples, ringSize)
	}
	if v := d.arrays[aLat][50]; v == nil || *v != 60 { // 950 % 900
		t.Errorf("slot 50 = %v, want 60", v)
	}
}

func TestDroppedPingLatencyNil(t *testing.T) {
	d := newDaemon(config{}, nil)
	d.startWall = t0.Add(-time.Hour)
	drop := make([]float32, ringSize)
	drop[7] = 1
	d.ingest(mkHist(899, []float32{20}, drop), 100, true, t0)
	if d.arrays[aLat][7] != nil {
		t.Errorf("dropped sample latency = %v, want nil", d.arrays[aLat][7])
	}
	if d.arrays[aLat][8] == nil {
		t.Error("non-dropped sample latency = nil")
	}
	if d.arrays[aDown][7] == nil {
		t.Error("throughput of dropped-ping slot should still exist")
	}
}

func TestIngestWithoutPowerIn(t *testing.T) {
	d := newDaemon(config{}, nil)
	d.startWall = t0.Add(-time.Hour)
	h := mkHist(10, []float32{20}, []float32{0})
	h.PowerIn = nil // hardware without power telemetry
	d.ingest(h, 100, true, t0)
	for _, slot := range []int{0, 10, 899} {
		if d.arrays[aPow][slot] != nil {
			t.Errorf("power slot %d = %v, want nil", slot, d.arrays[aPow][slot])
		}
	}
}

func TestToOutages(t *testing.T) {
	// Values from testdata/history.real.json; durS rounds per hist.sample.json.
	got := toOutages([]*dish.DishOutage{{
		Cause:            dish.DishOutage_BOOTING,
		StartTimestampNs: 1476462774083980407,
		DurationNs:       112576137083,
	}})
	if len(got) != 1 {
		t.Fatalf("toOutages len = %d", len(got))
	}
	o := got[0]
	if o.Cause != "BOOTING" || o.Start != 1476462774 || o.DurS != 113 {
		t.Errorf("outage = %+v, want {BOOTING 1476462774 113}", o)
	}
}

func TestObserveDishOutages(t *testing.T) {
	d := newDaemon(config{interval: 2 * time.Second, dir: t.TempDir()}, nil)
	d.warm = true // dish-reported outages only fire after warmup
	// Stub the notification side effect; the event file goes to a temp dir.
	orig := notifySend
	notifySend = func(string, string) error { return nil }
	defer func() { notifySend = orig }()
	now := time.Unix(1760000000, 0)
	// Historical outage: primes watermark, no event.
	old := []*dish.DishOutage{{
		Cause:            dish.DishOutage_BOOTING,
		StartTimestampNs: (now.Unix() - 3600) * 1e9,
		DurationNs:       60 * 1e9,
	}}
	d.observeDishOutages(old, now, 4*time.Second)
	if d.openOutage != nil {
		t.Fatal("historical outage opened a track")
	}
	if d.maxDishOutageStart != now.Unix()-3600 {
		t.Fatalf("watermark = %d", d.maxDishOutageStart)
	}
	// Fresh outage begins now: opens a track.
	fresh := []*dish.DishOutage{{
		Cause:            dish.DishOutage_BOOTING,
		StartTimestampNs: (now.Unix() - 1) * 1e9,
		DurationNs:       1 * 1e9,
	}}
	d.observeDishOutages(fresh, now, 4*time.Second)
	if d.openOutage == nil || d.openOutage.cause != "BOOTING" {
		t.Fatalf("openOutage = %+v, want BOOTING track", d.openOutage)
	}
	if d.daily.Outages != 1 {
		t.Errorf("daily.Outages = %d, want 1", d.daily.Outages)
	}
}

func TestUsageGateRestart(t *testing.T) {
	// Daemon B "restarted" 1.5s ago: the full ring is consumed for graphs,
	// but only samples newer than startWall count toward usage.
	b := newDaemon(config{}, nil)
	b.startWall = t0.Add(-time.Hour)
	b.startWall = t0.Add(-1500 * time.Millisecond)
	res := b.ingest(mkHist(899, []float32{20}, []float32{0}), 5000, true, t0)
	if res.samples != ringSize {
		t.Fatalf("restart ingest samples = %d, want %d", res.samples, ringSize)
	}
	// Samples at global 898 (t0-1s) and 899 (t0) postdate startWall: 2 × 1000
	// bytes down, 2 × 100 up. Everything older is the pre-restart overlap.
	if res.downBytes != 2000 || res.upBytes != 200 {
		t.Errorf("gated bytes = %d/%d, want 2000/200", res.downBytes, res.upBytes)
	}
}

func TestHistTimeOrder(t *testing.T) {
	// buildHist must emit oldest→newest regardless of where the cursor sits
	// in the ring (slots are ring-indexed; output is time-rotated).
	d := newDaemon(config{}, nil)
	d.startWall = t0.Add(-time.Hour)
	h := &dish.DishGetHistoryResponse{Current: 5}
	h.PopPingLatencyMs = make([]float32, ringSize)
	h.PopPingDropRate = make([]float32, ringSize)
	h.DownlinkThroughputBps = make([]float32, ringSize)
	h.UplinkThroughputBps = make([]float32, ringSize)
	h.PowerIn = make([]float32, ringSize)
	for i := 0; i < ringSize; i++ {
		h.PopPingLatencyMs[i] = float32(2000 - i) // slot value encodes slot index
		h.DownlinkThroughputBps[i] = 8000
		h.UplinkThroughputBps[i] = 800
	}
	d.ingest(h, 5000, true, t0)
	hf := d.buildHist(t0)
	// Newest sample is slot 5 (value 1995) → last emitted; oldest is slot 6
	// (value 1994) → first emitted.
	if hf.LatencyMs[899] == nil || *hf.LatencyMs[899] != 1995 {
		t.Errorf("newest emitted = %v, want 1995", hf.LatencyMs[899])
	}
	if hf.LatencyMs[0] == nil || *hf.LatencyMs[0] != 1994 {
		t.Errorf("oldest emitted = %v, want 1994", hf.LatencyMs[0])
	}
	if hf.LatencyMs[898] == nil || *hf.LatencyMs[898] != 1996 {
		t.Errorf("second-newest emitted = %v, want 1996", hf.LatencyMs[898])
	}
}

func TestPhantomForwardFill(t *testing.T) {
	// Firmware 2026.09.24+ alternates real samples with zeroed phantoms
	// (dropRate 0, latency 0). Phantoms must carry the previous real sample
	// forward and contribute no usage bytes.
	d := newDaemon(config{}, nil)
	d.startWall = t0.Add(-time.Hour)
	h := &dish.DishGetHistoryResponse{Current: 5}
	h.PopPingLatencyMs = make([]float32, ringSize)
	h.PopPingDropRate = make([]float32, ringSize)
	h.DownlinkThroughputBps = make([]float32, ringSize)
	h.UplinkThroughputBps = make([]float32, ringSize)
	h.PowerIn = make([]float32, ringSize)
	for i := 0; i < ringSize; i++ {
		real := i%2 == 0
		if real {
			h.PopPingLatencyMs[i] = 20
			h.DownlinkThroughputBps[i] = 8000
			h.UplinkThroughputBps[i] = 800
			h.PowerIn[i] = 30
		}
		// phantoms stay zeroed; dropRate stays 0
	}
	res := d.ingest(h, 5000, true, t0)
	if res.samples != ringSize {
		t.Fatalf("samples = %d, want %d", res.samples, ringSize)
	}
	// Only the real half contributes bytes: 450 × 1000 B down, 450 × 100 up.
	if res.downBytes != 450000 || res.upBytes != 45000 {
		t.Errorf("bytes = %d/%d, want 450000/45000", res.downBytes, res.upBytes)
	}
	for k, v := range d.arrays[aLat] {
		if v == nil || *v == 0 {
			t.Fatalf("latency slot %d = %v, want carried real value", k, v)
		}
	}
	for k, v := range d.arrays[aPow] {
		if v == nil || *v != 30 {
			t.Fatalf("power slot %d = %v, want carried 30", k, v)
		}
	}
	// Newest slot (5, odd → phantom) must not surface a fake 0 W.
	if d.lastPowerW != 30 {
		t.Errorf("lastPowerW = %v, want 30", d.lastPowerW)
	}
	// Emitted series stay time-ordered and gap-free.
	hf := d.buildHist(t0)
	for k, v := range hf.LatencyMs {
		if v == nil || *v != 20 {
			t.Fatalf("emitted latency[%d] = %v, want 20", k, v)
		}
	}
}

func TestHostileHistoryShortArrays(t *testing.T) {
	// Marketplace security review: goodTick used to validate only the
	// latency array, so a hostile LAN endpoint answering as the dish could
	// send 900 latencies with an empty/short drop array → index panic in
	// consume → daemon crash loop. Every required array is now validated
	// and a short response is rejected whole, never ingested.
	cases := map[string]func(h *dish.DishGetHistoryResponse){
		"nil drop":   func(h *dish.DishGetHistoryResponse) { h.PopPingDropRate = nil },
		"short drop": func(h *dish.DishGetHistoryResponse) { h.PopPingDropRate = h.PopPingDropRate[:10] },
		"short down": func(h *dish.DishGetHistoryResponse) { h.DownlinkThroughputBps = h.DownlinkThroughputBps[:899] },
		"nil up":     func(h *dish.DishGetHistoryResponse) { h.UplinkThroughputBps = nil },
		"only latencies": func(h *dish.DishGetHistoryResponse) {
			h.PopPingDropRate = nil
			h.DownlinkThroughputBps = nil
			h.UplinkThroughputBps = nil
			h.PowerIn = nil
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := mkStub()
			c.history = mkHist(899, []float32{20}, []float32{0})
			mutate(c.history)
			d := newDaemon(config{
				interval: 2 * time.Second, idleInterval: 10 * time.Second,
				idleHistInterval: time.Minute, mapInterval: time.Minute,
				geoInterval: time.Hour, geoTransitionInterval: time.Hour,
				dir: t.TempDir(),
			}, c)
			d.startWall = t0.Add(-time.Hour)

			d.goodTick(t0, c.status, false, true, 2*time.Second) // must not panic

			if d.sampled {
				t.Fatalf("%s: hostile history was ingested", name)
			}
			// The tick still publishes a healthy status (hist rejected, not
			// the dish): ok true, ONLINE, no crash.
			sf := readJSON(t, d.cfg.dir, "status.json")
			if sf["ok"] != true || sf["state"] != "ONLINE" {
				t.Fatalf("status degraded by rejected history: %v/%v", sf["ok"], sf["state"])
			}
		})
	}
}

func TestConsumeRefusesShortArraysDirectly(t *testing.T) {
	// Defense in depth: consume itself refuses arrays that are not the
	// full ring, even if a future caller skips goodTick's validation.
	c := mkStub()
	c.history = mkHist(899, []float32{20}, []float32{0})
	c.history.PopPingDropRate = c.history.PopPingDropRate[:1]
	d := newDaemon(config{dir: t.TempDir()}, nil)
	d.startWall = t0.Add(-time.Hour)
	res := d.consume(c.history, 5, 899, t0)
	if res.samples != 0 || res.downBytes != 0 {
		t.Fatalf("short-array consume advanced: %+v", res)
	}
	if d.hasHist {
		t.Error("hasHist set by refused consume")
	}
}

func TestHostileNonFiniteFloats(t *testing.T) {
	// Marketplace security review: a hostile endpoint answering as the dish
	// can set any proto float32 to NaN/Inf. encoding/json refuses whole
	// documents containing them, and marshalCompact used to degrade that
	// into "{}" overwriting status.json/hist.json/day-*.json every tick.
	// Three layers now: round() neutralizes, consume skips non-finite
	// samples, and a marshal failure keeps the last good file.
	dir := t.TempDir()
	sent := 0
	realSend := notifySend
	notifySend = func(string, string) error { sent++; return nil }
	defer func() { notifySend = realSend }()

	c := mkStub()
	c.status = onlineStatus()
	c.status.PopPingLatencyMs = float32(math.Float32frombits(0x7FC00000)) // NaN
	c.history = mkHist(899, []float32{20}, []float32{0})
	c.history.PopPingLatencyMs[12] = float32(math.Float32frombits(0x7FC00000))
	c.history.PowerIn[12] = float32(math.Float32frombits(0x7F800000)) // +Inf
	d := newDaemon(config{
		interval: 2 * time.Second, idleInterval: 10 * time.Second,
		idleHistInterval: time.Minute, mapInterval: time.Minute,
		geoInterval: time.Hour, geoTransitionInterval: time.Hour, dir: dir,
	}, c)
	d.startWall = t0.Add(-time.Hour)

	d.goodTick(t0, c.status, true, true, 2*time.Second) // active: hist.json is written

	for _, name := range []string{"status.json", "hist.json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(b) == "{}" {
			t.Fatalf("%s collapsed to {}: marshal failure replaced good state", name)
		}
		var probe map[string]any
		if json.Unmarshal(b, &probe) != nil || len(probe) == 0 {
			t.Fatalf("%s is not a full document: %s", name, b)
		}
	}
	// The day file must be a full document too (NaN sample skipped, not fed).
	dayB, err := os.ReadFile(filepath.Join(dir, "dishes", "ut-test", "day-"+t0.Format("20060102")+".json"))
	if err != nil {
		t.Fatalf("read day file: %v", err)
	}
	if strings.Contains(string(dayB), "NaN") {
		t.Error("day file contains NaN")
	}
	_ = sent
}

func TestOutageAnnounceCooldown(t *testing.T) {
	// A hostile endpoint rotating outage start_timestamp_ns per tick must
	// not fork notify-send per tick: the reannounce cooldown bounds it.
	dir := t.TempDir()
	sent := 0
	realSend := notifySend
	notifySend = func(string, string) error { sent++; return nil }
	defer func() { notifySend = realSend }()

	c := mkStub()
	c.status = onlineStatus()
	d := newDaemon(config{
		interval: 2 * time.Second, idleInterval: 10 * time.Second,
		idleHistInterval: time.Minute, mapInterval: time.Minute,
		geoInterval: time.Hour, geoTransitionInterval: time.Hour, dir: dir,
	}, c)
	d.startWall = t0.Add(-time.Hour)

	rotating := func(i int) {
		c.history = mkHist(uint64(500+i), []float32{20}, []float32{0})
		c.history.Outages = []*dish.DishOutage{{
			Cause:            dish.DishOutage_BOOTING,
			StartTimestampNs: int64(t0.Add(time.Duration(i)*time.Second).Unix()) * 1e9,
			DurationNs:       1e9,
		}}
	}
	for i := 0; i < 6; i++ {
		rotating(i)
		d.goodTick(t0.Add(time.Duration(i)*2*time.Second), c.status, false, true, 2*time.Second)
	}
	if sent > 4 {
		t.Fatalf("rotating outage starts sent %d notifications in 12s, cooldown not enforced", sent)
	}
	// A real dish's static start still announces exactly once.
	d2 := newDaemon(config{
		interval: 2 * time.Second, idleInterval: 10 * time.Second,
		idleHistInterval: time.Minute, mapInterval: time.Minute,
		geoInterval: time.Hour, geoTransitionInterval: time.Hour, dir: t.TempDir(),
	}, mkStub())
	d2.warm = true
	sent = 0
	fixed := []*dish.DishOutage{{
		Cause:            dish.DishOutage_BOOTING,
		StartTimestampNs: (t0.Unix() - 5) * 1e9,
		DurationNs:       3e9,
	}}
	d2.observeDishOutages(fixed, t0, 4*time.Second)
	d2.observeDishOutages(fixed, t0.Add(time.Second), 4*time.Second)
	if sent != 1 {
		t.Fatalf("fixed start announced %d times, want 1 (watermark)", sent)
	}
}

func TestRotatingIDDebounce(t *testing.T) {
	// A hostile endpoint rotating device ids per tick must not create a
	// partition per tick: the debounce means a strictly rotating attacker
	// creates none, and the LRU cap bounds slow churn.
	dir := t.TempDir()
	c := mkStub()
	d := newDaemon(config{
		interval: 2 * time.Second, idleInterval: 10 * time.Second,
		idleHistInterval: time.Minute, mapInterval: time.Minute,
		geoInterval: time.Hour, geoTransitionInterval: time.Hour, dir: dir,
	}, c)
	d.startWall = t0.Add(-time.Hour)

	for i := 0; i < 50; i++ {
		c.status = statusWithID(fmt.Sprintf("fake-%02d", i))
		c.history = mkHist(uint64(500+i), []float32{20}, []float32{0})
		d.goodTick(t0.Add(time.Duration(i)*time.Second), c.status, false, true, 2*time.Second)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "dishes"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 2 {
		t.Fatalf("rotating ids created %d partitions, want <= 2 (initial adopt only)", len(entries))
	}
}

func TestControlLineTruncation(t *testing.T) {
	// The control socket must reject oversize/interminated lines instead of
	// acting on a truncated prefix that normalizes to a different target.
	for _, tc := range []struct {
		name    string
		line    string
		wantBad bool
	}{
		{"normal length", "set host 192.168.100.1:9200\n", false},
		{"long but legal target forwards whole", "set host " + strings.Repeat("a", 245) + ":9200\n", false},
		{"max legal target still fits", "set host " + strings.Repeat("a", 253) + "\n", false},
		{"oversize line rejected", "set host " + strings.Repeat("a", 600) + "\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			ch := make(chan string, 1)
			go serveControlConn(server, ch)
			client.SetDeadline(time.Now().Add(2 * time.Second))
			go io.WriteString(client, tc.line)
			// Single reader on the client end: the daemon's reply ("ok"/"bad").
			buf := make([]byte, 16)
			var reply string
			for {
				n, err := client.Read(buf)
				reply += string(buf[:n])
				if err != nil || strings.Contains(reply, "\n") {
					break
				}
			}
			<-time.After(50 * time.Millisecond)
			got := ""
			select {
			case got = <-ch:
			default:
			}
			if tc.wantBad {
				if !strings.HasPrefix(reply, "bad") || got != "" {
					t.Fatalf("oversize line: reply=%q forwarded=%q, want bad + no forward", reply, got)
				}
				return
			}
			if got != strings.TrimSuffix(tc.line, "\n") {
				t.Fatalf("forwarded = %q, want %q", got, strings.TrimSuffix(tc.line, "\n"))
			}
			if !strings.HasPrefix(reply, "ok") {
				t.Fatalf("reply = %q, want ok", reply)
			}
		})
	}
}

func TestToObsMapRejectsIncoherentGrid(t *testing.T) {
	// The QML consumer loops rows*cols on the GUI thread: a map whose
	// declared grid exceeds the delivered cells (or realistic dims) is
	// rejected whole.
	if m := toObsMap(&dish.DishGetObstructionMapResponse{
		NumRows: 1000000, NumCols: 1000000, Snr: []float32{1},
	}); m != nil {
		t.Fatal("hostile 1e6 dims accepted")
	}
	if m := toObsMap(&dish.DishGetObstructionMapResponse{
		NumRows: 4294967295, NumCols: 4294967295, Snr: []float32{1},
	}); m != nil {
		t.Fatal("uint32-max dims accepted")
	}
	if m := toObsMap(&dish.DishGetObstructionMapResponse{
		NumRows: 4, NumCols: 4, Snr: []float32{1, 2, 3},
	}); m != nil {
		t.Fatal("rows*cols > len(snr) accepted")
	}
	if m := toObsMap(&dish.DishGetObstructionMapResponse{
		NumRows: 4, NumCols: 4, Snr: make([]float32, 16),
	}); m == nil {
		t.Fatal("coherent map rejected")
	}
}

func TestFailTickNotifyWarmGate(t *testing.T) {
	// Cold start against an unreachable dish: no notification spam. After the
	// first good tick (warm), a fresh outage notifies.
	sent := 0
	realSend := notifySend
	notifySend = func(summary, body string) error { sent++; return nil }
	defer func() { notifySend = realSend }()

	d := newDaemon(config{}, nil)
	d.cfg.dir = t.TempDir() // failTick persists; keep writes out of CWD
	d.startWall = t0.Add(-time.Hour)
	// The canonical at-home dark-dish case with no LAN signal: fresh egress
	// says Starlink, which keeps the link CONFIRMED while the RPC is down.
	// No route change ever happened (lastRouteChangedUnix = 0), so the geo
	// trivially postdates it and the outage gate stays open.
	d.geo = geoInfo{Ts: t0.Unix() - 5, IsStarlink: true, ASN: starlinkASN, Org: "SpaceX"}
	d.link = linkInfo{OnStarlink: true, Reason: "egress", Since: t0.Unix()}
	d.lastPositive = t0.Unix()
	for i := 0; i < 3; i++ {
		d.failTick(t0.Add(time.Duration(i)*2*time.Second), "refused", 2*time.Second, false)
	}
	if sent != 0 {
		t.Fatalf("cold-start failures sent %d notifications, want 0", sent)
	}

	d.warm = true // first good tick happened
	d.failed = 0
	d.failTick(t0.Add(10*time.Second), "refused", 2*time.Second, false)
	d.failTick(t0.Add(12*time.Second), "refused", 2*time.Second, false)
	if sent != 1 {
		t.Fatalf("post-warm outage sent %d notifications, want 1", sent)
	}
}

func TestEgressOutageRouteGate(t *testing.T) {
	// An egress-confirmed outage must not fire from a geo answer that
	// predates the current default route: "left home, the new network is
	// dead" is leaving, not dish downtime — even though the stale home geo
	// still says AS14593.
	sent := 0
	realSend := notifySend
	notifySend = func(summary, body string) error { sent++; return nil }
	defer func() { notifySend = realSend }()

	d := newDaemon(config{}, nil)
	d.cfg.dir = t.TempDir()
	d.startWall = t0.Add(-time.Hour)
	d.warm = true
	d.lastRouteChangedUnix = t0.Unix() // route changed now: just left home
	// Stale home answer, captured before the route change.
	d.geo = geoInfo{Ts: t0.Unix() - 60, IsStarlink: true, ASN: starlinkASN, Org: "SpaceX"}
	d.link = linkInfo{OnStarlink: true, Reason: "egress", Since: t0.Unix() - 3600}
	d.lastPositive = t0.Unix() - 60

	for i := 0; i < 3; i++ {
		d.failTick(t0.Add(time.Duration(i)*10*time.Second), "refused", 10*time.Second, false)
	}
	if sent != 0 || d.openOutage != nil {
		t.Fatalf("stale pre-change geo raised an outage: sent=%d track=%v", sent, d.openOutage)
	}

	// The next egress answer is captured after the route change and still
	// says Starlink: the machine genuinely sits on the dish's network, so
	// the persisting RPC failure is real downtime and fires.
	d.geo = geoInfo{Ts: t0.Unix() + 35, IsStarlink: true, ASN: starlinkASN, Org: "SpaceX"}
	d.failTick(t0.Add(40*time.Second), "refused", 10*time.Second, false)
	d.failTick(t0.Add(50*time.Second), "refused", 10*time.Second, false)
	if sent != 1 || d.openOutage == nil {
		t.Fatalf("post-change egress outage did not fire: sent=%d track=%v", sent, d.openOutage)
	}
}

func TestEgressOutageDishOKGate(t *testing.T) {
	// Mirror of TestEgressOutageRouteGate: the gate also opens when an
	// identity-verified dish answer postdates the route change — a real
	// at-home dish outage behind a transient route change still notifies,
	// even when egress lookups keep failing (dish deaths normally take the
	// internet down with them).
	sent := 0
	realSend := notifySend
	notifySend = func(summary, body string) error { sent++; return nil }
	defer func() { notifySend = realSend }()

	d := newDaemon(config{}, nil)
	d.cfg.dir = t.TempDir()
	d.startWall = t0.Add(-time.Hour)
	d.warm = true
	d.lastRouteChangedUnix = t0.Unix() // route changed now (transient, at home)
	// Stale home geo from before the change; post-change lookups keep
	// failing, so it stays the freshest answer.
	d.geo = geoInfo{Ts: t0.Unix() - 60, IsStarlink: true, ASN: starlinkASN, Org: "SpaceX"}
	d.link = linkInfo{OnStarlink: true, Reason: "egress", Since: t0.Unix() - 3600, LastDishOK: t0.Unix() - 120}
	d.lastPositive = t0.Unix() - 60

	for i := 0; i < 3; i++ {
		d.failTick(t0.Add(time.Duration(i)*10*time.Second), "refused", 10*time.Second, false)
	}
	if sent != 0 || d.openOutage != nil {
		t.Fatalf("pre-change dish answer did not suppress: sent=%d track=%v", sent, d.openOutage)
	}

	// One identity-verified answer at +20 (dish blipped back once): the
	// machine is provably on the dish's network now, so a persisting
	// RPC failure is real downtime and fires.
	d.link.LastDishOK = t0.Unix() + 20
	d.failTick(t0.Add(40*time.Second), "refused", 10*time.Second, false)
	d.failTick(t0.Add(50*time.Second), "refused", 10*time.Second, false)
	if sent != 1 || d.openOutage == nil {
		t.Fatalf("post-change dish answer did not re-arm the gate: sent=%d track=%v", sent, d.openOutage)
	}
}

func TestNotADishAnswer(t *testing.T) {
	// A device that answers the RPC without dish identity — e.g. a cable
	// modem or router holding 192.168.100.1 — must NOT count as dish
	// evidence: the tick fails with an explicit error, the link never reports
	// reason "dish", and no outage machinery runs.
	sent := 0
	realSend := notifySend
	notifySend = func(string, string) error { sent++; return nil }
	defer func() { notifySend = realSend }()

	now := time.Now()
	for name, status := range map[string]*dish.DishGetStatusResponse{
		"nil answer":     nil,
		"no device info": {},
	} {
		t.Run(name, func(t *testing.T) {
			c := mkStub()
			c.status = status
			d := newDaemon(config{
				interval: 2 * time.Second, idleInterval: 10 * time.Second,
				idleHistInterval: time.Minute, mapInterval: time.Minute,
				geoInterval: time.Hour, geoTransitionInterval: time.Hour,
			}, c)
			d.cfg.dir = t.TempDir()
			d.warm = true
			// Fresh non-Starlink egress (a cable ISP): classification there
			// rests entirely on dish identity, which the stub mocks out.
			d.geo = geoInfo{Ts: now.Unix(), IsStarlink: false, ASN: "AS11404", Org: "Qwest"}
			d.link = linkInfo{OnStarlink: false, Reason: "egress-other", Since: now.Unix()}
			d.lastDishOK = true
			d.link.LastDishOK = now.Unix() - 10 // must NOT be renewed without identity

			d.tick(false)

			if sent != 0 {
				t.Fatalf("identity failure sent %d notifications, want 0", sent)
			}
			sf := readJSON(t, d.cfg.dir, "status.json")
			if sf["ok"] != false || sf["state"] != "OFFLINE" {
				t.Fatalf("tick did not fail: ok=%v state=%v", sf["ok"], sf["state"])
			}
			if err, _ := sf["error"].(string); !strings.Contains(err, "not answering as a Starlink dish") {
				t.Fatalf("error = %q", err)
			}
			link, _ := sf["link"].(map[string]any)
			if link["reason"] == "dish" {
				t.Fatalf("reason = %v, no dish identity means no dish evidence", link["reason"])
			}
			if link["lastDishOK"].(float64) != float64(now.Unix()-10) {
				t.Fatalf("lastDishOK = %v, want untouched %d", link["lastDishOK"], now.Unix()-10)
			}
		})
	}
}
