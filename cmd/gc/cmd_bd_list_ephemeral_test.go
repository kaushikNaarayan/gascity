package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// fakeBdEphemeralSplit models the installed bd: `list` never returns
// ephemeral rows, `query` does, and every query is logged to $BD_LOG.
const fakeBdEphemeralSplit = `#!/bin/sh
echo "$@" >> "$BD_LOG"
case "$1" in
list) echo '[{"id":"demo-1","issue_type":"molecule","status":"open"}]' ;;
query) echo '[{"id":"demo-wisp-1","issue_type":"molecule","status":"open","ephemeral":true},{"id":"demo-1","issue_type":"molecule","status":"open"}]' ;;
esac
exit 0
`

func listIDs(t *testing.T, out string) []string {
	t.Helper()
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, out)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestGcBdListJSONUnionsEphemeralWisps(t *testing.T) {
	silentFallbackTestSetup(t, fakeBdEphemeralSplit)
	t.Setenv("BD_LOG", t.TempDir()+"/bd.log")

	var stdout, stderr bytes.Buffer
	args := []string{"list", "--json", "--assignee=witness", "--status=open", "--type=molecule"}
	if code := doBd(args, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd = %d; stderr=%q", code, stderr.String())
	}
	got := listIDs(t, stdout.String())
	want := []string{"demo-1", "demo-wisp-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v (planted wisp must appear, shared id deduped)", got, want)
	}
}

func TestGcBdListJSONEphemeralQueryCarriesFilters(t *testing.T) {
	silentFallbackTestSetup(t, fakeBdEphemeralSplit)
	log := t.TempDir() + "/bd.log"
	t.Setenv("BD_LOG", log)

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"list", "--json", "--assignee", "witness", "--status", "open", "--type", "molecule", "--limit", "0"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd = %d; stderr=%q", code, stderr.String())
	}
	logged := readFileString(t, log)
	want := "query --json ephemeral=true AND assignee=witness AND status=open AND type=molecule --limit 0"
	if !strings.Contains(logged, want) {
		t.Fatalf("bd query not issued with list filters.\nwant line: %s\nlog:\n%s", want, logged)
	}
}

func TestGcBdListWithoutJSONIsUnchanged(t *testing.T) {
	silentFallbackTestSetup(t, fakeBdEphemeralSplit)
	log := t.TempDir() + "/bd.log"
	t.Setenv("BD_LOG", log)

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"list", "--status", "open"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd = %d; stderr=%q", code, stderr.String())
	}
	if strings.Contains(readFileString(t, log), "query") {
		t.Fatalf("non-JSON list must stay a pure passthrough")
	}
}

func TestGcBdListUnsupportedFilterWarnsInsteadOfSilentlyOmitting(t *testing.T) {
	silentFallbackTestSetup(t, fakeBdEphemeralSplit)
	t.Setenv("BD_LOG", t.TempDir()+"/bd.log")

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"list", "--json", "--sort", "created"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd = %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ephemeral") || !strings.Contains(stderr.String(), "--sort") {
		t.Fatalf("stderr must say ephemeral rows were not merged and why; got %q", stderr.String())
	}
	if got := listIDs(t, stdout.String()); !reflect.DeepEqual(got, []string{"demo-1"}) {
		t.Fatalf("passthrough output altered: %v", got)
	}
}

func TestBdListEphemeralClauses(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantQuery   string
		wantAll     bool
		wantLimit   string
		wantBlocked string
	}{
		{"multi status", []string{"list", "--json", "-s", "open,in_progress"}, "ephemeral=true AND (status=open OR status=in_progress)", false, "50", ""},
		{"labels AND", []string{"list", "--json", "-l", "a,b"}, "ephemeral=true AND label=a AND label=b", false, "50", ""},
		{"all flag", []string{"list", "--json", "--all"}, "ephemeral=true", true, "50", ""},
		{"non bare value", []string{"list", "--json", "--assignee", "a b"}, "", false, "", "--assignee"},
		{"ready frontier", []string{"list", "--json", "--ready"}, "", false, "", "--ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := planBdListEphemeral(tt.args)
			if plan.blockedBy != tt.wantBlocked {
				t.Fatalf("blockedBy = %q, want %q", plan.blockedBy, tt.wantBlocked)
			}
			if tt.wantBlocked != "" {
				return
			}
			if plan.query != tt.wantQuery || plan.all != tt.wantAll || plan.limit != tt.wantLimit {
				t.Fatalf("plan = %+v", plan)
			}
		})
	}
}
