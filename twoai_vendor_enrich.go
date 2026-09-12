package main

// twoai_vendor_enrich: read the description the vendor already published.
//
// Stephen, 2026-09-11, on a Vercel post rendering as a stub: "here is a page
// where we could have added more content if we just read the website." He was
// right, and it was not one page. 2,733 of 5,689 live vendor posts - 48% -
// carried no summary, and each of those pages told the reader the vendor
// "did not publish a per-post description for this one" and sent them away.
//
// That sentence was false. Vercel's own post carries a 240-character
// og:description in its HTML; the RSS item simply omitted it. The feed was
// the only thing ever read, so an absent <description> was recorded as "the
// vendor published none" when it meant "this feed does not carry one".
//
// This stage fetches the post's own URL for summary-less rows and reads, in
// order: og:description, twitter:description, meta description, and the
// JSON-LD description field. All four are metadata the publisher wrote to be
// syndicated - the same fields every social platform and search engine reads
// - so using them is the intended use, not scraping. Nothing else is taken
// from the page: no body text, no headings, no images. The link still goes
// to the original, the words are still the vendor's, and the page still says
// they are the vendor's claims and not verified facts.
//
// A post whose page genuinely publishes no description keeps the honest
// stub, and is marked so it is not refetched every night forever.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

var (
	vendorMetaRe = regexp.MustCompile(`(?is)<meta[^>]+(?:property|name)\s*=\s*["'](og:description|twitter:description|description)["'][^>]*>`)
	vendorContRe = regexp.MustCompile(`(?is)content\s*=\s*["']([^"']*)["']`)
	vendorLdRe   = regexp.MustCompile(`(?is)<script[^>]+application/ld\+json[^>]*>([\s\S]*?)</script>`)
	vendorTagRe  = regexp.MustCompile(`(?s)<[^>]+>`)
	vendorWsRe   = regexp.MustCompile(`\s+`)
	// Body extraction, for pages whose metadata is a site-wide tagline.
	// Go's regexp is RE2 and has no backreferences, so each element is
	// spelled out rather than matched with \1 - the first draft used a
	// backreference, which panicked at init and took EVERY stage of the
	// binary down with it on 2026-09-12. One pattern per element, joined.
	vendorChromeRe  = regexp.MustCompile(`(?is)<script[^>]*>[\s\S]*?</script\s*>|<style[^>]*>[\s\S]*?</style\s*>|<nav[^>]*>[\s\S]*?</nav\s*>|<header[^>]*>[\s\S]*?</header\s*>|<footer[^>]*>[\s\S]*?</footer\s*>|<form[^>]*>[\s\S]*?</form\s*>|<noscript[^>]*>[\s\S]*?</noscript\s*>|<svg[^>]*>[\s\S]*?</svg\s*>|<aside[^>]*>[\s\S]*?</aside\s*>`)
	vendorArticleRe = regexp.MustCompile(`(?is)<article[^>]*>([\s\S]*?)</article\s*>|<main[^>]*>([\s\S]*?)</main\s*>`)
	vendorParaRe    = regexp.MustCompile(`(?is)<p[^>]*>([\s\S]*?)</p>`)
)

// twoaiVendorBody pulls the opening prose of a post out of its own page, as
// INPUT for the model to summarise. It is never rendered: this text is the
// publisher's words, and this site publishes its own.
func twoaiVendorBody(body string) string {
	s := vendorChromeRe.ReplaceAllString(body, " ")
	if m := vendorArticleRe.FindStringSubmatch(s); m != nil {
		// Two capture groups, one per alternative; whichever matched is set.
		inner := m[1]
		if inner == "" {
			inner = m[2]
		}
		if len(inner) > 400 {
			s = inner
		}
	}
	var out []string
	n := 0
	for _, m := range vendorParaRe.FindAllStringSubmatch(s, -1) {
		p := strings.TrimSpace(vendorWsRe.ReplaceAllString(
			html.UnescapeString(vendorTagRe.ReplaceAllString(m[1], " ")), " "))
		// Short lines are captions, bylines, cookie banners and button text.
		if len([]rune(p)) < 60 {
			continue
		}
		out = append(out, p)
		n += len([]rune(p))
		if n > 4000 {
			break
		}
	}
	return strings.Join(out, "\n\n")
}

const vendorBodySystem = `You write one or two sentences saying what a company's announcement post is about, for a reference site that links out to it.

ABSOLUTE RULES:
- Use ONLY what the supplied text says. Never add background knowledge, and never name a product, figure, company or date that is not in the text.
- Write in your own words. Do NOT copy the post's sentences or reuse its opening line.
- One or two sentences, under 300 characters. Plain and factual.
- Say what was announced or described. Not that a post exists, not what the reader should do. No marketing language.

If the text has nothing substantive to describe, output exactly: NOTHING`

// twoaiVendorDescription pulls the publisher's own syndication description
// out of a page. Ordered by intent: og:description is written for exactly
// this purpose, the meta description is written for search, and the JSON-LD
// description is the structured-data equivalent. First non-empty wins.
func twoaiVendorDescription(body string) string {
	best := map[string]string{}
	for _, m := range vendorMetaRe.FindAllString(body, -1) {
		key := ""
		switch {
		case strings.Contains(strings.ToLower(m), "og:description"):
			key = "og"
		case strings.Contains(strings.ToLower(m), "twitter:description"):
			key = "tw"
		default:
			key = "meta"
		}
		c := vendorContRe.FindStringSubmatch(m)
		if c == nil {
			continue
		}
		v := strings.TrimSpace(html.UnescapeString(c[1]))
		if v != "" && best[key] == "" {
			best[key] = v
		}
	}
	if v := best["og"]; v != "" {
		return v
	}
	if v := best["tw"]; v != "" {
		return v
	}
	if v := best["meta"]; v != "" {
		return v
	}
	// JSON-LD last: it is the least consistently populated of the four and
	// can be an array or a graph, so it is walked rather than type-asserted.
	for _, m := range vendorLdRe.FindAllStringSubmatch(body, -1) {
		var any1 any
		if json.Unmarshal([]byte(m[1]), &any1) != nil {
			continue
		}
		if d := twoaiFindDescription(any1, 0); d != "" {
			return d
		}
	}
	return ""
}

func twoaiFindDescription(v any, depth int) string {
	if depth > 6 {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		if s, ok := t["description"].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
		for _, x := range t {
			if d := twoaiFindDescription(x, depth+1); d != "" {
				return d
			}
		}
	case []any:
		for _, x := range t {
			if d := twoaiFindDescription(x, depth+1); d != "" {
				return d
			}
		}
	}
	return ""
}

func twoaiVendorEnrich(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE twoai_vendor_posts
		ADD COLUMN IF NOT EXISTS summary_source text,
		ADD COLUMN IF NOT EXISTS enriched_at timestamptz,
		ADD COLUMN IF NOT EXISTS enrich_attempts int NOT NULL DEFAULT 0`); err != nil {
		return err
	}

	// Cap per run: this is a courtesy fetch against other people's servers,
	// spread over nights rather than hammered in one. 2,733 rows clear in
	// about a week at this rate, newest first so the visible pages fill in
	// first. A row that has been tried three times and yielded nothing is
	// left alone: its page genuinely publishes no description.
	limit := 400
	if v := strings.TrimSpace(os.Getenv("TWOAI_VENDOR_ENRICH_LIMIT")); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	rows, err := db.Query(`SELECT slug, url, COALESCE(title,'') FROM twoai_vendor_posts
		WHERE retired_at IS NULL AND COALESCE(summary,'')='' AND enrich_attempts < 3
		  AND url LIKE 'http%'
		ORDER BY posted_on DESC NULLS LAST LIMIT $1`, limit)
	if err != nil {
		return err
	}
	type post struct{ slug, url, title string }
	var todo []post
	for rows.Next() {
		var p post
		if rows.Scan(&p.slug, &p.url, &p.title) == nil {
			todo = append(todo, p)
		}
	}
	rows.Close()
	if len(todo) == 0 {
		fmt.Println("twoai_vendor_enrich: nothing to enrich")
		return nil
	}

	client := &http.Client{Timeout: 30 * time.Second}
	filled, empty, failed, boiler := 0, 0, 0, 0
	fromMeta, fromBody := 0, 0
	// Per-host pacing: several hundred posts can share one vendor, and a
	// burst at one company's blog is rude regardless of robots.txt.
	lastHost := map[string]time.Time{}

	for _, p := range todo {
		host := ""
		if i := strings.Index(p.url, "://"); i > 0 {
			host = p.url[i+3:]
			if j := strings.IndexByte(host, '/'); j > 0 {
				host = host[:j]
			}
		}
		if t, ok := lastHost[host]; ok {
			if d := time.Second*2 - time.Since(t); d > 0 {
				time.Sleep(d)
			}
		}
		lastHost[host] = time.Now()

		req, _ := http.NewRequest("GET", p.url, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; SRJ-Consulting-research/1.0; +https://theworldofai.org/about/)")
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		resp, err := client.Do(req)
		if err != nil {
			db.Exec(`UPDATE twoai_vendor_posts SET enrich_attempts=enrich_attempts+1 WHERE slug=$1`, p.slug)
			failed++
			continue
		}
		bodyB, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		code := resp.StatusCode
		resp.Body.Close()
		if code != 200 {
			db.Exec(`UPDATE twoai_vendor_posts SET enrich_attempts=enrich_attempts+1 WHERE slug=$1`, p.slug)
			failed++
			continue
		}

		desc := twoaiVendorDescription(string(bodyB))
		desc = strings.TrimSpace(vendorWsRe.ReplaceAllString(vendorTagRe.ReplaceAllString(desc, " "), " "))
		source := "page-metadata"

		// IS THIS DESCRIPTION ACTUALLY ABOUT THIS POST? A description already
		// stored against another post from the same vendor is a site-wide
		// tagline, not a summary: every Hugging Face post carries "We're on a
		// journey to advance and democratize artificial intelligence", 107 of
		// them, and Mistral serves its product pitch on all twelve of its.
		boilerplate := false
		if len([]rune(desc)) >= 40 {
			var dupes int
			db.QueryRow(`SELECT count(*) FROM twoai_vendor_posts
				WHERE vendor=(SELECT vendor FROM twoai_vendor_posts WHERE slug=$1) AND summary=$2 AND slug <> $1`,
				p.slug, desc).Scan(&dupes)
			boilerplate = dupes > 0
		}

		// THE BODY IS THE FALLBACK, AND FOR SOME VENDORS IT IS THE ONLY PATH.
		// Stephen, 2026-09-12: Hugging Face had 437 posts given up on and 406
		// still queued against 24 real summaries, because their metadata is one
		// tagline sitewide. Those posts have real content in the page body; the
		// metadata simply does not describe it. So when the metadata is missing
		// or is a tagline, the article's own prose is read and the model writes
		// OUR sentence from it.
		//
		// Written, not copied. Taking the post's opening lines verbatim would
		// be reproduction, and this site's standing rule since the research
		// pages is that we publish our interpretation and link to the original.
		// The prompt forbids naming anything absent from the text, which is the
		// same no-invention guard the paper and point briefs use.
		if (len([]rune(desc)) < 40 || boilerplate) && (os.Getenv("ANTHROPIC_API_KEY") != "" || twoaiLLMFor("vendor_enrich") == "ollama") {
			if prose := twoaiVendorBody(string(bodyB)); len([]rune(prose)) >= 400 {
				if out, usedModel, cerr := twoaiGenerate("vendor_enrich", vendorBodySystem,
					"Post title: "+p.title+"\n\nText of the post:\n\n"+prose); cerr == nil {
					w := strings.TrimSpace(out)
					if w != "" && !strings.HasPrefix(w, "NOTHING") && len([]rune(w)) >= 40 {
						// The model is recorded in summary_source, not just
						// "page-body": when a local model takes over this job,
						// the only way to compare its output against Claude's
						// later is to know which wrote which.
						desc, source, boilerplate = w, "page-body:"+usedModel, false
					}
				}
				time.Sleep(900 * time.Millisecond)
			}
		}

		// A description that is just the title restated adds nothing, and a
		// one-word fragment is not a summary. Better the honest stub.
		if len([]rune(desc)) < 40 {
			db.Exec(`UPDATE twoai_vendor_posts SET enrich_attempts=enrich_attempts+1, enriched_at=now() WHERE slug=$1`, p.slug)
			empty++
			continue
		}
		// Still a tagline after the body attempt: nothing on this page is about
		// this post. The first post to carry it keeps it and the rest do not,
		// which is right either way - if it genuinely is that post's
		// description it is still true, and if it is a tagline then one page is
		// wrong instead of a hundred.
		if boilerplate {
			db.Exec(`UPDATE twoai_vendor_posts SET enrich_attempts=3, enriched_at=now() WHERE slug=$1`, p.slug)
			boiler++
			continue
		}
		if len([]rune(desc)) > 600 {
			r := []rune(desc)
			cut := 600
			for cut > 400 && r[cut] != ' ' {
				cut--
			}
			desc = strings.TrimSpace(string(r[:cut])) + "\u2026"
		}
		if _, err := db.Exec(`UPDATE twoai_vendor_posts
			SET summary=$1, summary_source=$2, enriched_at=now(),
				enrich_attempts=enrich_attempts+1
			WHERE slug=$3 AND COALESCE(summary,'')=''`, desc, source, p.slug); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_vendor_enrich:", p.slug, err)
			failed++
			continue
		}
		filled++
		if strings.HasPrefix(source, "page-body") {
			fromBody++
		} else {
			fromMeta++
		}
	}

	var remaining, total int
	db.QueryRow(`SELECT count(*) FILTER (WHERE COALESCE(summary,'')=''), count(*)
		FROM twoai_vendor_posts WHERE retired_at IS NULL`).Scan(&remaining, &total)
	fmt.Printf("twoai_vendor_enrich: filled=%d (metadata=%d body=%d) no_description=%d site_tagline_rejected=%d failed=%d | %d of %d posts still without a summary\n",
		filled, fromMeta, fromBody, empty, boiler, failed, remaining, total)
	return nil
}
