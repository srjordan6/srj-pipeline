package main

// twoai_dccom_watch: Datacenters.com news, read through the browser.
//
// Stephen, 2026-10-01: start monitoring datacenters.com for new information,
// and all data centre information belongs in the Data Centers section. The
// site sits behind a Vercel Security Checkpoint that refuses plain fetches
// (429 to every scripted request, robots.txt included) and has no RSS feed;
// news search engines do not index it. So its news index is read through
// Cloudflare Browser Run, as the site crawl does for blocked sources, and
// each new article becomes a pipeline.documents row (source datacenters_com)
// with its full text. The announcement extractor that runs straight after
// turns any article reporting a facility into a press: facility record in
// the Data Centers section, exactly as it does for the rest of the intake.
// Six new articles a run, at most, keeps the browser budget negligible.

import (
	"database/sql"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var dccomLinkRe = regexp.MustCompile(`href="(/news/[a-z0-9][a-z0-9-]{8,})"`)
var dccomDateRe = regexp.MustCompile(`(\d{1,2} (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) 20\d\d)`)

func twoaiDatacentersComWatch(db *sql.DB) {
	var srcID int
	if db.QueryRow(`SELECT id FROM pipeline.sources WHERE key='datacenters_com'`).Scan(&srcID) != nil {
		return
	}
	client := &http.Client{Timeout: 60 * time.Second}
	st, body, _, err := crawlFetchBrowser(client, "https://www.datacenters.com/news")
	if err != nil || st != 200 {
		fmt.Printf("twoai_dccom_watch: news index not read: %v\n", err)
		return
	}
	seen := map[string]bool{}
	var fresh []string
	for _, m := range dccomLinkRe.FindAllSubmatch(body, -1) {
		u := "https://www.datacenters.com" + string(m[1])
		if seen[u] {
			continue
		}
		seen[u] = true
		var n int
		db.QueryRow(`SELECT count(*) FROM pipeline.documents WHERE url=$1`, u).Scan(&n)
		if n == 0 {
			fresh = append(fresh, u)
		}
	}
	added := 0
	for _, u := range fresh {
		if added >= 6 {
			break
		}
		st, page, _, ferr := crawlFetchBrowser(client, u)
		time.Sleep(2 * time.Second)
		if ferr != nil || st != 200 {
			continue
		}
		title, text := crawlText(page)
		title = strings.TrimSpace(strings.TrimSuffix(title, "| Datacenters.com"))
		if title == "" || len(text) < 600 || strings.Contains(title, "Security Checkpoint") {
			continue
		}
		pub := ""
		if dm := dccomDateRe.FindString(trunc(text, 3000)); dm != "" {
			if t, perr := time.Parse("2 Jan 2006", dm); perr == nil {
				pub = t.Format("2006-01-02")
			}
		}
		if _, ierr := db.Exec(`INSERT INTO pipeline.documents (source_id, external_id, url, title, published_at, fetched_at, fulltext, summary)
			VALUES ($1, $2, $2, $3, NULLIF($4,'')::timestamptz, now(), $5, $6)`,
			srcID, u, title, pub, text, trunc(text, 600)); ierr == nil {
			added++
		}
	}
	fmt.Printf("twoai_dccom_watch: links=%d new=%d stored=%d\n", len(seen), len(fresh), added)
}
