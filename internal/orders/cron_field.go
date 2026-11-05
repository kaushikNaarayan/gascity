package orders

import (
	"fmt"
	"strconv"
	"strings"
)

// cronFieldCount is the number of whitespace-separated fields in a cron
// schedule: minute, hour, day-of-month, month, day-of-week.
const cronFieldCount = 5

// cronFieldBounds are the inclusive value limits of each cron field, in
// schedule order. Day-of-week uses the time.Weekday numbering (0 = Sunday).
var cronFieldBounds = [cronFieldCount]struct {
	name     string
	min, max int
}{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 6},
}

// cronPart is one comma-separated element of a cron field: "*", "*/N", "N",
// "A-B" or "A-B/N".
type cronPart struct {
	star   bool // "*" or "*/N": any value, stepped from zero
	lo, hi int  // inclusive bounds when !star
	step   int  // >= 1
}

func (p cronPart) matches(value int) bool {
	if p.star {
		return value%p.step == 0
	}
	return value >= p.lo && value <= p.hi && (value-p.lo)%p.step == 0
}

// parseCronField parses one cron field into its comma-separated parts. It
// rejects every syntax the matcher does not implement (names such as MON or
// JAN, "?", "L", "W", "#", and malformed numbers) so an unsupported schedule
// is an error instead of an order that silently never fires.
func parseCronField(field string) ([]cronPart, error) {
	if field == "" {
		return nil, fmt.Errorf("empty field")
	}
	var parts []cronPart
	for _, raw := range strings.Split(field, ",") {
		raw = strings.TrimSpace(raw)
		base, stepText, hasStep := strings.Cut(raw, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("invalid step in %q", raw)
			}
			step = n
		}
		switch {
		case base == "*":
			parts = append(parts, cronPart{star: true, step: step})
		case strings.Contains(base, "-"):
			loText, hiText, _ := strings.Cut(base, "-")
			lo, loErr := strconv.Atoi(loText)
			hi, hiErr := strconv.Atoi(hiText)
			if loErr != nil || hiErr != nil || lo < 0 || lo > hi {
				return nil, fmt.Errorf("invalid range %q", raw)
			}
			parts = append(parts, cronPart{lo: lo, hi: hi, step: step})
		default:
			if hasStep {
				return nil, fmt.Errorf("step %q needs * or a range", raw)
			}
			n, err := strconv.Atoi(base)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("unsupported value %q", raw)
			}
			parts = append(parts, cronPart{lo: n, hi: n, step: 1})
		}
	}
	return parts, nil
}

// cronFieldMatches reports whether a single cron field matches a value.
// Supports "*", "*/N", exact integers, ranges "A-B", range steps "A-B/N", and
// comma-separated lists of those. A field that does not parse matches nothing.
func cronFieldMatches(field string, value int) bool {
	parts, err := parseCronField(field)
	if err != nil {
		return false
	}
	for _, p := range parts {
		if p.matches(value) {
			return true
		}
	}
	return false
}

// validateCronSchedule checks that schedule has five fields and that every
// field uses only supported syntax within its value bounds.
func validateCronSchedule(schedule string) error {
	fields := strings.Fields(schedule)
	if len(fields) != cronFieldCount {
		return fmt.Errorf("want %d fields, got %d", cronFieldCount, len(fields))
	}
	for i, field := range fields {
		bounds := cronFieldBounds[i]
		parts, err := parseCronField(field)
		if err != nil {
			return fmt.Errorf("%s field %q: %w", bounds.name, field, err)
		}
		for _, p := range parts {
			if p.star {
				continue
			}
			if p.lo < bounds.min || p.hi > bounds.max {
				return fmt.Errorf("%s field %q: value outside %d-%d", bounds.name, field, bounds.min, bounds.max)
			}
		}
	}
	return nil
}
