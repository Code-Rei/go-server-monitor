package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

// ─────────────────────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────────────────────

// SQLMemoryMetrics holds all SQL Server memory & perf counters we expose.
type SQLMemoryMetrics struct {
	Connected           bool     `json:"connected"`
	Configured          bool     `json:"configured"`
	CurrentCommittedMB  int64    `json:"currentCommittedMB"`
	TargetCommittedMB   int64    `json:"targetCommittedMB"`
	BufferPoolMB        int64    `json:"bufferPoolMB"`
	PlanCacheMB         int64    `json:"planCacheMB"`
	OtherMemoryMB       int64    `json:"otherMemoryMB"`
	PageLifeExpectancy  int64    `json:"pageLifeExpectancy"`
	ActiveConnections   int64    `json:"activeConnections"`
	BatchRequestsPerSec float64  `json:"batchRequestsPerSec"`
	BufferPoolPct       float64  `json:"bufferPoolPct"`
	CommittedPct        float64  `json:"committedPct"`
	PLEStatus           string   `json:"pleStatus"`
	// Extended metrics
	TotalPhysicalMB     int64    `json:"totalPhysicalMB"`
	AvailablePhysicalMB int64    `json:"availablePhysicalMB"`
	SQLSystemPct        float64  `json:"sqlSystemPct"`
	CacheHitRatio       float64  `json:"cacheHitRatio"`
	LazyWritesPerSec    float64  `json:"lazyWritesPerSec"`
	CompilationsPerSec  float64  `json:"compilationsPerSec"`
	DeadlocksPerSec     float64  `json:"deadlocksPerSec"`
	LockWaitsPerSec     float64  `json:"lockWaitsPerSec"`
	// Health
	HealthScore         int      `json:"healthScore"`
	HealthStatus        string   `json:"healthStatus"`
	CrashRisk           string   `json:"crashRisk"`
	RiskFactors         []string `json:"riskFactors"`
	Error               string   `json:"error,omitempty"`
	Timestamp           string   `json:"timestamp"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Package-level state
// ─────────────────────────────────────────────────────────────────────────────

var (
	sqlDB      *sql.DB
	sqlEnabled bool

	sqlBatchMu      sync.Mutex
	prevBatchCount  int64
	prevBatchAt     time.Time
	batchDeltaReady bool

	// Extended delta counters
	prevLazyWrites  int64
	prevCompilations int64
	prevDeadlocks   int64
	prevLockWaits   int64
	extDeltaReady   bool
)

// ─────────────────────────────────────────────────────────────────────────────
// initSQL — called once from main()
// ─────────────────────────────────────────────────────────────────────────────

// initSQL opens a SQL Server connection from environment variables.
// If no SQL Server env vars are present it silently marks sqlEnabled=false.
//
// Supported env vars:
//   SQLSERVER_DSN  — full ADO.NET-style DSN (takes priority)
//   SQL_HOST       — hostname or IP, supports "host,port" notation (default: localhost)
//   SQL_PORT       — port override (default: 1433)
//   SQL_USER       — login name (omit for Windows Integrated Auth)
//   SQL_PASS       — password  (omit for Windows Integrated Auth)
//   SQL_DB         — database name (default: master)
//   SQL_INSTANCE   — named instance (optional)
func initSQL() {
	dsn := os.Getenv("SQLSERVER_DSN")

	if dsn == "" {
		host := os.Getenv("SQL_HOST")
		user := os.Getenv("SQL_USER")
		pass := os.Getenv("SQL_PASS")

		// Need at least SQL_HOST to enable SQL monitoring.
		if host == "" && user == "" {
			log.Println("SQL Server: no credentials configured — SQL panel disabled")
			return
		}

		if host == "" {
			host = "localhost"
		}

		// Support SQL Server "host,port" notation (e.g. 192.168.1.1,64597)
		port := os.Getenv("SQL_PORT")
		if strings.Contains(host, ",") {
			parts := strings.SplitN(host, ",", 2)
			host = parts[0]
			if port == "" {
				port = strings.TrimSpace(parts[1])
			}
		}
		if port == "" {
			port = "1433"
		}

		dbName := os.Getenv("SQL_DB")
		if dbName == "" {
			dbName = "master"
		}
		instance := os.Getenv("SQL_INSTANCE")

		q := url.Values{}
		q.Set("database", dbName)
		// Match SSMS "Trust Server Certificate" — required for self-signed certs
		// which are the default on most SQL Server installations.
		q.Set("TrustServerCertificate", "true")
		q.Set("encrypt", "true")
		if instance != "" {
			q.Set("instance", instance)
		}

		if user == "" {
			// Windows Integrated Authentication — no credentials needed.
			// The process runs as the current Windows user; SQL Server trusts it.
			q.Set("trusted_connection", "yes")
			log.Println("SQL Server: using Windows Integrated Authentication")
			dsn = fmt.Sprintf("sqlserver://%s:%s?%s", host, port, q.Encode())
		} else {
			// SQL Server login with username + password.
			// QueryEscape handles special chars ($, #, @, %, etc.)
			dsn = fmt.Sprintf("sqlserver://%s:%s@%s:%s?%s",
				url.QueryEscape(user),
				url.QueryEscape(pass),
				host, port,
				q.Encode(),
			)
		}
	}

	// Log target server (mask password) to help diagnose wrong host/port/user issues
	log.Printf("SQL Server: dialing → %s", maskDSN(dsn))

	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		log.Printf("SQL Server: failed to open driver: %v", err)
		return
	}

	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		log.Printf("SQL Server: ping failed — %v (will retry on each poll)", err)
		// Still keep the db handle so we retry on every metric fetch.
	} else {
		log.Println("SQL Server: connected ✓")
	}

	sqlDB = db
	sqlEnabled = true

	// Seed batch-requests baseline so the first reading is valid.
	if count, ok := readBatchCount(db); ok {
		sqlBatchMu.Lock()
		prevBatchCount = count
		prevBatchAt = time.Now()
		batchDeltaReady = true
		sqlBatchMu.Unlock()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// fetchSQLMetrics — called on every /metrics/sql request
// ─────────────────────────────────────────────────────────────────────────────

func fetchSQLMetrics() SQLMemoryMetrics {
	m := SQLMemoryMetrics{
		Configured: sqlEnabled,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}

	if !sqlEnabled || sqlDB == nil {
		return m
	}

	// All metrics in one single roundtrip using DMVs ──────────────────────────
	const query = `
SELECT
    (SELECT physical_memory_in_use_kb / 1024 FROM sys.dm_os_process_memory)                                                   AS CurrentCommittedMB,
    ISNULL((SELECT cntr_value / 1024 FROM sys.dm_os_performance_counters
             WHERE counter_name = 'Target Server Memory (KB)' AND object_name LIKE '%Memory Manager%'), 0)                    AS TargetCommittedMB,
    ISNULL((SELECT SUM(pages_kb) / 1024 FROM sys.dm_os_memory_clerks WHERE type = 'MEMORYCLERK_SQLBUFFERPOOL'), 0)            AS BufferPoolMB,
    ISNULL((SELECT SUM(pages_kb) / 1024 FROM sys.dm_os_memory_clerks WHERE type IN ('CACHESTORE_SQLCP','CACHESTORE_OBJCP')),0) AS PlanCacheMB,
    ISNULL((SELECT TOP 1 cntr_value FROM sys.dm_os_performance_counters
             WHERE counter_name = 'Page life expectancy' AND object_name LIKE '%Buffer Manager%'), 0)                         AS PLE,
    (SELECT COUNT(*) FROM sys.dm_exec_sessions WHERE is_user_process = 1)                                                     AS ActiveConnections,
     ISNULL((SELECT total_physical_memory_kb     / 1024 FROM sys.dm_os_sys_memory), 0)                                         AS TotalPhysicalMB,
    ISNULL((SELECT available_physical_memory_kb / 1024 FROM sys.dm_os_sys_memory), 0)                                          AS AvailablePhysicalMB,
    ISNULL((SELECT cntr_value FROM sys.dm_os_performance_counters
             WHERE counter_name = 'Buffer cache hit ratio'      AND object_name LIKE '%Buffer Manager%'), 0)                  AS CacheHitNum,
    ISNULL((SELECT cntr_value FROM sys.dm_os_performance_counters
             WHERE counter_name = 'Buffer cache hit ratio base' AND object_name LIKE '%Buffer Manager%'), 100)                AS CacheHitBase,
    ISNULL((SELECT cntr_value FROM sys.dm_os_performance_counters
             WHERE counter_name = 'Lazy writes/sec'             AND object_name LIKE '%Buffer Manager%'), 0)                  AS LazyWritesCum,
    ISNULL((SELECT cntr_value FROM sys.dm_os_performance_counters
             WHERE counter_name = 'SQL Compilations/sec'        AND object_name LIKE '%SQL Statistics%'), 0)                  AS CompilationsCum,
    ISNULL((SELECT cntr_value FROM sys.dm_os_performance_counters
             WHERE counter_name = 'Number of Deadlocks/sec'     AND object_name LIKE '%Locks%' AND instance_name = '_Total'),0) AS DeadlocksCum,
    ISNULL((SELECT cntr_value FROM sys.dm_os_performance_counters
             WHERE counter_name = 'Lock Waits/sec'              AND object_name LIKE '%Locks%' AND instance_name = '_Total'),0) AS LockWaitsCum,
    ISNULL((SELECT TOP 1 cntr_value FROM sys.dm_os_performance_counters WHERE counter_name = 'Batch Requests/sec'), 0)        AS BatchCum`

	var (
		committed      int64
		target         int64
		bufPool        int64
		planCache      int64
		ple            int64
		activeConns    int64
		totalPhys      int64
		availPhys      int64
		cacheHitNum    int64
		cacheHitBase   int64
		lazyWritesCum  int64
		compilCum      int64
		deadlocksCum   int64
		lockWaitsCum   int64
		batchCum       int64
	)

	err := sqlDB.QueryRow(query).Scan(
		&committed, &target, &bufPool, &planCache, &ple, &activeConns,
		&totalPhys, &availPhys, &cacheHitNum, &cacheHitBase,
		&lazyWritesCum, &compilCum, &deadlocksCum, &lockWaitsCum, &batchCum,
	)
	if err != nil {
		m.Error = err.Error()
		m.Connected = false
		return m
	}

	m.Connected = true
	m.CurrentCommittedMB  = committed
	m.TargetCommittedMB   = target
	m.BufferPoolMB        = bufPool
	m.PlanCacheMB         = planCache
	m.PageLifeExpectancy  = ple
	m.ActiveConnections   = activeConns
	m.TotalPhysicalMB     = totalPhys
	m.AvailablePhysicalMB = availPhys

	// Derived: other memory
	if other := committed - bufPool - planCache; other > 0 {
		m.OtherMemoryMB = other
	}

	// Percentages
	if target > 0    { m.CommittedPct  = r2(float64(committed) / float64(target)    * 100) }
	if committed > 0 { m.BufferPoolPct = r2(float64(bufPool)   / float64(committed)  * 100) }
	if totalPhys > 0 { m.SQLSystemPct  = r2(float64(committed) / float64(totalPhys)  * 100) }

	// Cache hit ratio (ratio counter: numerator / base)
	if cacheHitBase > 0 {
		m.CacheHitRatio = r2(float64(cacheHitNum) / float64(cacheHitBase) * 100)
	} else {
		m.CacheHitRatio = 100
	}

	// PLE status
	switch {
	case ple >= 300: m.PLEStatus = "good"
	case ple >= 100: m.PLEStatus = "warn"
	default:         m.PLEStatus = "critical"
	}

	// Delta counters (all in one mutex section) ───────────────────────────────
	sqlBatchMu.Lock()
	now := time.Now()
	dt  := now.Sub(prevBatchAt).Seconds()
	if batchDeltaReady && dt > 0 {
		if batchCum >= prevBatchCount {
			m.BatchRequestsPerSec = r2(float64(batchCum-prevBatchCount) / dt)
		}
		if extDeltaReady {
			if lazyWritesCum  >= prevLazyWrites   { m.LazyWritesPerSec    = r2(float64(lazyWritesCum -prevLazyWrites)   / dt) }
			if compilCum      >= prevCompilations  { m.CompilationsPerSec  = r2(float64(compilCum    -prevCompilations)  / dt) }
			if deadlocksCum   >= prevDeadlocks     { m.DeadlocksPerSec     = r2(float64(deadlocksCum -prevDeadlocks)     / dt) }
			if lockWaitsCum   >= prevLockWaits     { m.LockWaitsPerSec     = r2(float64(lockWaitsCum -prevLockWaits)     / dt) }
		}
	}
	prevBatchCount   = batchCum
	prevLazyWrites   = lazyWritesCum
	prevCompilations = compilCum
	prevDeadlocks    = deadlocksCum
	prevLockWaits    = lockWaitsCum
	prevBatchAt      = now
	batchDeltaReady  = true
	extDeltaReady    = true
	sqlBatchMu.Unlock()

	// Health score & crash risk
	computeHealthScore(&m)

	return m
}

// computeHealthScore calculates a 0–100 health score and crash risk from the
// collected metrics. A lower score means higher risk of OOM / performance issues.
func computeHealthScore(m *SQLMemoryMetrics) {
	score := 100
	var factors []string

	// PLE — most important single indicator of buffer pool pressure
	switch {
	case m.PageLifeExpectancy < 100:
		score -= 40
		factors = append(factors, "🔴 PLE critically low (<100s) — buffer pool starved, expect I/O thrashing")
	case m.PageLifeExpectancy < 300:
		score -= 20
		factors = append(factors, "🟡 PLE below recommended threshold (300s+)")
	}

	// Cache hit ratio — should be >99% on a healthy server
	switch {
	case m.CacheHitRatio < 90:
		score -= 40
		factors = append(factors, "🔴 Buffer cache hit ratio critically low (<90%) — heavy disk reads")
	case m.CacheHitRatio < 95:
		score -= 25
		factors = append(factors, "🟡 Cache hit ratio degraded (<95%) — frequent disk reads")
	case m.CacheHitRatio < 99:
		score -= 10
		factors = append(factors, "🔵 Cache hit ratio below optimal (<99%)")
	}

	// Committed vs target — near-limit means SQL can't grow further
	if m.CommittedPct > 95 {
		score -= 35
		factors = append(factors, "🔴 SQL memory at >95% of target limit — cannot allocate more")
	} else if m.CommittedPct > 90 {
		score -= 20
		factors = append(factors, "🟡 SQL memory at >90% of configured target")
	}

	// Available physical RAM on the OS
	availGB := float64(m.AvailablePhysicalMB) / 1024
	switch {
	case m.AvailablePhysicalMB > 0 && availGB < 1:
		score -= 30
		factors = append(factors, "🔴 System free RAM critically low (<1 GB) — OOM risk")
	case m.AvailablePhysicalMB > 0 && availGB < 2:
		score -= 15
		factors = append(factors, "🟡 System free RAM low (<2 GB)")
	}

	// Lazy writes — indicates SQL is spilling pages to disk under memory pressure
	switch {
	case m.LazyWritesPerSec > 100:
		score -= 30
		factors = append(factors, "🔴 Very high lazy writes (>100/s) — severe memory pressure")
	case m.LazyWritesPerSec > 20:
		score -= 15
		factors = append(factors, "🟡 Elevated lazy writes (>20/s) — memory under pressure")
	}

	// Deadlocks — any deadlock is a concern
	if m.DeadlocksPerSec > 0 {
		score -= 20
		factors = append(factors, "🔴 Active deadlocks detected — check blocking queries")
	}

	// Lock waits
	if m.LockWaitsPerSec > 100 {
		score -= 15
		factors = append(factors, "🟡 High lock wait rate (>100/s) — contention detected")
	}

	if score < 0 { score = 0 }
	m.HealthScore = score

	switch {
	case score >= 80:
		m.HealthStatus = "healthy"
		m.CrashRisk    = "Low"
	case score >= 60:
		m.HealthStatus = "warning"
		m.CrashRisk    = "Medium"
	case score >= 40:
		m.HealthStatus = "at-risk"
		m.CrashRisk    = "High"
	default:
		m.HealthStatus = "critical"
		m.CrashRisk    = "Critical"
	}

	if len(factors) == 0 {
		factors = []string{"✅ All metrics within healthy thresholds"}
	}
	m.RiskFactors = factors
}

// readBatchCount — kept for initSQL seeding only.
func readBatchCount(db *sql.DB) (int64, bool) {
	var v int64
	err := db.QueryRow(`
		SELECT TOP 1 cntr_value
		  FROM sys.dm_os_performance_counters
		 WHERE counter_name = 'Batch Requests/sec'`).Scan(&v)
	if err != nil {
		return 0, false
	}
	return v, true
}

// maskDSN returns the DSN with the password replaced by ***  for safe logging.
func maskDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "(unparseable DSN)"
	}
	if u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			u.User = url.UserPassword(u.User.Username(), "***")
		}
	}
	return u.String()
}
