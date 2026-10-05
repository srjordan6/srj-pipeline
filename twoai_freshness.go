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
	case strings.HasPrefix(path, "lawsuits/"):
		// Dockets are checked daily. Without this line the tracker fell to the
		// default, and on 2026-09-18 its live stamp read "refreshed every 30
		// days" directly under a sentence saying dockets are checked daily.
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
	// The default, for a page family this table does not name. 30 days,
	// Stephen's decision on 2026-09-18. It was 60.
	return 30
}

// twoaiSettledCadenceDays is the cadence for a page whose subject cannot
// change. Stephen, 2026-09-18, looking at the people directory: if somebody is
// already dead we probably do not need to refresh their data quite so often.
// 32 of the 283 people here have a date of death, from Ada Lovelace in 1852 to
// John Searle in 2025. A living researcher changes employer, publishes and
// wins prizes, which is what 90 days is for. A record that ends does not. Once
// a year is enough to catch a corrected date, a new biography or a posthumous
// award, and it keeps 32 pages out of the overdue list four times a year for
// no reason. Not never: the page is about a person, and what is known about
// a person does still move.
const twoaiSettledCadenceDays = 365

// twoaiLastCheckedSQL is the date a page was last confirmed against its
// sources, for a query over twoai_pages aliased p: the latest of when it was
// rebuilt, when a person last reviewed it, and when a stage last checked it
// against an unchanged source (twoai_page_checks). A law page rewritten on
// 2026-09-17 and confirmed unchanged by every LegiScan sweep since is current,
// not eleven days overdue. 2026-09-28.
const twoaiLastCheckedSQL = `GREATEST(NULLIF(left(p.data->>'generated',10),'')::date,
	NULLIF(left(p.data->>'last_reviewed',10),'')::date,
	(SELECT pc.checked_on FROM twoai_page_checks pc WHERE pc.path = p.path))`

// twoaiRecordChecks notes that pages were confirmed against their sources
// today without being rewritten. Kept in its own table so the page documents,
// and everything hashed from them, do not change when nothing changed.
func twoaiRecordChecks(db *sql.DB, pathLike, why string) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_page_checks (path text PRIMARY KEY, checked_on date NOT NULL, how text)`)
	res, err := db.Exec(`INSERT INTO twoai_page_checks (path, checked_on, how)
		SELECT path, current_date, $2 FROM twoai_pages WHERE path LIKE $1
		ON CONFLICT (path) DO UPDATE SET checked_on = EXCLUDED.checked_on, how = EXCLUDED.how
		WHERE twoai_page_checks.checked_on < EXCLUDED.checked_on`, pathLike, why)
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			fmt.Printf("twoai_freshness: %d pages under %s confirmed today (%s)\n", n, pathLike, why)
		}
	}
}

// twoaiStampFreshness writes the contract onto every page document. Runs
// inside the publish step, so a page cannot be published without one.
func twoaiStampFreshness(db *sql.DB) error {
	rows, err := db.Query(`SELECT path, COALESCE(kind,''), COALESCE(data->>'shape',''), COALESCE(data->>'died',''),
		COALESCE(data->>'archived','') = 'true', COALESCE(NULLIF(data->>'review_interval_days','')::int, 0),
		kind = 'week' AND path < (SELECT max(path) FROM twoai_pages WHERE kind = 'week')
		FROM twoai_pages`)
	if err != nil {
		return err
	}
	type pg struct {
		path, kind, shape, died string
		archived, pastWeek      bool
		review                  int
	}
	var pages []pg
	for rows.Next() {
		var p pg
		if rows.Scan(&p.path, &p.kind, &p.shape, &p.died, &p.archived, &p.review, &p.pastWeek) == nil {
			pages = append(pages, p)
		}
	}
	rows.Close()
	byCadence := map[int][]string{}
	settled := 0
	for _, p := range pages {
		c := twoaiCadenceDays(p.path, p.kind, p.shape)
		// An archived page, such as an incident that has left the AI
		// Incident Database's recent window (2026-09-28), is a closed record:
		// yearly, like a person who has died.
		//
		// A weekly recap for a week that has ended is a closed record too
		// (2026-09-28: four past weeks were listed as overdue every day).
		if (p.kind == "person" && strings.TrimSpace(p.died) != "") || p.archived || p.pastWeek {
			c = twoaiSettledCadenceDays
			settled++
		} else if p.review > 0 {
			// HAND-REVIEWED PAGES KEEP THEIR OWN CYCLE, 2026-09-28. The
			// benchmark and prompt guides declare review_interval_days (90 and
			// 180) and a last_reviewed date; this stamp overwrote the cycle
			// with the kind default of 30, so the Data Quality page reported
			// 38 pages overdue that were inside their review period.
			c = p.review
		}
		byCadence[c] = append(byCadence[c], p.path)
	}
	if settled > 0 {
		fmt.Printf("twoai_freshness: %d people with a date of death are on the yearly cadence\n", settled)
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
		SELECT p.path, COALESCE(` + twoaiLastCheckedSQL + `::text,''), (p.data->>'refresh_every_days')::int,
		       (current_date - ` + twoaiLastCheckedSQL + `) - (p.data->>'refresh_every_days')::int AS overdue_days
		FROM twoai_pages p
		WHERE p.data ? 'refresh_every_days' AND NULLIF(p.data->>'generated','') IS NOT NULL
		  AND (current_date - ` + twoaiLastCheckedSQL + `) > (p.data->>'refresh_every_days')::int
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
		// The same measure as the list above and the daily bridge row: last
		// checked, not last built (row 477: 510 here against 1 in the summary).
		db.QueryRow(`SELECT count(*) FROM twoai_pages p WHERE p.data ? 'refresh_every_days' AND NULLIF(p.data->>'generated','') IS NOT NULL
			AND (current_date - ` + twoaiLastCheckedSQL + `) > (p.data->>'refresh_every_days')::int`).Scan(&total)
		fmt.Printf("twoai_freshness: %d pages overdue in total\n", total)
	}
	return nil
}
