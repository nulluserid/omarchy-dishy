// Command starlinkd polls a Starlink dish over gRPC and maintains the state
// files described in CONTRACT.md: status.json, hist.json, daily.json and
// events.jsonl. It also offers one-shot control subcommands.
//
//	starlinkd [-t host:port] [-interval 2s] [-idle-interval 10s] [-once] [-v]
//	starlinkd stow|unstow|reboot [-t host:port]
//	starlinkd panel open|close
package main

import (
	"encoding/json"
	"flag"
	"fmt"
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
	once := fs.Bool("once", false, "run a single tick, write files, print status.json, exit")
	verbose := fs.Bool("v", false, "log transitions and errors to stderr")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println("starlinkd " + version)
		return 0
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
		once:                  *once,
		verbose:               *verbose,
		dir:                   dir,
	}, api.NewDeviceClient(conn))
	d.loadDaily()

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

	if err := d.startControl(); err != nil {
		fmt.Fprintf(osStderr, "starlinkd: %v (panel wake-ups disabled)\n", err)
	}

	// First tick is active: dashboard data (incl. hist.json) lands immediately.
	d.tick(true)
	for {
		tm := time.NewTimer(d.nextDelay())
		select {
		case <-tm.C:
			d.tick(d.panelActive(time.Now()))
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
