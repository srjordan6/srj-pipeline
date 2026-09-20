package main

// twoaiLearningExtract: reading a certification or course page for what a
// candidate needs, because the shared extractor was built to throw exactly
// that away.
//
// FOUND ON THE FIRST RUN, 2026-09-19. twoai_learning_readings wrote 27 readings
// and flagged one retirement, AWS Machine Learning Specialty. It should have
// flagged two. Microsoft's page for the Azure AI Engineer Associate says, in a
// warning box at the top, "This certification and the renewal assessment are
// retired." The model never saw that sentence. The harvest had handed it 521
// characters, and the sentence was not among them.
//
// The model was not at fault and neither, really, was the extractor.
// twoaiHarvestExtract was written for industry reports: it keeps paragraphs of
// 120 characters or more that mention AI, because in a forty page report that
// is where the substance is. On a certification page the substance is the
// opposite shape. The retirement notice is 59 characters. "Price: $99" is ten.
// "Exam duration: 120 minutes", "Retirement date", "Renews annually" are all
// short lines with no AI term in them. The filter removed the cost, the
// validity and the status, which are the three things a reader comes for, and
// kept the marketing paragraph. Three course pages, Stanford CS231n, Berkeley
// CS285 and Hugging Face Learn, came back as 190, 156 and 240 characters and
// the model correctly answered NOTHING.
//
// WHAT THIS READER DOES INSTEAD.
//   - Keeps any line of 40 characters or more, with no AI-term test.
//   - Keeps a shorter line, down to 12 characters, when it states a term of
//     the offer: price, fee, renewal, expiry, duration, questions, passing
//     score, prerequisites, required exams, languages.
//   - Pulls every line about retirement, replacement or a currency amount to
//     the TOP of the extract under its own heading, so the cap can never cut
//     the one sentence that changes what the page should say.
//   - Drops cookie banners, sign-in prompts and countdown promotions. The last
//     matters more than it looks: Coursera's "4 days left! Save 40%" counts
//     down daily, and a line that changes daily rewrites the page daily.
//
// THE HASH IS NOT TAKEN FROM THE EXTRACT, for the same reason. Course pages
// carry enrolment counters, review counts and next start dates. Hashing them
// verbatim would regenerate every reading every day, which is the defect that
// was just removed from twoai_enacted_laws. So digits are masked in the body
// before hashing, and kept in the status lines. A new learner count changes
// nothing. A new price, or a new retirement date, changes the hash and the
// reading is rewritten. Tested against fourteen live issuer pages fetched
// twice each: every hash held.
//
// An empty return means the page gave nothing usable, a login wall or a bot
// challenge, and the harvest treats it as a failed fetch and keeps the last
// good extract.

import (
	"regexp"
	"strings"
)

var learnBlockRe = regexp.MustCompile(`(?i)</(p|div|li|h1|h2|h3|h4|h5|td|th|tr|dt|dd|section|article|span|button|a)>|<br\s*/?>`)
var learnTagRe = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<noscript.*?</noscript>|<svg.*?</svg>|<[^>]+>`)
var learnWsRe = regexp.MustCompile(`[ \t\r\f\x{00a0}]+`)
var learnDigitRe = regexp.MustCompile(`\d+`)

// learnStatusRe: a line that says the thing is ending, or states money.
var learnStatusRe = regexp.MustCompile(`(?i)\b(retir\w*|no longer (offered|available|accept\w*)|discontinu\w*|deprecat\w*|supersed\w*|replaced by|will be replaced|last day|end of (life|sale)|sunset\w*)\b|[$€£]\s?\d`)

// learnDetailRe: a short line worth keeping because it states a term of the offer.
var learnDetailRe = regexp.MustCompile(`(?i)\b(expir\w*|renew\w*|recertif\w*|valid for|validity|pric\w+|cost\w*|fees?|USD|free|per month|subscription|duration|minutes|questions|passing score|proctor\w*|prerequisit\w*|recommended experience|exam code|required exams?|level|languages?)\b`)
var learnNoiseRe = regexp.MustCompile(`(?i)cookie|skip to (main )?content|sign in|log in|privacy (policy|statement)|terms of (use|service)|©|all rights reserved|browser is no longer supported|upgrade to microsoft edge|javascript|\bdays? left\b|save \d+%|\bsavings\b|limited[- ]time|\d+% off|free trial|enroll for free|already enrolled`)

// twoaiLearningExtract returns the extract the model reads, and the string the
// change hash is taken from. They differ on purpose; see above.
func twoaiLearningExtract(raw []byte) (extract, hashSrc string) {
	pre := learnBlockRe.ReplaceAllString(string(raw), "\n")
	txt := learnTagRe.ReplaceAllString(pre, " ")
	for _, r := range [][2]string{{"&amp;", "&"}, {"&nbsp;", " "}, {"&#39;", "'"}, {"&#x27;", "'"}, {"&quot;", "\""}, {"&ndash;", "-"}, {"&mdash;", "-"}, {"&rsquo;", "'"}} {
		txt = strings.ReplaceAll(txt, r[0], r[1])
	}
	txt = learnWsRe.ReplaceAllString(txt, " ")
	seen := map[string]bool{}
	var status, body []string
	sLen, bLen := 0, 0
	for _, ln := range strings.Split(txt, "\n") {
		ln = strings.TrimSpace(ln)
		if len(ln) < 12 || len(ln) > 1200 || learnNoiseRe.MatchString(ln) {
			continue
		}
		key := strings.ToLower(ln)
		if seen[key] {
			continue
		}
		isStatus := learnStatusRe.MatchString(ln)
		if !isStatus && len(ln) < 40 && !learnDetailRe.MatchString(ln) {
			continue
		}
		seen[key] = true
		if isStatus && sLen < 1500 {
			status = append(status, ln)
			sLen += len(ln)
			continue
		}
		if bLen < 7500 {
			body = append(body, ln)
			bLen += len(ln)
		}
	}
	// Too little to be a page about anything: a challenge screen or a wall.
	if len(status) == 0 && bLen < 200 {
		return "", ""
	}
	var b strings.Builder
	if len(status) > 0 {
		b.WriteString("LINES ON THE PAGE ABOUT RETIREMENT, REPLACEMENT OR PRICE:\n" + strings.Join(status, "\n") + "\n\nREST OF THE PAGE:\n")
	}
	b.WriteString(strings.Join(body, "\n"))
	extract = b.String()
	// The hash ignores digits in the body, so an enrolment counter or a start
	// date does not rewrite the page every day, and keeps them in the status
	// lines, so a new price or a new retirement date does.
	hashSrc = strings.Join(status, "\n") + "\n##\n" + learnDigitRe.ReplaceAllString(strings.Join(body, "\n"), "#")
	return extract, hashSrc
}
