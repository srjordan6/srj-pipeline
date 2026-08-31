package main

import "testing"

// Ideogram is the case that makes this test necessary. Searching Wikidata for
// the name returns a linguistics term as the top hit, with no website at all.
// Accepting the best text match would stamp an unrelated item's facts onto an
// AI company's page, so the domain has to be the thing that decides.
func TestWikidataRequiresDomainMatch(t *testing.T) {
	// The rule is asserted directly on the domain comparison rather than
	// through a stubbed HTTP server: the network shape is not what fails
	// here, the identity check is.
	if twoaiRegistrableHost("https://someone-else.com/") == twoaiRegistrableHost("https://ideogram.ai/") {
		t.Fatal("test premise wrong: those are different domains")
	}
	// An item with no P856 can never match.
	if twoaiRegistrableHost("") != "" {
		t.Error("an empty website must not produce a host")
	}
	// A www subdomain is the same registrable domain and must match.
	if twoaiRegistrableHost("https://www.langchain.com/") != twoaiRegistrableHost("https://langchain.com/") {
		t.Error("www subdomain should match the bare domain")
	}
	// A lookalike must not.
	if twoaiRegistrableHost("https://langchain.co/") == twoaiRegistrableHost("https://langchain.com/") {
		t.Error("a lookalike domain must not match")
	}
}

// The year is the only part of a Wikidata inception these items reliably
// carry, and a fabricated month would read as precision we do not have.
func TestWikidataYearParsing(t *testing.T) {
	claims := map[string]any{
		"P571": []any{map[string]any{"mainsnak": map[string]any{
			"datavalue": map[string]any{"value": map[string]any{"time": "+2019-00-00T00:00:00Z"}}}}},
	}
	v, ok := wdClaimValue(claims, "P571").(map[string]any)
	if !ok {
		t.Fatal("could not read the inception claim")
	}
	if ts, _ := v["time"].(string); ts[1:5] != "2019" {
		t.Errorf("year = %q, want 2019", ts[1:5])
	}
	if wdClaimValue(claims, "P159") != nil {
		t.Error("an absent property must return nothing")
	}
}
