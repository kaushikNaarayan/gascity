package cipolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// allowedSelfHostedLabels are the custom labels on our own runner fleet.
// A job whose runs-on is (optionally "self-hosted" plus) only labels from
// this set never leaves our infrastructure.
var allowedSelfHostedLabels = map[string]bool{
	"gt2":   true,
	"macos": true,
}

// approvedHostedException names one (workflow file, job name) pair that is
// allowed to stay on a GitHub-hosted or Blacksmith runner, with the reason
// it was approved. Every entry here is a standing exception to "self-hosted
// only" (Kaushik, 2026-08-13) and must be justified — see gcf-ye0 (audit)
// and gcf-5y0 (the fix this guard protects). This is what catches an
// upstream sync silently reintroducing a hosted runner: that is exactly how
// the blacksmith-* runners and the 4-maintainer allowlist arrived here.
type approvedHostedException struct {
	file   string
	job    string
	reason string
}

var approvedHostedExceptions = []approvedHostedException{
	{"ci.yml", "credential-provider-windows", "no self-hosted Windows runner; gated on internal/credentialprovider/** changes; Job Object assertions have no Linux/macOS equivalent"},
	{"release.yml", "update-homebrew-formula", "gated `if: github.repository == 'gastownhall/gascity'`; never runs for our fork"},
	{"gc-edge-publish.yml", "publish-gc-edge", "gated `if: github.repository == 'gastownhall/gascity'`; never runs for our fork"},

	// Everything below is disabled_manually as of the gcf-ye0 audit
	// (2026-08-22) and out of scope for gcf-5y0's re-enable waves. Named
	// explicitly rather than silently ignored (gcf-5y0 AC6): each is a
	// real, if currently dormant, hosted-runner exposure and deserves its
	// own re-enable decision, not a blanket pass.
	{"rc-gate.yml", "ubuntu_fast_tests", "RC gate (Blacksmith), disabled; separate governance decision, not gcf-5y0 scope"},
	{"rc-gate.yml", "ubuntu_make_check_docs", "RC gate (Blacksmith), disabled; separate governance decision, not gcf-5y0 scope"},
	{"rc-gate.yml", "ubuntu_acceptance_a", "RC gate (Blacksmith), disabled; separate governance decision, not gcf-5y0 scope"},
	{"rc-gate.yml", "ubuntu_acceptance_b", "RC gate (Blacksmith), disabled; separate governance decision, not gcf-5y0 scope"},
	{"rc-gate.yml", "ubuntu_acceptance_c", "RC gate (Blacksmith), disabled; separate governance decision, not gcf-5y0 scope"},
	{"rc-gate.yml", "ubuntu_integration_shards", "RC gate (Blacksmith), disabled; separate governance decision, not gcf-5y0 scope"},
	{"rc-gate.yml", "ubuntu_tutorial", "RC gate (Blacksmith), disabled; separate governance decision, not gcf-5y0 scope"},
	{"rc-gate.yml", "ubuntu_goreleaser_snapshot", "RC gate (Blacksmith), disabled; separate governance decision, not gcf-5y0 scope"},
	{"rc-gate.yml", "rc_summary", "RC gate (Blacksmith), disabled; separate governance decision, not gcf-5y0 scope"},
	{"homebrew-tap-smoke.yml", "tap-smoke", "Blacksmith macOS smoke test, disabled; not gcf-5y0 scope"},
	{"ollama-acceptance-c.yml", "acceptance-c", "Blacksmith, disabled; not gcf-5y0 scope"},
	{"close-stale-needs.yml", "close-needs-info", "cheap GH label-metadata bot, disabled; not gcf-5y0 scope"},
	{"dispatch-labeled-pr-suite.yml", "dispatch-suite", "cheap GH label-metadata bot, disabled; not gcf-5y0 scope"},
	{"docs-autofix.yml", "autofix", "cheap docs-check bot, disabled; not gcf-5y0 scope"},
	{"docs-render.yml", "broken-links", "cheap docs-check bot, disabled; not gcf-5y0 scope"},
	{"docs-title-review.yml", "request-docs-review", "cheap GH label-metadata bot, disabled; not gcf-5y0 scope"},
	{"remove-needs-info.yml", "remove-label", "cheap GH label-metadata bot, disabled; not gcf-5y0 scope"},
	{"remove-needs-triage.yml", "remove-triage-label", "cheap GH label-metadata bot, disabled; not gcf-5y0 scope"},
	{"request-needs-info.yml", "post-request", "cheap GH label-metadata bot, disabled; not gcf-5y0 scope"},
	{"triage-label.yml", "add-triage-label", "cheap GH label-metadata bot, disabled; not gcf-5y0 scope"},
}

func isApprovedHostedException(file, job string) bool {
	for _, exception := range approvedHostedExceptions {
		if exception.file == file && exception.job == job {
			return true
		}
	}
	return false
}

// runnerLabels normalizes a job's runs-on value (string or list) to a
// label slice.
func runnerLabels(runsOn any) []string {
	switch value := runsOn.(type) {
	case string:
		return []string{value}
	case []any:
		labels := make([]string, 0, len(value))
		for _, item := range value {
			labels = append(labels, fmt.Sprintf("%v", item))
		}
		return labels
	default:
		return []string{fmt.Sprintf("%v", value)}
	}
}

// isPolicyDerived is true when runs-on is computed from runner_policy.py's
// job outputs — which itself only ever resolves our self-hosted labels for
// material-lab-io/gascity (see runner_policy.py SELF_HOSTED_RUNNERS).
func isPolicyDerived(labels []string) bool {
	for _, label := range labels {
		if strings.Contains(label, "runner-policy.outputs") || strings.Contains(label, "steps.policy.outputs") {
			return true
		}
	}
	return false
}

// isApprovedSelfHosted is true when runs-on is a bare self-hosted label set
// naming only our own fleet (["self-hosted", "gt2"], "macos", etc.).
func isApprovedSelfHosted(labels []string) bool {
	rest := make([]string, 0, len(labels))
	for _, label := range labels {
		if label == "self-hosted" {
			continue
		}
		rest = append(rest, label)
	}
	if len(rest) == 0 {
		return false
	}
	for _, label := range rest {
		if !allowedSelfHostedLabels[label] {
			return false
		}
	}
	return true
}

// isConditionalSelfHostedForUs is true for the
// `github.repository == 'material-lab-io/gascity' && fromJSON('["self-hosted","gt2"]') || 'ubuntu-latest'`
// ternary used by release.yml / rc-release.yml: it resolves to our fleet
// for our fork and to a hosted fallback only for other repos/forks.
func isConditionalSelfHostedForUs(labels []string) bool {
	if len(labels) != 1 {
		return false
	}
	text := labels[0]
	return strings.Contains(text, "material-lab-io/gascity") &&
		strings.Contains(text, `"self-hosted"`) &&
		strings.Contains(text, `"gt2"`)
}

// checkRunnerLabels scans every job in every .github/workflows/*.yml file
// under root for a runs-on value that is neither derived from
// runner_policy.py, a bare self-hosted label, the release.yml/rc-release.yml
// conditional, nor an approved exception. It returns one violation string
// per offending job, sorted for deterministic output.
func checkRunnerLabels(root string) ([]string, error) {
	pattern := filepath.Join(root, ".github", "workflows", "*.yml")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", pattern, err)
	}
	sort.Strings(matches)

	var violations []string
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var workflow map[string]any
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		jobs, ok := workflow["jobs"].(map[string]any)
		if !ok {
			continue
		}
		jobNames := make([]string, 0, len(jobs))
		for name := range jobs {
			jobNames = append(jobNames, name)
		}
		sort.Strings(jobNames)

		file := filepath.Base(path)
		for _, jobName := range jobNames {
			job, ok := jobs[jobName].(map[string]any)
			if !ok {
				continue
			}
			runsOn, ok := job["runs-on"]
			if !ok {
				continue
			}
			labels := runnerLabels(runsOn)
			if isPolicyDerived(labels) ||
				isApprovedSelfHosted(labels) ||
				isConditionalSelfHostedForUs(labels) ||
				isApprovedHostedException(file, jobName) {
				continue
			}
			violations = append(violations, fmt.Sprintf(
				"%s: job %q runs on %v, which is neither self-hosted (gt2/macos) nor derived from runner_policy.py, and is not an approved exception",
				file, jobName, labels,
			))
		}
	}
	return violations, nil
}
