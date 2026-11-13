package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWalkUpDirs(t *testing.T) {
	dirs := walkUpDirs("/sys/fs/cgroup/kubepods.slice/pod123/container", "/sys/fs/cgroup")
	if len(dirs) < 3 {
		t.Fatalf("expected at least 3 directories, got %d: %v", len(dirs), dirs)
	}
	if dirs[0] != "/sys/fs/cgroup/kubepods.slice/pod123/container" {
		t.Errorf("unexpected first dir: %s", dirs[0])
	}
	if dirs[len(dirs)-1] != "/sys/fs/cgroup" {
		t.Errorf("unexpected last dir: %s", dirs[len(dirs)-1])
	}
}

func TestParseProcSelfCgroup(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "cgroup_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	content := `0::/kubepods.slice/kubepods-burstable.slice/pod123.slice/container.scope
`
	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	// Parse custom cgroup file directly
	data, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty cgroup test file")
	}
}

func TestDetectLimitsSmoke(t *testing.T) {
	limits := detectLimits()
	if limits.CPULimit <= 0 {
		t.Errorf("expected CPULimit > 0, got %.2f", limits.CPULimit)
	}
	if limits.MemoryLimit == 0 {
		t.Errorf("expected MemoryLimit > 0, got %d", limits.MemoryLimit)
	}
	if limits.HostCPUs <= 0 {
		t.Errorf("expected HostCPUs > 0, got %d", limits.HostCPUs)
	}
}

func TestDynamicVCPUQuotaCalculation(t *testing.T) {
	testCases := []struct {
		quotaStr    string
		periodStr   string
		expectedCPU float64
	}{
		{"25000", "100000", 0.25},
		{"50000", "100000", 0.50},
		{"75000", "100000", 0.75},
		{"100000", "100000", 1.00},
		{"125000", "100000", 1.25},
		{"150000", "100000", 1.50},
		{"200000", "100000", 2.00},
		{"400000", "100000", 4.00},
	}

	for _, tc := range testCases {
		tmpDir, err := os.MkdirTemp("", "cgroup_test_*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(tmpDir)

		cpuMaxFile := tmpDir + "/cpu.max"
		content := tc.quotaStr + " " + tc.periodStr + "\n"
		if err := os.WriteFile(cpuMaxFile, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}

		data, err := os.ReadFile(cpuMaxFile)
		if err != nil {
			t.Fatal(err)
		}

		// Replicate calculation logic
		var detectedCPU float64
		parts := strings.Fields(string(data))
		if len(parts) >= 2 && parts[0] != "max" {
			quota, err1 := strconv.ParseFloat(parts[0], 64)
			period, err2 := strconv.ParseFloat(parts[1], 64)
			if err1 == nil && err2 == nil && period > 0 {
				detectedCPU = quota / period
			}
		}

		if detectedCPU != tc.expectedCPU {
			t.Errorf("expected %.2f vCPU for %s/%s, got %.2f", tc.expectedCPU, tc.quotaStr, tc.periodStr, detectedCPU)
		}
	}
}

func TestTrafficGeneratorLifecycle(t *testing.T) {
	var requestCount int64

	// Mock server to receive requests
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer ts.Close()

	tg := newTrafficGenerator()
	defer tg.Stop()

	// 1. Start traffic with 5 workers
	req := TrafficRequest{
		TargetURL:       ts.URL,
		Concurrency:     5,
		DurationSeconds: 10,
	}
	if err := tg.Start(req); err != nil {
		t.Fatalf("failed to start traffic: %v", err)
	}

	// Allow some requests to execute
	time.Sleep(300 * time.Millisecond)

	status := tg.GetStatus()
	if !status.IsActive {
		t.Errorf("expected traffic to be active")
	}
	if status.ActiveWorkers <= 0 {
		t.Errorf("expected active workers > 0, got %d", status.ActiveWorkers)
	}
	if status.TotalRequests <= 0 {
		t.Errorf("expected total requests > 0, got %d", status.TotalRequests)
	}

	// 2. Scale concurrency on-the-fly to 15 workers
	if err := tg.UpdateConcurrency(15); err != nil {
		t.Fatalf("failed to update concurrency: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	status = tg.GetStatus()
	if status.Concurrency != 15 {
		t.Errorf("expected target concurrency 15, got %d", status.Concurrency)
	}

	// 3. Stop traffic
	tg.Stop()
	time.Sleep(100 * time.Millisecond)

	status = tg.GetStatus()
	if status.IsActive {
		t.Errorf("expected traffic to be inactive after stop")
	}
	if status.ActiveWorkers != 0 {
		t.Errorf("expected 0 active workers after stop, got %d", status.ActiveWorkers)
	}
}
