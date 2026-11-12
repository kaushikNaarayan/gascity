// Package formula provides Step.Condition evaluation for compile-time step filtering.
//
// Step.Condition is simpler than the runtime condition evaluation in condition.go.
// It evaluates at cook/pour time to include or exclude steps based on formula variables.
//
// Supported formats:
//   - "{{var}}" - truthy check (non-empty, non-"false", non-"0")
//   - "!{{var}}" - negated truthy check (include if var is falsy)
//   - "{{var}} == value" - equality check
//   - "{{var}} != value" - inequality check
package formula

import (
	"fmt"
	"regexp"
	"strings"
)

// Step condition patterns
var (
	// {{var}} - simple variable reference for truthy check
	stepCondVarPattern = regexp.MustCompile(`^\{\{(\w+)\}\}$`)

	// !{{var}} - negated truthy check
	stepCondNegatedVarPattern = regexp.MustCompile(`^!\{\{(\w+)\}\}$`)

	// {{var}} == value or {{var}} != value
	stepCondComparePattern = regexp.MustCompile(`^\{\{(\w+)\}\}\s*(==|!=)\s*(.+)$`)
)

// EvaluateStepCondition evaluates a step's condition against variable values.
// Returns true if the step should be included, false if it should be skipped.
//
// Condition formats:
//   - "" (empty) - always include
//   - "{{var}}" - include if var is truthy (non-empty, non-"false", non-"0")
//   - "!{{var}}" - include if var is NOT truthy (negated)
//   - "{{var}} == value" - include if var equals value
//   - "{{var}} != value" - include if var does not equal value
func EvaluateStepCondition(condition string, vars map[string]string) (bool, error) {
	condition = strings.TrimSpace(condition)

	// Empty condition means always include
	if condition == "" {
		return true, nil
	}

	// Try truthy pattern: {{var}}
	if m := stepCondVarPattern.FindStringSubmatch(condition); m != nil {
		varName := m[1]
		value := vars[varName]
		return isTruthy(value), nil
	}

	// Try negated truthy pattern: !{{var}}
	if m := stepCondNegatedVarPattern.FindStringSubmatch(condition); m != nil {
		varName := m[1]
		value := vars[varName]
		return !isTruthy(value), nil
	}

	// Try comparison pattern: {{var}} == value or {{var}} != value
	if m := stepCondComparePattern.FindStringSubmatch(condition); m != nil {
		varName := m[1]
		operator := m[2]
		expected := strings.TrimSpace(m[3])

		// Remove quotes from expected value if present
		expected = unquoteValue(expected)

		actual := vars[varName]

		switch operator {
		case "==":
			return actual == expected, nil
		case "!=":
			return actual != expected, nil
		}
	}

	return false, fmt.Errorf("invalid step condition format: %q (expected {{var}} or {{var}} == value)", condition)
}

// isTruthy returns true if a value is considered "truthy" for step conditions.
// Falsy values: empty string, "false", "0", "no", "off"
// All other values are truthy.
func isTruthy(value string) bool {
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	switch lower {
	case "false", "0", "no", "off":
		return false
	}
	return true
}

// unquoteValue removes surrounding quotes from a value if present.
func unquoteValue(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// FilterStepsByCondition filters a list of steps based on their Condition field.
// Steps with conditions that evaluate to false are excluded from the result.
// Children of excluded steps are also excluded.
//
// An excluded step is a pass-through for ordering (gcf-g6x5q): any remaining
// step that needs or depends on it inherits the excluded step's own needs,
// resolved transitively, so a -> b(false) -> c still orders c after a instead
// of leaving c with no blocking edge. Children of an excluded step inherit the
// parent's needs as well.
//
// Parameters:
//   - steps: the steps to filter
//   - vars: variable values for condition evaluation
//
// Returns the filtered steps and any error encountered during evaluation.
func FilterStepsByCondition(steps []*Step, vars map[string]string) ([]*Step, error) {
	if vars == nil {
		vars = make(map[string]string)
	}
	dropped := make(map[string][]string)
	result, err := filterStepsByCondition(steps, vars, dropped)
	if err != nil {
		return nil, err
	}
	if len(dropped) > 0 {
		rewireDroppedStepDeps(result, dropped)
	}
	return result, nil
}

func filterStepsByCondition(steps []*Step, vars map[string]string, dropped map[string][]string) ([]*Step, error) {
	result := make([]*Step, 0, len(steps))

	for _, step := range steps {
		// Evaluate step condition
		include, err := EvaluateStepCondition(step.Condition, vars)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", step.ID, err)
		}

		if !include {
			// Skip this step and all its children, remembering their needs
			// so dependents can be rewired through them.
			recordDroppedStep(step, nil, dropped)
			continue
		}

		// Clone the step to avoid mutating input
		clone := cloneStep(step)

		// Recursively filter children
		if len(step.Children) > 0 {
			filteredChildren, err := filterStepsByCondition(step.Children, vars, dropped)
			if err != nil {
				return nil, err
			}
			clone.Children = filteredChildren
		}

		result = append(result, clone)
	}

	return result, nil
}

// recordDroppedStep records the direct needs of a dropped step and its whole
// subtree. A child of a dropped step also carries its parent's needs.
func recordDroppedStep(step *Step, inherited []string, dropped map[string][]string) {
	deps := append(append(append([]string(nil), step.Needs...), step.DependsOn...), inherited...)
	dropped[step.ID] = deps
	for _, child := range step.Children {
		recordDroppedStep(child, deps, dropped)
	}
}

// rewireDroppedStepDeps replaces every reference to a dropped step in Needs and
// DependsOn with that step's transitively resolved needs, de-duplicated and in
// first-seen order.
func rewireDroppedStepDeps(steps []*Step, dropped map[string][]string) {
	resolved := make(map[string][]string, len(dropped))
	var resolve func(id string, visiting map[string]bool) []string
	resolve = func(id string, visiting map[string]bool) []string {
		if r, ok := resolved[id]; ok {
			return r
		}
		if visiting[id] {
			return nil // cycle among dropped steps: contributes no edge
		}
		visiting[id] = true
		var out []string
		for _, dep := range dropped[id] {
			if _, isDropped := dropped[dep]; isDropped {
				out = append(out, resolve(dep, visiting)...)
				continue
			}
			out = append(out, dep)
		}
		delete(visiting, id)
		resolved[id] = out
		return out
	}
	rewrite := func(ids []string) []string {
		if len(ids) == 0 {
			return ids
		}
		seen := make(map[string]bool, len(ids))
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			targets := []string{id}
			if _, isDropped := dropped[id]; isDropped {
				targets = resolve(id, map[string]bool{})
			}
			for _, t := range targets {
				if !seen[t] {
					seen[t] = true
					out = append(out, t)
				}
			}
		}
		return out
	}
	for _, step := range steps {
		step.Needs = rewrite(step.Needs)
		step.DependsOn = rewrite(step.DependsOn)
		rewireDroppedStepDeps(step.Children, dropped)
	}
}
