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
type liveCase struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type quotedRef struct {
	Lawsuit string `json:"lawsuit"`
	Path    string `json:"path"`
	By      string `json:"by"`
	Doc     string `json:"doc,omitempty"`
	DocURL  string `json:"doc_url,omitempty"`
}

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
		LiveCases  []liveCase      `json:"live_cases,omitempty"`
		LiveTotal  int             `json:"live_total,omitempty"`
		QuotedIn   []quotedRef     `json:"quoted_in,omitempty"`
		QuotedTot  int             `json:"quoted_total,omitempty"`
	}

	// DOCTRINE LANES, NOT QUOTATIONS. Stephen asked which precedents the live
	// AI cases are quoting in their defenses. The lawsuit rows carry no brief
	// text, and the district-court orders live in RECAP as PDFs rather than in
	// the opinion index, so a verified who-quoted-what pass needs a document
	// harvest this stage does not do. What the tracker does hold, verified, is
	// each case's claim category. So every precedent page lists the ACTIVE
	// cases in its doctrine's lane, newest first, labelled as exactly that:
	// tracker classification, not a citation count. Wrong-but-plausible here
	// would be claiming Sony is quoted in a docket nobody checked.
	laneFor := map[string][]string{
		"Fair use":                            {"copyright"},
		"Fair use and secondary liability":    {"copyright"},
		"Fair use and intermediate copying":   {"copyright"},
		"Copyrightability":                    {"copyright"},
		"Secondary liability":                 {"copyright"},
		"DMCA safe harbour":                   {"copyright"},
		"Computer Fraud and Abuse Act":        {"platform access & scraping"},
		"Section 230":                         {"product liability & wrongful death"},
		"Product liability and platform duty": {"product liability & wrongful death"},
		"Defamation fault standards":          {"product liability & wrongful death"},
		"Biometric privacy":                   {"biometric privacy"},
		"Patent eligibility":                  {"patent"},
		"Authorship and inventorship":         {"patent"},
	}
	laneCases := map[string][]liveCase{}
	laneTotal := map[string]int{}
	for doctrine, cats := range laneFor {
		var total int
		if err := db.QueryRow(`SELECT count(*) FROM ai_lawsuits
			WHERE is_active AND category = ANY($1)`, pq.Array(cats)).Scan(&total); err != nil || total == 0 {
			continue
		}
		laneTotal[doctrine] = total
		lr, err := db.Query(`SELECT case_name, slug FROM ai_lawsuits
			WHERE is_active AND category = ANY($1)
			ORDER BY filed_date DESC NULLS LAST, slug LIMIT 8`, pq.Array(cats))
		if err != nil {
			continue
		}
		for lr.Next() {
			var lc liveCase
			var slug string
			if lr.Scan(&lc.Name, &slug) == nil {
				lc.Path = "/ai-lawsuits/" + slug + "/"
				laneCases[doctrine] = append(laneCases[doctrine], lc)
			}
		}
		lr.Close()
	}

	// Verified quotations harvested from the docket record by twoai_recap.
	// Each row is a precedent matched inside the extracted text of a specific
	// RECAP document, so the page can say quoted and mean it.
	quoted := map[string][]quotedRef{}
	quotedTot := map[string]int{}
	qr, err := db.Query(`SELECT c.precedent_slug, l.case_name, c.lawsuit_slug,
			c.quoted_by, COALESCE(c.doc_description,''), COALESCE(c.doc_url,'')
		FROM twoai_precedent_citations c
		JOIN ai_lawsuits l ON l.slug = c.lawsuit_slug AND l.is_active
		ORDER BY c.found_on DESC, c.lawsuit_slug, c.recap_doc_id`)
	if err == nil {
		for qr.Next() {
			var pslug string
			var q quotedRef
			var lslug string
			if qr.Scan(&pslug, &q.Lawsuit, &lslug, &q.By, &q.Doc, &q.DocURL) != nil {
				continue
			}
			q.Path = "/ai-lawsuits/" + lslug + "/"
			quotedTot[pslug]++
			if len(quoted[pslug]) < 12 {
				quoted[pslug] = append(quoted[pslug], q)
			}
		}
		qr.Close()
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
		c.LiveCases = laneCases[c.Doctrine]
		c.LiveTotal = laneTotal[c.Doctrine]
		c.QuotedIn = quoted[c.Slug]
		c.QuotedTot = quotedTot[c.Slug]
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
			"dissected": c.Dissection != nil, "live_total": c.LiveTotal, "quoted_total": c.QuotedTot,
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
