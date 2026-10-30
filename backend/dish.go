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
	"os"
	"path/filepath"
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
	if err := os.MkdirAll(d.dishDir, 0o755); err != nil {
		d.vlog("partition %s: %v", dishDirName(id), err)
	}
	d.daily = dailyFile{} // replaced by the partition's counters below
	d.loadDaily()
	d.day = dayAccum{minute: -1}
	d.initDay(now)

	if err := writeDaemonConfig(d.cfg.dir, daemonConfig{Target: d.cfg.target, LastDish: id}); err != nil {
		d.vlog("write %s: %v", configName, err)
	}
	d.vlog("dish switch: %q -> %q (partition %s)", old, id, dishDirName(id))
}
