package cipolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoHostedRunnersOutsidePolicy(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	violations, err := checkRunnerLabels(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf(
			"every workflow job on material-lab-io/gascity must run on our self-hosted "+
				"fleet (via runner_policy.py outputs, or a bare self-hosted gt2/macos label) "+
				"unless explicitly allowlisted in approvedHostedExceptions with a stated "+
				"reason:\n  - %s",
			strings.Join(violations, "\n  - "),
		)
	}
}

func TestMakeTestCIPolicyRunsStaticScopeContracts(t *testing.T) {
	const want = "\t$(TEST_ENV) GOFLAGS= GOENV=off GOWORK=off go test -count=1 -run '^(TestPreflightStaticScopesOrdinaryPRsWithoutWeakeningProtectedRuns|TestFullStaticLintExplicitlyOwnsConfiguredGolangCIGovet|TestChangedStaticTargetsScopeLintAndFormattingToTheDiff|TestCIStaticScopeClassifierFailsClosedOutsideValidatedPullRequestMerge)$$' ./scripts"

	makefilePath := filepath.Join("..", "..", "Makefile")
	body, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read %s: %v", makefilePath, err)
	}
	_, rest, ok := strings.Cut(string(body), "\ntest-ci-policy:\n")
	if !ok {
		t.Fatal("Makefile has no test-ci-policy target")
	}
	recipe, _, _ := strings.Cut(rest, "\n\n")
	matches := 0
	for _, line := range strings.Split(recipe, "\n") {
		if line == want {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("test-ci-policy recipe must run the focused static-scope contracts with the exact hermetic command:\n%s", want)
	}
}
