package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	// The lending hours name IANA zones (Europe/Lisbon); embed the database so a host or
	// container without /usr/share/zoneinfo still resolves them.
	_ "time/tzdata"
)

// Hours is a parsed lending.hours window: "[<days>] <HH:MM>-<HH:MM> [<zone>]", e.g.
// "Mon-Fri 08:00-20:00 Europe/Lisbon".
//
//   - days: comma-separated days or ranges (Mon, Mon-Fri, Fri-Mon wraps); default every day;
//   - the time range is local to the zone (default UTC); an end before the start crosses
//     midnight and belongs to the day it starts on ("Fri 22:00-06:00" includes Saturday
//     03:00, not Friday 03:00); equal start and end is the whole day.
type Hours struct {
	days       [7]bool // by time.Weekday
	start, end int     // minutes since local midnight
	loc        *time.Location
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseHours parses a lending.hours value.
func ParseHours(s string) (*Hours, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 || len(fields) > 3 {
		return nil, fmt.Errorf("hours %q: want \"[days] HH:MM-HH:MM [zone]\"", s)
	}
	h := &Hours{loc: time.UTC}
	i := 0
	if !strings.Contains(fields[0], ":") {
		if err := h.parseDays(fields[0]); err != nil {
			return nil, fmt.Errorf("hours %q: %w", s, err)
		}
		i++
	} else {
		for d := range h.days {
			h.days[d] = true
		}
	}
	if i >= len(fields) {
		return nil, fmt.Errorf("hours %q: no time range", s)
	}
	from, to, ok := strings.Cut(fields[i], "-")
	if !ok {
		return nil, fmt.Errorf("hours %q: time range %q is not HH:MM-HH:MM", s, fields[i])
	}
	var err error
	if h.start, err = parseClock(from); err != nil {
		return nil, fmt.Errorf("hours %q: %w", s, err)
	}
	if h.end, err = parseClock(to); err != nil {
		return nil, fmt.Errorf("hours %q: %w", s, err)
	}
	i++
	if i < len(fields) {
		if h.loc, err = time.LoadLocation(fields[i]); err != nil {
			return nil, fmt.Errorf("hours %q: zone: %w", s, err)
		}
		i++
	}
	if i != len(fields) {
		return nil, fmt.Errorf("hours %q: unexpected %q", s, fields[i])
	}
	return h, nil
}

func (h *Hours) parseDays(s string) error {
	for part := range strings.SplitSeq(s, ",") {
		from, to, isRange := strings.Cut(part, "-")
		a, ok := weekdays[strings.ToLower(from)]
		if !ok {
			return fmt.Errorf("unknown day %q", from)
		}
		if !isRange {
			h.days[a] = true
			continue
		}
		b, ok := weekdays[strings.ToLower(to)]
		if !ok {
			return fmt.Errorf("unknown day %q", to)
		}
		for d := a; ; d = (d + 1) % 7 {
			h.days[d] = true
			if d == b {
				break
			}
		}
	}
	return nil
}

func parseClock(s string) (int, error) {
	hh, mm, ok := strings.Cut(s, ":")
	h, err1 := strconv.Atoi(hh)
	m, err2 := strconv.Atoi(mm)
	if !ok || err1 != nil || err2 != nil || len(mm) != 2 || h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, fmt.Errorf("time %q is not HH:MM", s)
	}
	return h*60 + m, nil
}

// Contains reports whether t is inside the window.
func (h *Hours) Contains(t time.Time) bool {
	local := t.In(h.loc)
	tod := local.Hour()*60 + local.Minute()
	day := local.Weekday()
	prev := (day + 6) % 7
	switch {
	case h.start == h.end:
		return h.days[day]
	case h.start < h.end:
		return h.days[day] && tod >= h.start && tod < h.end
	default: // crosses midnight: the evening part is today's window, the morning part yesterday's
		return (h.days[day] && tod >= h.start) || (h.days[prev] && tod < h.end)
	}
}
