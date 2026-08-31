package main

import "testing"

// The picker's job is mostly refusing. These assert the refusals, because a
// discovery stage that returns something plausible for every row would put a
// neighbour's capacity on a building's page, which is the failure this whole
// day has been about.
func TestPickFacilityURL(t *testing.T) {
	index := "https://h5datacenters.com/ashburn-data-center.html"

	// One deeper page on the operator's own domain: take it.
	got := twoaiPickFacilityURL([]discoverHit{
		{"https://www.datacenterknowledge.com/h5-ashburn-expands", "trade press"},
		{"https://h5datacenters.com/data-centers/ashburn/ash1/", "H5 Ashburn ASH1"},
	}, index)
	if got != "https://h5datacenters.com/data-centers/ashburn/ash1/" {
		t.Errorf("got %q, want the operator's own deeper page", got)
	}

	// Two candidates on the domain: refuse, because choosing between two
	// buildings by search rank is guessing.
	if u := twoaiPickFacilityURL([]discoverHit{
		{"https://h5datacenters.com/data-centers/ashburn/ash1/", "ASH1"},
		{"https://h5datacenters.com/data-centers/ashburn/ash2/", "ASH2"},
	}, index); u != "" {
		t.Errorf("picked %q from two candidates", u)
	}

	// Only off-domain results: refuse. A press release is somebody talking
	// about the operator, not the operator publishing.
	if u := twoaiPickFacilityURL([]discoverHit{
		{"https://www.prnewswire.com/h5-ashburn", "press release"},
		{"https://en.wikipedia.org/wiki/H5_Data_Centers", "encyclopedia"},
	}, index); u != "" {
		t.Errorf("accepted an off-domain result: %q", u)
	}

	// A result no deeper than the index is the index again.
	if u := twoaiPickFacilityURL([]discoverHit{
		{"https://h5datacenters.com/locations.html", "Locations"},
	}, index); u != "" {
		t.Errorf("accepted another index page: %q", u)
	}

	// A subdomain is the same registrable domain and should be accepted.
	if u := twoaiPickFacilityURL([]discoverHit{
		{"https://www.h5datacenters.com/data-centers/ashburn/ash1/", "ASH1"},
	}, index); u == "" {
		t.Error("rejected the operator's own www subdomain")
	}

	// A lookalike domain must never pass as the operator.
	if u := twoaiPickFacilityURL([]discoverHit{
		{"https://h5datacenters.co/data-centers/ashburn/ash1/", "lookalike"},
	}, index); u != "" {
		t.Errorf("accepted a lookalike domain: %q", u)
	}
}
