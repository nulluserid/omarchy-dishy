package main

import (
	"net"
	"time"

	api "starlinkd/gen/spacex_api/device/device"
	dish "starlinkd/gen/spacex_api/device/dish"
)

// Ring and cadence constants from CONTRACT.md.
const (
	ringSize        = 900 // history samples, 1s apart
	sampleIntervalS = 1
	rpcTimeout      = 3 * time.Second // per RPC; dish may vanish mid-tick

	// Link/panel dynamics (CONTRACT.md "Link state & marker").
	linkGraceWindow = 10 * time.Minute // hide delay once evidence stops
	probeWindow     = 30 * time.Second // fast-poll burst after a failure or route change
	panelLease      = 90 * time.Second // dashboard-open lease; QML heartbeats every 60s
	starlinkASN     = "AS14593"

	// Single-tick booking cap: timers freeze across suspend/resume, and the
	// first tick after waking carries a huge dt — book at most this (2 idle
	// ticks + tick time), never the sleep.
	maxBookedDt = 30 * time.Second
	// Freshness cap for dish-reported outage announcements: announce starts
	// at most this old (covers the 60 s idle-skim gap plus slack); older
	// entries only advance the watermark.
	maxOutageAnnounceWindow = 122 * time.Second
)

// Array index order inside daemon.arrays.
const (
	aLat = iota
	aDown
	aUp
	aPow
	aDrop
	arrayCount
)

type config struct {
	target                string
	interval              time.Duration // poll cadence while the dashboard is open
	idleInterval          time.Duration // poll cadence while the dashboard is closed
	idleHistInterval      time.Duration // background history refresh while closed
	mapInterval           time.Duration
	geoInterval           time.Duration // egress lookup while on-network
	geoTransitionInterval time.Duration // egress lookup while classifying / dashboard just opened
	once                  bool
	verbose               bool
	dir                   string // state dir, ~/.local/state/starlinkd
}

// ---- status.json ----

type dishInfo struct {
	ID        string `json:"id"`
	HW        string `json:"hw"`
	SW        string `json:"sw"`
	BootCount int64  `json:"bootCount"`
	UptimeS   int64  `json:"uptimeS"`
	EthMbps   int64  `json:"ethMbps"`
}

type liveStats struct {
	LatencyMs float64 `json:"latencyMs"`
	DownMbps  float64 `json:"downMbps"`
	UpMbps    float64 `json:"upMbps"`
	PowerW    float64 `json:"powerW"`
}

type lat5mStats struct {
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	Mean float64 `json:"mean"`
}

type drop15Stats struct {
	Ratio        float64 `json:"ratio"`
	TotalOutageS int64   `json:"totalOutageS"`
	Count15m     int64   `json:"count15m"`
}

type usageStats struct {
	DownGB        float64 `json:"downGB"`
	UpGB          float64 `json:"upGB"`
	AllTimeDownGB float64 `json:"allTimeDownGB"`
	AllTimeUpGB   float64 `json:"allTimeUpGB"`
}

type availStats struct {
	TodayPct   float64 `json:"todayPct"`
	SessionPct float64 `json:"sessionPct"`
}

type obsStats struct {
	Fraction        float64 `json:"fraction"`
	TimeObstructedS int64   `json:"timeObstructedS"`
	ValidS          int64   `json:"validS"`
}

type updateStats struct {
	State       string  `json:"state"`
	ProgressPct float64 `json:"progressPct"`
}

type gpsStats struct {
	Valid bool  `json:"valid"`
	Sats  int64 `json:"sats"`
}

// linkInfo is the off-network UI marker: QML hides the widget whenever
// onStarlink is false; consumers that predate this field never see it.
type linkInfo struct {
	OnStarlink bool   `json:"onStarlink"`
	Reason     string `json:"reason"`     // dish|lan|egress|egress-other|grace
	Since      int64  `json:"since"`      // unix s of the last onStarlink flip
	LastDishOK int64  `json:"lastDishOK"` // unix s of the last successful dish RPC
}

// geoInfo is the egress lookup result (ifconfig.co/json): public IP plus an
// approximate exit location and the carrier the traffic leaves through.
type geoInfo struct {
	Ts         int64  `json:"ts"`
	IP         string `json:"ip,omitempty"`
	City       string `json:"city,omitempty"`
	Region     string `json:"region,omitempty"`
	Country    string `json:"country,omitempty"`
	ASN        string `json:"asn,omitempty"`
	Org        string `json:"org,omitempty"`
	IsStarlink bool   `json:"isStarlink"`
}

type statusFile struct {
	Ts          int64       `json:"ts"`
	Ok          bool        `json:"ok"`
	Error       string      `json:"error"`
	Dish        dishInfo    `json:"dish"`
	State       string      `json:"state"`
	StateDetail string      `json:"stateDetail"`
	Live        liveStats   `json:"live"`
	Lat5m       lat5mStats  `json:"lat5m"`
	Drop15m     drop15Stats `json:"drop15m"`
	Usage       usageStats  `json:"usage"`
	Avail       availStats  `json:"avail"`
	Obs         obsStats    `json:"obs"`
	Alerts      []alert     `json:"alerts"`
	Update      updateStats `json:"update"`
	Gps         gpsStats    `json:"gps"`
	Link        linkInfo    `json:"link"`
	Geo         *geoInfo    `json:"geo,omitempty"`
}

// ---- hist.json ----

type outage struct {
	Cause string `json:"cause"`
	Start int64  `json:"start"`
	DurS  int64  `json:"durS"`
}

type obsMap struct {
	Rows        int       `json:"rows"`
	Cols        int       `json:"cols"`
	MaxThetaDeg float64   `json:"maxThetaDeg"`
	Snr         []float64 `json:"snr"` // row-major, -1 = no data
}

type histFile struct {
	Ts        int64      `json:"ts"`
	N         int        `json:"n"`
	IntervalS int        `json:"intervalS"`
	LatencyMs []*float64 `json:"latencyMs"` // null = dropped ping
	DownMbps  []*float64 `json:"downMbps"`
	UpMbps    []*float64 `json:"upMbps"`
	PowerW    []*float64 `json:"powerW"`
	DropRate  []*float64 `json:"dropRate"`
	Outages   []outage   `json:"outages"` // newest first, max 20
	Map       *obsMap    `json:"map"`
}

// ---- daily.json ----

type dailyAllTime struct {
	DownBytes int64 `json:"downBytes"`
	UpBytes   int64 `json:"upBytes"`
}

type dailyFile struct {
	Date      string       `json:"date"`
	DownBytes int64        `json:"downBytes"`
	UpBytes   int64        `json:"upBytes"`
	OnlineS   int64        `json:"onlineS"`
	TotalS    int64        `json:"totalS"`
	Outages   int64        `json:"outages"`
	AllTime   dailyAllTime `json:"allTime"`
}

// ---- events.jsonl ----

type event struct {
	Ts       int64   `json:"ts"`
	Type     string  `json:"type"`
	Cause    string  `json:"cause,omitempty"`
	DurS     int64   `json:"durS,omitempty"`
	ID       string  `json:"id,omitempty"`
	Label    string  `json:"label,omitempty"`
	Fraction float64 `json:"fraction,omitempty"`
	From     string  `json:"from,omitempty"`
	To       string  `json:"to,omitempty"`
}

// daemon carries all cross-tick state. Every field is touched only from the
// single tick loop, so there is no locking.
type daemon struct {
	cfg    config
	client api.DeviceClient

	arrays      [arrayCount][]*float64 // ring buffers, oldest first, null = no data
	sampled     bool                   // ever ingested a history response
	prevCur     uint64                 // history cursor of last ingest
	haveUptime  bool
	lastUptime  uint64
	lastNewest  int       // global ring index of the newest consumed sample
	startWall   time.Time // daemon start; usage counts only later samples
	lastPowerW  float64   // newest powerIn sample
	lastOutages []outage  // dish-reported outages, last history
	hasHist     bool

	daily          dailyFile
	sessionS       int64 // wall seconds since daemon start, link-gated
	sessionOnlineS int64

	prevState  string
	prevAlerts map[string]bool
	warm       bool // first good tick observed (no events/notifications)
	hyst       hyst

	openOutage         *outageTrack
	failed             int
	maxDishOutageStart int64

	mapData  *obsMap
	lastMap  time.Time
	lastHist time.Time // last successful history ingest (drives idle cadence)

	// Link state (backend/link.go). Consumers: only the tick loop.
	link           linkInfo // onStarlink/reason/since/lastDishOK, published in status.json
	lastPositive   int64    // unix s of the last on-starlink evidence
	lastDishOK     bool     // outcome of the most recent GetStatus
	geo            geoInfo  // last egress lookup; Ts=0 until the first call
	geoFetching    bool     // an egress lookup is in flight
	geoBump        bool     // dashboard (re)opened: refresh geo if older than transition interval
	geoCh          chan geoInfo
	lastRoute      string // default-route fingerprint, "" = no default route
	lastRouteKnown bool
	routeChanged   bool      // fingerprint changed since the last geo launch
	probeUntil     time.Time // one-shot fast-poll burst end
	panelUntil     time.Time // dashboard-open lease end
	lastTickAt     time.Time // previous round's tick time, for elapsed dt
	lastTickOK     bool
	dishBase       net.IP // dish target /24
	dishMask       net.IPMask
	addrsFn        func() ([]net.Addr, error) // injectable for tests
	ctrlLn         net.Listener               // panel control socket (optional)
	ctrlPath       string
	ctrlCh         chan string

	lastStatus *dish.DishGetStatusResponse // last good status, for dish info on failures
	lastObs    obsStats
	lastFile   *statusFile // printed by -once
	lastErr    string      // for rate-limited error logging
}

func newDaemon(cfg config, client api.DeviceClient) *daemon {
	d := &daemon{
		cfg:        cfg,
		client:     client,
		hyst:       newHyst(60, 300),
		startWall:  time.Now(),
		lastTickAt: time.Now(),
		geoCh:      make(chan geoInfo, 1),
		ctrlCh:     make(chan string, 4),
		addrsFn:    net.InterfaceAddrs,
	}
	d.dishBase, d.dishMask = dishSubnet(cfg.target)
	d.resetArrays()
	return d
}

// dishSubnet derives the /24 around the dish target. A named target (rare)
// falls back to the default dish LAN.
func dishSubnet(target string) (net.IP, net.IPMask) {
	mask := net.CIDRMask(24, 32)
	base := net.ParseIP("192.168.100.0").To4()
	if host, _, err := net.SplitHostPort(target); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
			base = ip.To4().Mask(mask)
		}
	}
	return base, mask
}

func (d *daemon) resetArrays() {
	for i := range d.arrays {
		d.arrays[i] = make([]*float64, ringSize)
	}
}
