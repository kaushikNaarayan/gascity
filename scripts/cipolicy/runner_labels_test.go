package cipolicy

import (
	"os"
	"path/filepath"
	"testing"
)

func writeWorkflow(t *testing.T, dir, name, contents string) {
	t.Helper()
	workflowsDir := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowsDir, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRunnerLabelsAcceptsCompliantShapes(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "compliant.yml", `
jobs:
  policy-derived:
    runs-on: ${{ needs.runner-policy.outputs.runner_2vcpu }}
  bare-self-hosted:
    runs-on: [self-hosted, gt2]
  bare-macos:
    runs-on: [self-hosted, macos]
`)

	violations, err := checkRunnerLabels(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("want no violations, got %v", violations)
	}
}

func TestCheckRunnerLabelsRejectsHostedFallbackInReleaseWorkflow(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "release.yml", `
jobs:
  release:
    runs-on: ${{ github.repository == 'material-lab-io/gascity' && fromJSON('["self-hosted","gt2"]') || 'ubuntu-latest' }}
`)

	violations, err := checkRunnerLabels(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 {
		t.Fatalf("hosted release fallback must be rejected, got %v", violations)
	}
}

func TestCheckRunnerLabelsFailsOnNewHostedRunner(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "regressed.yml", `
jobs:
  some-new-job:
    runs-on: ubuntu-latest
`)

	violations, err := checkRunnerLabels(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 {
		t.Fatalf("want exactly one violation for a new ubuntu-latest job, got %v", violations)
	}
}

func TestCheckRunnerLabelsFailsOnUnapprovedBlacksmithLabel(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "regressed.yml", `
jobs:
  some-new-job:
    runs-on: blacksmith-32vcpu-ubuntu-2404
`)

	violations, err := checkRunnerLabels(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 {
		t.Fatalf("want exactly one violation for an unapproved Blacksmith label, got %v", violations)
	}
}

func TestCheckRunnerLabelsHonorsApprovedExceptionsOnlyByExactJobName(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ci.yml", `
jobs:
  credential-provider-windows:
    runs-on: windows-latest
  another-windows-job:
    runs-on: windows-latest
`)

	violations, err := checkRunnerLabels(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 {
		t.Fatalf("want exactly one violation (the unapproved job), got %v", violations)
	}
}
