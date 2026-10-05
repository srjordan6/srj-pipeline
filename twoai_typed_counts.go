package main

// Typed counts, theworldofai row 467 (Stephen, 2026-10-05: "why do you keep
// hard coding numbers in this site, they all need to be dynamic").
//
// A number that describes this site's own data is read from the data at
// build time through a {{token}} (twoai_live_counts.go), never typed. This
// check finds the ones still typed: in the text fields of every page
// document, taxonomy blurbs and the section pages theworldofai writes, a
// number standing before a noun that has a live count (terms, tools,
// companies, lawsuits, MCP servers, CVEs ...) and within half to one and a
// half times that count. The range keeps "seven people were injured" out and
// catches "690 terms" when the glossary holds 720. Findings go to
// twoai_typed_counts and the daily freshness row; tokens are filled before
// this runs, so a filled token is never reported.

import (
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var typedCountNouns = []struct {
	token string
	re    *regexp.Regexp
}{
	{"glossary_terms", regexp.MustCompile(`(?i)(\d{1,3}(?:,\d{3})+|\d{2,})\s+(?:\w+\s+){0,2}?terms\b`)},
	{"tools_total", regexp.MustCompile(`(?i)(\d{1,3}(?:,\d{3})+|\d{2,})\s+(?:\w+\s+){0,2}?tools\b`)},
	{"companies_total", regexp.MustCompile(`(?i)(\d{1,3}(?:,\d{3})+|\d{2,})\s+(?:\w+\s+){0,2}?companies\b`)},
	{"people_total", regexp.MustCompile(`(?i)(\d{1,3}(?:,\d{3})+|\d{2,})\s+(?:\w+\s+){0,2}?(?:people|person profiles)\b`)},
	{"lawsuits_total", regexp.MustCompile(`(?i)(\d{1,3}(?:,\d{3})+|\d{2,})\s+(?:\w+\s+){0,2}?(?:lawsuits|cases)\b`)},
	{"mcp_servers_total", regexp.MustCompile(`(?i)(\d{1,3}(?:,\d{3})+|\d{2,})\s+(?:\w+\s+){0,2}?MCP servers\b`)},
	{"cves_total", regexp.MustCompile(`(?i)(\d{1,3}(?:,\d{3})+|\d{2,})\s+(?:\w+\s+){0,2}?CVEs\b`)},
	{"cwes_total", regexp.MustCompile(`(?i)(\d{1,3}(?:,\d{3})+|\d{2,})\s+(?:\w+\s+){0,2}?(?:CWEs|weakness classes)\b`)},
	{"facilities_total", regexp.MustCompile(`(?i)(\d{1,3}(?:,\d{3})+|\d{2,})\s+(?:\w+\s+){0,2}?(?:facilities|data ?centers)\b`)},
	{"model_families_total", regexp.MustCompile(`(?i)(\d{1,3}(?:,\d{3})+|\d{2,})\s+(?:\w+\s+){0,2}?families\b`)},
	{"benchmarks_total", regexp.MustCompile(`(?i)(\d{1,3}(?:,\d{3})+|\d{2,})\s+(?:\w+\s+){0,2}?benchmarks\b`)},
}

type typedCount struct {
	Where, Token, Found, Live, Snippet string
}

// twoaiTypedCountScan returns the typed counts it finds in s.
func twoaiTypedCountScan(where, s string, live map[string]string) []typedCount {
	var out []typedCount
	for _, n := range typedCountNouns {
		lv, ok := live[n.token]
		if !ok {
			continue
		}
		want, err := strconv.Atoi(strings.ReplaceAll(lv, ",", ""))
		if err != nil || want == 0 {
			continue
		}
		for _, m := range n.re.FindAllStringSubmatchIndex(s, -1) {
			num := s[m[2]:m[3]]
			got, err := strconv.Atoi(strings.ReplaceAll(num, ",", ""))
			if err != nil || got*2 < want || got*2 > want*3 {
				continue
			}
			a, b := m[0]-60, m[1]+40
			if a < 0 {
				a = 0
			}
			if b > len(s) {
				b = len(s)
			}
			out = append(out, typedCount{where, n.token, num, lv, strings.TrimSpace(s[a:b])})
		}
	}
	return out
}

func twoaiTypedCounts(db *sql.DB) []typedCount {
	live := twoaiLiveCounts(db)
	var found []typedCount
	scan := func(q, label string) {
		rows, err := db.Query(q)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var where, text string
			if rows.Scan(&where, &text) == nil {
				found = append(found, twoaiTypedCountScan(label+where, twoaiFillLiveCounts(text, live), live)...)
			}
		}
	}
	scan(`SELECT path, concat_ws(' ', data->>'answer', data->>'blurb', data->>'summary', data->>'intro',
			(SELECT string_agg(x->>'body', ' ') FROM jsonb_array_elements(CASE WHEN jsonb_typeof(data->'sections') = 'array' THEN data->'sections' ELSE '[]'::jsonb END) x))
		FROM twoai_pages`, "page ")
	scan(`SELECT slug, COALESCE(blurb,'') || ' ' || COALESCE(line,'') FROM twoai_taxonomy WHERE status <> 'retired'`, "taxonomy ")
	scan(`SELECT slug, concat_ws(' ', answer, blurb, meaning) FROM twoai_section_pages WHERE status = 'live'`, "section page ")
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_typed_counts (found_on date NOT NULL DEFAULT current_date, where_found text, token text,
		found text, live text, snippet text)`)
	db.Exec(`DELETE FROM twoai_typed_counts`)
	for _, f := range found {
		db.Exec(`INSERT INTO twoai_typed_counts (where_found, token, found, live, snippet) VALUES ($1,$2,$3,$4,$5)`,
			f.Where, f.Token, f.Found, f.Live, f.Snippet)
	}
	fmt.Printf("twoai_typed_counts: %d typed counts of the site's own data found ok=true\n", len(found))
	return found
}
