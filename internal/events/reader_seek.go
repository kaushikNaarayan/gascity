package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// The live events.jsonl is append-only and seq-ordered. Event-triggered orders
// call ReadFiltered with AfterSeq=<cursor> on every cadence tick; without a
// seek, each tick re-reads and re-parses the whole (hundreds of MB) file only
// to discard every line at or below the cursor (gcf-nar3h).
//
// seekHints remembers, per path, byte offsets known to sit just after a
// newline-terminated line whose Seq is <= seq. A later read with
// AfterSeq >= seq can start at that offset. The hints are in-memory only (no
// persisted status file): a process restart does one full scan and rebuilds
// them, and correctness never depends on a hint -- every use is validated and
// any doubt falls back to a full scan.

const maxSeekHintsPerPath = 16

type seekHint struct {
	ino    uint64
	offset int64
	seq    uint64
}

var seekHints = struct {
	sync.Mutex
	m map[string][]seekHint
}{m: map[string][]seekHint{}}

// activeLinesParsed counts active-file lines handed to json.Unmarshal. Tests
// use it to prove ticks parse only appended bytes.
var activeLinesParsed atomic.Int64

// findSeekHint returns the furthest valid hint for path: same inode, offset
// within size, and seq <= afterSeq.
func findSeekHint(path string, ino uint64, size int64, afterSeq uint64) (seekHint, bool) {
	if ino == 0 || afterSeq == 0 {
		return seekHint{}, false
	}
	seekHints.Lock()
	defer seekHints.Unlock()
	var best seekHint
	found := false
	for _, h := range seekHints.m[path] {
		if h.ino != ino || h.offset > size || h.seq > afterSeq {
			continue
		}
		if !found || h.offset > best.offset {
			best, found = h, true
		}
	}
	return best, found
}

func recordSeekHint(path string, h seekHint) {
	if h.ino == 0 || h.offset <= 0 || h.seq == 0 {
		return
	}
	seekHints.Lock()
	defer seekHints.Unlock()
	var kept []seekHint
	for _, old := range seekHints.m[path] {
		if old.ino != h.ino || old.seq == h.seq {
			continue // rotated-away inode, or superseded at the same seq
		}
		kept = append(kept, old)
	}
	kept = append(kept, h)
	if len(kept) > maxSeekHintsPerPath {
		lo := 0
		for i, k := range kept {
			if k.seq < kept[lo].seq {
				lo = i
			}
		}
		kept = append(kept[:lo], kept[lo+1:]...)
	}
	seekHints.m[path] = kept
}

func dropSeekHints(path string) {
	seekHints.Lock()
	delete(seekHints.m, path)
	seekHints.Unlock()
}

// scanActiveFile reads the live log into result, using a seek hint when the
// filter has an AfterSeq. Results are identical to a full scan.
func scanActiveFile(path string, f *os.File, filter Filter, result []Event) ([]Event, error) {
	base := len(result)
	var ino uint64
	var size int64
	if st, err := f.Stat(); err == nil {
		ino, size = inodeOf(st), st.Size()
	}
	if h, ok := findSeekHint(path, ino, size, filter.AfterSeq); ok && hintBoundaryOK(f, h) {
		out, valid, err := scanActiveFrom(path, f, h.offset, h.seq, ino, filter, result)
		if valid {
			return out, err
		}
		// Hint contradicted by the file (truncate/rewrite): forget and rescan.
		dropSeekHints(path)
		result = out[:base]
	}
	out, _, err := scanActiveFrom(path, f, 0, 0, ino, filter, result)
	return out, err
}

// byteIs reports whether the byte at off equals want.
func byteIs(f *os.File, off int64, want byte) bool {
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		return false
	}
	return b[0] == want
}

// maxEventLineBytes bounds the backward line search; it matches the scanner's
// 1MB line limit.
const maxEventLineBytes = 1024 * 1024

// hintBoundaryOK verifies a hint against the file: the byte before the offset
// must be a newline AND the line ending there must carry the hinted seq. The
// seq check catches an in-place rewrite (same inode, equal-width lines) that a
// newline-only check cannot see.
func hintBoundaryOK(f *os.File, h seekHint) bool {
	if h.offset <= 0 {
		return true
	}
	if !byteIs(f, h.offset-1, '\n') {
		return false
	}
	end := h.offset - 1 // exclusive end of the previous line's content
	start := end - 4096
	var buf []byte
	for {
		if start < 0 {
			start = 0
		}
		buf = make([]byte, end-start)
		if _, err := f.ReadAt(buf, start); err != nil {
			return false
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
			break
		}
		if start == 0 {
			break // first line of the file
		}
		if end-start >= maxEventLineBytes {
			return false
		}
		start -= end - start // double the window
	}
	var e Event
	if err := json.Unmarshal(buf, &e); err != nil {
		return false
	}
	return e.Seq == h.seq
}

// scanActiveFrom scans from start. When start > 0, floor is the seq the first
// parsed line must exceed; a violation returns valid=false so the caller
// rescans from zero.
func scanActiveFrom(path string, f *os.File, start int64, floor, ino uint64, filter Filter, result []Event) ([]Event, bool, error) {
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return result, start == 0, fmt.Errorf("reading events: %w", err)
	}
	pos := start        // bytes consumed by the scanner, i.e. end of the last returned line
	terminated := false // last returned line ended in '\n'
	split := func(data []byte, atEOF bool) (int, []byte, error) {
		adv, tok, err := bufio.ScanLines(data, atEOF)
		pos += int64(adv)
		if adv > 0 {
			terminated = data[adv-1] == '\n'
		}
		return adv, tok, err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // handle lines up to 1MB
	scanner.Split(split)

	var tailHint, boundaryHint seekHint
	first := true
	for scanner.Scan() {
		activeLinesParsed.Add(1)
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue // skip malformed lines
		}
		if first && start > 0 && e.Seq <= floor {
			return result, false, nil
		}
		first = false
		// Only newline-terminated lines are safe resume points (a partial
		// trailing line may still be mid-write).
		if e.Seq > 0 && terminated {
			tailHint = seekHint{ino: ino, offset: pos, seq: e.Seq}
			if e.Seq <= filter.AfterSeq {
				boundaryHint = tailHint
			}
		}
		if !matchesFilter(e, filter) {
			continue
		}
		result = append(result, e)
		if limitReached(len(result), filter) {
			break
		}
	}
	recordSeekHint(path, boundaryHint)
	recordSeekHint(path, tailHint)
	if err := scanner.Err(); err != nil {
		return result, true, fmt.Errorf("scanning events: %w", err)
	}
	return result, true, nil
}
