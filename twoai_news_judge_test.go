package main

import "testing"

// The two cases of bridge row 414.
func TestNewsBundleManilaTimes(t *testing.T) {
	titles := []string{
		"Distorted intelligence at the AI frontier | The Manila Times",
		"Asia factory activity grows due to AI boom | The Manila Times",
		"BMW bets on cuts, new models, AI | The Manila Times",
		"Google announces Argon AI model | The Manila Times",
	}
	if !newsBundle(titles, map[string]bool{"manilatimes.net": true}) {
		t.Fatal("seven unrelated Manila Times pieces must read as a bundle")
	}
	a := newsTitleTokens(newsStripOutlet(titles[0], "manilatimes.net"))
	b := newsTitleTokens(newsStripOutlet(titles[2], "manilatimes.net"))
	if a["manila"] || a["times"] {
		t.Errorf("outlet name left in the headline words: %v", a)
	}
	if newsTokSim(a, b) >= 0.6 {
		t.Errorf("unrelated headlines still look alike: %v %v", a, b)
	}
}

func TestNewsSameOutletNTTData(t *testing.T) {
	dom := "nttdata.com"
	t1 := "Enterprise AI Hits the Wall: NTT DATA Research Reveals Growing Privacy and Sovereignty Barriers"
	t2 := "NTT DATA unveils NVIDIA-powered enterprise AI factories - NTT Data"
	if !newsIsOutletEntity("ntt data", dom) {
		t.Fatal("NTT DATA on nttdata.com is the outlet")
	}
	entA := map[string]bool{"ntt data": true}
	entB := map[string]bool{"ntt data": true, "nvidia": true}
	if newsSameOutletStory(newsTitleTokens(newsStripOutlet(t1, dom)), newsTitleTokens(newsStripOutlet(t2, dom)), entA, entB, dom) {
		t.Error("two different NTT DATA releases must stay apart")
	}
	if !newsBundle([]string{t1, t2}, map[string]bool{dom: true}) {
		t.Error("the pair is a one-outlet bundle")
	}
	// One outlet, one event, a shared subject that is not the outlet: kept.
	r1 := "Reuters: AMD to buy World Labs for $8.2 billion"
	r2 := "AMD agrees to buy World Labs for $8.2 billion in stock"
	ea := map[string]bool{"amd": true, "world lab": true}
	if !newsSameOutletStory(newsTitleTokens(r1), newsTitleTokens(r2), ea, ea, "reuters.com") {
		t.Error("one outlet's two pieces on one event should still group")
	}
}
