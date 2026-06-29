package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

// ─────────────────────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────────────────────

// SQLMemoryMetrics holds all SQL Server memory & perf counters we expose.
type SQLMemoryMetrics struct {
	Connected           bool    `json:"connected"`
	Configured          bool    `json:"configured"`
	CurrentCommittedMB  int64   `json:"currentCommittedMB"`
	TargetCommittedMB   int64   `json:"targetCommittedMB"`
	BufferPoolMB        int64   `json:"bufferPoolMB"`
	PlanCacheMB         int64   `json:"planCacheMB"`
	OtherMemoryMB       int64   `json:"otherMemoryMB"`
	PageLifeExpectancy  int64   `json:"pageLifeExpectancy"`
	ActiveConnections   int64   `json:"activeConnections"`
	BatchRequestsPerSec float64 `json:"batchRequestsPerSec"`
	BufferPoolPct       float64 `json:"bufferPoolPct"`  // buffer pool % of committed
	CommittedPct        float64 `json:"committedPct"`   // committed % of target
	PLEStatus           string  `json:"pleStatus"`      // "good" | "warn" | "critical"
	Error               string  `json:"error,omitempty"`
	Timestamp           string  `json:"timestamp"`
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
)

// ─────────────────────────────────────────────────────────────────────────────
// initSQL — called once from main()
// ─────────────────────────────────────────────────────────────────────────────

// initSQL opens a SQL Server connection from environment variables.
// If no SQL Server env vars are present it silently marks sqlEnabled=false.
//
// Supported env vars:
//   SQLSERVER_DSN  — full ADO.NET-style DSN (takes priority)
//   SQL_HOST       — hostname or IP (default: localhost)
//   SQL_PORT       — port (default: 1433)
//   SQL_USER       — login name
//   SQL_PASS       — password
//   SQL_DB         — database name (default: master)
//   SQL_INSTANCE   — named instance (optional)
func initSQL() {
	dsn := os.Getenv("SQLSERVER_DSN")

	if dsn == "" {
		host := os.Getenv("SQL_HOST")
		user := os.Getenv("SQL_USER")
		pass := os.Getenv("SQL_PASS")

		// If none of the individual vars are set either, skip SQL entirely.
		if host == "" && user == "" {
			log.Println("SQL Server: no credentials configured — SQL panel disabled")
			return
		}

		if host == "" {
			host = "localhost"
		}
		port := os.Getenv("SQL_PORT")
		if port == "" {
			port = "1433"
		}
		db := os.Getenv("SQL_DB")
		if db == "" {
			db = "master"
		}
		instance := os.Getenv("SQL_INSTANCE")

		if instance != "" {
			dsn = fmt.Sprintf("sqlserver://%s:%s@%s\\%s:%s?database=%s",
				user, pass, host, instance, port, db)
		} else {
			dsn = fmt.Sprintf("sqlserver://%s:%s@%s:%s?database=%s",
				user, pass, host, port, db)
		}
	}

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

	// Single-roundtrip DMV query ──────────────────────────────────────────────
	const query = `
SELECT
    (SELECT physical_memory_in_use_kb / 1024 FROM sys.dm_os_process_memory) AS CurrentCommittedMB,
    ISNULL((SELECT cntr_value / 1024
       FROM sys.dm_os_performance_counters
      WHERE counter_name = 'Target Server Memory (KB)'
        AND object_name  LIKE '%Memory Manager%'), 0) AS TargetCommittedMB,
    ISNULL((SELECT SUM(pages_kb) / 1024
       FROM sys.dm_os_memory_clerks
      WHERE type = 'MEMORYCLERK_SQLBUFFERPOOL'), 0) AS BufferPoolMB,
    ISNULL((SELECT SUM(pages_kb) / 1024
       FROM sys.dm_os_memory_clerks
      WHERE type IN ('CACHESTORE_SQLCP', 'CACHESTORE_OBJCP')), 0) AS PlanCacheMB,
    ISNULL((SELECT TOP 1 cntr_value
       FROM sys.dm_os_performance_counters
      WHERE counter_name = 'Page life expectancy'
        AND object_name  LIKE '%Buffer Manager%'), 0) AS PLE,
    (SELECT COUNT(*)
       FROM sys.dm_exec_sessions
      WHERE is_user_process = 1) AS ActiveConnections`

	var (
		committed   int64
		target      int64
		bufPool     int64
		planCache   int64
		ple         int64
		activeConns int64
	)

	err := sqlDB.QueryRow(query).Scan(&committed, &target, &bufPool, &planCache, &ple, &activeConns)
	if err != nil {
		m.Error = err.Error()
		m.Connected = false
		return m
	}

	m.Connected = true
	m.CurrentCommittedMB = committed
	m.TargetCommittedMB = target
	m.BufferPoolMB = bufPool
	m.PlanCacheMB = planCache
	m.PageLifeExpectancy = ple
	m.ActiveConnections = activeConns

	// Derived: other memory (committed minus known categories)
	other := committed - bufPool - planCache
	if other < 0 {
		other = 0
	}
	m.OtherMemoryMB = other

	// Percentages
	if target > 0 {
		m.CommittedPct = r2(float64(committed) / float64(target) * 100)
	}
	if committed > 0 {
		m.BufferPoolPct = r2(float64(bufPool) / float64(committed) * 100)
	}

	// PLE health status
	switch {
	case ple >= 300:
		m.PLEStatus = "good"
	case ple >= 100:
		m.PLEStatus = "warn"
	default:
		m.PLEStatus = "critical"
	}

	// Batch requests/sec (delta from last call) ───────────────────────────────
	if curBatch, ok := readBatchCount(sqlDB); ok {
		sqlBatchMu.Lock()
		now := time.Now()
		if batchDeltaReady {
			dt := now.Sub(prevBatchAt).Seconds()
			if dt > 0 && curBatch >= prevBatchCount {
				m.BatchRequestsPerSec = r2(float64(curBatch-prevBatchCount) / dt)
			}
		}
		prevBatchCount = curBatch
		prevBatchAt = now
		batchDeltaReady = true
		sqlBatchMu.Unlock()
	}

	return m
}

// readBatchCount reads the cumulative batch-requests counter from dm_os_performance_counters.
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
