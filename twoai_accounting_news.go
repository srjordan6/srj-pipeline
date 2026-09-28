package main

// twoai_accounting_news: the accounting trade press, attached to the AI
// Accountant section. Stephen, 2026-09-28, after finding Accounting Today's
// "PwC puts guardrails on AI in audit with human oversight" in the corpus but
// on no page: the daily briefing needs several outlets on one event, so a
// single trade press story never surfaces, however relevant.
//
// The intel stage reads four coverage feeds tagged (accounting). Here each new
// item from them becomes a one-outlet news story, summarised by the model from
// the article's own text, and is pinned to the AI Accountant hub. Stories whose
// text cannot be fetched or is too short are left in the candidates, not
// summarised from a headline. Eight a run.

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const twoaiAccountantPageUID = "5b1526bd"
const twoaiAccountantPagePath = "industries/fin-fin.json"

var acctSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

func twoaiAccountingNews(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_accounting_news (
		url text PRIMARY KEY, story_uid text, outcome text NOT NULL, tried_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT c.name, c.url, c.vendor, c.discovered_at FROM ai_intel_candidates c
		WHERE c.vendor LIKE '%(accounting)%' AND c.discovered_at > now() - interval '21 days'
		  AND NOT EXISTS (SELECT 1 FROM twoai_accounting_news a WHERE a.url = c.url)
		ORDER BY c.discovered_at DESC LIMIT 8`)
	if err != nil {
		return err
	}
	type item struct {
		title, url, vendor string
		seen               time.Time
	}
	var items []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.title, &it.url, &it.vendor, &it.seen) == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	client := &http.Client{Timeout: 25 * time.Second}
	made, skipped := 0, 0
	for _, it := range items {
		outcome := func(o, uid string) {
			db.Exec(`INSERT INTO twoai_accounting_news (url, story_uid, outcome) VALUES ($1, NULLIF($2,''), $3) ON CONFLICT (url) DO NOTHING`, it.url, uid, o)
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
		text := enfPlain(regexp.MustCompile(`(?is)<(script|style|nav|header|footer)[^>]*>.*?</(script|style|nav|header|footer)>`).ReplaceAllString(string(body), " "))
		if resp.StatusCode != 200 || len(text) < 1500 {
			outcome(fmt.Sprintf("no usable text (HTTP %d, %d chars)", resp.StatusCode, len(text)), "")
			skipped++
			continue
		}
		if len(text) > 14000 {
			text = text[:14000]
		}
		system := `You summarise one news article for The World of AI, a reference site read by accountants and business owners. Use only facts stated in the article text given; do not add background, opinions or anything the article does not say. Ignore navigation, menus, related headlines and author bios in the text. Write two short paragraphs, 120 to 220 words in total, in plain English: what happened and who said it, then any figures or specifics the article gives. No headings, no bullet points, no quotation longer than ten words. If the article is not about artificial intelligence in accounting, audit, tax or finance, reply with exactly: NOT RELEVANT`
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
		h := md5.Sum([]byte("story:" + it.url))
		uid := hex.EncodeToString(h[:])[:8]
		headline := strings.TrimSpace(regexp.MustCompile(`\s+-\s+[^-]+$`).ReplaceAllString(it.title, ""))
		slug := strings.Trim(acctSlugRe.ReplaceAllString(strings.ToLower(headline), "-"), "-")
		if len(slug) > 80 {
			slug = strings.Trim(slug[:80], "-")
		}
		domain := it.url
		if i := strings.Index(domain, "://"); i >= 0 {
			domain = domain[i+3:]
		}
		domain = strings.TrimPrefix(strings.SplitN(domain, "/", 2)[0], "www.")
		pub := it.seen.Format("2006-01-02")
		story := map[string]any{
			"uid": uid, "Slug": slug, "Headline": headline, "Summary": out,
			"Articles": []map[string]string{{"URL": it.url, "Date": it.seen.UTC().Format(time.RFC3339), "Title": it.title, "Domain": domain}},
			"Domains":  []string{domain}, "DomainCount": 1, "ArticleCount": 1,
			"SummaryURL": it.url, "SummaryDomain": domain, "Orgs": []string{}, "Persons": []string{},
			"editor_note": "Accounting trade press, attached to the AI Accountant section automatically; summary by " + model + " from the article text.",
		}
		raw, _ := json.Marshal(story)
		if _, err := db.Exec(`INSERT INTO twoai_news_stories (slug, headline, story, published_on, first_published, last_seen, uid)
			VALUES ($1,$2,$3::jsonb,$4::date,now(),now(),$5) ON CONFLICT DO NOTHING`, slug, headline, string(raw), pub, uid); err != nil {
			return err
		}
		db.Exec(`INSERT INTO twoai_page_news (page_uid, page_path, story_uid, story_slug, headline, published_on, placed_by, reason, active, added_at)
			SELECT $1,$2,$3,$4,$5,$6::date,'accounting feed','Accounting trade press on AI, attached to the AI Accountant section automatically.',true,now()
			WHERE NOT EXISTS (SELECT 1 FROM twoai_page_news WHERE page_uid=$1 AND story_uid=$3)`,
			twoaiAccountantPageUID, twoaiAccountantPagePath, uid, slug, headline, pub)
		outcome("published", uid)
		made++
		fmt.Printf("twoai_accounting_news: %s [%s]\n", trunc(headline, 90), domain)
		time.Sleep(400 * time.Millisecond)
	}
	fmt.Printf("twoai_accounting_news: candidates=%d published=%d skipped=%d ok=true\n", len(items), made, skipped)
	return nil
}
