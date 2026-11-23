package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// notifySendPath is the only notify-send the daemon may run: an absolute
// path resolved and verified once at start (resolveNotifySend). Empty means
// notifications are disabled for the run. The auto-polling daemon must not
// resolve a helper from ambient PATH when a notification fires — whoever
// controls PATH would choose what runs as the user, on every transition,
// for as long as the daemon lives.
var notifySendPath string

// notifySend sends a desktop notification. Per CONTRACT.md a failure here is
// logged by the caller and never fatal. It is a variable so tests can stub it.
// Context-bounded: a wedged notification daemon must not stall the tick loop.
// The "--" separator keeps summary/body out of notify-send's option parsing.
// It runs the verified absolute path and nothing else: with no path stored
// (a refused helper, -once, the CLI subcommands, tests) it returns an error
// and spawns nothing.
var notifySend = func(summary, body string) error {
	if notifySendPath == "" {
		return errors.New("notify-send: no trusted path resolved (notifications disabled)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, notifySendPath, "-a", "Dishy", "-u", "normal", "--", summary, body)
	return cmd.Run()
}

// resolveNotifySend fixes the notification helper for this run, before the
// tick loop exists: exec.LookPath yields an absolute path, and only a
// regular file owned by root or the daemon's euid with no group/other write
// bits is accepted. Root is read as uid 0 — or, inside a service user
// namespace that leaves host root unmapped, as the kernel's overflow uid
// (overflowRootUID). Anything else — absent, not executable, a relative PATH
// entry, a directory, a file another user (or the group, or everyone) can
// rewrite — leaves notifications disabled for the run and returns the
// reason for the caller to report. Soft failure: the daemon keeps running
// (notifications are never fatal).
func (d *daemon) resolveNotifySend() error {
	notifySendPath = "" // disabled unless proven trusted below
	path, err := exec.LookPath("notify-send")
	if err != nil {
		return fmt.Errorf("notify-send: %w", err)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("notify-send %s: not an absolute path (relative PATH entry)", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("notify-send %s: %w", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("notify-send %s: not a regular file", path)
	}
	if !ok {
		return fmt.Errorf("notify-send %s: no owner information", path)
	}
	// Owner decision: root, the daemon's euid, or — only inside a namespace
	// that leaves host root unmapped — the kernel's overflow uid, which
	// stands for root there (overflowRootUID). Anything else is refused.
	if st.Uid != 0 && int(st.Uid) != os.Geteuid() {
		if over, ok := overflowRootUID(); !ok || st.Uid != over {
			return fmt.Errorf("notify-send %s: owned by uid %d, want root or %d", path, st.Uid, os.Geteuid())
		}
	}
	// The write-bit rule runs after EVERY accepted owner, the namespace
	// acceptance included. Sequential ifs on purpose: as cases of a switch,
	// an accepted overflow owner ended the switch and a helper that was
	// also writable by group/other slipped through.
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("notify-send %s: writable by group/other (%04o)", path, fi.Mode().Perm())
	}
	notifySendPath = path
	d.vlog("notify-send: %s", path) // the resolution itself stays verbose-only
	return nil
}

// overflowRootUID reports the kernel's overflow uid when the daemon runs
// inside a user namespace that leaves host root unmapped, and false
// otherwise. systemd starts an unprivileged user service that way once it
// carries sandboxing directives such as ProtectKernelTunables or
// PrivateDevices; the shipped unit's /proc/self/uid_map is "1000 1000 1".
// There every unmapped host uid, root included, is reported as the overflow
// uid: the root-owned /usr/bin/notify-send looked like uid 65534 and was
// refused on every start (first live run of v1.7.0, 2026-11-23).
//
// Limitation, stated plainly: inside such a namespace a root-owned file and
// a file owned by any other unmapped host uid are indistinguishable. The
// acceptance is still sound — the candidate path comes from the service's
// own PATH (the user's configuration, the same trust domain as the daemon),
// and the regular-file and no-group/other-write rules apply to it
// unchanged.
//
// False when host root is mapped (no namespace, a container's root
// namespace), or when either /proc file cannot be read or parsed: the
// caller then keeps the strict rule. Fail closed.
func overflowRootUID() (uint32, bool) {
	m, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return 0, false
	}
	// Lines of "ID-inside-ns ID-outside-ns length": host root is mapped
	// iff some extent starts at outside ID 0.
	f := strings.Fields(string(m))
	if len(f) == 0 || len(f)%3 != 0 {
		return 0, false
	}
	for i := 0; i < len(f); i += 3 {
		var n [3]uint64
		for j, s := range f[i : i+3] {
			v, err := strconv.ParseUint(s, 10, 32)
			if err != nil {
				return 0, false
			}
			n[j] = v
		}
		if n[1] == 0 && n[2] > 0 {
			return 0, false // host root is visible here: strict rule
		}
	}
	over, err := os.ReadFile("/proc/sys/kernel/overflowuid")
	if err != nil {
		return 0, false
	}
	uid, err := strconv.ParseUint(strings.TrimSpace(string(over)), 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(uid), true
}
