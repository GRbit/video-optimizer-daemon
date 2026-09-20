package main

import (
	"testing"
	"time"
)

func at(h, m int) time.Time {
	return time.Date(2026, 9, 21, h, m, 0, 0, time.Local)
}

// Windows crossing midnight and the inclusive/exclusive edges are where the
// minute arithmetic goes wrong, so both are pinned here.
func TestWorkWindowContains(t *testing.T) {
	day, _ := parseWorkWindow("09:00-17:00")
	night, _ := parseWorkWindow("23:00-07:00")

	cases := []struct {
		name string
		w    *workWindow
		t    time.Time
		want bool
	}{
		{"day start inclusive", day, at(9, 0), true},
		{"day middle", day, at(12, 30), true},
		{"day end exclusive", day, at(17, 0), false},
		{"day before", day, at(8, 59), false},
		{"night late evening", night, at(23, 30), true},
		{"night after midnight", night, at(3, 0), true},
		{"night end exclusive", night, at(7, 0), false},
		{"night daytime", night, at(15, 0), false},
		{"night start inclusive", night, at(23, 0), true},
	}
	for _, tc := range cases {
		if got := tc.w.contains(tc.t); got != tc.want {
			t.Errorf("%s: contains(%v) = %v, want %v", tc.name, tc.t.Format("15:04"), got, tc.want)
		}
	}
}

func TestWorkWindowNextChange(t *testing.T) {
	night, _ := parseWorkWindow("23:00-07:00")

	cases := []struct {
		name string
		from time.Time
		want time.Time
	}{
		{"daytime waits for start", at(15, 0), at(23, 0)},
		{"inside window ends at 07:00 tomorrow", at(23, 30), at(7, 0).AddDate(0, 0, 1)},
		{"early morning ends today", at(3, 0), at(7, 0)},
		{"exactly at boundary moves to next one", at(7, 0), at(23, 0)},
	}
	for _, tc := range cases {
		if got := night.nextChange(tc.from); !got.Equal(tc.want) {
			t.Errorf("%s: nextChange(%v) = %v, want %v", tc.name, tc.from, got, tc.want)
		}
	}
}
