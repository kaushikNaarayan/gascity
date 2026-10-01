package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gastownhall/gascity/internal/citylayout"
)

var nudgeUnconfirmedReadDir = os.ReadDir

// NudgeUnconfirmedCheck surfaces private artifacts left when a nudge was sent
// but delivery could not be confirmed safely.
type NudgeUnconfirmedCheck struct{}

// NewNudgeUnconfirmedCheck creates the delivery-diagnostic check.
func NewNudgeUnconfirmedCheck() *NudgeUnconfirmedCheck { return &NudgeUnconfirmedCheck{} }

// Name returns the check's stable identifier.
func (c *NudgeUnconfirmedCheck) Name() string { return "nudge-unconfirmed" }

// CanFix reports that artifacts require operator inspection rather than repair.
func (c *NudgeUnconfirmedCheck) CanFix() bool { return false }

// Fix implements Check without altering diagnostic artifacts.
func (c *NudgeUnconfirmedCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible reports that this diagnostic is not a warmup check.
func (c *NudgeUnconfirmedCheck) WarmupEligible() bool { return false }

// Run reports unconfirmed nudge-delivery artifacts to the operator.
func (c *NudgeUnconfirmedCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	root := citylayout.SessionDiagnosticsDir(ctx.CityPath)
	entries, err := nudgeUnconfirmedReadDir(root)
	if os.IsNotExist(err) {
		r.Status = StatusOK
		r.Message = "no session runtime directory; nothing to check"
		return r
	}
	if err != nil {
		r.Status = StatusError
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("cannot read session diagnostic directory %s: %v", root, err)
		return r
	}
	files := []string{"nudge-unconfirmed.log", "startup-nudge-unconfirmed.log"}
	var details []string
	sessions := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			details = append(details, fmt.Sprintf("unexpected non-directory entry %q under %s", e.Name(), root))
			continue
		}
		for _, file := range files {
			path := filepath.Join(root, e.Name(), file)
			if _, err := os.Stat(path); err == nil {
				details = append(details, fmt.Sprintf("session %q: %s", e.Name(), file))
				sessions[e.Name()] = true
			} else if !os.IsNotExist(err) {
				r.Status = StatusError
				r.Severity = SeverityAdvisory
				r.Message = fmt.Sprintf("cannot inspect session diagnostic %s: %v", path, err)
				return r
			}
		}
	}
	if len(details) == 0 {
		r.Status = StatusOK
		r.Message = "no unconfirmed nudge deliveries"
		return r
	}
	sort.Strings(details)
	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	if len(sessions) == 0 {
		r.Message = "unexpected entries in session diagnostic directory"
	} else {
		r.Message = fmt.Sprintf("%d session(s) have an unconfirmed nudge delivery", len(sessions))
	}
	r.Details = details
	return r
}
