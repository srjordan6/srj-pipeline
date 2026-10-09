package main

// THIS WEEK IN AI, THE NEWS WEEK (Stephen, 2026-10-09).
//
// "Everything moves from the daily headline to the weekly headline and then
// it is archived." /this-week-in-ai/ used to be the weekly policy digest
// (bills, the Federal Register, the courts). That digest moved to This Week
// in AI Laws and Compliance, a hub under AI Compliance, Law and Regulation
// (4d0aabb4), with its week pages at /this-week-in-ai-laws/{week}/. The old
// address now holds the week's news, drawn from the three daily streams:
// the headline stories (/ai-news/daily/), vendor news (/ai-news/vendor/) and
// When AI Goes Wrong (the incidents on the daily page).
//
// WHAT MAKES THE WEEK. Stephen: "choose the headlines that are getting the
// most traffic or are the most important." Traffic is Google Analytics page
// views of the item's own page (twoai_ga_pages), summed since it was
// published; importance is how widely it was covered (outlets for a story
// or an incident) and, for vendor news, whether the vendor has a company
// page here. A view is weighted as five outlets, so a story readers actually
// opened rises above one that was merely syndicated, and with no traffic the
// widest coverage leads. Each week keeps its twelve top stories, ten vendor
// posts and six incidents; nothing is deleted from the daily archive.
//
// ONE FILE PER WEEK, FOREVER. A week's file is rewritten while it is current
// and for a week after (late traffic still counts), then left alone, so the
// archive is stable: newsweek/{yyyy-wNN}.json, and newsweek/index.json for
// the hub.

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var newsWeekSentRe = regexp.MustCompile(`([.!?])\s+([A-Z"\x{201C}(])`)

// newsWeekTeaser is the first two sentences of a summary, for the list.
func newsWeekTeaser(s string) string {
	t := strings.Join(strings.Fields(s), " ")
	if t == "" {
		return ""
	}
	t = newsWeekSentRe.ReplaceAllString(t, "$1\x00$2")
	parts := strings.Split(t, "\x00")
	if len(parts) > 2 {
		parts = parts[:2]
	}
	out := strings.Join(parts, " ")
	if len([]rune(out)) > 420 {
		r := []rune(out)[:420]
		out = strings.TrimSpace(string(r))
		if i := strings.LastIndex(out, " "); i > 200 {
			out = out[:i]
		}
		out += "…"
	}
	return out
}

type newsWeekItem struct {
	Title   string `json:"title"`
	Href    string `json:"href"`
	Date    string `json:"date"`
	Teaser  string `json:"teaser,omitempty"`
	Source  string `json:"source,omitempty"`
	Outlets int    `json:"outlets,omitempty"`
	Views   int    `json:"views,omitempty"`
	UID     string `json:"uid,omitempty"`
	score   int
}

func twoaiNewsWeeks(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	now := time.Now().UTC()
	offset := (int(now.Weekday()) + 6) % 7
	thisMonday := time.Date(now.Year(), now.Month(), now.Day()-offset, 0, 0, 0, 0, time.UTC)
	first := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC) // the first full week of the story archive

	views := map[string]int{}
	if rows, err := db.Query(`SELECT path, sum(views)::int FROM twoai_ga_pages
		WHERE path LIKE '/ai-news/%' GROUP BY path`); err == nil {
		for rows.Next() {
			var p string
			var v int
			if rows.Scan(&p, &v) == nil {
				views[p] = v
			}
		}
		rows.Close()
	}
	// Weeks already archived are not rebuilt: a file exists and the week
	// ended more than seven days ago.
	have := map[string]bool{}
	if rows, err := db.Query(`SELECT path FROM twoai_pages WHERE path LIKE 'newsweek/%'`); err == nil {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				have[p] = true
			}
		}
		rows.Close()
	}

	var idx []map[string]any
	built := 0
	for start := thisMonday; !start.Before(first); start = start.AddDate(0, 0, -7) {
		end := start.AddDate(0, 0, 6)
		iso, wk := start.ISOWeek()
		slug := fmt.Sprintf("%d-w%02d", iso, wk)
		label := fmt.Sprintf("Week %d, %d", wk, iso)
		s, e := start.Format("2006-01-02"), end.Format("2006-01-02")
		path := "newsweek/" + slug + ".json"
		frozen := have[path] && now.Sub(end) > 8*24*time.Hour

		// Stories: the archive, live ones only.
		var stories []newsWeekItem
		if rows, err := db.Query(`SELECT uid, headline, published_on::text, COALESCE((story->>'DomainCount')::int, 0),
				COALESCE(story->>'Summary',''), COALESCE(story->>'Answer','')
			FROM twoai_news_stories WHERE retired_at IS NULL AND uid IS NOT NULL
			AND published_on BETWEEN $1::date AND $2::date`, s, e); err == nil {
			for rows.Next() {
				var it newsWeekItem
				var sum, ans string
				if rows.Scan(&it.UID, &it.Title, &it.Date, &it.Outlets, &sum, &ans) != nil {
					continue
				}
				it.Href = "/ai-news/" + it.UID + "/"
				it.Views = views[it.Href]
				it.Teaser = newsWeekTeaser(cleanSummaryText(sum))
				if it.Teaser == "" {
					it.Teaser = newsWeekTeaser(ans)
				}
				it.score = it.Views*5 + it.Outlets
				stories = append(stories, it)
			}
			rows.Close()
		}
		// Vendor news: what the companies published themselves.
		var vendor []newsWeekItem
		if rows, err := db.Query(`SELECT slug, title, posted_on::text, vendor, COALESCE(summary,''), entity_uid IS NOT NULL
			FROM twoai_vendor_posts WHERE retired_at IS NULL AND posted_on BETWEEN $1::date AND $2::date`, s, e); err == nil {
			for rows.Next() {
				var it newsWeekItem
				var slugV, sum string
				var hasCo bool
				if rows.Scan(&slugV, &it.Title, &it.Date, &it.Source, &sum, &hasCo) != nil {
					continue
				}
				it.Href = "/ai-news/vendor/" + slugV + "/"
				it.Views = views[it.Href]
				it.Teaser = newsWeekTeaser(vendorTagStrip(sum))
				it.score = it.Views * 5
				if hasCo {
					it.score += 2
				}
				if it.Teaser != "" {
					it.score++
				}
				vendor = append(vendor, it)
			}
			rows.Close()
		}
		// When AI goes wrong: the incident pages built from the AI Incident
		// Database, by the date of their first report.
		var incidents []newsWeekItem
		if rows, err := db.Query(`SELECT data->>'incident_id', COALESCE(data->>'title',''), COALESCE(data->>'published',''),
				COALESCE((data->>'outlet_count')::int, 1), COALESCE(data->>'summary','')
			FROM twoai_pages WHERE path LIKE 'news/incident-%'
			AND COALESCE(data->>'published','') BETWEEN $1 AND $2`, s, e); err == nil {
			for rows.Next() {
				var it newsWeekItem
				var id, sum string
				if rows.Scan(&id, &it.Title, &it.Date, &it.Outlets, &sum) != nil || id == "" {
					continue
				}
				it.Href = "/ai-news/incident/" + id + "/"
				it.UID = id
				it.Views = views[it.Href]
				it.Teaser = newsWeekTeaser(sum)
				it.score = it.Views*5 + it.Outlets
				incidents = append(incidents, it)
			}
			rows.Close()
		}
		top := func(items []newsWeekItem, n int) []newsWeekItem {
			for i := 1; i < len(items); i++ {
				for j := i; j > 0; j-- {
					a, b := items[j-1], items[j]
					if b.score > a.score || (b.score == a.score && b.Date > a.Date) {
						items[j-1], items[j] = b, a
					} else {
						break
					}
				}
			}
			if len(items) > n {
				items = items[:n]
			}
			if items == nil {
				items = []newsWeekItem{}
			}
			return items
		}
		nStories, nVendor, nInc := len(stories), len(vendor), len(incidents)
		stories, vendor, incidents = top(stories, 12), top(vendor, 10), top(incidents, 6)
		if nStories+nVendor+nInc == 0 {
			continue
		}
		lead := ""
		if len(stories) > 0 {
			lead = stories[0].Title
		}
		idx = append(idx, map[string]any{
			"slug": slug, "label": label, "start": s, "end": e, "lead": lead,
			"stories": nStories, "vendor": nVendor, "incidents": nInc,
			"current": start.Equal(thisMonday),
		})
		if frozen {
			continue
		}
		if err := upsert(path, "newsweek", map[string]any{
			"slug": slug, "label": label, "start": s, "end": e, "generated": today,
			"current": start.Equal(thisMonday),
			"counts":  map[string]int{"stories": nStories, "vendor": nVendor, "incidents": nInc},
			"stories": stories, "vendor": vendor, "incidents": incidents,
		}); err != nil {
			return built, err
		}
		built++
	}
	if len(idx) > 0 {
		if err := upsert("newsweek/index.json", "newsweek-hub", map[string]any{
			"weeks": idx, "latest": idx[0]["slug"], "generated": today,
		}); err != nil {
			return built, err
		}
	}
	fmt.Printf("twoai_news_weeks: %d weeks listed, %d written\n", len(idx), built)
	return built, nil
}

var vendorTagStripRe = regexp.MustCompile(`<[^>]+>`)

func vendorTagStrip(s string) string { return vendorTagStripRe.ReplaceAllString(s, " ") }

// cleanSummaryText drops a model's refusal or a markdown heading line from a
// stored summary before it is cut into a teaser.
func cleanSummaryText(s string) string {
	if isRefusal(s) {
		return ""
	}
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// twoaiLawWeekHub is the page document for This Week in AI Laws and
// Compliance, the hub of the weekly policy digest, a child of AI Compliance,
// Law and Regulation. The week files themselves are week/*.json, written by
// twoaiWeeks; the site renders this hub from them.
func twoaiLawWeekHub(db *sql.DB, today string, upsert func(path, kind string, v any) error) error {
	uid := twoaiUID("section:this-week-in-ai-laws")
	blurb := "The week in AI law and compliance: state bills that changed status, Federal Register documents, and AI lawsuits with new docket activity, grouped by subject and computed from the record every day."
	// The path is composed in SQL from the uid, so no argument is built by
	// string concatenation (Semgrep's gosql rule, row 585).
	if _, err := db.Exec(`INSERT INTO twoai_taxonomy (slug, name, parent_slug, level, sort, blurb, status, live_path, created_at, updated_at, line, line_auto)
		VALUES ('this-week-in-ai-laws', 'This Week in AI Laws and Compliance', 'law-and-compliance', 3, 0, $1, 'live',
			'/ai-ecosystem/enterprise-applications-governance-and-tools/' || $2::text || '/', now(), now(), $3, false)
		ON CONFLICT (slug) DO UPDATE SET live_path=EXCLUDED.live_path, status='live', updated_at=now()`,
		blurb, uid, "Each week of AI bills, federal rules and lawsuits, grouped by subject, with every subject linked to its bills."); err != nil {
		return err
	}
	return upsert("industries/this-week-ai-laws.json", "law-week-hub", map[string]any{
		"uid": uid, "page_uid": uid, "shape": "law-week-hub", "tax": "this-week-in-ai-laws",
		"name": "This Week in AI Laws and Compliance", "blurb": blurb, "generated": today,
		"parent_uid": "4d0aabb4", "refresh_every_days": 1,
	})
}
