package tmux

import (
	"errors"
	"testing"
	"time"
)

func showEnvironmentCalls(f *fakeExecutor) int {
	n := 0
	for _, call := range f.calls {
		for _, a := range call {
			if a == "show-environment" {
				n++
				break
			}
		}
	}
	return n
}

func codexFetcher(pid string) *mockFetcher {
	return &mockFetcher{
		state: runtimeStateSnapshot{
			Sessions: map[string]sessionRuntimeState{
				"agent-1": {Running: true, Panes: []paneRuntimeState{{Command: "bash", PID: pid}}},
			},
			Processes: newProcessSnapshot([]processRuntimeState{
				{PID: pid, PPID: "1", Command: "bash", Args: "bash -lc codex"},
				{PID: pid + "1", PPID: pid, Command: "node", Args: "node /usr/local/bin/codex"},
			}),
			ProcessesAvailable: true,
		},
	}
}

// GT_PROCESS_NAMES is set once at session creation, so the liveness path must
// not spawn `tmux show-environment` for every session on every observation.
func TestObserveLivenessReadsProcessNamesOncePerPane(t *testing.T) {
	exec := &fakeExecutor{out: "GT_PROCESS_NAMES=codex"}
	p := &Provider{
		tm:    &Tmux{cfg: DefaultConfig(), exec: exec},
		cache: NewStateCache(codexFetcher("101"), time.Hour),
	}

	for i := 0; i < 5; i++ {
		got := p.ObserveLiveness("agent-1", nil)
		if !got.Running || !got.Alive {
			t.Fatalf("observation %d = %+v, want running and alive", i, got)
		}
	}
	if n := showEnvironmentCalls(exec); n != 1 {
		t.Fatalf("show-environment calls = %d, want 1 across 5 observations", n)
	}
}

// A restarted session gets a new pane PID and may carry different process
// names, so the cached value must not survive it.
func TestObserveLivenessRereadsProcessNamesWhenPaneRestarts(t *testing.T) {
	exec := &fakeExecutor{out: "GT_PROCESS_NAMES=codex"}
	fetcher := codexFetcher("101")
	p := &Provider{
		tm:    &Tmux{cfg: DefaultConfig(), exec: exec},
		cache: NewStateCache(fetcher, time.Hour),
	}
	p.ObserveLiveness("agent-1", nil)

	restarted := codexFetcher("202")
	fetcher.mu.Lock()
	fetcher.state = restarted.state
	fetcher.mu.Unlock()
	p.cache.Invalidate()

	got := p.ObserveLiveness("agent-1", nil)
	if !got.Alive {
		t.Fatalf("after restart = %+v, want alive", got)
	}
	if n := showEnvironmentCalls(exec); n != 2 {
		t.Fatalf("show-environment calls = %d, want 2 (re-read after pane PID changed)", n)
	}
}

// A failed lookup says nothing about the session, so it must be retried
// rather than remembered as "no process names".
func TestObserveLivenessDoesNotCacheProcessNameLookupErrors(t *testing.T) {
	exec := &fakeExecutor{
		outs: []string{"", "GT_PROCESS_NAMES=codex"},
		errs: []error{errors.New("tmux: transient"), nil},
	}
	p := &Provider{
		tm:    &Tmux{cfg: DefaultConfig(), exec: exec},
		cache: NewStateCache(codexFetcher("101"), time.Hour),
	}

	first := p.ObserveLiveness("agent-1", nil)
	if !first.Running || !first.Alive {
		t.Fatalf("lookup error = %+v, want running+alive (no names, no process check)", first)
	}
	exec.out = "GT_PROCESS_NAMES=codex"
	second := p.ObserveLiveness("agent-1", nil)
	if !second.Alive {
		t.Fatalf("after retry = %+v, want alive", second)
	}
	if n := showEnvironmentCalls(exec); n != 2 {
		t.Fatalf("show-environment calls = %d, want 2 (error not cached)", n)
	}
}
