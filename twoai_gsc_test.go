package main

import "testing"

func TestGSCSection(t *testing.T) {
	cases := map[string]string{
		"https://theworldofai.org/":                                                          "home",
		"https://theworldofai.org/ai-news/cves/cve-2026-1234/":                               "ai-news/cves",
		"https://theworldofai.org/ai-news/some-story-slug/":                                  "ai-news/story",
		"https://theworldofai.org/ai-ecosystem/technology-and-core-infrastructure/1a2b3c4d/": "ai-ecosystem/technology-and-core-infrastructure",
		"https://theworldofai.org/research/paper/abc/":                                       "research/paper",
		"https://theworldofai.org/companies/d7018517/":                                       "companies",
		"https://theworldofai.org/ai-glossary/multics/":                                      "ai-glossary",
	}
	for u, want := range cases {
		if got := twoaiGSCSection(u); got != want {
			t.Errorf("%s: got %s, want %s", u, got, want)
		}
	}
}
