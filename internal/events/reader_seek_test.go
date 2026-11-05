package events

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeSeqLines(t *testing.T, path string, from, to int, appendMode bool) {
	t.Helper()
	flag := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	for s := from; s <= to; s++ {
		fmt.Fprintf(f, "{\"seq\":%d,\"type\":\"bead.created\",\"actor\":\"t\",\"ts\":\"2026-11-05T00:00:00Z\"}\n", s)
	}
}

func tick(t *testing.T, path string, after uint64) ([]Event, int64) {
	t.Helper()
	before := activeLinesParsed.Load()
	evs, err := ReadFiltered(path, Filter{Type: "bead.created", AfterSeq: after})
	if err != nil {
		t.Fatal(err)
	}
	return evs, activeLinesParsed.Load() - before
}

// (1) After a warm-up, each tick parses only appended lines.
func TestReadFilteredSeekParsesOnlyAppended(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeSeqLines(t, path, 1, 5000, false)
	cursor := uint64(5000)
	if evs, n := tick(t, path, cursor); len(evs) != 0 || n != 5000 {
		t.Fatalf("warm-up: %d events, parsed %d, want 0 / 5000", len(evs), n)
	}
	next := 5001
	for i := 0; i < 20; i++ {
		writeSeqLines(t, path, next, next+2, true)
		evs, n := tick(t, path, cursor)
		if n != 3 || len(evs) != 3 || evs[0].Seq != uint64(next) {
			t.Fatalf("tick %d: parsed %d lines, events %v; want 3 appended only", i, n, seqsOf(evs))
		}
		cursor = uint64(next + 2)
		next += 3
	}
}

// A lagging cursor still sees every event it has not consumed.
func TestReadFilteredSeekLaggingCursorLosesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeSeqLines(t, path, 1, 100, false)
	tick(t, path, 100)
	writeSeqLines(t, path, 101, 110, true)
	tick(t, path, 105)
	evs, _ := tick(t, path, 50)
	if len(evs) != 60 || evs[0].Seq != 51 || evs[59].Seq != 110 {
		t.Fatalf("lagging cursor got %d events %v", len(evs), seqsOf(evs))
	}
}

// A partial trailing line is not skipped once it completes.
func TestReadFilteredSeekPartialLineCompletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeSeqLines(t, path, 1, 10, false)
	tick(t, path, 10)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	fmt.Fprint(f, `{"seq":11,"type":"bead.created","actor":"t","ts":"2026-11-05T00:00:00Z"}`) // no newline yet
	tick(t, path, 10)
	fmt.Fprint(f, "\n")
	f.Close() //nolint:errcheck
	evs, _ := tick(t, path, 10)
	if len(evs) != 1 || evs[0].Seq != 11 {
		t.Fatalf("got %v, want [11]", seqsOf(evs))
	}
}

// (2) Rotation (new inode) and in-place truncate reset the hint, no event lost.
func TestReadFilteredSeekRotationAndTruncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeSeqLines(t, path, 1, 1000, false)
	tick(t, path, 1000)

	// Rotation: move the file away, start a fresh one with later seqs.
	if err := os.Rename(path, filepath.Join(dir, "old.keep")); err != nil {
		t.Fatal(err)
	}
	writeSeqLines(t, path, 1001, 1004, false)
	evs, _ := tick(t, path, 1000)
	if len(evs) != 4 || evs[0].Seq != 1001 {
		t.Fatalf("after rotation got %v, want 1001..1004", seqsOf(evs))
	}

	// Truncate in place and regrow with a larger file than the hint offset.
	writeSeqLines(t, path, 1, 3000, false)
	evs, _ = tick(t, path, 1004)
	if len(evs) != 3000-1004 || evs[0].Seq != 1005 {
		t.Fatalf("after truncate got %d events first=%v, want %d", len(evs), seqsOf(evs[:1]), 3000-1004)
	}
}

// (3) Negative control: without a hint (what the old code always did) every
// tick parses the whole file, so the counter would catch a regression.
func TestReadFilteredSeekNegativeControlFullScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeSeqLines(t, path, 1, 500, false)
	for i := 0; i < 3; i++ {
		dropSeekHints(path)
		if _, n := tick(t, path, 500); n != 500 {
			t.Fatalf("no-hint tick %d parsed %d, want full 500", i, n)
		}
	}
}
