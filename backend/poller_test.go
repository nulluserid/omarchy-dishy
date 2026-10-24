package main

import (
	"net"
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
	// On the dish's LAN: this is the canonical at-home dark-dish case — LAN
	// evidence keeps the link CONFIRMED while the RPC is down, so the outage
	// machinery still runs. Synthetic addrs keep the test hermetic.
	d.addrsFn = func() ([]net.Addr, error) { return []net.Addr{mkAddr("192.168.100.9/32")}, nil }
	d.link = linkInfo{OnStarlink: true, Reason: "dish", Since: t0.Unix()}
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
