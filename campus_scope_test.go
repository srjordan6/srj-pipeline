package main

import "testing"

// The sentences are taken verbatim from the pages Stephen pointed at, because
// a scope test written from imagination would pass on imagination. IAD12 and
// CH1 are the two that exposed the bug: both publish real numbers, and both
// numbers describe the campus rather than the building whose page they sit on.
func TestCampusScope(t *testing.T) {
	campus := []string{
		"The Ashburn campus is powered by 80 MW from the Greenway Substation",
		"brings the total capacity of the Digital Ashburn campus to 230MW",
		"1,100,000 square feet across our portfolio",
		"combined 45 MW in this market",
	}
	facility := []string{
		"Critical IT load 12 MW",
		"This building offers 120,000 square feet of raised floor",
		"Total power available to the suite: 3.5 MW",
	}
	for _, s := range campus {
		if !tfCampusRe.MatchString(s) {
			t.Errorf("should be campus scope: %q", s)
		}
	}
	for _, s := range facility {
		if tfCampusRe.MatchString(s) {
			t.Errorf("should be facility scope: %q", s)
		}
	}
}
