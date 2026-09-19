package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func writeProviderScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prov.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func withProgressConfig(t *testing.T, cfg providerOpProgressConfig, sampler func(int) (uint64, bool)) {
	t.Helper()
	oldCfg, oldSample := providerInitProgress, providerOpSampleProgress
	providerInitProgress = cfg
	if sampler != nil {
		providerOpSampleProgress = sampler
	}
	t.Cleanup(func() { providerInitProgress, providerOpSampleProgress = oldCfg, oldSample })
}

func skipUnlessProgressSupported(t *testing.T) {
	t.Helper()
	if !providerOpProgressSupported || runtime.GOOS == "windows" {
		t.Skip("progress-aware init needs /proc")
	}
}

// A slow init that keeps making progress must outlive the stall window that
// a hung one would not (fail-before: a fixed 120s deadline killed it).
func TestProviderInitProgressAdvancingSlowInitSucceeds(t *testing.T) {
	skipUnlessProgressSupported(t)
	var n atomic.Uint64
	withProgressConfig(t, providerOpProgressConfig{stall: 600 * time.Millisecond, ceiling: 30 * time.Second, poll: 50 * time.Millisecond},
		func(int) (uint64, bool) { return n.Add(1), true })
	script := writeProviderScript(t, "sleep 2")
	start := time.Now()
	if err := runProviderOpWithEnvContext(context.Background(), script, nil, "init"); err != nil {
		t.Fatalf("advancing init failed: %v", err)
	}
	if time.Since(start) < 1500*time.Millisecond {
		t.Fatalf("script did not run to completion (%v)", time.Since(start))
	}
}

// A stalled init fails within the no-progress window with the new message and
// its whole process group is reaped.
func TestProviderInitProgressStalledInitFailsFastAndReapsGroup(t *testing.T) {
	skipUnlessProgressSupported(t)
	withProgressConfig(t, providerOpProgressConfig{stall: 500 * time.Millisecond, ceiling: 30 * time.Second, poll: 50 * time.Millisecond},
		func(int) (uint64, bool) { return 42, true })
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	script := writeProviderScript(t, "sleep 60 &\necho $! > "+pidfile+"\nwait")
	start := time.Now()
	err := runProviderOpWithEnvContext(context.Background(), script, nil, "init")
	took := time.Since(start)
	if err == nil {
		t.Fatal("stalled init returned nil")
	}
	if took > 5*time.Second {
		t.Fatalf("stall not failed fast: %v", took)
	}
	if !strings.Contains(err.Error(), "made no progress") || !strings.Contains(err.Error(), "exec beads init") {
		t.Fatalf("unexpected error text: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stall error must unwrap to DeadlineExceeded: %v", err)
	}
	raw, rerr := os.ReadFile(pidfile)
	if rerr != nil {
		t.Fatalf("child pid not recorded: %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nested child %d survived stall kill", pid)
}

// Progress never extends past the hard ceiling.
func TestProviderInitProgressCeilingBoundsEndlessProgress(t *testing.T) {
	skipUnlessProgressSupported(t)
	var n atomic.Uint64
	withProgressConfig(t, providerOpProgressConfig{stall: 10 * time.Second, ceiling: 700 * time.Millisecond, poll: 50 * time.Millisecond},
		func(int) (uint64, bool) { return n.Add(1), true })
	script := writeProviderScript(t, "sleep 60")
	err := runProviderOpWithEnvContext(context.Background(), script, nil, "init")
	if err == nil || !strings.Contains(err.Error(), "hard ceiling") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want hard-ceiling deadline error, got %v", err)
	}
}

// End to end with the real /proc sampler: a script that keeps doing I/O passes
// a stall window shorter than its runtime; a silent one is killed at the window.
func TestProviderInitProgressRealSampler(t *testing.T) {
	skipUnlessProgressSupported(t)
	cfg := providerOpProgressConfig{stall: 1200 * time.Millisecond, ceiling: 30 * time.Second, poll: 100 * time.Millisecond}
	withProgressConfig(t, cfg, sampleProcessGroupProgress)

	active := writeProviderScript(t, "i=0\nwhile [ $i -lt 14 ]; do echo tick >/dev/null; i=$((i+1)); sleep 0.25; done")
	if err := runProviderOpWithEnvContext(context.Background(), active, nil, "init"); err != nil {
		t.Fatalf("active init (3.5s > 1.2s window) failed: %v", err)
	}

	silent := writeProviderScript(t, "sleep 60")
	start := time.Now()
	err := runProviderOpWithEnvContext(context.Background(), silent, nil, "init")
	if err == nil || !strings.Contains(err.Error(), "made no progress") {
		t.Fatalf("silent init must stall out, got %v", err)
	}
	if time.Since(start) > 6*time.Second {
		t.Fatalf("silent init took %v", time.Since(start))
	}
}

// Control: non-init ops keep the fixed deadline (behavior init had before).
func TestProviderOpNonInitKeepsFixedDeadline(t *testing.T) {
	old := providerOpTimeout
	providerOpTimeout = func(string) time.Duration { return 500 * time.Millisecond }
	t.Cleanup(func() { providerOpTimeout = old })
	script := writeProviderScript(t, "sleep 60")
	start := time.Now()
	err := runProviderOpWithEnvContext(context.Background(), script, nil, "health")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("want fixed deadline, got %v after %v", err, time.Since(start))
	}
}
