package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// THE CLAIM LAYER: WHO CLAIMED WHAT RESULT, AND WHERE.
//
// This is the part of the corpus nobody else has. Indexes of AI papers exist;
// what does not exist is a queryable table of the RESULTS those papers claim -
// this system, on this dataset, reached this number on this metric - kept
// current, dated, and traceable to the work that made the claim. It is what
// turns "we have 30,000 papers" into "we can tell you what the field claims
// about ImageNet accuracy and when each claim was made".
//
// WHAT IS STORED, AND WHY IT IS NOT COPYING. Abstracts are publisher prose
// and are held cite_only. A claim row holds NO SENTENCE FROM THE ABSTRACT:
// it holds structured fields - metric, value, unit, dataset, task, system,
// baseline - which are facts, plus the identifier of the work that made the
// claim. Facts about who said what are ours to publish; the wording is not.
// This is the same line the platform already draws between third-party prose
// and factual fields, applied to a new source. Claim rows are therefore
// trainable while the abstracts they came from remain cite_only.
//
// THE CHEAP GATE BEFORE THE EXPENSIVE STEP. Sending 19,533 abstracts to a
// model to be told most of them contain no measurement would be waste. A SQL
// prefilter finds abstracts carrying both a named metric and a digit: 1,548
// of 19,533, an eight-in-ten reduction before a single token is spent. Only
// those reach the model. The same shape as every other gate here - the
// summary floor, the case-study classifier, the Open Library filters.
//
// HONEST FAILURE. The model is asked to return an empty array when an
// abstract states no measured result, and that answer is recorded as a
// successful extraction with zero claims rather than a retry. A paper that
// claims nothing measurable is a fact about the paper, not a failure. Works
// are marked attempted either way so the stage never re-bills the same
// abstract, and the extractor version is stored so a better prompt later can
// re-run only what it needs to.

const twoaiClaimModel = "claude-haiku-4-5"
const twoaiClaimExtractor = "haiku-4-5/claims-v1"
const twoaiClaimBatch = 120 // works per run; the backfill is a marathon

type twoaiClaim struct {
	Metric    string  `json:"metric"`
	Value     float64 `json:"value"`
	Unit      string  `json:"unit"`
	Dataset   string  `json:"dataset"`
	Task      string  `json:"task"`
	System    string  `json:"system"`
	Baseline  string  `json:"baseline"`
	Direction string  `json:"direction"`
}

// twoaiExtractClaims asks for structured results and nothing else. The prompt
// is explicit that absence is an acceptable answer, because a model pushed to
// find a number in every abstract will invent one.
func twoaiExtractClaims(title, abstract string) ([]twoaiClaim, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	prompt := "Extract every measured result this abstract CLAIMS. Return a JSON array and nothing else - " +
		"no prose, no code fence.\n\n" +
		"Each element: {\"metric\":\"\",\"value\":0,\"unit\":\"\",\"dataset\":\"\",\"task\":\"\",\"system\":\"\",\"baseline\":\"\",\"direction\":\"\"}\n\n" +
		"metric: the measure named, lowercase (accuracy, f1, bleu, auc, error rate, perplexity, speedup).\n" +
		"value: the number as a number.\n" +
		"unit: %, points, x, ms, or \"\" if bare.\n" +
		"dataset: the named dataset or benchmark, \"\" if none named.\n" +
		"task: the task in three words or fewer, \"\" if unclear.\n" +
		"system: the name of the method or model making the claim, \"\" if unnamed.\n" +
		"baseline: what it is compared against, \"\" if none named.\n" +
		"direction: higher_better or lower_better.\n\n" +
		"RULES. Only results this work claims for itself - not numbers quoted about prior work, " +
		"not dataset sizes, not parameter counts, not years, not funding. " +
		"Do not infer a value that is not stated. " +
		"IF THE ABSTRACT STATES NO MEASURED RESULT, RETURN []. An empty array is a correct and " +
		"expected answer; do not manufacture a claim to avoid returning nothing.\n\n" +
		"Title: " + title + "\nAbstract: " + abstract
	body, _ := json.Marshal(map[string]any{
		"model":      twoaiClaimModel,
		"max_tokens": 900,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("anthropic %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	raw := ""
	for _, c := range out.Content {
		raw += c.Text
	}
	raw = strings.TrimSpace(raw)
	// Defensive: strip a fence if one appears despite the instruction.
	if strings.HasPrefix(raw, "```") {
		if i := strings.Index(raw, "\n"); i >= 0 {
			raw = raw[i+1:]
		}
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
	}
	if raw == "" {
		return nil, fmt.Errorf("empty response")
	}
	var claims []twoaiClaim
	if err := json.Unmarshal([]byte(raw), &claims); err != nil {
		return nil, fmt.Errorf("unparseable: %s", truncate(raw, 160))
	}
	return claims, nil
}

// twoaiClaimSane rejects extractions that are obviously wrong before they
// reach the table. A percentage above 100, a negative accuracy or a metric
// name longer than a phrase means the model drifted, and one bad row in a
// results table is worse than a missing one.
func twoaiClaimSane(c twoaiClaim) bool {
	if strings.TrimSpace(c.Metric) == "" || len(c.Metric) > 40 {
		return false
	}
	if c.Value == 0 && c.Unit == "" {
		return false
	}
	if c.Unit == "%" && (c.Value < 0 || c.Value > 100) {
		return false
	}
	if c.Value < -1e6 || c.Value > 1e9 {
		return false
	}
	return true
}

func twoaiClaims(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_claims (
		id bigserial PRIMARY KEY,
		openalex_id text NOT NULL REFERENCES twoai_works(openalex_id) ON DELETE CASCADE,
		metric text NOT NULL,
		value double precision NOT NULL,
		unit text,
		dataset text,
		task text,
		system_name text,
		baseline text,
		direction text,
		extractor text NOT NULL,
		extracted_on date NOT NULL DEFAULT current_date,
		license_class text NOT NULL DEFAULT 'derived_fact_trainable',
		UNIQUE (openalex_id, metric, value, dataset, system_name))`); err != nil {
		return fmt.Errorf("claims create table: %w", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS twoai_claims_metric ON twoai_claims (metric)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS twoai_claims_dataset ON twoai_claims (dataset) WHERE dataset <> ''`)
	// Attempt ledger: separate from the claims table because "we looked and
	// there was nothing" is information, and without it the stage would pay
	// to re-read every claimless abstract for ever.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_claim_attempts (
		openalex_id text PRIMARY KEY REFERENCES twoai_works(openalex_id) ON DELETE CASCADE,
		extractor text NOT NULL,
		claims_found int NOT NULL DEFAULT 0,
		attempted_on date NOT NULL DEFAULT current_date,
		note text)`); err != nil {
		return fmt.Errorf("claims create attempts: %w", err)
	}

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		fmt.Fprintln(os.Stderr, "twoai_claims: ANTHROPIC_API_KEY not set, nothing extracted")
		return nil
	}

	// The cheap gate. A named metric AND a digit; ordered by citations so the
	// papers the field actually reads are mined first, which matters while
	// the backfill is incomplete.
	rows, err := db.Query(`SELECT w.openalex_id, w.title, w.abstract
		FROM twoai_works w
		LEFT JOIN twoai_claim_attempts a
		  ON a.openalex_id = w.openalex_id AND a.extractor = $1
		WHERE w.abstract IS NOT NULL
		  AND a.openalex_id IS NULL
		  AND w.abstract ~* '\m(accuracy|f1|bleu|auc|precision|recall|error rate|perplexity|iou|rouge|speedup|win rate)\M'
		  AND w.abstract ~ '[0-9]'
		ORDER BY w.cited_by DESC
		LIMIT $2`, twoaiClaimExtractor, twoaiClaimBatch)
	if err != nil {
		return fmt.Errorf("claims candidate query: %w", err)
	}
	type cand struct{ id, title, abstract string }
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.title, &c.abstract) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()

	extracted, claimsSaved, empty, failed := 0, 0, 0, 0
	for _, c := range cands {
		abstract := c.abstract
		if len(abstract) > 4000 {
			abstract = abstract[:4000]
		}
		claims, err := twoaiExtractClaims(c.title, abstract)
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "twoai_claims: %s: %v\n", c.id, err)
			// Not marked attempted: a transport or parse failure is not an
			// answer, and this work should be tried again tomorrow.
			time.Sleep(400 * time.Millisecond)
			continue
		}
		kept := 0
		for _, cl := range claims {
			if !twoaiClaimSane(cl) {
				continue
			}
			if _, err := db.Exec(`INSERT INTO twoai_claims
				(openalex_id, metric, value, unit, dataset, task, system_name, baseline, direction, extractor)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
				ON CONFLICT (openalex_id, metric, value, dataset, system_name) DO NOTHING`,
				c.id, strings.ToLower(strings.TrimSpace(cl.Metric)), cl.Value, cl.Unit,
				cl.Dataset, cl.Task, cl.System, cl.Baseline, cl.Direction, twoaiClaimExtractor); err != nil {
				fmt.Fprintln(os.Stderr, "twoai_claims insert:", err)
				continue
			}
			kept++
		}
		db.Exec(`INSERT INTO twoai_claim_attempts (openalex_id, extractor, claims_found)
			VALUES ($1,$2,$3)
			ON CONFLICT (openalex_id) DO UPDATE SET extractor=EXCLUDED.extractor,
				claims_found=EXCLUDED.claims_found, attempted_on=current_date`,
			c.id, twoaiClaimExtractor, kept)
		extracted++
		claimsSaved += kept
		if kept == 0 {
			empty++
		}
		time.Sleep(250 * time.Millisecond)
	}

	var totalClaims, totalWorks, remaining int
	db.QueryRow(`SELECT count(*) FROM twoai_claims`).Scan(&totalClaims)
	db.QueryRow(`SELECT count(DISTINCT openalex_id) FROM twoai_claims`).Scan(&totalWorks)
	db.QueryRow(`SELECT count(*) FROM twoai_works w
		LEFT JOIN twoai_claim_attempts a ON a.openalex_id=w.openalex_id
		WHERE w.abstract IS NOT NULL AND a.openalex_id IS NULL
		  AND w.abstract ~* '\m(accuracy|f1|bleu|auc|precision|recall|error rate|perplexity|iou|rouge|speedup|win rate)\M'
		  AND w.abstract ~ '[0-9]'`).Scan(&remaining)
	fmt.Printf("twoai_claims: read=%d new_claims=%d no_claim=%d failed=%d | claims_total=%d works_with_claims=%d queue=%d\n",
		extracted, claimsSaved, empty, failed, totalClaims, totalWorks, remaining)
	return nil
}
