package main

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/lib/pq"
)

// AI CASE LAW: the precedents behind the defenses.
//
// WHAT THIS IS. Profiles of the pre-AI decisions that current AI defendants
// actually argue from: Sony on substantial non-infringing use, Campbell and
// Google Books on transformative purpose, Van Buren and hiQ on scraping,
// Zeran on Section 230, and the two cases most often used against them,
// Grokster and Warhol. The data lives in twoai_precedents, seeded by hand
// with every citation, court and decision date verified against the
// CourtListener v4 API before insertion, and the opinion URL stored per row.
// Nothing on these pages is quoted from an opinion; holdings and analysis
// are written in this site's own words, which is both the copyright rule of
// the site and the only honest way to publish legal summary.
//
// THE PAGE SHAPE IS STEPHEN'S FIVE-PILLAR DISSECTION (2026-08-29): facts and
// technical architecture, threshold jurisdiction and standing, substantive
// claims, evidence and algorithmic provenance, and normative policy, plus a
// teaching block with Socratic questions and an exercise. One field was
// added to pillar one, ai_mapping, because these are pre-AI precedents: the
// technology in the facts is a videocassette recorder or a book scanner, so
// each profile must say explicitly which parts of the old architecture map
// onto a model pipeline and which do not. A case whose dissection has not
// been written yet still renders its holding, why AI parties cite it, and
// where the analogy is weakest, and the page says the full dissection is
// coming rather than pretending the section does not exist.
func twoaiCaselaw(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	var name, blurb string
	if err := db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy
		WHERE slug='ai-case-law'`).Scan(&name, &blurb); err != nil {
		return 0, nil // section not defined yet
	}
	sectionUID := twoaiUID("section:ai-case-law")

	type caseRow struct {
		Slug       string          `json:"slug"`
		UID        string          `json:"uid"`
		CaseName   string          `json:"case_name"`
		Citation   string          `json:"citation"`
		Court      string          `json:"court"`
		Decided    string          `json:"decided_on"`
		Doctrine   string          `json:"doctrine"`
		Posture    string          `json:"posture"`
		Holding    string          `json:"holding"`
		WhyCited   string          `json:"why_ai_cites_it"`
		Limits     string          `json:"limits,omitempty"`
		CitedIn    []string        `json:"cited_in,omitempty"`
		EraTech    string          `json:"era_technology,omitempty"`
		SourceURL  string          `json:"source_url"`
		SourceName string          `json:"source_name"`
		VerifiedOn string          `json:"verified_on"`
		Dissected  string          `json:"dissected_on,omitempty"`
		Dissection json.RawMessage `json:"dissection,omitempty"`
	}

	rows, err := db.Query(`SELECT slug, case_name, citation, court,
			to_char(decided_on,'YYYY-MM-DD'), doctrine, posture, holding,
			why_ai_cites_it, COALESCE(limits,''), cited_in,
			COALESCE(era_technology,''), source_url, source_name,
			to_char(verified_on,'YYYY-MM-DD'),
			COALESCE(to_char(dissected_on,'YYYY-MM-DD'),''),
			CASE WHEN dissection = '{}'::jsonb THEN NULL ELSE dissection END
		FROM twoai_precedents
		WHERE status = 'live'
		ORDER BY sort, decided_on`)
	if err != nil {
		return 0, err
	}
	var cases []caseRow
	for rows.Next() {
		var c caseRow
		var diss []byte
		if err := rows.Scan(&c.Slug, &c.CaseName, &c.Citation, &c.Court,
			&c.Decided, &c.Doctrine, &c.Posture, &c.Holding, &c.WhyCited,
			&c.Limits, pq.Array(&c.CitedIn), &c.EraTech, &c.SourceURL,
			&c.SourceName, &c.VerifiedOn, &c.Dissected, &diss); err != nil {
			fmt.Println("twoai_build: caselaw scan:", err)
			continue
		}
		c.UID = twoaiUID("caselaw:" + c.Slug)
		if len(diss) > 0 {
			c.Dissection = json.RawMessage(diss)
		}
		cases = append(cases, c)
	}
	rows.Close()
	if len(cases) == 0 {
		fmt.Println("twoai_build: caselaw 0 rows, section not rendered")
		return 0, nil
	}

	// One document per case, plus the index. Each case page repeats the index
	// row fields so the detail route needs exactly one file, the same rule the
	// MCP and people pages follow.
	keep := make([]string, 0, len(cases))
	dissected := 0
	for _, c := range cases {
		path := "caselaw/" + c.Slug + ".json"
		if err := upsert(path, "caselaw-case", map[string]any{
			"shape": "caselaw-case", "generated": today, "case": c,
			"section": map[string]any{"uid": sectionUID, "name": name},
		}); err != nil {
			return 0, err
		}
		keep = append(keep, path)
		if c.Dissection != nil {
			dissected++
		}
	}
	// Kind-scoped prune, the same containment the person pages use: a case
	// removed from twoai_precedents disappears from the site on the next run
	// instead of surviving as an orphan no query can reach.
	if _, err := db.Exec(`DELETE FROM twoai_pages
		WHERE kind = 'caselaw-case' AND NOT (path = ANY($1))`, pq.Array(keep)); err != nil {
		return 0, err
	}

	// The index carries the rows without their dissections; a list page that
	// embedded fifteen full dissections would ship most of the section's
	// weight to readers who came to scan it.
	idx := make([]map[string]any, 0, len(cases))
	for _, c := range cases {
		idx = append(idx, map[string]any{
			"slug": c.Slug, "uid": c.UID, "case_name": c.CaseName,
			"citation": c.Citation, "court": c.Court, "decided_on": c.Decided,
			"doctrine": c.Doctrine, "posture": c.Posture, "holding": c.Holding,
			"dissected": c.Dissection != nil,
		})
	}
	if err := upsert("caselaw/index.json", "caselaw-index", map[string]any{
		"uid": sectionUID, "shape": "caselaw-index", "tax": "ai-case-law",
		"name": name, "blurb": blurb, "cases": idx, "total": len(cases),
		"dissected": dissected, "generated": today,
	}); err != nil {
		return 0, err
	}
	fmt.Printf("twoai_build: caselaw cases=%d dissected=%d\n", len(cases), dissected)
	return len(cases), nil
}
