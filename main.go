package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/http"
	"runtime"
	"sync"
	"time"

	cpu_pkg "github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	net_pkg "github.com/shirou/gopsutil/v4/net"
)

//go:embed static
var staticFS embed.FS

const listenAddr = ":8266"

// ─────────────────────────────────────────────────────────────────────────────
// Metric structs
// ─────────────────────────────────────────────────────────────────────────────

// CompactMetrics mirrors the original JSON format exactly.
type CompactMetrics struct {
	CPU    float64  `json:"cpu"`
	RAM    float64  `json:"ram"`
	Load   float64  `json:"load"`
	Temp   *float64 `json:"temp"`
	RX     int64    `json:"rx"`
	TX     int64    `json:"tx"`
	Uptime uint64   `json:"uptime"`
}

type MemDetail struct {
	Used    float64 `json:"used"`
	Total   float64 `json:"total"`
	Free    float64 `json:"free"`
	Percent float64 `json:"percent"`
}

type SwapDetail struct {
	Used    float64 `json:"used"`
	Total   float64 `json:"total"`
	Percent float64 `json:"percent"`
}

type DiskDetail struct {
	Used    float64 `json:"used"`
	Total   float64 `json:"total"`
	Free    float64 `json:"free"`
	Percent float64 `json:"percent"`
}

type NetworkDetail struct {
	RxBytes     int64  `json:"rxBytes"`
	TxBytes     int64  `json:"txBytes"`
	RxFormatted string `json:"rxFormatted"`
	TxFormatted string `json:"txFormatted"`
	Interface   string `json:"interface"`
}

type CoreLoad struct {
	Core int     `json:"core"`
	Load float64 `json:"load"`
}

type CPUInfoDetail struct {
	Model     string     `json:"model"`
	Cores     int32      `json:"cores"`
	Threads   int        `json:"threads"`
	Speed     float64    `json:"speed"`
	CoreLoads []CoreLoad `json:"coreLoads"`
}

type SystemDetail struct {
	Hostname string `json:"hostname"`
	Platform string `json:"platform"`
	Distro   string `json:"distro"`
	Release  string `json:"release"`
	Arch     string `json:"arch"`
}

type FullMetrics struct {
	// Compact backward-compatible fields
	CPU    float64  `json:"cpu"`
	RAM    float64  `json:"ram"`
	Load   float64  `json:"load"`
	Temp   *float64 `json:"temp"`
	RX     int64    `json:"rx"`
	TX     int64    `json:"tx"`
	Uptime uint64   `json:"uptime"`

	// Extended fields
	Memory    MemDetail     `json:"memory"`
	Swap      SwapDetail    `json:"swap"`
	Disk      DiskDetail    `json:"disk"`
	Network   NetworkDetail `json:"network"`
	CPUInfo   CPUInfoDetail `json:"cpuInfo"`
	System    SystemDetail  `json:"system"`
	Processes int           `json:"processes"`
	Timestamp string        `json:"timestamp"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Network bytes-per-second tracker
// ─────────────────────────────────────────────────────────────────────────────

var (
	netMu     sync.Mutex
	prevRX    uint64
	prevTX    uint64
	prevNetAt time.Time
	netIface  string
)

// initNetwork seeds the previous-reading so the first call returns 0 instead of a spike.
func initNetwork() {
	stats, err := net_pkg.IOCounters(false) // aggregate
	if err != nil || len(stats) == 0 {
		return
	}
	netMu.Lock()
	prevRX = stats[0].BytesRecv
	prevTX = stats[0].BytesSent
	prevNetAt = time.Now()
	netMu.Unlock()

	// Pick the first interface that has traffic for display name
	ifaces, _ := net_pkg.IOCounters(true)
	for _, ifc := range ifaces {
		if ifc.BytesRecv > 0 || ifc.BytesSent > 0 {
			netIface = ifc.Name
			break
		}
	}
	if netIface == "" && len(ifaces) > 0 {
		netIface = ifaces[0].Name
	}
}

// networkBytesPerSec returns RX and TX in bytes/sec since last call.
func networkBytesPerSec() (rx, tx int64, iface string) {
	stats, err := net_pkg.IOCounters(false)
	if err != nil || len(stats) == 0 {
		return 0, 0, "N/A"
	}
	now := time.Now()
	curRX := stats[0].BytesRecv
	curTX := stats[0].BytesSent

	netMu.Lock()
	defer netMu.Unlock()

	dt := now.Sub(prevNetAt).Seconds()
	if dt > 0 {
		var rxDiff, txDiff uint64
		if curRX >= prevRX {
			rxDiff = curRX - prevRX
		}
		if curTX >= prevTX {
			txDiff = curTX - prevTX
		}
		rx = int64(math.Round(float64(rxDiff) / dt))
		tx = int64(math.Round(float64(txDiff) / dt))
	}

	prevRX = curRX
	prevTX = curTX
	prevNetAt = now
	iface = netIface
	return
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func r1(v float64) float64 { return math.Round(v*10) / 10 }
func r2(v float64) float64 { return math.Round(v*100) / 100 }

func toGB(bytes uint64) float64 { return r2(float64(bytes) / 1073741824) }

func fmtBps(b int64) string {
	switch {
	case b <= 0:
		return "0 B/s"
	case b < 1024:
		return fmt.Sprintf("%d B/s", b)
	case b < 1048576:
		return fmt.Sprintf("%.1f KB/s", float64(b)/1024)
	case b < 1073741824:
		return fmt.Sprintf("%.1f MB/s", float64(b)/1048576)
	default:
		return fmt.Sprintf("%.2f GB/s", float64(b)/1073741824)
	}
}

// getTemperature is implemented per-platform in temperature_windows.go / temperature_other.go

// ─────────────────────────────────────────────────────────────────────────────
// Metric collection
// ─────────────────────────────────────────────────────────────────────────────

type rawData struct {
	cpuPct   []float64
	cpuInfos []cpu_pkg.InfoStat
	vmStat   *mem.VirtualMemoryStat
	swStat   *mem.SwapMemoryStat
	diskStat *disk.UsageStat
	hostStat *host.InfoStat
	loadStat *load.AvgStat
}

func gatherRaw() *rawData {
	var wg sync.WaitGroup
	d := &rawData{}

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Per-core CPU with 200 ms sample — also gives us overall via average
		pcts, err := cpu_pkg.Percent(200*time.Millisecond, true)
		if err == nil {
			d.cpuPct = pcts
		}
		infos, _ := cpu_pkg.Info()
		d.cpuInfos = infos
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.vmStat, _ = mem.VirtualMemory()
		d.swStat, _ = mem.SwapMemory()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		mount := "/"
		if runtime.GOOS == "windows" {
			mount = "C:\\"
		}
		d.diskStat, _ = disk.Usage(mount)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.hostStat, _ = host.Info()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.loadStat, _ = load.Avg()
	}()



	wg.Wait()
	return d
}

func collectMetrics() *FullMetrics {
	raw := gatherRaw()

	// ── CPU ─────────────────────────────────────────────────────────────────
	var cpuOverall float64
	coreLoads := make([]CoreLoad, len(raw.cpuPct))
	for i, v := range raw.cpuPct {
		cpuOverall += v
		coreLoads[i] = CoreLoad{Core: i, Load: r1(v)}
	}
	if len(raw.cpuPct) > 0 {
		cpuOverall /= float64(len(raw.cpuPct))
	}

	var cpuModel string
	var cpuCores int32
	var cpuSpeed float64 // GHz
	if len(raw.cpuInfos) > 0 {
		cpuModel = raw.cpuInfos[0].ModelName
		cpuCores = raw.cpuInfos[0].Cores
		cpuSpeed = r2(raw.cpuInfos[0].Mhz / 1000)
	}
	cpuThreads := len(raw.cpuPct)
	if cpuThreads == 0 {
		cpuThreads, _ = cpu_pkg.Counts(true)
	}

	// ── Memory ──────────────────────────────────────────────────────────────
	var ramPct, ramUsed, ramTotal, ramFree float64
	if raw.vmStat != nil {
		ramPct = r1(raw.vmStat.UsedPercent)
		// On Windows, Active is more accurate than Used
		used := raw.vmStat.Used
		if raw.vmStat.Active > 0 {
			used = raw.vmStat.Active
		}
		ramUsed = toGB(used)
		ramTotal = toGB(raw.vmStat.Total)
		ramFree = toGB(raw.vmStat.Available)
		if ramPct == 0 && raw.vmStat.Total > 0 {
			ramPct = r1(float64(used) / float64(raw.vmStat.Total) * 100)
		}
	}

	var swapPct, swapUsed, swapTotal float64
	if raw.swStat != nil {
		swapPct = r1(raw.swStat.UsedPercent)
		swapUsed = toGB(raw.swStat.Used)
		swapTotal = toGB(raw.swStat.Total)
	}

	// ── Disk ────────────────────────────────────────────────────────────────
	var diskPct, diskUsed, diskTotal, diskFree float64
	if raw.diskStat != nil {
		diskPct = r1(raw.diskStat.UsedPercent)
		diskUsed = toGB(raw.diskStat.Used)
		diskTotal = toGB(raw.diskStat.Total)
		diskFree = toGB(raw.diskStat.Free)
	}

	// ── Load average ────────────────────────────────────────────────────────
	var loadAvg float64
	if raw.loadStat != nil {
		loadAvg = r2(raw.loadStat.Load1)
	}

	// ── Network ─────────────────────────────────────────────────────────────
	rxBps, txBps, iface := networkBytesPerSec()

	// ── Temperature (platform-specific: WMI on Windows, nil on Linux) ───────
	tempPtr := getTemperature()

	// ── Host ────────────────────────────────────────────────────────────────
	var hostname, platform, distro, release, arch string
	var uptime uint64
	var procs int
	if raw.hostStat != nil {
		hostname = raw.hostStat.Hostname
		platform = raw.hostStat.Platform
		distro = raw.hostStat.PlatformFamily
		release = raw.hostStat.PlatformVersion
		arch = raw.hostStat.KernelArch
		uptime = raw.hostStat.Uptime
		procs = int(raw.hostStat.Procs)
	}
	if arch == "" {
		arch = runtime.GOARCH
	}

	return &FullMetrics{
		CPU:    r1(cpuOverall),
		RAM:    ramPct,
		Load:   loadAvg,
		Temp:   tempPtr,
		RX:     rxBps,
		TX:     txBps,
		Uptime: uptime,

		Memory: MemDetail{
			Used: ramUsed, Total: ramTotal,
			Free: ramFree, Percent: ramPct,
		},
		Swap: SwapDetail{
			Used: swapUsed, Total: swapTotal, Percent: swapPct,
		},
		Disk: DiskDetail{
			Used: diskUsed, Total: diskTotal,
			Free: diskFree, Percent: diskPct,
		},
		Network: NetworkDetail{
			RxBytes:     rxBps,
			TxBytes:     txBps,
			RxFormatted: fmtBps(rxBps),
			TxFormatted: fmtBps(txBps),
			Interface:   iface,
		},
		CPUInfo: CPUInfoDetail{
			Model: cpuModel, Cores: cpuCores,
			Threads: cpuThreads, Speed: cpuSpeed,
			CoreLoads: coreLoads,
		},
		System: SystemDetail{
			Hostname: hostname, Platform: platform,
			Distro: distro, Release: release, Arch: arch,
		},
		Processes: procs,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP handlers
// ─────────────────────────────────────────────────────────────────────────────

func jsonHandler(w http.ResponseWriter, r *http.Request, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Main
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	// Seed network baseline
	initNetwork()

	// Connect to SQL Server (no-op if env vars not set)
	initSQL()

	// Serve embedded static files at /
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("failed to sub static fs: %v", err)
	}

	mux := http.NewServeMux()

	// GET /metrics  →  compact JSON (original format)
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		m := collectMetrics()
		jsonHandler(w, r, CompactMetrics{
			CPU: m.CPU, RAM: m.RAM, Load: m.Load,
			Temp: m.Temp, RX: m.RX, TX: m.TX, Uptime: m.Uptime,
		})
	})

	// GET /metrics/full  →  all metrics
	mux.HandleFunc("/metrics/full", func(w http.ResponseWriter, r *http.Request) {
		m := collectMetrics()
		jsonHandler(w, r, m)
	})

	// GET /metrics/sql  →  SQL Server memory & perf metrics
	mux.HandleFunc("/metrics/sql", func(w http.ResponseWriter, r *http.Request) {
		jsonHandler(w, r, fetchSQLMetrics())
	})

	// GET /  →  dashboard (embedded static files)
	mux.Handle("/", http.FileServer(http.FS(staticSub)))

	log.Printf("\n🖥️  Server Monitor (Go) running")
	log.Printf("   ➜  Dashboard : http://localhost%s/", listenAddr)
	log.Printf("   ➜  Metrics   : http://localhost%s/metrics", listenAddr)
	log.Printf("   ➜  Full      : http://localhost%s/metrics/full", listenAddr)
	log.Printf("   ➜  SQL       : http://localhost%s/metrics/sql\n", listenAddr)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
