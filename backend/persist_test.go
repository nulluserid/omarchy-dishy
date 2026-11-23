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
	// No temp file is left behind, whatever its name.
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 1 || left[0] != p {
		t.Errorf("files left behind: %v, want only %s", left, p)
	}
	// Overwrite.
	if err := atomicWrite(p, []byte(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if string(b) != `{"a":2}` {
		t.Fatalf("overwrite readback = %q", b)
	}
	// Nothing planted at a predictable temp name is ever opened or followed.
	// The temp file is created fresh under a unique name in the destination
	// directory, so a planted file keeps its content and mode, a planted
	// symlink stays a symlink with its target untouched, and the real file is
	// still written.
	planted, link, decoy := p+".tmp", filepath.Join(dir, "g.json.tmp"), filepath.Join(dir, "decoy")
	if err := os.WriteFile(planted, []byte("planted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(decoy, []byte("decoy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(decoy, link); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(p, []byte(`{"a":3}`)); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(dir, "g.json"), []byte(`{"b":1}`)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != `{"a":3}` {
		t.Errorf("readback past a planted temp file = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "g.json")); string(b) != `{"b":1}` {
		t.Errorf("readback past a planted temp symlink = %q", b)
	}
	// Each planted object is reported on its own, so one violation cannot
	// hide another.
	for path, want := range map[string]string{planted: "planted", decoy: "decoy"} {
		if b, err := os.ReadFile(path); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v, want untouched %q", path, b, err, want)
		}
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("planted temp symlink = %v, %v, want it left a symlink", fi, err)
	}
	for _, path := range []string{planted, decoy} {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o644 {
			t.Errorf("%s mode = %o, want 644", path, fi.Mode().Perm())
		}
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != want {
		t.Fatalf("%s mode = %o, want %o", path, fi.Mode().Perm(), want)
	}
}

func TestUpgradeStatePermissions(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	dir := filepath.Join(base, "starlinkd")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := stateDir(); err != nil || got != dir {
		t.Fatalf("stateDir = %q, %v", got, err)
	}
	assertMode(t, dir, 0o700)
	p := filepath.Join(dir, "status.json")
	if err := os.WriteFile(p+".tmp", []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	assertMode(t, p, 0o600)
	// The pre-planted temp file was never opened: same content, same mode.
	if b, err := os.ReadFile(p + ".tmp"); err != nil || string(b) != "old" {
		t.Fatalf("planted temp file = %q, %v, want untouched", b, err)
	}
	assertMode(t, p+".tmp", 0o644)
	p = filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(dir, event{Type: "state_change"}); err != nil {
		t.Fatal(err)
	}
	assertMode(t, p, 0o600)
	// A symlink at the events path is refused, never followed — not even to
	// chmod it: the decoy keeps its content and mode, and the link stays a
	// link.
	decoy := filepath.Join(dir, "decoy")
	if err := os.WriteFile(decoy, []byte("decoy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(decoy, p); err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(dir, event{Type: "state_change"}); err == nil {
		t.Fatal("appendEvent appended through a symlink")
	}
	if b, err := os.ReadFile(decoy); err != nil || string(b) != "decoy" {
		t.Fatalf("symlink target = %q, %v, want untouched", b, err)
	}
	assertMode(t, decoy, 0o644)
	if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("events.jsonl = %v, %v; the planted symlink should be left in place", fi, err)
	}

	// STARLINKD_STATE_DIR is the user's choice, so an existing directory
	// keeps its mode — unlike the default path at the top of this test,
	// which is tightened on purpose. What it holds is protected all the
	// same: every file is written 0600 whatever the directory's mode.
	chosen := filepath.Join(t.TempDir(), "chosen")
	if err := os.Mkdir(chosen, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(chosen, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STARLINKD_STATE_DIR", chosen)
	if got, err := stateDir(); err != nil || got != chosen {
		t.Fatalf("stateDir with the override = %q, %v", got, err)
	}
	assertMode(t, chosen, 0o755)
	if err := writeTargetConfig(chosen, "192.168.100.1:9200"); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(chosen, "config.json"), 0o600)
	// A directory the override has to create is still 0700.
	created := filepath.Join(t.TempDir(), "missing", "state")
	t.Setenv("STARLINKD_STATE_DIR", created)
	if got, err := stateDir(); err != nil || got != created {
		t.Fatalf("stateDir creating the override = %q, %v", got, err)
	}
	assertMode(t, created, 0o700)
	t.Setenv("STARLINKD_STATE_DIR", "")
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
	assertMode(t, filepath.Join(dir, "events.old.jsonl"), 0o600)
	assertMode(t, p, 0o600)
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
	// Oversize is treated exactly like corrupt: a file that large was not
	// written by this daemon. The valid JSON comes first and the padding
	// after it, so only the size can refuse it — truncation would still
	// leave loadable data.
	oversize := `{"date":"2026-10-19","downBytes":777}` + strings.Repeat(" ", int(maxDailyBytes))
	if err := os.WriteFile(filepath.Join(dir, "daily.json"), []byte(oversize), 0o644); err != nil {
		t.Fatal(err)
	}
	d5 := newDaemon(config{dir: dir}, nil)
	d5.loadDaily()
	if d5.daily.DownBytes != 0 {
		t.Errorf("oversize daily loaded: %+v", d5.daily)
	}
}
