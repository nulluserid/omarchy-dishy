package main

import (
	"time"

	api "starlinkd/gen/spacex_api/device/device"
	dish "starlinkd/gen/spacex_api/device/dish"
)

// Ring and cadence constants from CONTRACT.md.
const (
	ringSize        = 900 // history samples, 1s apart
	sampleIntervalS = 1
	rpcTimeout      = 3 * time.Second // per RPC; dish may vanish mid-tick
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
	target      string
	interval    time.Duration
	mapInterval time.Duration
	once        bool
	verbose     bool
	dir         string // state dir, ~/.local/state/starlinkd
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

	daily         dailyFile
	sessionTicks  int
	sessionOnline int

	prevState  string
	prevAlerts map[string]bool
	warm       bool // first good tick observed (no events/notifications)
	hyst       hyst

	openOutage         *outageTrack
	failed             int
	maxDishOutageStart int64

	mapData *obsMap
	lastMap time.Time

	lastStatus *dish.DishGetStatusResponse // last good status, for dish info on failures
	lastObs    obsStats
	lastFile   *statusFile // printed by -once
	lastErr    string      // for rate-limited error logging
}

func newDaemon(cfg config, client api.DeviceClient) *daemon {
	d := &daemon{cfg: cfg, client: client, hyst: newHyst(60, 300), startWall: time.Now()}
	d.resetArrays()
	return d
}

func (d *daemon) resetArrays() {
	for i := range d.arrays {
		d.arrays[i] = make([]*float64, ringSize)
	}
}
