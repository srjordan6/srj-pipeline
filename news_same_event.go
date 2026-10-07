package main

// ONE EVENT, ONE STORY: two more merge tests and two summary guards, from
// theworldofai row 547 (Stephen, 2026-10-07, on /ai-news/: "it says
// 2026-10-07 but a lot of the information is old").
//
// What the live page showed that day, checked against twoai_news_stories:
//   - the Australian Senate AI inquiry as four stories (10aeca36, ef97b967,
//     cd3219df, a8284029): four witnesses at one hearing, headlines with
//     almost no words in common, but the same committee, the same broadcaster
//     and the same named people in GDELT's extraction of every article;
//   - Minnesota's nudification ban twice (b21a0c3c, edac9258): no names
//     extracted at all, but two words, "Minnesota" and "nudification", that
//     no other story of the window used;
//   - an article from 2026-07-17 inside a 2026-10-06 cluster (0fe54ab6),
//     GDELT having fetched old coverage again;
//   - a summary about Anthropic and government systems under a headline
//     about the ABC and copyright (cd3219df), because the summary came from
//     the first article that had text, not from the one the headline came
//     from.
//
// The tests here compare the names and words each cluster had when it
// formed, never the union after a merge, for the reason seedTk exists: on
// 2026-09-15 a widening comparison let one cluster swallow 489 articles.

import (
	"strings"
	"time"
)

// newsArticleMaxAge is the oldest an article may be, by its own publication
// date, and still count toward a story.
const newsArticleMaxAge = 14 * 24 * time.Hour

// newsTooOld says whether an article's own date is more than
// newsArticleMaxAge before asOf. An article with no readable date is kept.
func newsTooOld(date string, asOf time.Time) bool {
	date = strings.TrimSpace(date)
	if len(date) < 10 {
		return false
	}
	t, err := time.Parse(time.RFC3339, date)
	if err != nil {
		if t, err = time.Parse("2006-01-02", date[:10]); err != nil {
			return false
		}
	}
	return asOf.Sub(t) > newsArticleMaxAge
}

// newsBylineOrgs are names GDELT extracts from bylines and credits rather
// than from what a story is about. They say who reported it, never which
// event it is.
var newsBylineOrgs = map[string]bool{
	"reuters": true, "associated press": true, "ap": true, "afp": true, "agence france presse": true,
	"bloomberg": true, "bloomberg news": true, "getty images": true, "united states": true,
	"white house": true, "european union": true, "united kingdom": true, "china": true,
}

// newsNameSet is a cluster's GDELT-extracted people and organisations,
// lower-cased, from semicolon lists as the documents carry them.
func newsNameSet(lists []string) map[string]bool {
	m := map[string]bool{}
	for _, l := range lists {
		for _, n := range strings.Split(l, ";") {
			n = strings.ToLower(strings.TrimSpace(n))
			if len(n) >= 4 && !newsBylineOrgs[n] {
				m[n] = true
			}
		}
	}
	return m
}

// newsCommon returns the members of sets that appear in more than limit of
// them: a name or word that common this window identifies no one event.
func newsCommon(sets []map[string]bool, limit int) map[string]bool {
	df := map[string]int{}
	for _, s := range sets {
		for k := range s {
			df[k]++
		}
	}
	out := map[string]bool{}
	for k, n := range df {
		if n > limit {
			out[k] = true
		}
	}
	return out
}

// newsSharedUncommon counts members of both a and b that are not common.
func newsSharedUncommon(a, b, common map[string]bool) int {
	n := 0
	for k := range a {
		if b[k] && !common[k] {
			n++
		}
	}
	return n
}

// newsSameEventByNames: three shared names that are not common this window
// make two clusters one event on their own; two do when the headlines also
// share a little (overlap at least 0.1).
func newsSameEventByNames(a, b, common map[string]bool, overlap float64) bool {
	n := newsSharedUncommon(a, b, common)
	return n >= 3 || (n >= 2 && overlap >= 0.1)
}

// newsSameEventByRareWords: two clusters whose seed headlines share two
// words that at most three clusters of the window use at all.
func newsSameEventByRareWords(a, b, rare map[string]bool) bool {
	n := 0
	for k := range a {
		if b[k] && rare[k] {
			n++
		}
	}
	return n >= 2
}

// newsSummaryFits says whether a summary is about the headline: it must
// contain at least one of the headline's significant words (two when the
// headline has five or more), so a summary of another article in the
// cluster is not printed under it.
func newsSummaryFits(headline, summary string) bool {
	hw := newsTitleTokens(headline)
	if len(hw) == 0 || strings.TrimSpace(summary) == "" {
		return len(hw) == 0
	}
	sw := newsTitleTokens(summary)
	n := 0
	for k := range hw {
		if sw[k] {
			n++
		}
	}
	need := 1
	if len(hw) >= 5 {
		need = 2
	}
	return n >= need
}
