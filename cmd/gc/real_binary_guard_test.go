package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cmd/gc's TestMain hands control to testscript.Main with "gc" and "bd"
// registered, and testscript prepends a directory of copies of THE TEST BINARY
// (named gc and bd) to PATH for the whole process. So inside this package's
// tests a PATH-resolved "bd" is the fake bdTestCmd unless the test names a real
// one by an explicit path. "bd was found on PATH" is therefore not evidence a
// test ran against a real bd (gcf-9qh6); the property that matters is that the
// binary reports the VERSIONS-pinned version.

// pinnedVersionFromFile returns the value of KEY from a KEY=VALUE versions file.
func pinnedVersionFromFile(path, key string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck
	prefix := key + "="
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, prefix) {
			value := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if value != "" {
				return value, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s not found in %s", key, path)
}

// repoPinnedVersion reads a pin from the repository's VERSIONS file. Package
// tests run with the package directory (cmd/gc) as the working directory.
func repoPinnedVersion(key string) (string, error) {
	return pinnedVersionFromFile(filepath.Join("..", "..", "VERSIONS"), key)
}

func TestPinnedVersionFromFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "VERSIONS")
	if err := os.WriteFile(file, []byte("# c\nBD_VERSION=1.3.0-rc.2\nDOLT_VERSION=2.1.10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := pinnedVersionFromFile(file, "BD_VERSION"); err != nil || got != "1.3.0-rc.2" {
		t.Fatalf("BD_VERSION = %q, %v", got, err)
	}
	if _, err := pinnedVersionFromFile(file, "MISSING"); err == nil {
		t.Fatal("a missing key must be an error")
	}
	for _, key := range []string{"BD_VERSION", "DOLT_VERSION"} {
		if got, err := repoPinnedVersion(key); err != nil || got == "" {
			t.Fatalf("the repository VERSIONS file must pin %s: %q, %v", key, got, err)
		}
	}
}
