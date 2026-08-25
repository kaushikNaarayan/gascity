package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestTaskCacheRunCleansOwnedRootOnExit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("task cache ownership uses Linux /proc process identity")
	}
	parent := t.TempDir()
	command := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "task-cache-run"), "--owner", "qa", "--bead", "gcf-vtw7", "--session", "session-a", "--", "bash", "-c", "test -f \"$GC_TASK_CACHE_ROOT/.gc-task-cache\"; test \"$GOCACHE\" = \"$GC_TASK_CACHE_ROOT/gocache\"; test \"$GOTMPDIR\" = \"$GC_TASK_CACHE_ROOT/gotmp\"")
	command.Env = append(os.Environ(), "GC_TASK_CACHE_PARENT="+parent)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("task-cache-run: %v\n%s", err, output)
	}

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read cache parent: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("cache roots left after successful command: %v", entries)
	}
}

func TestTaskCacheRunCleansOwnedRootOnTermination(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("task cache ownership uses Linux /proc process identity")
	}
	parent := t.TempDir()
	command := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "task-cache-run"), "--owner", "qa", "--bead", "gcf-vtw7", "--session", "session-a", "--", "bash", "-c", "kill -TERM \"$PPID\"; wait")
	command.Env = append(os.Environ(), "GC_TASK_CACHE_PARENT="+parent)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("task-cache-run unexpectedly succeeded after TERM:\n%s", output)
	}

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read cache parent: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("cache roots left after termination: %v", entries)
	}
}

func TestTaskCacheReaperPreservesLiveAndReapsDeadOwnedRoots(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("task cache ownership uses Linux /proc process identity")
	}
	parent := t.TempDir()
	live := filepath.Join(parent, "gc-task-cache-live")
	dead := filepath.Join(parent, "gc-task-cache-dead")
	for _, root := range []string{live, dead} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatalf("create cache root %q: %v", root, err)
		}
	}

	startTicks := procStartTicks(t, os.Getpid())
	writeTaskCacheMetadata(t, live, os.Getpid(), startTicks)
	writeTaskCacheMetadata(t, dead, 99999999, "1")

	command := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "task-cache-reap"), "--root", parent, "--min-age", "0", "--delete")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("task-cache-reap: %v\n%s", err, output)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live root was not preserved: %v", err)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("dead root still exists or could not be inspected: %v", err)
	}
}

func procStartTicks(t *testing.T, pid int) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		t.Fatalf("read process start ticks: %v", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 22 {
		t.Fatalf("unexpected /proc stat: %q", data)
	}
	return fields[21]
}

func writeTaskCacheMetadata(t *testing.T, root string, pid int, startTicks string) {
	t.Helper()
	metadata := "version=1\nowner=qa\nbead=gcf-vtw7\nsession=session-a\npid=" + strconv.Itoa(pid) + "\npid_start_ticks=" + startTicks + "\ncreated_unix=1\n"
	if err := os.WriteFile(filepath.Join(root, ".gc-task-cache"), []byte(metadata), 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
}
