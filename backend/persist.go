package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const maxEventBytes = 512 << 10 // rotate events.jsonl beyond this

func stateDir() (string, error) {
	// STARLINKD_STATE_DIR pins the whole state directory, ahead of
	// XDG_STATE_HOME/HOME: the demo harness runs against a sandbox, and the
	// widget can hand its one-shot CLI calls the directory it actually
	// reads. Absolute only — a relative value is a misconfiguration and
	// must fail loudly here, never fall back to the real state directory.
	if dir := os.Getenv("STARLINKD_STATE_DIR"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("STARLINKD_STATE_DIR %q is not an absolute path", dir)
		}
		// The directory is the user's choice, so its mode is too. MkdirAll
		// gives 0700 only to directories it creates; an existing one is
		// returned as it is and never chmod'ed — it may be one other people
		// or tools rely on, and Chmod would follow a symlink to do it. The
		// content is protected regardless: every state file is written 0600
		// by atomicWrite, and the event log is tightened through its own
		// descriptor. (The default path below is tightened on purpose.)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		return dir, nil
	}
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

// errRuntimeDirUntrusted marks a publish that was skipped on purpose: the
// runtime dir is missing, not this user's, or writable by others. Nothing
// failed — the consumers fall back to the env resolution — so the caller
// logs it quietly instead of reporting a fault.
var errRuntimeDirUntrusted = errors.New("runtime dir missing, not owned by this user, or writable by group/others")

// checkRuntimeDir reports errRuntimeDirUntrusted unless dir is a directory
// owned by this user that group and others cannot write — the XDG runtime
// dir's own preconditions. A dir other accounts can write would let one of
// them plant the link (or a non-symlink occupant) and aim the widget at a
// state dir of their choosing.
func checkRuntimeDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return fmt.Errorf("%w: %s: %v", errRuntimeDirUntrusted, dir, err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %s is not owned by uid %d", errRuntimeDirUntrusted, dir, os.Getuid())
	}
	if mode := fi.Mode().Perm(); mode&0o022 != 0 {
		return fmt.Errorf("%w: %s has mode %o", errRuntimeDirUntrusted, dir, mode)
	}
	return nil
}

// publishStateDir points $XDG_RUNTIME_DIR/starlinkd at the daemon's state
// dir. The daemon honors XDG_STATE_HOME; the QML consumers see only the
// shell's environment, so a user service configured with a different state
// directory would keep updating files the UI never reads. The new link is
// created under a temp name beside the target and renamed over the old one:
// consumers never see it missing or half-made.
//
// An unset XDG_RUNTIME_DIR is a no-op — nowhere to publish, and the consumers
// fall back to the same env resolution the daemon uses. So is a runtime dir
// that is not this user's alone (checkRuntimeDir). The one thing never done
// here is removing or replacing a non-symlink occupant of the path (a real
// directory or file): it is the user's, so it is reported and left alone.
// Errors are the caller's to log, never fatal.
func publishStateDir(dir string) error {
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		return nil
	}
	if err := checkRuntimeDir(runtime); err != nil {
		return err
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

// chmodTemp and writeTemp are the two steps of atomicWrite that fail only
// when the filesystem does: tightening the fresh temp file to 0600, and
// writing the data. A plain user cannot provoke either on a file they have
// just created, so they are variables — a test makes one fail to see that
// the error is returned, never swallowed (TestAtomicWriteFailureKeepsOldFile).
var (
	chmodTemp = func(f *os.File) error { return f.Chmod(0o600) }
	writeTemp = func(f *os.File, data []byte) error {
		_, err := f.Write(data)
		return err
	}
)

// atomicWrite writes via tmp+rename so consumers never see partial files.
// The temp file is created fresh under a unique name in the destination
// directory (0600 by construction), so nothing planted at a predictable name
// is ever opened or followed; it is renamed into place, and removed whenever
// any step fails. A failed step is returned to the caller and leaves the
// previous file untouched: only a temp file that was written whole is ever
// renamed over it. The name is "<base>.tmp-<random>", which the day-file
// pruning pattern (day-YYYYMMDD.json, exact length) cannot match.
// Mode 0600: state files carry the machine's public IP and location metadata.
func atomicWrite(path string, data []byte) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Plain assignments on purpose: an `if err := …` here scopes its own
	// err, and a failed step then never reaches the checks below — the
	// empty or partial temp was renamed over the good file, as success.
	err = chmodTemp(f)
	if err == nil {
		err = writeTemp(f, data)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
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

// ensureStatDir creates the current dish's partition directory when missing
// — and tightens it then, once, the way a switch does. An existing directory
// is left alone: this runs on every tick.
func (d *daemon) ensureStatDir() {
	if d.dishDir == "" {
		return
	}
	if _, err := os.Stat(d.dishDir); err == nil {
		return
	}
	if err := os.MkdirAll(d.dishDir, 0o700); err == nil {
		d.securePartition()
	}
}

// maxDailyBytes caps the daily.json the daemon will load. The one it writes
// is a few hundred bytes; anything larger was not written by this daemon.
const maxDailyBytes = 16 << 10

// readCapped reads a state file the daemon is about to LOAD and refuses more
// than max bytes. Every writer here is bounded, so a larger file is not one
// this daemon wrote: it is reported as an error, and the caller treats it
// exactly like any other unreadable file.
func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s: larger than %d bytes", filepath.Base(path), max)
	}
	return b, nil
}

// loadDaily restores the current dish's usage counters; missing/corrupt
// starts fresh — as does a file too large to be one this daemon wrote.
// A file without a retention stamp (≤ v1.4 partition, legacy
// root file) is seeded from its mtime: the stamp then freezes and the
// retention sweep ages the dish out from load time even though later writes
// (including fail ticks) rewrite the file.
func (d *daemon) loadDaily() {
	b, err := readCapped(filepath.Join(d.statDir(), "daily.json"), maxDailyBytes)
	if err != nil {
		if !os.IsNotExist(err) {
			d.vlog("daily.json: %v (starting fresh)", err)
		}
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
// when the file exceeds 512KB. The log is opened with O_NOFOLLOW and only
// ever touched through that descriptor: a symlink planted at events.jsonl
// is refused with an error instead of followed.
func appendEvent(dir string, ev event) error {
	p := filepath.Join(dir, "events.jsonl")
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := openEventLog(p)
	if err != nil {
		return err
	}
	// An oversize log rotates. It is tightened through the descriptor
	// first, so a legacy mode never survives into the rotated file.
	if fi, serr := f.Stat(); serr == nil && fi.Size() > maxEventBytes {
		cerr := f.Chmod(0o600)
		f.Close()
		if cerr != nil {
			return cerr
		}
		if err := os.Rename(p, filepath.Join(dir, "events.old.jsonl")); err != nil {
			return fmt.Errorf("rotate: %w", err)
		}
		if f, err = openEventLog(p); err != nil {
			return err
		}
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// openEventLog opens the event log for appending, creating it 0600. A
// symlink at that path is never followed: O_NOFOLLOW makes the open fail.
func openEventLog(p string) (*os.File, error) {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EEXIST) {
		return nil, fmt.Errorf("%s is a symlink: refusing to append through it", p)
	}
	return f, err
}
