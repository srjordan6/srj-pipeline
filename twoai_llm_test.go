package main

import (
	"testing"
	"time"
)

// Off-peak is outside 12:00 to 18:00 UTC on weekdays and all day at weekends.
func TestTwoaiPeakAt(t *testing.T) {
	cases := []struct {
		when string
		peak bool
	}{
		{"2026-10-02T11:59:00Z", false},     // Friday, just before
		{"2026-10-02T12:00:00Z", true},      // Friday, window opens
		{"2026-10-02T17:59:00Z", true},      // Friday, still inside
		{"2026-10-02T18:00:00Z", false},     // Friday, window closes
		{"2026-10-03T15:00:00Z", false},     // Saturday afternoon
		{"2026-10-04T15:00:00Z", false},     // Sunday afternoon
		{"2026-10-05T15:00:00Z", true},      // Monday afternoon
		{"2026-10-02T09:00:00-05:00", true}, // 14:00 UTC written in Central time
	}
	for _, c := range cases {
		tm, err := time.Parse(time.RFC3339, c.when)
		if err != nil {
			t.Fatal(err)
		}
		if got := twoaiPeakAt(tm); got != c.peak {
			t.Errorf("twoaiPeakAt(%s) = %v, want %v", c.when, got, c.peak)
		}
	}
}
