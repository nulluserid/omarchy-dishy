package main

import (
	"container/heap"
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	grpcstatus "google.golang.org/grpc/status"

	api "starlinkd/gen/spacex_api/device/device"
	dish "starlinkd/gen/spacex_api/device/dish"
)

// dial connects to the dish. The connection is lazy; failures surface per
// RPC. Client keepalive pings retest a transport that has been quiet longer
// than any healthy cadence gap, so a silently stalled HTTP/2 session is
// reaped and re-dialed instead of timing out every RPC forever.
func dial(target string) (*grpc.ClientConn, error) {
	var err error
	target, err = normalizeTarget(target)
	if err != nil {
		return nil, err
	}
	// Treat even resolver-named hosts ("unix:9200", "dns:9200") as TCP
	// host:port, not opaque gRPC resolver URIs.
	return grpc.NewClient("dns:///"+target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                keepalivePingInterval,
			Timeout:             keepalivePongTimeout,
			PermitWithoutStream: true, // stalls happen between ticks, with no stream open
		}))
}

// rpcErr renders a failed Handle call as "code: message". The message is
// the peer's own text, hostile-sized on a hostile endpoint, and it is
// written into status.json on every fail tick — so the whole string is
// clipped, never trusted.
func rpcErr(err error) string {
	s := grpcstatus.Convert(err)
	return clip(s.Code().String()+": "+s.Message(), maxRPCErrorRunes)
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

// publishPendingTick writes status.json (no hist/day writes) for a debounced
// identity-switch candidate: nothing is booked, ingested, or transitioned
// until the same id persists, so a hostile rotating id never touches
// accumulated state.
func (d *daemon) publishPendingTick(now time.Time, s *dish.DishGetStatusResponse) {
	state, detail := deriveState(s)
	d.warn("")
	sf := d.buildStatus(now, s, state, detail)
	d.persist(now, sf, nil)
}

func (d *daemon) goodTick(now time.Time, s *dish.DishGetStatusResponse, active, fetchHist bool, dt time.Duration) {
	// Dish identity: accumulated state is per dish. A different id than the
	// loaded partition — including the first sighting after start — switches
	// before anything books (CONTRACT.md v4). A new id from a KNOWN dish must
	// persist for dishSwitchDebounceTicks consecutive good ticks first: a
	// hostile endpoint rotating ids per tick then never creates partitions.
	// The id and hardware string are taken clipped from the trust boundary
	// (identifyDish): the id compared here is the same one that reaches the
	// partition, every state file and the notification text.
	ident := identifyDish(s)
	if id := ident.ID; id != "" {
		switch {
		case d.dishID == "":
			d.switchDish(now, id, ident.HW) // first sighting adopts immediately
		case id == d.dishID:
			d.pendingID, d.pendingIDTicks = "", 0
		case id == d.pendingID:
			d.pendingIDTicks++
			if d.pendingIDTicks >= dishSwitchDebounceTicks {
				d.switchDish(now, id, ident.HW)
			} else {
				d.publishPendingTick(now, s)
				return
			}
		default:
			d.pendingID, d.pendingIDTicks = id, 1
			d.publishPendingTick(now, s)
			return
		}
	}
	state, detail := deriveState(s)
	d.lastStatus = s
	d.updateLink(now, true)
	// Identity-verified dish answer: advance the stamp exactly here — the
	// only path that ever does (applyGeo's link recompute carries reason
	// without stamping). The outage gate (failTick) treats this field as
	// "a real dish answer postdates X".
	d.link.LastDishOK = now.Unix()
	// Retention evidence (CONTRACT.md): the sweep's "connected" marker.
	// Fail ticks never reach this, so an adopted-but-never-reconnected
	// partition ages out even though its daily.json keeps being written
	// (pre-stamp files are seeded from mtime at load, freezing the stamp).
	d.daily.LastSeen = now.Unix()

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
		// Every array consume() indexes must be the full ring: a short or
		// hostile response (a LAN endpoint answering as the dish can send
		// 900 latencies with an empty drop array) is rejected whole, never
		// ingested — an index panic here would crash-loop the daemon.
		if hh := h.GetDishGetHistory(); hh != nil &&
			len(hh.GetPopPingLatencyMs()) == ringSize &&
			len(hh.GetPopPingDropRate()) == ringSize &&
			len(hh.GetDownlinkThroughputBps()) == ringSize &&
			len(hh.GetUplinkThroughputBps()) == ringSize {
			uptime, haveUp := uint64(0), false
			if ds := s.GetDeviceState(); ds != nil {
				uptime, haveUp = ds.GetUptimeS(), true
			}
			res = d.ingest(hh, uptime, haveUp, now)
			d.lastHist = now
			// Bounded once, at ingestion: both consumers below see the same
			// newest maxOutageEntries outages, never the delivered list whole.
			outages := newestOutages(hh.GetOutages(), maxOutageEntries)
			d.observeDishOutages(outages, now, ingestGap)
			d.lastOutages = toOutages(outages)
			histOK = true
		} else if h != nil {
			d.vlog("history: short response")
		}
	}

	// Obstruction map is only needed for the dashboard: skip entirely while
	// the panel is closed. On the first open the last map is minutes old —
	// good enough until the next active due tick refreshes it.
	if active && now.Sub(d.lastMap) >= d.cfg.mapInterval {
		if m := d.fetch(now, &api.Request{Request: &api.Request_DishGetObstructionMap{DishGetObstructionMap: &dish.DishGetObstructionMapRequest{}}},
			grpc.MaxCallRecvMsgSize(maxObsMapRecvBytes)); m != nil {
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

// fetch performs one unary Handle call; nil response on any failure. opts
// are per-call gRPC options — a reply cap belongs to the one RPC that needs
// it, never to the client-wide default the other calls share.
func (d *daemon) fetch(now time.Time, req *api.Request, opts ...grpc.CallOption) *api.Response {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	resp, err := d.client.Handle(ctx, req, opts...)
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
	if count < 0 {
		// A hostile `current` can wrap the uint64 delta past int: treat it
		// as too-far-behind and re-read the whole ring instead of skipping.
		count = ringSize
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
	// Defense in depth: goodTick rejects responses whose required arrays
	// are not the full ring; consume never indexes past the end even if a
	// future caller forgets (the slots below are bounded by ringSize).
	if len(lat) != ringSize || len(drop) != ringSize || len(down) != ringSize || len(up) != ringSize {
		d.vlog("history: short arrays, skipping ingest")
		return ingestResult{}
	}
	// A hostile response can carry NaN/Inf float32s: they would poison the
	// ring, the day aggregates and every marshaled document (encoding/json
	// refuses non-finite floats). Skip such samples entirely.
	nonFinite := func(v float32) bool { return math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) }

	real := false // at least one non-phantom sample consumed
	var res ingestResult
	// Oldest → newest so a phantom's carry source (the temporal predecessor)
	// is always already written.
	for i := count - 1; i >= 0; i-- {
		g := newest - i
		slot := ((g % ringSize) + ringSize) % ringSize
		if nonFinite(lat[slot]) || nonFinite(down[slot]) || nonFinite(up[slot]) ||
			nonFinite(drop[slot]) || (hasPow && nonFinite(pow[slot])) {
			continue // hostile non-finite sample: never stored or aggregated
		}
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
		real = true
		if now.Add(-time.Duration(newest-g) * time.Second).After(d.startWall) {
			res.downBytes += int64(down[slot] / 8)
			res.upBytes += int64(up[slot] / 8)
		}
		// Day store: only identity-verified dish samples ever reach this
		// loop (ingest runs inside goodTick), and the link gate keeps the
		// contract's off-network freeze explicit — parked elsewhere writes
		// no day history, same rule as daily.json's counters.
		if d.link.OnStarlink {
			wall := now.Add(-time.Duration(newest-g) * time.Second)
			d.feedSample(wall, *d.arrays[aDown][slot], *d.arrays[aUp][slot],
				d.arrays[aLat][slot], float64(drop[slot]), d.arrays[aPow][slot])
		}
	}
	newestSlot := ((newest % ringSize) + ringSize) % ringSize
	if hasPow {
		if p := d.arrays[aPow][newestSlot]; p != nil {
			d.lastPowerW = *p // carried through phantoms, so never a fake 0
		}
	}
	if real {
		d.lastSampleWall = now // freshness anchor for typicalDownNow
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

// maxOutageEntries bounds the dish-reported outage list kept from one
// history answer: newestOutages keeps the newest this many at ingestion, and
// observeDishOutages walks at most this many.
const maxOutageEntries = 100

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

// outageHeap is newestOutages' running "newest so far" set: a min-heap on the
// start timestamp, so the root is always the oldest kept entry — the one a
// newer delivered outage evicts.
type outageHeap []*dish.DishOutage

func (h outageHeap) Len() int { return len(h) }
func (h outageHeap) Less(i, j int) bool {
	return h[i].GetStartTimestampNs() < h[j].GetStartTimestampNs()
}
func (h outageHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *outageHeap) Push(x any)   { *h = append(*h, x.(*dish.DishOutage)) }
func (h *outageHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// newestOutages keeps the newest n outages by start time, oldest first. It
// does not lean on the dish's delivery order — the capture holds a single
// outage, so there is no documented order an end-truncation could trust —
// and it holds at most n entries: a list as long as the gRPC message limit
// allows costs one pass and no allocation that grows with it.
func newestOutages(list []*dish.DishOutage, n int) []*dish.DishOutage {
	if len(list) <= n {
		return list
	}
	h := make(outageHeap, n)
	copy(h, list[:n])
	heap.Init(&h)
	for _, o := range list[n:] {
		if o.GetStartTimestampNs() > h[0].GetStartTimestampNs() {
			h[0] = o // newer than the oldest kept one: evict it
			heap.Fix(&h, 0)
		}
	}
	// Pops come out oldest first, straight into the head of the delivered
	// slice (free by now), so the result reuses that backing array.
	for i := 0; i < n; i++ {
		list[i] = heap.Pop(&h).(*dish.DishOutage)
	}
	return list[:n]
}

// observeDishOutages fires outage_start for an outage the dish reports as
// just beginning. Historical entries only prime the watermark. The freshness
// window derives from the ingest gap (a ring skimmed every 60 s still
// announces windows that open during the gap) but is capped: after hours
// parked away, outages that started (and likely ended) long ago are recorded
// silently instead of announced as new.
func (d *daemon) observeDishOutages(list []*dish.DishOutage, now time.Time, gap time.Duration) {
	if len(list) > maxOutageEntries {
		list = list[:maxOutageEntries] // second guard: the tick path is already bounded
	}
	window := int64(gap.Seconds()) + 2
	if window > int64(maxOutageAnnounceWindow.Seconds()) {
		window = int64(maxOutageAnnounceWindow.Seconds())
	}
	for _, o := range list {
		start := o.GetStartTimestampNs() / 1e9
		// Future-dated starts can never announce and must not advance the
		// watermark (a single far-future start would poison it forever).
		nowS := now.Unix()
		if nowS-start < 0 {
			continue
		}
		if start <= d.maxDishOutageStart {
			continue
		}
		// The watermark is attacker-advanceable, so it alone cannot gate
		// announcements: reject starts outside the freshness window and
		// rate-limit announcements so a rotating-start flapper cannot fork
		// notify-send per tick.
		fresh := gap > 0 && nowS-start <= window
		if fresh && nowS-d.lastOutageAnnounce < outageReannounceS {
			fresh = false
		}
		d.maxDishOutageStart = start
		if fresh && d.openOutage == nil && d.warm {
			cause := o.GetCause().String()
			d.lastOutageAnnounce = nowS
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

// dishIdent is a dish's identity as the daemon may use it anywhere: the
// device id, hardware and software strings of an answer, clipped at the dish
// trust boundary. Every consumer — status.json on good and fail ticks, the
// identity comparison in goodTick, the partition key, config.json's
// lastDish, events.jsonl and the notification text — derives from this one
// conversion, so they all agree on the same bounded id.
type dishIdent struct {
	ID, HW, SW string
}

func identifyDish(s *dish.DishGetStatusResponse) dishIdent {
	di := s.GetDeviceInfo()
	return dishIdent{
		ID: clip(di.GetId(), maxDishStrRunes),
		HW: clip(di.GetHardwareVersion(), maxDishStrRunes),
		SW: clip(di.GetSoftwareVersion(), maxDishStrRunes),
	}
}

func (d *daemon) buildStatus(now time.Time, s *dish.DishGetStatusResponse, state, detail string) *statusFile {
	di := s.GetDeviceInfo()
	ident := identifyDish(s)
	o := s.GetObstructionStats()
	totalOut, countOut := outagesInWindow(d.lastOutages, now.Unix(), 15*60)
	p50, p95, mean := latStats(d.arrays[aLat], 300)

	sf := &statusFile{
		Ts: now.Unix(),
		Ok: true,
		Dish: dishInfo{
			ID:        ident.ID,
			HW:        ident.HW,
			SW:        ident.SW,
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

			TypicalDownMbps: typicalDownNow(&d.arrays, d.lastNewest, d.lastSampleWall, now),
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
		ident := identifyDish(s) // the same clipped identity the good ticks published
		sf.Dish = dishInfo{
			ID: ident.ID, HW: ident.HW, SW: ident.SW,
			BootCount: int64(s.GetDeviceInfo().GetBootcount()), EthMbps: int64(s.GetEthSpeedMbps()),
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

// Obstruction-map bounds at the dish trust boundary. The endpoint is
// unauthenticated, and the QML consumer visits every declared cell on the
// GUI thread while re-parsing hist.json every 2 s — an unbounded map is a
// shell stall the dish can trigger at will.
const (
	// maxObsMapDim caps each declared dimension. A real dish reports
	// 123x123 (the live Mini; the round dish reported the same grid in
	// 2021), so 256x256 leaves room without admitting stall-sized grids.
	maxObsMapDim = 256
	// maxObsMapRecvBytes caps the DishGetObstructionMap reply per call, not
	// client-wide: a map at the bound is 256 KiB on the wire (packed
	// float32), while GetHistory legitimately carries five 900-sample arrays
	// and keeps the gRPC default.
	maxObsMapRecvBytes = 1 << 20
)

// toObsMap converts a dish obstruction map into the published shape. The
// QML consumer loops rows*cols on the GUI thread (ObsMap.qml), so a response
// that is not a coherent, bounded grid is rejected WHOLE (nil) rather than
// truncated or repaired — never trust a partial view of the sky.
func toObsMap(m *dish.DishGetObstructionMapResponse) *obsMap {
	if m == nil {
		return nil
	}
	rows, cols := int(m.GetNumRows()), int(m.GetNumCols())
	// Dimensions first: rows*cols is evaluated only once both are bounded,
	// so uint32-max dims can never overflow the product.
	if rows < 1 || cols < 1 || rows > maxObsMapDim || cols > maxObsMapDim {
		return nil
	}
	// Exactly the declared grid. The old rule (rows*cols <= len(snr)) let a
	// response pad the file with every cell it delivered.
	if len(m.GetSnr()) != rows*cols {
		return nil
	}
	snr := make([]float64, rows*cols)
	for i, v := range m.GetSnr() {
		c := round(float64(v), 2)
		// CONTRACT.md: 0..1 per cell, -1 = no data. round() already maps
		// NaN/Inf to 0; the finite term is stated anyway so what this
		// accepts never silently depends on another function's behavior.
		if math.IsNaN(c) || math.IsInf(c, 0) || c < -1 || c > 1 {
			return nil
		}
		snr[i] = c
	}
	return &obsMap{
		Rows:        rows,
		Cols:        cols,
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
