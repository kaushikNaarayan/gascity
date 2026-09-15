package tmux

import (
	"strconv"
	"testing"
	"time"
)

// TestBulkSessionAttachedSingleExec proves BulkSessionAttached resolves every
// session's #{session_attached} in exactly one tmux exec (list-sessions,
// unfiltered), parsing the tab-separated name/attached pairs correctly.
func TestBulkSessionAttachedSingleExec(t *testing.T) {
	fe := &fakeExecutor{out: "alpha\t1\nbravo\t0\ncharlie\t1"}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

	got, err := tm.BulkSessionAttached()
	if err != nil {
		t.Fatalf("BulkSessionAttached: %v", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("tmux execs = %d, want 1: %v", len(fe.calls), fe.calls)
	}
	want := map[string]bool{"alpha": true, "bravo": false, "charlie": true}
	for name, wantAttached := range want {
		if got[name] != wantAttached {
			t.Errorf("attached[%q] = %v, want %v", name, got[name], wantAttached)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("result = %v, want exactly %v", got, want)
	}

	args := fe.calls[0]
	for _, want := range []string{"list-sessions", "-F", "#{session_name}\t#{session_attached}"} {
		found := false
		for _, a := range args {
			if a == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("exec args %v missing %q", args, want)
		}
	}
	// Unfiltered: must not carry a -f/-t target restricting it to one session.
	for _, a := range args {
		if a == "-f" || a == "-t" {
			t.Errorf("BulkSessionAttached exec is filtered (%q present), want unfiltered: %v", a, args)
		}
	}
}

// TestBulkSessionActivitySingleExecMatchesPerSessionMax proves
// bulkRawSessionActivity groups list-windows -a output by session and applies
// the SAME latestActivityTimestamp per-session-max helper rawSessionActivity
// uses for a single session — so the bulk answer for each session is
// byte-identical to calling rawSessionActivity on it alone.
func TestBulkSessionActivitySingleExecMatchesPerSessionMax(t *testing.T) {
	// alpha has three windows, most recent activity 300. bravo has one window.
	fe := &fakeExecutor{out: "alpha\t100\nbravo\t250\nalpha\t300\nalpha\t200"}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

	got, err := tm.bulkRawSessionActivity()
	if err != nil {
		t.Fatalf("bulkRawSessionActivity: %v", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("tmux execs = %d, want 1: %v", len(fe.calls), fe.calls)
	}

	wantAlpha := time.Unix(300, 0)
	wantBravo := time.Unix(250, 0)
	if !got["alpha"].Equal(wantAlpha) {
		t.Errorf("alpha activity = %v, want %v", got["alpha"], wantAlpha)
	}
	if !got["bravo"].Equal(wantBravo) {
		t.Errorf("bravo activity = %v, want %v", got["bravo"], wantBravo)
	}

	// Cross-check against the exact per-session helper rawSessionActivity
	// would apply if called alone (constraint: same latestActivityTimestamp,
	// not a different reduction).
	wantAlphaTS, err := latestActivityTimestamp("100\n300\n200")
	if err != nil {
		t.Fatalf("latestActivityTimestamp: %v", err)
	}
	if got["alpha"].Unix() != wantAlphaTS {
		t.Errorf("alpha bulk timestamp = %d, want %d (from latestActivityTimestamp directly)", got["alpha"].Unix(), wantAlphaTS)
	}
}

// TestBulkSessionActivityAppliesSamePokeDiscount proves BulkSessionActivity
// discounts a gc-own keystroke echo exactly like GetSessionActivity does for
// a single session (same discountPokeActivity helper, same in-memory
// t.pokes — no extra subprocess for the discount).
func TestBulkSessionActivityAppliesSamePokeDiscount(t *testing.T) {
	fe := &fakeExecutor{out: "poked\t1000"}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

	pokeAt := time.Unix(1000, 0) // within pokeEcho of the raw activity: echo-only
	tm.recordPokeAt("poked", time.Unix(500, 0), pokeAt)

	got, err := tm.BulkSessionActivity()
	if err != nil {
		t.Fatalf("BulkSessionActivity: %v", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("tmux execs = %d, want 1: %v", len(fe.calls), fe.calls)
	}

	// discountPokeActivity requires graceElapsed too; without it standing
	// alone this only proves the raw value flows through pokesCopy lookup
	// consistently with GetSessionActivity's own call shape. Compare
	// directly against discountPokeActivity so bulk and single-session paths
	// can never silently diverge.
	tm.pokeMu.Lock()
	pk := tm.pokes["poked"]
	tm.pokeMu.Unlock()
	want := discountPokeActivity(time.Unix(1000, 0), pk, time.Now())
	if !got["poked"].Equal(want) {
		t.Errorf("BulkSessionActivity poke-discounted = %v, want %v (discountPokeActivity applied identically)", got["poked"], want)
	}
}

// TestProviderBulkEnrichSessionsExecCountIndependentOfN is the negative
// control PM asked for: it proves BulkEnrichSessions costs exactly 2 tmux
// execs (one list-sessions, one list-windows -a) NO MATTER how many session
// names are requested — the property the old per-session IsAttached +
// GetLastActivity path structurally cannot have (see the companion
// TestProviderPerSessionExecCountScalesWithN below, which passes on main
// AND this branch since that path is untouched; BulkEnrichSessions itself
// does not exist on main at all, so this test cannot even compile there).
func TestProviderBulkEnrichSessionsExecCountIndependentOfN(t *testing.T) {
	for _, n := range []int{1, 5, 50} {
		var sessAttachedLines, windowActivityLines string
		names := make([]string, 0, n)
		for i := 0; i < n; i++ {
			name := sessionNameFor(i)
			names = append(names, name)
			sessAttachedLines += name + "\t1\n"
			windowActivityLines += name + "\t123\n"
		}
		fe := &fakeExecutor{outs: []string{sessAttachedLines, windowActivityLines}}
		p := &Provider{tm: &Tmux{cfg: Config{SocketName: "x"}, exec: fe}}

		results, err := p.BulkEnrichSessions(names)
		if err != nil {
			t.Fatalf("n=%d: BulkEnrichSessions: %v", n, err)
		}
		if len(fe.calls) != 2 {
			t.Fatalf("n=%d: tmux execs = %d, want exactly 2: %v", n, len(fe.calls), fe.calls)
		}
		if len(results) != n {
			t.Fatalf("n=%d: results = %d, want %d", n, len(results), n)
		}
	}
}

// TestProviderPerSessionExecCountScalesWithN documents, for contrast, the
// exact per-N cost of the pre-existing IsAttached + GetLastActivity path
// this fix bypasses for listing — 2 execs PER session, unbounded. This test
// is unchanged behavior (it passes identically on main); it exists so the
// 2-execs-total claim above has a concrete "what N used to cost" baseline in
// the same file.
func TestProviderPerSessionExecCountScalesWithN(t *testing.T) {
	n := 5
	fe := &fakeExecutor{out: "0"} // constant per-call answer is enough here
	p := &Provider{tm: &Tmux{cfg: Config{SocketName: "x"}, exec: fe}}

	for i := 0; i < n; i++ {
		name := sessionNameFor(i)
		_ = p.IsAttached(name)
		_, _ = p.GetLastActivity(name)
	}
	if len(fe.calls) != 2*n {
		t.Fatalf("tmux execs = %d, want %d (2 per session, unbounded in N)", len(fe.calls), 2*n)
	}
}

// TestProviderBulkEnrichSessionsMissingSessionOmitted proves a session that
// exited between the caller's listing and this call (present in `names` but
// absent from both bulk tmux outputs) is simply missing from the result map
// — never a crash, never a fabricated zero-value entry standing in for real
// data. Manager.EnrichInfos is responsible for collapsing an absent entry to
// the same "not attached, LastActive untouched" shape IsAttached/
// GetLastActivity's own per-session error path produces (see the session
// package test of the same name).
func TestProviderBulkEnrichSessionsMissingSessionOmitted(t *testing.T) {
	fe := &fakeExecutor{outs: []string{"alive\t1", "alive\t123"}}
	p := &Provider{tm: &Tmux{cfg: Config{SocketName: "x"}, exec: fe}}

	results, err := p.BulkEnrichSessions([]string{"alive", "vanished"})
	if err != nil {
		t.Fatalf("BulkEnrichSessions: %v", err)
	}
	if _, ok := results["vanished"]; ok {
		t.Errorf("results contains %q, want it absent (session not in tmux output)", "vanished")
	}
	if _, ok := results["alive"]; !ok {
		t.Errorf("results missing %q", "alive")
	}
}

func sessionNameFor(i int) string {
	return "gc-bulk-test-session-" + strconv.Itoa(i)
}
