package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	api "starlinkd/gen/spacex_api/device/device"
	dish "starlinkd/gen/spacex_api/device/dish"
)

// runControl performs a one-shot stow/unstow/reboot, then exits.
// Exit 0 on success; nonzero + stderr message on failure.
func runControl(cmd string, args []string) int {
	fs := flag.NewFlagSet("starlinkd "+cmd, flag.ContinueOnError)
	target := fs.String("t", "192.168.100.1:9200", "dish gRPC target host:port")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := dial(*target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "starlinkd: dial %s: %v\n", *target, err)
		return 1
	}
	defer conn.Close()

	var req api.Request
	switch cmd {
	case "stow":
		req.Request = &api.Request_DishStow{DishStow: &dish.DishStowRequest{Unstow: false}}
	case "unstow":
		req.Request = &api.Request_DishStow{DishStow: &dish.DishStowRequest{Unstow: true}}
	case "reboot":
		req.Request = &api.Request_Reboot{Reboot: &api.RebootRequest{}}
	}

	if _, err := api.NewDeviceClient(conn).Handle(ctx, &req); err != nil {
		fmt.Fprintf(os.Stderr, "starlinkd: %s failed: %v\n", cmd, err)
		return 1
	}
	return 0
}

// ---- panel control socket ----
//
// The dashboard tells starlinkd "open" (immediate refresh + fast-poll lease)
// and "close" (return to idle cadence) over a tiny line protocol on a Unix
// socket in $XDG_RUNTIME_DIR — tmpfs, 0600, no new dependencies. The QML
// panel heartbeats "open" every 60 s; the panelLease covers a crashed shell.
// CONTRACT.md "Panel control socket".

const ctrlSocketName = "starlinkd.sock"

func controlSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, ctrlSocketName)
	}
	if dir, err := stateDir(); err == nil {
		return filepath.Join(dir, ctrlSocketName)
	}
	return ""
}

// startControl listens for panel open/close commands; on error the daemon
// still runs, just without fast wake-ups.
func (d *daemon) startControl() error {
	path := controlSocketPath()
	if path == "" {
		return errors.New("control socket: no usable path")
	}
	if len(path) >= 104 {
		return fmt.Errorf("control socket: path too long (%d)", len(path))
	}
	// Never steal a live daemon's socket: if something is already answering
	// there, let it keep the panel channel (manual double-starts happen).
	if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		if c, derr := net.DialTimeout("unix", path, 300*time.Millisecond); derr == nil {
			c.Close()
			return errors.New("control socket: another daemon is listening")
		}
	}
	os.Remove(path) // stale socket from a previous run
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		d.vlog("control chmod: %v", err)
	}
	d.ctrlLn, d.ctrlPath = ln, path

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed at process exit
			}
			go serveControlConn(c, d.ctrlCh)
		}
	}()
	d.vlog("control socket: %s", path)
	return nil
}

func serveControlConn(c net.Conn, ch chan<- string) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 16)
	n, err := c.Read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	cmd := strings.TrimSpace(string(b[:n]))
	switch cmd {
	case "open", "close":
		select {
		case ch <- cmd:
		default:
		}
		_, _ = io.WriteString(c, "ok\n")
	default:
		_, _ = io.WriteString(c, "bad\n")
	}
}

// runPanel is the client side of the control socket: `starlinkd panel
// open|close`. Nonzero exit when the daemon is not listening (the QML caller
// ignores errors; the dashboard still works at idle cadence).
func runPanel(args []string) int {
	if len(args) != 1 || (args[0] != "open" && args[0] != "close") {
		fmt.Fprintln(os.Stderr, "usage: starlinkd panel open|close")
		return 2
	}
	path := controlSocketPath()
	if path == "" {
		fmt.Fprintln(osStderr, "starlinkd: control socket: no usable path")
		return 1
	}
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		fmt.Fprintf(osStderr, "starlinkd: control socket: %v\n", err)
		return 1
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(c, args[0]+"\n"); err != nil {
		fmt.Fprintf(osStderr, "starlinkd: control socket: %v\n", err)
		return 1
	}
	return 0
}

// panelActive reports whether the dashboard-open lease is live (lease also
// covers the one-shot probe burst; nextDelay distinguishes them).
func (d *daemon) panelActive(now time.Time) bool {
	return now.Before(d.panelUntil)
}

// applyControl handles one control-socket command ("open"/"close").
func (d *daemon) applyControl(cmd string, now time.Time) {
	switch cmd {
	case "open":
		d.panelUntil = now.Add(panelLease)
		d.geoBump = true // fresh egress answer when the dashboard wants it
		d.vlog("panel open: lease %s", panelLease)
	case "close":
		d.panelUntil = time.Time{}
		d.vlog("panel close")
	}
}

// nextDelay is the wait until the next background tick: the fast cadence
// while the lease or a probe burst is live, idle otherwise.
func (d *daemon) nextDelay() time.Duration {
	now := time.Now()
	if d.panelActive(now) || now.Before(d.probeUntil) {
		return d.cfg.interval
	}
	return d.cfg.idleInterval
}
