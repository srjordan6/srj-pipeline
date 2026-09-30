package main

// twoai_site_crawl: read a source's whole website before writing a page
// about it.
//
// Stephen, 2026-09-29, after the source summary pages turned out to describe
// homepages and error pages: "we have to go to a website and crawl it and
// digest all that it has to offer and then and only then do you make a web
// page about that content of that website."
//
// Three steps, each resumable and spread across runs:
//
//  1. Crawl. For every website an industry page cites, fetch its robots.txt
//     and sitemap, then walk its own pages, AI-related addresses first, up to
//     twoaiCrawlMaxPages, one request a second, never outside the host and
//     never where robots.txt says not to. Each page's readable text is kept.
//  2. Digest. The model reads the AI-relevant pages in batches and records,
//     for each, what it is and the specific findings, figures, guidance,
//     programmes or uses of AI it contains, with the page's own address.
//  3. Write. Only when a site has been crawled and digested is its page
//     written, from the digest alone, with the specific pages it drew on
//     listed at the foot. A site whose digest holds nothing substantive gets
//     no page.
//
// Two sites are crawled and two digested per run, so the 128 cited sites
// are all read within a few days of the stage shipping and re-read every 90.

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	twoaiCrawlMaxPages    = 60
	twoaiCrawlSitesPerRun = 2
	twoaiDigestPerRun     = 2
	twoaiCrawlRefreshDays = 90
)

var crawlUA = "theworldofai.org site reader (+https://theworldofai.org/editorial-policy/; srj@srjconsultingservices.com)"
var crawlSkipExt = regexp.MustCompile(`(?i)\.(pdf|jpe?g|png|gif|svg|webp|ico|zip|gz|mp4|mp3|mov|avi|docx?|xlsx?|pptx?|css|js|json|xml|rss|woff2?|ttf)(\?|$)`)
var crawlSkipPath = regexp.MustCompile(`(?i)/(login|signin|sign-in|register|cart|checkout|account|my-account|search|wp-admin|wp-login|subscribe|unsubscribe|privacy|cookie|terms|careers?|jobs?|donate|shop|store|tag|author|feed)(/|$)|mailto:|tel:|javascript:`)
var crawlPriority = regexp.MustCompile(`(?i)(artificial-intelligence|/ai[/-]|-ai[/-]|/ai$|machine-learning|automation|autonomous|generative|agentic|algorithm|analytics|data-science|innovation|technology|digital|research|report|insight|study|survey|guidance|standard|framework|policy|publication|whitepaper|white-paper|case-stud|resource)`)

// crawlStrongAI marks addresses that are about AI itself. A path that says
// only "technology" or "insights" is a general section; on aicpa-cima.com,
// 2026-09-29, sixty such pages filled the budget and none of them mentioned AI.
var crawlStrongAI = regexp.MustCompile(`(?i)(artificial-intelligence|artificial_intelligence|/ai[/-]|-ai[/-]|-ai$|/ai$|machine-learning|generative|genai|gen-ai|agentic|llm|large-language)`)

var crawlHrefRe = regexp.MustCompile(`(?is)<a\s[^>]*href\s*=\s*["']([^"'#]+)["'][^>]*>(.*?)</a>`)
var crawlBlockRe = regexp.MustCompile(`(?i)</(p|div|li|h1|h2|h3|h4|h5|td|tr|section|article|blockquote)>|<br\s*/?>`)

// One pattern per element: RE2 has no back-references, and a shared
// alternation let <style> close on a later </svg>, leaking CSS into the text.
var crawlDropRes = func() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, t := range []string{"script", "style", "noscript", "svg", "nav", "header", "footer", "form", "iframe"} {
		out = append(out, regexp.MustCompile(`(?is)<`+t+`\b[^>]*>.*?</`+t+`\s*>`))
	}
	return out
}()

func crawlText(raw []byte) (string, string) {
	title := ""
	if m := ihTitleRe.FindSubmatch(raw); m != nil {
		title = strings.TrimSpace(enfPlain(string(m[1])))
	}
	s := string(raw)
	for _, re := range crawlDropRes {
		s = re.ReplaceAllString(s, " ")
	}
	s = crawlBlockRe.ReplaceAllString(s, "\n")
	s = ihTagRe.ReplaceAllString(s, " ")
	var keep []string
	seen := map[string]bool{}
	total := 0
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ihWsRe.ReplaceAllString(enfPlain(ln), " "))
		if len(ln) < 60 || strings.Count(ln, "{")+strings.Count(ln, ";") > 6 {
			continue
		}
		k := ln
		if len(k) > 80 {
			k = k[:80]
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		keep = append(keep, ln)
		total += len(ln)
		if total > 20000 {
			break
		}
	}
	return title, strings.Join(keep, "\n")
}

type robotsRules struct{ disallow []string }

func (r robotsRules) allowed(path string) bool {
	for _, d := range r.disallow {
		if d != "" && strings.HasPrefix(path, d) {
			return false
		}
	}
	return true
}

func crawlFetch(client *http.Client, u string) (int, []byte, string, error) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", crawlUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
	return resp.StatusCode, b, resp.Request.URL.String(), nil
}

func crawlRobots(client *http.Client, root string) robotsRules {
	var r robotsRules
	st, b, _, err := crawlFetch(client, root+"/robots.txt")
	if err != nil || st != 200 {
		return r
	}
	applies := false
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(strings.SplitN(ln, "#", 2)[0])
		low := strings.ToLower(ln)
		switch {
		case strings.HasPrefix(low, "user-agent:"):
			ua := strings.TrimSpace(ln[len("user-agent:"):])
			applies = ua == "*"
		case applies && strings.HasPrefix(low, "disallow:"):
			r.disallow = append(r.disallow, strings.TrimSpace(ln[len("disallow:"):]))
		}
	}
	return r
}

func crawlSitemap(client *http.Client, root string) []string {
	var out []string
	var walk func(u string, depth int)
	walk = func(u string, depth int) {
		if depth > 2 || len(out) > 3000 {
			return
		}
		st, b, _, err := crawlFetch(client, u)
		if err != nil || st != 200 {
			return
		}
		var sm struct {
			Locs []string `xml:"url>loc"`
			Subs []string `xml:"sitemap>loc"`
		}
		dec := xml.NewDecoder(bytes.NewReader(b))
		dec.Strict = false
		if dec.Decode(&sm) != nil {
			return
		}
		out = append(out, sm.Locs...)
		for i, s := range sm.Subs {
			if i >= 6 {
				break
			}
			walk(strings.TrimSpace(s), depth+1)
		}
	}
	walk(root+"/sitemap.xml", 0)
	return out
}

func crawlHost(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(p.Hostname()), "www.")
}

// twoaiCrawlSite walks one website and stores its pages.
func twoaiCrawlSite(db *sql.DB, domain string, starts []string) (int, int) {
	client := &http.Client{Timeout: 20 * time.Second}
	scheme := "https://"
	if p, err := url.Parse(starts[0]); err == nil && p.Scheme != "" {
		scheme = p.Scheme + "://"
	}
	hostForm := domain
	if p, err := url.Parse(starts[0]); err == nil && p.Host != "" {
		hostForm = p.Host
	}
	root := scheme + hostForm
	rules := crawlRobots(client, root)

	type cand struct {
		u     string
		score int
	}
	queue := []cand{}
	seen := map[string]bool{}
	push := func(u, anchor string, base int) {
		p, err := url.Parse(strings.TrimSpace(u))
		if err != nil {
			return
		}
		p.Fragment = ""
		if crawlHost(p.String()) != domain || crawlSkipExt.MatchString(p.Path) || crawlSkipPath.MatchString(p.Path+" ") {
			return
		}
		if len(p.RawQuery) > 40 {
			return
		}
		k := strings.TrimSuffix(p.Scheme+"://"+p.Host+p.Path, "/") + "?" + p.RawQuery
		if seen[k] || !rules.allowed(p.EscapedPath()) {
			return
		}
		seen[k] = true
		sc := base
		if crawlStrongAI.MatchString(p.Path) {
			sc += 30
		} else if crawlPriority.MatchString(p.Path) {
			sc += 10
		}
		if aiTermRe.MatchString(anchor) {
			sc += 6
		}
		queue = append(queue, cand{p.String(), sc})
	}
	for _, s := range starts {
		push(s, "", 100)
	}
	push(root+"/", "", 50)
	for _, s := range crawlSitemap(client, root) {
		if crawlStrongAI.MatchString(s) {
			push(s, "", 20)
		} else if crawlPriority.MatchString(s) {
			push(s, "", 0)
		}
	}
	fetched, relevant, okPages, okChars := 0, 0, 0, 0
	for len(queue) > 0 && fetched < twoaiCrawlMaxPages {
		sort.SliceStable(queue, func(i, j int) bool { return queue[i].score > queue[j].score })
		c := queue[0]
		queue = queue[1:]
		st, body, final, err := crawlFetch(client, c.u)
		time.Sleep(1 * time.Second)
		if err != nil {
			continue
		}
		fetched++
		if crawlHost(final) != domain {
			continue
		}
		title, text := crawlText(body)
		score := len(aiTermRe.FindAllString(text, -1))
		if sourceErrorPage.MatchString(trunc(text, 1500)) {
			st = 404
		}
		h := sha256.Sum256([]byte(text))
		db.Exec(`INSERT INTO twoai_site_crawl_pages (url, domain, title, text, ai_score, http_status, fetched_on, content_hash)
			VALUES ($1,$2,$3,$4,$5,$6,current_date,$7)
			ON CONFLICT (url) DO UPDATE SET title=$3, text=$4, ai_score=$5, http_status=$6, fetched_on=current_date, content_hash=$7`,
			final, domain, title, text, score, st, hex.EncodeToString(h[:8]))
		if st == 200 {
			okPages++
			okChars += len(text)
		}
		if st == 200 && score >= 3 && len(text) > 800 {
			relevant++
		}
		if st != 200 {
			continue
		}
		for _, m := range crawlHrefRe.FindAllSubmatch(body, 400) {
			ref, err := url.Parse(string(m[1]))
			if err != nil {
				continue
			}
			base, _ := url.Parse(final)
			push(base.ResolveReference(ref).String(), enfPlain(string(m[2])), 0)
		}
	}
	// UNREADABLE IS NOT EMPTY. A site that serves a script shell, or blocks
	// the reader after a page or two, has not been read, so its lack of AI
	// material proves nothing. aicpa-cima.com gave sixty pages of about 650
	// characters each; airbus.com gave two. Such a site is marked unreadable
	// and gets no page, and no judgement is recorded against it.
	status := "read"
	if relevant == 0 && (okPages < 10 || okChars/max(okPages, 1) < 1200) {
		status = "unreadable"
	}
	db.Exec(`INSERT INTO twoai_site_crawl (domain, start_url, pages_fetched, pages_relevant, crawled_on, crawl_status)
		VALUES ($1,$2,$3,$4,current_date,$5)
		ON CONFLICT (domain) DO UPDATE SET start_url=$2, pages_fetched=$3, pages_relevant=$4, crawled_on=current_date, crawl_status=$5`,
		domain, starts[0], fetched, relevant, status)
	return fetched, relevant
}

type siteFinding struct {
	URL    string   `json:"url"`
	What   string   `json:"what"`
	Facts  []string `json:"facts"`
	Date   string   `json:"date"`
	Useful bool     `json:"useful"`
	Kind   string   `json:"kind"`
}

// twoaiDigestSite reads a crawled site's AI-relevant pages in batches and
// stores what each one actually says.
func twoaiDigestSite(db *sql.DB, domain, industry string) (int, error) {
	rows, err := db.Query(`SELECT url, COALESCE(title,''), text FROM twoai_site_crawl_pages
		WHERE domain=$1 AND http_status=200 AND ai_score >= 3 AND length(text) > 800
		ORDER BY ai_score DESC LIMIT 24`, domain)
	if err != nil {
		return 0, err
	}
	type pg struct{ u, t, x string }
	var pages []pg
	for rows.Next() {
		var p pg
		if rows.Scan(&p.u, &p.t, &p.x) == nil {
			pages = append(pages, p)
		}
	}
	rows.Close()
	var findings []siteFinding
	model := ""
	sys := "You read pages from one organisation's website for theworldofai.org, for a reader following artificial intelligence in the " + industry + " industry. " +
		"For each page, report only what the page itself says: what the page is, and the specific findings, figures, guidance, requirements, programmes, products or documented uses of AI it contains, each as a short factual sentence in your own words with any number or date exactly as the page gives it. " +
		"Mark a page useful only if it contains such substance; navigation, membership, event promotion, marketing slogans and error pages are not useful. Add nothing the page does not say. " +
		`Return only JSON: {"pages": [{"url": "<the page address given>", "what": "<one sentence: what this page is>", "kind": "<report|guidance|standard|research|case study|product|news|course|other>", "date": "<publication date if stated, else empty>", "useful": true or false, "facts": ["<fact>", "..."]}]}`
	batch, size := []pg{}, 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		var sb strings.Builder
		for _, p := range batch {
			fmt.Fprintf(&sb, "=== PAGE %s\nTitle: %s\n%s\n\n", p.u, p.t, trunc(p.x, 5000))
		}
		out, m, gerr := twoaiGenerate("twoai_source_pages", sys, sb.String())
		batch, size = batch[:0], 0
		if gerr != nil {
			return
		}
		model = m
		if i := strings.Index(out, "{"); i >= 0 {
			out = out[i:]
		}
		if k := strings.LastIndex(out, "}"); k >= 0 {
			out = out[:k+1]
		}
		var res struct {
			Pages []siteFinding `json:"pages"`
		}
		if json.Unmarshal([]byte(out), &res) == nil {
			findings = append(findings, res.Pages...)
		}
	}
	for _, p := range pages {
		n := len(trunc(p.x, 5000))
		if size+n > 15000 {
			flush()
		}
		batch = append(batch, p)
		size += n
	}
	flush()
	useful := 0
	for _, f := range findings {
		if f.Useful && len(f.Facts) > 0 {
			useful++
		}
	}
	raw, _ := json.Marshal(findings)
	db.Exec(`UPDATE twoai_site_crawl SET digest=$2::jsonb, digested_on=current_date, useful_pages=$3, model=$4 WHERE domain=$1`,
		domain, string(raw), useful, model)
	return useful, nil
}

// twoaiSiteCrawlStep runs the crawl and digest work for this run and returns
// the digests ready to be written, keyed by domain.
func twoaiSiteCrawlStep(db *sql.DB, cited map[string][]string, industryOf map[string]string) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_site_crawl (domain text PRIMARY KEY, start_url text,
		pages_fetched int, pages_relevant int, crawled_on date, digest jsonb, digested_on date, useful_pages int, model text)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_site_crawl_pages (url text PRIMARY KEY, domain text NOT NULL, title text,
		text text, ai_score int, http_status int, fetched_on date, content_hash text)`)
	db.Exec(`ALTER TABLE twoai_site_crawl ADD COLUMN IF NOT EXISTS crawl_status text`)
	db.Exec(`CREATE INDEX IF NOT EXISTS twoai_site_crawl_pages_domain ON twoai_site_crawl_pages (domain)`)
	var domains []string
	for d := range cited {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	crawled := 0
	for _, d := range domains {
		if crawled >= twoaiCrawlSitesPerRun || d == "theworldofai.org" {
			continue
		}
		var last sql.NullTime
		db.QueryRow(`SELECT crawled_on FROM twoai_site_crawl WHERE domain=$1`, d).Scan(&last)
		if last.Valid && time.Since(last.Time) < twoaiCrawlRefreshDays*24*time.Hour {
			continue
		}
		f, r := twoaiCrawlSite(db, d, cited[d])
		fmt.Printf("twoai_site_crawl: %s fetched=%d relevant=%d\n", d, f, r)
		crawled++
	}
	digested := 0
	for _, d := range domains {
		if digested >= twoaiDigestPerRun {
			break
		}
		var need bool
		db.QueryRow(`SELECT crawled_on IS NOT NULL AND (digested_on IS NULL OR digested_on < crawled_on) FROM twoai_site_crawl WHERE domain=$1`, d).Scan(&need)
		if !need {
			continue
		}
		u, err := twoaiDigestSite(db, d, industryOf[d])
		if err == nil {
			fmt.Printf("twoai_site_crawl: digested %s useful_pages=%d\n", d, u)
		}
		digested++
	}
}
