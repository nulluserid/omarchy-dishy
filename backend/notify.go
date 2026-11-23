package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// bits is accepted. Anything else — absent, not executable, a relative PATH
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
	switch {
	case !fi.Mode().IsRegular():
		return fmt.Errorf("notify-send %s: not a regular file", path)
	case !ok:
		return fmt.Errorf("notify-send %s: no owner information", path)
	case st.Uid != 0 && int(st.Uid) != os.Geteuid():
		return fmt.Errorf("notify-send %s: owned by uid %d, want root or %d", path, st.Uid, os.Geteuid())
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("notify-send %s: writable by group/other (%04o)", path, fi.Mode().Perm())
	}
	notifySendPath = path
	d.vlog("notify-send: %s", path) // the resolution itself stays verbose-only
	return nil
}
