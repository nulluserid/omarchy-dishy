// Command starlinkd polls a Starlink dish over gRPC and maintains the state
// files described in CONTRACT.md: status.json, hist.json, daily.json and
// events.jsonl. It also offers one-shot control subcommands.
//
//	starlinkd [-t host:port] [-interval 2s] [-map-interval 60s] [-once] [-v]
//	starlinkd stow|unstow|reboot [-t host:port]
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

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "stow", "unstow", "reboot":
			os.Exit(runControl(os.Args[1], os.Args[2:]))
		}
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("starlinkd", flag.ContinueOnError)
	target := fs.String("t", "192.168.100.1:9200", "dish gRPC target host:port")
	interval := fs.Duration("interval", 2*time.Second, "poll interval")
	mapInterval := fs.Duration("map-interval", 60*time.Second, "obstruction map refresh interval")
	once := fs.Bool("once", false, "run a single tick, write files, print status.json, exit")
	verbose := fs.Bool("v", false, "log transitions and errors to stderr")
	if err := fs.Parse(args); err != nil {
		return 2
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
		target:      *target,
		interval:    *interval,
		mapInterval: *mapInterval,
		once:        *once,
		verbose:     *verbose,
		dir:         dir,
	}, api.NewDeviceClient(conn))
	d.loadDaily()

	if *once {
		d.tick()
		b, err := json.Marshal(d.lastFile)
		if err != nil {
			fmt.Fprintf(osStderr, "starlinkd: %v\n", err)
			return 1
		}
		fmt.Println(string(b))
		return 0
	}

	d.tick()
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for range ticker.C {
		d.tick()
	}
	return 0
}
