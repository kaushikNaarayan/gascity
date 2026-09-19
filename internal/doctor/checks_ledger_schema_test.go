package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// unreadableLedgerStore models a v59 ledger opened by a gc whose bd expects
// v66: the connection is healthy (Ping succeeds) but any schema-gated read
// fails. The error strings are the ones observed by qa's G-UPGRADE run.
type unreadableLedgerStore struct {
	beads.MemStore
	listErr error
	listed  int
}

func (s *unreadableLedgerStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.listed++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.MemStore.List(q)
}

var (
	errLedgerLeases  = errors.New(`bd list: table not found: leases`)
	errLedgerColumns = errors.New(`bd list: table "i" does not have column "storage_class"`)
)

func newLedgerCheck(t *testing.T, s beads.Store) *BeadsStoreCheck {
	t.Helper()
	return NewBeadsStoreCheck(t.TempDir(), func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: s}, nil
	})
}

// gcf-tf22: doctor was GREEN ("store accessible") on a ledger the pinned bd
// cannot read, because Ping never touches the schema-gated read path.
func TestBeadsStoreCheck_RedOnUnmigratedLedger(t *testing.T) {
	for _, e := range []error{errLedgerLeases, errLedgerColumns} {
		s := &unreadableLedgerStore{listErr: e}
		r := newLedgerCheck(t, s).Run(&CheckContext{})
		if r.Status != StatusError {
			t.Fatalf("%v: status = %d, want Error (false green); msg = %q", e, r.Status, r.Message)
		}
		for _, want := range []string{"founder-gated", "migration"} {
			if !strings.Contains(r.Message+r.FixHint, want) {
				t.Errorf("%v: result must mention %q; msg=%q hint=%q", e, want, r.Message, r.FixHint)
			}
		}
		if !strings.Contains(r.FixHint, "bd-v59-to-v66-fleet-migration.md") {
			t.Errorf("fix hint must name the runbook: %q", r.FixHint)
		}
		if strings.Contains(r.FixHint, "--force") {
			t.Errorf("hint must not hand out the migrate command: %q", r.FixHint)
		}
	}
}

// Negative controls: a readable ledger stays green, and a read failure that is
// NOT the schema signature is surfaced but not misreported as the migration.
func TestBeadsStoreCheck_ReadableLedgerStaysGreen(t *testing.T) {
	s := &unreadableLedgerStore{}
	r := newLedgerCheck(t, s).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("readable ledger: status = %d, want OK; msg = %q", r.Status, r.Message)
	}
	if s.listed == 0 {
		t.Fatal("the schema-gated read probe did not run")
	}
}

func TestBeadsStoreCheck_OtherReadFailureWarnsWithoutMigrationClaim(t *testing.T) {
	s := &unreadableLedgerStore{listErr: errors.New("i/o timeout")}
	r := newLedgerCheck(t, s).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %q", r.Status, r.Message)
	}
	if strings.Contains(r.Message+r.FixHint, "founder-gated") {
		t.Errorf("non-schema failure must not claim the migration: %q / %q", r.Message, r.FixHint)
	}
}

func newRigLedgerCheck(t *testing.T, s beads.Store) *RigBeadsCheck {
	t.Helper()
	dir := t.TempDir()
	return NewRigBeadsCheck(dir, config.Rig{Name: "gymbo", Path: dir}, func(string) (beads.Store, error) {
		return s, nil
	})
}

// The rig stores were the unreadable ones in qa's G-UPGRADE run, and the
// per-rig check (Ping only) stayed green with them.
func TestRigBeadsCheck_RedOnUnmigratedLedger(t *testing.T) {
	for _, e := range []error{errLedgerLeases, errLedgerColumns} {
		r := newRigLedgerCheck(t, &unreadableLedgerStore{listErr: e}).Run(&CheckContext{})
		if r.Status != StatusError {
			t.Fatalf("%v: status = %d, want Error (false green); msg = %q", e, r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "founder-gated") || !strings.Contains(r.Message, "gymbo") {
			t.Errorf("%v: message must name the rig and the founder-gated migration: %q", e, r.Message)
		}
		if !strings.Contains(r.FixHint, "bd-v59-to-v66-fleet-migration.md") || strings.Contains(r.FixHint, "--force") {
			t.Errorf("%v: bad fix hint: %q", e, r.FixHint)
		}
	}
}

func TestRigBeadsCheck_ReadableLedgerStaysGreenAndOtherFailureWarns(t *testing.T) {
	if r := newRigLedgerCheck(t, &unreadableLedgerStore{}).Run(&CheckContext{}); r.Status != StatusOK {
		t.Fatalf("readable rig ledger: status = %d, want OK; msg = %q", r.Status, r.Message)
	}
	r := newRigLedgerCheck(t, &unreadableLedgerStore{listErr: errors.New("i/o timeout")}).Run(&CheckContext{})
	if r.Status != StatusWarning || strings.Contains(r.Message+r.FixHint, "founder-gated") {
		t.Fatalf("non-schema failure: status = %d msg = %q hint = %q", r.Status, r.Message, r.FixHint)
	}
}

func TestIsLedgerSchemaMismatch(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errLedgerLeases, true},
		{errLedgerColumns, true},
		{errors.New("i/o timeout"), false},
		{errors.New("connection refused"), false},
		{errors.New("column widget not found in query"), false},
		{nil, false},
	} {
		if got := IsLedgerSchemaMismatch(tc.err); got != tc.want {
			t.Errorf("IsLedgerSchemaMismatch(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
