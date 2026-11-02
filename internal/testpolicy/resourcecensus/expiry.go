package resourcecensus

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ExpiryWarnDays is how far ahead the census test warns about a waiver or
// baseline that is about to lapse. Entries that already expired fail Validate;
// this exists so the date is never a surprise (gcf-zemh4: three date-bombs).
const ExpiryWarnDays = 14

// ExpiringEntry is one dated ledger row that lapses within the warning window.
type ExpiringEntry struct {
	Kind     string
	Name     string
	Expires  string
	DaysLeft int
	Owner    string
}

// ExpiringEntries returns every dated ledger row (audit baseline, debt, small
// debt, medium owner) that expires within windowDays of now, including today.
// Rows that already expired are not returned: Validate reports those as errors.
// Rows with an unparsable date are skipped here because Validate rejects them.
func ExpiringEntries(ledger Ledger, now time.Time, windowDays int) []ExpiringEntry {
	today := day(now)
	var out []ExpiringEntry
	add := func(kind, name, expires, owner string) {
		expiry, err := time.Parse("2006-01-02", expires)
		if err != nil {
			return
		}
		left := int(expiry.Sub(today).Hours() / 24)
		if left < 0 || left > windowDays {
			return
		}
		out = append(out, ExpiringEntry{Kind: kind, Name: name, Expires: expires, DaysLeft: left, Owner: owner})
	}
	baselines := func(kind string, rows []Baseline) {
		for _, b := range rows {
			add(kind, renderedSourceScope(b.Scope)+" "+string(b.Resource), b.Expires, b.OwnerBead)
		}
	}
	baselines("audit_baseline", ledger.AuditBaseline)
	baselines("debt", ledger.Debt)
	baselines("small_debt", ledger.SmallDebt)
	for _, m := range ledger.Medium {
		add("medium", m.PackageDir+" "+m.Owner, m.Expires, m.OwnerBead)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Expires != out[j].Expires {
			return out[i].Expires < out[j].Expires
		}
		return out[i].Kind+out[i].Name < out[j].Kind+out[j].Name
	})
	return out
}

// FormatExpiryWarning renders the entries for t.Log and CI annotations. It
// returns "" when nothing is expiring.
func FormatExpiryWarning(entries []ExpiringEntry, windowDays int) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d test-policy waiver/baseline entries expire within %d days; renew or retire them before the gate goes red:\n", len(entries), windowDays)
	for _, e := range entries {
		fmt.Fprintf(&b, "  %s  (%d d)  %s: %s  [%s]\n", e.Expires, e.DaysLeft, e.Kind, e.Name, e.Owner)
	}
	return b.String()
}
