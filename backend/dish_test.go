// Per-dish state partitions (CONTRACT.md v4): accumulated state switches
// with the dish's verified identity, never blends across dishes.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dish "starlinkd/gen/spacex_api/device/dish"
)

func TestDishDirName(t *testing.T) {
	cases := map[string]string{
		"ut4118910a-4432740c-1ab9659e": "ut4118910a-4432740c-1ab9659e", // real shape, unchanged
		"Ut31_ABC.def":                 "ut31_abc.def",
		"a/b\\c:d e":                   "a-b-c-d-e",
		"":                             "unknown",
		".":                            "unknown",
		"..":                           "unknown",
		strings.Repeat("x", 100):       strings.Repeat("x", 64),
	}
	for in, want := range cases {
		if got := dishDirName(in); got != want {
			t.Errorf("dishDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

// statusWithID returns an ONLINE status carrying the given device id.
func statusWithID(id string) *dish.DishGetStatusResponse {
	s := onlineStatus()
	s.DeviceInfo.Id = id
	return s
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestDishSwitchPartitionsKeepStats(t *testing.T) {
	dir := t.TempDir()
	c := mkStub()
	c.status = statusWithID("ut-aaa")
	d := newDaemon(config{
		interval: 2 * time.Second, idleInterval: 10 * time.Second,
		idleHistInterval: time.Minute, mapInterval: time.Minute,
		geoInterval: time.Hour, geoTransitionInterval: time.Hour, dir: dir,
	}, c)
	d.startWall = t0.Add(-time.Hour)

	// Two ticks on dish A. The first adopts A and resets the usage epoch
	// (its pre-adoption ring is not this machine's "today"); the second's
	// ring delta books normally.
	c.history = mkHist(10, []float32{20}, []float32{0})
	d.goodTick(t0, c.status, false, true, 2*time.Second)
	c.history = mkHist(12, []float32{20}, []float32{0})
	d.goodTick(t0.Add(2*time.Second), c.status, false, true, 2*time.Second)
	partA := filepath.Join(dir, "dishes", "ut-aaa")
	dfa := readJSON(t, partA, "daily.json")
	if dfa["totalS"].(float64) != 4 || dfa["downBytes"].(float64) != 2000 {
		t.Fatalf("dish A daily = %v, want totalS 4 and the 2-sample delta's 2000 bytes", dfa)
	}
	if _, err := os.Stat(filepath.Join(dir, "daily.json")); !os.IsNotExist(err) {
		t.Fatal("good ticks wrote daily.json outside the dish partition")
	}

	// Switch to dish B: a fresh partition, fresh session.
	c.status = statusWithID("ut-bbb")
	c.history = mkHist(500, []float32{30}, []float32{0})
	d.goodTick(t0.Add(4*time.Second), c.status, false, true, 2*time.Second)
	partB := filepath.Join(dir, "dishes", "ut-bbb")
	dfb := readJSON(t, partB, "daily.json")
	if dfb["totalS"].(float64) != 2 {
		t.Fatalf("dish B totalS = %v, want 2 (fresh session)", dfb)
	}
	if dfb["downBytes"].(float64) != 0 {
		t.Fatalf("dish B downBytes = %v, want 0 (pre-switch ring not backfilled)", dfb)
	}
	// Dish A's counters are untouched on disk.
	dfa2 := readJSON(t, partA, "daily.json")
	if dfa2["totalS"].(float64) != 4 {
		t.Fatalf("dish A daily mutated by the switch: %v", dfa2)
	}
	// The switch is logged and remembered for the next start.
	ev := readFile(t, filepath.Join(dir, "events.jsonl"))
	if !strings.Contains(ev, `"type":"dish_switch","from":"ut-aaa","to":"ut-bbb"`) {
		t.Fatalf("events.jsonl missing dish_switch: %s", ev)
	}
	cfg := readJSON(t, dir, "config.json")
	if cfg["lastDish"] != "ut-bbb" {
		t.Fatalf("config lastDish = %v, want ut-bbb", cfg["lastDish"])
	}

	// Switch back to dish A: its counters resume, not restart. The switch
	// tick itself books no bytes (the ring it reads predates the new usage
	// epoch); the next delta books normally on top of A's kept counters.
	c.status = statusWithID("ut-aaa")
	c.history = mkHist(14, []float32{20}, []float32{0})
	d.goodTick(t0.Add(6*time.Second), c.status, false, true, 2*time.Second)
	c.history = mkHist(16, []float32{20}, []float32{0})
	d.goodTick(t0.Add(8*time.Second), c.status, false, true, 2*time.Second)
	dfa3 := readJSON(t, partA, "daily.json")
	if dfa3["totalS"].(float64) != 8 || dfa3["downBytes"].(float64) != 4000 {
		t.Fatalf("dish A daily after return = %v, want totalS 8 (4 kept + 4 new) and 4000 bytes (2000 kept + 2000 new)", dfa3)
	}
	if d.sessionS != 4 {
		t.Fatalf("sessionS = %d, want 4 (session is per dish, resets on switch)", d.sessionS)
	}
	if d.dishID != "ut-aaa" {
		t.Fatalf("dishID = %q", d.dishID)
	}
}

func TestDishSwitchResetsWatchState(t *testing.T) {
	dir := t.TempDir()
	sent := 0
	realSend := notifySend
	notifySend = func(string, string) error { sent++; return nil }
	defer func() { notifySend = realSend }()

	c := mkStub()
	c.status = statusWithID("ut-bbb")
	c.status.Alerts = &dish.DishAlerts{MotorsStuck: true}
	d := newDaemon(config{
		interval: 2 * time.Second, idleInterval: 10 * time.Second,
		idleHistInterval: time.Minute, mapInterval: time.Minute,
		geoInterval: time.Hour, geoTransitionInterval: time.Hour, dir: dir,
	}, c)
	d.startWall = t0.Add(-time.Hour)
	// State left over from watching a previous dish.
	d.warm = true
	d.prevAlerts = map[string]bool{"old_alert": true}
	d.openOutage = &outageTrack{cause: "OFFLINE", start: t0.Unix()}
	d.maxDishOutageStart = t0.Unix() - 60
	d.hyst.fired = true
	d.mapData = &obsMap{Rows: 1, Cols: 1}
	d.lastPowerW = 99

	// The switch tick: B answers with its own pre-existing alert.
	d.goodTick(t0, c.status, false, true, 2*time.Second)

	if d.maxDishOutageStart != 0 {
		t.Errorf("outage watermark = %d, want reset", d.maxDishOutageStart)
	}
	if d.openOutage != nil {
		t.Error("openOutage survived the switch")
	}
	if d.hyst.fired {
		t.Error("obstruction alarm carried across the switch")
	}
	if d.mapData != nil {
		t.Error("old dish's obstruction map carried across the switch")
	}
	if d.prevAlerts["motors_stuck"] != true || len(d.prevAlerts) != 1 {
		t.Errorf("prevAlerts = %v, want silently re-primed to the new dish's alerts", d.prevAlerts)
	}
	if sent != 0 {
		t.Errorf("switch sent %d notifications, want 0 (events only)", sent)
	}
	ev := readFile(t, filepath.Join(dir, "events.jsonl"))
	if !strings.Contains(ev, `"cause":"OFFLINE (dish switched)"`) {
		t.Fatalf("open outage not closed with a (dish switched) event: %s", ev)
	}
	if !strings.Contains(ev, `"type":"dish_switch","to":"ut-bbb"`) {
		t.Fatalf("events.jsonl missing dish_switch: %s", ev)
	}
	if strings.Contains(ev, `"type":"alert_set"`) {
		t.Fatal("switch fired alert_set for the new dish's pre-existing alert")
	}
}

func TestDishSwitchNoByteBackfill(t *testing.T) {
	dir := t.TempDir()
	c := mkStub()
	c.status = statusWithID("ut-aaa")
	d := newDaemon(config{
		interval: 2 * time.Second, idleInterval: 10 * time.Second,
		idleHistInterval: time.Minute, mapInterval: time.Minute,
		geoInterval: time.Hour, geoTransitionInterval: time.Hour, dir: dir,
	}, c)
	d.startWall = t0.Add(-time.Hour)
	d.goodTick(t0, c.status, false, true, 2*time.Second) // adopt dish A

	// Dish B has a full ring of throughput from before the switch: none of
	// it is this machine's "today" (same rule as a daemon restart).
	c.status = statusWithID("ut-bbb")
	c.history = mkHist(899, []float32{20}, []float32{0})
	d.goodTick(t0.Add(2*time.Second), c.status, false, true, 2*time.Second)
	dfb := readJSON(t, filepath.Join(dir, "dishes", "ut-bbb"), "daily.json")
	if dfb["downBytes"].(float64) != 0 || dfb["upBytes"].(float64) != 0 {
		t.Fatalf("switch backfilled pre-switch bytes: %v", dfb)
	}

	// The next ring delta books normally.
	c.history = mkHist(901, []float32{20}, []float32{0})
	d.goodTick(t0.Add(4*time.Second), c.status, false, true, 2*time.Second)
	dfb = readJSON(t, filepath.Join(dir, "dishes", "ut-bbb"), "daily.json")
	if dfb["downBytes"].(float64) != 2000 || dfb["upBytes"].(float64) != 200 {
		t.Fatalf("post-switch bytes = %v, want 2000/200", dfb)
	}
}

func TestPruneDaysPartition(t *testing.T) {
	// Retention runs inside the dish partition: a stale day file there is
	// pruned, and the state root's files are never touched.
	dir := t.TempDir()
	d := newDaemon(config{dir: dir}, nil)
	d.dishID = "ut-aaa"
	d.dishDir = d.partitionDir("ut-aaa")
	if err := os.MkdirAll(d.dishDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := t0.AddDate(0, 0, -40).Format("20060102")
	fresh := t0.Format("20060102")
	for _, n := range []string{"day-" + old + ".json", "day-" + fresh + ".json"} {
		for _, base := range []string{d.dishDir, dir} {
			if err := os.WriteFile(filepath.Join(base, n), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	d.pruneDays(t0)
	if _, err := os.Stat(filepath.Join(d.dishDir, "day-"+old+".json")); !os.IsNotExist(err) {
		t.Error("stale partition day file not pruned")
	}
	if _, err := os.Stat(filepath.Join(d.dishDir, "day-"+fresh+".json")); err != nil {
		t.Error("fresh partition day file pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, "day-"+old+".json")); err != nil {
		t.Error("prune reached outside the partition")
	}
}

func TestAdoptLastDish(t *testing.T) {
	dir := t.TempDir()
	// A previous run left config.json naming dish B, whose partition holds
	// counters; the daemon must preload it before B ever answers.
	partB := filepath.Join(dir, "dishes", "ut-bbb")
	if err := os.MkdirAll(partB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonConfig(dir, daemonConfig{Target: "192.168.100.1:9200", LastDish: "ut-bbb"}); err != nil {
		t.Fatal(err)
	}
	d := newDaemon(config{dir: dir}, nil)
	d.dishID = "ut-bbb"
	d.dishDir = partB
	d.daily = dailyFile{DownBytes: 4242}
	d.writeStat("daily.json", d.daily)

	d2 := newDaemon(config{dir: dir}, nil)
	d2.adoptLastDish(t0)
	if d2.dishID != "ut-bbb" || d2.statDir() != partB {
		t.Fatalf("adopt: id=%q dir=%q", d2.dishID, d2.statDir())
	}
	d2.loadDaily()
	if d2.daily.DownBytes != 4242 {
		t.Fatalf("adopted daily = %+v, want the partition's counters", d2.daily)
	}

	// No config / missing partition: nothing adopted, statDir stays the root.
	d3 := newDaemon(config{dir: t.TempDir()}, nil)
	d3.adoptLastDish(t0)
	if d3.dishID != "" || d3.statDir() != d3.cfg.dir {
		t.Fatalf("empty adopt: id=%q dir=%q", d3.dishID, d3.statDir())
	}
}
