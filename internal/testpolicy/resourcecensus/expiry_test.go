package resourcecensus

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func expiryFixture(now time.Time) Ledger {
	d := func(n int) string { return day(now).AddDate(0, 0, n).Format("2006-01-02") }
	return Ledger{
		AuditBaseline: []Baseline{
			{Scope: ScopeAll, Resource: ResourceSubprocess, OwnerBead: "b-plus10", Expires: d(10)},
			{Scope: ScopeAll, Resource: ResourceFixedSleep, OwnerBead: "b-plus30", Expires: d(30)},
		},
		SmallDebt: []Baseline{
			{Scope: ScopeAll, Resource: ResourceNetListen, OwnerBead: "b-today", Expires: d(0)},
			{Scope: ScopeAll, Resource: ResourceCWD, OwnerBead: "b-plus14", Expires: d(14)},
			{Scope: ScopeAll, Resource: ResourceEnvironment, OwnerBead: "b-plus15", Expires: d(15)},
		},
		Debt: []Baseline{
			{Scope: ScopeAll, Resource: ResourceTmux, OwnerBead: "b-expired", Expires: d(-1)},
		},
		Medium: []MediumOwner{
			{PackageDir: "pkg/a", Owner: "TestA", OwnerBead: "b-medium7", Expires: d(7)},
			{PackageDir: "pkg/b", Owner: "TestB", OwnerBead: "b-bad", Expires: "not-a-date"},
		},
	}
}

// Negative control from gcf-zemh4: today+10 must appear, today+30 must not.
func TestExpiringEntriesWindow(t *testing.T) {
	now := time.Date(2026, 11, 2, 15, 30, 0, 0, time.UTC)
	got := ExpiringEntries(expiryFixture(now), now, ExpiryWarnDays)
	var owners []string
	for _, e := range got {
		owners = append(owners, e.Owner)
	}
	want := []string{"b-today", "b-medium7", "b-plus10", "b-plus14"}
	if strings.Join(owners, ",") != strings.Join(want, ",") {
		t.Fatalf("expiring owners = %v, want %v (sorted by date; +15, +30, expired and malformed excluded)", owners, want)
	}
	if got[0].DaysLeft != 0 || got[3].DaysLeft != 14 {
		t.Fatalf("days left = %d..%d, want 0..14", got[0].DaysLeft, got[3].DaysLeft)
	}
}

func TestExpiringEntriesIgnoresTimeOfDay(t *testing.T) {
	late := time.Date(2026, 11, 2, 23, 59, 59, 0, time.UTC)
	l := Ledger{SmallDebt: []Baseline{{Scope: ScopeAll, Resource: ResourceFixedSleep, OwnerBead: "x", Expires: "2026-11-16"}}}
	if got := ExpiringEntries(l, late, 14); len(got) != 1 || got[0].DaysLeft != 14 {
		t.Fatalf("got %+v, want one entry at 14 days", got)
	}
	if got := ExpiringEntries(l, late.AddDate(0, 0, -1), 14); len(got) != 0 {
		t.Fatalf("15 days out must not warn, got %+v", got)
	}
}

func TestFormatExpiryWarning(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	if s := FormatExpiryWarning(nil, 14); s != "" {
		t.Fatalf("empty list must render nothing, got %q", s)
	}
	s := FormatExpiryWarning(ExpiringEntries(expiryFixture(now), now, 14), 14)
	for _, need := range []string{"4 test-policy", "b-plus10", "b-medium7", "(10 d)"} {
		if !strings.Contains(s, need) {
			t.Fatalf("warning missing %q:\n%s", need, s)
		}
	}
	if strings.Contains(s, "b-plus30") {
		t.Fatalf("today+30 must not be listed:\n%s", s)
	}
}

// warnAboutExpiringEntries logs the warning and, on GitHub Actions, emits a
// workflow annotation. It never fails the test: expired rows fail Validate.
func warnAboutExpiringEntries(t *testing.T, ledger Ledger, now time.Time) {
	t.Helper()
	msg := FormatExpiryWarning(ExpiringEntries(ledger, now, ExpiryWarnDays), ExpiryWarnDays)
	if msg == "" {
		return
	}
	t.Log("WARNING: " + msg)
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		fmt.Printf("::warning title=test-policy waivers expiring::%s\n", strings.ReplaceAll(strings.TrimSpace(msg), "\n", "%0A"))
	}
}

// The repository ledger must not hide an approaching date-bomb: this test logs
// what is about to expire and stays green; its negative control is above.
func TestRepositoryLedgerExpiryEarlyWarning(t *testing.T) {
	root := repositoryRoot(t)
	ledger, err := LoadLedger(filepath.Join(root, "test", "test-resources.toml"))
	if err != nil {
		t.Fatalf("LoadLedger: %v", err)
	}
	warnAboutExpiringEntries(t, ledger, time.Now().UTC())
}
