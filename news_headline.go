package main

// news_headline: the outlet name does not belong in a headline.
//
// theworldofai, bridge row 354 (Stephen, 2026-10-02): /ai-news/dd7aa204/ was
// headlined "Japan court rules human voice is protected in TikTok AI case -
// Yahoo", which reads as if the case were against Yahoo. Google News appends
// the publisher to every title it hands out (" - Yahoo", " - CBS News",
// " | The Manila Times", " – The Frontier Post"), and 399 live stories carried
// it. They were cleaned in SQL. This is the rule that keeps it from coming
// back, applied wherever a headline is set from an article title: the cluster
// stage, the news archive that refreshes a story from news.json, and the
// one-outlet story writers.
//
// The trailing segment is removed only when it names the outlet: the article
// domain, a label of it, or a publisher name handed in. A segment that merely
// sits after a dash stays, because "with help from AI" and "August 8th" are
// part of the headline and the 16 the project left alone are exactly those.

import (
	"regexp"
	"strings"
)

var newsOutletTailRe = regexp.MustCompile(`^(.*\S)\s+[-\x{2013}\x{2014}|]\s+([^-\x{2013}\x{2014}|]{2,45})$`)
var newsAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)

func newsNorm(s string) string { return newsAlnumRe.ReplaceAllString(strings.ToLower(s), "") }

// newsStripOutlet returns title without a trailing publisher segment. outlets
// are the names the segment may be: domains (with or without a scheme and
// path) or plain publisher names. Anything else is returned trimmed.
func newsStripOutlet(title string, outlets ...string) string {
	title = strings.TrimSpace(title)
	m := newsOutletTailRe.FindStringSubmatch(title)
	if m == nil {
		return title
	}
	seg := newsNorm(m[2])
	if len(seg) < 3 {
		return title
	}
	for _, o := range outlets {
		o = strings.ToLower(strings.TrimSpace(o))
		if i := strings.Index(o, "://"); i >= 0 {
			o = o[i+3:]
		}
		if i := strings.IndexAny(o, "/?#"); i >= 0 {
			o = o[:i]
		}
		if o == "" {
			continue
		}
		host := newsNorm(o)
		if seg == host || (len(seg) >= 4 && strings.Contains(host, seg)) {
			return strings.TrimSpace(m[1])
		}
		// Each label of a host on its own: "Yahoo" against news.yahoo.com,
		// "The Manila Times" against manilatimes.net. Generic labels and
		// country codes say nothing about who the outlet is and are skipped.
		for _, lab := range strings.Split(o, ".") {
			lab = newsNorm(lab)
			switch lab {
			case "", "www", "m", "amp", "en", "news", "com", "net", "org", "co", "uk", "ca", "de", "au", "in", "io", "info", "ai", "tv", "us":
				continue
			}
			if seg == lab {
				return strings.TrimSpace(m[1])
			}
			if len(lab) >= 4 && len(seg) >= 4 && (strings.Contains(seg, lab) || strings.Contains(lab, seg)) {
				return strings.TrimSpace(m[1])
			}
		}
	}
	return title
}

// newsArticleDomains lists the Domain of every article in a story as the
// news archive holds it, for passing to newsStripOutlet.
func newsArticleDomains(s map[string]any) []string {
	out := []string{}
	if arts, ok := s["Articles"].([]any); ok {
		for _, a := range arts {
			if m, ok := a.(map[string]any); ok {
				if d, _ := m["Domain"].(string); d != "" {
					out = append(out, d)
				}
			}
		}
	}
	return out
}
