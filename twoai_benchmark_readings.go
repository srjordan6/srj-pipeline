package main

// twoai_benchmark_readings: new benchmark entries written by Ollama from the
// maintainer's own page, never typed in.
//
// WHY. Stephen, 2026-09-21: add METR's time horizon measure, HCAST and
// RE-Bench to the Benchmarks hub. The existing 18 rows in twoai_benchmarks
// each state what a benchmark measures, what it does not, how it is scored
// and how to read a result. Those fields for a new benchmark must come from
// the maintainer's own publication, read by the model the site uses for
// every other reading, the same rule as the certification pages.
//
// HOW. A benchmark to add is a row in twoai_benchmark_candidates: slug, name,
// maintainer, section and the maintainer's URL. twoaiHarvestSources already
// fetches that URL daily into twoai_source_harvest (the candidates were added
// to its query), so this stage fetches nothing. When the harvest holds text,
// the model is asked for the fields as JSON, from the page only. A complete
// answer is inserted into twoai_benchmarks, where the existing benchmark
// build renders it like the other 18; an incomplete one is recorded on the
// candidate with the reason and tried again only when the page changes, at
// most three times per version of the page.
//
// NOTHING PARTIAL IS PUBLISHED. A candidate is not a benchmark row, so the
// Benchmarks hub never shows an entry with empty fields. Once promoted, the
// row belongs to twoai_benchmarks and its review cycle.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const benchmarkReadingSystem = `You write one reference entry for a benchmark on The World of AI, from the maintainer's own page, which is given to you as PAGE TEXT.

Rules:
- Use only what the page text says. Do not add facts, numbers, dates, model names or scores that are not in the page text.
- Plain English, for a business reader who is not a researcher. No hype, no marketing words.
- No hyphens used as dashes. Use commas or full stops.
- If the page text does not describe the benchmark well enough to fill a field truthfully, answer {"nothing": true, "why": "<one sentence>"} and nothing else.

Answer with one JSON object and nothing else, with these string fields:
"measures": what the benchmark measures, two or three sentences.
"not_measured": what it does not measure or where its results should not be relied on, one or two sentences.
"how": how tasks are built and how results are scored, two or three sentences.
"interpretation": how a reader should read a published result from it, one or two sentences.
"note": the single most useful fact for a reader, one sentence.
and "faq": an array of three objects, each {"q": "...", "a": "..."}, answered only from the page text.`

type benchReading struct {
	Nothing        bool   `json:"nothing"`
	Why            string `json:"why"`
	Measures       string `json:"measures"`
	NotMeasured    string `json:"not_measured"`
	How            string `json:"how"`
	Interpretation string `json:"interpretation"`
	Note           string `json:"note"`
	FAQ            []struct {
		Q string `json:"q"`
		A string `json:"a"`
	} `json:"faq"`
}

func twoaiBenchmarkReadings(db *sql.DB) error {
	if twoaiLLMFor("benchmark_readings") != "ollama" {
		fmt.Println("twoai_benchmark_readings: stage is not routed to ollama, skipping")
		return nil
	}
	rows, err := db.Query(`
		SELECT c.slug, c.name, c.maintainer, c.section, c.url, h.extract, h.content_hash
		FROM twoai_benchmark_candidates c
		JOIN twoai_source_harvest h ON h.url = c.url
		WHERE c.promoted_on IS NULL AND h.http_status = 200 AND h.extract <> ''
		  AND NOT EXISTS (SELECT 1 FROM twoai_benchmarks b WHERE b.slug = c.slug)
		  AND NOT (c.try_hash IS NOT DISTINCT FROM h.content_hash AND c.attempts >= 3)
		ORDER BY c.slug`)
	if err != nil {
		return err
	}
	type job struct{ slug, name, maintainer, section, url, extract, hash string }
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.slug, &j.name, &j.maintainer, &j.section, &j.url, &j.extract, &j.hash) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()

	promoted, declined, failed := 0, 0, 0
	note := func(j job, why string) {
		db.Exec(`UPDATE twoai_benchmark_candidates SET
				attempts = CASE WHEN try_hash IS NOT DISTINCT FROM $2 THEN attempts + 1 ELSE 1 END,
				try_hash = $2, last_note = $3, updated_at = now()
			WHERE slug = $1`, j.slug, j.hash, why)
	}
	for _, j := range jobs {
		extract := j.extract
		if len(extract) > 14000 {
			extract = extract[:14000]
		}
		user := "Benchmark as this site will list it: " + j.name + "\nMaintainer: " + j.maintainer +
			"\nMaintainer's page: " + j.url + "\n\nPAGE TEXT:\n" + extract
		reply, model, gerr := twoaiGenerate("benchmark_readings", benchmarkReadingSystem, user)
		if gerr != nil {
			fmt.Fprintf(os.Stderr, "twoai_benchmark_readings: %s: %v\n", j.slug, gerr)
			if strings.Contains(gerr.Error(), "marked down") || strings.Contains(gerr.Error(), "unreachable") {
				break
			}
			failed++
			note(j, "generate: "+gerr.Error())
			continue
		}
		s := strings.TrimSpace(reply)
		if i := strings.Index(s, "{"); i > 0 {
			s = s[i:]
		}
		if i := strings.LastIndex(s, "}"); i >= 0 && i < len(s)-1 {
			s = s[:i+1]
		}
		var r benchReading
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			failed++
			note(j, "not JSON: "+err.Error())
			continue
		}
		if r.Nothing {
			declined++
			note(j, "model: "+r.Why)
			continue
		}
		if r.Measures == "" || r.NotMeasured == "" || r.How == "" || len(r.FAQ) == 0 {
			failed++
			note(j, "incomplete reading")
			continue
		}
		faq, _ := json.Marshal(r.FAQ)
		var sort int
		db.QueryRow(`SELECT COALESCE(max(sort),0)+1 FROM twoai_benchmarks WHERE section = $1`, j.section).Scan(&sort)
		if _, err := db.Exec(`
			INSERT INTO twoai_benchmarks (slug, name, section, maintainer, url, measures, not_measured, how,
				note, interpretation, faq, sort, updated_at, last_reviewed, review_interval_days)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,now(),current_date,90)
			ON CONFLICT (slug) DO NOTHING`,
			j.slug, j.name, j.section, j.maintainer, j.url, r.Measures, r.NotMeasured, r.How,
			r.Note, r.Interpretation, faq, sort); err != nil {
			failed++
			note(j, "insert: "+err.Error())
			continue
		}
		db.Exec(`UPDATE twoai_benchmark_candidates SET promoted_on = current_date, model = $2,
			try_hash = $3, last_note = 'promoted', updated_at = now() WHERE slug = $1`, j.slug, model, j.hash)
		promoted++
		fmt.Printf("twoai_benchmark_readings: %s <- %s\n", j.slug, model)
	}
	var waiting int
	db.QueryRow(`SELECT count(*) FROM twoai_benchmark_candidates WHERE promoted_on IS NULL`).Scan(&waiting)
	fmt.Printf("twoai_benchmark_readings: promoted=%d declined=%d failed=%d still_waiting=%d ok=true\n",
		promoted, declined, failed, waiting)
	return nil
}
