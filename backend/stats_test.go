package main

import (
	"testing"

	devicecommon "starlinkd/gen/spacex_api/device/common"
	dish "starlinkd/gen/spacex_api/device/dish"
	udc "starlinkd/gen/spacex_api/satellites/network/ut_disablement_codes"
)

func TestRound(t *testing.T) {
	tests := []struct {
		v    float64
		n    int
		want float64
	}{
		{27.381021, 2, 27.38},
		{0.05, 2, 0.05},
		{13.0423975, 1, 13},
		{0.00071, 4, 0.0007},
		{0.0021353483, 5, 0.00214},
		{872400000000, 2, 872400000000},
	}
	for _, tc := range tests {
		if got := round(tc.v, tc.n); got != tc.want {
			t.Errorf("round(%v, %d) = %v, want %v", tc.v, tc.n, got, tc.want)
		}
	}
}

func TestLatStats(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	// 4 samples, one dropped (nil): stats must ignore nils.
	series := []*float64{f(10), nil, f(20), f(30)}
	p50, p95, mean := latStats(series, 300)
	if p50 != 20 || p95 != 30 || mean != 20 {
		t.Errorf("latStats = %v, %v, %v; want 20, 30, 20", p50, p95, mean)
	}
	// Tail limit: only the newest 3 non-nil samples count.
	series2 := []*float64{f(100), f(100), f(1), f(2), f(3)}
	p50, _, mean = latStats(series2, 3)
	if p50 != 2 || mean != 2 {
		t.Errorf("tail latStats p50=%v mean=%v; want 2, 2", p50, mean)
	}
	if p50, p95, mean := latStats([]*float64{nil, nil}, 300); p50 != 0 || p95 != 0 || mean != 0 {
		t.Errorf("all-dropped latStats = %v %v %v; want zeros", p50, p95, mean)
	}
}

func TestDropMean(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	series := []*float64{f(0), f(0.05), f(1), f(0)}
	// mean over all = 1.05/4 = 0.2625 -> 0.2625 (4 decimals)
	if got := dropMean(series, 4); got != 0.2625 {
		t.Errorf("dropMean = %v, want 0.2625", got)
	}
	if got := dropMean([]*float64{nil}, 900); got != 0 {
		t.Errorf("dropMean of nils = %v, want 0", got)
	}
}

func TestSnakeAndTitle(t *testing.T) {
	tests := []struct {
		field, wantID, wantLabel string
	}{
		{"InstallPending", "install_pending", "Install pending"},
		{"ThermalThrottle", "thermal_throttle", "Thermal throttle"},
		{"SlowEthernetSpeeds_100", "slow_ethernet_speeds_100", "Slow ethernet speeds 100"},
		{"DbfTelemStale", "dbf_telem_stale", "Dbf telem stale"},
		{"UpsuRouterPortSlow", "upsu_router_port_slow", "Upsu router port slow"},
	}
	for _, tc := range tests {
		if got := snakeCase(tc.field); got != tc.wantID {
			t.Errorf("snakeCase(%q) = %q, want %q", tc.field, got, tc.wantID)
		}
		if got := titleCase(tc.wantID); got != tc.wantLabel {
			t.Errorf("titleCase(%q) = %q, want %q", tc.wantID, got, tc.wantLabel)
		}
	}
}

func TestDecodeAlerts(t *testing.T) {
	got := decodeAlerts(&dish.DishAlerts{
		InstallPending:         true,
		SlowEthernetSpeeds_100: true,
	})
	if len(got) != 2 {
		t.Fatalf("decodeAlerts len = %d, want 2", len(got))
	}
	if got[0].ID != "slow_ethernet_speeds_100" || got[0].Label != "Slow ethernet speeds 100" {
		t.Errorf("alerts[0] = %+v", got[0])
	}
	if got[1].ID != "install_pending" || got[1].Label != "Install pending" {
		t.Errorf("alerts[1] = %+v", got[1])
	}
	if empty := decodeAlerts(nil); empty == nil || len(empty) != 0 {
		t.Errorf("decodeAlerts(nil) = %#v, want empty non-nil slice", empty)
	}
}

func TestDeriveState(t *testing.T) {
	st := func(mutate func(*dish.DishGetStatusResponse)) *dish.DishGetStatusResponse {
		s := &dish.DishGetStatusResponse{
			DeviceInfo:           &devicecommon.DeviceInfo{},
			PopPingLatencyMs:     18,
			IsSnrAboveNoiseFloor: true,
			DeviceState:          &devicecommon.DeviceState{UptimeS: 5000},
		}
		if mutate != nil {
			mutate(s)
		}
		return s
	}
	tests := []struct {
		name       string
		st         *dish.DishGetStatusResponse
		wantState  string
		wantDetail string
	}{
		{"nil", nil, "NO_DATA", ""},
		{"no device info", &dish.DishGetStatusResponse{}, "NO_DATA", ""},
		{"online", st(nil), "ONLINE", ""},
		{"reboot pending", st(func(s *dish.DishGetStatusResponse) {
			s.SoftwareUpdateState = dish.SoftwareUpdateState_REBOOT_REQUIRED
		}), "ONLINE", "SOFTWARE_UPDATE_REBOOT_PENDING"},
		{"disablement", st(func(s *dish.DishGetStatusResponse) {
			s.DisablementCode = udc.UtDisablementCode_NO_ACTIVE_ACCOUNT
		}), "ONLINE", "NO_ACTIVE_ACCOUNT"},
		{"thermal", st(func(s *dish.DishGetStatusResponse) {
			s.Alerts = &dish.DishAlerts{ThermalShutdown: true}
			s.PopPingLatencyMs = 0
		}), "THERMAL_SHUTDOWN", ""},
		{"booting", st(func(s *dish.DishGetStatusResponse) {
			s.PopPingLatencyMs = 0
			s.IsSnrAboveNoiseFloor = false
			s.DeviceState.UptimeS = 60
		}), "BOOTING", ""},
		{"searching", st(func(s *dish.DishGetStatusResponse) {
			s.PopPingLatencyMs = 0
			s.IsSnrAboveNoiseFloor = false
		}), "SEARCHING", ""},
		{"no signal", st(func(s *dish.DishGetStatusResponse) {
			s.PopPingLatencyMs = 0
		}), "NO_SIGNAL", ""},
	}
	for _, tc := range tests {
		gotState, gotDetail := deriveState(tc.st)
		if gotState != tc.wantState || gotDetail != tc.wantDetail {
			t.Errorf("%s: deriveState = %q,%q; want %q,%q",
				tc.name, gotState, gotDetail, tc.wantState, tc.wantDetail)
		}
	}
}

func TestHysteresis(t *testing.T) {
	// Fire after 60s above 0.01; re-arm after 300s below 0.005.
	h := newHyst(60, 300)
	for i := 0; i < 29; i++ {
		if h.step(0.02, 2) {
			t.Fatalf("fired early at step %d", i)
		}
	}
	if !h.step(0.02, 2) {
		t.Error("did not fire after 60s sustained")
	}
	if h.step(0.02, 2) {
		t.Error("refired while already fired")
	}
	// Mid-band holds state.
	for i := 0; i < 10; i++ {
		if h.step(0.007, 2) {
			t.Error("fired/changed in mid-band")
		}
	}
	// Below threshold: not re-armed until 300s accumulate.
	for i := 0; i < 149; i++ {
		if h.step(0.001, 2) {
			t.Error("fired while cooling down")
		}
	}
	if !h.fired {
		t.Error("re-armed early, after only 298s below threshold")
	}
	if h.step(0.001, 2) { // reaches 300s
		t.Error("fired during re-arm tick")
	}
	if h.fired {
		t.Error("hysteresis did not re-arm after 300s below threshold")
	}
	// Fresh alarm cycle after re-arm.
	for i := 0; i < 29; i++ {
		h.step(0.02, 2)
	}
	if !h.step(0.02, 2) {
		t.Error("did not re-fire after re-arm")
	}
}

func TestHumanizeCause(t *testing.T) {
	if got := humanizeCause("BOOTING"); got != "Booting" {
		t.Errorf("humanizeCause(BOOTING) = %q", got)
	}
	if got := humanizeCause("OFFLINE"); got != "Offline" {
		t.Errorf("humanizeCause(OFFLINE) = %q", got)
	}
}

func TestOutagesInWindow(t *testing.T) {
	now := int64(1760000000)
	outs := []outage{
		{Cause: "BOOTING", Start: now - 10, DurS: 60},
		{Cause: "OBSTRUCTED", Start: now - 900, DurS: 30}, // exactly at edge: inside
		{Cause: "BOOTING", Start: now - 2000, DurS: 500},  // outside
	}
	total, count := outagesInWindow(outs, now, 900)
	if total != 90 || count != 2 {
		t.Errorf("outagesInWindow = %d, %d; want 90, 2", total, count)
	}
}

func TestPct(t *testing.T) {
	if got := pct(0, 0); got != 100 {
		t.Errorf("pct(0,0) = %v, want 100", got)
	}
	if got := pct(1, 3); got != 33.33 {
		t.Errorf("pct(1,3) = %v, want 33.33", got)
	}
}
