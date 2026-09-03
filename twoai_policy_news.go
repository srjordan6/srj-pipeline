package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// THE BRIEFING MISSED THE BIGGEST AI STORY OF THE DAY. On 2026-09-02 New
// York City banned generative AI in its public schools through eighth grade,
// the broadest such policy in the country. The daily news is built from
// GDELT, and GDELT's fifteen-minute slices carried the story exactly once,
// from a press-release mirror; the outlets that actually reported it - ABC7,
// Chalkbeat, the Times - were not in the slices the pipeline read. Stephen
// asked whether more sources are needed. Yes, for one class of story: AI
// POLICY - laws, bans, court rulings, regulators, school systems, cities -
// which is the class this site exists to cover and the class GDELT's AI
// sampling under-represents, because it keys on the phrase "artificial
// intelligence" and a city hall press conference says "AI".
//
// This stage runs a fixed set of Google News queries for that class,
// resolves each result to its publisher URL through the existing
// resolveGoogleNews, and inserts the article into pipeline.documents under
// the GDELT source, so publishNews clusters it alongside everything else
// with no change to the briefing. Titles are decoded from HTML entities
// before they are stored. Queries are US-first because the laws page is;
// the same list can widen.
var twoaiPolicyNewsQueries = []string{
	`AI ban schools`,
	`artificial intelligence law signed`,
	`AI executive order`,
	`AI regulation court ruling`,
	`"attorney general" artificial intelligence`,
	`AI legislation passed`,
	`"data center" moratorium`,
	`AI Act enforcement`,
	`school district artificial intelligence policy`,
	`city council artificial intelligence`,
}

func twoaiPolicyNews(db *sql.DB) (fetched, added int, err error) {
	var sourceID int
	if err := db.QueryRow(`SELECT id FROM pipeline.sources WHERE name ILIKE '%gdelt%' LIMIT 1`).Scan(&sourceID); err != nil {
		return 0, 0, fmt.Errorf("gdelt source id: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for _, q := range twoaiPolicyNewsQueries {
		u := "https://news.google.com/rss/search?q=" + strings.ReplaceAll(strings.ReplaceAll(q, `"`, "%22"), " ", "+") +
			"+when:2d&hl=en-US&gl=US&ceid=US:en"
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", "theworldofai.org news intake")
		resp, e := client.Do(req)
		if e != nil {
			fmt.Fprintln(os.Stderr, "twoai_policy_news:", q, e)
			continue
		}
		body, e := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if e != nil || resp.StatusCode != 200 {
			continue
		}
		var feed struct {
			Items []struct {
				Title   string `xml:"title"`
				Link    string `xml:"link"`
				PubDate string `xml:"pubDate"`
			} `xml:"channel>item"`
		}
		dec := xml.NewDecoder(bytes.NewReader(body))
		dec.Strict = false
		dec.Entity = xml.HTMLEntity
		if dec.Decode(&feed) != nil {
			continue
		}
		for i, it := range feed.Items {
			if i >= 15 {
				break
			}
			title := html.UnescapeString(strings.TrimSpace(it.Title))
			// Google News appends " - Outlet"; publishNews strips that itself,
			// but the AI test must see the headline the outlet wrote.
			if !twoaiTitleIsAI(title) {
				continue
			}
			link := strings.TrimSpace(it.Link)
			if isGoogleNewsURL(link) {
				link = resolveGoogleNews(link)
			}
			if !strings.HasPrefix(link, "http") || isGoogleNewsURL(link) {
				continue
			}
			fetched++
			var pub any
			if t, e := time.Parse(time.RFC1123Z, it.PubDate); e == nil {
				pub = t.UTC().Format(time.RFC3339)
			} else if t, e := time.Parse(time.RFC1123, it.PubDate); e == nil {
				pub = t.UTC().Format(time.RFC3339)
			}
			meta := map[string]string{"url": link, "domain": publisherFromURL(link), "date": fmt.Sprint(pub),
				"title": title, "query": q, "intake": "policy-news"}
			raw, _ := json.Marshal(meta)
			h := sha256.Sum256([]byte(link))
			id := hex.EncodeToString(h[:])[:32]
			ok, e := insertDoc(db, sourceID, id, id, link, title, pub, raw)
			if e != nil {
				return fetched, added, e
			}
			if ok {
				added++
			}
		}
		time.Sleep(1500 * time.Millisecond)
	}
	fmt.Printf("twoai_policy_news: queries=%d fetched=%d new=%d\n", len(twoaiPolicyNewsQueries), fetched, added)
	return fetched, added, nil
}
