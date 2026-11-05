package main

import (
	"os"
	"path/filepath"
	"strings"
)

// isTestBinary reports whether the current process is a Go test binary.
// Go test binaries are named *.test (e.g., "gc.test"). Used by runtime
// guards to prevent tests from accidentally hitting host infrastructure.
func isTestBinary() bool {
	if len(os.Args) == 0 {
		return false
	}
	return isTestBinaryArg0(os.Args[0])
}

// isTestBinaryArg0 is isTestBinary for an explicit argv0.
func isTestBinaryArg0(argv0 string) bool {
	// Go test binaries are named <pkg>.test. Match the binary's own name only:
	// a directory such as /home/u/app.test/bin/ must not make production gc
	// look like a test binary (gcf-a14q).
	return strings.HasSuffix(filepath.Base(argv0), ".test")
}
