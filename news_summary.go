package main

// THE STORY SUMMARY, AS STEPHEN SET IT ON 2026-10-08: three to four
// paragraphs of four to five sentences each, written from every article the
// cluster holds, not from one. The only thin summary allowed is a breaking
// story, where little has been reported yet and more will follow; that one
// says so and is rewritten on a later run when the coverage has grown.
//
// What was there before: one article's text, two short paragraphs, cached
// on the article row and never revisited. The Utah executive order of
// 2026-10-06 came out as "No specific numbers, costs, or deadlines were
// included in the article" under a story six outlets had reported in
// detail, because the one article the summary was drawn from was a video
// page. Stephen: "this story is very thin".
//
// An editor's summary beats the model's: a story whose document carries
// summary_by (written by hand into twoai_news_stories) keeps its Summary,
// SummaryURL and SummaryDomain through every rebuild, here and in the
// archive upsert.

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	newsSummaryMinParas = 3
	newsSummaryMinWords = 300
	// A story this young, or with this little text behind it, is breaking.
	newsBreakingAge   = 6 * time.Hour
	newsBreakingTexts = 2
	// How much article text the model reads for one story.
	newsSummaryMaxArticles = 4
	newsSummaryMaxChars    = 16000
)

// newsArt is one article of a cluster as the summary needs it.
type newsArt struct {
	Title, URL, Domain, Date string
}

// newsStorySummary is what the summary stage returns for a story.
type newsStorySummary struct {
	Summary, URL, Domain string
	By                   string // "editorial" when a person wrote it
	Thin, Breaking       bool
	Used                 int
}

func newsEnsureSummaries(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_news_summaries (
		slug text PRIMARY KEY,
		summary text NOT NULL,
		source_url text, source_domain text,
		articles_used int NOT NULL DEFAULT 0,
		words int NOT NULL DEFAULT 0, paragraphs int NOT NULL DEFAULT 0,
		thin boolean NOT NULL DEFAULT false, breaking boolean NOT NULL DEFAULT false,
		model text, written_at timestamptz NOT NULL DEFAULT now(), note text)`)
}

// newsSummaryShape counts paragraphs (blank-line separated) and words.
func newsSummaryShape(s string) (paras, words int) {
	for _, p := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n\n") {
		if strings.TrimSpace(p) != "" {
			paras++
		}
	}
	return paras, len(strings.Fields(s))
}

// newsArticleBody keeps the lines of a fetched page that read as prose and
// drops the navigation that comes with them: a line is kept when it is long
// or ends like a sentence.
func newsArticleBody(text string) string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		t := strings.TrimSpace(line)
		if len(t) >= 80 || (len(t) >= 40 && strings.ContainsAny(t[len(t)-1:], ".!?\"”")) {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

// newsSummarizeStory writes or reuses the summary of one story.
//
// getText returns the cached per-article summary and the fetched text for a
// URL; texts are the material, summaries only a fallback. generate is
// twoaiGenerate in production and a stub in tests.
func newsSummarizeStory(db *sql.DB, slug, headline string, arts []newsArt, now time.Time,
	getText func(url string) (summary, text string, ok bool),
	generate func(prompt string) (string, error)) newsStorySummary {
	// 1. An editor's words win.
	var es, eu, ed, eby sql.NullString
	if db.QueryRow(`SELECT story->>'Summary', story->>'SummaryURL', story->>'SummaryDomain', story->>'summary_by'
		FROM twoai_news_stories WHERE slug = $1 AND story ? 'summary_by'`, slug).Scan(&es, &eu, &ed, &eby) == nil && es.Valid && strings.TrimSpace(es.String) != "" {
		return newsStorySummary{Summary: es.String, URL: eu.String, Domain: ed.String, By: "editorial"}
	}

	// 2. The material: every article with real text, longest first, the
	// headline's own article first among them.
	type piece struct {
		a    newsArt
		body string
	}
	var pieces []piece
	for i, a := range arts {
		_, text, ok := getText(a.URL)
		if !ok {
			continue
		}
		body := newsArticleBody(text)
		if len(body) < 400 {
			continue
		}
		pieces = append(pieces, piece{a, body})
		_ = i
	}
	leadURL := ""
	if len(arts) > 0 {
		leadURL = arts[0].URL
	}
	sort.SliceStable(pieces, func(i, j int) bool {
		if (pieces[i].a.URL == leadURL) != (pieces[j].a.URL == leadURL) {
			return pieces[i].a.URL == leadURL
		}
		return len(pieces[i].body) > len(pieces[j].body)
	})
	earliest := now
	for _, a := range arts {
		if t, err := time.Parse(time.RFC3339, a.Date); err == nil && t.Before(earliest) {
			earliest = t
		} else if len(a.Date) >= 10 {
			if t, err := time.Parse("2006-01-02", a.Date[:10]); err == nil && t.Before(earliest) {
				earliest = t
			}
		}
	}
	breaking := len(pieces) < newsBreakingTexts || now.Sub(earliest) < newsBreakingAge

	// 3. A summary already written for this story is kept while nothing has
	// changed: the same number of articles read, and not a thin one that
	// is no longer breaking.
	newsEnsureSummaries(db)
	var cs, cu, cd sql.NullString
	var cused int
	var cthin bool
	if db.QueryRow(`SELECT summary, source_url, source_domain, articles_used, thin FROM twoai_news_summaries WHERE slug = $1`, slug).
		Scan(&cs, &cu, &cd, &cused, &cthin) == nil && cs.Valid {
		avail := len(pieces)
		if avail > newsSummaryMaxArticles {
			avail = newsSummaryMaxArticles
		}
		if avail <= cused && !(cthin && !breaking) {
			return newsStorySummary{Summary: cs.String, URL: cu.String, Domain: cd.String, Thin: cthin, Breaking: breaking, Used: cused}
		}
	}
	if len(pieces) == 0 {
		return newsStorySummary{Breaking: breaking}
	}

	// 4. Write it.
	var b strings.Builder
	used := 0
	for _, p := range pieces {
		if used == newsSummaryMaxArticles || b.Len() > newsSummaryMaxChars {
			break
		}
		room := newsSummaryMaxChars - b.Len()
		body := p.body
		if len(body) > room {
			body = body[:room]
		}
		fmt.Fprintf(&b, "Article %d (%s, %s): %s\n%s\n\n", used+1, p.a.Domain, p.a.Date, p.a.Title, body)
		used++
	}
	house := "Plain English. Use commas rather than dashes. Do not quote more than a few words at a time. Do not repeat the headline. " +
		"Do not add opinions or anything the articles do not say. Output only the paragraphs, separated by blank lines."
	ask := "Write the story in three to four paragraphs of four to five sentences each, 350 to 550 words in all, entirely in your own words, from the articles below. " +
		"First what happened, who did it and when; then what it requires or changes, with every number, date and name the articles give; " +
		"then who is responsible and what they said, paraphrased; then the context and what happens next if the articles say. "
	if breaking {
		ask += "If the articles carry too little for that, this is a breaking story: write what is known in one or two paragraphs and end with the sentence \"More to follow.\" "
	}
	prompt := ask + house + "\n\nHeadline: " + headline + "\n\n" + b.String()
	best, bestParas, bestWords := "", 0, 0
	for attempt := 0; attempt < 2; attempt++ {
		s, err := generate(prompt)
		if err != nil {
			fmt.Fprintln(os.Stderr, "publish_news summarize:", err)
			break
		}
		s = strings.TrimSpace(s)
		if s == "" || isRefusal(s) {
			continue
		}
		paras, words := newsSummaryShape(s)
		if words > bestWords {
			best, bestParas, bestWords = s, paras, words
		}
		if paras >= newsSummaryMinParas && words >= newsSummaryMinWords {
			break
		}
		if breaking {
			break // a short breaking summary is allowed
		}
		prompt = "The previous answer was too short. " + prompt
	}
	if best == "" {
		return newsStorySummary{Breaking: breaking}
	}
	thin := bestParas < newsSummaryMinParas || bestWords < newsSummaryMinWords
	src := pieces[0].a
	db.Exec(`INSERT INTO twoai_news_summaries (slug, summary, source_url, source_domain, articles_used, words, paragraphs, thin, breaking, model, written_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())
		ON CONFLICT (slug) DO UPDATE SET summary=EXCLUDED.summary, source_url=EXCLUDED.source_url, source_domain=EXCLUDED.source_domain,
			articles_used=EXCLUDED.articles_used, words=EXCLUDED.words, paragraphs=EXCLUDED.paragraphs, thin=EXCLUDED.thin,
			breaking=EXCLUDED.breaking, model=EXCLUDED.model, written_at=now()`,
		slug, best, src.URL, src.Domain, used, bestWords, bestParas, thin, breaking, "news_summary")
	return newsStorySummary{Summary: best, URL: src.URL, Domain: src.Domain, Thin: thin, Breaking: breaking, Used: used}
}
