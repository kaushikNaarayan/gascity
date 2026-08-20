package dispatch

import (
	"errors"
	"fmt"
)

// persistFailureError marks a failure where the work (or a terminal decision
// about it) already succeeded and only the store-side persist that records it
// — the metadata update or the close in updateMetadataAndClose — failed. It is
// deliberately distinct from a transient spawn/query error (the attempt has not
// re-run; it completed) and from a hard failure (the completed result must not
// be discarded). Callers classify it with beadmeta.FailureClassPersist so the
// retry engine retries only the persist step instead of re-running the attempt
// or giving up. See internal/beadmeta.FailureClassPersist for the vocabulary.
//
// Note: partialAttemptAttachError is a SEPARATE type and is not a persist
// failure — a partial molecule attach must stay hard to avoid re-attaching a
// duplicate molecule, so isPersistFailureError never matches it.
type persistFailureError struct {
	err error
}

func (e *persistFailureError) Error() string {
	return fmt.Sprintf("persist failure (work complete, recording it failed): %v", e.err)
}

func (e *persistFailureError) Unwrap() error {
	return e.err
}

// wrapPersistFailure tags err as a persist-only failure. It returns nil for a
// nil error and is idempotent — wrapping an already-tagged error is a no-op so
// nested close helpers do not double-wrap.
func wrapPersistFailure(err error) error {
	if err == nil || isPersistFailureError(err) {
		return err
	}
	return &persistFailureError{err: err}
}

// isPersistFailureError reports whether err (or anything it wraps) is a
// persist-only failure.
func isPersistFailureError(err error) bool {
	var persist *persistFailureError
	return errors.As(err, &persist)
}
