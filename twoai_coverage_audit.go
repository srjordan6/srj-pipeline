package main

// THE COVERAGE AUDIT, theworldofai row 567 (2026-10-07).
//
// Stephen asked why he constantly has to check the news and tell us. In one
// day he found the FTC probe, the Genesis Mission credits, the Trahan draft,
// the Cantwell framework, the Originality.ai analysis and a Utah executive
// order that the site had either missed or not placed. The first cause is
// that nothing compared outside coverage to ours. This stage does that,
// every run, before publish_news: it reads the top AI items of the last day
// from aggregators that see the whole press (Google News queries, which also
// reach AP, Reuters, Politico and the Post through site: queries, and
// Techmeme's feed), looks for each in our own stories and corpus of the last
// 72 hours, and harvests every miss into pipeline.documents, so the same
// run clusters and publishes it. Then it writes one bridge row to
// theworldofai saying what it read, what was already ours, what it harvested
// and what it could not reach.
//
// Success, as Stephen set it: a week in which he forwards nothing we did not
// already have.

import (
	"database/sql"
	"fmt"
	"html"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// twoaiCoverageSources are the places the audit reads. Google News RSS is
// reachable and complete; AP, Reuters, Politico's Digital Future Daily and
// the Post's AI brief have no feed we can read directly (Politico and the
// Post block the fetch, AP and Reuters publish no RSS), so they are reached
// through site-restricted Google News queries, which is the aggregator's
// own index of them.
var twoaiCoverageSources = []struct{ name, url string }{
	{"Google News: artificial intelligence", gnewsSearch(`artificial intelligence when:1d`)},
	{"Google News: AI regulation and law", gnewsSearch(`AI (regulation OR law OR bill OR "executive order" OR lawsuit OR FTC OR governor OR senator) when:1d`)},
	{"Google News: AI companies", gnewsSearch(`(OpenAI OR Anthropic OR Nvidia OR "Google DeepMind" OR Microsoft OR Meta) AI when:1d`)},
	{"Google News: AP", gnewsSearch(`site:apnews.com artificial intelligence when:1d`)},
	{"Google News: Reuters", gnewsSearch(`site:reuters.com artificial intelligence when:1d`)},
	{"Google News: Politico", gnewsSearch(`site:politico.com artificial intelligence when:1d`)},
	{"Google News: Washington Post", gnewsSearch(`site:washingtonpost.com artificial intelligence when:1d`)},
	{"Google News: Axios", gnewsSearch(`site:axios.com AI when:1d`)},
	{"Techmeme", "https://www.techmeme.com/feed.xml"},
}

func gnewsSearch(q string) string {
	return "https://news.google.com/rss/search?q=" + strings.ReplaceAll(strings.ReplaceAll(q, `"`, "%22"), " ", "+") + "&hl=en-US&gl=US&ceid=US:en"
}

func twoaiCoverageAudit(db *sql.DB) error {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_coverage_audit (
		id serial PRIMARY KEY, run_on date NOT NULL DEFAULT current_date, source text, title text, url text,
		published_on date, outcome text, match_uid text, doc_id bigint, created_at timestamptz DEFAULT now())`)
	var sourceID int
	db.QueryRow(`SELECT id FROM pipeline.sources WHERE key='gdelt'`).Scan(&sourceID)
	if sourceID == 0 {
		return fmt.Errorf("gdelt source id not found")
	}
	// What we already hold: story headlines of the last 72 hours and every
	// article URL in the window.
	type story struct {
		uid  string
		toks map[string]bool
	}
	var stories []story
	if rows, err := db.Query(`SELECT uid, headline FROM twoai_news_stories WHERE retired_at IS NULL AND published_on >= current_date - 3`); err == nil {
		for rows.Next() {
			var u, h string
			if rows.Scan(&u, &h) == nil {
				stories = append(stories, story{u, newsTitleTokens(newsStripOutlet(h))})
			}
		}
		rows.Close()
	}
	held := map[string]bool{}
	if rows, err := db.Query(`SELECT url FROM pipeline.documents WHERE source_id=$1 AND fetched_at > now() - interval '72 hours'`, sourceID); err == nil {
		for rows.Next() {
			var u string
			if rows.Scan(&u) == nil {
				held[newsCanonURL(u)] = true
			}
		}
		rows.Close()
	}
	client := &http.Client{Timeout: 30 * time.Second}
	today := time.Now().UTC().Format("2006-01-02")
	read, asStory, asDoc, harvested, dupes := 0, 0, 0, 0, 0
	var unreachable, finds []string
	seen := map[string]bool{}
	for _, src := range twoaiCoverageSources {
		_, items, _, err := twoaiFetchFeed(client, src.url)
		if err != nil {
			unreachable = append(unreachable, src.name+": "+trunc(err.Error(), 100))
			continue
		}
		for _, it := range items {
			title := strings.TrimSpace(html.UnescapeString(twoaiTagStrip.ReplaceAllString(it.Title, "")))
			link := strings.TrimSpace(it.URL())
			if title == "" || link == "" {
				continue
			}
			if isGoogleNewsURL(link) {
				link = resolveGoogleNews(link)
				if link == "" || isGoogleNewsURL(link) {
					continue
				}
			}
			if src.name == "Techmeme" && !twoaiGovAIRe.MatchString(title) {
				continue
			}
			title = newsStripOutlet(title, publisherFromURL(link))
			canon := newsCanonURL(link)
			if seen[canon] {
				dupes++
				continue
			}
			seen[canon] = true
			read++
			date := twoaiFeedDate(it.PubDate, it.Published, it.Updated, it.Date)
			if date == "" {
				date = today
			}
			outcome, match := "", ""
			if held[canon] {
				outcome = "in corpus"
				asDoc++
			} else {
				toks := newsTitleTokens(title)
				best, bestUID := 0.0, ""
				for _, s := range stories {
					if sim := newsTokSim(toks, s.toks); sim > best {
						best, bestUID = sim, s.uid
					}
				}
				if best >= 0.34 {
					outcome, match = "in story", bestUID
					asStory++
				}
			}
			var docID sql.NullInt64
			if outcome == "" {
				if err := db.QueryRow(`INSERT INTO pipeline.documents (source_id, external_id, change_hash, url, title, published_at, fetched_at, raw)
					SELECT $1, md5($2), md5($2), $2, $3, $4::date, now(),
					       jsonb_build_object('url', $2, 'date', $8 || 'T12:00:00Z', 'title', $3, 'domain', $5, 'intake', 'coverage_audit',
					                          'query', $6, 'hand', 'twoai_coverage_audit ' || $7 || ': not in our stories or corpus of the last 72 hours')
					WHERE NOT EXISTS (SELECT 1 FROM pipeline.documents WHERE url=$2) RETURNING id`,
					sourceID, link, title, date, publisherFromURL(link), src.name, today, date).Scan(&docID); err == nil && docID.Valid {
					outcome = "harvested"
					harvested++
					held[canon] = true
					if len(finds) < 25 {
						finds = append(finds, fmt.Sprintf("%s (%s, %s)", title, publisherFromURL(link), src.name))
					}
				} else {
					outcome = "in corpus"
					asDoc++
				}
			}
			db.Exec(`INSERT INTO twoai_coverage_audit (source, title, url, published_on, outcome, match_uid, doc_id)
				VALUES ($1,$2,$3,NULLIF($4,'')::date,$5,NULLIF($6,''),$7)`, src.name, title, link, date, outcome, match, docID)
		}
	}
	fmt.Printf("twoai_coverage_audit: read=%d in_story=%d in_corpus=%d harvested=%d duplicates=%d unreachable=%d\n",
		read, asStory, asDoc, harvested, dupes, len(unreachable))
	if len(unreachable) > 0 {
		fmt.Fprintf(os.Stderr, "twoai_coverage_audit: unreachable: %s\n", strings.Join(unreachable, "; "))
	}
	// The morning row, once a day: what was read, what was ours, what was
	// harvested and will cluster in this run, and what could not be reached.
	var already bool
	db.QueryRow(`SELECT EXISTS (SELECT 1 FROM project_bridge WHERE from_project='srj' AND topic = 'Coverage audit ' || $1)`, today).Scan(&already)
	if !already && read > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "Coverage audit %s (twoai_coverage_audit, row 567). Read %d items from %d aggregator feeds: %d already in a story of the last 72 hours, %d already in the corpus, %d harvested as new documents for this run's publish_news to cluster (a story needs three domains, so a single-outlet find becomes a story only when others follow).\n",
			today, read, len(twoaiCoverageSources)-len(unreachable), asStory, asDoc, harvested)
		if len(finds) > 0 {
			b.WriteString("\nHARVESTED (title, publisher, which feed):\n")
			sort.Strings(finds)
			for _, f := range finds {
				b.WriteString("- " + f + "\n")
			}
			if harvested > len(finds) {
				fmt.Fprintf(&b, "- and %d more, all in twoai_coverage_audit for today\n", harvested-len(finds))
			}
		}
		if len(unreachable) > 0 {
			b.WriteString("\nNOT REACHED this run:\n")
			for _, u := range unreachable {
				b.WriteString("- " + u + "\n")
			}
		}
		b.WriteString("\nAP, Reuters, Politico and the Post are read through site: queries on Google News, since none of them serves a feed this pipeline can fetch. Every item and its outcome is in twoai_coverage_audit.")
		db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body) VALUES ('srj','theworldofai', 'Coverage audit ' || $1, $2)`, today, b.String())
	}
	return nil
}
