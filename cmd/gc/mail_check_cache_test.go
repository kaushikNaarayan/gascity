package main

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func TestMailCheckEmptyMarkerFreshness(t *testing.T) {
	city := t.TempDir()
	t.Setenv("GC_MAIL_CHECK_CACHE_TTL", "")
	if mailCheckEmptyMarkerFresh(city, "gascity/build") {
		t.Fatal("fresh before any marker was written")
	}
	touchMailCheckEmptyMarker(city, "gascity/build")
	if !mailCheckEmptyMarkerFresh(city, "gascity/build") {
		t.Fatal("marker should be fresh right after touch")
	}
	if mailCheckEmptyMarkerFresh(city, "gascity/qa") {
		t.Fatal("marker must be per recipient")
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(mailCheckEmptyMarkerPath(city, "gascity/build"), old, old); err != nil {
		t.Fatal(err)
	}
	if mailCheckEmptyMarkerFresh(city, "gascity/build") {
		t.Fatal("marker older than the TTL must not be fresh")
	}
}

func TestMailCheckEmptyMarkerTTLZeroDisables(t *testing.T) {
	city := t.TempDir()
	t.Setenv("GC_MAIL_CHECK_CACHE_TTL", "0")
	touchMailCheckEmptyMarker(city, "gascity/build")
	if mailCheckEmptyMarkerFresh(city, "gascity/build") {
		t.Fatal("TTL 0 must disable the fast path")
	}
}

func TestRouteMailCheckInjectSkipsQueryWhenRecentlyEmpty(t *testing.T) {
	city := t.TempDir()
	t.Setenv("GC_MAIL_CHECK_CACHE_TTL", "")
	touchMailCheckEmptyMarker(city, "gascity/build")
	var out, errb bytes.Buffer
	// A nil client and no provider would otherwise reach the local fallback; the
	// fresh marker must short-circuit before any of it.
	if code := routeMailCheck(city, []string{"gascity/build"}, true, "", nil, "test", &out, &errb); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("expected silent fast path, got out=%q err=%q", out.String(), errb.String())
	}
}
