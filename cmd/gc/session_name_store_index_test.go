package main

import (
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// listCountingStore counts every List the resolve path issues against the
// store. The count is the deterministic budget for gcf-eln5: it must not grow
// with the number of names that miss the session snapshot.
type listCountingStore struct {
	beads.Store
	mu    sync.Mutex
	lists int
}

func (s *listCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.Store.List(q)
}

func (s *listCountingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

func nameIndexFixture(t *testing.T, n int) (*listCountingStore, []string) {
	t.Helper()
	mem := beads.NewMemStore()
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("rig%d/agent%d", i%7, i)
		names = append(names, name)
		switch {
		case i%5 == 0: // open bead keyed by agent_name
			if _, err := mem.Create(beads.Bead{
				Title: name, Type: sessionpkg.BeadType, Labels: []string{sessionpkg.LabelSession},
				Metadata: map[string]string{"agent_name": name, "template": name, "session_name": "s-" + fmt.Sprint(i)},
			}); err != nil {
				t.Fatal(err)
			}
		case i%5 == 1: // open bead known only by template
			if _, err := mem.Create(beads.Bead{
				Title: name, Type: sessionpkg.BeadType, Labels: []string{sessionpkg.LabelSession},
				Metadata: map[string]string{"template": name, "session_name": "s-t-" + fmt.Sprint(i)},
			}); err != nil {
				t.Fatal(err)
			}
		}
		// the rest have no session bead: they must fall to the legacy name
	}
	return &listCountingStore{Store: mem}, names
}

func newNameParams(store beads.Store) *agentBuildParams {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	return newAgentBuildParams("test-city", t0Dir(), cfg, runtime.NewFake(), time.Now(), store, io.Discard)
}

func t0Dir() string { return "/tmp/test-city" }

// TestResolveSessionNameStoreFallbackBudgetDoesNotScaleWithNames pins that the
// store fallback issues a constant number of lists for 1, 50 and 200 names
// that miss the session snapshot, and that every name resolves exactly as the
// per-name scan resolved it.
func TestResolveSessionNameStoreFallbackBudgetDoesNotScaleWithNames(t *testing.T) {
	var counts []int
	for _, n := range []int{1, 50, 200} {
		store, names := nameIndexFixture(t, n)
		p := newNameParams(store)
		// nil sessionBeads: every name misses the tick snapshot and reaches
		// the store fallback, the path this change replaces.
		got := make(map[string]string, n)
		for _, name := range names {
			got[name] = p.resolveSessionName(name, "")
		}
		counts = append(counts, store.count())

		// Golden: the pre-change per-name store scan, then the legacy name.
		for _, name := range names {
			want := findSessionNameByTemplate(store.Store, name)
			if want == "" {
				want = agent.SessionNameFor("test-city", name, "")
			}
			if got[name] != want {
				t.Errorf("n=%d name %q resolved %q, want %q", n, name, got[name], want)
			}
		}
	}
	if counts[0] > 4 || counts[1] != counts[0] || counts[2] != counts[0] {
		t.Fatalf("store lists for 1/50/200 names = %v, want a small constant independent of names", counts)
	}
}
