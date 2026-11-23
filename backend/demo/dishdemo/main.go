// Command dishdemo is the Dishy demo kit's fake Starlink dish (see
// backend/demo/README.md). It answers the gRPC requests starlinkd issues,
// serves a local stand-in for the egress lookup, and can seed a sandbox state
// directory with a populated history. Every number is a pure function of the
// wall clock, so the data moves while the panel is open. Nothing about a real
// dish is read or reproduced, and it binds to loopback only.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api "starlinkd/gen/spacex_api/device/device"
	dish "starlinkd/gen/spacex_api/device/dish"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9210", "gRPC host:port, a numeric loopback address only")
	geoListen := flag.String("geo-listen", "127.0.0.1:9211", "egress HTTP host:port, a numeric loopback address only")
	scenario := flag.String("scenario", "clean", "clean | busy | trouble")
	seed := flag.String("seed", "", "write a populated starlinkd state directory here, then exit")
	verbose := flag.Bool("v", false, "log every request to stderr")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	grpcAddr, err := loopbackAddr(*listen)
	if err != nil {
		fail(err)
	}
	if *seed != "" {
		if err := runSeed(*seed, *scenario, grpcAddr); err != nil {
			fail(err)
		}
		return
	}
	d, err := newFakeDish(*scenario, time.Now())
	if err != nil {
		fail(err)
	}
	d.verbose = *verbose

	geoAddr, err := loopbackAddr(*geoListen)
	if err != nil {
		fail(err)
	}
	ln, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		fail(err)
	}
	geoLn, err := net.Listen("tcp", geoAddr)
	if err != nil {
		fail(err)
	}

	srv := grpc.NewServer()
	api.RegisterDeviceServer(srv, d)
	geo := &http.Server{Handler: geoHandler(d), ReadHeaderTimeout: 5 * time.Second}
	go func() { log.Print("egress server: ", geo.Serve(geoLn)) }()
	log.Printf("scenario %s: gRPC %s, egress http://%s/json", *scenario, grpcAddr, geoAddr)
	if err := srv.Serve(ln); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "dishdemo:", err)
	os.Exit(1)
}

// loopbackAddr keeps only numeric loopback literals, so the fake dish can
// never answer another machine.
func loopbackAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("%s is not a numeric loopback address", addr)
	}
	return net.JoinHostPort(ip.String(), port), nil
}

// geoHandler serves ifconfig.co's schema as backend/link.go parses it: an
// RFC 5737 documentation address and an invented exit town.
func geoHandler(d *fakeDish) http.Handler {
	const geoJSON = `{"ip":"203.0.113.7","city":"Marquette","region_name":"Michigan",` +
		`"country_iso":"US","asn":"AS14593","asn_org":"Starlink"}` + "\n"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json", func(w http.ResponseWriter, r *http.Request) {
		d.vlogf("geo %s", r.UserAgent())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, geoJSON)
	})
	return mux
}

func (d *fakeDish) vlogf(format string, args ...any) {
	if d.verbose {
		log.Printf(format, args...)
	}
}

// Handle answers what starlinkd polls. Stow and reboot succeed without
// changing the scenario, so the panel's controls work against the demo.
func (d *fakeDish) Handle(_ context.Context, req *api.Request) (*api.Response, error) {
	now := time.Now()
	resp := &api.Response{Id: req.GetId(), ApiVersion: apiVersion}
	switch r := req.GetRequest().(type) {
	case *api.Request_GetStatus:
		d.vlogf("get_status")
		resp.Response = &api.Response_DishGetStatus{DishGetStatus: d.status(now)}
	case *api.Request_GetHistory:
		d.vlogf("get_history")
		resp.Response = &api.Response_DishGetHistory{DishGetHistory: d.history(now)}
	case *api.Request_DishGetObstructionMap:
		d.vlogf("get_obstruction_map")
		resp.Response = &api.Response_DishGetObstructionMap{DishGetObstructionMap: d.sky}
	case *api.Request_DishStow:
		log.Printf("DishStow unstow=%v (answered, nothing changes)", r.DishStow.GetUnstow())
		resp.Response = &api.Response_DishStow{DishStow: &dish.DishStowResponse{}}
	case *api.Request_Reboot:
		log.Print("Reboot (answered, nothing changes)")
		resp.Response = &api.Response_Reboot{Reboot: &api.RebootResponse{}}
	default:
		return nil, status.Errorf(codes.Unimplemented, "dishdemo: %T is not served", r)
	}
	return resp, nil
}
