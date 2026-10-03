package main

import "testing"

// Shapes copied from MITRE's 1000.csv of 2026-10-02.
func TestCWEFieldsMitigations(t *testing.T) {
	f := "::PHASE:Implementation:STRATEGY:Input Validation:DESCRIPTION:Assume all input is malicious. Use an accept known good input validation strategy.::PHASE:Architecture and Design:PHASE:Operation:STRATEGY:Environment Hardening:DESCRIPTION:Run your code using the lowest privileges, see https://example.org/a:b for std::string handling.:EFFECTIVENESS:High::"
	got := cweFields(f, "PHASE", "STRATEGY", "DESCRIPTION", "EFFECTIVENESS_NOTES", "EFFECTIVENESS")
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %v", len(got), got)
	}
	if cweFirst(got[0], "STRATEGY") != "Input Validation" || cweFirst(got[0], "PHASE") != "Implementation" {
		t.Errorf("first entry wrong: %v", got[0])
	}
	if len(got[1]["PHASE"]) != 2 {
		t.Errorf("second entry should hold two phases: %v", got[1]["PHASE"])
	}
	if d := cweFirst(got[1], "DESCRIPTION"); d != "Run your code using the lowest privileges, see https://example.org/a:b for std::string handling." {
		t.Errorf("a colon or :: inside a value must survive, got %q", d)
	}
	if cweFirst(got[1], "EFFECTIVENESS") != "High" {
		t.Errorf("effectiveness lost: %v", got[1])
	}
}

func TestCWEFieldsConsequences(t *testing.T) {
	f := "::SCOPE:Confidentiality:IMPACT:Read Application Data::SCOPE:Integrity:IMPACT:Execute Unauthorized Code or Commands::SCOPE:Access Control:IMPACT:Bypass Protection Mechanism:NOTE:By providing URLs to unexpected hosts or ports, attackers can make it appear that the server is sending the request.::"
	got := cweFields(f, "SCOPE", "IMPACT", "LIKELIHOOD", "NOTE")
	if len(got) != 3 {
		t.Fatalf("want 3 consequences, got %d", len(got))
	}
	if cweFirst(got[2], "NOTE") == "" || cweFirst(got[0], "IMPACT") != "Read Application Data" {
		t.Errorf("consequences parsed wrong: %v", got)
	}
}

func TestCWEFieldsEmpty(t *testing.T) {
	if got := cweFields("", "PHASE"); got != nil {
		t.Errorf("empty field should give nil, got %v", got)
	}
}
