package main

// twoai_state_law_pages: a page for every state that has enacted AI law.
//
// Stephen, 2026-09-17: is this all the states with AI laws? It was not. The
// state-ai-laws framework listed twelve children, five of them states with no
// enacted bill in the corpus, while twenty states WITH enacted law - New York
// with nine, Louisiana with eight, Hawaii with six - had no page at all. The
// list was written by hand in August and the legislatures kept going.
//
// This builds the list from the data. For every state with at least one
// relevant passed bill, a page compliance/<state>-ai-laws.json exists: its
// enacted laws in date order, each with the reading twoai_enacted_laws wrote
// from the statute and a link to that law's own page. A state whose page was
// written by hand keeps its prose and gains the list beneath it. The parent
// framework's children list is rebuilt from the same set, so the page reads
// the states that have law, all of them, and only them.
//
// Generated every run, so a state that passes its first AI law tonight has a
// page tomorrow with no one adding it.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"html"
	"strings"
	"time"
)

var stateNames = map[string]string{
	"AL": "Alabama", "AK": "Alaska", "AZ": "Arizona", "AR": "Arkansas", "CA": "California", "CO": "Colorado",
	"CT": "Connecticut", "DE": "Delaware", "DC": "District of Columbia", "FL": "Florida", "GA": "Georgia", "HI": "Hawaii",
	"ID": "Idaho", "IL": "Illinois", "IN": "Indiana", "IA": "Iowa", "KS": "Kansas", "KY": "Kentucky", "LA": "Louisiana",
	"ME": "Maine", "MD": "Maryland", "MA": "Massachusetts", "MI": "Michigan", "MN": "Minnesota", "MS": "Mississippi",
	"MO": "Missouri", "MT": "Montana", "NE": "Nebraska", "NV": "Nevada", "NH": "New Hampshire", "NJ": "New Jersey",
	"NM": "New Mexico", "NY": "New York", "NC": "North Carolina", "ND": "North Dakota", "OH": "Ohio", "OK": "Oklahoma",
	"OR": "Oregon", "PA": "Pennsylvania", "PR": "Puerto Rico", "RI": "Rhode Island", "SC": "South Carolina",
	"SD": "South Dakota", "TN": "Tennessee", "TX": "Texas", "UT": "Utah", "VT": "Vermont", "VA": "Virginia",
	"WA": "Washington", "WV": "West Virginia", "WI": "Wisconsin", "WY": "Wyoming", "US": "Federal",
}

// The hand-written pages that already cover a state, by state code. Their
// prose is kept; the enacted-law list is added beneath it. Anything else gets
// a generated page.
var stateHandPages = map[string]string{
	"CA": "california-ai-laws", "CO": "colorado-ai-act", "TX": "texas-ai-act", "UT": "utah-ai-policy-act",
	"IL": "illinois-ai-laws", "CT": "connecticut-ai-act", "TN": "tennessee-elvis-act", "WY": "wyoming-ai-laws",
	"MT": "montana-ai-laws", "NV": "nevada-ai-laws", "AR": "arkansas-ai-laws", "PR": "puerto-rico-ai-laws",
}

func twoaiStateLawPages(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT data->>'state', data->>'bill_number', data->>'title', data->>'status_date',
		       data->>'slug', COALESCE(data->'reading'->>'what_it_does',''), COALESCE(data->'reading'->>'effective_date','')
		FROM twoai_pages WHERE path LIKE 'compliance/law-%'
		ORDER BY data->>'state', data->>'status_date' DESC`)
	if err != nil {
		return err
	}
	type law struct{ state, number, title, date, slug, what, effective string }
	byState := map[string][]law{}
	for rows.Next() {
		var l law
		if rows.Scan(&l.state, &l.number, &l.title, &l.date, &l.slug, &l.what, &l.effective) == nil && l.state != "" {
			byState[l.state] = append(byState[l.state], l)
		}
	}
	rows.Close()

	today := time.Now().UTC().Format("2006-01-02")
	var children []string
	generated, augmented := 0, 0
	for st, laws := range byState {
		if st == "US" {
			continue
		}
		name := stateNames[st]
		if name == "" {
			name = st
		}
		// The list of enacted laws, the same block whether the page is
		// generated or hand-written.
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("<h2 id=\"enacted\">Enacted AI laws in %s</h2><p>%d %s this site judged relevant to AI, newest first, each read from its enrolled text.</p><ul class=\"law-list\">",
			html.EscapeString(name), len(laws), plural(len(laws), "law", "laws")))
		for _, l := range laws {
			title := strings.TrimPrefix(l.title, l.state+" "+l.number+": ")
			sb.WriteString("<li><a href=\"/ai-compliance/" + html.EscapeString(l.slug) + "/\"><b>" + html.EscapeString(l.number) + "</b>: " + html.EscapeString(title) + "</a>")
			if l.date != "" {
				sb.WriteString(" <span class=\"meta-line\">passed " + html.EscapeString(l.date) + "</span>")
			}
			if l.what != "" {
				sb.WriteString("<p>" + html.EscapeString(l.what) + "</p>")
			}
			sb.WriteString("</li>")
		}
		sb.WriteString("</ul>")
		block := sb.String()

		if slug, ok := stateHandPages[st]; ok {
			// Hand-written page: replace or append the generated block.
			var body string
			if err := db.QueryRow(`SELECT COALESCE(data->>'body_html','') FROM twoai_pages WHERE path=$1`, "compliance/"+slug+".json").Scan(&body); err == nil {
				if i := strings.Index(body, "<h2 id=\"enacted\">"); i >= 0 {
					body = body[:i]
				}
				db.Exec(`UPDATE twoai_pages SET data = data || jsonb_build_object('body_html', $2::text), updated_at=now() WHERE path=$1`,
					"compliance/"+slug+".json", body+block)
				augmented++
			}
			children = append(children, slug)
			continue
		}
		slug := strings.ToLower(strings.ReplaceAll(name, " ", "-")) + "-ai-laws"
		h8 := sha256.Sum256([]byte("compliance:" + slug))
		uid := hex.EncodeToString(h8[:4])
		summary := fmt.Sprintf("%s has enacted %d AI-related %s. Each is read from its enrolled text: what it does, who it applies to, when it takes effect, and what it requires.", name, len(laws), plural(len(laws), "law", "laws"))
		body := "<p class=\"answer\">" + html.EscapeString(summary) + "</p>" + block +
			"<h2>How this page is kept</h2><p>This page is generated from the enacted laws this site tracks for " + html.EscapeString(name) +
			". When the legislature passes another AI-related bill, it appears here the night the bill's text is read. The full 50-state picture is at <a href=\"/ai-compliance/state-ai-laws/\">State AI Laws</a>.</p>"
		if _, err := db.Exec(`
			INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, updated_at)
			VALUES ($1, 'compliance', 'state-ai-laws', jsonb_build_object(
				'uid', $2::text, 'page_uid', $2::text, 'slug', $3::text,
				'title', $4::text, 'parent', 'state-ai-laws', 'state', $5::text,
				'generated', $6::text, 'verified', $6::text,
				'summary', $7::text, 'short', $7::text, 'body_html', $8::text,
				'law_count', $9::int, 'generated_from', 'enacted-laws'), now())
			ON CONFLICT (path) DO UPDATE SET data = twoai_pages.data || EXCLUDED.data, updated_at = now()`,
			"compliance/"+slug+".json", uid, slug, name+" AI Laws", st, today, summary, body, len(laws)); err != nil {
			return fmt.Errorf("%s: %w", st, err)
		}
		children = append(children, slug)
		generated++
	}
	// The parent lists exactly the states that have enacted law.
	if len(children) > 0 {
		cj := "[\"" + strings.Join(children, "\",\"") + "\"]"
		db.Exec(`UPDATE twoai_pages SET data = jsonb_set(data, '{children}', $1::jsonb), updated_at=now() WHERE path='compliance/state-ai-laws.json'`, cj)
	}
	fmt.Printf("twoai_state_law_pages: states_with_enacted_law=%d generated=%d hand_written_augmented=%d\n", len(children), generated, augmented)
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
