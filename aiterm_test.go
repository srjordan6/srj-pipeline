package main

import "testing"

// The vocabulary must catch what legislatures actually title bills, and must
// not catch generic technology or ordinary words. Both halves are the test.
func TestAITermVocabulary(t *testing.T) {
	must := []string{
		"Artificial Intelligence Accountability Act",
		"Companion chatbots: children's safety.",
		"Customer service chatbots.",
		"Synthetic Media: Liability; Elections",
		"Digital replicas.",
		"Relating to deepfake images of a person",
		"Automated decision-making tools in hiring",
		"Algorithmic pricing prohibition",
		"Facial recognition technology; law enforcement use",
		"Large language model transparency",
		"Prohibiting voice cloning without consent",
		"Autonomous vehicle testing requirements",
		"Machine learning in insurance underwriting",
		"Predictive policing ban",
	}
	for _, s := range must {
		if !mentionsAI(s) {
			t.Errorf("MISSED: %q", s)
		}
	}
	mustNot := []string{
		"Declaring a National Emergency To Secure the United States Bulk-Power System",
		"Maximizing Efficiencies in Universal Service Administration",
		"Critical Position Pay Authority",
		"Regulation Crypto Assets",
		"Airworthiness Directives; The Boeing Company Airplanes",
		"An Act relating to the maintenance of state highways",
		"Chairperson appointment; advisory board",
		"Said property shall be conveyed to the county",
		"Interest rate calculation for consumer loans",
		"Automation of payroll tax remittance",
		"Adjusting Imports of Polysilicon and Its Derivatives",
	}
	for _, s := range mustNot {
		if mentionsAI(s) {
			t.Errorf("FALSE POSITIVE: %q", s)
		}
	}
}
