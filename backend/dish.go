// Per-dish state partitions. Everything the DISH measures — usage counters,
// day history, session availability — belongs to that dish, so alternating
// between two Starlinks (a week at each, or a laptop that moves between
// networks) keeps both dishes' stats intact instead of blending them.
// status.json / hist.json stay shared "current dish" views; events.jsonl and
// config.json are machine-level.
//
// The partition key is the dish's device id, resolved from an
// identity-verified GetStatus answer — never the configured target: joining
// another Starlink's network re-aims the same 192.168.100.1 at a different
// dish with no target change, and a target change to the SAME dish (new
// router) must not reset anything. The switch runs on the tick loop the
// moment a different identity answers (CONTRACT.md v4).

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// dishesDirName is the state-dir subdirectory holding one folder per dish.
const dishesDirName = "dishes"

// dishDirName maps a dish id to a single safe path element. Dish ids look
// like "ut4118910a-4432740c-1ab9659e"; anything outside [a-z0-9._-] is
// replaced so a hostile id cannot escape the partition directory, and "." /
// ".." cannot alias the parent directories.
func dishDirName(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !ok {
			r = '-'
		}
		if b.Len() >= 64 {
			break
		}
		b.WriteRune(r)
	}
	name := b.String()
	if name == "" || name == "." || name == ".." {
		return "unknown"
	}
	return name
}

// partitionDir is where the named dish's accumulated state lives.
func (d *daemon) partitionDir(id string) string {
	return filepath.Join(d.cfg.dir, dishesDirName, dishDirName(id))
}

// statDir is where THIS dish's accumulated state files go: the dish's
// partition once an identity is known, the state root before that (startup
// fail ticks with no evidence yet).
func (d *daemon) statDir() string {
	if d.dishDir != "" {
		return d.dishDir
	}
	return d.cfg.dir
}

// adoptLastDish restores the last-seen dish's partition at startup so
// counters continue in the right place even while that dish is unreachable
// (fail ticks book into the correct dish before the first identity-verified
// answer arrives). config.json's lastDish is written by switchDish.
func (d *daemon) adoptLastDish(now time.Time) {
	dc, ok := loadDaemonConfig(d.cfg.dir)
	if !ok || dc.LastDish == "" {
		return
	}
	p := d.partitionDir(dc.LastDish)
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		return
	}
	d.dishID = dc.LastDish
	d.dishDir = p
	d.loadDaily()
	d.initDay(now)
	d.vlog("dish: adopted partition %s (%s)", dishDirName(dc.LastDish), dc.LastDish)
}

// switchDish moves all dish-derived accumulated state to the partition of
// the dish that just answered with a different identity. Runs only from the
// tick loop, so the swap is race-free with in-flight RPCs. A first sighting
// switches too: stats live in partitions from the very first tick.
//
// Reset on switch: the history ring and ingest cursor (the new dish's ring
// is consumed fresh), the usage epoch (its pre-switch ring bytes are not
// today's usage), session counters, outage tracking and watermark, warm
// state (the new dish re-primes alerts/outages quietly on its first ONLINE),
// obstruction hysteresis, and the map. geo/link/route state is network-level
// and keeps its own change detection.
func (d *daemon) switchDish(now time.Time, id string) {
	old := d.dishID
	if d.day.dirty || d.day.minute >= 0 {
		d.persistDay(now, true) // final write into the outgoing partition
	}
	if d.openOutage != nil {
		cause, started := d.openOutage.cause, d.openOutage.start
		d.openOutage = nil // silent event: the outage belonged to the old watch
		d.emit(event{Type: "outage_end", Cause: cause + " (dish switched)",
			DurS: now.Unix() - started}, "", "")
	}
	d.emit(event{Type: "dish_switch", From: old, To: id}, "", "")

	d.maxDishOutageStart = 0
	d.lastOutages = nil
	d.failed = 0
	d.prevState = ""
	d.prevAlerts = nil
	d.warm = false
	d.hyst = newHyst(60, 300)
	d.mapData = nil
	d.lastMap = time.Time{}
	d.lastObs = obsStats{}
	d.lastPowerW = 0
	d.lastSampleWall = time.Time{}
	d.hasHist = false
	d.resetArrays()
	d.sampled = false
	d.prevCur = 0
	d.haveUptime = false
	d.lastUptime = 0
	d.lastNewest = 0
	d.startWall = now
	d.sessionS = 0
	d.sessionOnlineS = 0

	d.dishID = id
	d.dishDir = d.partitionDir(id)
	if err := os.MkdirAll(d.dishDir, 0o700); err != nil {
		d.vlog("partition %s: %v", dishDirName(id), err)
	}
	d.pendingID, d.pendingIDTicks = "", 0
	d.lastOutageAnnounce = 0
	d.daily = dailyFile{} // replaced by the partition's counters below
	d.loadDaily()
	d.day = dayAccum{minute: -1}
	d.initDay(now)
	// Cardinality bound: identity churn (hostile or unlucky) must not grow
	// the partition set without limit. Debounced switches make this rare.
	d.evictPartitions(now)

	if err := writeDaemonConfig(d.cfg.dir, daemonConfig{Target: d.cfg.target, LastDish: id}); err != nil {
		d.vlog("write %s: %v", configName, err)
	}
	d.vlog("dish switch: %q -> %q (partition %s)", old, id, dishDirName(id))
}

// tidyDishes is the retention sweep: a dish partition with no connection
// activity for dayKeep days is deleted whole — its counters and day history
// (CONTRACT.md v4). "Connected" evidence is the newest of daily.json's
// lastSeen stamp (written only by identity-verified good ticks, so fail
// ticks cannot keep a dead dish fresh) and the newest day-*.json date;
// partitions predating the stamp fall back to daily.json's mtime. The
// ACTIVE partition is swept like any other: if it reconnects after a sweep,
// writeStat/writeDay recreate the directory and counters resume — and its
// in-memory counters keep the session alive meanwhile. Legacy root-level
// day files from pre-partition installs share the same window; the
// dishes/ directory itself is never touched. Runs at startup and on day
// roll — housekeeping, vlog only, never fatal.
func (d *daemon) tidyDishes(now time.Time) {
	cutoff := now.AddDate(0, 0, -dayKeep)
	root := filepath.Join(d.cfg.dir, dishesDirName)
	entries, err := os.ReadDir(root)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(root, e.Name())
			if last := partitionLastActivity(dir); last.Before(cutoff) {
				if err := os.RemoveAll(dir); err != nil {
					d.vlog("tidy %s: %v", e.Name(), err)
				} else {
					d.vlog("tidy: removed stale dish partition %s (idle since %s)",
						e.Name(), last.Format("2006-01-02"))
				}
				continue
			}
			// Kept partition: still roll its day files through retention.
			d.pruneDaysIn(dir, now)
		}
	}
	// Legacy root day files (≤ v1.2 installs): same retention, and the
	// pattern cannot match the dishes/ directory.
	d.pruneDaysIn(d.cfg.dir, now)
}

// evictPartitions bounds the partition set at maxDishPartitions: beyond it,
// the least-recently-active partitions (never the active one) are removed.
// Runs after switchDish creates a partition — debounced, so the ReadDir
// cost is rare.
func (d *daemon) evictPartitions(now time.Time) {
	root := filepath.Join(d.cfg.dir, dishesDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	type part struct {
		name string
		last time.Time
	}
	var parts []part
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		parts = append(parts, part{e.Name(), partitionLastActivity(dir)})
	}
	if len(parts) <= maxDishPartitions {
		return
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].last.Before(parts[j].last) })
	for _, p := range parts[:len(parts)-maxDishPartitions] {
		if filepath.Join(root, p.name) == d.dishDir {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, p.name)); err != nil {
			d.vlog("evict %s: %v", p.name, err)
		} else {
			d.vlog("evicted dish partition %s (cap %d)", p.name, maxDishPartitions)
		}
	}
}

// partitionLastActivity is the newest connection evidence in a dish
// partition: daily.json's lastSeen stamp (good-tick only), the day after
// the newest day file's date, or — for pre-stamp files — daily.json's
// mtime, whichever is later.
func partitionLastActivity(dir string) time.Time {
	var last time.Time
	haveStamp := false
	if b, err := os.ReadFile(filepath.Join(dir, "daily.json")); err == nil {
		var df dailyFile
		if json.Unmarshal(b, &df) == nil && df.LastSeen > 0 {
			last = time.Unix(df.LastSeen, 0)
			haveStamp = true
		}
	}
	if !haveStamp {
		// Pre-stamp file or unreadable daily.json: mtime is the best
		// remaining signal (pre-1.5 partitions and drive-bys).
		if fi, err := os.Stat(filepath.Join(dir, "daily.json")); err == nil {
			last = fi.ModTime()
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return last
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "day-") || !strings.HasSuffix(name, ".json") || len(name) != len("day-20060102.json") {
			continue
		}
		if date, err := time.Parse("20060102", name[len("day-"):len(name)-len(".json")]); err == nil {
			if end := date.Add(24 * time.Hour); end.After(last) {
				last = end
			}
		}
	}
	return last
}
