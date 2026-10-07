package main

import (
	"testing"
	"time"
)

func TestNewsTooOld(t *testing.T) {
	asOf := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	cases := map[string]bool{
		"2026-07-17":           true, // 0fe54ab6's July article
		"2026-10-06T12:21:08Z": false,
		"2026-09-24":           false,
		"2026-09-22":           true,
		"":                     false,
		"not a date":           false,
	}
	for d, want := range cases {
		if got := newsTooOld(d, asOf); got != want {
			t.Errorf("%q: got %v, want %v", d, got, want)
		}
	}
}

// The four Australian inquiry stories of 2026-10-06 and 07, with the names
// GDELT extracted for them.
func TestNewsSameEventByNames(t *testing.T) {
	anthropic := newsNameSet([]string{"committee on artificial intelligence;australian broadcasting corporation;reuters", "david masters;kate gilchrist;david orr;byron kaye;edwina gibbs"})
	abc := newsNameSet([]string{"reuters;australian broadcasting corporation;committee on artificial intelligence", "kate gilchrist;david sutton;byron kaye;neil fullick"})
	sovereignty := newsNameSet([]string{"committee on artificial intelligence;parliament of new south wales;australian broadcasting corporation;reuters;services australia;google", "jason kwon;sam altman;david masters;david orr;anthony albanese;hollie adams"})
	other := newsNameSet([]string{"google;bloomberg", "iyaz akhtar;cole kan;demis hassabis"})
	common := newsCommon([]map[string]bool{anthropic, abc, sovereignty, other}, 5)
	if !newsSameEventByNames(anthropic, abc, common, 0.1) {
		t.Error("Anthropic and ABC testimony at one inquiry should merge")
	}
	if !newsSameEventByNames(anthropic, sovereignty, common, 0) {
		t.Error("three shared names should merge without headline overlap")
	}
	if newsSameEventByNames(anthropic, other, common, 0.5) {
		t.Error("unrelated stories must not merge")
	}
	// Reuters is a byline, never evidence.
	if newsNameSet([]string{"reuters;associated press"})["reuters"] {
		t.Error("wire agencies are excluded")
	}
}

func TestNewsSameEventByRareWords(t *testing.T) {
	a := newsTitleTokens("Federal appeals court pauses Minnesota's AI nudification technology ban")
	b := newsTitleTokens("Elon Musk's xAI Wins Block of Minnesota's AI 'Nudification' Ban")
	c := newsTitleTokens("Google Gemini Live Avatar: what it can do")
	d := newsTitleTokens("States weigh AI ban in schools")
	rare := map[string]bool{}
	df := map[string]int{}
	for _, s := range []map[string]bool{a, b, c, d} {
		for k := range s {
			df[k]++
		}
	}
	for k, n := range df {
		if n <= 3 {
			rare[k] = true
		}
	}
	delete(rare, "ban") // as if many stories that day said ban
	if !newsSameEventByRareWords(a, b, rare) {
		t.Errorf("Minnesota nudification stories should merge: a=%v b=%v", a, b)
	}
	if newsSameEventByRareWords(a, c, rare) {
		t.Error("unrelated stories must not merge")
	}
}

func TestNewsSummaryFits(t *testing.T) {
	h := "Australia's ABC rejects AI copyright carveout, believes already been scraped"
	if newsSummaryFits(h, "Anthropic told the committee it could support rules on government systems and agent security.") {
		t.Error("a summary about Anthropic does not fit a headline about the ABC and copyright")
	}
	if !newsSummaryFits(h, "The ABC told the inquiry it opposes a copyright exception for AI training, saying its archive has already been scraped.") {
		t.Error("the ABC's own summary fits")
	}
}
