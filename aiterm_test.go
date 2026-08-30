package main

import "testing"

// The vocabulary must catch what legislatures and regulators actually title
// documents, and must not catch generic technology or ordinary words. Both
// halves are the test.
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
		// A test expectation corrected rather than the code bent to it:
		// this title names AI infrastructure and no data center word, so it
		// belongs to the AI vocabulary, not the facility one.
		"Request for Information on Artificial Intelligence Infrastructure on DOE Lands",
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

// The data center vocabulary carries the layer underneath AI, in every
// spelling sources use. It must not fire on the ordinary sense of "center".
func TestDCTermVocabulary(t *testing.T) {
	must := []string{
		"Bolstering Data Center Growth, Resilience, and Security",
		"Notice of datacenter interconnection agreement",
		"Data Centre Energy Efficiency Directive reporting",
		"Hyperscale campus zoning ordinance",
		"Large load interconnection queue reform",
		"Accelerating Speed to Power for AI facilities",
		"Colocation tenant power allocation",
		"Behind-the-meter generation for computing loads",
	}
	for _, s := range must {
		if !mentionsDC(s) {
			t.Errorf("MISSED: %q", s)
		}
	}
	mustNot := []string{
		"Notice of Public Meeting of the Maryland Advisory Committee",
		"Community Outreach Office Locations in the Southeast",
		"Medical center accreditation standards",
		"Center for Disease Control funding authorization",
		"Regulation Crypto Assets",
	}
	for _, s := range mustNot {
		if mentionsDC(s) {
			t.Errorf("FALSE POSITIVE: %q", s)
		}
	}
}

// The two subjects together are what earns a corpus row.
func TestOnSubject(t *testing.T) {
	if !onSubject("Bolstering Data Center Growth", "") {
		t.Error("data center title should be on subject")
	}
	if !onSubject("Notice of rulemaking", "The rule governs automated decision systems used in hiring.") {
		t.Error("abstract mention should be on subject")
	}
	if onSubject("Airworthiness Directives; The Boeing Company Airplanes", "Requires inspection of the wing spar.") {
		t.Error("unrelated document should not be on subject")
	}
}
