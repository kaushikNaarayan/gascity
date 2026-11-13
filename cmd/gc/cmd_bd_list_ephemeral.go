package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// bdListEphemeralPlan describes the `bd query` leg that must accompany a
// `bd list --json` so ephemeral wisps are counted. The installed bd list
// surface never returns ephemeral rows, while bd query does, so the two
// commands disagree about the same predicate (gcf-a65a). When blockedBy is
// set the list carries a filter the query leg cannot express and the union is
// skipped.
type bdListEphemeralPlan struct {
	applies   bool
	query     string
	all       bool
	limit     string
	blockedBy string
}

var (
	bdListEphemeralValueFlags = map[string]string{
		"-a": "assignee", "--assignee": "assignee",
		"-s": "status", "--status": "status",
		"-t": "type", "--type": "type",
		"-l": "label", "--label": "label",
		"-n": "limit", "--limit": "limit",
	}
	bdListEphemeralBoolFlags = map[string]bool{"--json": true, "--all": true}
)

// planBdListEphemeral inspects `gc bd` arguments and returns the ephemeral
// query leg for a JSON `list`. Non-list and non-JSON invocations do not apply.
func planBdListEphemeral(args []string) bdListEphemeralPlan {
	if len(args) == 0 || args[0] != "list" {
		return bdListEphemeralPlan{}
	}
	plan := bdListEphemeralPlan{limit: "50"}
	filters := map[string]string{}
	json := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		if bdListEphemeralBoolFlags[arg] {
			switch arg {
			case "--json":
				json = true
			case "--all":
				plan.all = true
			}
			continue
		}
		field, known := bdListEphemeralValueFlags[name]
		if !known {
			return bdListEphemeralPlan{applies: true, blockedBy: name}
		}
		if !hasValue {
			if i+1 >= len(args) {
				return bdListEphemeralPlan{applies: true, blockedBy: name}
			}
			i++
			value = args[i]
		}
		if field == "limit" {
			plan.limit = value
			continue
		}
		filters[field] = value
	}
	if !json {
		return bdListEphemeralPlan{}
	}
	clauses := []string{"ephemeral=true"}
	for _, field := range []string{"assignee", "status", "type", "label"} {
		value, ok := filters[field]
		if !ok {
			continue
		}
		parts := strings.Split(value, ",")
		terms := make([]string, 0, len(parts))
		for _, part := range parts {
			if !isBareBdQueryToken(part) {
				return bdListEphemeralPlan{applies: true, blockedBy: "--" + field}
			}
			terms = append(terms, field+"="+part)
		}
		switch {
		case field == "label" || len(terms) == 1:
			clauses = append(clauses, terms...)
		default:
			clauses = append(clauses, "("+strings.Join(terms, " OR ")+")")
		}
	}
	plan.applies = true
	plan.query = strings.Join(clauses, " AND ")
	return plan
}

// isBareBdQueryToken reports whether value can be placed in a bd query clause
// without quoting.
func isBareBdQueryToken(value string) bool {
	if value == "" {
		return false
	}
	switch strings.ToUpper(value) {
	case "AND", "OR", "NOT":
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == ':' || r == '.':
		default:
			return false
		}
	}
	return true
}

// queryArgs returns the bd argv for the ephemeral leg.
func (p bdListEphemeralPlan) queryArgs() []string {
	args := []string{"query", "--json", p.query}
	if p.all {
		args = append(args, "--all")
	}
	return append(args, "--limit", p.limit)
}

// mergeBdListJSON appends rows from extra that primary does not already hold
// (by id), preserving primary order. Both inputs must be JSON arrays.
func mergeBdListJSON(primary, extra []byte) ([]byte, error) {
	var base, add []json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(primary), &base); err != nil {
		return nil, fmt.Errorf("parsing bd list output: %w", err)
	}
	if err := json.Unmarshal(bytes.TrimSpace(extra), &add); err != nil {
		return nil, fmt.Errorf("parsing bd query output: %w", err)
	}
	seen := make(map[string]bool, len(base))
	for _, row := range base {
		seen[bdRowID(row)] = true
	}
	for _, row := range add {
		if id := bdRowID(row); id == "" || !seen[id] {
			seen[id] = true
			base = append(base, row)
		}
	}
	out, err := json.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("encoding merged bd list: %w", err)
	}
	return append(out, '\n'), nil
}

func bdRowID(row json.RawMessage) string {
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(row, &r); err != nil {
		return ""
	}
	return r.ID
}

// emitBdListWithEphemeral runs the ephemeral query leg and writes the merged
// list to stdout. listOut is bd list's captured stdout.
func emitBdListWithEphemeral(plan bdListEphemeralPlan, bdPath, dir string, env []string, listOut []byte, stdout, stderr io.Writer) int {
	cmd := exec.Command(bdPath, plan.queryArgs()...)
	cmd.Dir = dir
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "gc bd: list: ephemeral wisp query failed: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	merged, err := mergeBdListJSON(listOut, out.Bytes())
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if _, err := stdout.Write(merged); err != nil {
		fmt.Fprintf(stderr, "gc bd: list: writing output: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}
