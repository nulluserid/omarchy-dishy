package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"

	api "starlinkd/gen/spacex_api/device/device"
	dish "starlinkd/gen/spacex_api/device/dish"
)

// dial connects to the dish. The connection is lazy; failures surface per RPC.
func dial(target string) (*grpc.ClientConn, error) {
	return grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

func rpcErr(err error) string {
	s := grpcstatus.Convert(err)
	return s.Code().String() + ": " + s.Message()
}

func (d *daemon) vlog(format string, args ...any) {
	if d.cfg.verbose {
		fmt.Fprintf(osStderr, "starlinkd: "+format+"\n", args...)
	}
}

// warn logs an error only when its message changes (rate-limited).
// An empty message resets the rate-limit key without logging.
func (d *daemon) warn(msg string) {
	if msg == "" {
		d.lastErr = ""
		return
	}
	if msg != d.lastErr {
		d.vlog("%s", msg)
		d.lastErr = msg
	}
}

// tick runs one poll cycle. It never panics and never blocks longer than
// ~3 RPC timeouts; every error path still refreshes the state files.
// active means the dashboard is open (or the probe burst is running): fast
// cadence, hist.json writes, obstruction-map refreshes.
func (d *daemon) tick(active bool) {
	now := time.Now()
	d.drainGeo()
	d.updateRoute(now)
	d.maybeFetchGeo(now, active)

	// Real elapsed: the cadence varies idle/active. Booked at most
	// maxBookedDt — timers freeze across suspend, and a laptop waking after
	// hours must not enter those hours as dish downtime.
	dt := now.Sub(d.lastTickAt)
	d.lastTickAt = now
	if dt > maxBookedDt || dt < 0 {
		if dt > maxBookedDt {
			dt = maxBookedDt
		} else {
			dt = 0
		}
	}

	// History ingest runs every active tick and every idleHistInterval in the
	// background (keeps dailies accurate and the ring current at 60s cost).
	fetchHist := active || now.Sub(d.lastHist) >= d.cfg.idleHistInterval

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	resp, err := d.client.Handle(ctx, &api.Request{Request: &api.Request_GetStatus{GetStatus: &api.GetStatusRequest{}}})
	cancel()
	if err != nil {
		d.failTick(now, rpcErr(err), dt, active)
		d.postTick(now, false)
		return
	}
	s := resp.GetDishGetStatus()
	// Identity check: an answer counts as dish evidence only when it comes
	// back AS a Starlink dish. Residential cable modems / routers can hold
	// the dish's address and respond, but never with dish device info.
	if s == nil || s.GetDeviceInfo() == nil {
		d.failTick(now, "not answering as a Starlink dish (no device info)", dt, active)
		d.postTick(now, false)
		return
	}
	d.warn("") // success resets rate-limit key

	d.goodTick(now, s, active, fetchHist, dt)
	d.postTick(now, true)
}

// postTick books the one-shot probe: only the transition into failure starts
// a burst, so a machine parked away from the dish does not permanently poll
// at 2 s.
func (d *daemon) postTick(now time.Time, ok bool) {
	if !ok && d.lastTickOK {
		d.probeUntil = now.Add(probeWindow)
		d.vlog("failure probe: %s window", probeWindow)
	}
	d.lastTickOK = ok
}

func (d *daemon) goodTick(now time.Time, s *dish.DishGetStatusResponse, active, fetchHist bool, dt time.Duration) {
	state, detail := deriveState(s)
	d.lastStatus = s
	d.updateLink(now, true)
	// Identity-verified dish answer: advance the stamp exactly here — the
	// only path that ever does (applyGeo's link recompute carries reason
	// without stamping). The outage gate (failTick) treats this field as
	// "a real dish answer postdates X".
	d.link.LastDishOK = now.Unix()

	// History: delta-process the ring and refresh dish-reported outages.
	histOK := false
	var res ingestResult
	if fetchHist {
		// Freshness window for dish-reported outages spans the ingest gap:
		// with a 60 s background cadence an outage observed on resume is
		// still announced, while a settled ring stays watermark-only.
		ingestGap := now.Sub(d.lastHist)
		var h *api.Response
		h = d.fetch(now, &api.Request{Request: &api.Request_GetHistory{GetHistory: &api.GetHistoryRequest{}}})
		if hh := h.GetDishGetHistory(); hh != nil && len(hh.GetPopPingLatencyMs()) == ringSize {
			uptime, haveUp := uint64(0), false
			if ds := s.GetDeviceState(); ds != nil {
				uptime, haveUp = ds.GetUptimeS(), true
			}
			res = d.ingest(hh, uptime, haveUp, now)
			d.lastHist = now
			d.observeDishOutages(hh.GetOutages(), now, ingestGap)
			d.lastOutages = toOutages(hh.GetOutages())
			histOK = true
		} else if h != nil {
			d.vlog("history: short response")
		}
	}

	// Obstruction map is only needed for the dashboard: skip entirely while
	// the panel is closed. On the first open the last map is minutes old —
	// good enough until the next active due tick refreshes it.
	if active && now.Sub(d.lastMap) >= d.cfg.mapInterval {
		if m := d.fetch(now, &api.Request{Request: &api.Request_DishGetObstructionMap{DishGetObstructionMap: &dish.DishGetObstructionMapRequest{}}}); m != nil {
			d.mapData = toObsMap(m.GetDishGetObstructionMap())
			d.lastMap = now
		}
	}

	// Daily/session counters — only while on the Starlink network
	// (CONTRACT.md "Off-network accounting": parked elsewhere ≠ dish downtime).
	d.rollDaily(now)
	if d.link.OnStarlink {
		if histOK {
			d.daily.DownBytes += res.downBytes
			d.daily.UpBytes += res.upBytes
		} else {
			secs := dt.Seconds()
			d.daily.DownBytes += int64(s.GetDownlinkThroughputBps() / 8 * float32(secs))
			d.daily.UpBytes += int64(s.GetUplinkThroughputBps() / 8 * float32(secs))
		}
		dtS := int64(dt.Seconds() + 0.5)
		d.daily.TotalS += dtS
		d.sessionS += dtS
		if state == "ONLINE" {
			d.daily.OnlineS += dtS
			d.sessionOnlineS += dtS
		}
	}

	d.handleTransitions(now, s, state, dt)

	// Close an open outage once we are back online.
	if d.openOutage != nil && state == "ONLINE" {
		cause := d.openOutage.cause
		durS := now.Unix() - d.openOutage.start
		d.openOutage = nil
		d.emit(event{Type: "outage_end", Cause: cause, DurS: durS},
			"Starlink online", fmt.Sprintf("down %dm %ds", durS/60, durS%60))
	}
	d.failed = 0

	sf := d.buildStatus(now, s, state, detail)
	// hist.json is written on active ticks (the only consumer reads it while
	// the panel is open); idle ticks keep the ring in memory only.
	var hf *histFile
	if active {
		hf = d.buildHist(now)
	}
	d.persist(now, sf, hf)
}

// failTick handles a failed GetStatus: state OFFLINE, ok:false, fresh status.
// Outage tracking and daily booking require CONFIRMED evidence of being on
// Starlink (dish/egress) — a grace hold or a no-evidence state books nothing
// and notifies nothing (a parked or portal-walled machine is not dish
// downtime).
//
// Egress-confirmed outages carry one extra gate: the geo answer must postdate
// the last default-route change. Stale home geo still says AS14593 after
// leaving, and "walked out of Wi-Fi range into a dead network" is leaving,
// not dish downtime — the announcement waits for evidence captured on the
// current route. The gate opens on either post-change signal: fresh geo
// (lookups succeed) or link.lastDishOK — renewed ONLY by identity-verified
// good ticks, never by geo application — so a real at-home dish outage behind
// a flaky route still notifies even when egress lookups cannot succeed (dish
// deaths normally kill that path too). Geo results are stamped at request
// launch, so a lookup in flight across the route change cannot fake either
// term. (updateLink has already recomputed the reason, and a failing dish RPC
// can never leave reason "dish" here, so this gates exactly the egress path.)
func (d *daemon) failTick(now time.Time, msg string, dt time.Duration, active bool) {
	d.failed++
	d.warn("status rpc: " + msg)
	d.updateLink(now, false)
	if d.linkConfirmed() {
		if d.failed >= 2 && d.openOutage == nil && d.warm &&
			(d.geo.Ts >= d.lastRouteChangedUnix || d.link.LastDishOK >= d.lastRouteChangedUnix) {
			// Contract: an outage is >=2 consecutive failed ticks. Warm-gated:
			// a daemon that starts while the dish is already unreachable stays
			// quiet (e.g. boot races), same as dish-reported outages.
			d.openOutage = &outageTrack{cause: "OFFLINE", start: now.Unix()}
			d.daily.Outages++
			d.emit(event{Type: "outage_start", Cause: "OFFLINE"},
				"Starlink offline", humanizeCause("OFFLINE"))
		}
		d.rollDaily(now)
		dtS := int64(dt.Seconds() + 0.5)
		d.daily.TotalS += dtS
		d.sessionS += dtS
	}

	sf := d.buildStatusFail(now, msg)
	d.persist(now, sf, nil)
}

// fetch performs one unary Handle call; nil response on any failure.
func (d *daemon) fetch(now time.Time, req *api.Request) *api.Response {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	resp, err := d.client.Handle(ctx, req)
	if err != nil {
		d.warn(requestName(req) + ": " + rpcErr(err))
		return nil
	}
	return resp
}

func requestName(req *api.Request) string {
	switch req.GetRequest().(type) {
	case *api.Request_GetHistory:
		return "get_history"
	case *api.Request_DishGetObstructionMap:
		return "obstruction_map"
	default:
		return "request"
	}
}

// ---- history ring processing ----

type ingestResult struct {
	samples   int
	downBytes int64
	upBytes   int64
}

// ingest delta-processes the history ring via the `current` cursor.
// Reboots are detected by dish uptime regression, which disambiguates the
// cursor wrapping from an actual reset. On the first tick (and after a
// reboot) the whole ring is consumed so graphs are immediately complete;
// usage counters are gated per-sample by wall time (see consume) so a
// restart never re-counts the overlap.
func (d *daemon) ingest(h *dish.DishGetHistoryResponse, uptime uint64, haveUptime bool, now time.Time) ingestResult {
	cur := h.GetCurrent()
	if !d.sampled {
		d.resetArrays()
		d.sampled = true
		d.prevCur = cur
		d.markUptime(uptime, haveUptime)
		return d.consume(h, ringSize, int(cur), now)
	}

	var count int
	switch {
	case haveUptime && d.haveUptime && uptime < d.lastUptime:
		// Dish rebooted: cursor went backwards; the whole ring is new.
		d.resetArrays()
		count = ringSize
		d.vlog("history: dish reboot detected, ring reset")
	case cur >= d.prevCur:
		count = int(cur - d.prevCur)
	default: // normal ring wrap
		count = ringSize - int(d.prevCur) + int(cur)
	}
	if count > ringSize {
		count = ringSize // too far behind: re-read the whole ring
	}
	d.prevCur = cur
	d.markUptime(uptime, haveUptime)
	if count == 0 {
		return ingestResult{}
	}
	return d.consume(h, count, int(cur), now)
}

func (d *daemon) markUptime(uptime uint64, have bool) {
	if have {
		d.haveUptime = true
		d.lastUptime = uptime
	}
}

// consume applies the newest `count` samples ending at global index `newest`.
// Usage bytes are counted only for samples whose wall time (newest - offset
// seconds) postdates the daemon's start: the ring is always fully read for
// graphs, and this gate keeps a restart from re-counting the overlap.
//
// Firmware 2026.09.24+ fills skipped history seconds with zeroed samples
// (dropRate 0, latency 0 — impossible for a real ping). Those phantoms are
// forward-filled from the previous real sample so graphs and live numbers
// stay meaningful; they contribute no usage bytes.
func (d *daemon) consume(h *dish.DishGetHistoryResponse, count, newest int, now time.Time) ingestResult {
	lat := h.GetPopPingLatencyMs()
	down := h.GetDownlinkThroughputBps()
	up := h.GetUplinkThroughputBps()
	pow := h.GetPowerIn()
	drop := h.GetPopPingDropRate()
	hasPow := len(pow) == ringSize

	var res ingestResult
	// Oldest → newest so a phantom's carry source (the temporal predecessor)
	// is always already written.
	for i := count - 1; i >= 0; i-- {
		g := newest - i
		slot := ((g % ringSize) + ringSize) % ringSize
		phantom := drop[slot] < 1 && lat[slot] == 0
		if phantom {
			prev := ((g-1)%ringSize + ringSize) % ringSize
			d.arrays[aLat][slot] = d.arrays[aLat][prev]
			d.arrays[aDown][slot] = d.arrays[aDown][prev]
			d.arrays[aUp][slot] = d.arrays[aUp][prev]
			d.arrays[aPow][slot] = d.arrays[aPow][prev]
			d.arrays[aDrop][slot] = pf(float64(drop[slot]), 2)
			continue // phantom: no usage bytes
		}
		if drop[slot] >= 1 {
			d.arrays[aLat][slot] = nil // dropped ping
		} else {
			d.arrays[aLat][slot] = pf(float64(lat[slot]), 2)
		}
		d.arrays[aDown][slot] = pf(float64(down[slot])/1e6, 2)
		d.arrays[aUp][slot] = pf(float64(up[slot])/1e6, 2)
		if hasPow {
			d.arrays[aPow][slot] = pf(float64(pow[slot]), 1)
		} else {
			d.arrays[aPow][slot] = nil // powerIn absent on this hardware
		}
		d.arrays[aDrop][slot] = pf(float64(drop[slot]), 2)
		if now.Add(-time.Duration(newest-g) * time.Second).After(d.startWall) {
			res.downBytes += int64(down[slot] / 8)
			res.upBytes += int64(up[slot] / 8)
		}
	}
	newestSlot := ((newest % ringSize) + ringSize) % ringSize
	if hasPow {
		if p := d.arrays[aPow][newestSlot]; p != nil {
			d.lastPowerW = *p // carried through phantoms, so never a fake 0
		}
	}
	res.samples = count
	d.hasHist = true
	d.lastNewest = newest
	return res
}

func pf(v float64, n int) *float64 {
	r := round(v, n)
	return &r
}

// ---- dish-reported outages ----

func toOutages(list []*dish.DishOutage) []outage {
	out := make([]outage, 0, len(list))
	for _, o := range list {
		out = append(out, outage{
			Cause: o.GetCause().String(),
			Start: o.GetStartTimestampNs() / 1e9,
			DurS:  int64(math.Round(float64(o.GetDurationNs()) / 1e9)),
		})
	}
	return out
}

// observeDishOutages fires outage_start for an outage the dish reports as
// just beginning. Historical entries only prime the watermark. The freshness
// window derives from the ingest gap (a ring skimmed every 60 s still
// announces windows that open during the gap) but is capped: after hours
// parked away, outages that started (and likely ended) long ago are recorded
// silently instead of announced as new.
func (d *daemon) observeDishOutages(list []*dish.DishOutage, now time.Time, gap time.Duration) {
	window := int64(gap.Seconds()) + 2
	if window > int64(maxOutageAnnounceWindow.Seconds()) {
		window = int64(maxOutageAnnounceWindow.Seconds())
	}
	for _, o := range list {
		start := o.GetStartTimestampNs() / 1e9
		if start <= d.maxDishOutageStart {
			continue
		}
		fresh := gap > 0 && now.Unix()-start <= window
		d.maxDishOutageStart = start
		if fresh && d.openOutage == nil && d.warm {
			cause := o.GetCause().String()
			d.openOutage = &outageTrack{cause: cause, start: start}
			d.daily.Outages++
			d.emit(event{Type: "outage_start", Cause: cause},
				"Starlink offline", humanizeCause(cause))
		}
	}
}

// ---- transitions, alerts, hysteresis ----

func (d *daemon) handleTransitions(now time.Time, s *dish.DishGetStatusResponse, state string, dt time.Duration) {
	if d.warm && state != d.prevState {
		d.emit(event{Type: "state_change", From: d.prevState, To: state}, "", "")
		d.vlog("state %s -> %s", d.prevState, state)
	}
	d.prevState = state

	cur := map[string]bool{}
	for _, a := range decodeAlerts(s.GetAlerts()) {
		cur[a.ID] = true
	}
	if d.warm {
		for id := range cur {
			if !d.prevAlerts[id] {
				label := titleCase(id)
				d.emit(event{Type: "alert_set", ID: id, Label: label},
					"Starlink alert", label)
			}
		}
		for id := range d.prevAlerts {
			if !cur[id] {
				d.emit(event{Type: "alert_cleared", ID: id, Label: titleCase(id)}, "", "")
			}
		}
	}
	d.prevAlerts = cur

	fraction := float64(s.GetObstructionStats().GetFractionObstructed())
	if d.hyst.step(fraction, dt.Seconds()) {
		d.emit(event{Type: "obstruction_heavy", Fraction: round(fraction, 5)},
			"Starlink obstructed", fmt.Sprintf("%.1f%% obstruction, last 60s", fraction*100))
	}

	if !d.warm {
		d.warm = true // first good tick: state recorded, nothing announced
	}
}

// emit appends an event and, when summary is non-empty, sends a notification.
// Neither is ever fatal.
func (d *daemon) emit(ev event, summary, body string) {
	ev.Ts = time.Now().Unix()
	if err := appendEvent(d.cfg.dir, ev); err != nil {
		d.vlog("events: %v", err)
	}
	if summary != "" {
		if err := notifySend(summary, body); err != nil {
			d.vlog("notify: %v", err)
		}
	}
}

// ---- file builders ----

func (d *daemon) buildStatus(now time.Time, s *dish.DishGetStatusResponse, state, detail string) *statusFile {
	di := s.GetDeviceInfo()
	o := s.GetObstructionStats()
	totalOut, countOut := outagesInWindow(d.lastOutages, now.Unix(), 15*60)
	p50, p95, mean := latStats(d.arrays[aLat], 300)

	sf := &statusFile{
		Ts: now.Unix(),
		Ok: true,
		Dish: dishInfo{
			ID:        di.GetId(),
			HW:        di.GetHardwareVersion(),
			SW:        di.GetSoftwareVersion(),
			BootCount: int64(di.GetBootcount()),
			UptimeS:   int64(s.GetDeviceState().GetUptimeS()),
			EthMbps:   int64(s.GetEthSpeedMbps()),
		},
		State:       state,
		StateDetail: detail,
		Live: liveStats{
			LatencyMs: round(float64(s.GetPopPingLatencyMs()), 1),
			DownMbps:  round(float64(s.GetDownlinkThroughputBps())/1e6, 2),
			UpMbps:    round(float64(s.GetUplinkThroughputBps())/1e6, 2),
			PowerW:    round(d.lastPowerW, 1),
		},
		Lat5m: lat5mStats{P50: p50, P95: p95, Mean: mean},
		Drop15m: drop15Stats{
			Ratio:        dropMean(d.arrays[aDrop], ringSize),
			TotalOutageS: totalOut,
			Count15m:     countOut,
		},
		Usage: usageStats{
			DownGB:        round(float64(d.daily.DownBytes)/1e9, 2),
			UpGB:          round(float64(d.daily.UpBytes)/1e9, 2),
			AllTimeDownGB: round(float64(d.daily.AllTime.DownBytes)/1e9, 2),
			AllTimeUpGB:   round(float64(d.daily.AllTime.UpBytes)/1e9, 2),
		},
		Avail: availStats{
			TodayPct:   pct(d.daily.OnlineS, d.daily.TotalS),
			SessionPct: pct(d.sessionOnlineS, d.sessionS),
		},
		Obs: obsStats{
			Fraction:        round(float64(o.GetFractionObstructed()), 5),
			TimeObstructedS: int64(o.GetTimeObstructed() + 0.5),
			ValidS:          int64(o.GetValidS() + 0.5),
		},
		Alerts: decodeAlerts(s.GetAlerts()),
		Update: updateStats{
			State:       strings.TrimPrefix(s.GetSoftwareUpdateState().String(), "SOFTWARE_UPDATE_STATE_"),
			ProgressPct: round(float64(s.GetSoftwareUpdateStats().GetSoftwareUpdateProgress()), 2),
		},
		Gps: gpsStats{
			Valid: s.GetGpsStats().GetGpsValid(),
			Sats:  int64(s.GetGpsStats().GetGpsSats()),
		},
		Link: d.link,
	}
	if d.geo.Ts > 0 {
		g := d.geo
		sf.Geo = &g
	}
	d.lastObs = sf.Obs
	return sf
}

// buildStatusFail renders the contract's ok:false / OFFLINE shape, keeping
// last-known dish identity and observations so widgets degrade gracefully.
func (d *daemon) buildStatusFail(now time.Time, msg string) *statusFile {
	sf := &statusFile{
		Ts:     now.Unix(),
		Ok:     false,
		Error:  msg,
		State:  "OFFLINE",
		Alerts: []alert{},
		Obs:    d.lastObs,
		Drop15m: drop15Stats{
			Ratio: dropMean(d.arrays[aDrop], ringSize),
		},
	}
	if s := d.lastStatus; s != nil && s.GetDeviceInfo() != nil {
		di := s.GetDeviceInfo()
		sf.Dish = dishInfo{
			ID: di.GetId(), HW: di.GetHardwareVersion(), SW: di.GetSoftwareVersion(),
			BootCount: int64(di.GetBootcount()), EthMbps: int64(s.GetEthSpeedMbps()),
		}
	}
	sf.Usage = usageStats{
		DownGB:        round(float64(d.daily.DownBytes)/1e9, 2),
		UpGB:          round(float64(d.daily.UpBytes)/1e9, 2),
		AllTimeDownGB: round(float64(d.daily.AllTime.DownBytes)/1e9, 2),
		AllTimeUpGB:   round(float64(d.daily.AllTime.UpBytes)/1e9, 2),
	}
	sf.Avail = availStats{
		TodayPct:   pct(d.daily.OnlineS, d.daily.TotalS),
		SessionPct: pct(d.sessionOnlineS, d.sessionS),
	}
	sf.Link = d.link
	if d.geo.Ts > 0 {
		g := d.geo
		sf.Geo = &g
	}
	return sf
}

// buildHist emits the ring time-ordered (oldest→newest, per CONTRACT): the
// arrays are slot-indexed, so rotate around the newest sample's global index.
func (d *daemon) buildHist(now time.Time) *histFile {
	outs := make([]outage, len(d.lastOutages))
	copy(outs, d.lastOutages)
	sort.Slice(outs, func(i, j int) bool { return outs[i].Start > outs[j].Start })
	if len(outs) > 20 {
		outs = outs[:20]
	}
	rotated := func(a []*float64) []*float64 {
		out := make([]*float64, ringSize)
		base := d.lastNewest - (ringSize - 1) // global index of the oldest sample
		for k := 0; k < ringSize; k++ {
			out[k] = a[((base+k)%ringSize+ringSize)%ringSize]
		}
		return out
	}
	return &histFile{
		Ts:        now.Unix(),
		N:         ringSize,
		IntervalS: sampleIntervalS,
		LatencyMs: rotated(d.arrays[aLat]),
		DownMbps:  rotated(d.arrays[aDown]),
		UpMbps:    rotated(d.arrays[aUp]),
		PowerW:    rotated(d.arrays[aPow]),
		DropRate:  rotated(d.arrays[aDrop]),
		Outages:   outs,
		Map:       d.mapData,
	}
}

func toObsMap(m *dish.DishGetObstructionMapResponse) *obsMap {
	if m == nil {
		return nil
	}
	snr := make([]float64, len(m.GetSnr()))
	for i, v := range m.GetSnr() {
		snr[i] = round(float64(v), 2)
	}
	return &obsMap{
		Rows:        int(m.GetNumRows()),
		Cols:        int(m.GetNumCols()),
		MaxThetaDeg: round(float64(m.GetMaxThetaDeg()), 1),
		Snr:         snr,
	}
}

// pct is online/total as a percentage rounded to 2 decimals; 100 when empty.
func pct(online, total int64) float64 {
	if total <= 0 {
		return 100
	}
	return round(float64(online)/float64(total)*100, 2)
}

// outagesInWindow sums dish-reported outages starting within the window.
func outagesInWindow(outs []outage, now, windowS int64) (total, count int64) {
	for _, o := range outs {
		if o.Start >= now-windowS {
			count++
			total += o.DurS
		}
	}
	return total, count
}
