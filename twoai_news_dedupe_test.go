package main

import "testing"

func dedupeStoryFor(uid, head string, orgs ...string) *dedupeStory {
	st := map[string]any{"Orgs": []any{}}
	for _, o := range orgs {
		st["Orgs"] = append(st["Orgs"].([]any), o)
	}
	return newsDedupeFrom(uid, "", head, st)
}

// The four NTT DATA pairs the 18:05 run of 2026-10-02 merged on five
// shared words and one actor must not match: the shared words were the
// company's name, ai and two more.
func TestNewsWordRuleNameIsNotAnEvent(t *testing.T) {
	pairs := [][2]string{
		{"NTT DATA Partners with Google Cloud to Accelerate Agentic AI Adoption and Cloud Modernization for Enterprises Globally",
			"AI Ambitions at Risk as Only 14% of Enterprises Fully Realize Cloud Value, NTT DATA Study Finds"},
		{"NTT DATA and Palo Alto Networks Sign Global Strategic Alliance to Accelerate Secure AI Transformation",
			"IP networks critical for global AI economy, NTT DATA sees Asia-Pacific growth"},
		{"NTT DATA Forges Strategic Partnership with Databricks to Advance Data and AI Platforms",
			"NTT DATA and ENGIE announce strategic partnership to power sustainable AI and data center growth - NTT, Inc."},
		{"NTT DATA Named a Market Shaper in Gartner Emerging Market Quadrant for Physical AI Services-Established Vendors",
			"NTT DATA Named a Leader in 2024 ISG Provider Lens Generative AI Services (Global)"},
	}
	for _, p := range pairs {
		a := dedupeStoryFor("a", p[0], "NTT DATA")
		b := dedupeStoryFor("b", p[1], "NTT DATA")
		if r := newsWordRule(a, b); r != "" {
			t.Errorf("merged %q <- %q on %q", p[1], p[0], r)
		}
	}
}

// Genuine duplicates still match, including the five the first cut of the
// name rule wrongly reversed on 2026-10-02 (short headlines that are mostly
// the actors' names because the actors are the event).
func TestNewsWordRuleKeepsRealDuplicates(t *testing.T) {
	type pair struct {
		a, b   string
		actors []string
	}
	pairs := []pair{
		{"Bill Gates warns artificial intelligence 'powerful enough' to cause 'a billion deaths' if unchecked",
			"Bill Gates Warns AI Could Cause 'A Billion Deaths,' Again Calls for Regulation", []string{"Bill Gates"}},
		{"Canada unveils national AI literacy initiative",
			"Government of Canada launches National AI Literacy Initiative", []string{"Government of Canada", "National AI Literacy Initiative", "Canada"}},
		{"AI 'superintelligence' ban proposed by Casar, Sanders",
			"Bernie Sanders and Greg Casar propose AI 'superintelligence' ban with a 20-year jail penalty", []string{"Bernie Sanders", "Greg Casar", "Sanders", "Casar", "Congress", "AI"}},
		{"Gov. Spanberger unveils accountability framework for data centers, launches AI task force",
			"Spanberger issues executive order to hold data centers 'accountable,' form AI task force", []string{"Spanberger", "Virginia", "AI Task Force", "Abigail Spanberger"}},
		{"Trump says US will henceforth call AI 'super intelligence'",
			"Trump Attempts to Rebrand Artificial Intelligence as \"Super Intelligence\"", []string{"Trump", "Donald Trump", "US", "White House"}},
	}
	for _, p := range pairs {
		a := dedupeStoryFor("a", p.a, p.actors...)
		b := dedupeStoryFor("b", p.b, p.actors...)
		if r := newsWordRule(a, b); r == "" {
			t.Errorf("should match: %q <- %q", p.b, p.a)
		}
	}
}
