// The synthetic dish: the scenarios, the wall-clock sample model and the
// answers starlinkd consumes. main.go holds the servers, seed.go the seed.

package main

import (
	"fmt"
	"math"
	"time"

	common "starlinkd/gen/spacex_api/device/common"
	api "starlinkd/gen/spacex_api/device/device"
	dish "starlinkd/gen/spacex_api/device/dish"
	udc "starlinkd/gen/spacex_api/satellites/network/ut_disablement_codes"
)

// The dish's identity and every number below are invented. Nothing here is
// read from a real dish or from starlinkd's state.
const (
	dishID     = "utdemo0000000-00000000-0000demo"
	dishHW     = "mini1_panda_prod1"
	dishSW     = "2026.11.15.mr90042"
	bootcount  = 360
	apiVersion = 43
	ringSize   = 900 // history samples, 1 s apart (CONTRACT.md)
)

// Salts keep the hash streams independent.
const (
	saltLat uint64 = iota + 1
	saltSpike
	saltSpikeLen
	saltSpikePeak
	saltDrop
	saltDown
	saltUp
	saltPower
	saltBurst
	saltWobble
	saltJitter
	saltActive
	saltRun
	saltGps
)

// burstFloor is the noise level past which a download burst builds.
const burstFloor = 0.6

// mix is the splitmix64 finalizer: a stateless uniform hash of (sec, salt),
// which is what makes every value a pure function of the wall clock.
func mix(sec int64, salt uint64) uint64 {
	x := uint64(sec)*0x9E3779B97F4A7C15 + salt
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	return x ^ (x >> 31)
}

func unit(v uint64) float64 { return float64(v>>11) / (1 << 53) }

// noise is smooth value noise in [0,1): a fresh hash every scale seconds,
// eased in between. Organic drift with no state to carry across restarts.
func noise(sec, scale int64, salt uint64) float64 {
	a, f := sec/scale, float64(sec%scale)/float64(scale)
	f = f * f * (3 - 2*f)
	return unit(mix(a, salt))*(1-f) + unit(mix(a+1, salt))*f
}

// band is the range a live value wanders in.
type band struct{ lo, hi float64 }

// inBand places x, nominally in [0,1], inside b.
func inBand(b band, x float64) float64 { return b.lo + (b.hi-b.lo)*x }

// sample is one second of history.
type sample struct {
	latMs, drop, downBps, upBps, powerW float32
}

type outageDef struct {
	cause dish.DishOutage_Cause
	ago   time.Duration // started this long before the anchor
	dur   time.Duration
}

// scenario is one demo mood. The bands are where the live values sit while
// traffic flows; downAgo > 0 means the dish has been SEARCHING since then.
type scenario struct {
	lat, spike, down, up, power band
	burstMbps                   float64 // added on top of the down band in bursts
	spikeEvery                  float64 // seconds between latency spikes, on average
	dropRate                    float64 // share of seconds with a dropped ping
	downAgo                     time.Duration
	outages                     []outageDef // dish-reported, oldest first
	fraction                    float32     // fractionObstructed
	alerts                      *dish.DishAlerts
	update                      dish.SoftwareUpdateState
}

var scenarios = map[string]*scenario{
	// clean is the screenshot scenario: a healthy connection.
	"clean": {
		lat: band{22, 35}, spike: band{60, 90}, spikeEvery: 220, dropRate: 0.002,
		down: band{40, 88}, burstMbps: 65, up: band{8, 25}, power: band{25, 45},
		fraction: 0.0031, alerts: &dish.DishAlerts{}, update: dish.SoftwareUpdateState_IDLE,
		outages: []outageDef{
			{dish.DishOutage_BOOTING, 21*time.Hour + 6*time.Minute, 112 * time.Second},
			{dish.DishOutage_OBSTRUCTED, 9*time.Hour + 20*time.Second, 21 * time.Second},
			{dish.DishOutage_NO_SCHEDULE, 3*time.Hour + 40*time.Second, 38 * time.Second},
		},
	},
	// busy: a sustained heavy download.
	"busy": {
		lat: band{30, 46}, spike: band{95, 125}, spikeEvery: 170, dropRate: 0.004,
		down: band{150, 205}, burstMbps: 50, up: band{15, 38}, power: band{38, 58},
		fraction: 0.0031, alerts: &dish.DishAlerts{}, update: dish.SoftwareUpdateState_IDLE,
		outages: []outageDef{
			{dish.DishOutage_BOOTING, 21*time.Hour + 6*time.Minute, 112 * time.Second},
			{dish.DishOutage_NO_SCHEDULE, 3*time.Hour + 40*time.Second, 38 * time.Second},
		},
	},
	// trouble: a knocked mast, searching, with an update waiting.
	"trouble": {
		lat: band{22, 35}, spike: band{60, 90}, spikeEvery: 220, dropRate: 0.002,
		down: band{40, 88}, burstMbps: 65, up: band{8, 25}, power: band{25, 45},
		downAgo:  38 * time.Minute,
		fraction: 0.0082,
		alerts:   &dish.DishAlerts{MastNotNearVertical: true, ObstructionMapReset: true, InstallPending: true},
		update:   dish.SoftwareUpdateState_REBOOT_REQUIRED,
		outages: []outageDef{
			{dish.DishOutage_BOOTING, 26*time.Hour + 12*time.Minute, 112 * time.Second},
			{dish.DishOutage_NO_SATS, 6*time.Hour + 10*time.Minute, 240 * time.Second},
			{dish.DishOutage_OBSTRUCTED, 2*time.Hour + 5*time.Minute, 95 * time.Second},
			{dish.DishOutage_NO_SCHEDULE, 40 * time.Minute, 61 * time.Second},
		},
	},
}

type fakeDish struct {
	api.UnimplementedDeviceServer
	sc        *scenario
	anchor    time.Time // start of the minute the demo began; relative times hang off it
	bootAt    time.Time
	downSince time.Time // pings unanswered from here on; zero while the link is up
	sky       *dish.DishGetObstructionMapResponse
	verbose   bool
}

func newFakeDish(name string, now time.Time) (*fakeDish, error) {
	sc := scenarios[name]
	if sc == nil {
		return nil, fmt.Errorf("unknown scenario %q (clean, busy, trouble)", name)
	}
	// The anchor is rounded to the minute so the seed step and the server
	// started after it place the same outages.
	d := &fakeDish{sc: sc, anchor: now.Truncate(time.Minute), sky: newObstructionMap()}
	d.bootAt = d.anchor.Add(-30 * 24 * time.Hour)
	for _, o := range sc.outages {
		if o.cause == dish.DishOutage_BOOTING {
			d.bootAt = d.anchor.Add(-o.ago)
		}
	}
	if sc.downAgo > 0 {
		d.downSince = d.anchor.Add(-sc.downAgo)
	}
	return d, nil
}

func (d *fakeDish) searching(sec int64) bool {
	return !d.downSince.IsZero() && sec >= d.downSince.Unix()
}

// dropped says whether the ping at sec went unanswered: the dish is
// searching, sec lies inside a reported outage, or the random draw hit.
func (d *fakeDish) dropped(sec int64) bool {
	if d.searching(sec) {
		return true
	}
	for _, o := range d.sc.outages {
		start := d.anchor.Add(-o.ago).Unix()
		if sec >= start && sec < start+int64(o.dur/time.Second) {
			return true
		}
	}
	return unit(mix(sec, saltDrop)) < d.sc.dropRate
}

// spike is the latency spike covering sec, if any. One starts about every
// sc.spikeEvery seconds, lasts one or two seconds and peaks in sc.spike.
func (d *fakeDish) spike(sec int64) (float64, bool) {
	sc := d.sc
	for back := int64(0); back < 2; back++ {
		start := sec - back
		if unit(mix(start, saltSpike)) >= 1/sc.spikeEvery {
			continue
		}
		two := unit(mix(start, saltSpikeLen)) < 0.5
		if back == 1 && !two {
			continue // the one-second spike is over
		}
		peak := sc.spike.lo + (sc.spike.hi-sc.spike.lo)*unit(mix(start, saltSpikePeak))
		if back == 0 && two {
			peak = sc.lat.hi + 0.5*(peak-sc.lat.hi) // climbs over the first second
		}
		return peak, true
	}
	return 0, false
}

// sample is one second of history at wall-clock second sec. idle is the quiet
// background traffic of an unused connection; otherwise the scenario's usage
// flows. A dropped ping carries no latency, like the real ring.
func (d *fakeDish) sample(sec int64, idle bool) sample {
	sc := d.sc
	if d.searching(sec) {
		// A dish hunting for satellites draws more than one online.
		return sample{drop: 1, powerW: float32(sc.power.hi + 4*noise(sec, 330, saltPower))}
	}
	burst := math.Max(0, noise(sec, 41, saltBurst)-burstFloor) / (1 - burstFloor)
	s := sample{powerW: float32(inBand(sc.power, 0.8*noise(sec, 330, saltPower)+0.2*burst))}
	if idle {
		s.downBps = float32(inBand(band{5e4, 9e5}, noise(sec, 90, saltDown)))
		s.upBps = float32(inBand(band{2e4, 2.5e5}, noise(sec, 90, saltUp)))
		s.powerW = float32(inBand(band{sc.power.lo, sc.power.lo + 0.3*(sc.power.hi-sc.power.lo)},
			noise(sec, 330, saltPower)))
	} else {
		// Downlink bursts push past the band; uplink and power stay inside
		// theirs, drawn toward the top while a burst runs.
		s.downBps = float32((inBand(sc.down, noise(sec, 260, saltDown)) + sc.burstMbps*burst +
			3*(noise(sec, 5, saltWobble)-0.5)) * 1e6)
		s.upBps = float32(inBand(sc.up, 0.85*noise(sec, 170, saltUp)+0.15*burst) * 1e6)
	}
	if d.dropped(sec) {
		s.latMs, s.drop, s.downBps, s.upBps = 0, 1, 0, 0
		return s
	}
	s.latMs = float32(inBand(sc.lat, noise(sec, 210, saltLat)) +
		1.6*(noise(sec, 6, saltWobble)-0.5) + 1.5*(unit(mix(sec, saltJitter))-0.5))
	if peak, ok := d.spike(sec); ok {
		s.latMs = float32(peak)
	}
	return s
}

// live is the status readout at now: the newest sample, approached from the
// one before it, so the numbers move between ticks instead of stepping.
func (d *fakeDish) live(now time.Time) sample {
	sec := now.Unix()
	cur, prev := d.sample(sec, false), d.sample(sec-1, false)
	if d.searching(sec) {
		return sample{drop: 1, powerW: cur.powerW}
	}
	if cur.drop >= 1 {
		cur = prev // a dropped second reports the last answered ping
	}
	if prev.drop >= 1 {
		prev = cur
	}
	if cur.drop >= 1 {
		mid := float32((d.sc.lat.lo + d.sc.lat.hi) / 2)
		cur.latMs, prev.latMs = mid, mid
	}
	f := float32(now.Nanosecond()) / 1e9
	return sample{
		latMs:   prev.latMs + f*(cur.latMs-prev.latMs),
		downBps: prev.downBps + f*(cur.downBps-prev.downBps),
		upBps:   prev.upBps + f*(cur.upBps-prev.upBps),
		powerW:  prev.powerW + f*(cur.powerW-prev.powerW),
	}
}

// status is the GetStatus answer: what starlinkd reads for identity, state
// and the live numbers, plus a few fields a real dish always carries.
func (d *fakeDish) status(now time.Time) *dish.DishGetStatusResponse {
	sc, cur := d.sc, d.live(now)
	uptime := now.Sub(d.bootAt).Seconds()
	validS := math.Max(0, uptime-1800) // the sky map needs half an hour to count
	reboot := sc.update == dish.SoftwareUpdateState_REBOOT_REQUIRED
	var progress float32 // 0 .. 1; 1 once the download finished
	if reboot {
		progress = 1
	}
	return &dish.DishGetStatusResponse{
		DeviceInfo: &common.DeviceInfo{
			Id: dishID, HardwareVersion: dishHW, SoftwareVersion: dishSW,
			CountryCode: "US", UtcOffsetS: -5 * 3600, Bootcount: bootcount,
		},
		DeviceState: &common.DeviceState{UptimeS: uint64(uptime)},
		Alerts:      sc.alerts,
		GpsStats: &dish.DishGpsStats{
			GpsValid:                  true,
			GpsSats:                   uint32(22 + math.Round(2*noise(now.Unix(), 600, saltGps))),
			PntFilterConvergenceState: dish.AttitudeEstimationState_FILTER_CONVERGED,
		},
		PopPingDropRate:       cur.drop,
		DownlinkThroughputBps: cur.downBps,
		UplinkThroughputBps:   cur.upBps,
		PopPingLatencyMs:      cur.latMs,
		ObstructionStats: &dish.DishObstructionStats{
			FractionObstructed: sc.fraction,
			TimeObstructed:     sc.fraction * float32(validS),
			ValidS:             float32(validS),
			PatchesValid:       6088,
		},
		EthSpeedMbps:         1000,
		MobilityClass:        dish.UserMobilityClass_MOBILE,
		IsSnrAboveNoiseFloor: !d.searching(now.Unix()),
		ReadyStates:          &dish.DishReadyStates{Scp: true, L1L2: true, Xphy: true, Aap: true, Rf: true},
		ClassOfService:       dish.UserClassOfService_CONSUMER,
		SoftwareUpdateState:  sc.update,
		SwupdateRebootReady:  reboot,
		SoftwareUpdateStats: &dish.SoftwareUpdateStats{
			SoftwareUpdateState:    sc.update,
			SoftwareUpdateProgress: progress,
			UpdateRequiresReboot:   reboot,
		},
		DisablementCode: udc.UtDisablementCode_OKAY,
		HasActuators:    dish.HasActuators_HAS_ACTUATORS_NO,
	}
}

// history is the GetHistory answer: the full 900-sample ring ending now. The
// ring is slot-indexed like the real dish's, so sample sec lives in slot
// sec mod 900 and the cursor is the newest sample's slot.
func (d *fakeDish) history(now time.Time) *dish.DishGetHistoryResponse {
	h := &dish.DishGetHistoryResponse{
		Current:               uint64(now.Unix()) % ringSize,
		PopPingDropRate:       make([]float32, ringSize),
		PopPingLatencyMs:      make([]float32, ringSize),
		DownlinkThroughputBps: make([]float32, ringSize),
		UplinkThroughputBps:   make([]float32, ringSize),
		PowerIn:               make([]float32, ringSize),
	}
	for sec := now.Unix() - ringSize + 1; sec <= now.Unix(); sec++ {
		s := d.sample(sec, false)
		i := sec % ringSize
		h.PopPingDropRate[i], h.PopPingLatencyMs[i] = s.drop, s.latMs
		h.DownlinkThroughputBps[i], h.UplinkThroughputBps[i], h.PowerIn[i] = s.downBps, s.upBps, s.powerW
	}
	for _, o := range d.sc.outages {
		start := d.anchor.Add(-o.ago)
		h.Outages = append(h.Outages, &dish.DishOutage{
			Cause: o.cause, StartTimestampNs: start.UnixNano(), DurationNs: uint64(o.dur), DidSwitch: true,
		})
	}
	return h
}

// newObstructionMap is the Mini's 123×123 sky map: -1 outside the unit
// circle and across a thin geostationary-exclusion band, a soft tree line of
// partial clearance low toward the south, clear sky (1) elsewhere. Values
// step by 0.1 like the real dish's.
func newObstructionMap() *dish.DishGetObstructionMapResponse {
	const n = 123
	m := &dish.DishGetObstructionMapResponse{
		NumRows: n, NumCols: n, MinElevationDeg: 10, MaxThetaDeg: 80,
		MapReferenceFrame: dish.ObstructionMapReferenceFrame_FRAME_UT,
		Snr:               make([]float32, n*n),
	}
	c := float64(n-1) / 2
	for row := range n {
		for col := range n {
			dx, dy := (float64(col)-c)/c, (float64(row)-c)/c
			v := 1.0
			switch {
			case dx*dx+dy*dy > 1, row >= 59 && row <= 63:
				v = -1
			default:
				// Polar view of the sky: theta from zenith, phi from north
				// (180 = south, the bottom of the map). The tree line is
				// an ellipse on the rim due south, about 85° across and
				// reaching 22° up from it: 0.1 in its middle, fading out
				// softly.
				theta := math.Hypot(dx, dy) * 80
				off := math.Abs(math.Atan2(dx, -dy)*180/math.Pi) - 180
				q := (off*off)/(26*26) + (theta-80)*(theta-80)/(13*13)
				if d := 1 - q/2.8; d > 0 {
					v = 1 - 0.9*d*d
				}
			}
			m.Snr[row*n+col] = float32(math.Round(v*10) / 10)
		}
	}
	return m
}
