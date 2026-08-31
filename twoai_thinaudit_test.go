package main

import "testing"

// The old list held "Last verified" but not a bare "Verified", so 92 lawsuit
// pages rendering "Filed 2026-08-14 · Verified 2026-08-31" were counted as
// carrying no date at all. These strings are taken from live pages.
func TestAuditDateDetection(t *testing.T) {
	dated := []string{
		"Filed 2026-08-14 · Verified 2026-08-31",
		"Last verified: 2026-08-30",
		"Written 2026-08-31 from the docket",
		"Generated 2026-08-31",
		"Decided 2019-04-23",
	}
	undated := []string{
		"Verified by our editorial team", // a claim, not a stamp
		"Updated regularly",              // the same
		"2026-08-31",                     // a bare date says nothing about what it dates
		"The case was filed in Delaware", // prose
	}
	for _, s := range dated {
		if !auditDateRe.MatchString(s) {
			t.Errorf("should count as dated: %q", s)
		}
	}
	for _, s := range undated {
		if auditDateRe.MatchString(s) {
			t.Errorf("should NOT count as dated: %q", s)
		}
	}
}

// The news factory writes a UTC string, not an ISO date. A rule that only
// accepted ISO would have left 262 news pages counted as undated after the
// word list was already widened, which is how a fix produces the same wrong
// number twice.
func TestAuditDateAcceptsUTCStamp(t *testing.T) {
	for _, s := range []string{
		"Daily briefing · 2026-08-30 · compiled Sun, 31 Aug 2026 UTC · 4-minute read",
		"compiled 31 Aug 2026",
		"Last reviewed 3 September 2026",
	} {
		if !auditDateRe.MatchString(s) {
			t.Errorf("should count as dated: %q", s)
		}
	}
	if auditDateRe.MatchString("compiled by hand from several outlets") {
		t.Error("a label with no date must not count")
	}
}
