package main

// twoai_techgig_news: TechGig as a proper news source. Stephen, 2026-10-02,
// after the TechGig daily newsletter carried ten AI stories and the site had
// none of them. TechGig was already read by the intel stage through a
// site-scoped coverage feed (2026-09-29), and 51 of its articles sat in
// ai_intel_candidates in five days. None surfaced, because the daily briefing
// needs several outlets on one event and TechGig is usually the only one.
//
// Same shape as twoai_accounting_news: each new TechGig article becomes a
// one-outlet story, summarised by the model from the article's own text, with
// the TechGig link at the bottom. Unlike the accounting stories it is pinned
// to no section; it joins the general news. Only real articles under
// techgig.com/news/ qualify, so challenge, tag and listing pages never become
// stories. Articles whose text cannot be fetched or is too short stay in the
// candidates and are not summarised from a headline. Eight a run.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

func twoaiTechGigNews(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_techgig_news (
		url text PRIMARY KEY, story_uid text, outcome text NOT NULL, tried_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT DISTINCT ON (c.url) c.name, c.url, c.discovered_at FROM ai_intel_candidates c
		WHERE (c.url LIKE 'https://techgig.com/news/%' OR c.url LIKE 'https://www.techgig.com/news/%'
		       OR c.url LIKE 'https://content.techgig.com/%')
		  AND c.discovered_at > now() - interval '14 days'
		  AND NOT EXISTS (SELECT 1 FROM twoai_techgig_news t WHERE t.url = c.url)
		ORDER BY c.url, c.discovered_at DESC`)
	if err != nil {
		return err
	}
	type item struct {
		title, url string
		seen       time.Time
	}
	var items []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.title, &it.url, &it.seen) == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	// Newest first, eight a run.
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].seen.After(items[i].seen) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
	if len(items) > 8 {
		items = items[:8]
	}
	strip := regexp.MustCompile(`(?is)<(script|style|nav|header|footer|aside)[^>]*>.*?</(script|style|nav|header|footer|aside)>`)
	tail := regexp.MustCompile(`\s+[-|]\s+(TechGig|techgig\.com)\s*$`)
	client := &http.Client{Timeout: 25 * time.Second}
	made, skipped := 0, 0
	for _, it := range items {
		outcome := func(o, uid string) {
			db.Exec(`INSERT INTO twoai_techgig_news (url, story_uid, outcome) VALUES ($1, NULLIF($2,''), $3) ON CONFLICT (url) DO NOTHING`, it.url, uid, o)
		}
		req, _ := http.NewRequest("GET", it.url, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; theworldofai.org news reader; srj@srjconsultingservices.com)")
		resp, ferr := client.Do(req)
		if ferr != nil {
			outcome("fetch failed", "")
			skipped++
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
		resp.Body.Close()
		text := enfPlain(strip.ReplaceAllString(string(body), " "))
		if resp.StatusCode != 200 || len(text) < 1500 {
			outcome(fmt.Sprintf("no usable text (HTTP %d, %d chars)", resp.StatusCode, len(text)), "")
			skipped++
			continue
		}
		if len(text) > 14000 {
			text = text[:14000]
		}
		system := `You summarise one news article for The World of AI, a reference site about artificial intelligence read by business leaders, developers and policy readers. Use only facts stated in the article text given; do not add background, opinions or anything the article does not say. Ignore navigation, menus, related headlines, job listings and author bios in the text. Write two short paragraphs, 120 to 220 words in total, in plain English: what happened and who said it, then any figures or specifics the article gives. No headings, no bullet points, no quotation longer than ten words. If the article is not about artificial intelligence, reply with exactly: NOT RELEVANT`
		out, model, gerr := twoaiGenerate("news_summary", system, "Headline: "+it.title+"\n\nArticle text:\n"+text)
		out = strings.TrimSpace(out)
		if gerr != nil || out == "" {
			skipped++ // not recorded: retried next run
			continue
		}
		if strings.HasPrefix(out, "NOT RELEVANT") {
			outcome("not relevant", "")
			skipped++
			continue
		}
		headline := newsStripOutlet(strings.TrimSpace(tail.ReplaceAllString(it.title, "")), "techgig.com")
		slug := strings.Trim(acctSlugRe.ReplaceAllString(strings.ToLower(headline), "-"), "-")
		if len(slug) > 80 {
			slug = strings.Trim(slug[:80], "-")
		}
		// THE UID IS THE ONE THE SITE ADDRESSES THE STORY BY. Until 2026-10-02
		// this minted an md5 of the URL, while the archive and the site mint
		// sha256 of story:<slug>, so every pin and link to a one-outlet story
		// pointed at a uid that no page answered (158 stories, 112 pins).
		uid := twoaiUID("story:" + slug)
		domain := "techgig.com"
		pub := it.seen.Format("2006-01-02")
		story := map[string]any{
			"uid": uid, "Slug": slug, "Headline": headline, "Summary": out,
			"Articles": []map[string]string{{"URL": it.url, "Date": it.seen.UTC().Format(time.RFC3339), "Title": it.title, "Domain": domain}},
			"Domains":  []string{domain}, "DomainCount": 1, "ArticleCount": 1,
			"SummaryURL": it.url, "SummaryDomain": domain, "Orgs": []string{}, "Persons": []string{},
			"editor_note": "TechGig, taken as a news source on its own; summary by " + model + " from the article text.",
		}
		raw, _ := json.Marshal(story)
		if _, err := db.Exec(`INSERT INTO twoai_news_stories (slug, headline, story, published_on, first_published, last_seen, uid)
			VALUES ($1,$2,$3::jsonb,$4::date,now(),now(),$5) ON CONFLICT DO NOTHING`, slug, headline, string(raw), pub, uid); err != nil {
			return err
		}
		outcome("published", uid)
		made++
		fmt.Printf("twoai_techgig_news: %s\n", trunc(headline, 90))
		time.Sleep(400 * time.Millisecond)
	}
	fmt.Printf("twoai_techgig_news: candidates=%d published=%d skipped=%d ok=true\n", len(items), made, skipped)
	return nil
}
