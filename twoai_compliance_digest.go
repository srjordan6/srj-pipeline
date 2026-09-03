package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// WHAT CHANGED, FROM THE DATA, NOT FROM A HAND.
//
// The compliance hub carried a "What changed" block that was written by hand
// on 2026-07-14 and inserted directly into twoai_pages on 2026-08-10. Nothing
// regenerated it. Stephen, 2026-09-03: this is not getting updated. Of course
// it was not; no stage owned it.
//
// The same three tables that drive the rest of the site already hold what
// changed: twoai_bill_events records every relevant state bill that reached
// a new status and when; twoai_agency_actions records federal agency AI
// actions as they publish; and site_content records when each framework
// explainer was last revised (sync_content leaves updated_at alone on a
// no-op, so a bumped date is a real edit). This stage reads the last ninety
// days of all three and writes the digest in the exact shape the template
// already renders - flag, headline, body, link - dated today. On a quiet
// month it says so instead of repeating July.
//
// Links go to the site's own pages: the state law page for a bill, the
// agency enforcement explainer for an agency action, the framework page for
// a revision. Titles come from the record; the site's own prose is the only
// prose here.
func twoaiComplianceDigest(db *sql.DB, today string, upsert func(path, kind string, v any) error) error {
	type item struct {
		Flag      string `json:"flag"`
		FlagClass string `json:"flag_class"`
		Headline  string `json:"headline"`
		Body      string `json:"body"`
		Link      string `json:"link"`
		LinkText  string `json:"link_text"`
		date      string
	}
	var items []item

	// Enacted and materially advanced state bills, newest first.
	rows, err := db.Query(`SELECT state, bill_number, COALESCE(status_label, status, ''), status_date::text,
			COALESCE(title,''), COALESCE(relevance_note,'')
		FROM twoai_bill_events
		WHERE relevant AND status_date > current_date - 90
		ORDER BY status_date DESC LIMIT 12`)
	if err == nil {
		for rows.Next() {
			var st, bill, status, date, title, note string
			if rows.Scan(&st, &bill, &status, &date, &title, &note) != nil {
				continue
			}
			flag, cls := "Enacted", "is-final"
			if !strings.Contains(strings.ToLower(status), "enact") && !strings.Contains(strings.ToLower(status), "sign") {
				flag, cls = "Advanced", "is-new"
			}
			body := strings.TrimSpace(note)
			if body == "" {
				body = fmt.Sprintf("%s %s reached %s on %s.", st, bill, strings.ToLower(status), date)
			}
			items = append(items, item{flag, cls, fmt.Sprintf("%s %s: %s", st, bill, strings.TrimSpace(title)), body,
				"/ai-laws/" + twoaiSlug(twoaiStates[strings.ToUpper(st)]) + "/", "Read the state page", date})
		}
		rows.Close()
	}

	// Federal agency actions, newest first.
	rows, err = db.Query(`SELECT agency, COALESCE(title,''), COALESCE(summary,''), published_on::text
		FROM twoai_agency_actions
		WHERE published_on > current_date - 90
		ORDER BY published_on DESC LIMIT 8`)
	if err == nil {
		for rows.Next() {
			var agency, title, summary, date string
			if rows.Scan(&agency, &title, &summary, &date) != nil {
				continue
			}
			body := strings.TrimSpace(summary)
			if len(body) > 280 {
				body = body[:277] + "..."
			}
			if body == "" {
				body = fmt.Sprintf("Published by %s on %s.", agency, date)
			}
			items = append(items, item{"Agency", "is-superseded", agency + ": " + strings.TrimSpace(title), body,
				"/ai-compliance/agency-enforcement/", "Read agency enforcement", date})
		}
		rows.Close()
	}

	// Framework explainers revised, newest first.
	rows, err = db.Query(`SELECT data->>'slug', COALESCE(data->>'title',''), COALESCE(data->>'short',''), updated_at::date::text
		FROM site_content
		WHERE path LIKE 'governance/%'
		  AND path NOT IN ('governance/_meta.json','governance/sources.json','governance/ai-tools.json')
		  AND updated_at > now() - interval '90 days'
		  AND data->>'slug' <> ''
		ORDER BY updated_at DESC LIMIT 10`)
	if err == nil {
		for rows.Next() {
			var slug, title, short, date string
			if rows.Scan(&slug, &title, &short, &date) != nil {
				continue
			}
			body := strings.TrimSpace(short)
			if body == "" {
				body = "This explainer was revised against its primary sources on " + date + "."
			}
			items = append(items, item{"Revised", "is-new", title, body, "/ai-compliance/" + slug + "/", "Read the framework", date})
		}
		rows.Close()
	}

	// Newest first across all three kinds, capped so the block stays a digest.
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].date > items[i].date {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
	if len(items) > 12 {
		items = items[:12]
	}

	intro := fmt.Sprintf("This library is reviewed against primary sources, not secondary summaries. %d changes in the last ninety days, from the state bill tracker, the federal agency watch and the framework explainers, newest first.", len(items))
	if len(items) == 0 {
		intro = "This library is reviewed against primary sources, not secondary summaries. No relevant state bill, federal agency action or framework revision was recorded in the last ninety days."
	}
	return upsert("compliance/what-changed.json", "compliance-digest", map[string]any{
		"reviewed": today, "intro": intro, "items": items, "generated": today,
	})
}
