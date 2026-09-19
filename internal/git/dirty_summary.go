package git

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	// DirtySummaryMaxFiles bounds how many paths DirtyFileSummary lists.
	DirtySummaryMaxFiles = 10
	// dirtySummaryMaxPathLen bounds a single listed path.
	dirtySummaryMaxPathLen = 200
)

// porcelainLine matches one `git status --porcelain` (v1) entry. The status
// column is tolerated as one or two characters because callers commonly
// TrimSpace the whole output, which eats the leading blank of a first entry
// such as " M path".
var porcelainLine = regexp.MustCompile(`^[ MADRCUT?!]{1,2} +(.+)$`)

// DirtyFileSummary renders the `git status --porcelain` output of a dirty
// worktree as a bounded, single-line list of relative paths, e.g.
// "pack.toml, scripts/x.sh (+3 more)". It never includes file contents.
// It returns "" when the status output lists no paths.
func DirtyFileSummary(porcelain string) string {
	var paths []string
	for _, line := range strings.Split(porcelain, "\n") {
		line = strings.TrimRight(line, " \r\t")
		if line == "" {
			continue
		}
		m := porcelainLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		p := m[1]
		if len(p) > dirtySummaryMaxPathLen {
			p = p[:dirtySummaryMaxPathLen] + "..."
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return ""
	}
	shown := paths
	if len(shown) > DirtySummaryMaxFiles {
		shown = shown[:DirtySummaryMaxFiles]
	}
	out := strings.Join(shown, ", ")
	if extra := len(paths) - len(shown); extra > 0 {
		out += fmt.Sprintf(" (+%d more)", extra)
	}
	return out
}
