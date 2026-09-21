package main

// twoai_politics_pages: the page documents for The Politics of AI hub,
// rebuilt from the politics tables on every run.
//
// WHAT IS PUBLISHED, 2026-09-21. The hub (d9480073) and one section, Lobbying
// on AI (f97534ef), both as drafts: reachable at their permanent URLs for
// Stephen to review, carrying noindex, and kept out of the sitemap by the
// postbuild step that reads the built HTML. Publishing is setting draft to
// false here, not a new URL. The other sections stay in the taxonomy with no
// page until their data can carry one; the hub lists only sections that have
// a page, so it never links to an address that does not load.
//
// SHAPE. industries/<slug>.json with shape tech-section and is_hub true, the
// document the Enterprise route already renders for the AI Insurance hub:
// name, summary, blurb, a Sections list from children, and points, each a
// name, a description and a source link. No template change.
//
// WHAT A POINT SAYS. Only what the filings say: client as filed, filing count,
// the firms that filed for it, bills named in its AI activity text, agencies
// contacted, the latest posting date, and the latest filing as the source.
// Nothing about why. The page says so above the list.
//
// Rebuilt every run from SQL, so the counts on the page are the counts in the
// table the day it was built, and deleting these rows and rerunning restores
// them exactly.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	polHubUID      = "d9480073"
	polLobbyUID    = "f97534ef"
	polCategory    = "enterprise-applications-governance-and-tools"
	polBase        = "/ai-ecosystem/enterprise-applications-governance-and-tools/"
	polLobbyPoints = 150
)

func twoaiPoliticsPages(db *sql.DB) error {
	today := time.Now().UTC().Format("2006-01-02")

	if n, err := polProposeClientMatches(db); err != nil {
		fmt.Println("twoai_politics_pages: client matching:", err)
	} else if n > 0 {
		fmt.Printf("twoai_politics_pages: %d new client to company matches proposed\n", n)
	}
	confirmed := map[int64]string{} // client_id -> page href; a company page wins over an operator page
	if r, err := db.Query(`SELECT client_id, company_uid, target_kind FROM twoai_pol_client_matches
		WHERE status = 'confirmed' ORDER BY (target_kind = 'company')`); err == nil {
		for r.Next() {
			var id int64
			var uid, kind string
			if r.Scan(&id, &uid, &kind) == nil {
				if kind == "dc_operator" {
					confirmed[id] = "/ai-ecosystem/technology-and-core-infrastructure/" + uid + "/"
				} else {
					confirmed[id] = "/companies/" + uid + "/"
				}
			}
		}
		r.Close()
	}

	// Downloads, digest and member timelines first, so the hub below can list
	// the member pages that exist this run.
	if err := twoaiPoliticsExports(db, today); err != nil {
		fmt.Println("twoai_politics_exports:", err)
	}
	// Bills, firms and lobbyists as pages, cross referenced with members,
	// companies and AI people.
	if err := twoaiPoliticsDirectory(db, today); err != nil {
		fmt.Println("twoai_politics_directory:", err)
	}
	type memberPoint struct {
		Name   string `json:"name"`
		Desc   string `json:"desc"`
		Source string `json:"source"`
	}
	var memberPts []memberPoint
	if r, err := db.Query(`SELECT data->>'uid', data->>'name', (data->>'total')::int FROM twoai_pages
		WHERE path LIKE 'industries/pol-member-%' ORDER BY (data->>'total')::int DESC, data->>'name' LIMIT 300`); err == nil {
		for r.Next() {
			var uid, name string
			var n int
			if r.Scan(&uid, &name, &n) == nil {
				memberPts = append(memberPts, memberPoint{Name: strings.TrimSuffix(name, ": AI record"),
					Desc: fmt.Sprintf("%d dated AI events: bills, votes and committee money, each with its source. uid %s.", n, uid),
					Source: polBase + uid + "/"})
			}
		}
		r.Close()
	}
	var filings, clients, registrants int
	var first, last sql.NullTime
	if err := db.QueryRow(`SELECT count(*), count(DISTINCT client_id), count(DISTINCT registrant_id), min(dt_posted), max(dt_posted)
		FROM twoai_pol_lobbying WHERE retired_reason IS NULL`).Scan(&filings, &clients, &registrants, &first, &last); err != nil {
		return err
	}
	if filings == 0 {
		fmt.Println("twoai_politics_pages: no lobbying filings held, nothing built")
		return nil
	}

	rows, err := db.Query(`
		WITH c AS (
			SELECT client_id,
			       (array_agg(client_name ORDER BY dt_posted DESC))[1] AS client_name,
			       count(*) AS n,
			       max(dt_posted) AS latest,
			       (array_agg(document_url ORDER BY dt_posted DESC))[1] AS latest_url,
			       (array_agg(uid ORDER BY dt_posted DESC))[1] AS latest_uid,
			       array_agg(DISTINCT registrant_name) FILTER (WHERE registrant_name IS NOT NULL AND registrant_name <> '') AS regs
			FROM twoai_pol_lobbying WHERE retired_reason IS NULL
			GROUP BY client_id)
		SELECT c.client_id, c.client_name, c.n, c.latest, c.latest_url, c.latest_uid, COALESCE(c.regs, '{}'),
		       COALESCE((SELECT array_agg(DISTINCT b) FROM twoai_pol_lobbying l, unnest(l.bill_refs) b
		                 WHERE l.client_id = c.client_id AND l.retired_reason IS NULL), '{}'),
		       COALESCE((SELECT array_agg(DISTINCT e) FROM twoai_pol_lobbying l, unnest(l.government_entities) e
		                 WHERE l.client_id = c.client_id AND l.retired_reason IS NULL), '{}')
		FROM c WHERE c.latest IS NOT NULL ORDER BY c.n DESC, c.latest DESC LIMIT $1`, polLobbyPoints)
	if err != nil {
		return err
	}
	type point struct {
		Name   string `json:"name"`
		Desc   string `json:"desc"`
		Source string `json:"source"`
	}
	var points []point
	list := func(a []string, n int) string {
		out := []string{}
		for _, s := range a {
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		more := ""
		if len(out) > n {
			more = fmt.Sprintf(" and %d more", len(out)-n)
			out = out[:n]
		}
		return strings.Join(out, ", ") + more
	}
	for rows.Next() {
		var id int64
		var name, url, uid string
		var n int
		var latest time.Time
		var regs, bills, ents []string
		if err := rows.Scan(&id, &name, &n, &latest, &url, &uid, pq.Array(&regs), pq.Array(&bills), pq.Array(&ents)); err != nil {
			continue
		}
		word := "filings"
		if n == 1 {
			word = "filing"
		}
		d := fmt.Sprintf("%d %s reporting AI lobbying, latest posted %s.", n, word, latest.UTC().Format("2006-01-02 15:04 UTC"))
		if len(regs) > 0 {
			d += " Filed by " + list(regs, 3) + "."
		}
		if len(bills) > 0 {
			d += " Bills named: " + list(bills, 8) + "."
		}
		if len(ents) > 0 {
			d += " Contacted: " + strings.ToLower(list(ents, 4)) + "."
		}
		d += fmt.Sprintf(" LDA client id %d, latest filing uid %s.", id, uid)
		// A client confirmed as a company this site profiles links to that
		// company's page, which lists every filing with its lda.gov link. An
		// unconfirmed client links to its latest filing directly.
		src := url
		if h, ok := confirmed[id]; ok {
			src = h
		}
		points = append(points, point{Name: name, Desc: d, Source: src})
	}
	rows.Close()

	span := ""
	if first.Valid && last.Valid {
		span = fmt.Sprintf(" posted between %s and %s", first.Time.UTC().Format("2006-01-02"), last.Time.UTC().Format("2006-01-02"))
	}
	lobbySummary := fmt.Sprintf("%d Lobbying Disclosure Act filings%s report lobbying the federal government on artificial intelligence or machine learning, for %d clients through %d registrants. The %d clients with the most filings are listed below, each linked to its latest filing on lda.gov.",
		filings, span, clients, registrants, len(points))
	lobbyBlurb := "Each entry is what the filings themselves report: the client as it is named in the filing, the lobbying firms or in-house teams that filed for it, the bill numbers written in the activity description, and the parts of government contacted. A filing records that lobbying happened and on what issue. It does not record the position taken, and this page does not infer one.\n\n" +
		"Filings are read daily from lda.gov, the official Lobbying Disclosure Act database. An activity counts only when its own description is about AI, so a filing that mentions AI in one activity and trade policy in another is counted once, for the AI activity. Amended filings are kept beside the originals."

	lobby := map[string]any{
		"tax": "pol-lobbying", "uid": polLobbyUID, "page_uid": polLobbyUID, "slug": "pol-lobbying",
		"name": "Lobbying on AI", "shape": "tech-section", "is_hub": true, "draft": true,
		"parent_name": "The Politics of AI", "parent_href": polBase + polHubUID + "/",
		"summary": lobbySummary, "blurb": lobbyBlurb, "points": points, "children": []any{},
		"total": filings, "generated": today, "verified": today, "built_at": time.Now().Format(time.RFC3339),
		"refresh_every_days": 1, "category": polCategory,
	}

	hubSummary := "Who in government is doing what about AI, shown through the public record: lobbying filed on AI, the bills and votes in Congress, the money from AI company committees, and the federal effort to override state AI laws. Every entry cites the filing, vote or record it rests on."
	hubBlurb := "This hub records what can be checked and leaves motive to the reader. A lobbying filing shows that lobbying happened on an issue, not what was argued. A contribution shown beside a vote is placed there for comparison, not as a claim that one caused the other.\n\n" +
		"Lobbying on AI is the first section. Below it, each member of Congress with an AI bill, a recorded AI vote or money from an AI company or AI-focused committee has a timeline page listing every dated event with its source. Money beside votes by issue and the preemption watch follow as the Congress and Federal Election Commission data finish loading. Reporters will find every dataset in the press room."
	hub := map[string]any{
		"tax": "politics-of-ai", "uid": polHubUID, "page_uid": polHubUID, "slug": "politics-of-ai",
		"name": "The Politics of AI", "shape": "tech-section", "is_hub": true, "draft": true,
	"summary": hubSummary, "blurb": hubBlurb, "points": memberPts,
		"children": []map[string]any{
			{"name": "Lobbying on AI", "href": polBase + polLobbyUID + "/", "desc": lobbySummary, "sort": 1},
			{"name": "AI Bills in Congress", "href": polBase + polBillsUID + "/", "desc": "Every AI bill in the current Congress with its sponsors, recorded votes and the lobbying filings that name it.", "sort": 2},
			{"name": "Lobbying Firms on AI", "href": polBase + polFirmsUID + "/", "desc": "Every firm and in-house team filing AI lobbying reports, with its clients, lobbyists and the bills named.", "sort": 3},
			{"name": "Lobbyists on AI", "href": polBase + polLobbyistsUID + "/", "desc": "Registered lobbyists named on AI filings, with their firms, clients, bills and disclosed prior government positions.", "sort": 4},
			{"name": "Press room and data downloads", "href": "/press/", "desc": "Every dataset behind this hub as JSON and CSV, a daily digest feed, and how the data is collected and cited.", "sort": 5},
		},
		"total": 1, "generated": today, "verified": today, "built_at": time.Now().Format(time.RFC3339),
		"refresh_every_days": 1, "category": polCategory,
	}

	for path, doc := range map[string]map[string]any{
		"industries/politics-of-ai.json": hub,
		"industries/pol-lobbying.json":   lobby,
	} {
		b, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, updated_at)
			VALUES ($1, 'tech-section', $2, $3, now())
			ON CONFLICT (path) DO UPDATE SET kind = EXCLUDED.kind, taxonomy_slug = EXCLUDED.taxonomy_slug,
				data = EXCLUDED.data, updated_at = now()`, path, doc["tax"], ldaRaw(b)); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	db.Exec(`UPDATE twoai_taxonomy SET live_path = $2, updated_at = now() WHERE slug = $1 AND live_path IS NULL`, "politics-of-ai", polBase+polHubUID+"/")
	db.Exec(`UPDATE twoai_taxonomy SET live_path = $2, updated_at = now() WHERE slug = $1 AND live_path IS NULL`, "pol-lobbying", polBase+polLobbyUID+"/")

	fmt.Printf("twoai_politics_pages: hub %s and lobbying %s written as drafts, %d filings, %d clients, %d points ok=true\n",
		polHubUID, polLobbyUID, filings, clients, len(points))
	return nil
}
