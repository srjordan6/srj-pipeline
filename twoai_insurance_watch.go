package main

// twoai_insurance_watch: the weekly report on what needs Stephen's attention
// in the AI Insurance hub, and the parts of the site the hub depends on.
//
// Stephen, 2026-09-17: develop a plan to keep all this data current. The
// evidence keeps itself current through the source harvest, the news gate,
// the vendor feeds and the LegiScan and Federal Register queries, all of
// which learned the insurance vocabulary that day. What none of them can do
// is decide that a page's JUDGMENT is stale. This stage does not decide
// either. It lists, once a week, the three things only a person can act on:
//
//   1. Anchor reports that are due. The hub rests on a handful of annual
//      publications (Swiss Re sigma, Zurich's data centre risk report, the
//      FM guidance, Aon's program limit, S&P's TIV figures). Each has an
//      expected month in twoai_report_watch. When that month arrives and the
//      harvester has not seen a changed edition, it is named here.
//
//   2. Items whose evidence moved. A coverage item was seeded on a date. If
//      any source it cites has been re-fetched with different content since
//      that date, the item's template may describe a market that has shifted.
//      This is Stephen's review queue - not all 100 every quarter, only the
//      ones the evidence moved under.
//
//   3. Claims still waiting for a source. Points published with source: ""
//      render as "Tracked. No source published yet." That is honest on the
//      page and invisible in the log until listed.
//
// It writes nothing. It prints, so the weekly log carries the queue and the
// action stays Stephen's.

import (
	"database/sql"
	"fmt"
	"time"
)

func twoaiInsuranceWatch(db *sql.DB) error {
	month := int(time.Now().UTC().Month())

	// 1. Anchor reports due.
	rows, err := db.Query(`
		SELECT name, publisher, expected_month, COALESCE(last_seen_edition,''), last_seen_on::text
		FROM twoai_report_watch
		WHERE expected_month > 0 AND expected_month <= $1
		  AND (last_seen_on IS NULL OR EXTRACT(YEAR FROM last_seen_on) < EXTRACT(YEAR FROM current_date)
		       OR EXTRACT(MONTH FROM last_seen_on) < expected_month)
		ORDER BY expected_month`, month)
	if err != nil {
		return err
	}
	due := 0
	for rows.Next() {
		var name, pub, ed, seen string
		var em int
		if rows.Scan(&name, &pub, &em, &ed, &seen) == nil {
			if due == 0 {
				fmt.Println("twoai_insurance_watch: anchor reports due and not yet seen this year:")
			}
			due++
			fmt.Printf("  %s (%s), expected month %d, last seen %s edition %s\n", name, pub, em, seen, ed)
		}
	}
	rows.Close()
	if due == 0 {
		fmt.Println("twoai_insurance_watch: no anchor report overdue")
	}

	// 2. Items whose cited sources changed after they were seeded.
	rows, err = db.Query(`
		SELECT pg.data->>'name', pg.data->>'seeded_on', h.url, h.fetched_on::date::text
		FROM twoai_pages pg
		JOIN LATERAL jsonb_array_elements(pg.data->'points') p ON true
		JOIN twoai_source_harvest h ON h.url = p->>'source'
		WHERE pg.data->>'shape' = 'coverage-item'
		  AND pg.data->>'seeded_on' IS NOT NULL
		  AND h.http_status = 200
		  AND h.fetched_on::date > (pg.data->>'seeded_on')::date
		  AND h.content_changed_on IS NOT NULL
		  AND h.content_changed_on::date > (pg.data->>'seeded_on')::date
		ORDER BY 1`)
	if err != nil {
		// content_changed_on may not exist on older schemas; say so rather
		// than fail the run.
		fmt.Printf("twoai_insurance_watch: evidence-moved query unavailable (%v); skipping\n", err)
	} else {
		moved := 0
		for rows.Next() {
			var item, seeded, url, changed string
			if rows.Scan(&item, &seeded, &url, &changed) == nil {
				if moved == 0 {
					fmt.Println("twoai_insurance_watch: items whose evidence changed after seeding, for review:")
				}
				moved++
				fmt.Printf("  %s (seeded %s): %s changed %s\n", item, seeded, url, changed)
			}
		}
		rows.Close()
		if moved == 0 {
			fmt.Println("twoai_insurance_watch: no seeded item has had a source change under it")
		}
	}

	// 3. Claims without a source.
	rows, err = db.Query(`
		SELECT pg.data->>'name', p->>'name'
		FROM twoai_pages pg, jsonb_array_elements(pg.data->'points') p
		WHERE (pg.path LIKE 'industries/ins-%' OR pg.data->>'shape' = 'coverage-item')
		  AND COALESCE(p->>'source','') = ''
		  AND NOT (pg.data ? 'draft')
		ORDER BY 1, 2`)
	if err != nil {
		return err
	}
	open := 0
	for rows.Next() {
		var page, point string
		if rows.Scan(&page, &point) == nil {
			if open == 0 {
				fmt.Println("twoai_insurance_watch: published points still waiting for a source:")
			}
			open++
			fmt.Printf("  %s: %s\n", page, point)
		}
	}
	rows.Close()
	fmt.Printf("twoai_insurance_watch: due=%d open_points=%d\n", due, open)
	return nil
}
