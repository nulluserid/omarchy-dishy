package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	common "starlinkd/gen/spacex_api/device/common"
	api "starlinkd/gen/spacex_api/device/device"
	dish "starlinkd/gen/spacex_api/device/dish"
)

func TestFetchGeoHTTPBoundary(t *testing.T) {
	var redirected atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
		_, _ = w.Write([]byte(`{"asn":"AS14593"}`))
	}))
	defer destination.Close()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		wantOK bool
	}{
		{"normal", 200, `{"ip":"203.0.113.1","asn":"AS14593","asn_org":"Starlink"}`, true},
		{"oversize", 200, `{"city":"` + strings.Repeat("x", maxGeoBytes) + `"}`, false},
		{"trailing oversize", 200, `{}` + strings.Repeat(" ", maxGeoBytes), false},
		{"invalid", 200, `{`, false},
		{"redirect", 302, "", false},
		{"error", 503, `{}`, false},
		// The dial is IPv4; the answer has to be too. None of these is an
		// IPv4 address, and each used to be published as the machine's.
		{"ipv6 address", 200, `{"ip":"2001:db8::1","asn":"AS14593","asn_org":"Starlink"}`, false},
		{"ipv4-mapped ipv6 address", 200, `{"ip":"::ffff:203.0.113.1","asn":"AS14593"}`, false},
		{"malformed address", 200, `{"ip":"` + strings.Repeat("1", 300) + `","asn":"AS14593"}`, false},
		{"no address", 200, `{"asn":"AS14593","asn_org":"Starlink"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", destination.URL)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			g, err := fetchGeoURL(t0, srv.URL)
			if (err == nil) != tc.wantOK {
				t.Fatalf("fetch = %+v, %v", g, err)
			}
			if tc.wantOK {
				if !g.IsStarlink || g.IP != "203.0.113.1" || g.Ts != t0.Unix() {
					t.Fatalf("unexpected geo: %+v", g)
				}
			} else {
				d := newDaemon(config{}, nil)
				before := geoInfo{Ts: t0.Unix(), IP: "198.51.100.1"}
				d.geo = before
				d.applyGeo(g)
				if d.geo != before {
					t.Fatal("failed lookup replaced last good geo")
				}
				if d.lastGeoFailUnix == 0 {
					t.Fatal("failed lookup did not anchor the failure backoff")
				}
			}
		})
	}
	// The provider's strings are the provider's to size, and each lands in
	// every status.json until the next lookup: oversized, control-laden or
	// bidi-spoofed values are clipped at the boundary, never copied whole.
	// The leading ESC / newline / bidi overrides are dropped before the cap
	// counts, so the kept text is the full cap of clean runes. (The address
	// is the one field that is validated instead — see the table above.)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"203.0.113.7","city":"\u001b\u202e` + strings.Repeat("c", 300) +
			`","region_name":"` + strings.Repeat("r", 300) + `","country_iso":"` + strings.Repeat("U", 40) +
			`","asn":"` + strings.Repeat("A", 64) + `","asn_org":"\n\u2066` + strings.Repeat("o", 300) + `"}`))
	}))
	defer srv.Close()
	if g, err := fetchGeoURL(t0, srv.URL); err != nil || g != (geoInfo{
		Ts:      t0.Unix(),
		IP:      "203.0.113.7",
		City:    strings.Repeat("c", maxGeoTextRunes),
		Region:  strings.Repeat("r", maxGeoTextRunes),
		Country: strings.Repeat("U", maxGeoCountryRunes),
		ASN:     strings.Repeat("A", maxGeoASNRunes),
		Org:     strings.Repeat("o", maxGeoTextRunes),
	}) {
		t.Fatalf("oversize geo = %+v, %v", g, err)
	}
	if redirected.Load() {
		t.Fatal("redirect destination was contacted")
	}
}

func TestDialRejectsURLTargets(t *testing.T) {
	for _, target := range []string{"dns:///dish.local:9200", "unix:///tmp/dish:9200", "dish.local/path:9200", "user@dish.local:9200", "dish.local?x:9200", "dish.local#x:9200", "dish%2elocal:9200", `dish\local:9200`} {
		if _, err := normalizeTarget(target); err == nil {
			t.Errorf("normalize accepted %q", target)
		}
		if conn, err := dial(target); err == nil {
			conn.Close()
			t.Errorf("dial accepted %q", target)
		}
	}
	for _, target := range []string{"192.168.100.1:9200", "dish.local:9200", "[::1]:9200", "unix:9200", "unix-abstract:9200", "dns:9200", "passthrough:9200"} {
		conn, err := dial(target)
		if err != nil {
			t.Fatalf("dial rejected %q: %v", target, err)
		}
		if got, want := conn.CanonicalTarget(), "dns:///"+target; got != want {
			t.Errorf("dial(%q) canonical target = %q, want %q", target, got, want)
		}
		conn.Close()
	}
}

// stubClient routes Handle by request kind so full goodTick/failTick cycles
// run without a dish.
type stubClient struct {
	status  *dish.DishGetStatusResponse
	history *dish.DishGetHistoryResponse
	mapRes  *dish.DishGetObstructionMapResponse
}

func (c *stubClient) Handle(ctx context.Context, in *api.Request, opts ...grpc.CallOption) (*api.Response, error) {
	switch in.GetRequest().(type) {
	case *api.Request_GetStatus:
		return &api.Response{Response: &api.Response_DishGetStatus{DishGetStatus: c.status}}, nil
	case *api.Request_GetHistory:
		return &api.Response{Response: &api.Response_DishGetHistory{DishGetHistory: c.history}}, nil
	case *api.Request_DishGetObstructionMap:
		return &api.Response{Response: &api.Response_DishGetObstructionMap{DishGetObstructionMap: c.mapRes}}, nil
	default:
		return nil, errors.New("stub: unknown request")
	}
}

func (c *stubClient) Stream(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[api.ToDevice, api.FromDevice], error) {
	return nil, errors.New("stub: unused")
}

// onlineStatus builds a minimal status that deriveState maps to ONLINE.
func onlineStatus() *dish.DishGetStatusResponse {
	s := &dish.DishGetStatusResponse{}
	s.DeviceInfo = &common.DeviceInfo{Id: "ut-test", HardwareVersion: "mini", SoftwareVersion: "sw"}
	s.PopPingLatencyMs = 20
	s.DeviceState = &common.DeviceState{UptimeS: 600}
	return s
}

func mkStub() *stubClient {
	return &stubClient{status: onlineStatus(), history: mkHist(5, []float32{20}, []float32{0})}
}

func readJSON(t *testing.T, dir, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return m
}

func TestGoodTickIdleSkipsHistWrite(t *testing.T) {
	dir := t.TempDir()
	d := newDaemon(config{interval: 2 * time.Second, idleInterval: 10 * time.Second, idleHistInterval: 60 * time.Second, mapInterval: time.Minute, geoInterval: 5 * time.Minute, geoTransitionInterval: time.Second, dir: dir}, mkStub())
	d.startWall = t0.Add(-time.Hour)

	d.goodTick(t0, onlineStatus(), false, false, 10*time.Second)

	if _, err := os.Stat(filepath.Join(dir, "hist.json")); !os.IsNotExist(err) {
		t.Fatal("idle tick wrote hist.json")
	}
	sf := readJSON(t, dir, "status.json")
	link, _ := sf["link"].(map[string]any)
	if link["onStarlink"] != true {
		t.Fatalf("link.onStarlink = %v, want true", link["onStarlink"])
	}
	if link["reason"] != "dish" {
		t.Fatalf("link.reason = %v, want dish", link["reason"])
	}
	if sf["ts"].(float64) != float64(t0.Unix()) {
		t.Fatalf("status ts = %v", sf["ts"])
	}
	df := readJSON(t, filepath.Join(dir, "dishes", "ut-test"), "daily.json")
	if df["totalS"].(float64) != 10 {
		t.Fatalf("daily.totalS = %v, want 10 (elapsed dt)", df["totalS"])
	}
	if df["onlineS"].(float64) != 10 {
		t.Fatalf("daily.onlineS = %v, want 10", df["onlineS"])
	}
}

func TestGoodTickActiveWritesHist(t *testing.T) {
	dir := t.TempDir()
	c := mkStub()
	c.mapRes = &dish.DishGetObstructionMapResponse{NumRows: 4, NumCols: 4, Snr: make([]float32, 16)}
	d := newDaemon(config{interval: 2 * time.Second, idleHistInterval: 60 * time.Second, geoInterval: 5 * time.Minute, geoTransitionInterval: time.Second, dir: dir}, c)
	d.startWall = t0.Add(-time.Hour)

	d.goodTick(t0, onlineStatus(), true, true, 2*time.Second)

	if _, err := os.Stat(filepath.Join(dir, "hist.json")); err != nil {
		t.Fatalf("active tick did not write hist.json: %v", err)
	}
	hf := readJSON(t, dir, "hist.json")
	if hf["map"] == nil {
		t.Fatal("active tick did not refresh the obstruction map into hist")
	}
}

func TestFailTickOffNetworkFreeze(t *testing.T) {
	dir := t.TempDir()
	sent := 0
	realSend := notifySend
	notifySend = func(string, string) error { sent++; return nil }
	defer func() { notifySend = realSend }()

	d := newDaemon(config{interval: 2 * time.Second, idleInterval: 10 * time.Second, idleHistInterval: 60 * time.Second, dir: dir}, mkStub())
	d.cfg.dir = dir
	d.startWall = t0.Add(-time.Hour)
	d.daily = dailyFile{DownBytes: 5, UpBytes: 1, OnlineS: 100, TotalS: 100, AllTime: dailyAllTime{DownBytes: 9, UpBytes: 3}}

	// Machine parked on a non-Starlink network: classify off via fresh geo.
	d.geo = geoInfo{Ts: t0.Unix() - 30, IsStarlink: false, ASN: "AS11404", Org: "Qwest"}
	d.link = linkInfo{OnStarlink: true, Reason: "dish", Since: t0.Unix() - 600}
	d.lastPositive = t0.Unix() - 600

	d.failTick(t0, "refused", 10*time.Second, false)
	d.failTick(t0.Add(10*time.Second), "refused", 10*time.Second, false)
	if sent != 0 {
		t.Fatalf("off-network outage sent %d notifications, want 0", sent)
	}
	if d.openOutage != nil {
		t.Fatal("off-network failure opened an outage track")
	}
	if d.daily.DownBytes != 5 || d.daily.UpBytes != 1 || d.daily.OnlineS != 100 || d.daily.TotalS != 100 {
		t.Fatalf("daily counters moved while off-network: %+v", d.daily)
	}
	if d.sessionS != 0 {
		t.Fatalf("sessionS = %d, want 0", d.sessionS)
	}
	sf := readJSON(t, dir, "status.json")
	link, _ := sf["link"].(map[string]any)
	if link["onStarlink"] != false {
		t.Fatalf("link.onStarlink = %v, want false", link["onStarlink"])
	}
	if link["reason"] != "egress-other" {
		t.Fatalf("link.reason = %v, want egress-other", link["reason"])
	}
}

func TestUpdateLinkGraceExpiry(t *testing.T) {
	d := newDaemon(config{target: "192.168.100.1:9200"}, mkStub())
	// On-network then the machine goes fully offline (no geo lookup, no dish
	// answer): stays visible through the grace window, then hides. The walk
	// uses the real idle cadence — a one-shot 11-minute jump would mask a
	// grace tick renewing the anchor (the bug this pins: ONLY real evidence
	// may renew d.lastPositive, never a grace hold).
	d.link = linkInfo{OnStarlink: true, Reason: "dish", Since: t0.Unix() - 7200}
	d.lastPositive = t0.Unix() - 30 // last real evidence right before the walk

	for i := 1; i <= 57; i++ { // t0+10s .. t0+570s: still within 10 min
		now := t0.Add(time.Duration(i) * 10 * time.Second)
		d.updateLink(now, false)
		if !d.link.OnStarlink || d.link.Reason != "grace" {
			t.Fatalf("tick %d: %+v, want on-starlink grace hold", i, d.link)
		}
		if d.lastPositive != t0.Unix()-30 {
			t.Fatalf("tick %d: grace renewed the anchor to %d", i, d.lastPositive)
		}
	}
	anchor := d.lastPositive
	for i := 58; i <= 65; i++ { // crosses the 10-minute mark
		d.updateLink(t0.Add(time.Duration(i)*10*time.Second), false)
	}
	if d.link.OnStarlink {
		t.Fatalf("grace expired but link stayed on: %+v", d.link)
	}
	if d.link.Reason != "grace" {
		t.Fatalf("reason = %q, want grace", d.link.Reason)
	}
	if d.lastPositive != anchor {
		t.Fatal("anchor moved during expiry")
	}
	if d.link.Since != t0.Unix()+580 {
		t.Fatalf("Since = %d, want the flip at t0+580", d.link.Since)
	}
}

func TestGraceHoldNoOutageNotificationsOrBooking(t *testing.T) {
	dir := t.TempDir()
	sent := 0
	realSend := notifySend
	notifySend = func(string, string) error { sent++; return nil }
	defer func() { notifySend = realSend }()

	d := newDaemon(config{interval: 10 * time.Second, idleInterval: 10 * time.Second, idleHistInterval: 60 * time.Second, dir: dir}, mkStub())
	d.cfg.dir = dir
	d.warm = true
	d.link = linkInfo{OnStarlink: true, Reason: "dish", Since: t0.Unix() - 7200}
	d.lastPositive = t0.Unix() - 30

	// 15+ minutes of failing ticks with no evidence anywhere (machine left,
	// nothing classifies it): the grace hold must never notify, never open an
	// outage, and — with confirmed-evidence booking — never book downtime.
	for i := 0; i < 95; i++ {
		d.failTick(t0.Add(time.Duration(i)*10*time.Second), "refused", 10*time.Second, false)
	}
	if sent != 0 {
		t.Fatalf("grace hold produced %d outage notifications, want 0", sent)
	}
	if d.openOutage != nil {
		t.Fatal("grace hold opened an outage track")
	}
	if d.daily.TotalS != 0 || d.sessionS != 0 || d.daily.Outages != 0 {
		t.Fatalf("grace hold booked downtime: daily=%+v sessionS=%d", d.daily, d.sessionS)
	}
	if d.link.OnStarlink {
		t.Fatal("15 min with no evidence never converged to hidden")
	}
}

func TestSuspendClamp(t *testing.T) {
	dir := t.TempDir()
	d := newDaemon(config{
		interval: 2 * time.Second, idleInterval: 10 * time.Second,
		idleHistInterval: 60 * time.Second, mapInterval: time.Minute,
		geoInterval: time.Hour, geoTransitionInterval: time.Hour, dir: dir,
	}, mkStub())
	d.warm = true
	d.geo.Ts = time.Now().Unix() // fresh: maybeFetchGeo must not fire a live lookup
	// Simulate suspend: last tick 8 h ago. The booked dt must be capped so
	// the whole sleep never lands in daily/session counters.
	d.lastTickAt = time.Now().Add(-8 * time.Hour)
	d.tick(false)
	if d.daily.TotalS != 30 || d.sessionS != 30 {
		t.Fatalf("suspend booked TotalS=%d sessionS=%d, want 30/30 (capped)", d.daily.TotalS, d.sessionS)
	}
}

func TestAnnounceWindowCap(t *testing.T) {
	dir := t.TempDir()
	sent := 0
	realSend := notifySend
	notifySend = func(string, string) error { sent++; return nil }
	defer func() { notifySend = realSend }()

	d := newDaemon(config{dir: dir}, mkStub())
	d.cfg.dir = dir
	d.warm = true
	// Returning from a long stretch away (2 h ingest gap): an outage that
	// started (and probably ended) an hour ago must watermark only,
	// while one that truly just started still announces.
	old := []*dish.DishOutage{{
		Cause:            dish.DishOutage_BOOTING,
		StartTimestampNs: (t0.Unix() - 3600) * 1e9,
		DurationNs:       60 * 1e9,
	}}
	d.observeDishOutages(old, t0, 2*time.Hour)
	if sent != 0 || d.openOutage != nil {
		t.Fatalf("stale outage announced on resume: sent=%d track=%v", sent, d.openOutage)
	}
	if d.maxDishOutageStart != t0.Unix()-3600 {
		t.Fatalf("watermark not advanced: %d", d.maxDishOutageStart)
	}
	recent := []*dish.DishOutage{{
		Cause:            dish.DishOutage_BOOTING,
		StartTimestampNs: (t0.Unix() - 30) * 1e9,
		DurationNs:       20 * 1e9,
	}}
	d.observeDishOutages(recent, t0.Add(time.Second), 2*time.Hour)
	if sent != 1 || d.openOutage == nil {
		t.Fatalf("recent outage not announced: sent=%d track=%v", sent, d.openOutage)
	}
}

func TestUpdateLinkEgressEvidence(t *testing.T) {
	d := newDaemon(config{target: "192.168.100.1:9200"}, mkStub())

	// Fresh Starlink-side geo while the dish is unreachable: egress keeps us shown.
	d.geo = geoInfo{Ts: t0.Unix() - 10, IsStarlink: true, ASN: starlinkASN, Org: "SpaceX"}
	d.updateLink(t0, false)
	if !d.link.OnStarlink || d.link.Reason != "egress" {
		t.Fatalf("egress evidence: %+v", d.link)
	}

	// Fresh non-starlink geo: classifies off immediately (no 10-min grace).
	d.geo = geoInfo{Ts: t0.Unix() - 10, IsStarlink: false, ASN: "AS3320", Org: "Deutsche Telekom"}
	d.updateLink(t0.Add(time.Second), false)
	if d.link.OnStarlink || d.link.Reason != "egress-other" {
		t.Fatalf("egress-other evidence: %+v", d.link)
	}
	if d.link.Since != t0.Add(time.Second).Unix() {
		t.Fatalf("Since = %d, want the flip time", d.link.Since)
	}

	// Stale geo beyond the grace window counts as no evidence: grace branch.
	d.geo = geoInfo{Ts: t0.Unix() - 660}
	d.link = linkInfo{OnStarlink: false, Reason: "egress-other", Since: t0.Unix() - 3600}
	d.updateLink(t0, false)
	if d.link.Reason != "grace" {
		t.Fatalf("stale geo reason = %q, want grace", d.link.Reason)
	}

	// Lookup cadence (maybeFetchGeo): the transition clock applies only while
	// classifying. A settled off-network machine — a fresh answer naming
	// another carrier — is a finished classification and runs on the settled
	// clock: no lookup at transition age. Keying it on the marker instead
	// (onStarlink false) asked every 60 s for as long as the machine was away.
	d.cfg.geoInterval, d.cfg.geoTransitionInterval = 5*time.Minute, time.Minute
	d.geo = geoInfo{Ts: t0.Unix() - 60, IsStarlink: false, ASN: "AS3320", Org: "Deutsche Telekom"}
	d.link = linkInfo{OnStarlink: false, Reason: "egress-other", Since: t0.Unix() - 3600}
	d.maybeFetchGeo(t0, false)
	if d.geoFetching {
		t.Fatal("settled off-network answer re-queried on the transition clock, want the settled clock")
	}

	// Failure backoff: with no answer at all the clock is the transition one,
	// but a lookup that failed less than geoInterval ago holds it — an
	// offline machine was otherwise asked on every tick.
	d.geo = geoInfo{}
	d.lastGeoFailUnix = t0.Unix() - 4*60
	d.maybeFetchGeo(t0, false)
	if d.geoFetching {
		t.Fatal("lookup started inside the failure backoff")
	}
	// Only the negative side is pinned: a started lookup is a real request
	// leaving the process, so the due cases are covered by the daemon's
	// verbose log, not here.
}

func TestApplyGeoDoesNotStampDish(t *testing.T) {
	// applyGeo recomputes the geo dimension only: a good standing belief
	// keeps reason "dish" without claiming a fresh RPC (the outage gate
	// treats link.lastDishOK as "identity-verified answer", so nothing but
	// goodTick may advance it). Without the belief, geo alone reclassifies.
	d := newDaemon(config{}, mkStub())
	now := time.Now()
	d.warm = true
	d.lastDishOK = true
	d.link = linkInfo{OnStarlink: true, Reason: "dish", Since: now.Unix() - 3600, LastDishOK: now.Unix() - 30}
	d.geo = geoInfo{Ts: now.Unix() - 3600} // old: forces a fresh result through

	d.applyGeo(geoInfo{Ts: now.Unix(), IsStarlink: false, ASN: "AS11404", Org: "Qwest"})
	if d.link.Reason != "dish" {
		t.Fatalf("good belief overruled by geo: reason=%q", d.link.Reason)
	}
	if d.link.LastDishOK != now.Unix()-30 {
		t.Fatalf("applyGeo advanced lastDishOK to %d, want untouched %d", d.link.LastDishOK, now.Unix()-30)
	}

	d.lastDishOK = false
	d.applyGeo(geoInfo{Ts: now.Unix() + 1, IsStarlink: false, ASN: "AS11404", Org: "Qwest"})
	if d.link.OnStarlink || d.link.Reason != "egress-other" {
		t.Fatalf("fresh geo without dish belief: %+v", d.link)
	}
}

func TestGeoPublishExpires(t *testing.T) {
	// The last-good policy is bounded: a failed lookup keeps the previous
	// result, but one older than geoMaxAge without a successful lookup is
	// no longer published — the egress line goes away instead of showing
	// an address the machine may have left (moved to IPv6-only: from then
	// on every lookup fails).
	d := newDaemon(config{dir: t.TempDir()}, mkStub())
	d.startWall = t0.Add(-time.Hour)
	d.geo = geoInfo{IP: "203.0.113.7", IsStarlink: true, ASN: starlinkASN, Org: "Starlink"}
	for _, tc := range []struct {
		age     time.Duration
		publish bool
	}{
		{geoMaxAge, true}, {geoMaxAge + time.Second, false}, {time.Hour, false},
	} {
		d.geo.Ts = t0.Unix() - int64(tc.age.Seconds())
		// Both status builders gate the same way, and what a tick persists
		// is what the panel reads: the geo key is in status.json, or absent.
		if got := d.buildStatusFail(t0, "refused").Geo; (got != nil) != tc.publish {
			t.Errorf("fail tick, answer %v old: geo published = %v", tc.age, got != nil)
		}
		d.goodTick(t0, onlineStatus(), false, false, 2*time.Second)
		if _, in := readJSON(t, d.cfg.dir, "status.json")["geo"]; in != tc.publish {
			t.Errorf("good tick, answer %v old: geo in status.json = %v", tc.age, in)
		}
	}
}

func TestDefaultRouteFingerprint(t *testing.T) {
	fp, ok := defaultRouteFingerprint()
	if !ok || fp == "" {
		t.Skip("no default route on this machine")
	}
	// Fingerprint is "iface/gateway" and stable across calls.
	if fp2, ok2 := defaultRouteFingerprint(); !ok2 || fp2 != fp {
		t.Fatalf("fingerprint unstable: %q vs %q", fp, fp2)
	}
}

func TestControlSocketRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	d := newDaemon(config{interval: 2 * time.Second, idleInterval: 10 * time.Second}, mkStub())
	if err := d.startControl(); err != nil {
		t.Fatalf("startControl: %v", err)
	}

	if code := runPanel([]string{"bogus"}); code != 2 {
		t.Fatalf("usage exit = %d, want 2", code)
	}
	if code := runPanel([]string{"open"}); code != 0 {
		t.Fatalf("panel open exit = %d, want 0", code)
	}
	select {
	case got := <-d.ctrlCh:
		if got != "open" {
			t.Fatalf("cmd = %q, want open", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no command received over the control socket")
	}

	// Old daemons keep working at idle cadence without a listener.
	code := runPanel([]string{"close"})
	if code != 0 {
		t.Fatalf("panel close exit = %d, want 0", code)
	}
	select {
	case got := <-d.ctrlCh:
		if got != "close" {
			t.Fatalf("cmd = %q, want close", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close command not received")
	}
}

func TestApplyControlLeaseAndDelay(t *testing.T) {
	d := newDaemon(config{interval: 2 * time.Second, idleInterval: 10 * time.Second}, mkStub())
	now := time.Now()

	if d.nextDelay() != 10*time.Second {
		t.Fatal("idle daemon should wait idleInterval")
	}
	d.applyControl("open", now)
	if d.nextDelay() != 2*time.Second {
		t.Fatal("open panel should wait the fast interval")
	}
	d.applyControl("close", now)
	if d.nextDelay() != 10*time.Second {
		t.Fatal("closed panel should return to idle")
	}
}

func TestPostTickProbeOneShot(t *testing.T) {
	d := newDaemon(config{interval: 2 * time.Second, idleInterval: 10 * time.Second}, mkStub())
	now := time.Now()
	d.postTick(now, true)
	if !now.Before(d.probeUntil) && !d.probeUntil.IsZero() {
		t.Fatal("success set a probe")
	}
	d.postTick(now, false)
	if !now.Before(d.probeUntil) {
		t.Fatal("first failure did not start the probe burst")
	}
	// Persisting failure: the burst is one-shot, not extended every tick.
	until := d.probeUntil
	d.postTick(now.Add(4*time.Second), false)
	if !d.probeUntil.Equal(until) {
		t.Fatal("probe extended on a persisting failure")
	}
	d.postTick(now.Add(8*time.Second), true) // recovery re-arms
	d.postTick(now.Add(12*time.Second), false)
	if !now.Add(12 * time.Second).Before(d.probeUntil) {
		t.Fatal("fresh failure after recovery did not re-probe")
	}

	// Re-arming is bounded all the same: a link flapping good/fail forever
	// must not keep the daemon at the 2 s cadence. Two re-arms were granted
	// above. One good->fail transition whose failing tick is `at`:
	flap := func(at time.Time) (before time.Time, armed bool) {
		before = d.probeUntil
		d.postTick(at.Add(-10*time.Second), true)
		d.postTick(at, false)
		return before, !d.probeUntil.Equal(before)
	}
	for i := 2; i < probeArmMax; i++ {
		if _, armed := flap(now.Add(time.Duration(26+50*(i-2)) * time.Second)); !armed {
			t.Fatalf("re-arm %d refused, want the first %d in the hour granted", i+1, probeArmMax)
		}
	}
	// The 21st, still inside the hour: no new burst, so the daemon stays on
	// the idle cadence — which still sees the dish recover.
	before, armed := flap(now.Add(926 * time.Second))
	if armed || !d.probeUntil.Equal(before) {
		t.Fatalf("re-arm %d inside the hour granted: probe until %v, want unchanged %v", probeArmMax+1, d.probeUntil, before)
	}
	if _, armed := flap(now.Add(probeArmWindow - 5*time.Second)); armed {
		t.Fatalf("re-arm granted before the oldest counted one was %v old", probeArmWindow)
	}
	// Once the oldest counted re-arm is an hour old, a transition re-arms again.
	if _, armed := flap(now.Add(probeArmWindow + 5*time.Second)); !armed {
		t.Fatal("re-arm still refused after the oldest counted one aged out")
	}
	if len(d.probeArms) != probeArmMax {
		t.Fatalf("%d re-arms counted, want %d", len(d.probeArms), probeArmMax)
	}
}

func TestOnLinkChangeClosesOutageSilently(t *testing.T) {
	dir := t.TempDir()
	sent := 0
	realSend := notifySend
	notifySend = func(string, string) error { sent++; return nil }
	defer func() { notifySend = realSend }()

	d := newDaemon(config{dir: dir}, mkStub())
	d.warm = true
	now := time.Unix(1760000000, 0)
	d.openOutage = &outageTrack{cause: "OFFLINE", start: now.Unix() - 30}

	d.onLinkChange(now, false) // machine left the Starlink network
	if d.openOutage != nil {
		t.Fatal("open outage survived the link flip")
	}
	if sent != 0 {
		t.Fatalf("link flip sent %d notifications, want 0", sent)
	}
	b, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if !strings.Contains(string(b), `"type":"outage_end"`) {
		t.Fatal("outage_end not logged on link flip")
	}
	if strings.Contains(string(b), `"type":"outage_start"`) {
		t.Fatal("unexpected outage_start on link flip")
	}
}
