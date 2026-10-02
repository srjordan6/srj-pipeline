package main

import "testing"

// The cases are the ones bridge row 354 named, plus the shapes it said to
// leave alone.
func TestNewsStripOutlet(t *testing.T) {
	cases := []struct {
		title   string
		outlets []string
		want    string
	}{
		{"Japan court rules human voice is protected in TikTok AI case - Yahoo", []string{"news.yahoo.com"}, "Japan court rules human voice is protected in TikTok AI case"},
		{"Fed weighs AI risk in bank supervision - CBS News", []string{"cbsnews.com"}, "Fed weighs AI risk in bank supervision"},
		{"PH firms urged to adopt AI governance | The Manila Times", []string{"manilatimes.net"}, "PH firms urged to adopt AI governance"},
		{"Pakistan drafts AI policy – The Frontier Post", []string{"thefrontierpost.com"}, "Pakistan drafts AI policy"},
		{"Nvidia results beat estimates — Bloomberg", []string{"www.bloomberg.com"}, "Nvidia results beat estimates"},
		{"Student wins science fair with help from AI", []string{"cbsnews.com"}, "Student wins science fair with help from AI"},
		{"Stocks to watch today - August 8th", []string{"finance.yahoo.com"}, "Stocks to watch today - August 8th"},
		{"Boards face new duties, including AI", []string{"law.com"}, "Boards face new duties, including AI"},
		{"Company X expands into defense - Complete Financial Solutions", []string{"intelligencer.ca"}, "Company X expands into defense - Complete Financial Solutions"},
		{"TechGig story title - TechGig", []string{"techgig.com"}, "TechGig story title"},
		{"No outlets given - Yahoo", nil, "No outlets given - Yahoo"},
	}
	for _, c := range cases {
		if got := newsStripOutlet(c.title, c.outlets...); got != c.want {
			t.Errorf("newsStripOutlet(%q, %v) = %q, want %q", c.title, c.outlets, got, c.want)
		}
	}
}
