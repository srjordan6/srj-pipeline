package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// THE QUESTIONS READERS ASK ARE THE SITE'S BACKLOG, AND THEY WERE INVISIBLE.
//
// The site assistant logs every question to answer_log in D1: the question,
// whether it was answered, the best match score, the page it matched, the
// guard verdict, the model used. That table is the trace layer of the system
// - and nothing ever read it. It sat in D1 where no analysis, no report and
// no SQL query on this side could see it.
//
// This stage mirrors it into Postgres as twoai_ask_log, the same D1 REST
// pattern talent_pull uses. Once here, one query answers the question that
// actually matters for an audience-driven site: what do visitors ask that
// the site cannot answer yet? Unanswered questions, grouped and counted, are
// a ranked list of pages worth writing, straight from the people the site is
// for.
//
// The pull is INCREMENTAL BY ROWID and rows are never updated after insert:
// answer_log is append-only on the Worker side, so the highest rowid already
// copied is a complete high-water mark. Re-running is always safe.
func askLogPull(db *sql.DB) error {
	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	if token == "" {
		return fmt.Errorf("ask_pull: CLOUDFLARE_API_TOKEN not set")
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_ask_log (
		d1_rowid bigint PRIMARY KEY,
		asked_at text,
		question text NOT NULL,
		question_norm text,
		answered boolean NOT NULL DEFAULT false,
		best_score double precision,
		top_url text,
		guard_verdict text,
		guard_categories text,
		model_used text,
		model_errors text,
		pulled_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("ask_pull create table: %w", err)
	}

	var since int64
	if err := db.QueryRow(`SELECT COALESCE(max(d1_rowid),0) FROM twoai_ask_log`).Scan(&since); err != nil {
		return fmt.Errorf("ask_pull watermark: %w", err)
	}

	// rowid is SQLite's implicit key; ts may or may not exist as a column in
	// answer_log (the Worker's schema grew by ALTER TABLE), so it is read
	// defensively via the * projection and picked out by name if present.
	body, _ := json.Marshal(map[string]any{
		"sql":    "SELECT rowid AS d1_rowid, * FROM answer_log WHERE rowid > ? ORDER BY rowid LIMIT 2000",
		"params": []any{since},
	})
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/d1/database/%s/query", d1AccountID, d1DatabaseID)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("ask_pull d1 query: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("ask_pull d1 http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Result []struct {
			Results []map[string]any `json:"results"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("ask_pull d1 parse: %w", err)
	}
	if !out.Success {
		msg := "unknown"
		if len(out.Errors) > 0 {
			msg = out.Errors[0].Message
		}
		return fmt.Errorf("ask_pull d1 error: %s", msg)
	}
	rows := []map[string]any{}
	if len(out.Result) > 0 {
		rows = out.Result[0].Results
	}

	str := func(v any) any {
		if v == nil {
			return nil
		}
		s := fmt.Sprintf("%v", v)
		if s == "" {
			return nil
		}
		return s
	}
	num := func(v any) any {
		if f, ok := v.(float64); ok {
			return f
		}
		return nil
	}
	boolish := func(v any) bool {
		if f, ok := v.(float64); ok {
			return f != 0
		}
		return false
	}

	saved := 0
	for _, r := range rows {
		rowid, ok := r["d1_rowid"].(float64)
		if !ok {
			continue
		}
		q, _ := r["question"].(string)
		if q == "" {
			continue
		}
		if _, err := db.Exec(`INSERT INTO twoai_ask_log
			(d1_rowid, asked_at, question, question_norm, answered, best_score,
			 top_url, guard_verdict, guard_categories, model_used, model_errors)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (d1_rowid) DO NOTHING`,
			int64(rowid), str(r["ts"]), q, str(r["question_norm"]), boolish(r["answered"]),
			num(r["best_score"]), str(r["top_url"]), str(r["guard_verdict"]),
			str(r["guard_categories"]), str(r["model_used"]), str(r["model_errors"])); err != nil {
			fmt.Fprintln(os.Stderr, "ask_pull insert:", err)
			continue
		}
		saved++
	}

	var total, unanswered int
	db.QueryRow(`SELECT count(*), count(*) FILTER (WHERE NOT answered) FROM twoai_ask_log`).Scan(&total, &unanswered)
	fmt.Printf("ask_pull: new=%d total=%d unanswered=%d\n", saved, total, unanswered)
	return nil
}
