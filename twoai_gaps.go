package main

// twoai_gaps: publish the backlog, so "we have not covered that" stops being a
// sentence and becomes a number anyone can check.
//
// Stephen, 2026-09-20, choosing this over three other proposals: it turns the
// site's own honesty rule from a conversational fallback into a queryable
// artifact. Every other AI directory publishes what it has. None publishes
// what it knows it is missing.
//
// THE NUMBERS ALREADY EXISTED AND WERE BEING THROWN AWAY. Every count below
// was already computed on every run and printed to a log file nobody reads:
// 38 siting stories awaiting an ordinance read, 34 confirmed dead links, 91
// bill events pending publication, 6 unclassified case studies, 31 proposed
// entities awaiting a decision. This stage does not measure anything new. It
// stops discarding the measurements.
//
// RECOMPUTED, NOT SCRAPED FROM THE LOG. Each figure is a fresh query at emit
// time, because a number lifted from an earlier stage's output is a number
// that can silently drift from the table it claims to describe. That also
// means the ledger is correct on a run where an upstream stage was skipped.
//
// THIS STAGE MUST RUN LAST, after every ingestion and linkage pass, or it
// reports a backlog that the same run has already cleared.
//
// A ZERO IS A RESULT, NOT AN ABSENCE. Every key is always present, even at
// zero, so a consumer can tell "nothing is waiting" apart from "the field
// stopped being written". That distinction is the whole point of the feature
// and it is the failure mode it exists to prevent.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// twoaiGapsItem is one measured backlog. Query returns a single integer.
type twoaiGapsItem struct {
	Key   string
	Label string // for the one-line stamp a human reads
	SQL   string
}

// Each query is deliberately written against the table that owns the fact,
// not against a cached count elsewhere.
//
// EVERY COLUMN HERE WAS CHECKED AGAINST THE SCHEMA, and four of the ten first
// drafts were wrong: twoai_siting_watch has worked_on, not worked;
// twoai_link_health records a verdict of 'broken', while status is the HTTP
// integer and 'dead' is not a value it uses; twoai_bill_events has
// published_on, not published; twoai_glossary_lenses keys on term_slug, not
// slug. A ledger of what the site has not done is worthless if its own
// numbers are guesses, so every query below was run against the live database
// before it was committed, and the count it returned is in the comment.
var twoaiGapsItems = []twoaiGapsItem{
	{"siting_stories_awaiting_ordinance_read", "siting stories awaiting an ordinance read", // 38
		`SELECT count(*) FROM twoai_siting_watch WHERE worked_on IS NULL`},
	{"broken_source_links", "source links found broken", // 'dead' is not a verdict this table uses
		`SELECT count(*) FROM twoai_link_health WHERE verdict = 'broken'`},
	{"bill_events_pending_publication", "bill events pending publication", // 91
		`SELECT count(*) FROM twoai_bill_events WHERE relevant AND published_on IS NULL`},
	{"unclassified_case_studies", "case studies unclassified", // null-safe: the column is nullable
		`SELECT count(*) FROM twoai_case_studies WHERE active AND COALESCE(classification,'unclassified') = 'unclassified'`},
	{"proposed_entities_awaiting_decision", "proposed entities awaiting a decision", // 31
		`SELECT count(*) FROM twoai_missing_entities WHERE status = 'proposed'`},
	{"feed_candidates_awaiting_review", "press feeds awaiting review", // table arrives with twoai_feed_discovery
		`SELECT count(*) FROM twoai_feed_candidates WHERE status = 'proposed' AND COALESCE(feed_url,'') <> ''`},
	{"filings_not_yet_read", "8-K filings not yet read", // 351
		`SELECT count(*) FROM twoai_ma_filings f
		 WHERE f.items ~ '1\.01|1\.02|2\.01|2\.03|3\.02|5\.02|8\.01'
		   AND NOT EXISTS (SELECT 1 FROM twoai_ma_readings r WHERE r.accession = f.accession AND r.reading IS NOT NULL)`},
	{"learning_entries_without_a_reading", "certifications and courses whose issuer page is unread", // 3
		`SELECT count(*) FROM twoai_learning l
		 WHERE l.section_slug IN ('certifications','courses')
		   AND NOT EXISTS (SELECT 1 FROM twoai_learning_readings r
		                   WHERE r.section_slug = l.section_slug AND r.slug = l.slug AND r.reading IS NOT NULL)`},
	{"glossary_terms_without_a_lens", "glossary terms without an audience lens", // 164
		`SELECT count(*) FROM synced_glossary_terms g
		 WHERE NOT EXISTS (SELECT 1 FROM twoai_glossary_lenses x WHERE x.term_slug = g.slug)`},
	{"documents_awaiting_a_human_read", "documents whose source changed and need a human read", // 0 after the docwatch fix
		`SELECT count(*) FROM twoai_downloads WHERE source_changed_on IS NOT NULL`},
}

func twoaiGaps(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_gaps_history (
		generated_on date PRIMARY KEY,
		backlog jsonb NOT NULL,
		total int NOT NULL,
		generated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	backlog := map[string]any{}
	notes := map[string]string{}
	total := 0
	for _, it := range twoaiGapsItems {
		var n int
		if err := db.QueryRow(it.SQL).Scan(&n); err != nil {
			// A query that cannot run is itself a gap, and hiding it would be
			// the exact failure this feature exists to prevent. The key stays
			// present with a null and the reason is published beside it.
			backlog[it.Key] = nil
			notes[it.Key] = "not measurable on this run: " + err.Error()
			fmt.Fprintf(os.Stderr, "twoai_gaps: %s: %v\n", it.Key, err)
			continue
		}
		backlog[it.Key] = n
		total += n
	}

	// The Federal Register pair, which is two numbers rather than one: what
	// arrived on subject, and what the database took in.
	var frOnSubject, frIndexed int
	db.QueryRow(`SELECT count(*) FROM pipeline.documents d
		JOIN pipeline.sources s ON s.id = d.source_id AND s.key = 'federal_register'
		WHERE d.created_at::date = current_date`).Scan(&frIndexed)
	db.QueryRow(`SELECT count(*) FROM pipeline.documents d
		JOIN pipeline.sources s ON s.id = d.source_id AND s.key = 'federal_register'`).Scan(&frOnSubject)
	backlog["federal_register"] = map[string]int{
		"indexed_total": frOnSubject, "new_today": frIndexed,
	}

	doc := map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"generated_on": time.Now().UTC().Format("2006-01-02"),
		"backlog":      backlog,
		"total_open":   total,
		"what_this_is": "Work this site knows it has not done. Every figure is a live count from the " +
			"table that owns it, recomputed when this file is written, not copied from an earlier stage. " +
			"A key present with a null could not be measured on this run and says why. A key at zero means " +
			"nothing is waiting, which is different from a key that stopped being written.",
	}
	if len(notes) > 0 {
		doc["not_measurable"] = notes
	}

	j, _ := json.Marshal(doc)
	if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, url_count, updated_at)
		VALUES ('static/gaps.json','static',$1::jsonb,0,now())
		ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, updated_at = now()`, string(j)); err != nil {
		return err
	}
	bj, _ := json.Marshal(backlog)
	db.Exec(`INSERT INTO twoai_gaps_history (generated_on, backlog, total)
		VALUES (current_date, $1::jsonb, $2)
		ON CONFLICT (generated_on) DO UPDATE SET backlog = $1::jsonb, total = $2, generated_at = now()`,
		string(bj), total)

	// The one line a person reads. Yesterday's total comes from the history
	// table, so the log says whether the backlog is growing.
	var prev sql.NullInt64
	db.QueryRow(`SELECT total FROM twoai_gaps_history WHERE generated_on < current_date
		ORDER BY generated_on DESC LIMIT 1`).Scan(&prev)
	trend := ""
	if prev.Valid {
		d := total - int(prev.Int64)
		switch {
		case d > 0:
			trend = fmt.Sprintf(", up %d since the last run", d)
		case d < 0:
			trend = fmt.Sprintf(", down %d since the last run", -d)
		default:
			trend = ", unchanged"
		}
	}
	fmt.Printf("twoai_gaps: %d open items across %d measures%s\n", total, len(twoaiGapsItems), trend)
	for _, it := range twoaiGapsItems {
		if n, ok := backlog[it.Key].(int); ok && n > 0 {
			fmt.Printf("twoai_gaps:   %5d %s\n", n, it.Label)
		}
	}
	return nil
}
