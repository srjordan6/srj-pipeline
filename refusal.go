package main

// isRefusal: a language model declining to do the job is not the job's output.
//
// On 2026-09-06 theworldofai.org published, as the summary of a story about
// New York City banning AI in K-8 classrooms: "I appreciate you sharing this,
// but I'm unable to complete your request." It sat on the daily briefing under
// a real headline, beside a real outlet count, looking exactly like reporting.
//
// The model was not at fault. All four refusals found in pipeline.documents
// came from pages whose fetched text was navigation markup or JavaScript
// rather than an article - two video pages (CBS, WBNG), a Chosun page and a
// UPI page. Asked to summarise nothing, it correctly said it could not. The
// fault was entirely ours: the pipeline stored the sentence as though it were
// prose and the site printed it.
//
// TWO SIGNALS, NOT ONE. A single phrase is not enough to condemn a summary,
// because real reporting says these things. "The judge said she was unable to
// complete the hearing" is news. So is a chief executive quoted saying "I'm
// sorry". So is "the text provided to jurors ran to 400 pages".
//
//   hard   - phrasings that only ever open a refusal, at the very start of the
//            text: "I'm sorry/unable/not able", "I can't help/assist/complete",
//            "Unfortunately, I cannot", "As an AI language model". Anchored, so
//            a quoted apology inside an article cannot match.
//   soft   - a courtesy opener that often precedes a refusal but is innocent on
//            its own: "I appreciate", "Thank you for", "The text you provided".
//   marker - evidence the model is describing its own inability or the page's
//            emptiness: "I'm unable to", "contains only the headline",
//            "appears to be JavaScript", "rather than the actual article".
//
// A soft opener convicts only with a marker beside it in the first 400
// characters, where a refusal always lives. Deliberately, "the text provided"
// is NOT a marker, or a soft opener would convict itself.
//
// Tested against all four real refusals plus five constructed ones, and
// against nine pieces of real or realistic prose built to trip it, including
// every phrase above used innocently: 9 of 9 caught, 9 of 9 kept.
//
// A false positive costs one summary and the caller falls through to the next
// article in the cluster. A false negative puts a chatbot's voice in front of
// a reader as though it were the news. The pattern is tight for that reason.

import (
	"regexp"
	"strings"
)

const refusalLead = `^[\s\-*_>]{0,4}`

var refusalHardRe = regexp.MustCompile(`(?i)` + refusalLead +
	`((i'?m|i am)\s+(sorry|unable|not able|afraid)\b` +
	`|(i\s+)?can'?t\s+(help|assist|complete|summari[sz]e|provide)\b` +
	`|(i\s+)?cannot\s+(help|assist|complete|summari[sz]e|provide)\b` +
	`|unfortunately,?\s+i\s+(can|am|cannot)\b` +
	`|as an ai (language )?model\b)`)

var refusalSoftRe = regexp.MustCompile(`(?i)` + refusalLead +
	`(i\s+(appreciate|apologi[sz]e|regret)\b` +
	`|thank you for\b` +
	`|the (text|content|article)\s+(you\s+)?(provided|shared)\b)`)

var refusalMarkerRe = regexp.MustCompile(`(?i)\b(` +
	`(i'?m|i am)\s+unable to\b` +
	`|i\s+(cannot|can'?t)\s+(complete|summari[sz]e|provide|help|assist)\b` +
	`|appears to be (corrupted|website code|javascript|navigation)` +
	`|contains (only|mostly) (the headline|javascript|navigation|website)` +
	`|only contains the headline` +
	`|rather than the actual (news )?article)`)

func isRefusal(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	if refusalHardRe.MatchString(t) {
		return true
	}
	head := t
	if len(head) > 400 {
		head = head[:400]
	}
	return refusalSoftRe.MatchString(t) && refusalMarkerRe.MatchString(head)
}
