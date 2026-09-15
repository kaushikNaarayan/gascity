package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPreflightScopeIsRigResolvesSymlinkedCityPath is the gcf-bym6 regression
// control. QA's REJECT of candidate 71beafba6 found that its scope compare
// (filepath.Clean(scope) != filepath.Clean(cityPath)) is lexical only: a
// symlinked city path (e.g. macOS's /tmp, which is itself a symlink to
// /private/tmp) compares UNEQUAL to its real-path form even though both name
// the same directory, so a real city-scope preflight is misclassified as a
// rig scope and silently downgraded onto the rig-aware bd runner instead of
// the city one. The Mac is one of our two hosts, so this is not
// hypothetical.
//
// This test is written to discriminate, not just to pass: it runs BOTH the
// pre-fix lexical compare (inlined below, exactly as it shipped in 71beafba6)
// and the shipped preflightScopeIsRig (which resolves symlinks via samePath)
// against the same symlinked-alias pair, and requires them to disagree in the
// documented direction. If a future edit collapses that gap -- accidentally
// reintroducing a lexical-only compare -- this test fails loudly rather than
// silently stop catching it.
func TestPreflightScopeIsRigResolvesSymlinkedCityPath(t *testing.T) {
	rawCityDir := t.TempDir()
	// Resolve the temp dir to its real path first: on some hosts t.TempDir()
	// itself returns a path that is already a symlink alias (e.g. macOS's
	// /tmp -> /private/tmp), which would make "real" and "alias" already
	// equal under a raw string compare and defeat the test's premise before
	// it starts.
	resolvedReal, err := filepath.EvalSymlinks(rawCityDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(real): %v", err)
	}

	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "city-alias")
	if err := os.Symlink(resolvedReal, alias); err != nil {
		t.Skipf("symlink not supported on this host: %v", err)
	}

	// Sanity: the two spellings must actually differ lexically, and must
	// resolve to the same real directory -- otherwise this isn't testing
	// what it claims to.
	if filepath.Clean(alias) == filepath.Clean(resolvedReal) {
		t.Fatalf("test setup invalid: alias %q and real path %q are lexically identical", alias, resolvedReal)
	}
	evalAlias, err := filepath.EvalSymlinks(alias)
	if err != nil {
		t.Fatalf("EvalSymlinks(alias): %v", err)
	}
	if evalAlias != resolvedReal {
		t.Fatalf("test setup invalid: alias %q does not resolve to real path %q (got %q)", alias, resolvedReal, evalAlias)
	}

	// PRE-FIX BEHAVIOR (71beafba6, QA REJECT): lexical-only compare. Inlined
	// verbatim from the rejected candidate's beads_preflight_checker.go
	// rather than referencing removed code, so this half of the test keeps
	// discriminating even after that code is long gone.
	preFixTreatsAsRig := filepath.Clean(alias) != filepath.Clean(resolvedReal)
	if !preFixTreatsAsRig {
		t.Fatalf("test setup invalid: the pre-fix lexical compare must treat %q and %q as different scopes for this test to demonstrate the bug", alias, resolvedReal)
	}

	// POST-FIX BEHAVIOR (this candidate): symlink-resolving compare. The
	// alias and the real path name the SAME city, so this must be false.
	if got := preflightScopeIsRig(alias, resolvedReal); got {
		t.Fatalf("preflightScopeIsRig(%q, %q) = true, want false -- a symlinked alias of the city root must not be misclassified as a rig scope (gcf-bym6)", alias, resolvedReal)
	}

	// Same-string case must obviously still report "not a rig" too.
	if got := preflightScopeIsRig(resolvedReal, resolvedReal); got {
		t.Fatalf("preflightScopeIsRig(%q, %q) = true, want false for an identical path", resolvedReal, resolvedReal)
	}

	// A genuinely different directory (a real rig subdirectory) must still
	// be classified as a rig scope -- the fix must not over-correct into
	// treating everything as the city.
	rigDir := filepath.Join(resolvedReal, "rig-subdir")
	if err := os.Mkdir(rigDir, 0o755); err != nil {
		t.Fatalf("Mkdir(rigDir): %v", err)
	}
	if got := preflightScopeIsRig(rigDir, resolvedReal); !got {
		t.Fatalf("preflightScopeIsRig(%q, %q) = false, want true -- a genuine rig subdirectory must still route to the rig-aware runner", rigDir, resolvedReal)
	}
}
