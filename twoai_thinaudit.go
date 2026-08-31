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
	"sort"
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
		targets                 []string // internal hrefs this page points at
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
			for t := range links {
				r.targets = append(r.targets, t)
			}
			results <- r
		}(u)
	}
	wg.Wait()
	close(results)
	n, thin, failed := 0, 0, 0
	// Where each internal link points, and one page that points there, so a
	// broken target names a page to fix rather than a bare path.
	linkFrom := map[string]string{}
	for r := range results {
		for _, t := range r.targets {
			if _, seen := linkFrom[t]; !seen {
				linkFrom[t] = r.url
			}
		}
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
	twoaiThinLinkAudit(db, client, urls, linkFrom)
}

// WHERE THE LINKS ACTUALLY GO. Added 2026-08-31, after Stephen's crawler
// reported /industries/ returning 404 from the case studies page and a
// one-off sweep of the build found 68 more of the same kind.
//
// The page audit above measures pages that EXIST. This measures links to
// pages that do not, which is a different fault and invisible to the other
// check: a page can be perfect and still send every reader to a dead end.
//
// Only targets the page audit has not already proved are fetched. Everything
// in the sitemap was just crawled and answered 200, so a link there needs no
// second request; what is left is a few hundred paths, and each is asked for
// once. A redirect is recorded, not counted as broken, because a 301 to a
// live page is a working link that costs a hop, and knowing which links cost
// a hop is worth having separately from knowing which are dead.
func twoaiThinLinkAudit(db *sql.DB, client *http.Client, audited []string, linkFrom map[string]string) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_link_audit (
		target text PRIMARY KEY,
		status int NOT NULL,
		final_status int NOT NULL DEFAULT 0,
		final_url text NOT NULL DEFAULT '',
		linked_from text NOT NULL DEFAULT '',
		checked_on date NOT NULL DEFAULT current_date)`)

	known := map[string]bool{}
	for _, u := range audited {
		known[strings.TrimPrefix(u, "https://theworldofai.org")] = true
	}
	var todo []string
	for t := range linkFrom {
		if !known[t] {
			todo = append(todo, t)
		}
	}
	if len(todo) == 0 {
		return
	}
	sort.Strings(todo)

	type res struct {
		target, finalURL, from string
		status, final          int
	}
	out := make(chan res, len(todo))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, t := range todo {
		wg.Add(1)
		go func(t string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := res{target: t, from: linkFrom[t]}
			// The first response tells us whether the link is direct, a
			// redirect, or dead; the followed one tells us where it lands.
			noRedirect := &http.Client{
				Timeout: 20 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			}
			req, _ := http.NewRequest("GET", "https://theworldofai.org"+t, nil)
			req.Header.Set("User-Agent", "theworldofai.org link audit (contact: stephen@srjconsultingservices.com)")
			if resp, err := noRedirect.Do(req); err == nil {
				r.status = resp.StatusCode
				r.finalURL = resp.Header.Get("Location")
				resp.Body.Close()
			}
			if r.status >= 300 && r.status < 400 {
				freq, _ := http.NewRequest("GET", "https://theworldofai.org"+t, nil)
				freq.Header.Set("User-Agent", "theworldofai.org link audit (contact: stephen@srjconsultingservices.com)")
				if fresp, err := client.Do(freq); err == nil {
					r.final = fresp.StatusCode
					r.finalURL = fresp.Request.URL.String()
					fresp.Body.Close()
				}
			}
			out <- r
		}(t)
	}
	wg.Wait()
	close(out)

	broken, hops := 0, 0
	var worst []string
	for r := range out {
		if r.status == 0 || r.status >= 400 || (r.final >= 400) {
			broken++
			if len(worst) < 10 {
				worst = append(worst, fmt.Sprintf("%s (from %s)", r.target, r.from))
			}
		} else if r.status >= 300 && r.status < 400 {
			hops++
		}
		db.Exec(`INSERT INTO twoai_link_audit (target, status, final_status, final_url, linked_from, checked_on)
			VALUES ($1,$2,$3,$4,$5,current_date)
			ON CONFLICT (target) DO UPDATE SET status=EXCLUDED.status, final_status=EXCLUDED.final_status,
				final_url=EXCLUDED.final_url, linked_from=EXCLUDED.linked_from, checked_on=current_date`,
			r.target, r.status, r.final, r.finalURL, r.from)
	}
	db.Exec(`DELETE FROM twoai_link_audit WHERE checked_on < current_date - 7`)
	fmt.Printf("thinpages: link audit: %d off-sitemap targets checked, %d broken, %d redirect (a working link that costs a hop)\n",
		len(todo), broken, hops)
	for _, w := range worst {
		fmt.Println("thinpages: broken link ->", w)
	}
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
