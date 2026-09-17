package main

// twoai_live_counts: numbers in prose that stay true.
//
// Stephen, 2026-09-17: is the Research Library blurb up to date? It said
// 1.38 million works, 964,000 with abstracts, reaching back to 1812. The
// corpus held 3.64 million, 2.5 million with abstracts, back to 1633. The
// numbers were literal text in a taxonomy blurb, true the day they were
// typed and wrong the day after, and nothing was going to notice.
//
// A blurb now writes {{works_total}} and the publisher fills it from the
// database on every run. Prose carries the shape of the sentence; the data
// carries the number. A placeholder with no value is left visible rather
// than silently blanked, so a typo in a name is seen on the page.

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

var liveCountRe = regexp.MustCompile(`\{\{([a-z_]+)\}\}`)

// twoaiLiveCounts computes every value a blurb may ask for. Once per run;
// the queries are cheap and the values are stable for the run's duration.
func twoaiLiveCounts(db *sql.DB) map[string]string {
	v := map[string]string{}
	one := func(key, q string) {
		var n sql.NullInt64
		if err := db.QueryRow(q).Scan(&n); err == nil && n.Valid {
			v[key] = withCommas(n.Int64)
		}
	}
	one("works_total", `SELECT count(*) FROM twoai_works WHERE duplicate_of IS NULL`)
	one("works_with_abstract", `SELECT count(*) FROM twoai_works WHERE duplicate_of IS NULL AND COALESCE(abstract,'') <> ''`)
	one("works_earliest_year", `SELECT min(pub_year) FROM twoai_works WHERE pub_year > 1000`)
	one("shelf_papers", `SELECT COALESCE(NULLIF(data->>'total','')::int, 0) FROM twoai_pages WHERE path = 'research/index.json'`)
	one("companies_total", `SELECT count(*) FROM twoai_company_profiles`)
	one("people_total", `SELECT count(*) FROM site_people`)
	one("lawsuits_total", `SELECT count(*) FROM twoai_lawsuits`)
	one("facilities_total", `SELECT count(*) FROM twoai_dc_facilities`)
	one("bills_total", `SELECT count(DISTINCT (raw->'bill'->>'state', raw->'bill'->>'bill_number')) FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id AND s.key='legiscan'`)
	one("enacted_laws_total", `SELECT count(*) FROM twoai_bill_events WHERE relevant AND status = 4`)
	one("tools_total", `SELECT COALESCE(NULLIF(data->>'total','')::int, 0) FROM twoai_pages WHERE path = 'tools/index.json'`)
	one("mcp_servers_total", `SELECT count(*) FROM twoai_mcp_servers`)
	if y, ok := v["works_earliest_year"]; ok {
		v["works_earliest_year"] = strings.ReplaceAll(y, ",", "")
	}
	return v
}

// twoaiFillLiveCounts substitutes {{name}} placeholders. Unknown names stay
// as written, which makes a misspelt placeholder visible rather than blank.
func twoaiFillLiveCounts(s string, counts map[string]string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	return liveCountRe.ReplaceAllStringFunc(s, func(m string) string {
		key := strings.Trim(m, "{}")
		if val, ok := counts[key]; ok {
			return val
		}
		return m
	})
}

func withCommas(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
