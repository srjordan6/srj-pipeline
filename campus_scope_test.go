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

// The Dallas metro page carries all three scopes within a few sentences.
// Only the IT-load figure belongs to a single building, and the campus rule
// alone would have discarded it along with the other two.
func TestFacilityBeatsCampus(t *testing.T) {
	facilityWins := []string{
		"The data center services in Ft. Worth/Dallas facility features 69,867 square feet of raised floor space, 6.75 MW of IT load",
		"multitenant facility boasts approximately 60,687 square feet of raised floor space and 6.75MW of IT load",
	}
	campusWins := []string{
		"the Dallas-Fort Worth data center market at 28% (1,840 MW of IT power) between 2023 and 2028",
		"our dedicated, privately owned, substation with the ability to deliver up to 100 MW of power",
	}
	for _, s := range facilityWins {
		if tfCampusRe.MatchString(s) && !tfFacilityRe.MatchString(s) {
			t.Errorf("facility figure demoted to campus: %q", s)
		}
	}
	for _, s := range campusWins {
		if tfFacilityRe.MatchString(s) && tfCampusRe.MatchString(s) {
			t.Errorf("campus figure promoted to facility: %q", s)
		}
	}
}
