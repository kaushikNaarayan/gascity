package telemetry

import (
	"os"
	"testing"
	"time"
)

// TestMain keeps every test in this package off the real per-user stall
// markers and off live collectors: a genuinely hung local collector (the
// gcf-thn63 incident) must not make unrelated Init/OTELEnvMap tests fail.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gc-telemetry-test-")
	if err != nil {
		panic(err)
	}
	stallDir = func() string { return dir }
	probeTimeout = 100 * time.Millisecond
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
