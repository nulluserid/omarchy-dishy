// Command starlinkd polls a Starlink dish over gRPC and maintains the state
// files described in CONTRACT.md: status.json, hist.json, daily.json and
// events.jsonl. It also offers one-shot control subcommands.
//
//	starlinkd [-t host:port] [-interval 2s] [-idle-interval 10s] [-idle-exit <duration>] [-once] [-v] [-geo-url https://ifconfig.co/json]
//	starlinkd stow|unstow|reboot [-t host:port]
//	starlinkd panel open|close|alive
//	starlinkd set host <host[:port]>
//
// The dish target resolves as: -t flag > config.json > 192.168.100.1:9200.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	api "starlinkd/gen/spacex_api/device/device"
)

var osStderr = os.Stderr

// version is stamped at build time (-ldflags -X main.version=...).
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "stow", "unstow", "reboot":
			os.Exit(runControl(os.Args[1], os.Args[2:]))
		case "panel":
			os.Exit(runPanel(os.Args[2:]))
		case "set":
			os.Exit(runSetHost(os.Args[2:]))
		}
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("starlinkd", flag.ContinueOnError)
	target := fs.String("t", "192.168.100.1:9200", "dish gRPC target host:port")
	interval := fs.Duration("interval", 2*time.Second, "poll interval while the dashboard is open")
	idleInterval := fs.Duration("idle-interval", 10*time.Second, "poll interval while the dashboard is closed")
	idleHistInterval := fs.Duration("idle-hist-interval", 60*time.Second, "background history refresh while the dashboard is closed")
	mapInterval := fs.Duration("map-interval", 60*time.Second, "obstruction map refresh interval (dashboard open)")
	// -geo-url exists for the demo harness alone: a local server answers
	// with sample egress data, so screenshots never show a real IP or
	// location. The systemd unit never sets it. It is dialed IPv4 only
	// (fetchGeoURL): serve it on 127.0.0.1, not [::1].
	geoURL := fs.String("geo-url", defaultGeoEndpoint, "egress lookup URL (absolute http or https, with a host)")
	once := fs.Bool("once", false, "run a single tick, write files, print status.json, exit")
	verbose := fs.Bool("v", false, "log transitions and errors to stderr")
	showVersion := fs.Bool("version", false, "print version and exit")
	idleExit := fs.Duration("idle-exit", 0, "exit once no control-socket command (alive/open/close/set host) arrived for this long; 0 = never")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println("starlinkd " + version)
		return 0
	}
	// -geo-url is validated to exactly this much: an absolute http(s) URL
	// with a host. Anything else (a relative path, another scheme, no
	// host) is a usage error, refused before anything dials or creates a
	// state directory.
	if u, err := url.Parse(*geoURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		fmt.Fprintln(osStderr, "starlinkd: -geo-url must be an absolute http or https URL with a host")
		return 2
	}
	// Cadence sanity: idle must never be faster than active.
	if *idleInterval < *interval {
		*idleInterval = *interval
	}
	if *idleHistInterval < *idleInterval {
		*idleHistInterval = *idleInterval
	}

	dir, err := stateDir()
	if err != nil {
		fmt.Fprintf(osStderr, "starlinkd: state dir: %v\n", err)
		return 1
	}
	// Daemon mode only: -once returns further down without publishing, and
	// the CLI subcommands never reach run(). A runtime dir that is not this
	// user's alone is a quiet skip (the consumers fall back to the env
	// resolution); a real failure is reported. Never fatal.
	if !*once {
		if err := publishStateDir(dir); err != nil {
			switch skip := errors.Is(err, errRuntimeDirUntrusted); {
			case skip && *verbose:
				fmt.Fprintf(osStderr, "starlinkd: state dir: %v (not published; the widget falls back)\n", err)
			case !skip:
				fmt.Fprintf(osStderr, "starlinkd: %v (state dir not published to the widget)\n", err)
			}
		}
	}

	// Target precedence: explicit -t > config.json (`set host`) > default.
	tSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "t" {
			tSet = true
		}
	})
	if !tSet {
		if cfgT, ok := loadTargetConfig(dir); ok {
			*target = cfgT
		}
	}

	conn, err := dial(*target)
	if err != nil {
		fmt.Fprintf(osStderr, "starlinkd: dial %s: %v\n", *target, err)
		return 1
	}
	defer conn.Close()

	d := newDaemon(config{
		target:                *target,
		interval:              *interval,
		idleInterval:          *idleInterval,
		idleHistInterval:      *idleHistInterval,
		mapInterval:           *mapInterval,
		geoInterval:           5 * time.Minute,
		geoTransitionInterval: 60 * time.Second,
		geoEndpoint:           *geoURL,
		once:                  *once,
		verbose:               *verbose,
		dir:                   dir,
		idleExit:              *idleExit,
	}, api.NewDeviceClient(conn))
	d.conn = conn
	// Per-dish partitions: preload the last-seen dish's counters so restarts
	// (and fail ticks while that dish is down) land in the right partition.
	// With no identity on record, the legacy top-level files keep startup
	// behavior until the first good tick switches to a partition.
	d.adoptLastDish(time.Now())
	if d.dishDir == "" {
		d.loadDaily()
		d.initDay(time.Now())
	}
	// Retention sweep: drop dish partitions (and legacy root day files)
	// idle past the window, before the first tick books anything.
	d.tidyDishes(time.Now())

	if *once {
		d.tick(true)
		b, err := json.Marshal(d.lastFile)
		if err != nil {
			fmt.Fprintf(osStderr, "starlinkd: %v\n", err)
			return 1
		}
		fmt.Println(string(b))
		return 0
	}

	// The notification helper is resolved and verified here, once, before
	// the tick loop — never from ambient PATH at notification time. A
	// refusal is reported once, unconditionally: the unit does not pass -v,
	// so notifications stopping would otherwise leave no journal line.
	if err := d.resolveNotifySend(); err != nil {
		fmt.Fprintf(osStderr, "starlinkd: notifications disabled: %v\n", err)
	}
	if err := d.startControl(); err != nil {
		fmt.Fprintf(osStderr, "starlinkd: %v (panel wake-ups disabled)\n", err)
	}

	// First tick is active: dashboard data (incl. hist.json) lands immediately.
	d.tick(true)
	for {
		// The wait ends at the next tick or at the idle deadline, whichever
		// comes first: a daemon nobody polls any more leaves on time.
		tm := time.NewTimer(d.nextWake())
		select {
		case <-tm.C:
			now := time.Now()
			if d.idleExpired(now) {
				d.vlog("idle exit: no control-socket command for %s", d.cfg.idleExit)
				d.closeControl() // the gRPC connection closes with the deferred Close below
				return 0
			}
			d.tick(d.panelActive(now))
		case cmd := <-d.ctrlCh:
			if !tm.Stop() {
				select {
				case <-tm.C:
				default:
				}
			}
			d.applyControl(cmd, time.Now())
			if cmd == "open" {
				d.tick(true) // immediate refresh on dashboard open
			}
		}
	}
}
