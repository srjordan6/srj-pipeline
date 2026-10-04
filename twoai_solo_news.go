package main

// twoai_solo_news: one-outlet stories for a named subject the briefing would
// never surface.
//
// theworldofai, bridge row 355 (Stephen, 2026-10-02): follow Complete Defense
// Solutions, the wholly owned subsidiary of Complete Financial Solutions, Inc.
// (OTC: CFSU). A company that size is rarely covered by several outlets on one
// event, and the daily briefing needs several outlets on one event, so its
// coverage feed would fill ai_intel_candidates and publish nothing, which is
// what happened to TechGig before twoai_techgig_news. Same shape as that
// stage: each new candidate whose title names the subject becomes a story of
// its own, summarised by the model from the article text, with the link at
// the bottom, pinned to the subject's company page when it has one.
//
// Why the subject is matched by title and not by feed: the intel stage
// overwrites a coverage feed's vendor tag with the publisher host once the
// Google News link is resolved, so the tag is gone by the time this runs. The
// title is the stable key. A syndicated announcement arrives from several
// outlets with the same title; the first becomes the story and the rest are
// ledgered as duplicates of it, so one announcement is one story.

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

type soloSubject struct {
	key      string // ledger and log name
	label    string // as written in the editor note
	match    string // Postgres regex over the candidate title, case-insensitive
	pageUID  string // company page each story is pinned to, "" for none
	pagePath string
	// aiOnly marked the subjects that kept the AI-in-the-headline rule. Since
	// 2026-10-03 (bridge row 417, Stephen) every subject is judged from the
	// article itself by twoaiAIJudge instead; the field is kept as a record.
	aiOnly bool
}

var soloSubjects = []soloSubject{
	{
		key:      "complete-defense-solutions",
		label:    "Complete Defense Solutions",
		match:    `complete (defense|defence|financial) solutions|\mCFSU\M`,
		pageUID:  "2fc4d267", // twoaiUID("company:complete defense solutions")
		pagePath: "companies/2fc4d267.json",
	},
	// theworldofai row 375, Stephen 2026-10-02: company news on company pages
	// for the two followed with their own feeds since 2026-10-01 and 10-02.
	{
		key:      "kyndryl",
		label:    "Kyndryl",
		match:    `\mKyndryl\M`,
		pageUID:  "b0d1ccaf",
		pagePath: "companies/b0d1ccaf.json",
		aiOnly:   true,
	},
	{
		key:      "ntt-data",
		label:    "NTT DATA",
		match:    `\mNTT DATA\M`,
		pageUID:  "e07c8073",
		pagePath: "companies/e07c8073.json",
		aiOnly:   true,
	},
}

// soloFeedVendor reports an intel feed that follows one of the solo
// subjects ("Kyndryl press releases (coverage)", "NTT DATA (coverage)").
func soloFeedVendor(vendor string) bool {
	v := strings.ToLower(vendor)
	for _, s := range soloSubjects {
		if strings.Contains(v, strings.ToLower(s.label)) {
			return true
		}
	}
	return false
}

func twoaiSoloNews(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_solo_news (
		url text PRIMARY KEY, subject text NOT NULL, story_uid text, outcome text NOT NULL,
		tried_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	strip := regexp.MustCompile(`(?is)<(script|style|nav|header|footer|aside)[^>]*>.*?</(script|style|nav|header|footer|aside)>`)
	client := &http.Client{Timeout: 25 * time.Second}
	system := `You summarise one news article for The World of AI, a reference site about artificial intelligence read by business leaders, developers and policy readers. Use only facts stated in the article text given; do not add background, opinions or anything the article does not say. Ignore navigation, menus, related headlines, job listings and author bios in the text. Write two short paragraphs, 120 to 220 words in total, in plain English: what happened and who said it, then any figures or specifics the article gives. No headings, no bullet points, no quotation longer than ten words. If the article is not about artificial intelligence, reply with exactly: NOT RELEVANT`
	total, made, skipped := 0, 0, 0
	for _, sub := range soloSubjects {
		rows, err := db.Query(`SELECT DISTINCT ON (c.url) c.name, c.url, c.discovered_at FROM ai_intel_candidates c
			WHERE c.name ~* $1
			  AND c.discovered_at > now() - interval '21 days'
			  AND NOT EXISTS (SELECT 1 FROM twoai_solo_news t WHERE t.url = c.url AND t.outcome <> 'not about AI by title')
			ORDER BY c.url, c.discovered_at DESC`, sub.match)
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
		total += len(items)
		for _, it := range items {
			outcome := func(o, uid string) {
				db.Exec(`INSERT INTO twoai_solo_news (url, subject, story_uid, outcome) VALUES ($1, $2, NULLIF($3,''), $4)
					ON CONFLICT (url) DO UPDATE SET story_uid=EXCLUDED.story_uid, outcome=EXCLUDED.outcome, tried_at=now()`,
					it.url, sub.key, uid, o)
			}
			// Already on the site as part of a briefing story, or as an
			// earlier copy of the same announcement: ledgered, not repeated.
			var have string
			db.QueryRow(`SELECT uid FROM twoai_news_stories
				WHERE story->'Articles' @> jsonb_build_array(jsonb_build_object('URL', $1::text)) LIMIT 1`, it.url).Scan(&have)
			if have != "" {
				outcome("already in story "+have, have)
				skipped++
				continue
			}
			headline := newsStripOutlet(it.title, publisherFromURL(it.url))
			db.QueryRow(`SELECT uid FROM twoai_news_stories WHERE headline = $1
				AND published_on > current_date - 7 AND retired_at IS NULL LIMIT 1`, headline).Scan(&have)
			if have != "" {
				outcome("duplicate of "+have, have)
				skipped++
				continue
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
			// AI IS JUDGED FROM THE ARTICLE (bridge row 417, Stephen
			// 2026-10-03), not from words in the headline: a Kyndryl
			// infrastructure release can be an AI story without saying AI.
			// The verdict and its reason are kept in twoai_ai_verdicts with
			// the safety check's.
			yes, why, jmodel, jerr := twoaiAIJudge("solo_news_judge", sub.label, []string{it.title}, text)
			if jerr != nil {
				skipped++ // not recorded: retried next run
				continue
			}
			twoaiAIVerdictLog(db, "solo_news", sub.key, it.url, yes, why, jmodel)
			if !yes {
				outcome("judged not about AI: "+trunc(why, 200), "")
				skipped++
				continue
			}
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
			slug := strings.Trim(acctSlugRe.ReplaceAllString(strings.ToLower(headline), "-"), "-")
			if len(slug) > 80 {
				slug = strings.Trim(slug[:80], "-")
			}
			// The uid the site addresses the story by: sha256 of story:<slug>,
			// as the archive mints it (see twoai_techgig_news.go, 2026-10-02).
			uid := twoaiUID("story:" + slug)
			domain := publisherFromURL(it.url)
			pub := it.seen.Format("2006-01-02")
			story := map[string]any{
				"uid": uid, "Slug": slug, "Headline": headline, "Summary": out,
				"Articles": []map[string]string{{"URL": it.url, "Date": it.seen.UTC().Format(time.RFC3339), "Title": it.title, "Domain": domain}},
				"Domains":  []string{domain}, "DomainCount": 1, "ArticleCount": 1,
				"SummaryURL": it.url, "SummaryDomain": domain, "Orgs": []string{sub.label}, "Persons": []string{},
				"editor_note": sub.label + " is followed on its own, one outlet is enough; summary by " + model + " from the article text.",
			}
			raw, _ := json.Marshal(story)
			if _, err := db.Exec(`INSERT INTO twoai_news_stories (slug, headline, story, published_on, first_published, last_seen, uid)
				VALUES ($1,$2,$3::jsonb,$4::date,now(),now(),$5) ON CONFLICT DO NOTHING`, slug, headline, string(raw), pub, uid); err != nil {
				return err
			}
			if sub.pageUID != "" {
				db.Exec(`INSERT INTO twoai_page_news (page_uid, page_path, story_uid, story_slug, headline, published_on, placed_by, reason, active, added_at)
					SELECT $1, $2, $3, $4, $5, $6::date, 'solo feed', $7, true, now()
					WHERE NOT EXISTS (SELECT 1 FROM twoai_page_news WHERE page_uid=$1 AND story_uid=$3)`,
					sub.pageUID, sub.pagePath, uid, slug, headline, pub,
					"Coverage of "+sub.label+", attached to its company page automatically.")
			}
			outcome("published", uid)
			made++
			fmt.Printf("twoai_solo_news: %s: %s\n", sub.key, trunc(headline, 90))
			time.Sleep(400 * time.Millisecond)
		}
	}
	fmt.Printf("twoai_solo_news: subjects=%d candidates=%d published=%d skipped=%d ok=true\n", len(soloSubjects), total, made, skipped)
	return nil
}
