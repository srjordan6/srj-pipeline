package main

// twoai_model_watch: notice when the models change, in either direction.
//
// Stephen, 2026-09-12, after moving four jobs off Claude onto Ollama Cloud:
// how do we know when to upgrade our models. Three questions live in that
// one, and only two are about upgrading.
//
//  1. A BETTER MODEL APPEARED. Detectable: the catalogue changes. This stage
//     records ollama.com's model list every run and says what is new, what
//     is gone, and what changed digest.
//
//  2. THE CURRENT MODEL GOT WORSE. This is the one that will actually bite,
//     and nothing else here would catch it. deepseek-v4.1-flash is a TAG,
//     not a frozen artifact: a provider can update the weights behind a tag
//     without notice, and the same prompt that passed 40 times can start
//     inventing. A digest change is the signal, when the API exposes one.
//     When it does not, the golden set below is the fallback.
//
//  3. THE WORK CHANGED. A new job, a longer payload, a harder prompt. No
//     machine notices this; a person does.
//
// THE GOLDEN SET is the part that matters most and costs least. Twenty
// payloads whose Claude-written output was read and accepted are pinned, by
// key, in twoai_model_golden. Re-running them monthly against the current
// champion produces evidence rather than a feeling: if the figure validator
// starts rejecting outputs that passed in September, something moved, and
// the pinned pairs are there to read side by side.
//
// This stage DECIDES NOTHING. It prints what changed and what the golden set
// did. Moving a job to a different model stays a human judgement made after
// reading pairs, exactly as the first move was.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func twoaiModelWatch(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_model_catalog_watch (
		name text PRIMARY KEY,
		digest text,
		size_bytes bigint,
		first_seen date NOT NULL DEFAULT current_date,
		last_seen date NOT NULL DEFAULT current_date,
		gone_on date)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_model_golden (
		job text NOT NULL, key text NOT NULL,
		pinned_on date NOT NULL DEFAULT current_date,
		note text,
		PRIMARY KEY (job, key))`); err != nil {
		return err
	}

	// ---- 1. the catalogue --------------------------------------------------
	req, _ := http.NewRequest("GET", twoaiOllamaHost()+"/api/tags", nil)
	if key := strings.TrimSpace(os.Getenv("OLLAMA_API_KEY")); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	code := resp.StatusCode
	resp.Body.Close()
	if code != 200 {
		return fmt.Errorf("ollama /api/tags: %d", code)
	}
	var tags struct {
		Models []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"models"`
	}
	if err := json.Unmarshal(b, &tags); err != nil {
		return err
	}

	var added, changed []string
	seen := map[string]bool{}
	for _, m := range tags.Models {
		seen[m.Name] = true
		var oldDigest string
		err := db.QueryRow(`SELECT digest FROM twoai_model_catalog_watch WHERE name=$1`, m.Name).Scan(&oldDigest)
		switch {
		case err != nil:
			added = append(added, m.Name)
		case oldDigest != "" && m.Digest != "" && oldDigest != m.Digest:
			// THE IMPORTANT ONE. Same tag, different weights. Every
			// comparison this site ran was against the old artifact, so the
			// evidence for using it no longer strictly applies.
			changed = append(changed, fmt.Sprintf("%s (%s -> %s)", m.Name, oldDigest[:8], m.Digest[:8]))
		}
		db.Exec(`INSERT INTO twoai_model_catalog_watch (name, digest, size_bytes)
			VALUES ($1,$2,$3)
			ON CONFLICT (name) DO UPDATE SET digest=$2, size_bytes=$3, last_seen=current_date, gone_on=NULL`,
			m.Name, m.Digest, m.Size)
	}
	// Gone: a model this site may be configured to use that the catalogue no
	// longer serves. Marked, never deleted, because the history of what we
	// ran and when is the only way to explain a page written in September.
	var goneNow []string
	grows, _ := db.Query(`SELECT name FROM twoai_model_catalog_watch WHERE gone_on IS NULL`)
	if grows != nil {
		for grows.Next() {
			var n string
			if grows.Scan(&n) == nil && !seen[n] {
				goneNow = append(goneNow, n)
			}
		}
		grows.Close()
	}
	for _, n := range goneNow {
		db.Exec(`UPDATE twoai_model_catalog_watch SET gone_on=current_date WHERE name=$1`, n)
	}

	// ---- 2. is anything we actually USE affected? --------------------------
	//
	// A new model in the catalogue is interesting. A change to a model this
	// pipeline is configured to run is urgent, and the difference should be
	// obvious in the log rather than inferred by reading a list.
	inUse := map[string]string{}
	for _, stage := range []string{"vendor_enrich", "point_briefs", "company_profiles",
		"page_readings", "sector_analysis"} {
		if twoaiLLMFor(stage) == "ollama" {
			inUse[twoaiOllamaModel(stage)] = stage
		}
	}
	var urgent []string
	for _, c := range changed {
		name := strings.SplitN(c, " ", 2)[0]
		if stage, ok := inUse[name]; ok {
			urgent = append(urgent, c+" - used by "+stage)
		}
	}
	for _, g := range goneNow {
		if stage, ok := inUse[g]; ok {
			urgent = append(urgent, g+" REMOVED from the catalogue - used by "+stage+", that stage now falls back to Claude every run")
		}
	}

	fmt.Printf("twoai_model_watch: catalogue=%d new=%d digest_changed=%d removed=%d in_use=%d\n",
		len(tags.Models), len(added), len(changed), len(goneNow), len(inUse))
	if len(added) > 0 {
		fmt.Printf("twoai_model_watch: NEW models available: %s\n", strings.Join(added, ", "))
		fmt.Println("twoai_model_watch: a new model is a candidate, not an upgrade. Run:")
		fmt.Println("  pipeline twoai_llm_compare <job> <model> 20   then read the pairs before moving anything.")
	}
	for _, u := range urgent {
		fmt.Fprintf(os.Stderr, "twoai_model_watch: ATTENTION: %s\n", u)
	}
	if len(changed) > 0 && len(urgent) == 0 {
		fmt.Printf("twoai_model_watch: digest changed on models we do not use: %s\n", strings.Join(changed, ", "))
	}

	// ---- 3. the golden set -------------------------------------------------
	//
	// Seed it once from what has already been compared and accepted. Pinning
	// by KEY rather than copying the payload is deliberate: the payload
	// should track the live data, so a golden re-run tests today's model
	// against today's inputs, which is the question actually being asked.
	var golden int
	db.QueryRow(`SELECT count(*) FROM twoai_model_golden`).Scan(&golden)
	if golden == 0 {
		res, err := db.Exec(`INSERT INTO twoai_model_golden (job, key, note)
			SELECT DISTINCT ON (job, key) job, key,
				'seeded from the first accepted comparison, ' || b_model
			FROM twoai_llm_compare WHERE b_validation = 'ok'
			ON CONFLICT DO NOTHING`)
		if err == nil {
			n, _ := res.RowsAffected()
			fmt.Printf("twoai_model_watch: golden set seeded with %d payloads from accepted comparisons\n", n)
		}
	}
	// How long since the champion was last checked against them.
	var lastCheck sql.NullString
	db.QueryRow(`SELECT max(created_at)::date::text FROM twoai_llm_compare`).Scan(&lastCheck)
	if lastCheck.Valid {
		var days int
		db.QueryRow(`SELECT current_date - $1::date`, lastCheck.String).Scan(&days)
		if days >= 30 {
			fmt.Printf("twoai_model_watch: the models have not been re-checked in %d days. Re-run the golden set:\n", days)
			fmt.Println("  pipeline twoai_llm_compare sector_analysis <current model> 20")
			fmt.Println("  then compare fabricated_figures against the last run before trusting it another month.")
		}
	}
	return nil
}
