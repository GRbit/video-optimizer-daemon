package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// workWindow is a daily time-of-day window in local time. A nil window is
// always open.
type workWindow struct {
	start int // minutes since midnight, inclusive
	end   int // minutes since midnight, exclusive
}

// parseWorkWindow accepts "HH:MM-HH:MM". The window may cross midnight
// ("23:00-07:00"). An empty string means no restriction.
func parseWorkWindow(s string) (*workWindow, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return nil, fmt.Errorf("work hours %q: expected HH:MM-HH:MM", s)
	}
	start, err := parseMinutes(parts[0])
	if err != nil {
		return nil, fmt.Errorf("work hours %q: %w", s, err)
	}
	end, err := parseMinutes(parts[1])
	if err != nil {
		return nil, fmt.Errorf("work hours %q: %w", s, err)
	}
	if start == end {
		return nil, fmt.Errorf("work hours %q: start and end are equal", s)
	}
	return &workWindow{start: start, end: end}, nil
}

func parseMinutes(s string) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil {
		return 0, fmt.Errorf("bad time %q", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return h*60 + m, nil
}

func (w *workWindow) String() string {
	if w == nil {
		return "always"
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.start/60, w.start%60, w.end/60, w.end%60)
}

func (w *workWindow) contains(t time.Time) bool {
	if w == nil {
		return true
	}
	m := t.Hour()*60 + t.Minute()
	if w.start < w.end {
		return m >= w.start && m < w.end
	}
	return m >= w.start || m < w.end
}

// nextChange returns the first moment after t at which contains() flips.
func (w *workWindow) nextChange(t time.Time) time.Time {
	var best time.Time
	for day := 0; day <= 1; day++ {
		for _, mins := range []int{w.start, w.end} {
			b := time.Date(t.Year(), t.Month(), t.Day()+day, mins/60, mins%60, 0, 0, t.Location())
			if b.After(t) && (best.IsZero() || b.Before(best)) {
				best = b
			}
		}
	}
	return best
}

// waitUntilOpen blocks until the window is open. It returns false when ctx is
// cancelled first.
func (w *workWindow) waitUntilOpen(ctx context.Context) bool {
	for {
		now := time.Now()
		if w.contains(now) {
			return true
		}
		next := w.nextChange(now)
		slog.Info("Outside work hours, sleeping", "window", w.String(), "until", next.Format(time.RFC3339))
		select {
		case <-ctx.Done():
			return false
		// The extra second lands safely inside the next minute.
		case <-time.After(time.Until(next) + time.Second):
		}
	}
}
