package main

// twoai_glossary_lenses: every glossary term explained for a child, an
// executive and a small business owner.
//
// Stephen, 2026-09-30: "on every page of the glossary you need to explain For
// a child, For an executive, and For a small business owner; many of the
// pages do not have this information." The 2,156 lenses in
// twoai_glossary_lenses were all hand curated, so 153 terms had no child
// lens, 343 no executive lens and 556 no small business lens. This stage
// writes the missing ones from the term's own definition and example, sixty
// terms a run, origin 'model', and leaves every curated lens untouched.

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

const twoaiLensTermsPerRun = 60

var twoaiLensRequired = []string{"child", "executive", "small-business"}

func twoaiGlossaryLenses(db *sql.DB) error {
	rows, err := db.Query(`SELECT t->>'slug', t->>'term', COALESCE(t->>'definition',''), COALESCE(t->>'example',''), COALESCE(t->>'category','')
		FROM twoai_pages p, jsonb_array_elements(p.data->'terms') t
		WHERE p.path = 'glossary/glossary.json' AND COALESCE(t->>'slug','') <> ''
		  AND (SELECT count(*) FROM twoai_glossary_lenses l WHERE l.term_slug = t->>'slug' AND l.audience = ANY($1)) < 3
		ORDER BY t->>'slug' LIMIT $2`, pq.Array(twoaiLensRequired), twoaiLensTermsPerRun)
	if err != nil {
		return err
	}
	type term struct{ slug, name, def, ex, cat string }
	var terms []term
	for rows.Next() {
		var t term
		if rows.Scan(&t.slug, &t.name, &t.def, &t.ex, &t.cat) == nil {
			terms = append(terms, t)
		}
	}
	rows.Close()
	if len(terms) == 0 {
		return nil
	}
	written, failed := 0, 0
	for _, t := range terms {
		have := map[string]bool{}
		if hr, herr := db.Query(`SELECT audience FROM twoai_glossary_lenses WHERE term_slug=$1`, t.slug); herr == nil {
			for hr.Next() {
				var a string
				if hr.Scan(&a) == nil {
					have[a] = true
				}
			}
			hr.Close()
		}
		var need []string
		for _, a := range twoaiLensRequired {
			if !have[a] {
				need = append(need, a)
			}
		}
		if len(need) == 0 {
			continue
		}
		sys := "You explain one artificial intelligence term for The World of AI's glossary to particular readers. " +
			"Write only the explanations asked for, each two to three sentences, from the definition and example supplied; add no facts, products, numbers or claims they do not support. " +
			"child: for a ten year old, everyday words, one familiar comparison, no jargon. " +
			"executive: what the term means for decisions, money, risk and who is accountable, in plain business English. " +
			"small-business: what it means in practice for a company with five to fifty staff, when it matters to them and when it does not, and what it tends to cost or save in time. " +
			"Plain English. Commas, not dashes. No em dashes. No marketing language. " +
			`Answer with one JSON object whose keys are exactly the audiences requested, for example {"child": "", "executive": ""}.`
		user := fmt.Sprintf("Term: %s\nCategory: %s\nDefinition: %s\nExample: %s\n\nAudiences requested: %s\n\nAnswer now.",
			t.name, t.cat, t.def, t.ex, strings.Join(need, ", "))
		out, _, gerr := twoaiGenerate("page_readings", sys, user)
		if gerr != nil {
			failed++
			continue
		}
		got, perr := twoaiArtJSON(out)
		if perr != nil {
			failed++
			continue
		}
		for _, a := range need {
			body := strings.TrimSpace(got[a])
			if len(body) < 60 {
				failed++
				continue
			}
			if _, ierr := db.Exec(`INSERT INTO twoai_glossary_lenses (term_slug, audience, body, origin, reviewed_on, review_interval_days, created_at, updated_at)
				VALUES ($1,$2,$3,'model',current_date,365,now(),now()) ON CONFLICT DO NOTHING`, t.slug, a, body); ierr == nil {
				written++
			}
		}
	}
	fmt.Printf("twoai_glossary_lenses: terms=%d lenses_written=%d failed=%d\n", len(terms), written, failed)
	return nil
}
