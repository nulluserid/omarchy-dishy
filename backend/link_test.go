package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	api "starlinkd/gen/spacex_api/device/device"
	common "starlinkd/gen/spacex_api/device/common"
	dish "starlinkd/gen/spacex_api/device/dish"
)

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

// mkAddr builds one synthetic interface address (for subnet evidence tests).
func mkAddr(base string) net.Addr {
	_, ipn, _ := net.ParseCIDR(base)
	return ipn
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
	df := readJSON(t, dir, "daily.json")
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
	c.mapRes = &dish.DishGetObstructionMapResponse{}
	d := newDaemon(config{interval: 2 * time.Second, idleHistInterval: 60 * time.Second, geoInterval: 5 * time.Minute, geoTransitionInterval: time.Second, dir: dir}, c)
	d.startWall = t0.Add(-time.Hour)

	d.goodTick(t0, onlineStatus(), true, true, 2*time.Second)

	if _, err := os.Stat(filepath.Join(dir, "hist.json")); err != nil {
		t.Fatalf("active tick did not write hist.json: %v", err)
	}
	hf := readJSON(t, dir, "hist.json")
	if hf["map"] == nil {
		t.Fatal("active tick did not refresh the oblastmap into hist")
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
	// On-network then the machine goes fully offline (no geo, no route
	// evidence): stays visible through the grace window, then hides.
	d.link = linkInfo{OnStarlink: true, Reason: "dish", Since: t0.Unix() - 7200}
	d.lastPositive = t0.Unix() - 600

	d.updateLink(t0, false)
	if !d.link.OnStarlink || d.link.Reason != "grace" {
		t.Fatalf("within grace: %+v, want onSTARLINK/grace", d.link)
	}
	if d.link.Since != t0.Unix()-7200 {
		t.Fatalf("grace bump changed Since to %d", d.link.Since)
	}

	d.updateLink(t0.Add(11*time.Minute), false)
	if d.link.OnStarlink {
		t.Fatal("grace expired but link stayed on")
	}
	if d.link.Reason != "grace" {
		t.Fatalf("reason = %q, want grace", d.link.Reason)
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
}

func TestSubnetEvidence(t *testing.T) {
	if !addrInSubnet([]net.Addr{mkAddr("192.168.100.7/32")}, net.ParseIP("192.168.100.0"), net.CIDRMask(24, 32)) {
		t.Fatal("192.168.100.7 should match the dish /24")
	}
	if addrInSubnet([]net.Addr{mkAddr("10.5.5.60/32")}, net.ParseIP("192.168.100.0"), net.CIDRMask(24, 32)) {
		t.Fatal("10.5.5.60 must not match the dish /24")
	}
	// Dish target parsing: /24 base around the configured host.
	base, mask := dishSubnet("192.168.100.1:9200")
	if !base.Equal(net.ParseIP("192.168.100.0")) || mask.String() != net.CIDRMask(24, 32).String() {
		t.Fatalf("dishSubnet = %v %v", base, mask)
	}
	base, _ = dishSubnet("not-a-host:9200")
	if !base.Equal(net.ParseIP("192.168.100.0")) {
		t.Fatalf("dishSubnet fallback = %v", base)
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
}

func TestOnLinkChangeClosesOutageSilently(t *testing.T) {	dir := t.TempDir()
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
