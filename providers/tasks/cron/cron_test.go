package cron

import (
	"testing"
	"time"
)

// TestVendoredParserSanity confirms the vendored parser handles the cron
// surface we promised: standard 5-field, descriptor tags, Quartz L/W/#.
// Not exhaustive — gronx's own test suite covers parsing edge cases.
// This test exists so a future refactor that breaks something obvious
// fails immediately.
func TestVendoredParserSanity(t *testing.T) {
	g := New()

	cases := []struct {
		expr  string
		valid bool
	}{
		// Standard 5-field
		{"* * * * *", true},
		{"0 9 * * 1-5", true},
		{"*/15 * * * *", true},
		// 6-field with seconds
		{"0 0 12 * * *", true},
		// Tags
		{"@hourly", true},
		{"@daily", true},
		{"@5minutes", true},
		{"@30minutes", true},
		{"@everysecond", true},
		// Quartz extensions — the reason we picked gronx over robfig
		{"0 0 L * *", true},      // last day of month
		{"0 0 1W * *", true},     // closest weekday to the 1st
		{"0 0 * * 1#2", true},    // 2nd Monday of every month
		{"0 0 * * 5L", true},     // last Friday of every month
		// Garbage
		{"", false},
		{"not a cron", false},
		{"60 * * * *", false}, // minute > 59
	}
	for _, tc := range cases {
		got := g.IsValid(tc.expr)
		if got != tc.valid {
			t.Errorf("IsValid(%q) = %v, want %v", tc.expr, got, tc.valid)
		}
	}
}

func TestVendoredParserNextTick(t *testing.T) {
	// "every minute" starting at noon — next tick must be 12:01.
	start := time.Date(2026, 5, 2, 12, 0, 30, 0, time.UTC)
	next, err := NextTickAfter("* * * * *", start, false)
	if err != nil {
		t.Fatalf("NextTickAfter: %v", err)
	}
	want := time.Date(2026, 5, 2, 12, 1, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next tick: got %v want %v", next, want)
	}
}

func TestVendoredParserNextTickQuartzL(t *testing.T) {
	// "last day of month at noon" starting Feb 15 2026 — next is Feb 28.
	start := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)
	next, err := NextTickAfter("0 12 L * *", start, false)
	if err != nil {
		t.Fatalf("NextTickAfter: %v", err)
	}
	want := time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("L for Feb 2026: got %v want %v", next, want)
	}
}
