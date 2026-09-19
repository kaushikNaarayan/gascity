package git

import (
	"fmt"
	"strings"
	"testing"
)

func TestDirtyFileSummary(t *testing.T) {
	many := make([]string, 0, 13)
	for i := 0; i < 13; i++ {
		many = append(many, fmt.Sprintf(" M dir/f%02d.sh", i))
	}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean", "", ""},
		{"single modified", " M pack.toml", "pack.toml"},
		// runRepoCacheGit TrimSpaces its output, eating the first entry's blank.
		{"leading blank trimmed", "M pack.toml\n?? new.txt", "pack.toml, new.txt"},
		{"rename kept whole", "R  old.txt -> new.txt", "old.txt -> new.txt"},
		{"bounded", strings.Join(many, "\n"), strings.TrimSuffix(func() string {
			var b []string
			for i := 0; i < DirtySummaryMaxFiles; i++ {
				b = append(b, fmt.Sprintf("dir/f%02d.sh", i))
			}
			return strings.Join(b, ", ")
		}(), "") + " (+3 more)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DirtyFileSummary(tt.in); got != tt.want {
				t.Fatalf("DirtyFileSummary = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDirtyFileSummaryTruncatesLongPath(t *testing.T) {
	got := DirtyFileSummary(" M " + strings.Repeat("a", 500))
	if len(got) > dirtySummaryMaxPathLen+len("...") {
		t.Fatalf("path not bounded: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("want truncation marker, got %q", got)
	}
}
