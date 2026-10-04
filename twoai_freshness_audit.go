package main

// The freshness audit, theworldofai bridge row 439 (Stephen, 2026-10-03: "why
// do i keep having to ask you, keep all data on the website current").
//
// The page contracts in twoai_freshness.go measure when a page was rebuilt,
// not how old the data inside it is. The benchmark pages were rebuilt on time
// every day while the LMArena results on them stayed at 2026-08, so the
// contracts called them healthy. This audit measures the data itself.
//
// freshDatasets is the registry: every dataset the site shows, with the query
// that says how old each item in it is and how often it should be refreshed.
// Each run, inside the build and just before the Data Quality page:
//
//  1. every dataset is measured, and the items past their cadence are kept in
//     twoai_freshness_items (an item that is current again is removed and
//     counted as fixed);
//  2. a dataset with overdue items is queued for its next run ahead of new
//     work: its once-a-day gate is reopened, and the stage that refreshes it
//     can ask twoaiFreshnessQueue for its overdue items, oldest first;
//  3. once a day one bridge row goes to theworldofai, "Freshness: n overdue,
//     m fixed, these need a decision", listing what the pipeline cannot fix
//     by itself: a dataset with no automatic refresher, or one overdue three
//     runs running;
//  4. the share of datasets within cadence goes on /data-quality/, dated.
//
// An item with no date at all (a benchmark with no results) counts as
// overdue: data that was never fetched is not current.

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

type freshDataset struct {
	Key, Label string
	// Items returns (item text, as_of date or NULL, cadence int or NULL).
	// A NULL cadence takes Cadence.
	Items   string
	Cadence int
	// Stage is the once-a-day stage that refreshes the dataset, reopened
	// when it is overdue. Empty when the refresh runs every pass anyway.
	Stage string
	// Auto is false when nothing in the pipeline refreshes this dataset by
	// itself yet, so an overdue item needs a decision, not another run.
	Auto bool
	How  string
}

// The live leaderboards of row 438 move weekly.
const freshBenchWeekly = `'lmarena','livebench','swe-bench','hle','gpqa','osworld','gaia','webarena','tau-bench','metr-time-horizon'`

var freshDatasets = []freshDataset{
	{Key: "benchmarks", Label: "Benchmark results", Auto: false,
		How: "official leaderboards; automatic only for SWE-bench, HLE and GPQA so far, the rest are being built under row 438",
		Items: `SELECT slug, CASE WHEN results->>'as_of' ~ '^\d{4}-\d{2}-\d{2}' THEN left(results->>'as_of',10)::date
				WHEN results->>'as_of' ~ '^\d{4}-\d{2}$' THEN (results->>'as_of' || '-01')::date END,
			CASE WHEN slug IN (` + freshBenchWeekly + `) THEN 7 WHEN slug LIKE 'mlperf%' THEN 90 ELSE review_interval_days END
			FROM twoai_benchmarks`},
	{Key: "model_catalog", Label: "Model catalog", Cadence: 1, Auto: true, Stage: "twoai_model_watch",
		How: "Hugging Face and provider model lists", Items: `SELECT 'catalog', max(fetched_at)::date, NULL::int FROM twoai_model_catalog`},
	{Key: "model_prices", Label: "Model prices", Cadence: 1, Auto: true, How: "provider price lists",
		Items: `SELECT 'prices', max(day), NULL::int FROM twoai_model_prices`},
	// A lookup table seeded once on 2026-08-18; the family histories use
	// its Wikipedia titles. Nothing refreshes it yet, so overdue means a
	// decision. (twoai_model_deprecations is not on the site, so it is not
	// listed.)
	{Key: "wikidata_models", Label: "Model records from Wikidata", Cadence: 90, Auto: false, How: "Wikidata, seeded once, no refresher yet",
		Items: `SELECT 'wikidata', max(fetched_on), NULL::int FROM twoai_wikidata_models`},
	{Key: "model_families", Label: "Model family pages", Cadence: 7, Auto: true, How: "rebuilt from the model catalog each run",
		Items: `SELECT name, page_built_on, NULL::int FROM twoai_model_families WHERE page_built_on IS NOT NULL`},
	// Fourteen days, not seven: intel checks twelve dockets a day, oldest
	// first, because CourtListener rate limits anything more, so a full
	// sweep of 150 cases takes about thirteen days. A shorter cadence here
	// would only report that limit every day.
	{Key: "lawsuits", Label: "Lawsuit dockets", Cadence: 14, Auto: true,
		How:   "CourtListener dockets, twelve a day oldest first; state cases from established outlets",
		Items: `SELECT slug, docket_checked_at::date, NULL::int FROM ai_lawsuits WHERE is_active`},
	{Key: "federal_register", Label: "Federal Register", Cadence: 7, Auto: true, How: "Federal Register API",
		Items: `SELECT 'federal register', max(fetched_at)::date, NULL::int FROM pipeline.documents WHERE source_id = 1`},
	{Key: "state_bills", Label: "State bills", Cadence: 7, Auto: true, Stage: "legiscan", How: "LegiScan API",
		Items: `SELECT 'legiscan', max(fetched_at)::date, NULL::int FROM pipeline.documents WHERE source_id = 2`},
	{Key: "court_opinions", Label: "Federal court opinions", Cadence: 7, Auto: true, How: "govinfo USCOURTS",
		Items: `SELECT 'govinfo', max(fetched_at)::date, NULL::int FROM pipeline.documents WHERE source_id = 6`},
	{Key: "cves", Label: "AI CVEs", Cadence: 1, Auto: true, How: "NVD and CISA KEV",
		Items: `SELECT 'cves', max(updated_at)::date, NULL::int FROM twoai_cves`},
	{Key: "cwes", Label: "CWE weaknesses", Cadence: 30, Auto: true, How: "MITRE CWE",
		Items: `SELECT 'cwes', max(fetched_at)::date, NULL::int FROM twoai_cwes`},
	{Key: "news", Label: "News briefing", Cadence: 1, Auto: true, How: "GDELT and outlet feeds",
		Items: `SELECT 'stories', max(last_seen)::date, NULL::int FROM twoai_news_stories`},
	{Key: "vendor_feeds", Label: "Vendor news feeds", Cadence: 3, Auto: true, How: "each lab's own feed",
		Items: `SELECT vendor || ' ' || feed_url, last_ok::date, NULL::int FROM twoai_vendor_feeds WHERE active`},
	{Key: "incidents", Label: "AI incidents", Cadence: 1, Auto: true, How: "AI Incident Database",
		Items: `SELECT 'incidents', max(last_seen)::date, NULL::int FROM twoai_incidents`},
	{Key: "enforcement", Label: "FTC and SEC actions", Cadence: 30, Auto: true, Stage: "twoai_enforcement_watch", How: "FTC and SEC feeds",
		Items: `SELECT 'enforcement', max(first_seen)::date, NULL::int FROM twoai_enforcement_actions`},
	{Key: "agency_actions", Label: "Agency actions", Cadence: 30, Auto: true, How: "agency feeds",
		Items: `SELECT 'agency', max(first_seen)::date, NULL::int FROM twoai_agency_actions`},
	{Key: "jobs", Label: "Jobs", Cadence: 1, Auto: true, How: "job boards",
		Items: `SELECT 'jobs', max(last_seen)::date, NULL::int FROM twoai_jobs`},
	{Key: "stocks", Label: "Stock closes", Cadence: 4, Auto: true, Stage: "twoai_stocks", How: "Twelve Data",
		Items: `SELECT 'closes', max(trade_date), NULL::int FROM twoai_stock_closes`},
	{Key: "fred", Label: "Economic series", Cadence: 7, Auto: true, Stage: "twoai_fred", How: "FRED",
		Items: `SELECT series_id, checked_on, NULL::int FROM twoai_fred_series`},
	{Key: "people", Label: "People readings", Cadence: 90, Auto: true, How: "rewritten when a person's record changes",
		Items: `SELECT uid, generated_on, NULL::int FROM twoai_person_readings`},
	{Key: "companies", Label: "Company profiles", Cadence: 90, Auto: true, Stage: "twoai_companyfacts", How: "SEC EDGAR, Wikidata, company sites",
		// 184 profiles written by the org steps carry no verified_on; the
		// date their record was last written from a source stands in.
		Items: `SELECT uid, COALESCE(verified_on, updated_at::date), NULL::int FROM twoai_company_profiles`},
	{Key: "company_harvest", Label: "Company site harvest", Cadence: 3, Auto: true, Stage: "twoai_company_sites", How: "company websites",
		Items: `SELECT 'harvest', max(fetched_on), NULL::int FROM twoai_company_harvest`},
	{Key: "mcp", Label: "MCP registry", Cadence: 1, Auto: true, How: "the official MCP registry",
		Items: `SELECT 'registry', max(fetched_at)::date, NULL::int FROM twoai_mcp_servers`},
	{Key: "dc_facilities", Label: "Data centre facilities", Cadence: 7, Auto: true, How: "operator and planning records",
		Items: `SELECT 'facilities', max(last_seen)::date, NULL::int FROM twoai_dc_facilities`},
	{Key: "grid", Label: "Grid and power", Cadence: 1, Auto: true, How: "EIA grid data",
		Items: `SELECT 'grid', max(fetched_at)::date, NULL::int FROM twoai_grid_obs`},
	{Key: "research", Label: "Research papers", Cadence: 7, Auto: true, Stage: "openalex_watch", How: "OpenAlex and arXiv",
		Items: `SELECT 'works', max(last_seen)::date, NULL::int FROM twoai_works`},
	{Key: "glossary_lenses", Label: "Glossary lenses", Cadence: 180, Auto: true, How: "reviewed on each lens's own cycle",
		Items: `SELECT term_slug || ' ' || audience, GREATEST(reviewed_on, updated_at::date), review_interval_days FROM twoai_glossary_lenses`},
	{Key: "source_pages", Label: "Industry source pages", Cadence: 90, Auto: true, How: "the cited source, re-read by the site crawl",
		Items: `SELECT sp.uid, GREATEST(sp.written_on, (SELECT max(p.fetched_on) FROM twoai_site_crawl_pages p WHERE p.url = sp.url)), NULL::int
			FROM twoai_source_pages sp WHERE sp.doc IS NOT NULL`},
	{Key: "lang_pages", Label: "Language profiles", Cadence: 30, Auto: true, How: "each language's own site",
		Items: `SELECT key, written_on, NULL::int FROM twoai_lang_pages WHERE doc IS NOT NULL`},
	{Key: "onet", Label: "Occupations (O*NET)", Cadence: 120, Auto: true, Stage: "twoai_onet", How: "O*NET web services",
		Items: `SELECT 'onet', max(fetched_at)::date, NULL::int FROM twoai_onet_occupations`},
	// The page contracts, as one dataset: a page past its refresh contract
	// is stale data whatever the tables say. The 405 withdrawn-CVE
	// tombstones stay as they are by design (row 439).
	{Key: "pages", Label: "Pages past their refresh contract", Auto: true, How: "rebuilt by the run that owns the page",
		Items: `SELECT p.path, ` + twoaiLastCheckedSQL + `, (p.data->>'refresh_every_days')::int
			FROM twoai_pages p WHERE p.data ? 'refresh_every_days' AND p.kind <> 'cve-withdrawn'`},
}

type freshItem struct {
	item    string
	asOf    sql.NullTime
	cadence int
}

// twoaiFreshnessQueue returns the overdue items of a dataset, oldest first,
// for the stage that refreshes it to take ahead of new work.
func twoaiFreshnessQueue(db *sql.DB, dataset string, limit int) []string {
	rows, err := db.Query(`SELECT item FROM twoai_freshness_items WHERE dataset = $1
		ORDER BY as_of ASC NULLS FIRST LIMIT $2`, dataset, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			out = append(out, s)
		}
	}
	return out
}

func twoaiFreshnessEnsure(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_freshness (dataset text PRIMARY KEY, label text, how text, auto boolean,
		items int, overdue int, oldest date, newest date, within boolean, overdue_runs int DEFAULT 0,
		error text, checked_at timestamptz)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_freshness_items (dataset text, item text, as_of date, cadence int,
		first_overdue date DEFAULT current_date, PRIMARY KEY (dataset, item))`)
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_freshness_runs (run_at timestamptz PRIMARY KEY DEFAULT now(),
		datasets int, within int, overdue_items int, fixed int, bridge_sent boolean DEFAULT false)`)
}

// twoaiFreshnessAudit measures every dataset. preview prints and writes
// nothing.
func twoaiFreshnessAudit(db *sql.DB, preview bool) error {
	twoaiFreshnessEnsure(db)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	type result struct {
		d                    freshDataset
		items, overdue, runs int
		oldest, newest       string
		err                  string
		late                 []freshItem
	}
	var results []result
	datasets, within, overdueItems, fixed := 0, 0, 0, 0
	for _, d := range freshDatasets {
		r := result{d: d}
		rows, err := db.Query(d.Items)
		if err != nil {
			r.err = err.Error()
			fmt.Printf("twoai_freshness: %s: %v\n", d.Key, err)
			results = append(results, r)
			continue
		}
		var all []freshItem
		for rows.Next() {
			var it freshItem
			var cad sql.NullInt64
			if rows.Scan(&it.item, &it.asOf, &cad) != nil {
				continue
			}
			it.cadence = d.Cadence
			if cad.Valid && cad.Int64 > 0 {
				it.cadence = int(cad.Int64)
			}
			if it.cadence <= 0 {
				it.cadence = 30
			}
			all = append(all, it)
		}
		rows.Close()
		r.items = len(all)
		for _, it := range all {
			if it.asOf.Valid {
				s := it.asOf.Time.Format("2006-01-02")
				if r.oldest == "" || s < r.oldest {
					r.oldest = s
				}
				if s > r.newest {
					r.newest = s
				}
			}
			if !it.asOf.Valid || int(today.Sub(it.asOf.Time.UTC().Truncate(24*time.Hour)).Hours()/24) > it.cadence {
				r.late = append(r.late, it)
			}
		}
		r.overdue = len(r.late)
		datasets++
		if r.overdue == 0 {
			within++
		}
		overdueItems += r.overdue
		results = append(results, r)
		if preview {
			fmt.Printf("  %-20s %6d items, %5d overdue, oldest %s, newest %s\n", d.Key, r.items, r.overdue, r.oldest, r.newest)
			continue
		}

		// Items no longer overdue are fixed; the rest are kept with the date
		// they first went overdue.
		late := make([]string, 0, len(r.late))
		for _, it := range r.late {
			late = append(late, it.item)
			var asOf any
			if it.asOf.Valid {
				asOf = it.asOf.Time
			}
			db.Exec(`INSERT INTO twoai_freshness_items (dataset, item, as_of, cadence) VALUES ($1,$2,$3,$4)
				ON CONFLICT (dataset, item) DO UPDATE SET as_of = EXCLUDED.as_of, cadence = EXCLUDED.cadence`, d.Key, it.item, asOf, it.cadence)
		}
		var gone int
		db.QueryRow(`WITH d AS (DELETE FROM twoai_freshness_items WHERE dataset = $1 AND NOT (item = ANY($2)) RETURNING 1)
			SELECT count(*) FROM d`, d.Key, pq.Array(late)).Scan(&gone)
		fixed += gone
		db.QueryRow(`INSERT INTO twoai_freshness (dataset, label, how, auto, items, overdue, oldest, newest, within, overdue_runs, error, checked_at)
			VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,'')::date,NULLIF($8,'')::date,$6 = 0, CASE WHEN $6 > 0 THEN 1 ELSE 0 END, NULL, now())
			ON CONFLICT (dataset) DO UPDATE SET label=$2, how=$3, auto=$4, items=$5, overdue=$6, oldest=EXCLUDED.oldest, newest=EXCLUDED.newest,
				within = $6 = 0, overdue_runs = CASE WHEN $6 > 0 THEN twoai_freshness.overdue_runs + 1 ELSE 0 END, error=NULL, checked_at=now()
			RETURNING overdue_runs`,
			d.Key, d.Label, d.How, d.Auto, r.items, r.overdue, r.oldest, r.newest).Scan(&r.runs)
		results[len(results)-1].runs = r.runs
		// Queued for the next run, ahead of new work: a once-a-day stage that
		// already ran today runs again.
		if r.overdue > 0 && d.Stage != "" {
			db.Exec(`DELETE FROM pipeline_stage_runs WHERE stage = $1`, d.Stage)
		}
	}
	for _, r := range results {
		if r.err != "" && !preview {
			db.Exec(`INSERT INTO twoai_freshness (dataset, label, how, auto, error, checked_at, overdue_runs) VALUES ($1,$2,$3,$4,$5,now(),1)
				ON CONFLICT (dataset) DO UPDATE SET error=$5, checked_at=now(), overdue_runs = twoai_freshness.overdue_runs + 1`,
				r.d.Key, r.d.Label, r.d.How, r.d.Auto, r.err)
		}
	}
	fmt.Printf("twoai_freshness: %d of %d datasets within cadence, %d items overdue, %d fixed since the last run ok=true\n",
		within, datasets, overdueItems, fixed)
	if preview {
		return nil
	}
	db.Exec(`INSERT INTO twoai_freshness_runs (datasets, within, overdue_items, fixed) VALUES ($1,$2,$3,$4)`, datasets, within, overdueItems, fixed)

	// One bridge row a day.
	var sentToday bool
	db.QueryRow(`SELECT EXISTS (SELECT 1 FROM twoai_freshness_runs WHERE bridge_sent AND run_at::date = current_date)`).Scan(&sentToday)
	if sentToday || os.Getenv("TWOAI_FRESHNESS_NO_BRIDGE") != "" {
		return nil
	}
	var fixedToday int
	db.QueryRow(`SELECT COALESCE(sum(fixed),0) FROM twoai_freshness_runs WHERE run_at::date = current_date`).Scan(&fixedToday)
	var b strings.Builder
	var decide, working []string
	sort.SliceStable(results, func(i, j int) bool { return results[i].overdue > results[j].overdue })
	for _, r := range results {
		if r.err != "" {
			decide = append(decide, fmt.Sprintf("%s: could not be measured (%s)", r.d.Label, trunc(r.err, 120)))
			continue
		}
		if r.overdue == 0 {
			continue
		}
		sort.Slice(r.late, func(i, j int) bool {
			if r.late[i].asOf.Valid != r.late[j].asOf.Valid {
				return !r.late[i].asOf.Valid
			}
			return r.late[i].asOf.Time.Before(r.late[j].asOf.Time)
		})
		var ex []string
		for i, it := range r.late {
			if i == 4 {
				break
			}
			d := "never"
			if it.asOf.Valid {
				d = it.asOf.Time.Format("2006-01-02")
			}
			ex = append(ex, fmt.Sprintf("%s (%s, every %dd)", it.item, d, it.cadence))
		}
		line := fmt.Sprintf("%s: %d of %d overdue, oldest %s", r.d.Label, r.overdue, r.items, strings.Join(ex, ", "))
		if !r.d.Auto || r.runs >= 3 {
			why := "no automatic refresher"
			if r.d.Auto {
				why = fmt.Sprintf("overdue %d runs running", r.runs)
			}
			decide = append(decide, line+". "+why+", source: "+r.d.How)
		} else {
			working = append(working, line)
		}
	}
	fmt.Fprintf(&b, "Freshness audit (row 439), %s. %d of %d datasets within cadence, %d items overdue, %d fixed today.\n",
		today.Format("2006-01-02"), within, datasets, overdueItems, fixedToday)
	if len(decide) > 0 {
		b.WriteString("\nNEED A DECISION (the pipeline cannot fix these by itself):\n")
		for _, s := range decide {
			b.WriteString("- " + s + "\n")
		}
	}
	if len(working) > 0 {
		b.WriteString("\nQUEUED, refreshing on the next runs:\n")
		for _, s := range working {
			b.WriteString("- " + s + "\n")
		}
	}
	b.WriteString("\nItems in twoai_freshness_items, datasets in twoai_freshness. srj owns the refreshers; send code needs by bridge.")
	topic := fmt.Sprintf("Freshness: %d overdue, %d fixed, %d need a decision", overdueItems, fixedToday, len(decide))
	if _, err := db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body) VALUES ('srj','theworldofai',$1,$2)`, topic, b.String()); err == nil {
		db.Exec(`UPDATE twoai_freshness_runs SET bridge_sent = true WHERE run_at = (SELECT max(run_at) FROM twoai_freshness_runs)`)
	}
	return nil
}

// twoaiFreshnessLine is the public line for /data-quality/.
func twoaiFreshnessLine(db *sql.DB) string {
	var n, w int
	var at sql.NullTime
	if db.QueryRow(`SELECT datasets, within, run_at FROM twoai_freshness_runs ORDER BY run_at DESC LIMIT 1`).Scan(&n, &w, &at) != nil || n == 0 {
		return ""
	}
	return fmt.Sprintf(`<p><b>Data freshness, %s</b>: %d of %d datasets (%d%%) were within their refresh schedule, measured by the age of the data itself, not the date a page was rebuilt.</p>`,
		at.Time.UTC().Format("2 January 2006"), w, n, w*100/n)
}
