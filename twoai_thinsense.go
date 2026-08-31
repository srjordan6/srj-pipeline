package main

// SENSE-MAKING FOR HARVESTED FACTS. Stephen's point on 2026-08-30: facts
// without a reading are a spec sheet, not this site's thesis. The thin-page
// cron therefore carries the same interpretation layer the Grid Observatory
// and the company profiles use: Claude reads ONLY the harvested facts and
// the context this site already holds, writes a short plain-English reading,
// and the result is cached by data hash in twoai_industry_analysis so the
// model runs again only when the facts change. Every rendered passage names
// the model and the date.
//
// Today: one reading per enriched facility. Company law exposure and MCP
// package readings are deterministic and live in the page builder and the
// templates, because a rule is more honest than a model where a rule exists.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Readings for every facility that has specifications and no reading yet.
// The hash cache means a steady state costs nothing; the first run after a
// big scrape is the expensive one, which is the run that should be.
var thinSenseCap = thinBudget("SENSE", 400)

const thinFacilitySystem = `You write for The World of AI, a reference site whose thesis is that every AI capability is downstream of compute, compute is downstream of buildings and power, and the grid is now the binding constraint on AI scaling.

You are given the published specifications of one data center campus, as structured facts read from the operator's own spec page, plus context this site holds about the state it sits in. Write a reading of what those facts mean, in three short paragraphs of plain English, for a reader deciding whether this campus matters to AI.

Rules, in order:
1. Use only the facts and context provided. Never add a fact about the operator, the site, tenants, pricing, or the grid that is not in the data. If something is not published, say it is not published.
2. When you translate megawatts into racks or accelerators, state the assumption explicitly as an assumption (for example: at 40 kW per rack, a common AI training density, 25 MW of IT load is roughly 600 racks). Show the arithmetic once. Do not present the result as the operator's claim.
3. Read the certifications for what they qualify the campus to host, in one sentence each at most, without inventing which tenants are there.
4. Place the campus in its state using only the state context given: rank, share of published capacity, number of mapped facilities.
5. Plain declarative sentences, one idea each. Commas rather than dashes. No marketing language, no superlatives you cannot derive from the numbers, no hedging filler.
6. Do not repeat the spec table. Interpret it.
Return the three paragraphs only, separated by blank lines, no heading, no preamble.`

const thinPageSystem = `You write for The World of AI, a reference site whose thesis is that every AI capability is downstream of compute, compute is downstream of buildings and power, and the grid is now the binding constraint on AI scaling.

You are given the DATA behind one page of that site, as JSON, and the URL it publishes at. The page renders these facts already. Your job is the paragraph the facts cannot write themselves: what a reader should take from them.

Rules, in order:
1. Use ONLY the data given. Never add a company, number, date, law, or event that is not in it. If the data is thin, say what it does and does not cover rather than padding.
2. Lead with the answer. The first sentence must state the single most useful thing this data says, in under 40 words, so it stands alone if quoted.
3. Then two or three short paragraphs of interpretation: what the pattern is, why it matters for AI specifically, and what it does not tell the reader. Connect to compute, buildings, or power when the data genuinely supports it, and not when it does not.
4. Plain declarative sentences, one idea each. Commas rather than dashes. No marketing language, no superlatives you cannot derive from the data, no filler.
5. Never describe the page or the site. Do not write "this page shows" or "this section lists". Write about the subject.
Return the paragraphs only, separated by blank lines, no heading, no preamble.`

// THE GENERIC READING. Stephen, 2026-08-30: Sonnet is supposed to take care
// of this. He is right, and the thin-words queue had no filler at all.
//
// 276 pages render under 300 words. They are not empty: a state data center
// directory with forty rows, a repository section with eight repos, a laws
// page with its bills, all facts and no reading. The facility readings
// already proved the shape, so this generalises it to any page in the queue.
//
// The guardrails are the same, and they matter more here because the input
// is a whole page document rather than one campus: the model sees only that
// document, is told to name nothing outside it, and every reading is cached
// on a hash of the data so it regenerates only when the facts change. The
// publishers merge it into the page at publish time, so no page builder
// needs to know this exists.
func twoaiThinSensePages(db *sql.DB) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		fmt.Println("thinpages: page readings: ANTHROPIC_API_KEY not set, skipped")
		return
	}
	model := os.Getenv("TWOAI_ANALYSIS_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	due := thinDue(db, "thin-words", thinBudget("PAGEREAD", 400))
	if len(due) == 0 {
		return
	}
	written, cached, skipped, failed := 0, 0, 0, 0
	for _, c := range due {
		// The queue holds URLs; the data behind them lives in twoai_pages.
		// A URL with no row is a static page or a route this cron cannot
		// improve, and says so rather than being retried forever.
		var path, data string
		err := db.QueryRow(`SELECT path, data::text FROM twoai_pages
			WHERE $1 IN (
				'https://theworldofai.org/ai-ecosystem/technology-and-core-infrastructure/' || (data->>'uid') || '/',
				'https://theworldofai.org/ai-ecosystem/' || (data->>'slug') || '/',
				'https://theworldofai.org/ai-tools/' || (data->>'slug') || '/',
				'https://theworldofai.org/ai-glossary/' || (data->>'slug') || '/',
				'https://theworldofai.org/ai-laws/' || (data->>'slug') || '/',
				'https://theworldofai.org/companies/' || (data->>'uid') || '/'
			) LIMIT 1`, c.ref).Scan(&path, &data)
		if err != nil {
			skipped++
			thinAttempt(db, c.path, "no twoai_pages row resolves to this URL; not a data-driven page")
			continue
		}
		if len(data) > 60000 { // a reading needs the shape, not every row
			data = data[:60000] + "\n... (truncated)"
		}
		h := sha256.Sum256([]byte(data))
		hash := hex.EncodeToString(h[:8])
		metric := "page:" + path
		var exists int
		db.QueryRow(`SELECT 1 FROM twoai_industry_analysis WHERE metric=$1 AND data_hash=$2`, metric, hash).Scan(&exists)
		if exists == 1 {
			cached++
			db.Exec(`DELETE FROM twoai_thin_queue WHERE path=$1`, c.path)
			continue
		}
		body, err := twoaiClaudeCall(model, thinPageSystem,
			"The page publishes at "+c.ref+"\n\nIts data:\n"+data+"\n\nWrite the reading now.")
		if err != nil || len(body) < 200 {
			failed++
			thinAttempt(db, c.path, fmt.Sprintf("reading failed: %v (len %d)", err, len(body)))
			continue
		}
		if _, err := db.Exec(`INSERT INTO twoai_industry_analysis (metric, data_hash, model, body, generated_on)
			VALUES ($1,$2,$3,$4,current_date) ON CONFLICT (metric, data_hash) DO NOTHING`,
			metric, hash, model, body); err != nil {
			thinAttempt(db, c.path, "db: "+err.Error())
			continue
		}
		db.Exec(`DELETE FROM twoai_thin_queue WHERE path=$1`, c.path)
		written++
		time.Sleep(1200 * time.Millisecond)
	}
	fmt.Printf("thinpages: page readings written=%d cached=%d not-data-driven=%d failed=%d of %d due\n",
		written, cached, skipped, failed, len(due))
}

func twoaiThinSense(db *sql.DB) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		fmt.Println("thinpages: sense: ANTHROPIC_API_KEY not set, readings skipped")
		return
	}
	model := os.Getenv("TWOAI_ANALYSIS_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	rows, err := db.Query(`SELECT id, name, operator, city, COALESCE(state,''), COALESCE(country,'US'),
			COALESCE(critical_it_mw,0), profile::text
		FROM twoai_dc_facilities WHERE status='enriched' AND profile<>'{}'::jsonb ORDER BY id`)
	if err != nil {
		fmt.Println("thinpages: sense:", err)
		return
	}
	type fac struct {
		id, name, op, city, state, country, profile string
		mw                                          float64
	}
	var facs []fac
	for rows.Next() {
		var f fac
		if rows.Scan(&f.id, &f.name, &f.op, &f.city, &f.state, &f.country, &f.mw, &f.profile) == nil {
			facs = append(facs, f)
		}
	}
	rows.Close()
	written, cached, failed := 0, 0, 0
	for _, f := range facs {
		if written >= thinSenseCap {
			break
		}
		h := sha256.Sum256([]byte(f.profile + fmt.Sprintf("|%.1f", f.mw)))
		hash := hex.EncodeToString(h[:8])
		metric := "dc-fac-" + f.id
		var exists int
		db.QueryRow(`SELECT 1 FROM twoai_industry_analysis WHERE metric=$1 AND data_hash=$2`, metric, hash).Scan(&exists)
		if exists == 1 {
			cached++
			continue
		}
		// State context from this site's own registry: how many facilities
		// are mapped there, how much capacity operators publish, and where
		// this campus ranks among the ones with a published figure.
		var mapped, published int
		var stateMW float64
		var rank int
		db.QueryRow(`SELECT count(*), count(*) FILTER (WHERE critical_it_mw>0), COALESCE(sum(critical_it_mw),0)
			FROM twoai_dc_facilities WHERE country=$1 AND state=$2`, f.country, f.state).Scan(&mapped, &published, &stateMW)
		db.QueryRow(`SELECT 1+count(*) FROM twoai_dc_facilities WHERE country=$1 AND state=$2 AND critical_it_mw > $3`,
			f.country, f.state, f.mw).Scan(&rank)
		var prof map[string]any
		json.Unmarshal([]byte(f.profile), &prof)
		payload, _ := json.MarshalIndent(map[string]any{
			"facility":      f.name,
			"operator":      f.op,
			"city":          f.city,
			"state":         f.state,
			"country":       f.country,
			"published_specifications": prof,
			"state_context": map[string]any{
				"facilities_mapped_in_state":            mapped,
				"facilities_with_published_mw_in_state": published,
				"published_mw_in_state_total":           stateMW,
				"this_campus_rank_by_published_mw":      rank,
				"note": "Mapped facilities come from OpenStreetMap; published MW comes only from operator spec pages this site has read. Most mapped facilities publish no figure.",
			},
		}, "", "  ")
		body, err := twoaiClaudeCall(model, thinFacilitySystem, "The data:\n"+string(payload)+"\n\nWrite the reading now.")
		if err != nil || len(body) < 200 {
			failed++
			fmt.Printf("thinpages: sense %s: %v (len %d)\n", f.id, err, len(body))
			continue
		}
		if _, err := db.Exec(`INSERT INTO twoai_industry_analysis (metric, data_hash, model, body, generated_on)
			VALUES ($1,$2,$3,$4,current_date) ON CONFLICT (metric, data_hash) DO NOTHING`,
			metric, hash, model, body); err == nil {
			written++
		}
		time.Sleep(1500 * time.Millisecond)
	}
	fmt.Printf("thinpages: sense: facility readings written=%d cached=%d failed=%d of %d enriched\n",
		written, cached, failed, len(facs))
}
