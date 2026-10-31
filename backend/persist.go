package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const maxEventBytes = 512 << 10 // rotate events.jsonl beyond this

func stateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "starlinkd")
	return dir, os.MkdirAll(dir, 0o755)
}

// atomicWrite writes via tmp+rename so consumers never see partial files.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func marshalCompact(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// persist writes the contract files for one tick. Write failures are
// logged, never fatal. hf == nil skips hist.json: idle ticks and failure
// ticks carry no new history data (CONTRACT.md "hist.json — cadence").
// status/hist describe the current dish for the UI (state root); daily.json
// is dish-derived and lands in the dish's partition (CONTRACT.md v4).
func (d *daemon) persist(now time.Time, sf *statusFile, hf *histFile) {
	write := func(name string, v any) {
		if err := atomicWrite(filepath.Join(d.cfg.dir, name), marshalCompact(v)); err != nil {
			d.vlog("write %s: %v", name, err)
		}
	}
	write("status.json", sf)
	if hf != nil {
		write("hist.json", hf)
	}
	d.writeStat("daily.json", d.daily)
	d.persistDay(now, d.cfg.once)
	d.lastFile = sf
}

// writeStat writes into the current dish's partition (statDir), logging
// failures like the shared-file writer. The directory is ensured so a
// partition deleted by the retention sweep self-heals on the dish's return
// (CONTRACT.md v4 retention).
func (d *daemon) writeStat(name string, v any) {
	d.ensureStatDir()
	if err := atomicWrite(filepath.Join(d.statDir(), name), marshalCompact(v)); err != nil {
		d.vlog("write %s: %v", name, err)
	}
}

// ensureStatDir creates the current dish's partition directory when missing.
func (d *daemon) ensureStatDir() {
	if d.dishDir != "" {
		os.MkdirAll(d.dishDir, 0o755)
	}
}

// loadDaily restores the current dish's usage counters; missing/corrupt
// starts fresh. A file without a retention stamp (≤ v1.4 partition, legacy
// root file) is seeded from its mtime: the stamp then freezes and the
// retention sweep ages the dish out from load time even though later writes
// (including fail ticks) rewrite the file.
func (d *daemon) loadDaily() {
	b, err := os.ReadFile(filepath.Join(d.statDir(), "daily.json"))
	if err != nil {
		return
	}
	var df dailyFile
	if err := json.Unmarshal(b, &df); err != nil {
		d.vlog("daily.json: %v (starting fresh)", err)
		return
	}
	if df.LastSeen == 0 {
		if fi, err := os.Stat(filepath.Join(d.statDir(), "daily.json")); err == nil {
			df.LastSeen = fi.ModTime().Unix()
		}
	}
	d.daily = df
}

// rollDaily moves today's bytes into allTime on local date change.
func (d *daemon) rollDaily(now time.Time) {
	today := now.Format("2006-01-02")
	if d.daily.Date == today {
		return
	}
	if d.daily.Date != "" {
		d.daily.AllTime.DownBytes += d.daily.DownBytes
		d.daily.AllTime.UpBytes += d.daily.UpBytes
	}
	d.daily.Date = today
	d.daily.DownBytes = 0
	d.daily.UpBytes = 0
	d.daily.OnlineS = 0
	d.daily.TotalS = 0
	d.daily.Outages = 0
}

// appendEvent appends one JSONL event, rotating to events.old.jsonl first
// when the file exceeds 512KB.
func appendEvent(dir string, ev event) error {
	p := filepath.Join(dir, "events.jsonl")
	if fi, err := os.Stat(p); err == nil && fi.Size() > maxEventBytes {
		if err := os.Rename(p, filepath.Join(dir, "events.old.jsonl")); err != nil {
			return fmt.Errorf("rotate: %w", err)
		}
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}
