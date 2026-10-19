package main

import (
	"context"
	"flag"
	"fmt"
	"os"
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
