package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// THE HARD SITES. Stephen, 2026-09-06: this cron should take the very toughest
// websites Cowork is having problems with.
//
// The division of labour is real, not a preference. Cowork reads a page with a
// plain HTTP fetch. That fails on three whole classes of site and it fails
// silently enough that the page just looks empty: a React or Angular site
// where the figures arrive after the JavaScript runs; a site behind Cloudflare
// or Akamai bot protection that answers 403 to anything without a browser
// fingerprint; and a page where the specification sits behind a cookie wall,
// an accordion or a "view specifications" button.
//
// This cron has Firecrawl, which handles all three, and a budget. So the
// pattern is escalation rather than duplication: Cowork works normally, and
// when a site defeats it, it writes one row into twoai_scrape_escalations
// saying what it wanted and why it could not get it. This stage works that
// queue with progressively heavier instruments and writes the text back for
// Cowork to read on its next pass.
//
// THE THREE TIERS, cheapest first, because each costs more than the last:
//
//	basic     - a normal scrape. Handles JavaScript, which is most of it.
//	            Tried first even for a site Cowork called blocked, because
//	            Cowork's failure may have been a user-agent block that any
//	            real browser clears.
//	wait      - the same with waitFor, for pages that render their figures
//	            after an XHR settles. 5 seconds, which is generous.
//	enhanced  - Firecrawl's anti-bot proxy. This is the expensive one and it
//	            is the last resort, only for a page that returned 403 or an
//	            empty body from the two cheaper tiers.
//
// A row that fails all three tiers three times is marked abandoned with the
// last error, and is not retried. A site that will not be read is a fact
// about the site, and recording it once is worth more than retrying forever -
// which is the same reasoning as the link checker and the thin queue.
//
// WHAT THIS STAGE DOES NOT DO. It does not interpret. It fetches the text and
// stores it; Cowork applies the scope rules, the facility-versus-campus
// distinction and the sourcing discipline, exactly as it does for a page it
// fetched itself. Handing a model raw text and letting it write a figure
// straight into the registry is how fabrication gets in, and the whole point
// of the escalation is to widen what can be READ, not what will be believed.

type escalationRow struct {
	id       int64
	url      string
	wanted   string
	attempts int
}

type escTier struct {
	name    string
	payload func(url string) string
	credits int
}

func twoaiScrapeEscalations(db *sql.DB) {
	key := os.Getenv("FIRECRAWL_API_KEY")
	if key == "" {
		fmt.Println("twoai_escalations: skipped, FIRECRAWL_API_KEY not set")
		return
	}
	budget := 15
	if b, err := strconv.Atoi(os.Getenv("TWOAI_ESCALATION_BUDGET")); err == nil && b >= 0 {
		budget = b
	}
	if budget == 0 {
		fmt.Println("twoai_escalations: budget 0, skipped")
		return
	}

	rows, err := db.Query(`SELECT id, url, wanted, attempts
		FROM twoai_scrape_escalations
		WHERE status = 'queued' AND attempts < 3
		ORDER BY requested_at LIMIT $1`, budget)
	if err != nil {
		fmt.Println("twoai_escalations:", err)
		return
	}
	var queue []escalationRow
	for rows.Next() {
		var r escalationRow
		if rows.Scan(&r.id, &r.url, &r.wanted, &r.attempts) == nil {
			queue = append(queue, r)
		}
	}
	rows.Close()
	if len(queue) == 0 {
		fmt.Println("twoai_escalations: queue empty")
		return
	}

	// The whole payload is marshalled as a struct rather than concatenated
	// around a quoted value. json.Marshal on the value alone was safe, but
	// hand-assembled JSON is the pattern CodeQL rightly flags (go/unsafe-
	// quoting): the day someone adds a field by string edit, the escaping
	// guarantee silently stops covering it. Marshalling the object makes the
	// unsafe edit inexpressible.
	type fcScrape struct {
		URL             string   `json:"url"`
		Formats         []string `json:"formats"`
		OnlyMainContent bool     `json:"onlyMainContent"`
		BlockAds        bool     `json:"blockAds"`
		WaitFor         int      `json:"waitFor,omitempty"`
		Proxy           string   `json:"proxy,omitempty"`
		Timeout         int      `json:"timeout"`
	}
	mk := func(p fcScrape) string { b, _ := json.Marshal(p); return string(b) }
	tiers := []escTier{
		{"basic", func(u string) string {
			return mk(fcScrape{URL: u, Formats: []string{"markdown"}, OnlyMainContent: true, BlockAds: true, Timeout: 45000})
		}, 1},
		{"wait", func(u string) string {
			return mk(fcScrape{URL: u, Formats: []string{"markdown"}, OnlyMainContent: true, BlockAds: true, WaitFor: 5000, Timeout: 60000})
		}, 1},
		{"enhanced", func(u string) string {
			return mk(fcScrape{URL: u, Formats: []string{"markdown"}, OnlyMainContent: true, BlockAds: true, WaitFor: 5000, Proxy: "enhanced", Timeout: 90000})
		}, 5},
	}

	client := &http.Client{Timeout: 120 * time.Second}
	fetched, failed, abandoned := 0, 0, 0

	for _, r := range queue {
		var text, tierUsed, lastErr string
		var status int

		for _, t := range tiers {
			req, _ := http.NewRequest("POST", "https://api.firecrawl.dev/v2/scrape",
				bytes.NewReader([]byte(t.payload(r.url))))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				lastErr = t.name + ": " + err.Error()
				continue
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			status = resp.StatusCode
			if resp.StatusCode != 200 {
				snip := strings.TrimSpace(string(b))
				if len(snip) > 200 {
					snip = snip[:200]
				}
				lastErr = fmt.Sprintf("%s: HTTP %d %s", t.name, resp.StatusCode, snip)
				continue
			}
			var out struct {
				Data struct {
					Markdown string `json:"markdown"`
				} `json:"data"`
			}
			if json.Unmarshal(b, &out) != nil {
				lastErr = t.name + ": unreadable response"
				continue
			}
			// A 200 with almost nothing in it is the signature of a bot wall
			// serving a challenge page. Treat it as a failure of this tier and
			// escalate, rather than storing a challenge page as content.
			if len(strings.TrimSpace(out.Data.Markdown)) < 400 {
				lastErr = fmt.Sprintf("%s: 200 but only %d chars, likely a challenge page",
					t.name, len(strings.TrimSpace(out.Data.Markdown)))
				continue
			}
			text, tierUsed = out.Data.Markdown, t.name
			break
		}

		if text != "" {
			if len(text) > 200000 {
				text = text[:200000]
			}
			db.Exec(`UPDATE twoai_scrape_escalations
				SET status='fetched', attempts=attempts+1, last_attempt=now(),
				    tier_used=$2, http_status=$3, content=$4, content_chars=$5, error=NULL
				WHERE id=$1`, r.id, tierUsed, status, text, len(text))
			fetched++
			fmt.Printf("twoai_escalations: fetched %d chars via %s | %.70s\n", len(text), tierUsed, r.url)
			continue
		}

		newAttempts := r.attempts + 1
		st := "queued"
		if newAttempts >= 3 {
			st = "abandoned"
			abandoned++
		} else {
			failed++
		}
		db.Exec(`UPDATE twoai_scrape_escalations
			SET status=$2, attempts=$3, last_attempt=now(), error=$4, http_status=$5
			WHERE id=$1`, r.id, st, newAttempts, lastErr, status)
		fmt.Printf("twoai_escalations: %s after %d attempts | %.60s | %s\n", st, newAttempts, r.url, lastErr)
	}

	var queued, waiting int
	db.QueryRow(`SELECT count(*) FILTER (WHERE status='queued'),
		count(*) FILTER (WHERE status='fetched' AND delivered_at IS NULL)
		FROM twoai_scrape_escalations`).Scan(&queued, &waiting)
	fmt.Printf("twoai_escalations: worked=%d fetched=%d failed=%d abandoned=%d | %d still queued, %d fetched awaiting Cowork\n",
		len(queue), fetched, failed, abandoned, queued, waiting)
}
