package dispatch

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// wrapPersistFailure/isPersistFailureError are the classification seam the
// retry engine keys off. These tests pin the contract: nil stays nil, tagging
// is idempotent, the wrap is transparent to errors.As/Unwrap through %w chains,
// and — critically — a partialAttemptAttachError is NOT a persist failure (it
// must stay hard so a partial molecule attach is never re-attached).

func TestWrapPersistFailureNilStaysNil(t *testing.T) {
	t.Parallel()
	if got := wrapPersistFailure(nil); got != nil {
		t.Fatalf("wrapPersistFailure(nil) = %v, want nil", got)
	}
}

func TestWrapPersistFailureTagsAndIsDetected(t *testing.T) {
	t.Parallel()
	base := errors.New("store update failed")
	wrapped := wrapPersistFailure(base)
	if wrapped == nil {
		t.Fatal("wrapPersistFailure returned nil for a non-nil error")
	}
	if !isPersistFailureError(wrapped) {
		t.Fatalf("isPersistFailureError(wrapped) = false, want true")
	}
	if !errors.Is(wrapped, base) {
		t.Fatalf("wrapped error does not unwrap to its base error")
	}
}

func TestWrapPersistFailureIsIdempotent(t *testing.T) {
	t.Parallel()
	base := errors.New("close failed")
	once := wrapPersistFailure(base)
	twice := wrapPersistFailure(once)
	if once != twice {
		t.Fatalf("wrapPersistFailure is not idempotent: once=%p twice=%p", once, twice)
	}
}

func TestIsPersistFailureErrorMatchesThroughFmtChain(t *testing.T) {
	t.Parallel()
	// A persist failure wrapped again by a caller's fmt.Errorf %w must still
	// classify as persist (errors.As walks the whole chain).
	inner := wrapPersistFailure(errors.New("close verify failed"))
	outer := fmt.Errorf("verifying close of gp-1: %w", inner)
	if !isPersistFailureError(outer) {
		t.Fatalf("isPersistFailureError did not see through an fmt.Errorf chain")
	}
}

func TestIsPersistFailureErrorRejectsPartialAttach(t *testing.T) {
	t.Parallel()
	// A partial molecule attach must stay hard, never persist — re-attaching a
	// duplicate root is exactly what the persist class must not trigger.
	partial := &partialAttemptAttachError{rootID: "gp-root", err: errors.New("molecule_failed")}
	if isPersistFailureError(partial) {
		t.Fatalf("partialAttemptAttachError misclassified as a persist failure")
	}
	if isPersistFailureError(errors.New("plain error")) {
		t.Fatalf("plain error misclassified as a persist failure")
	}
}

// classifyRetryAttempt is the consumer side: a fail attempt tagged
// gc.failure_class=persist must retry (outcome "transient") with the explicit
// persist_failure reason, not ride the hard/unknown default branches.
func TestClassifyRetryAttemptPersistClassRetries(t *testing.T) {
	t.Parallel()
	got := classifyRetryAttempt(beads.Bead{
		Metadata: map[string]string{
			beadmeta.OutcomeMetadataKey:      beadmeta.OutcomeFail,
			beadmeta.FailureClassMetadataKey: beadmeta.FailureClassPersist,
		},
	})
	want := retryEvalResult{Outcome: "transient", Reason: "persist_failure"}
	if got != want {
		t.Fatalf("classifyRetryAttempt(persist) = %+v, want %+v", got, want)
	}
}

// markControllerSpawnError must record a persist-class controller error as
// retryable and leave the bead OPEN — the completed work behind it must not be
// discarded by a hard close.
func TestMarkControllerSpawnErrorPersistIsRetryableAndDoesNotClose(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	bead := mustCreate(t, store, beads.Bead{Title: "attempt", Metadata: map[string]string{}})

	retryable := markControllerSpawnError(store, bead.ID, wrapPersistFailure(errors.New("store close failed")), ProcessOptions{})
	if !retryable {
		t.Fatalf("markControllerSpawnError(persist) = false, want true (retryable)")
	}

	after := mustGet(t, store, bead.ID)
	if got := after.Metadata[beadmeta.ControllerErrorClassMetadataKey]; got != beadmeta.FailureClassPersist {
		t.Fatalf("controller error class = %q, want %q", got, beadmeta.FailureClassPersist)
	}
	if got := after.Metadata[beadmeta.ControllerRetryableMetadataKey]; got != "true" {
		t.Fatalf("controller retryable = %q, want \"true\"", got)
	}
	if after.Status == "closed" {
		t.Fatalf("bead was closed on a persist failure; completed work would be discarded")
	}
	if got := after.Metadata[beadmeta.FinalDispositionMetadataKey]; got != "" {
		t.Fatalf("final disposition = %q, want empty (persist is not terminal)", got)
	}
}

// updateMetadataAndClose tags every error it returns as a persist failure: the
// outcome is already decided, so a store-side failure means only recording it
// failed, never the work.
func TestUpdateMetadataAndCloseWrapsStoreErrorAsPersist(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("backend unavailable")
	store := &updateFailStore{Store: beads.NewMemStore(), err: sentinel}

	err := updateMetadataAndClose(store, "gp-any", map[string]string{"gc.outcome": "pass"})
	if err == nil {
		t.Fatal("updateMetadataAndClose returned nil, want a persist failure")
	}
	if !isPersistFailureError(err) {
		t.Fatalf("updateMetadataAndClose error not classified as persist: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("updateMetadataAndClose error does not unwrap to the store error")
	}
}

func TestUpdateMetadataAndCloseSuccessReturnsNil(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	bead := mustCreate(t, store, beads.Bead{Title: "work", Metadata: map[string]string{}})

	if err := updateMetadataAndClose(store, bead.ID, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatalf("updateMetadataAndClose on a healthy store = %v, want nil", err)
	}
	if after := mustGet(t, store, bead.ID); after.Status != "closed" {
		t.Fatalf("bead status = %q, want closed", after.Status)
	}
}

// updateFailStore makes every Update fail, exercising updateMetadataAndClose's
// first (Update) branch.
type updateFailStore struct {
	beads.Store
	err error
}

func (s *updateFailStore) Update(string, beads.UpdateOpts) error {
	return s.err
}
