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
	"pool":  true,
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

// Empty since gcf-kjrft: the inactive upstream workflows that carried hosted-runner
// exceptions were deleted; only release/rc-release/fork-verify remain, all self-hosted.
var approvedHostedExceptions = []approvedHostedException{}

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

// checkRunnerLabels scans every job in every .github/workflows/*.yml file
// under root for a runs-on value that is neither derived from
// runner_policy.py, a bare self-hosted label, nor an approved exception. It returns one violation string
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
