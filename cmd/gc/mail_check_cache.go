package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultMailCheckEmptyTTL bounds how long a clean "no unread mail" answer is
// reused by `gc mail check --inject` (UserPromptSubmit hook). Override with
// GC_MAIL_CHECK_CACHE_TTL (Go duration or whole seconds); 0 disables.
const defaultMailCheckEmptyTTL = 20 * time.Second

func mailCheckEmptyTTL() time.Duration {
	v := strings.TrimSpace(os.Getenv("GC_MAIL_CHECK_CACHE_TTL"))
	if v == "" {
		return defaultMailCheckEmptyTTL
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return defaultMailCheckEmptyTTL
}

func mailCheckEmptyMarkerPath(cityPath, recipient string) string {
	if cityPath == "" || recipient == "" {
		return ""
	}
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, recipient)
	return filepath.Join(cityPath, ".gc", "runtime", "mail-check-empty", name)
}

// mailCheckEmptyMarkerFresh reports whether a clean empty answer for recipient
// was recorded within the TTL.
func mailCheckEmptyMarkerFresh(cityPath, recipient string) bool {
	ttl := mailCheckEmptyTTL()
	path := mailCheckEmptyMarkerPath(cityPath, recipient)
	if ttl <= 0 || path == "" {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	age := time.Since(fi.ModTime())
	return age >= 0 && age < ttl
}

// touchMailCheckEmptyMarker records a clean empty answer. Best-effort: a
// failure only means the next prompt pays the full query.
func touchMailCheckEmptyMarker(cityPath, recipient string) {
	path := mailCheckEmptyMarkerPath(cityPath, recipient)
	if path == "" || mailCheckEmptyTTL() <= 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	now := time.Now()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return
	}
	_ = os.Chtimes(path, now, now)
}
