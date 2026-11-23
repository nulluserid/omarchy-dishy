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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// MkdirAll leaves pre-upgrade permissions unchanged. Tighten the root
	// boundary without recursively rewriting historical files.
	return dir, os.Chmod(dir, 0o700)
}

// stateLinkName is the runtime-dir symlink naming the daemon's state dir for
// consumers that cannot ask the daemon (CONTRACT.md "State directory").
const stateLinkName = "starlinkd"

// publishStateDir points $XDG_RUNTIME_DIR/starlinkd at the daemon's state
// dir. The daemon honors XDG_STATE_HOME; the QML consumers see only the
// shell's environment, so a user service configured with a different state
// directory would keep updating files the UI never reads. The new link is
// created under a temp name beside the target and renamed over the old one:
// consumers never see it missing or half-made.
//
// An unset XDG_RUNTIME_DIR is a no-op — nowhere to publish, and the consumers
// fall back to the same env resolution the daemon uses. The one thing never
// done here is removing or replacing a non-symlink occupant of the path (a
// real directory or file): it is the user's, so it is reported and left
// alone. Errors are the caller's to log, never fatal.
func publishStateDir(dir string) error {
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		return nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	link := filepath.Join(runtime, stateLinkName)
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s exists and is not a symlink; left alone", link)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// The Lstat/Rename pair is not atomic; the runtime dir is the user's own
	// (0700), so only the user's processes could race it. The temp name is
	// ours by pattern — a stale one means a killed predecessor with this pid.
	tmp := filepath.Join(runtime, fmt.Sprintf(".%s.%d.tmp", stateLinkName, os.Getpid()))
	os.Remove(tmp)
	if err := os.Symlink(abs, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// atomicWrite writes via tmp+rename so consumers never see partial files.
// Mode 0600: state files carry the machine's public IP and location metadata.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(tmp)
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

// marshalCompact serializes v; a marshal failure is returned, never
// converted into a valid-but-empty document — a hostile value must not
// replace previously written state files.
func marshalCompact(v any) ([]byte, error) {
	return json.Marshal(v)
}

// persist writes the contract files for one tick. Write failures are
// logged, never fatal. hf == nil skips hist.json: idle ticks and failure
// ticks carry no new history data (CONTRACT.md "hist.json — cadence").
// status/hist describe the current dish for the UI (state root); daily.json
// is dish-derived and lands in the dish's partition (CONTRACT.md v4). A
// marshal failure skips the write (last-good-wins — a hostile value never
// replaces a good file with an empty one).
func (d *daemon) persist(now time.Time, sf *statusFile, hf *histFile) {
	write := func(name string, v any) {
		b, err := marshalCompact(v)
		if err != nil {
			d.vlog("marshal %s: %v — keeping last good file", name, err)
			return
		}
		if err := atomicWrite(filepath.Join(d.cfg.dir, name), b); err != nil {
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
	b, err := marshalCompact(v)
	if err != nil {
		d.vlog("marshal %s: %v — keeping last good file", name, err)
		return
	}
	if err := atomicWrite(filepath.Join(d.statDir(), name), b); err != nil {
		d.vlog("write %s: %v", name, err)
	}
}

// ensureStatDir creates the current dish's partition directory when missing.
func (d *daemon) ensureStatDir() {
	if d.dishDir != "" {
		os.MkdirAll(d.dishDir, 0o700)
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
	// Tighten a legacy event file before it can become the rotated file.
	if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
		return err
	}
	if fi, err := os.Stat(p); err == nil && fi.Size() > maxEventBytes {
		if err := os.Rename(p, filepath.Join(dir, "events.old.jsonl")); err != nil {
			return fmt.Errorf("rotate: %w", err)
		}
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}
