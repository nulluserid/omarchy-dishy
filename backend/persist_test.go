package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRollDaily(t *testing.T) {
	d := newDaemon(config{}, nil)
	d.daily = dailyFile{
		Date: "2026-10-18", DownBytes: 100, UpBytes: 50,
		OnlineS: 10, TotalS: 20, Outages: 1,
		AllTime: dailyAllTime{DownBytes: 5, UpBytes: 3},
	}
	d.rollDaily(time.Date(2026, 10, 19, 0, 0, 1, 0, time.Local))
	if d.daily.Date != "2026-10-19" {
		t.Errorf("date = %q", d.daily.Date)
	}
	if d.daily.AllTime.DownBytes != 105 || d.daily.AllTime.UpBytes != 53 {
		t.Errorf("allTime = %+v, want {105 53}", d.daily.AllTime)
	}
	if d.daily.DownBytes != 0 || d.daily.UpBytes != 0 || d.daily.OnlineS != 0 ||
		d.daily.TotalS != 0 || d.daily.Outages != 0 {
		t.Errorf("today not zeroed: %+v", d.daily)
	}
	// Same date: no-op.
	d.daily.DownBytes = 7
	d.rollDaily(time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local))
	if d.daily.DownBytes != 7 {
		t.Errorf("same-date roll changed counters: %+v", d.daily)
	}
}

func TestRollDailyFirstRun(t *testing.T) {
	d := newDaemon(config{}, nil)
	// Empty date: initialize without touching allTime.
	d.daily.AllTime = dailyAllTime{DownBytes: 9}
	d.rollDaily(time.Date(2026, 10, 19, 8, 0, 0, 0, time.Local))
	if d.daily.Date != "2026-10-19" || d.daily.AllTime.DownBytes != 9 {
		t.Errorf("first-run roll: %+v", d.daily)
	}
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.json")
	if err := atomicWrite(p, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != `{"a":1}` {
		t.Fatalf("readback = %q, %v", b, err)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file left behind")
	}
	// Overwrite.
	if err := atomicWrite(p, []byte(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if string(b) != `{"a":2}` {
		t.Fatalf("overwrite readback = %q", b)
	}
}

func TestAppendEventRotation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "events.jsonl")
	// Seed just over the rotation threshold.
	big := strings.Repeat("x", maxEventBytes+1) + "\n"
	if err := os.WriteFile(p, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(dir, event{Type: "state_change", From: "A", To: "B"}); err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(filepath.Join(dir, "events.old.jsonl"))
	if err != nil {
		t.Fatalf("events.old.jsonl missing: %v", err)
	}
	if old.Size() <= maxEventBytes {
		t.Errorf("old file size = %d, want > %d", old.Size(), maxEventBytes)
	}
	cur, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(cur)), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"type":"state_change"`) {
		t.Errorf("events.jsonl after rotate = %q", lines)
	}
	// No second rotation while small.
	if err := appendEvent(dir, event{Type: "alert_set", ID: "x", Label: "X"}); err != nil {
		t.Fatal(err)
	}
	cur2, _ := os.ReadFile(p)
	if !strings.Contains(string(cur2), `"id":"x"`) {
		t.Errorf("second append missing: %q", cur2)
	}
}

func TestDailyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	d := newDaemon(config{dir: dir}, nil)
	d.daily = dailyFile{Date: "2026-10-19", DownBytes: 1234, UpBytes: 567,
		LastSeen: 1760000000,
		AllTime:  dailyAllTime{DownBytes: 872400000000}}
	mb, merr := marshalCompact(d.daily)
	if merr != nil {
		t.Fatal(merr)
	}
	if err := atomicWrite(filepath.Join(dir, "daily.json"), mb); err != nil {
		t.Fatal(err)
	}
	d2 := newDaemon(config{dir: dir}, nil)
	d2.loadDaily()
	if d2.daily != d.daily {
		t.Errorf("round trip: %+v != %+v", d2.daily, d.daily)
	}
	// A pre-stamp file (≤ v1.4) is seeded from its mtime on load, freezing
	// the retention stamp (CONTRACT.md retention).
	seed := dailyFile{Date: "2026-10-19", DownBytes: 5}
	sb, serr := marshalCompact(seed)
	if serr != nil {
		t.Fatal(serr)
	}
	if err := atomicWrite(filepath.Join(dir, "daily.json"), sb); err != nil {
		t.Fatal(err)
	}
	mtime := t0
	os.Chtimes(filepath.Join(dir, "daily.json"), mtime, mtime)
	d4 := newDaemon(config{dir: dir}, nil)
	d4.loadDaily()
	if d4.daily.LastSeen != mtime.Unix() {
		t.Errorf("stampless file not seeded: LastSeen = %d, want %d", d4.daily.LastSeen, mtime.Unix())
	}
	if d4.daily.DownBytes != 5 {
		t.Errorf("seeded counters lost: %+v", d4.daily)
	}
	// Corrupt file: starts fresh without failing.
	if err := os.WriteFile(filepath.Join(dir, "daily.json"), []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	d3 := newDaemon(config{dir: dir}, nil)
	d3.loadDaily()
	if d3.daily.DownBytes != 0 {
		t.Errorf("corrupt daily not discarded: %+v", d3.daily)
	}
}
