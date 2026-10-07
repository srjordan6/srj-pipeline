package main

import (
	"strings"
	"testing"
)

// sri.com's index, read 2026-10-07: everything but pages lives in the post
// sitemaps, so the path decides.
func TestSitemapSection(t *testing.T) {
	cases := []struct{ sitemap, url, want string }{
		{"https://www.sri.com/page-sitemap.xml", "https://www.sri.com/about-us/", "core"},
		{"https://www.sri.com/page-sitemap.xml", "https://www.sri.com/timeline-of-innovation/", "core"},
		{"https://www.sri.com/post-sitemap.xml", "https://www.sri.com/press/story/sri-launches-x/", "press"},
		{"https://www.sri.com/post-sitemap3.xml", "https://www.sri.com/people/jane-doe/", "people"},
		{"https://www.sri.com/post-sitemap5.xml", "https://www.sri.com/publication/speech-natural-language-pubs/a-paper/", "publication"},
		{"https://www.sri.com/post-sitemap2.xml", "https://www.sri.com/ja/press/story/x/", "ja"},
		{"https://www.sri.com/category-sitemap.xml", "https://www.sri.com/category/ai/", "taxonomy"},
		{"https://www.sri.com/author-sitemap.xml", "https://www.sri.com/author/someone/", "taxonomy"},
		{"https://www.sri.com/post-sitemap7.xml", "https://www.sri.com/hoi/siri/", "other"},
	}
	for _, c := range cases {
		if got := twoaiSitemapSection(c.sitemap, c.url); got != c.want {
			t.Errorf("%s: got %s, want %s", c.url, got, c.want)
		}
	}
}

func TestSitemapPageParts(t *testing.T) {
	h := `<html><head><title>Siri origins | SRI International</title>
		<meta property="og:title" content="Siri: from DARPA CALO to the iPhone">
		<meta name="description" content="How &amp; why it began.">
		<meta property="article:published_time" content="2023-04-20T19:35:00+00:00">
		</head><body><h1>Ignored</h1></body></html>`
	meta := twoaiSitemapMeta(h)
	if got := twoaiSitemapTitle(h, meta, "SRI International"); got != "Siri: from DARPA CALO to the iPhone" {
		t.Errorf("title: %q", got)
	}
	if meta["description"] != "How & why it began." {
		t.Errorf("description: %q", meta["description"])
	}
	if got := twoaiSitemapDate(h, meta); got != "2023-04-20" {
		t.Errorf("date: %q", got)
	}
	plain := `<title>Board of directors - SRI International</title>`
	if got := twoaiSitemapTitle(plain, twoaiSitemapMeta(plain), "SRI International"); got != "Board of directors" {
		t.Errorf("title without og: %q", got)
	}
}

func TestSitemapWordsIn(t *testing.T) {
	text := strings.ToLower("In 1969 SRI hosted the second node of the ARPANET. In 2007 Siri was spun out.")
	if !twoaiSitemapWordsIn("ARPANET second node", text) {
		t.Error("a milestone in the page's words should pass")
	}
	if twoaiSitemapWordsIn("Invention of the computer mouse", text) {
		t.Error("a milestone the page does not state should fail")
	}
}

func TestSitemapResearchFilter(t *testing.T) {
	if !twoaiSitemapResearchRe.MatchString("Robust speaker recognition in noisy conditions") {
		t.Error("speech paper should be kept")
	}
	if twoaiSitemapResearchRe.MatchString("Effects of a reading intervention in grade 3 classrooms") {
		t.Error("education paper should not be kept")
	}
}

func TestSitemapChrome(t *testing.T) {
	chrome := map[string]bool{"AI": true, "Security": true, "Privacy Policy": true}
	got := twoaiSitemapStripChrome("Alan Braun\nAI\nSecurity\nHe leads quantum sensor work.\nPrivacy Policy", chrome)
	if got != "Alan Braun\nHe leads quantum sensor work." {
		t.Fatalf("got %q", got)
	}
	if twoaiSitemapPeopleRe.MatchString(got) {
		t.Fatal("a quantum physicist should not pass the AI and security filter once the menu is gone")
	}
	h := `<title>Alan Braun - SRI</title>`
	if got := twoaiSitemapTitle(h, twoaiSitemapMeta(h), "SRI International"); got != "Alan Braun" {
		t.Fatalf("title: %q", got)
	}
	h = `<title>Advanced Technology &#038; Systems | SRI</title>`
	if got := twoaiSitemapTitle(h, twoaiSitemapMeta(h), "SRI International"); got != "Advanced Technology & Systems" {
		t.Fatalf("entity title: %q", got)
	}
}
