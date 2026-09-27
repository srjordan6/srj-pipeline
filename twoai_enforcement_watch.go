package main

// twoai_enforcement_watch: AI enforcement actions by the FTC and the SEC, read
// from the agencies' own feeds. Stephen, 2026-09-27, after the weekly watch
// surfaced FTC orders against Cox Media Group and two others and an SEC
// settlement with GenesisAI that the site had no way to see: they are neither
// bills nor court cases.
//
// Once a day. The FTC press release feeds carry the whole release, so an item
// is kept when its text names AI. The SEC litigation release and press
// release feeds carry only a title, so each new item's page is fetched once
// and kept when its text names AI. Every kept action is published on
// /ai-compliance/ai-enforcement-actions/, newest first, with the agency's own
// link; nothing is summarised beyond the agency's first paragraph.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var enforcementAIRe = regexp.MustCompile(`(?i)(artificial intelligence|\bA\.?I\.?\b|AI-powered|AI-driven|AI washing|machine learning|chatbot|generative|large language model|deepfake|algorithm(ic)? (pricing|targeting|decision))`)

// An action, as opposed to a meeting notice or a request for comment: the
// title says someone was charged, settled, ordered, fined or sued. Litigation
// releases are actions by definition.
var enforcementActionRe = regexp.MustCompile(`(?i)(charges?|charged|settl|order|fine[sd]?|penalt|complaint|sues|sued|finaliz|action against|bars?|barred|judgment|halts|refund|ban)`)

type enforcementFeed struct {
	agency, url string
	fetchPage   bool
}

var enforcementFeeds = []enforcementFeed{
	{"FTC", "https://www.ftc.gov/feeds/press-release-consumer-protection.xml", false},
	{"FTC", "https://www.ftc.gov/feeds/press-release.xml", false},
	{"SEC", "https://www.sec.gov/enforcement-litigation/litigation-releases/rss", true},
	{"SEC", "https://www.sec.gov/news/pressreleases.rss", true},
}

var enfTagRe = regexp.MustCompile(`<[^>]+>`)
var enfWsRe = regexp.MustCompile(`\s+`)

func enfPlain(s string) string {
	s = html.UnescapeString(s)
	s = enfTagRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(enfWsRe.ReplaceAllString(html.UnescapeString(s), " "))
}

func twoaiEnforcementWatch(db *sql.DB, today string) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_enforcement_actions (
		url text PRIMARY KEY, agency text NOT NULL, title text NOT NULL,
		published_on date, summary text, ai_terms text, is_ai boolean NOT NULL DEFAULT false, kind text,
		first_seen timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	client := &http.Client{Timeout: 25 * time.Second}
	// sec.gov requires a descriptive agent with contact details.
	ua := "SRJ Consulting & Services theworldofai.org srj@srjconsultingservices.com"
	get := func(u string) ([]byte, error) {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", ua)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	}
	seen, kept := 0, 0
	for _, f := range enforcementFeeds {
		body, err := get(f.url)
		if err != nil {
			fmt.Println("twoai_enforcement_watch:", f.url, err)
			continue
		}
		var feed struct {
			Items []struct {
				Title string `xml:"title"`
				Link  string `xml:"link"`
				Desc  string `xml:"description"`
				Pub   string `xml:"pubDate"`
			} `xml:"channel>item"`
		}
		dec := xml.NewDecoder(bytes.NewReader(body))
		dec.Strict = false
		dec.Entity = xml.HTMLEntity
		if err := dec.Decode(&feed); err != nil {
			fmt.Println("twoai_enforcement_watch:", f.url, "parse:", err)
			continue
		}
		for _, it := range feed.Items {
			link := strings.TrimSpace(it.Link)
			if link == "" {
				continue
			}
			var exists bool
			if db.QueryRow(`SELECT EXISTS(SELECT 1 FROM twoai_enforcement_actions WHERE url=$1)`, link).Scan(&exists); exists {
				continue
			}
			seen++
			title := enfPlain(it.Title)
			text := enfPlain(it.Desc)
			if f.fetchPage {
				if pb, perr := get(link); perr == nil {
					// The release body only: sec.gov's footer links to its own
					// Artificial Intelligence page, so the whole page matched every
					// release. Body starts at the field--name-body block and ends
					// before the page chrome.
					page := string(pb)
					if i := strings.Index(page, "field--name-body"); i >= 0 {
						page = page[i:]
					} else if i := strings.Index(page, "<main"); i >= 0 {
						page = page[i:]
					}
					text = enfPlain(page)
					for _, stop := range []string{"Return to top", "SEC homepage", "Related Materials"} {
						if k := strings.Index(text, stop); k > 0 {
							text = text[:k]
						}
					}
				}
				time.Sleep(300 * time.Millisecond)
			}
			terms := uniqueLower(enforcementAIRe.FindAllString(title+" "+text, -1))
			isAI := len(terms) > 0
			kind := "policy"
			if strings.Contains(f.url, "litigation-releases") || enforcementActionRe.MatchString(title) {
				kind = "enforcement"
			}
			pub := ""
			for _, layout := range []string{time.RFC1123Z, time.RFC1123, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 02 Jan 2006 15:04:05 MST"} {
				if t, perr := time.Parse(layout, strings.TrimSpace(it.Pub)); perr == nil {
					pub = t.Format("2006-01-02")
					break
				}
			}
			summary := trunc(text, 600)
			if _, err := db.Exec(`INSERT INTO twoai_enforcement_actions (url, agency, title, published_on, summary, ai_terms, is_ai, kind)
				VALUES ($1,$2,$3,NULLIF($4,'')::date,$5,NULLIF($6,''),$7,$8) ON CONFLICT (url) DO NOTHING`,
				link, f.agency, title, pub, summary, strings.Join(terms, ", "), isAI, kind); err != nil {
				return err
			}
			if isAI {
				kept++
				fmt.Printf("twoai_enforcement_watch: %s %s [%s]\n", f.agency, trunc(title, 90), strings.Join(terms, ", "))
			}
		}
	}
	return twoaiEnforcementPage(db, today, seen, kept)
}

func twoaiEnforcementPage(db *sql.DB, today string, seen, kept int) error {
	db.Exec(`ALTER TABLE twoai_enforcement_actions ADD COLUMN IF NOT EXISTS kind text`)
	var b strings.Builder
	n := 0
	b.WriteString(`<p>AI enforcement actions by the Federal Trade Commission and the Securities and Exchange Commission, read daily from the agencies' own press release and litigation release feeds and kept when the release names artificial intelligence. Each entry links to the agency's release; the text under it is the release's own opening, not our summary. For how each agency approaches AI, see the <a href="/ai-compliance/ftc-ai-enforcement/">FTC</a> and <a href="/ai-compliance/sec-ai-enforcement/">SEC</a> enforcement pages; for the other agencies, <a href="/ai-compliance/agency-enforcement/">agency enforcement</a>.</p>`)
	for _, sec := range []struct{ kind, heading string }{{"enforcement", "Enforcement actions"}, {"policy", "Policy statements, guidance and requests for comment"}} {
		rows, err := db.Query(`SELECT agency, title, url, COALESCE(published_on::text,''), COALESCE(summary,'')
			FROM twoai_enforcement_actions WHERE is_ai AND COALESCE(kind,'enforcement') = $1
			ORDER BY published_on DESC NULLS LAST, first_seen DESC`, sec.kind)
		if err != nil {
			return err
		}
		var tb strings.Builder
		m := 0
		for rows.Next() {
			var ag, t, u, d, s string
			if rows.Scan(&ag, &t, &u, &d, &s) != nil {
				continue
			}
			m++
			fmt.Fprintf(&tb, `<tr><td>%s</td><td>%s</td><td><a href="%s" rel="noopener">%s</a><br><small>%s</small></td></tr>`,
				html.EscapeString(d), html.EscapeString(ag), html.EscapeString(u), html.EscapeString(t), html.EscapeString(trunc(s, 300)))
		}
		rows.Close()
		if m == 0 {
			continue
		}
		if sec.kind == "enforcement" {
			n = m
		}
		fmt.Fprintf(&b, `<h2>%s</h2><table class="srjgov-table"><thead><tr><th>Date</th><th>Agency</th><th>Release</th></tr></thead><tbody>%s</tbody></table>`, sec.heading, tb.String())
	}
	doc := map[string]any{
		"slug": "ai-enforcement-actions", "title": "AI Enforcement Actions", "subtitle": "FTC and SEC actions that name artificial intelligence, from the agencies' own releases",
		"parent":    "agency-enforcement",
		"short":     fmt.Sprintf("%d FTC and SEC enforcement actions naming artificial intelligence, newest first, each linked to the agency's release and updated daily.", n),
		"seo_title": "AI Enforcement Actions: FTC and SEC, Updated Daily", "meta_description": "FTC and SEC enforcement actions involving artificial intelligence, from AI washing to deceptive AI claims, read daily from the agencies' own releases.",
		"focus_keyword": "AI enforcement actions", "body_html": b.String(), "generated": today, "built_at": time.Now().Format(time.RFC3339),
		"uid": twoaiUID("section:ai-enforcement-actions"), "page_uid": twoaiUID("section:ai-enforcement-actions"), "verified": today, "refresh_every_days": 1,
	}
	raw, _ := json.Marshal(doc)
	if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, url_count, updated_at)
		VALUES ('compliance/ai-enforcement-actions.json', 'compliance', 'agency-enforcement', $1::jsonb, 1, now())
		ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, updated_at = now()
		WHERE twoai_pages.data::text IS DISTINCT FROM EXCLUDED.data::text`, string(raw)); err != nil {
		return err
	}
	fmt.Printf("twoai_enforcement_watch: new_items=%d new_ai=%d listed=%d ok=true\n", seen, kept, n)
	return nil
}
