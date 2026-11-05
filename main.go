package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var staticFS embed.FS

// --- Cgroup and System Limits ---

type SystemLimits struct {
	CgroupVersion string  `json:"cgroup_version"`
	CPULimit      float64 `json:"cpu_limit"`       // Cores / vCPUs
	MemoryLimit   uint64  `json:"memory_limit_bytes"` // In bytes
	HostCPUs      int     `json:"host_cpus"`
	HostMemory    uint64  `json:"host_memory_bytes"`
}

func detectLimits() SystemLimits {
	limits := SystemLimits{
		CgroupVersion: "none",
		HostCPUs:      runtime.NumCPU(),
	}

	// Read host total memory from /proc/meminfo
	limits.HostMemory = readMemInfoTotal()

	// 1. Detect Cgroup v2
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		limits.CgroupVersion = "v2"

		// CPU limit in v2: /sys/fs/cgroup/cpu.max ("quota period", e.g. "50000 100000")
		if data, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
			parts := strings.Fields(string(data))
			if len(parts) >= 2 && parts[0] != "max" {
				quota, err1 := strconv.ParseFloat(parts[0], 64)
				period, err2 := strconv.ParseFloat(parts[1], 64)
				if err1 == nil && err2 == nil && period > 0 {
					limits.CPULimit = quota / period
				}
			}
		}

		// Memory limit in v2: /sys/fs/cgroup/memory.max
		if data, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
			str := strings.TrimSpace(string(data))
			if str != "max" {
				if val, err := strconv.ParseUint(str, 10, 64); err == nil {
					limits.MemoryLimit = val
				}
			}
		}
	} else {
		// 2. Detect Cgroup v1
		cgroupV1 := false

		// CPU limit in v1: /sys/fs/cgroup/cpu/cpu.cfs_quota_us & cpu.cfs_period_us
		quotaData, errQ := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
		periodData, errP := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
		if errQ == nil && errP == nil {
			cgroupV1 = true
			quota, err1 := strconv.ParseFloat(strings.TrimSpace(string(quotaData)), 64)
			period, err2 := strconv.ParseFloat(strings.TrimSpace(string(periodData)), 64)
			if err1 == nil && err2 == nil && quota > 0 && period > 0 {
				limits.CPULimit = quota / period
			}
		}

		// Memory limit in v1: /sys/fs/cgroup/memory/memory.limit_in_bytes
		memData, errM := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
		if errM == nil {
			cgroupV1 = true
			if val, err := strconv.ParseUint(strings.TrimSpace(string(memData)), 10, 64); err == nil {
				// Cgroup v1 sets a huge number (like 0x7FFFFFFFFFFFF000) when unlimited
				if val < 9000000000000000000 {
					limits.MemoryLimit = val
				}
			}
		}

		if cgroupV1 {
			limits.CgroupVersion = "v1"
		}
	}

	// Fallback if unlimited or not set
	if limits.CPULimit <= 0 {
		limits.CPULimit = float64(limits.HostCPUs)
	}
	if limits.MemoryLimit == 0 {
		limits.MemoryLimit = limits.HostMemory
	}

	return limits
}

func readMemInfoTotal() uint64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 1024 * 1024 * 1024 // 1GB default fallback
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseUint(fields[1], 10, 64)
				if err == nil {
					return kb * 1024
				}
			}
		}
	}
	return 1024 * 1024 * 1024
}

// --- Live Process & Container Metrics ---

type ProcessMetrics struct {
	CPUUsagePercent    float64 `json:"cpu_usage_percent"`
	MemoryRSSBytes     uint64  `json:"memory_rss_bytes"`
	MemoryRSSMB        float64 `json:"memory_rss_mb"`
	MemoryUsagePercent float64 `json:"memory_usage_percent"`
}

type MetricsCollector struct {
	mu           sync.Mutex
	lastCPUTime  int64
	lastReadTime time.Time
	currentCPU   float64
	limits       SystemLimits
}

func newMetricsCollector(limits SystemLimits) *MetricsCollector {
	mc := &MetricsCollector{
		limits:       limits,
		lastReadTime: time.Now(),
		lastCPUTime:  readProcessCPUTime(),
	}
	go mc.monitorLoop()
	return mc
}

func readProcessCPUTime() int64 {
	// Read /proc/self/stat utime + stime (clock ticks)
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) > 14 {
		utime, _ := strconv.ParseInt(fields[13], 10, 64)
		stime, _ := strconv.ParseInt(fields[14], 10, 64)
		return utime + stime
	}
	return 0
}

func (mc *MetricsCollector) monitorLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		cpuTime := readProcessCPUTime()

		mc.mu.Lock()
		duration := now.Sub(mc.lastReadTime).Seconds()
		if duration > 0 {
			// clock ticks in Linux is usually 100 Hz (USER_HZ)
			deltaTicks := float64(cpuTime - mc.lastCPUTime)
			ticksPerSec := 100.0
			deltaSeconds := deltaTicks / ticksPerSec

			// CPU % relative to container limit
			if mc.limits.CPULimit > 0 {
				mc.currentCPU = math.Min(100.0, math.Max(0.0, (deltaSeconds/duration)/mc.limits.CPULimit*100.0))
			} else {
				mc.currentCPU = 0.0
			}
		}
		mc.lastCPUTime = cpuTime
		mc.lastReadTime = now
		mc.mu.Unlock()
	}
}

func (mc *MetricsCollector) GetMetrics() ProcessMetrics {
	mc.mu.Lock()
	cpuPct := mc.currentCPU
	mc.mu.Unlock()

	rss := getWorkingSetMemory()
	rssMB := float64(rss) / (1024 * 1024)

	memPct := 0.0
	if mc.limits.MemoryLimit > 0 {
		memPct = math.Min(100.0, (float64(rss)/float64(mc.limits.MemoryLimit))*100.0)
	}

	return ProcessMetrics{
		CPUUsagePercent:    math.Round(cpuPct*10) / 10,
		MemoryRSSBytes:     rss,
		MemoryRSSMB:        math.Round(rssMB*10) / 10,
		MemoryUsagePercent: math.Round(memPct*10) / 10,
	}
}

func getWorkingSetMemory() uint64 {
	// First check cgroup memory current (reflects container working set)
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.current"); err == nil {
		if val, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil && val > 0 {
			return val
		}
	}
	if data, err := os.ReadFile("/sys/fs/cgroup/memory/memory.usage_in_bytes"); err == nil {
		if val, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil && val > 0 {
			return val
		}
	}

	// Fallback to /proc/self/statm (pages * 4096)
	if data, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 2 {
			pages, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil {
				return pages * 4096
			}
		}
	}

	// Fallback to Go runtime stats
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Sys
}

// --- Load Controller ---

type LoadController struct {
	mu            sync.Mutex
	limits        SystemLimits
	collector     *MetricsCollector
	isActive      bool
	cancelCPU     context.CancelFunc
	memChunks     [][]byte
	targetCPUPct  float64
	targetMemPct  float64
	targetMemMB   int
	targetDuration int
	timerEnd      time.Time
}

func newLoadController(limits SystemLimits, collector *MetricsCollector) *LoadController {
	return &LoadController{
		limits:    limits,
		collector: collector,
	}
}

type LoadRequest struct {
	CPUPercent      float64 `json:"cpu_percent"`
	CPUCores        float64 `json:"cpu_cores"`
	MemoryPercent   float64 `json:"memory_percent"`
	MemoryMB        int     `json:"memory_mb"`
	DurationSeconds int     `json:"duration_seconds"`
}

type StatusResponse struct {
	Limits           SystemLimits   `json:"limits"`
	Metrics          ProcessMetrics `json:"metrics"`
	IsActive         bool           `json:"is_active"`
	TargetCPUPercent float64        `json:"target_cpu_percent"`
	TargetMemPercent float64        `json:"target_memory_percent"`
	TargetMemMB      int            `json:"target_memory_mb"`
	DurationSeconds  int            `json:"duration_seconds"`
	RemainingSeconds int            `json:"remaining_seconds"`
}

func (lc *LoadController) ApplyLoad(req LoadRequest) error {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	// Stop any existing CPU workers
	if lc.cancelCPU != nil {
		lc.cancelCPU()
		lc.cancelCPU = nil
	}

	// Determine CPU target
	cpuPct := req.CPUPercent
	if req.CPUCores > 0 && lc.limits.CPULimit > 0 {
		cpuPct = (req.CPUCores / lc.limits.CPULimit) * 100.0
	}
	if cpuPct < 0 {
		cpuPct = 0
	}
	if cpuPct > 100 {
		cpuPct = 100
	}
	lc.targetCPUPct = cpuPct

	// Determine Memory target in Bytes
	var targetBytes uint64
	if req.MemoryMB > 0 {
		targetBytes = uint64(req.MemoryMB) * 1024 * 1024
		if lc.limits.MemoryLimit > 0 {
			lc.targetMemPct = (float64(targetBytes) / float64(lc.limits.MemoryLimit)) * 100.0
		}
		lc.targetMemMB = req.MemoryMB
	} else if req.MemoryPercent > 0 {
		if req.MemoryPercent > 100 {
			req.MemoryPercent = 100
		}
		lc.targetMemPct = req.MemoryPercent
		targetBytes = uint64(float64(lc.limits.MemoryLimit) * (req.MemoryPercent / 100.0))
		lc.targetMemMB = int(targetBytes / (1024 * 1024))
	} else {
		lc.targetMemPct = 0
		lc.targetMemMB = 0
		targetBytes = 0
	}

	// Allocate/Free Memory
	lc.adjustMemory(targetBytes)

	// Start CPU Workers if CPU > 0
	if cpuPct > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		lc.cancelCPU = cancel
		totalCoresToBurn := lc.limits.CPULimit * (cpuPct / 100.0)
		numWorkers := int(math.Ceil(totalCoresToBurn))
		if numWorkers < 1 {
			numWorkers = 1
		}
		dutyPerWorker := totalCoresToBurn / float64(numWorkers)
		if dutyPerWorker > 1.0 {
			dutyPerWorker = 1.0
		}

		for i := 0; i < numWorkers; i++ {
			go runCPUWorker(ctx, dutyPerWorker)
		}
	}

	lc.isActive = (cpuPct > 0 || targetBytes > 0)
	lc.targetDuration = req.DurationSeconds

	if req.DurationSeconds > 0 {
		lc.timerEnd = time.Now().Add(time.Duration(req.DurationSeconds) * time.Second)
		go func(endTime time.Time) {
			time.Sleep(time.Until(endTime))
			lc.mu.Lock()
			defer lc.mu.Unlock()
			// If this timer matches the active timer, stop it
			if lc.isActive && !lc.timerEnd.IsZero() && time.Now().After(lc.timerEnd.Add(-50*time.Millisecond)) {
				lc.stopInternal()
			}
		}(lc.timerEnd)
	} else {
		lc.timerEnd = time.Time{}
	}

	return nil
}

func (lc *LoadController) adjustMemory(targetBytes uint64) {
	currentBytes := uint64(len(lc.memChunks)) * 10 * 1024 * 1024
	chunkSize := 10 * 1024 * 1024 // 10MB chunks

	if targetBytes > currentBytes {
		needed := targetBytes - currentBytes
		chunksToAdd := int(math.Ceil(float64(needed) / float64(chunkSize)))
		for i := 0; i < chunksToAdd; i++ {
			chunk := make([]byte, chunkSize)
			// Touch each page to force Linux kernel RSS allocation
			for p := 0; p < len(chunk); p += 4096 {
				chunk[p] = 1
			}
			lc.memChunks = append(lc.memChunks, chunk)
		}
	} else if targetBytes < currentBytes {
		targetChunks := int(targetBytes / uint64(chunkSize))
		if targetChunks < len(lc.memChunks) {
			for i := targetChunks; i < len(lc.memChunks); i++ {
				lc.memChunks[i] = nil
			}
			lc.memChunks = lc.memChunks[:targetChunks]
			runtime.GC()
			debug.FreeOSMemory()
		}
	}
}

func (lc *LoadController) Stop() {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.stopInternal()
}

func (lc *LoadController) stopInternal() {
	if lc.cancelCPU != nil {
		lc.cancelCPU()
		lc.cancelCPU = nil
	}
	for i := range lc.memChunks {
		lc.memChunks[i] = nil
	}
	lc.memChunks = nil
	runtime.GC()
	debug.FreeOSMemory()

	lc.isActive = false
	lc.targetCPUPct = 0
	lc.targetMemPct = 0
	lc.targetMemMB = 0
	lc.targetDuration = 0
	lc.timerEnd = time.Time{}
}

func (lc *LoadController) GetStatus() StatusResponse {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	remaining := 0
	if lc.isActive && !lc.timerEnd.IsZero() {
		rem := time.Until(lc.timerEnd).Seconds()
		if rem > 0 {
			remaining = int(math.Ceil(rem))
		}
	}

	return StatusResponse{
		Limits:           lc.limits,
		Metrics:          lc.collector.GetMetrics(),
		IsActive:         lc.isActive,
		TargetCPUPercent: lc.targetCPUPct,
		TargetMemPercent: lc.targetMemPct,
		TargetMemMB:      lc.targetMemMB,
		DurationSeconds:  lc.targetDuration,
		RemainingSeconds: remaining,
	}
}

// runCPUWorker runs a tight loop with duty cycle over a 100ms window
func runCPUWorker(ctx context.Context, dutyRatio float64) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	window := 100 * time.Millisecond
	busyTime := time.Duration(float64(window) * dutyRatio)
	sleepTime := window - busyTime

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		start := time.Now()
		// Busy burn
		for time.Since(start) < busyTime {
			_ = math.Sqrt(1234567.89) * math.Sin(987654.321)
		}

		// Sleep remainder of window
		if sleepTime > 0 {
			time.Sleep(sleepTime)
		}
	}
}

// --- Main HTTP Server ---

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	limits := detectLimits()
	log.Printf("[uet-cloud-test] Starting load tester on port %s", port)
	log.Printf("[uet-cloud-test] Cgroup Version: %s | Limit: %.2f vCPUs, %d MB RAM | Host: %d CPUs, %d MB RAM",
		limits.CgroupVersion, limits.CPULimit, limits.MemoryLimit/(1024*1024), limits.HostCPUs, limits.HostMemory/(1024*1024))

	collector := newMetricsCollector(limits)
	controller := newLoadController(limits, collector)

	mux := http.NewServeMux()

	// Static web assets
	staticContent, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("Failed to load embedded static filesystem: %v", err)
	}
	fileServer := http.FileServer(http.FS(staticContent))
	mux.Handle("/", fileServer)

	// API Status
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := controller.GetStatus()
		json.NewEncoder(w).Encode(status)
	})

	// API Load
	mux.HandleFunc("/api/load", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req LoadRequest
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		if err := controller.ApplyLoad(req); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(controller.GetStatus())
	})

	// API Stop
	mux.HandleFunc("/api/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		controller.Stop()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(controller.GetStatus())
	})

	// Health check
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	log.Printf("[uet-cloud-test] WebUI is ready at http://0.0.0.0:%s", port)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
