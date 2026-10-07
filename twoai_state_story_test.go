package main

import (
	"regexp"
	"testing"
)

// Row 566: the Utah page listed a Trump roundup and a cellphone piece and
// missed the Utah executive order.
func TestTwoaiStateStory(t *testing.T) {
	policy := regexp.MustCompile(`(?i)\b(law|laws|bill|bills|legislat\w*|executive order|governor|gov\.|attorney general|regulat\w*|statute|ban|lawmakers|general assembly|senate|house)\b`)
	utah := regexp.MustCompile(`\bUtah\b`)
	if !twoaiStateStory(utah, policy, "Utah Gov. Cox signs executive order on AI integration in government", "Utah Governor Spencer Cox signed an executive order creating a framework.") {
		t.Error("the Utah order is a Utah story")
	}
	if twoaiStateStory(utah, policy, "What we're reading: Trump signs new AI executive order, pilot stabs co-pilot and more",
		"President Trump signed documents for voluntary self regulation of AI. Hurricane Polo hit Mexico. A Utah hiker was rescued after three days.") {
		t.Error("a roundup that mentions Utah in passing is not a Utah story")
	}
	if !twoaiStateStory(utah, policy, "States move on AI in schools", "Several states acted this week. Utah lawmakers advanced a bill on AI tutors in classrooms. Texas did not.") {
		t.Error("a sentence naming Utah beside a policy word routes")
	}
}
