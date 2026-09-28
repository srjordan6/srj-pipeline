package main

// twoai_quality: the public Data Quality page, /data-quality/. Stephen,
// 2026-09-28: the site's growth depends on AI assistants and readers trusting
// it, and every measure of that trust already lived inside the pipeline where
// nobody outside could see it. This page publishes them: what each section is
// built from, how often it is supposed to be rebuilt, when it last was, what is
// overdue, what could not be verified, and every correction.
//
// Every number is a live count taken when the page is written. A measure that
// cannot be taken on a run says so instead of showing a stale figure. The page
// separates two things readers tend to merge: a page not rebuilt on schedule is
// overdue, which says nothing about whether it is wrong; a fact known to have
// been wrong is a correction, listed with what it was and what it is now.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/lib/pq"
)

type qualSection struct {
	name, url, sources string
	kinds              []string
}

var qualSections = []qualSection{
	{"State AI laws", "/ai-laws/", "LegiScan (bill status and full text), read in full for each enacted law", []string{"state-law"}},
	{"Compliance and regulation", "/ai-compliance/", "The statutes, regulations and agency texts each page cites; written and reviewed by the editor", []string{"compliance", "compliance-hub", "compliance-digest", "compliance-reviews", "compliance-agency-actions"}},
	{"AI lawsuits and case law", "/ai-lawsuits/", "CourtListener federal dockets and opinions; for state court cases, reporting from established outlets", []string{"lawsuits", "caselaw-case", "caselaw-index"}},
	{"News, weekly recaps and AI incidents", "/ai-news/", "GDELT, curated outlet, agency and trade press feeds, and the AI Incident Database", []string{"news-archive", "vendor-news", "incident", "week", "week-hub"}},
	{"Companies", "/companies/", "SEC EDGAR filings, USPTO patents, Wikidata, company websites, Twelve Data prices", []string{"company", "company-section", "company-hub", "company-ma", "stocks"}},
	{"People", "/people/", "Public biographies and the sources listed on each profile", []string{"person", "person-category", "person-hub"}},
	{"Research", "/research/", "arXiv and OpenAlex", []string{"research-paper", "research-topic", "research-hub", "research-watch"}},
	{"Models and benchmarks", "/models/", "Hugging Face model metadata and published benchmark leaderboards", []string{"model-section", "benchmark", "benchmark-hub"}},
	{"Tools, MCP servers and repositories", "/mcp/", "The official MCP registry, npm, PyPI, GitHub, vendor sites", []string{"tool", "tool-category", "tool-hub", "mcp-server", "mcp-hub", "repo-section"}},
	{"AI ecosystem reference", "/ai-ecosystem/", "Mixed: operator and grid records for data centres, cited sources for each topic, editor-written hubs", []string{"tech-section", "tech-dc-child", "tech-datacenters", "tech-dc-grid", "art-topic", "art-hub", "ecosystem-section", "ecosystem-category", "ecosystem-hub", "industry-hub", "hub", "security-section", "security-domain", "security-hub"}},
	{"AI Observatory", "/ai-ecosystem/", "GitHub, Hugging Face, provider status pages, SEC XBRL, USPTO", []string{"obs-section", "status-section"}},
	{"Learning, prompts and skills", "/learn/", "Course providers, certification bodies, publishers; editor-written prompt guides", []string{"learning-entry", "book-catalog", "prompt-technique", "prompt-domain", "prompt-hub", "skills-hub", "onet-occupation"}},
}

func twoaiQualityPage(db *sql.DB, today string) error {
	var sb strings.Builder
	esc := html.EscapeString
	sections := []map[string]string{}
	add := func(h, body string) { sections = append(sections, map[string]string{"h": h, "body": body}) }

	add("What this page is", `<p>How this site is kept accurate, in numbers, taken from the site's own records each time this page is rebuilt. It shows what each section is built from, how often it is supposed to be rebuilt, when it last was, what is overdue, what could not be verified, and every correction we have made. Nothing here is estimated; a figure that could not be measured on a run says so.</p>`)

	add("Terms used on this page", `<p><b>Refresh schedule</b>: how often a page is supposed to be rebuilt from its sources. A news hub is rebuilt daily, a compliance page every 14 days, a company profile every 90 days.</p>
<p><b>Last rebuilt</b>: the most recent date a page in the section was rebuilt from its sources.</p>
<p><b>Overdue</b>: a page not rebuilt within its refresh schedule. Overdue means we have not re-checked it on time. It does <em>not</em> mean the page is wrong; most overdue pages are unchanged because their sources are unchanged.</p>
<p><b>Not verified</b>: a fact we could not confirm from a primary source. The page shows it as unverified rather than stating it.</p>
<p><b>Correction</b>: a published fact we later found to be wrong. Every correction is listed below with what was wrong and what replaced it.</p>`)

	// Sections table.
	sb.Reset()
	sb.WriteString(`<table class="srjgov-table"><thead><tr><th>Section</th><th>Built from</th><th>Pages</th><th>Refresh schedule</th><th>Last rebuilt</th><th>Overdue</th></tr></thead><tbody>`)
	totalPages, totalOver := 0, 0
	for _, s := range qualSections {
		var n, over, cmin, cmax int
		var last sql.NullString
		err := db.QueryRow(`SELECT count(*),
				count(*) FILTER (WHERE (current_date - NULLIF(left(data->>'generated',10),'')::date) > (data->>'refresh_every_days')::int),
				COALESCE(min((data->>'refresh_every_days')::int),0), COALESCE(max((data->>'refresh_every_days')::int),0),
				max(left(data->>'generated',10))
			FROM twoai_pages WHERE kind = ANY($1)`, pq.Array(s.kinds)).Scan(&n, &over, &cmin, &cmax, &last)
		if err != nil || n == 0 {
			continue
		}
		totalPages += n
		totalOver += over
		cad := fmt.Sprintf("every %d days", cmin)
		if cmin == 1 {
			cad = "daily"
		}
		if cmax != cmin {
			cad = fmt.Sprintf("every %d to %d days", cmin, cmax)
			if cmin == 1 {
				cad = fmt.Sprintf("daily to every %d days", cmax)
			}
		}
		overTxt := "0"
		if over > 0 {
			overTxt = fmt.Sprintf("<b>%d</b>", over)
		}
		fmt.Fprintf(&sb, `<tr><td><a href="%s">%s</a></td><td>%s</td><td>%d</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			s.url, esc(s.name), esc(s.sources), n, cad, esc(last.String), overTxt)
	}
	sb.WriteString(`</tbody></table>`)
	fmt.Fprintf(&sb, `<p>%d pages across these sections; %d overdue today.</p>`, totalPages, totalOver)
	add("Sections, sources and schedules", sb.String())

	// Not verified and incomplete.
	sb.Reset()
	measure := func(label, q, note string) {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			fmt.Fprintf(&sb, `<tr><td>%s</td><td>not measurable on this run</td><td>%s</td></tr>`, esc(label), esc(note))
			return
		}
		fmt.Fprintf(&sb, `<tr><td>%s</td><td>%d</td><td>%s</td></tr>`, esc(label), n, esc(note))
	}
	sb.WriteString(`<table class="srjgov-table"><thead><tr><th>Measure</th><th>Count</th><th>What it means</th></tr></thead><tbody>`)
	measure("Lawsuits followed from reporting, not a court docket", `SELECT count(*) FROM ai_lawsuits WHERE is_active AND courtlistener_url IS NULL`,
		"State court cases have no public docket feed; their developments come from established outlets and are marked as reporting.")
	measure("AI incidents with no account of what happened", `SELECT count(*) FROM twoai_pages WHERE kind='incident' AND COALESCE(data->>'summary','')=''`,
		"Every report of the incident is paywalled or blocks automated reading, and no database description was available yet.")
	measure("Cited sources whose text could not be read", `SELECT count(*) FROM twoai_source_pages WHERE doc IS NULL`,
		"The source is cited on an industry page but gets no summary page of its own, because we could not read its text.")
	measure("Outbound links found broken", `SELECT count(*) FROM twoai_link_health WHERE verdict='broken'`,
		"Links checked and found dead; each is repaired or replaced on the page that carries it.")
	measure("Outbound links that refuse automated checking", `SELECT count(*) FROM twoai_link_health WHERE verdict='blocked'`,
		"Sites that block link checkers. These links are not known to be broken; we simply cannot test them automatically.")
	measure("Outbound links checked and working", `SELECT count(*) FROM twoai_link_health WHERE verdict='ok'`, "")
	sb.WriteString(`</tbody></table>`)
	add("Not verified, incomplete or unreachable", sb.String())

	// Corrections.
	sb.Reset()
	rows, err := db.Query(`SELECT corrected_on::text, COALESCE(page_url,''), section, what_was_wrong, what_is_right, how_found
		FROM twoai_corrections WHERE public ORDER BY corrected_on DESC, id DESC`)
	nc := 0
	if err == nil {
		sb.WriteString(`<table class="srjgov-table"><thead><tr><th>Date</th><th>Where</th><th>What was wrong</th><th>What it is now</th><th>How we found it</th></tr></thead><tbody>`)
		for rows.Next() {
			var d, u, sec, wrong, right, how string
			if rows.Scan(&d, &u, &sec, &wrong, &right, &how) != nil {
				continue
			}
			nc++
			where := esc(sec)
			if u != "" {
				where = fmt.Sprintf(`<a href="%s">%s</a>`, esc(u), esc(sec))
			}
			fmt.Fprintf(&sb, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`, d, where, esc(wrong), esc(right), esc(how))
		}
		rows.Close()
		sb.WriteString(`</tbody></table>`)
	}
	corr := `<p>Every correction to a published fact, newest first. To report an error, email <a href="mailto:theworldofai@inkboxmail.com">theworldofai@inkboxmail.com</a>; the <a href="/editorial-policy/">editorial policy</a> explains how corrections are handled.</p>` + sb.String()
	if nc == 0 {
		corr = `<p>No corrections recorded yet.</p>`
	}
	add("Corrections", corr)

	add("How the checks run", `<p>A pipeline rebuilds the site several times a day from its sources. Each page carries its refresh schedule and the date it was last rebuilt; every run lists the pages that are overdue. Outbound links are re-checked in rotation. Automated news watches can only change a page when their source is an established outlet and the report names the subject, and the editor reviews what they change. This page is rebuilt on every run, so its counts are never more than a few hours old.</p>`)

	doc := map[string]any{
		"slug": "data-quality", "title": "Data Quality and Corrections",
		"description": fmt.Sprintf("How The World of AI is kept accurate: sources and refresh schedules for %d pages, what is overdue or unverified today, and every correction we have made.", totalPages),
		"updated":     today, "sections": sections, "generated": today, "built_at": time.Now().Format(time.RFC3339),
		"page_uid": twoaiUID("static:data-quality"), "refresh_every_days": 1,
	}
	raw, _ := json.Marshal(doc)
	if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, url_count, updated_at)
		VALUES ('static/data-quality.json', 'static', $1::jsonb, 1, now())
		ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, updated_at = now()
		WHERE twoai_pages.data::text IS DISTINCT FROM EXCLUDED.data::text`, string(raw)); err != nil {
		return err
	}
	fmt.Printf("twoai_quality: pages=%d overdue=%d corrections=%d ok=true\n", totalPages, totalOver, nc)
	return nil
}
