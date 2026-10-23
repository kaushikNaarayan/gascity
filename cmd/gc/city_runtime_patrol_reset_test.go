package main

import (
	"testing"
	"time"
)

// TestSyncPatrolTickerAppliesReloadedInterval pins gcf-rrzbj (d): a reloaded
// Daemon.PatrolInterval must re-arm the running patrol ticker, not wait for a
// controller restart.
func TestSyncPatrolTickerAppliesReloadedInterval(t *testing.T) {
	current := time.Hour
	ticker := time.NewTicker(current)
	defer ticker.Stop()

	if syncPatrolTicker(ticker, &current, time.Hour) {
		t.Fatal("unchanged interval must not reset the ticker")
	}
	if syncPatrolTicker(ticker, &current, 0) {
		t.Fatal("non-positive interval must be ignored")
	}
	if !syncPatrolTicker(ticker, &current, 10*time.Millisecond) {
		t.Fatal("changed interval must reset the ticker")
	}
	if current != 10*time.Millisecond {
		t.Fatalf("current = %v, want 10ms", current)
	}
	select {
	case <-ticker.C:
	case <-time.After(5 * time.Second):
		t.Fatal("ticker still on the old 1h interval after reload")
	}
}
