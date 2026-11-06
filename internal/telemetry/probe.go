package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// A collector that accepts TCP connections but never answers (an overloaded
// VictoriaMetrics/VictoriaLogs) makes every OTLP exporter wait for its flush
// deadline: bd hard-codes 5 s per process, and one `gc hook --claim` spawns
// dozens of bd/gc children, so a claim exceeds a minute (gcf-thn63). A
// refused connection fails fast and costs nothing, so only a STALLED
// collector disables telemetry for this process and its children.
var (
	// probeTimeout bounds one health probe. A healthy local collector answers
	// in single-digit milliseconds.
	probeTimeout = 300 * time.Millisecond
	// stallTTL is how long a stalled verdict is reused without re-probing, so
	// a hung collector costs at most one probeTimeout per stallTTL city-wide
	// instead of one per gc invocation.
	stallTTL = 20 * time.Second
	// stallDir returns the private directory holding the per-collector stall
	// markers. It must be owned by the caller and mode 0700 (see privateDir).
	stallDir = defaultStallDir
)

// defaultStallDir prefers the per-user cache dir; a predictable path in a
// shared /tmp would let another user pre-create markers or plant symlinks.
func defaultStallDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "gc-otel-probe-"+strconv.Itoa(os.Getuid()))
}

// privateDir makes sure dir exists as a real directory owned by this user with
// no group/other access; anything else (symlink, foreign owner) is refused.
func privateDir(dir string) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0o077 != 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// markerFresh reports whether marker is a regular file we own, written within
// stallTTL. Symlinks and foreign files are never trusted.
func markerFresh(marker string) bool {
	fi, err := os.Lstat(marker)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return false
	}
	return time.Since(fi.ModTime()) < stallTTL
}

// writeMarker replaces the marker atomically (temp file + rename), so a
// pre-planted symlink at the marker path is replaced, never followed.
func writeMarker(marker string) {
	dir := filepath.Dir(marker)
	if !privateDir(dir) {
		return
	}
	f, err := os.CreateTemp(dir, ".stall-*")
	if err != nil {
		return
	}
	tmp := f.Name()
	_, werr := f.WriteString(time.Now().UTC().Format(time.RFC3339))
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Rename(tmp, marker) != nil {
		_ = os.Remove(tmp)
	}
}

// collectorsStalled reports whether any of the given OTLP endpoint URLs points
// at a collector that accepted a probe connection but did not answer in time.
func collectorsStalled(endpoints ...string) bool {
	seen := map[string]bool{}
	for _, raw := range endpoints {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || seen[u.Host] {
			continue
		}
		seen[u.Host] = true
		if hostStalled(u.Scheme, u.Host) {
			return true
		}
	}
	return false
}

func stallMarker(host string) string {
	sum := sha256.Sum256([]byte(host))
	return filepath.Join(stallDir(), "stalled-"+hex.EncodeToString(sum[:4]))
}

func hostStalled(scheme, host string) bool {
	marker := stallMarker(host)
	if markerFresh(marker) {
		return true
	}
	if scheme == "" {
		scheme = "http"
	}
	client := &http.Client{Timeout: probeTimeout}
	resp, err := client.Get(scheme + "://" + host + "/health")
	if err == nil {
		_ = resp.Body.Close()
		_ = os.Remove(marker)
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		writeMarker(marker)
		return true
	}
	// Refused, reset, DNS: exporters fail fast on these; not a stall.
	return false
}
