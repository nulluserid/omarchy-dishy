// Link state and egress classification for the off-network UI marker.
//
// status.json carries two additive blocks (CONTRACT.md "Link state & marker"):
//
//	link: {onStarlink, reason, since, lastDishOK}
//	geo:  {ts, ip, city, region, country, asn, org, isStarlink}
//
// Evidence, strongest first:
//
//	dish         — the dish answers GetStatus (its exact RPC target)
//	lan          — a local IPv4 address sits inside the dish target's /24
//	egress       — the egress IP's ASN/org resolves to Starlink
//	egress-other — fresh geo with a non-Starlink carrier: positively elsewhere
//	grace        — no evidence at all; keep showing for linkGraceWindow
//	               (outage, captive portal, dish reboot), then hide
//
// The egress lookup is ifconfig.co/json (no key, documented ~1 request/min
// courtesy limit): every geoInterval on the network, every
// geoTransitionInterval while classifying or after a dashboard open, and
// immediately after a default-route change. Lookups run off the tick path so
// a slow provider never delays a status write.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const geoEndpoint = "https://ifconfig.co/json"

// updateRoute refreshes the default-route fingerprint. A change starts the
// one-shot probe burst and demands a fresh egress lookup: the machine may
// have just left (or joined) the Starlink network.
func (d *daemon) updateRoute(now time.Time) {
	fp, ok := defaultRouteFingerprint()
	if !ok {
		return // table unreadable; keep the last known route
	}
	if fp == d.lastRoute && d.lastRouteKnown {
		return
	}
	changed := d.lastRouteKnown
	d.lastRoute, d.lastRouteKnown = fp, true
	if changed {
		d.vlog("default route: %s", fp)
		d.routeChanged = true
		d.probeUntil = now.Add(probeWindow)
	}
}

// localAddrInDishSubnet reports whether any local IPv4 address shares the
// dish target's /24.
func (d *daemon) localAddrInDishSubnet() bool {
	addrs, err := d.addrsFn()
	if err != nil {
		return false
	}
	return addrInSubnet(addrs, d.dishBase, d.dishMask)
}

// addrInSubnet is the pure subnet check (testable without live interfaces).
func addrInSubnet(addrs []net.Addr, base net.IP, mask net.IPMask) bool {
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip == nil {
			continue
		}
		if ip.Mask(mask).Equal(base) {
			return true
		}
	}
	return false
}

// updateLink recomputes link.onStarlink from the current evidence and applies
// the grace window when nothing positive is known. The grace anchor
// (d.lastPositive) is renewed only by real evidence — a quiet network
// (captive portal before login, Wi-Fi drop) converges to hidden in exactly
// linkGraceWindow. Runs on the tick loop.
func (d *daemon) updateLink(now time.Time, dishOK bool) {
	lastDish := d.link.LastDishOK
	if dishOK {
		lastDish = now.Unix()
	}
	d.lastDishOK = dishOK

	was := d.link.OnStarlink
	var on bool
	var reason string
	positive := false // current tick produced actual on-starlink evidence
	switch {
	case dishOK:
		on, reason, positive = true, "dish", true
	case d.localAddrInDishSubnet():
		on, reason, positive = true, "lan", true
	case geoEvidence(d.geo, now):
		if d.geo.IsStarlink {
			on, reason, positive = true, "egress", true
		} else {
			on, reason = false, "egress-other"
		}
	default:
		// No evidence right now: keep the current belief through the grace
		// window, then flip off. An unlucky start (boot before network) is
		// already off.
		if now.Unix()-d.lastPositive <= int64(linkGraceWindow.Seconds()) && d.link.Since > 0 {
			on, reason = d.link.OnStarlink, "grace"
		} else {
			on, reason = false, "grace"
		}
	}
	if positive {
		d.lastPositive = now.Unix()
	}

	nl := d.link
	nl.Reason = reason
	nl.LastDishOK = lastDish
	if on != was {
		nl.OnStarlink = on
		nl.Since = now.Unix()
	}
	d.link = nl
	if on != was {
		d.onLinkChange(now, on)
	}
}

// linkConfirmed answers whether the CURRENT evidence positively says
// Starlink — not a held grace belief. Outage tracking and daily booking gate
// on this (CONTRACT.md "Notifications": suppressed during grace), so a
// network with no evidence at all never books "Starlink offline" time.
func (d *daemon) linkConfirmed() bool {
	switch d.link.Reason {
	case "dish", "lan", "egress":
		return true
	}
	return false
}

// onLinkChange reacts to an onStarlink flip.
func (d *daemon) onLinkChange(now time.Time, on bool) {
	if on {
		d.failed = 0
		if d.openOutage != nil {
			// Back on the dish mid-outage: re-arm so a persisting dish outage
			// notifies again instead of silently staying open.
			d.openOutage = nil
		}
		return
	}
	// Leaving the Starlink network: close any open local outage quietly (the
	// machine left, the dish did not fail) and log the end without a
	// notification — CONTRACT.md "Off-network accounting".
	if d.openOutage != nil {
		cause := d.openOutage.cause
		durS := now.Unix() - d.openOutage.start
		d.openOutage = nil
		d.emit(event{Type: "outage_end", Cause: cause + " (left network)", DurS: durS}, "", "")
	}
}

// geoEvidence answers whether the stored geo result is fresh enough to act as
// evidence (positive or negative). Results older than the grace window count
// as no evidence at all.
func geoEvidence(g geoInfo, now time.Time) bool {
	return g.Ts > 0 && now.Unix()-g.Ts <= int64(linkGraceWindow.Seconds())
}

// maybeFetchGeo starts an egress lookup when the stored one is due: on the
// normal clock while on-network, on the transition clock while classifying,
// and (on the dashboard-open open command) whenever it is older than the
// transition interval.
func (d *daemon) maybeFetchGeo(now time.Time, active bool) {
	if d.geoFetching {
		return
	}
	age := now.Sub(time.Unix(d.geo.Ts, 0))
	// Fresh egress lookup whenever the answer could change: normal clock on a
	// settled network, transition clock while classifying or right after a
	// default-route flip.
	if pending := !d.link.OnStarlink || d.routeChanged; age >= d.cfg.geoInterval ||
		(pending && age >= d.cfg.geoTransitionInterval) {
		d.routeChanged = false
		d.startGeoFetch()
		return
	}
	if active && d.geoBump && age >= d.cfg.geoTransitionInterval {
		d.geoBump = false
		d.routeChanged = false
		d.startGeoFetch()
	}
}

func (d *daemon) startGeoFetch() {
	d.geoFetching = true
	go func() {
		g, err := fetchGeo()
		if err != nil {
			d.vlog("geo: %v", err)
		}
		select {
		case d.geoCh <- g:
		default:
		}
	}()
}

// drainGeo applies any completed lookups; called at the top of each tick so
// a result never lags into the next standing cadence.
func (d *daemon) drainGeo() {
	for {
		select {
		case g := <-d.geoCh:
			d.applyGeo(g)
		default:
			return
		}
	}
}

func (d *daemon) applyGeo(g geoInfo) {
	d.geoFetching = false
	if g.Ts == 0 {
		return // failed lookup: keep the last known result
	}
	d.geo = g
	d.updateLink(time.Now(), d.lastDishOK)
}

// fetchGeo performs one egress lookup. A failed call returns a zero geoInfo —
// applyGeo keeps the previous result.
func fetchGeo() (geoInfo, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, geoEndpoint, nil)
	if err != nil {
		return geoInfo{}, err
	}
	req.Header.Set("User-Agent", "starlinkd/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return geoInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return geoInfo{}, fmt.Errorf("http %d", resp.StatusCode)
	}
	var raw struct {
		IP         string `json:"ip"`
		City       string `json:"city"`
		RegionName string `json:"region_name"`
		CountryISO string `json:"country_iso"`
		ASN        string `json:"asn"`
		ASNOrg     string `json:"asn_org"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return geoInfo{}, err
	}
	return geoInfo{
		Ts:         time.Now().Unix(),
		IP:         raw.IP,
		City:       raw.City,
		Region:     raw.RegionName,
		Country:    raw.CountryISO,
		ASN:        raw.ASN,
		Org:        raw.ASNOrg,
		IsStarlink: isStarlinkCarrier(raw.ASN, raw.ASNOrg),
	}, nil
}

// isStarlinkCarrier matches the Starlink egress: ASN AS14593 or a SpaceX /
// Starlink organization string (case-insensitive).
func isStarlinkCarrier(asn, org string) bool {
	if strings.EqualFold(strings.TrimSpace(asn), starlinkASN) {
		return true
	}
	o := strings.ToLower(org)
	return strings.Contains(o, "spacex") || strings.Contains(o, "starlink")
}

// defaultRouteFingerprint reads /proc/net/route (Linux only) and returns
// "iface/gateway" for the first IPv4 default entry carrying a gateway.
// ok=false means the table could not be read; the caller keeps the last
// known route in that case.
func defaultRouteFingerprint() (fp string, ok bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[0] == "Iface" {
			continue
		}
		// Iface, Destination, Gateway, Flags,...
		if fields[1] != "00000000" || fields[2] == "00000000" {
			continue
		}
		gw, err := decodeRouteHex(fields[2])
		if err != nil {
			continue
		}
		return fields[0] + "/" + gw, true
	}
	return "", true
}

// decodeRouteHex decodes /proc/net/route's little-endian hex IPv4 quad.
func decodeRouteHex(h string) (string, error) {
	if len(h) != 8 {
		return "", errors.New("route: bad hex " + h)
	}
	b := make([]byte, net.IPv4len)
	for i := range b {
		v, err := strconv.ParseUint(h[i*2:(i+1)*2], 16, 8)
		if err != nil {
			return "", err
		}
		b[i] = byte(v)
	}
	return net.IPv4(b[3], b[2], b[1], b[0]).String(), nil
}
