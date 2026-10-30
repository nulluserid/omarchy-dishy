package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	api "starlinkd/gen/spacex_api/device/device"
	dish "starlinkd/gen/spacex_api/device/dish"
)

// runControl performs a one-shot stow/unstow/reboot, then exits.
// Exit 0 on success; nonzero + stderr message on failure.
func runControl(cmd string, args []string) int {
	fs := flag.NewFlagSet("starlinkd "+cmd, flag.ContinueOnError)
	target := fs.String("t", "", "dish gRPC target host:port (default: config.json target or 192.168.100.1:9200)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	t := *target
	if t == "" {
		// Same precedence as the daemon minus the (absent) flag: config then
		// default, so customized targets work for one-shot controls too.
		if dir, err := stateDir(); err == nil {
			if cfgT, ok := loadTargetConfig(dir); ok {
				t = cfgT
			}
		}
	}
	if t == "" {
		t = "192.168.100.1:9200"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := dial(t)
	if err != nil {
		fmt.Fprintf(os.Stderr, "starlinkd: dial %s: %v\n", t, err)
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

// ---- runtime dish target ----
//
// The target resolves as: explicit -t flag > config.json (written by
// `starlinkd set host`) > built-in default. The default stays the dish's
// link-local address; there is deliberately NO hostname fallback — DNS is
// untrustworthy on VPNs and foreign networks, and a silent fallback could
// aim dish gRPC at whatever answers a name.

const configName = "config.json"

// daemonConfig is the persisted daemon configuration: the dish gRPC target
// plus the last-seen dish identity (so a restart preloads the right per-dish
// state partition even before that dish answers — CONTRACT.md v4).
type daemonConfig struct {
	Target   string `json:"target"`
	LastDish string `json:"lastDish,omitempty"`
}

// normalizeTarget validates a host[:port] target and appends the default
// port when omitted. Rejects empties, whitespace, control characters, and
// bad ports; caps length at the DNS limit.
func normalizeTarget(t string) (string, error) {
	t = strings.TrimSpace(t)
	if t == "" {
		return "", fmt.Errorf("empty target")
	}
	if len(t) > 253 {
		return "", fmt.Errorf("target too long (%d)", len(t))
	}
	for _, r := range t {
		if r <= ' ' || r == 0x7f {
			return "", fmt.Errorf("invalid character in target")
		}
	}
	host, port := t, "9200"
	if i := strings.LastIndex(t, ":"); i >= 0 {
		host, port = t[:i], t[i+1:]
		if host == "" {
			return "", fmt.Errorf("missing host before ':'")
		}
	}
	// A bare IPv6 literal ("::1") would split into garbage host/port —
	// require bracketed form, matching the widget-side validation.
	if strings.Contains(host, ":") && !(strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]")) {
		return "", fmt.Errorf("bare IPv6 needs brackets: %q", host)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("bad port %q", port)
	}
	return host + ":" + port, nil
}

// loadTargetConfig reads the persisted target; missing/corrupt yields none.
func loadTargetConfig(dir string) (string, bool) {
	dc, ok := loadDaemonConfig(dir)
	if !ok {
		return "", false
	}
	t, err := normalizeTarget(dc.Target)
	if err != nil {
		return "", false
	}
	return t, true
}

// loadDaemonConfig reads the full persisted configuration.
func loadDaemonConfig(dir string) (daemonConfig, bool) {
	b, err := os.ReadFile(filepath.Join(dir, configName))
	if err != nil {
		return daemonConfig{}, false
	}
	var dc daemonConfig
	if err := json.Unmarshal(b, &dc); err != nil {
		return daemonConfig{}, false
	}
	return dc, true
}

// writeTargetConfig persists the target, preserving any recorded dish
// identity (atomic) so the next daemon start picks both up even when the
// write happens while no daemon is running.
func writeTargetConfig(dir, target string) error {
	dc, _ := loadDaemonConfig(dir)
	dc.Target = target
	return writeDaemonConfig(dir, dc)
}

// writeDaemonConfig persists the full configuration (atomic).
func writeDaemonConfig(dir string, dc daemonConfig) error {
	return atomicWrite(filepath.Join(dir, configName), marshalCompact(dc))
}

// setTarget switches the live daemon to a new dish target: validate, persist,
// swap the connection. Runs only from the tick loop (applyControl), so the
// swap is race-free with in-flight RPCs — fetches use d.client per call.
func (d *daemon) setTarget(target string) error {
	t, err := normalizeTarget(target)
	if err != nil {
		return err
	}
	if t == d.cfg.target {
		return nil
	}
	nc, err := dial(t)
	if err != nil {
		return err
	}
	old := d.conn
	d.conn = nc
	d.client = api.NewDeviceClient(nc)
	d.cfg.target = t
	if err := writeDaemonConfig(d.cfg.dir, daemonConfig{Target: t, LastDish: d.dishID}); err != nil {
		d.vlog("write %s: %v", configName, err)
	}
	if old != nil {
		old.Close()
	}
	d.vlog("target: %s", t)
	return nil
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
	b := make([]byte, 256)
	n, err := c.Read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	cmd := strings.TrimSpace(string(b[:n]))
	forward := ""
	switch {
	case cmd == "open" || cmd == "close":
		forward = cmd
	case strings.HasPrefix(cmd, "set host "):
		if _, err := normalizeTarget(strings.TrimPrefix(cmd, "set host ")); err != nil {
			_, _ = io.WriteString(c, "bad\n")
			return
		}
		forward = cmd
	}
	if forward != "" {
		select {
		case ch <- forward:
		default:
		}
		_, _ = io.WriteString(c, "ok\n")
		return
	}
	_, _ = io.WriteString(c, "bad\n")
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

// runSetHost is the client side of `starlinkd set host <host[:port]>`.
// Prefers the live daemon's control socket (validates, persists, swaps the
// connection); with no daemon listening, persists config.json directly so
// the next start picks the target up.
func runSetHost(args []string) int {
	if len(args) != 2 || args[0] != "host" {
		fmt.Fprintln(os.Stderr, "usage: starlinkd set host <host[:port]>")
		return 2
	}
	t, err := normalizeTarget(args[1])
	if err != nil {
		fmt.Fprintf(osStderr, "starlinkd: %v\n", err)
		return 2
	}
	if path := controlSocketPath(); path != "" {
		c, derr := net.DialTimeout("unix", path, 2*time.Second)
		if derr == nil {
			c.SetDeadline(time.Now().Add(2 * time.Second))
			_, werr := io.WriteString(c, "set host "+t+"\n")
			b := make([]byte, 8)
			n, rerr := c.Read(b)
			c.Close()
			if werr == nil && rerr == nil && strings.TrimSpace(string(b[:n])) == "ok" {
				return 0
			}
			fmt.Fprintln(osStderr, "starlinkd: set host rejected")
			return 1
		}
		// No daemon listening: persist for the next start.
	}
	dir, err := stateDir()
	if err != nil {
		fmt.Fprintf(osStderr, "starlinkd: state dir: %v\n", err)
		return 1
	}
	if err := writeTargetConfig(dir, t); err != nil {
		fmt.Fprintf(osStderr, "starlinkd: %v\n", err)
		return 1
	}
	fmt.Println("starlinkd: dish target set to " + t + " (used at next daemon start)")
	return 0
}

// panelActive reports whether the dashboard-open lease is live (lease also
// covers the one-shot probe burst; nextDelay distinguishes them).
func (d *daemon) panelActive(now time.Time) bool {
	return now.Before(d.panelUntil)
}

// applyControl handles one control-socket command ("open"/"close"/"set host …").
func (d *daemon) applyControl(cmd string, now time.Time) {
	switch {
	case cmd == "open":
		d.panelUntil = now.Add(panelLease)
		d.geoBump = true // fresh egress answer when the dashboard wants it
		d.vlog("panel open: lease %s", panelLease)
	case cmd == "close":
		d.panelUntil = time.Time{}
		d.vlog("panel close")
	case strings.HasPrefix(cmd, "set host "):
		if err := d.setTarget(strings.TrimPrefix(cmd, "set host ")); err != nil {
			d.vlog("set host: %v", err)
		}
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
