package main

// THE SELF-AUDIT: the thin-page cron reads the live site the way Google
// does, page by page from the sitemap, and records what each page actually
// renders inside <main>: words, headings, question-form headings, FAQ
// schema, a visible date label, internal links, title and h1. The queue's
// "thin-words" test reads this table, so "which pages are thin" is a query
// against measured content rather than a guess from the page data.
//
// Ran first by hand on 2026-08-30 against 2,183 sitemap URLs: 541 under
// 300 words in the main content, 134 under 150, 437 without a date label,
// one page with a question-form H2 on the whole site. Those are the
// numbers this stage keeps current.
//
// The site is our own and served from a CDN, so the crawl is polite by
// construction: eight requests in flight, no retries, and a sitemap that
// comes back near-empty aborts the run rather than emptying the table.

import (
	"database/sql"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const thinAuditWords = 300 // the Yoast/AEO floor Stephen set on 2026-08-30

var (
	auditChromeRe = regexp.MustCompile(`(?is)<(script|style|noscript|svg|nav|header|footer|aside)[^>]*>.*?</(script|style|noscript|svg|nav|header|footer|aside)>`)
	auditMainRe   = regexp.MustCompile(`(?is)<main[^>]*>(.*?)</main>`)
	auditH2Re     = regexp.MustCompile(`(?is)<h2[^>]*>(.*?)</h2>`)
	auditH1Re     = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	auditTitleRe  = regexp.MustCompile(`(?is)<title>(.*?)</title>`)
	auditLinkRe   = regexp.MustCompile(`(?is)<a[^>]+href="(/[^"#]*)`)
	auditLDRe     = regexp.MustCompile(`(?is)<script[^>]+application/ld\+json[^>]*>(.*?)</script>`)
	auditTypeRe   = regexp.MustCompile(`"@type"\s*:\s*"([A-Za-z]+)"`)
	auditDateRe   = regexp.MustCompile(`(?i)(Last verified|Last checked|Last harvested|Last reviewed|Generated|Updated|Retrieved)[:\s]`)
	auditWS       = regexp.MustCompile(`\s+`)
)

func twoaiThinEnsureAudit(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_page_audit (
		url text PRIMARY KEY,
		status int NOT NULL,
		words int NOT NULL DEFAULT 0,
		h2_count int NOT NULL DEFAULT 0,
		question_h2 int NOT NULL DEFAULT 0,
		faq_schema boolean NOT NULL DEFAULT false,
		schema_types text NOT NULL DEFAULT '',
		dated boolean NOT NULL DEFAULT false,
		internal_links int NOT NULL DEFAULT 0,
		title text NOT NULL DEFAULT '',
		h1 text NOT NULL DEFAULT '',
		audited_on date NOT NULL DEFAULT current_date)`)
}

func twoaiThinAudit(db *sql.DB) {
	twoaiThinEnsureAudit(db)
	client := &http.Client{Timeout: 40 * time.Second}
	var urls []string
	for _, sm := range thinSitemapLocs(client, "https://theworldofai.org/sitemap-index.xml") {
		urls = append(urls, thinSitemapLocs(client, sm)...)
	}
	if len(urls) < 100 {
		fmt.Printf("thinpages: audit: sitemap yielded %d urls, refusing to audit (keeping prior rows)\n", len(urls))
		return
	}
	type row struct {
		url, title, h1, types string
		status, words, h2, qh2  int
		faq, dated              bool
		links                   int
	}
	results := make(chan row, len(urls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req, _ := http.NewRequest("GET", u, nil)
			req.Header.Set("User-Agent", "theworldofai.org self-audit (contact: stephen@srjconsultingservices.com)")
			resp, err := client.Do(req)
			if err != nil {
				results <- row{url: u}
				return
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
			resp.Body.Close()
			r := row{url: u, status: resp.StatusCode}
			if resp.StatusCode != 200 {
				results <- r
				return
			}
			page := string(b)
			core := page
			if m := auditMainRe.FindStringSubmatch(page); m != nil {
				core = m[1]
			}
			txt := html.UnescapeString(thinStrip.ReplaceAllString(auditChromeRe.ReplaceAllString(core, " "), " "))
			txt = strings.TrimSpace(auditWS.ReplaceAllString(txt, " "))
			if txt != "" {
				r.words = len(strings.Split(txt, " "))
			}
			for _, m := range auditH2Re.FindAllStringSubmatch(core, -1) {
				r.h2++
				if strings.HasSuffix(strings.TrimSpace(html.UnescapeString(thinStrip.ReplaceAllString(m[1], ""))), "?") {
					r.qh2++
				}
			}
			if m := auditTitleRe.FindStringSubmatch(page); m != nil {
				r.title = strings.TrimSpace(html.UnescapeString(m[1]))
			}
			if m := auditH1Re.FindStringSubmatch(page); m != nil {
				r.h1 = strings.TrimSpace(html.UnescapeString(thinStrip.ReplaceAllString(m[1], "")))
			}
			seen := map[string]bool{}
			for _, ld := range auditLDRe.FindAllStringSubmatch(page, -1) {
				for _, t := range auditTypeRe.FindAllStringSubmatch(ld[1], -1) {
					seen[t[1]] = true
				}
			}
			var types []string
			for t := range seen {
				types = append(types, t)
			}
			r.types = strings.Join(types, ",")
			r.faq = seen["FAQPage"]
			r.dated = auditDateRe.MatchString(txt)
			links := map[string]bool{}
			for _, m := range auditLinkRe.FindAllStringSubmatch(core, -1) {
				links[m[1]] = true
			}
			r.links = len(links)
			results <- r
		}(u)
	}
	wg.Wait()
	close(results)
	n, thin, failed := 0, 0, 0
	for r := range results {
		if r.status != 200 {
			failed++
		} else if r.words < thinAuditWords {
			thin++
		}
		if _, err := db.Exec(`INSERT INTO twoai_page_audit
			(url, status, words, h2_count, question_h2, faq_schema, schema_types, dated, internal_links, title, h1, audited_on)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,current_date)
			ON CONFLICT (url) DO UPDATE SET status=EXCLUDED.status, words=EXCLUDED.words,
				h2_count=EXCLUDED.h2_count, question_h2=EXCLUDED.question_h2, faq_schema=EXCLUDED.faq_schema,
				schema_types=EXCLUDED.schema_types, dated=EXCLUDED.dated, internal_links=EXCLUDED.internal_links,
				title=EXCLUDED.title, h1=EXCLUDED.h1, audited_on=current_date`,
			r.url, r.status, r.words, r.h2, r.qh2, r.faq, r.types, r.dated, r.links, r.title, r.h1); err == nil {
			n++
		}
	}
	// A URL that left the sitemap leaves the audit; the queue follows.
	db.Exec(`DELETE FROM twoai_page_audit WHERE audited_on < current_date - 3`)
	fmt.Printf("thinpages: audit: %d urls, %d recorded, %d under %d words, %d not 200\n", len(urls), n, thin, thinAuditWords, failed)
}

func thinSitemapLocs(client *http.Client, u string) []string {
	body, err := thinGet(client, u)
	if err != nil {
		return nil
	}
	var doc struct {
		Locs []string `xml:"sitemap>loc"`
		URLs []string `xml:"url>loc"`
	}
	if xml.Unmarshal([]byte(body), &doc) != nil {
		return nil
	}
	return append(doc.Locs, doc.URLs...)
}
