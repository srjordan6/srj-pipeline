package main

// vendor_notes: writes "What this could mean for readers of The World of AI"
// for vendor announcements, using the cheapest model in the stack.
//
// WHY A SEPARATE STAGE. The note is the site's own reading, not a fact from a
// source, so it is generated once and stored rather than re-derived at build
// time. Stored means it is stable, citable, and reviewable: a note that changed
// every night would make the page unquotable and would hide a bad note behind
// a good one the next day.
//
// COST. claude-haiku-4-5, the same model email_route already uses, is the
// cheapest available here. Only posts that will actually get a page are
// considered, which is roughly 1,956 of 4,856 rather than all of them, and the
// per-run cap spreads the backfill across nightly runs instead of spending it
// in one burst. Steady state is a handful of new posts a day.
//
// GROUNDING. The prompt is given the vendor's own published summary and
// nothing else, and is told to reason only from it. It is explicitly permitted
// to return NONE, which is the important part: a post with nothing worth
// saying should produce no section rather than filler, and filler is what an
// unconstrained model produces when asked to find significance in a routine
// changelog entry.
//
// A note is written once. Rows already carrying one are skipped, so re-running
// is free and a hand-written note (the Diagram-MMU one) is never overwritten
// by a generated one.

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

// Posts per run. The backfill is ~1,950 notes, so this clears it in about a
// week of nightly runs while keeping any single run's spend bounded and
// interruptible. Raise it to backfill faster.
const vnPerRun = 300

// Below this the vendor's summary is too thin to reason from, and it is also
// the threshold the archive query uses to decide a post gets a page at all.
const vnSummaryFloor = 120

const vnPrompt = `You write one short section for The World of AI, an atlas of artificial intelligence published by SRJ Consulting & Services. The section is titled "What this could mean for readers of The World of AI" and appears below a vendor's own announcement.

House style: plain English, commas rather than dashes, no em-dashes, no marketing language, no exclamation. Address the reader directly but never as "you guys" or similar. Two short paragraphs, 60 to 110 words in total.

What the section is for: telling a practitioner what changes for them, what to be sceptical of, and what it does not prove. Prefer the concrete over the sweeping. A capability demonstrated is not a capability deployed, and a vendor's framing is not a finding.

Hard rules:
- Reason ONLY from the announcement text given. Invent no numbers, dates, customers, prices, or comparisons.
- Do not restate the announcement. The reader has just read it.
- Do not predict market outcomes or say anything about stock, valuation, or competitive position.
- If the announcement is routine, trivial, or too thin to say anything honest about, reply with exactly: NONE

Reply with the section text only, no heading, no preamble.`

func vnGenerate(key, vendor, title, summary string) (string, error) {
	user := fmt.Sprintf("Vendor: %s\nHeadline: %s\n\nThe vendor's own description:\n%s", vendor, title, summary)
	body, _ := json.Marshal(map[string]any{
		"model":      "claude-haiku-4-5",
		"max_tokens": 320,
		"system":     vnPrompt,
		"messages":   []map[string]string{{"role": "user", "content": user}},
	})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("anthropic: %d %s", resp.StatusCode, string(raw[:min(len(raw), 200)]))
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Content) == 0 {
		return "", fmt.Errorf("anthropic response unusable")
	}
	return strings.TrimSpace(out.Content[0].Text), nil
}

// vnUsable rejects the failure modes that would otherwise reach the site: the
// model declining, a stub, or a wall of text. Length is checked in words rather
// than characters because the brief is written in words.
func vnUsable(s string) bool {
	if s == "" || strings.EqualFold(strings.TrimRight(s, "."), "NONE") {
		return false
	}
	if strings.Contains(s, "NONE") && len(s) < 40 {
		return false
	}
	w := len(strings.Fields(s))
	return w >= 35 && w <= 200
}

func vendorNotes(db *sql.DB) error {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		fmt.Println("vendor_notes: ANTHROPIC_API_KEY not set, skipped")
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE twoai_vendor_posts ADD COLUMN IF NOT EXISTS reader_note text`); err != nil {
		return err
	}

	// Newest first: a note on today's announcement is worth more than one on a
	// post from eighteen months ago, and if the backfill is ever stopped part
	// way the half that got done is the half that matters.
	rows, err := db.Query(`SELECT slug, vendor, title, summary
		FROM twoai_vendor_posts
		WHERE reader_note IS NULL
		  AND length(summary) >= $1
		ORDER BY posted_on DESC NULLS LAST, slug
		LIMIT $2`, vnSummaryFloor, vnPerRun)
	if err != nil {
		return err
	}
	type cand struct{ slug, vendor, title, summary string }
	todo := []cand{}
	for rows.Next() {
		var c cand
		if rows.Scan(&c.slug, &c.vendor, &c.title, &c.summary) == nil {
			todo = append(todo, c)
		}
	}
	rows.Close()

	var remaining int
	db.QueryRow(`SELECT count(*) FROM twoai_vendor_posts
		WHERE reader_note IS NULL AND length(summary) >= $1`, vnSummaryFloor).Scan(&remaining)

	written, declined, failed := 0, 0, 0
	for _, c := range todo {
		note, err := vnGenerate(key, c.vendor, c.title, c.summary)
		if err != nil {
			fmt.Fprintln(os.Stderr, "vendor_notes:", c.slug, err)
			failed++
			// A run of failures is a broken key or a rate limit, not bad luck.
			// Stop rather than burning the whole batch against the same wall.
			if failed >= 10 {
				fmt.Fprintln(os.Stderr, "vendor_notes: 10 consecutive failures, stopping this run")
				break
			}
			continue
		}
		failed = 0
		if !vnUsable(note) {
			// Declining is a valid answer and must be recorded, or every run
			// retries the same thin posts forever. The empty string is the
			// marker: not NULL, so it is not picked up again, and the template
			// renders nothing for it.
			if _, err := db.Exec(`UPDATE twoai_vendor_posts SET reader_note = '' WHERE slug = $1`, c.slug); err != nil {
				fmt.Fprintln(os.Stderr, "vendor_notes decline:", err)
			}
			declined++
			continue
		}
		if _, err := db.Exec(`UPDATE twoai_vendor_posts SET reader_note = $1 WHERE slug = $2`, note, c.slug); err != nil {
			fmt.Fprintln(os.Stderr, "vendor_notes write:", err)
			continue
		}
		written++
	}

	fmt.Printf("vendor_notes: written=%d declined=%d failed=%d of %d attempted, %d still without a note\n",
		written, declined, failed, len(todo), remaining-written-declined)
	return nil
}
