package main

import "testing"

func TestNewsCanonURL(t *testing.T) {
	cases := map[string]string{
		// The RTHK pair of bridge row 409: one stored with a literal \u003d.
		`https://news.rthk.hk/rthk/en/component/k2/1871960-20260929.htm?spTabChangeable\u003d0`: "https://news.rthk.hk/rthk/en/component/k2/1871960-20260929.htm",
		"https://news.rthk.hk/rthk/en/component/k2/1871960-20260929.htm":                        "https://news.rthk.hk/rthk/en/component/k2/1871960-20260929.htm",
		"https://Example.com/a?id=4&utm_source=x#top":                                           "https://example.com/a?id=4",
		"https://example.com/a?b=2&a=1":                                                         "https://example.com/a?b=2&a=1",
	}
	for in, want := range cases {
		if got := newsCanonURL(in); got != want {
			t.Errorf("newsCanonURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewsTitleEntitiesWorldLabs(t *testing.T) {
	dict := map[string]string{"amd": "company", "nvidia": "company", "fei fei li": "person"}
	titles := []string{
		"AMD to Acquire World Labs to Advance the Future of AI Compute",
		"AMD Buys World Labs For $8.2 Billion To Get AI Model Cred",
		"AMD Stock: World Labs Acquisition Bolsters AI Expertise",
		"AMD buys World Labs founded by 'Godmother of AI'",
		"Nvidia rival AMD to buy industry pioneer's AI startup World Labs for US$8.2 billion",
	}
	vocab := newsEntityVocab(dict, titles, []string{"world lab", "exchange commission", "xilinx"})
	if vocab["world lab"] != "gdelt" {
		t.Fatalf("world lab not added to the vocabulary: %v", vocab)
	}
	if _, ok := vocab["exchange commission"]; ok {
		t.Errorf("a GDELT name absent from every headline was added")
	}
	for _, ti := range titles {
		es := newsTitleEntities(ti, vocab)
		has := map[string]bool{}
		for _, e := range es {
			has[e] = true
		}
		if !has["amd"] || !has["world lab"] {
			t.Errorf("%q: entities %v, want amd and world lab", ti, es)
		}
	}
	a := map[string]bool{"amd": true, "world lab": true}
	b := map[string]bool{"amd": true, "world lab": true, "nvidia": true}
	if n := newsSharedEntities(a, b, map[string]bool{}); n != 2 {
		t.Errorf("shared = %d, want 2", n)
	}
	if n := newsSharedEntities(a, b, map[string]bool{"amd": true}); n != 1 {
		t.Errorf("shared with amd ubiquitous = %d, want 1", n)
	}
}

func TestNewsEntNormPlural(t *testing.T) {
	if got := newsEntNorm("World Labs"); got != "world lab" {
		t.Errorf("got %q", got)
	}
	if got := newsEntNorm("Fei-Fei Li"); got != "fei fei li" {
		t.Errorf("got %q", got)
	}
	if got := newsEntNorm("Business"); got != "business" {
		t.Errorf("double s kept: got %q", got)
	}
}

func TestNewsProductKeysArgon(t *testing.T) {
	vocab := map[string]string{"google": "company", "gemini": "model"}
	titles := []string{
		"Gemini 4 Argon: Check How It Works, Features and Difference From Other AI Chatbots",
		"Google's Gemini 4 Argon is the latest super-smart AI that most cannot get",
		"Google Enters Cyber AI Race With Gemini 4 Argon",
		"Google Unveils Gemini 4 Argon, Its Most Advanced AI Model Yet",
	}
	for _, ti := range titles {
		ks := newsProductKeys(ti, vocab)
		found := false
		for _, k := range ks {
			if k == "gemini argon" {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: keys %v, want gemini argon", ti, ks)
		}
	}
	if ks := newsProductKeys("OpenAI says Gemini is popular", vocab); len(ks) != 0 {
		t.Errorf("no product name expected, got %v", ks)
	}
}
