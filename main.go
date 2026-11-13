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
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// parseProcSelfCgroup parses /proc/self/cgroup to get the relative cgroup path for v2 and v1 controllers.
func parseProcSelfCgroup() map[string]string {
	paths := make(map[string]string)
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return paths
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 {
			controllers := parts[1]
			relPath := strings.TrimPrefix(parts[2], "/")
			if controllers == "" {
				// Cgroup v2 unified hierarchy
				paths["v2"] = relPath
			} else {
				for _, ctrl := range strings.Split(controllers, ",") {
					paths[ctrl] = relPath
				}
			}
		}
	}
	return paths
}

// walkUpDirs generates directories starting from fullPath walking up to stopDir (inclusive).
func walkUpDirs(fullPath, stopDir string) []string {
	var dirs []string
	curr := filepath.Clean(fullPath)
	stop := filepath.Clean(stopDir)
	for {
		dirs = append(dirs, curr)
		if curr == stop || curr == "/" || curr == "." {
			break
		}
		parent := filepath.Dir(curr)
		if parent == curr {
			break
		}
		curr = parent
	}
	return dirs
}

func detectLimits() SystemLimits {
	limits := SystemLimits{
		CgroupVersion: "none",
		HostCPUs:      runtime.NumCPU(),
	}

	// Read host total memory from /proc/meminfo
	limits.HostMemory = readMemInfoTotal()
	cgroupPaths := parseProcSelfCgroup()

	// 1. Detect Cgroup v2
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		limits.CgroupVersion = "v2"

		// Candidate paths to check for cpu.max and memory.max
		var searchDirs []string
		if v2Rel, ok := cgroupPaths["v2"]; ok && v2Rel != "" {
			searchDirs = append(searchDirs, walkUpDirs(filepath.Join("/sys/fs/cgroup", v2Rel), "/sys/fs/cgroup")...)
		}
		searchDirs = append(searchDirs, "/sys/fs/cgroup")

		// Search cpu.max
		for _, dir := range searchDirs {
			cpuMaxFile := filepath.Join(dir, "cpu.max")
			if data, err := os.ReadFile(cpuMaxFile); err == nil {
				parts := strings.Fields(string(data))
				if len(parts) >= 2 && parts[0] != "max" {
					quota, err1 := strconv.ParseFloat(parts[0], 64)
					period, err2 := strconv.ParseFloat(parts[1], 64)
					if err1 == nil && err2 == nil && quota > 0 && period > 0 {
						limits.CPULimit = quota / period
						log.Printf("[uet-cloud-test] Found cgroup v2 CPU quota in %s: quota=%.0f, period=%.0f => %.2f vCPUs",
							cpuMaxFile, quota, period, limits.CPULimit)
						break
					}
				}
			}
		}

		// Search memory.max
		for _, dir := range searchDirs {
			memMaxFile := filepath.Join(dir, "memory.max")
			if data, err := os.ReadFile(memMaxFile); err == nil {
				str := strings.TrimSpace(string(data))
				if str != "max" && str != "" {
					if val, err := strconv.ParseUint(str, 10, 64); err == nil && val > 0 {
						limits.MemoryLimit = val
						log.Printf("[uet-cloud-test] Found cgroup v2 Memory limit in %s: %d bytes (%.1f MB)",
							memMaxFile, val, float64(val)/(1024*1024))
						break
					}
				}
			}
		}
	} else {
		// 2. Detect Cgroup v1
		cgroupV1 := false

		// Candidate directories for CPU controller in v1
		cpuBaseDirs := []string{
			"/sys/fs/cgroup/cpu,cpuacct",
			"/sys/fs/cgroup/cpu",
			"/sys/fs/cgroup/cpuacct,cpu",
		}

		var cpuSearchDirs []string
		cpuRel := cgroupPaths["cpu"]
		if cpuRel == "" {
			cpuRel = cgroupPaths["cpu,cpuacct"]
		}

		for _, base := range cpuBaseDirs {
			if _, err := os.Stat(base); err == nil {
				cgroupV1 = true
				if cpuRel != "" {
					cpuSearchDirs = append(cpuSearchDirs, walkUpDirs(filepath.Join(base, cpuRel), base)...)
				}
				cpuSearchDirs = append(cpuSearchDirs, base)
			}
		}

		for _, dir := range cpuSearchDirs {
			qFile := filepath.Join(dir, "cpu.cfs_quota_us")
			pFile := filepath.Join(dir, "cpu.cfs_period_us")
			quotaData, errQ := os.ReadFile(qFile)
			periodData, errP := os.ReadFile(pFile)
			if errQ == nil && errP == nil {
				quota, err1 := strconv.ParseFloat(strings.TrimSpace(string(quotaData)), 64)
				period, err2 := strconv.ParseFloat(strings.TrimSpace(string(periodData)), 64)
				if err1 == nil && err2 == nil && quota > 0 && period > 0 {
					limits.CPULimit = quota / period
					log.Printf("[uet-cloud-test] Found cgroup v1 CPU quota in %s: quota=%.0f, period=%.0f => %.2f vCPUs",
						qFile, quota, period, limits.CPULimit)
					break
				}
			}
		}

		// Candidate directories for Memory controller in v1
		memBaseDirs := []string{
			"/sys/fs/cgroup/memory",
		}
		var memSearchDirs []string
		memRel := cgroupPaths["memory"]
		for _, base := range memBaseDirs {
			if _, err := os.Stat(base); err == nil {
				cgroupV1 = true
				if memRel != "" {
					memSearchDirs = append(memSearchDirs, walkUpDirs(filepath.Join(base, memRel), base)...)
				}
				memSearchDirs = append(memSearchDirs, base)
			}
		}

		for _, dir := range memSearchDirs {
			mFile := filepath.Join(dir, "memory.limit_in_bytes")
			if memData, err := os.ReadFile(mFile); err == nil {
				if val, err := strconv.ParseUint(strings.TrimSpace(string(memData)), 10, 64); err == nil {
					if val > 0 && val < 9000000000000000000 {
						limits.MemoryLimit = val
						log.Printf("[uet-cloud-test] Found cgroup v1 Memory limit in %s: %d bytes (%.1f MB)",
							mFile, val, float64(val)/(1024*1024))
						break
					}
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
		log.Printf("[uet-cloud-test] No cgroup CPU limit detected, fallback to Host CPUs: %d", limits.HostCPUs)
	}
	if limits.MemoryLimit == 0 {
		limits.MemoryLimit = limits.HostMemory
		log.Printf("[uet-cloud-test] No cgroup Memory limit detected, fallback to Host Memory: %d MB", limits.HostMemory/(1024*1024))
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

// --- HTTP Traffic Generator Engine ---

type TrafficRequest struct {
	TargetURL       string `json:"target_url"`
	Concurrency     int    `json:"concurrency"`
	DurationSeconds int    `json:"duration_seconds"`
}

type TrafficUpdateRequest struct {
	Concurrency int `json:"concurrency"`
}

type TrafficStatus struct {
	IsActive         bool    `json:"is_active"`
	TargetURL        string  `json:"target_url"`
	Concurrency      int     `json:"concurrency"`
	ActiveWorkers    int32   `json:"active_workers"`
	DurationSeconds  int     `json:"duration_seconds"`
	ElapsedSeconds   int     `json:"elapsed_seconds"`
	RemainingSeconds int     `json:"remaining_seconds"`
	RPS              float64 `json:"rps"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	TotalRequests    int64   `json:"total_requests"`
	Status2xx        int64   `json:"status_2xx"`
	Status4xx        int64   `json:"status_4xx"`
	Status5xx        int64   `json:"status_5xx"`
	StatusErrors     int64   `json:"status_errors"`
}

type TrafficGenerator struct {
	mu            sync.RWMutex
	client        *http.Client
	isActive      bool
	targetURL     string
	concurrency   int
	targetWorkers int32
	activeWorkers int32
	durationSec   int
	startTime     time.Time
	ctx           context.Context
	cancel        context.CancelFunc

	totalRequests  int64
	status2xx      int64
	status4xx      int64
	status5xx      int64
	statusErrors   int64
	totalLatencyNs int64

	lastSampleTime   time.Time
	lastSampleReqs   int64
	lastSampleLatNs  int64
	currentRPS       float64
	currentLatencyMs float64
}

func newTrafficGenerator() *TrafficGenerator {
	transport := &http.Transport{
		MaxIdleConns:        2000,
		MaxIdleConnsPerHost: 1000,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
	tg := &TrafficGenerator{
		client:         client,
		lastSampleTime: time.Now(),
	}
	go tg.metricsLoop()
	return tg
}

func (tg *TrafficGenerator) metricsLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		tg.mu.Lock()
		if !tg.isActive {
			tg.currentRPS = 0
			tg.mu.Unlock()
			continue
		}

		now := time.Now()
		elapsed := now.Sub(tg.lastSampleTime).Seconds()
		if elapsed > 0 {
			currReqs := atomic.LoadInt64(&tg.totalRequests)
			currLatNs := atomic.LoadInt64(&tg.totalLatencyNs)

			deltaReqs := currReqs - tg.lastSampleReqs
			deltaLatNs := currLatNs - tg.lastSampleLatNs

			tg.currentRPS = float64(deltaReqs) / elapsed
			if deltaReqs > 0 {
				tg.currentLatencyMs = float64(deltaLatNs) / float64(deltaReqs) / 1e6
			}
			tg.lastSampleReqs = currReqs
			tg.lastSampleLatNs = currLatNs
			tg.lastSampleTime = now
		}

		// Auto-stop if duration expired
		if tg.durationSec > 0 && time.Since(tg.startTime) >= time.Duration(tg.durationSec)*time.Second {
			tg.stopLocked()
		}
		tg.mu.Unlock()
	}
}

func (tg *TrafficGenerator) Start(req TrafficRequest) error {
	target := strings.TrimSpace(req.TargetURL)
	if target == "" {
		return fmt.Errorf("target_url is required")
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "http://" + target
	}
	if req.Concurrency <= 0 {
		req.Concurrency = 10
	}
	if req.Concurrency > 2000 {
		req.Concurrency = 2000
	}

	tg.mu.Lock()
	defer tg.mu.Unlock()

	if tg.isActive && tg.cancel != nil {
		tg.cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	tg.ctx = ctx
	tg.cancel = cancel
	tg.isActive = true
	tg.targetURL = target
	tg.concurrency = req.Concurrency
	tg.durationSec = req.DurationSeconds
	tg.startTime = time.Now()
	tg.lastSampleTime = time.Now()
	tg.lastSampleReqs = 0
	tg.lastSampleLatNs = 0
	tg.currentRPS = 0
	tg.currentLatencyMs = 0

	atomic.StoreInt64(&tg.totalRequests, 0)
	atomic.StoreInt64(&tg.status2xx, 0)
	atomic.StoreInt64(&tg.status4xx, 0)
	atomic.StoreInt64(&tg.status5xx, 0)
	atomic.StoreInt64(&tg.statusErrors, 0)
	atomic.StoreInt64(&tg.totalLatencyNs, 0)
	atomic.StoreInt32(&tg.activeWorkers, 0)
	atomic.StoreInt32(&tg.targetWorkers, int32(req.Concurrency))

	for i := 0; i < req.Concurrency; i++ {
		atomic.AddInt32(&tg.activeWorkers, 1)
		go tg.runWorker(ctx, target)
	}

	log.Printf("[uet-cloud-test] Started HTTP traffic generator: Target=%s, Concurrency=%d, Duration=%ds",
		target, req.Concurrency, req.DurationSeconds)
	return nil
}

func (tg *TrafficGenerator) UpdateConcurrency(concurrency int) error {
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > 2000 {
		concurrency = 2000
	}

	tg.mu.Lock()
	defer tg.mu.Unlock()

	if !tg.isActive {
		return fmt.Errorf("traffic generator is not active")
	}

	prevTarget := atomic.LoadInt32(&tg.targetWorkers)
	newTarget := int32(concurrency)
	tg.concurrency = concurrency
	atomic.StoreInt32(&tg.targetWorkers, newTarget)

	if newTarget > prevTarget {
		diff := int(newTarget - prevTarget)
		for i := 0; i < diff; i++ {
			atomic.AddInt32(&tg.activeWorkers, 1)
			go tg.runWorker(tg.ctx, tg.targetURL)
		}
	}
	log.Printf("[uet-cloud-test] Scaled HTTP traffic concurrency on-the-fly: %d -> %d", prevTarget, newTarget)
	return nil
}

func (tg *TrafficGenerator) Stop() {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	tg.stopLocked()
	log.Printf("[uet-cloud-test] Stopped HTTP traffic generator")
}

func (tg *TrafficGenerator) stopLocked() {
	if !tg.isActive {
		return
	}
	tg.isActive = false
	if tg.cancel != nil {
		tg.cancel()
		tg.cancel = nil
	}
	atomic.StoreInt32(&tg.targetWorkers, 0)
}

func (tg *TrafficGenerator) runWorker(ctx context.Context, targetURL string) {
	defer atomic.AddInt32(&tg.activeWorkers, -1)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if atomic.LoadInt32(&tg.activeWorkers) > atomic.LoadInt32(&tg.targetWorkers) {
			return
		}

		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
		if err != nil {
			atomic.AddInt64(&tg.statusErrors, 1)
			atomic.AddInt64(&tg.totalRequests, 1)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		req.Header.Set("User-Agent", "uet-cloud-load-tester/1.0")

		resp, err := tg.client.Do(req)
		latency := time.Since(start)
		atomic.AddInt64(&tg.totalLatencyNs, latency.Nanoseconds())
		atomic.AddInt64(&tg.totalRequests, 1)

		if err != nil {
			atomic.AddInt64(&tg.statusErrors, 1)
			time.Sleep(20 * time.Millisecond)
			continue
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			atomic.AddInt64(&tg.status2xx, 1)
		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			atomic.AddInt64(&tg.status4xx, 1)
		case resp.StatusCode >= 500:
			atomic.AddInt64(&tg.status5xx, 1)
		}
	}
}

func (tg *TrafficGenerator) GetStatus() TrafficStatus {
	tg.mu.RLock()
	defer tg.mu.RUnlock()

	elapsed := 0
	remaining := 0
	if tg.isActive {
		elapsed = int(time.Since(tg.startTime).Seconds())
		if tg.durationSec > 0 {
			remaining = tg.durationSec - elapsed
			if remaining < 0 {
				remaining = 0
			}
		}
	}

	return TrafficStatus{
		IsActive:         tg.isActive,
		TargetURL:        tg.targetURL,
		Concurrency:      tg.concurrency,
		ActiveWorkers:    atomic.LoadInt32(&tg.activeWorkers),
		DurationSeconds:  tg.durationSec,
		ElapsedSeconds:   elapsed,
		RemainingSeconds: remaining,
		RPS:              tg.currentRPS,
		AvgLatencyMs:     tg.currentLatencyMs,
		TotalRequests:    atomic.LoadInt64(&tg.totalRequests),
		Status2xx:        atomic.LoadInt64(&tg.status2xx),
		Status4xx:        atomic.LoadInt64(&tg.status4xx),
		Status5xx:        atomic.LoadInt64(&tg.status5xx),
		StatusErrors:     atomic.LoadInt64(&tg.statusErrors),
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
	trafficGen := newTrafficGenerator()

	mux := http.NewServeMux()

	// Static web assets
	staticContent, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("Failed to load embedded static filesystem: %v", err)
	}
	fileServer := http.FileServer(http.FS(staticContent))
	mux.Handle("/", fileServer)

	// API Status (Container Internal Load)
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := controller.GetStatus()
		json.NewEncoder(w).Encode(status)
	})

	// API Load (Container Internal Load)
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

	// API Stop (Container Internal Load)
	mux.HandleFunc("/api/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		controller.Stop()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(controller.GetStatus())
	})

	// API Traffic Status
	mux.HandleFunc("/api/traffic/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(trafficGen.GetStatus())
	})

	// API Traffic Start
	mux.HandleFunc("/api/traffic/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req TrafficRequest
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		if err := trafficGen.Start(req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(trafficGen.GetStatus())
	})

	// API Traffic Update (on-the-fly concurrency change)
	mux.HandleFunc("/api/traffic/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req TrafficUpdateRequest
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		if err := trafficGen.UpdateConcurrency(req.Concurrency); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(trafficGen.GetStatus())
	})

	// API Traffic Stop
	mux.HandleFunc("/api/traffic/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		trafficGen.Stop()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(trafficGen.GetStatus())
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
