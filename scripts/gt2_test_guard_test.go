package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGT2TestGuard(t *testing.T) {
	mark := t.TempDir()
	const gt2 = "Ubuntu-2404-noble-amd64-base"
	tests := []struct {
		name    string
		env     []string
		blocked bool
	}{
		{"gt2 host with city dir is refused", []string{"GC_GT2_GUARD_HOSTNAME=" + gt2, "GC_GT2_GUARD_MARK_DIR=" + mark}, true},
		{"gt2 host with explicit override runs", []string{"GC_GT2_GUARD_HOSTNAME=" + gt2, "GC_GT2_GUARD_MARK_DIR=" + mark, "GC_ALLOW_GT2_TESTS=1"}, false},
		{"other host runs", []string{"GC_GT2_GUARD_HOSTNAME=gt3", "GC_GT2_GUARD_MARK_DIR=" + mark}, false},
		{"gt2 hostname without city dir runs", []string{"GC_GT2_GUARD_HOSTNAME=" + gt2, "GC_GT2_GUARD_MARK_DIR=" + filepath.Join(mark, "absent")}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script, err := filepath.Abs("gt2-test-guard.sh")
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", ". "+script+"; assert_not_gt2_heavy_run")
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, tt.env...)
			raw, err := cmd.CombinedOutput()
			out := string(raw)
			if tt.blocked {
				if err == nil || !strings.Contains(out, "gt2-test-guard") {
					t.Fatalf("expected refusal, got err=%v out=%q", err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected pass, got err=%v out=%q", err, out)
			}
		})
	}
}

// The runner must actually call the guard: refusing on GT2 before any work.
func TestTestLocalParallelCallsGT2Guard(t *testing.T) {
	b, err := os.ReadFile("test-local-parallel")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "assert_not_gt2_heavy_run") {
		t.Fatal("scripts/test-local-parallel does not call assert_not_gt2_heavy_run")
	}
}
