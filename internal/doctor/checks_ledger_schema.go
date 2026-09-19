package doctor

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// ledgerMigrationRunbook is the founder-gated procedure for moving a v59 town
// ledger to the schema the pinned bd expects.
const ledgerMigrationRunbook = "docs/runbooks/bd-v59-to-v66-fleet-migration.md"

// ProbeLedgerRead performs one bounded read through the schema-gated path.
// Store.Ping only proves the connection is alive: on a ledger older than the
// pinned bd expects it succeeds while every real read fails (gcf-tf22), so a
// health check built on Ping alone reports a false green.
func ProbeLedgerRead(s beads.Store) error {
	_, err := s.List(beads.ListQuery{Limit: 1, AllowScan: true, Live: true})
	return err
}

// IsLedgerSchemaMismatch reports whether err carries the signature of a ledger
// whose schema predates the pinned bd (observed on a v59 ledger read by
// bd 1.3.0-rc.2: a missing table, or a missing column on the issues table,
// worded by Dolt as "table not found", "does not have column" or "column ...
// could not be found in any table in scope").
func IsLedgerSchemaMismatch(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "table not found") ||
		strings.Contains(m, "could not be found in any table") ||
		(strings.Contains(m, "does not have column") && strings.Contains(m, "table"))
}

// LedgerUnreadableMessage and LedgerUnreadableFixHint (exported so gc bd ready
// reuses the exact wording, gcf-tf22) name the founder-gated migration
// without handing out the migrate command: it must not be run on a live ledger
// outside the runbook.
func LedgerUnreadableMessage(scope string, err error) string {
	return fmt.Sprintf(
		"beads ledger for %s is not readable by this gc/bd: its schema predates the pinned bd (%v). "+
			"This needs the founder-gated ledger migration; the store is otherwise reachable",
		scope, err)
}

const LedgerUnreadableFixHint = "do not roll this gc/bd out to a town with an unmigrated ledger; follow " +
	ledgerMigrationRunbook + " (founder-gated) and keep the previous gc/bd pinned until then; " +
	"do not run bd migrate against a live ledger outside that runbook"
