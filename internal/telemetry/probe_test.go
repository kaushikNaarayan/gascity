package telemetry

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func withProbeEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldDir, oldTO, oldTTL := stallDir, probeTimeout, stallTTL
	stallDir = func() string { return dir }
	probeTimeout = 150 * time.Millisecond
	stallTTL = time.Minute
	t.Cleanup(func() { stallDir, probeTimeout, stallTTL = oldDir, oldTO, oldTTL })
}

// hangingListener accepts connections and never answers.
func hangingListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		var conns []net.Conn
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return "http://" + ln.Addr().String()
}

func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(srv.Close)
	return srv
}

func TestCollectorsStalledHangingCollector(t *testing.T) {
	withProbeEnv(t)
	if !collectorsStalled(hangingListener(t) + "/m") {
		t.Fatal("hung collector not detected as stalled")
	}
}

func TestCollectorsStalledHealthyAndRefusedAreNot(t *testing.T) {
	withProbeEnv(t)
	if collectorsStalled(okServer(t).URL + "/m") {
		t.Fatal("healthy collector reported stalled")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if collectorsStalled("http://" + addr + "/m") {
		t.Fatal("refused port must fail fast, not count as stalled")
	}
}

func TestStalledVerdictIsCachedWithoutReprobing(t *testing.T) {
	withProbeEnv(t)
	u := hangingListener(t)
	if !collectorsStalled(u) {
		t.Fatal("first probe should stall")
	}
	start := time.Now()
	if !collectorsStalled(u) {
		t.Fatal("cached verdict lost")
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("cached verdict re-probed (%s)", d)
	}
}

func TestStalledMarkerExpiresAndHealthyClearsIt(t *testing.T) {
	withProbeEnv(t)
	srv := okServer(t)
	m := stallMarker(strings.TrimPrefix(srv.URL, "http://"))
	if err := os.WriteFile(m, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(m, old, old); err != nil {
		t.Fatal(err)
	}
	if collectorsStalled(srv.URL) {
		t.Fatal("expired marker must re-probe and find the collector healthy")
	}
	if _, err := os.Stat(m); !os.IsNotExist(err) {
		t.Fatal("healthy probe should remove the stale marker")
	}
}

func TestResolveEndpointsDisablesTelemetryForStalledCollector(t *testing.T) {
	withProbeEnv(t)
	u := hangingListener(t)
	t.Setenv(EnvSDKDisabled, "")
	t.Setenv(EnvMetricsURL, u+"/m")
	t.Setenv(EnvLogsURL, u+"/l")
	if _, _, enabled := resolveEndpoints(); enabled {
		t.Fatal("stalled collector must disable telemetry (no 5 s flush per child)")
	}
}

func TestStallMarkerSymlinkIsReplacedNotFollowed(t *testing.T) {
	withProbeEnv(t)
	u := hangingListener(t)
	victim := t.TempDir() + "/victim"
	if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := stallMarker(strings.TrimPrefix(u, "http://"))
	if err := os.Symlink(victim, marker); err != nil {
		t.Fatal(err)
	}
	if !collectorsStalled(u) {
		t.Fatal("hung collector must still be detected")
	}
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Fatalf("symlink target was overwritten: %q", b)
	}
}

func TestSymlinkedMarkerIsNotTrusted(t *testing.T) {
	withProbeEnv(t)
	srv := okServer(t)
	marker := stallMarker(strings.TrimPrefix(srv.URL, "http://"))
	target := t.TempDir() + "/fresh"
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, marker); err != nil {
		t.Fatal(err)
	}
	if collectorsStalled(srv.URL) {
		t.Fatal("a pre-planted symlink marker must not suppress telemetry")
	}
}

func TestLooseStallDirIsRefused(t *testing.T) {
	withProbeEnv(t)
	u := hangingListener(t)
	dir := stallDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !collectorsStalled(u) {
		t.Fatal("hung collector must still be detected")
	}
	if _, err := os.Lstat(stallMarker(strings.TrimPrefix(u, "http://"))); err == nil {
		t.Fatal("marker written into a group/other-accessible dir")
	}
}

func TestResolveEndpointsOTLPFallbackBranchGated(t *testing.T) {
	withProbeEnv(t)
	u := hangingListener(t)
	t.Setenv(EnvSDKDisabled, "")
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, u)
	if _, _, enabled := resolveEndpoints(); enabled {
		t.Fatal("OTEL_EXPORTER_OTLP_ENDPOINT fallback must also be disabled for a stalled collector")
	}
}
