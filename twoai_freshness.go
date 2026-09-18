package main

// twoai_freshness: every page declares how fresh it should be, and the site
// says whether it is.
//
// Stephen, 2026-09-17: stop making me check all the time on existing pages.
// Every page should have something that makes certain it is up to date.
//
// A date on a page is not that. "Last verified 2026-09-16" tells a reader
// when the page was built and nothing about whether it should have been
// rebuilt since. The 50-state law map carried yesterday's date while 177
// bills arrived, because the site build was failing on an unrelated fault
// and nothing on the page could say so.
//
// This gives each page a CONTRACT: a cadence, in days, derived from what
// kind of page it is - a daily briefing is stale after one day, a law page
// after a week, a company profile after ninety. The pipeline stamps every
// page with its cadence and the run that built it. The site compares the
// stamp to the clock at render time and, when a page is past its cadence,
// says so in the data stamp, in words: "Due for refresh. Last built
// 2026-09-16, expected every 1 day." That sentence is on the page, where the
// reader and Stephen both see it, rather than in a log.
//
// And the weekly watch lists every page past its cadence, most overdue
// first, so the question "what is stale" has an answer without opening
// pages one at a time.

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// twoaiCadenceDays is the contract by page family. A page not matched here
// gets the default, which is generous, because a wrong stale flag is worse
// than a missing one.
func twoaiCadenceDays(path, kind, shape string) int {
	switch {
	case strings.HasPrefix(path, "news/"):
		return 1
	case strings.HasPrefix(path, "laws/"), strings.HasPrefix(path, "compliance/law-"):
		return 7
	case strings.HasPrefix(path, "compliance/"):
		return 14
	case strings.HasPrefix(path, "jobs/"), strings.HasPrefix(path, "stocks/"):
		return 1
	case strings.HasPrefix(path, "datacenters/"), strings.HasPrefix(path, "dc/"):
		return 7
	case strings.HasPrefix(path, "industries/"):
		return 30
	case strings.HasPrefix(path, "companies/"), strings.HasPrefix(path, "people/"):
		return 90
	case strings.HasPrefix(path, "ecosystem/"):
		return 7
	case strings.HasPrefix(path, "research/"), strings.HasPrefix(path, "papers/"):
		return 30
	case strings.HasPrefix(path, "glossary/"), strings.HasPrefix(path, "tools/"):
		return 90
	}
	return 60
}

// twoaiStampFreshness writes the contract onto every page document. Runs
// inside the publish step, so a page cannot be published without one.
func twoaiStampFreshness(db *sql.DB) error {
	rows, err := db.Query(`SELECT path, COALESCE(kind,''), COALESCE(data->>'shape','') FROM twoai_pages`)
	if err != nil {
		return err
	}
	type pg struct{ path, kind, shape string }
	var pages []pg
	for rows.Next() {
		var p pg
		if rows.Scan(&p.path, &p.kind, &p.shape) == nil {
			pages = append(pages, p)
		}
	}
	rows.Close()
	byCadence := map[int][]string{}
	for _, p := range pages {
		c := twoaiCadenceDays(p.path, p.kind, p.shape)
		byCadence[c] = append(byCadence[c], p.path)
	}
	for c, paths := range byCadence {
		// One statement per cadence value rather than one per page.
		//
		// pq.Array, not the bare slice. database/sql cannot send a Go []string
		// as a Postgres array by itself, so this failed on every run with
		// "unsupported type []string" and no page ever received its
		// refresh_every_days. The stage logged the error and the pipeline
		// carried on, which is how it went unnoticed until Stephen's log of
		// 2026-09-18. main.go already wraps its arrays this way.
		if _, err := db.Exec(`UPDATE twoai_pages SET data = data || jsonb_build_object('refresh_every_days', $1::int)
			WHERE path = ANY($2) AND COALESCE((data->>'refresh_every_days')::int, -1) <> $1::int`, c, pq.Array(paths)); err != nil {
			return fmt.Errorf("stamp cadence %d: %w", c, err)
		}
	}
	return nil
}

// twoaiFreshnessReport lists every page past its cadence, most overdue first.
// Called from the weekly watch and available on its own.
func twoaiFreshnessReport(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT path, COALESCE(data->>'generated',''), (data->>'refresh_every_days')::int,
		       (current_date - NULLIF(data->>'generated','')::date) - (data->>'refresh_every_days')::int AS overdue_days
		FROM twoai_pages
		WHERE data ? 'refresh_every_days' AND NULLIF(data->>'generated','') IS NOT NULL
		  AND (current_date - NULLIF(data->>'generated','')::date) > (data->>'refresh_every_days')::int
		ORDER BY overdue_days DESC LIMIT 40`)
	if err != nil {
		return err
	}
	n := 0
	for rows.Next() {
		var path, gen string
		var cadence, overdue int
		if rows.Scan(&path, &gen, &cadence, &overdue) == nil {
			if n == 0 {
				fmt.Println("twoai_freshness: pages past their refresh cadence, most overdue first:")
			}
			n++
			fmt.Printf("  %s  built %s, expected every %d days, %d days overdue\n", path, gen, cadence, overdue)
		}
	}
	rows.Close()
	if n == 0 {
		fmt.Println("twoai_freshness: every page is within its refresh cadence")
	} else {
		var total int
		db.QueryRow(`SELECT count(*) FROM twoai_pages WHERE data ? 'refresh_every_days' AND NULLIF(data->>'generated','') IS NOT NULL
			AND (current_date - NULLIF(data->>'generated','')::date) > (data->>'refresh_every_days')::int`).Scan(&total)
		fmt.Printf("twoai_freshness: %d pages overdue in total\n", total)
	}
	return nil
}
