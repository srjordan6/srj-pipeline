package main

// twoai_bill_events.go - the enactment trigger.
//
// LegiScan has always told us a bill's status. Nobody read it. The ingest
// stage put bills into pipeline.documents and the state-law pages rendered
// them as rows, so a bill that BECAME LAW looked identical on the site to one
// still sitting in committee. New Jersey's algorithmic rent-setting act was
// law for four weeks before anyone noticed. Enactment is the single most
// important event in a bill's life and it was invisible to every downstream
// system.
//
// This stage watches for the transition, not the state. It records the moment
// a bill crosses into enacted or vetoed exactly once, and that one event then
// drives four outputs that were previously four separate manual jobs:
//
//	1. the compliance page and the state law page  (published_on)
//	2. AI News                                     (news_on)
//	3. the Marky social post                       (social_on)
//	4. an alert to Stephen                         (notified_on)
//
// Each output stamps its own column, so a partial failure retries only the
// part that failed and nothing double-posts.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// LegiScan status codes. 4 and 5 are the terminal outcomes worth announcing;
// a bill that passed one chamber is not news to a compliance audience.
var twoaiBillStatusLabel = map[int]string{
	1: "Introduced", 2: "Engrossed", 3: "Enrolled",
	4: "Passed", 5: "Vetoed", 6: "Failed",
}

// Relevance gate. The AI corpus is keyword-matched at ingest, so it contains
// general appropriations acts and budget technical corrections that mention
// AI once in passing. Publishing those as AI laws would be worse than
// publishing nothing: it teaches a reader that the tracker cannot tell the
// difference. Title and description must carry real AI subject matter.
var twoaiAIRelevanceRe = regexp.MustCompile(`(?i)(artificial intelligence|\bAI\b|algorithmic|automated decision|automated employment|machine learning|deepfake|synthetic media|generative)`)

// Explicit exclusions for the shapes that survive the relevance test on a
// stray word but are not AI legislation.
var twoaiBillExcludeRe = regexp.MustCompile(`(?i)(general appropriation|budget technical correction|omnibus appropriation|supplemental appropriation|making appropriations)`)

func twoaiBillRelevant(title, desc string) (bool, string) {
	blob := title + " " + desc
	if twoaiBillExcludeRe.MatchString(blob) {
		return false, "appropriations or budget vehicle, AI mention is incidental"
	}
	if !twoaiAIRelevanceRe.MatchString(blob) {
		return false, "no AI subject matter in title or description"
	}
	return true, "AI subject matter in title or description"
}

// twoaiBillEvents detects new terminal-status transitions and records them.
// Detection is idempotent by primary key: a bill that was already recorded at
// status 4 never produces a second event, so nothing fires twice however many
// times the cron runs.
func twoaiBillEvents(db *sql.DB, today string) (int, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_bill_events (
		state text NOT NULL, bill_number text NOT NULL, status int NOT NULL,
		status_label text NOT NULL, status_date date NOT NULL,
		title text NOT NULL DEFAULT '', description text NOT NULL DEFAULT '',
		url text NOT NULL DEFAULT '', relevant boolean NOT NULL DEFAULT false,
		relevance_note text NOT NULL DEFAULT '', detected_on date NOT NULL DEFAULT current_date,
		published_on date, news_on date, social_on date, notified_on date,
		PRIMARY KEY (state, bill_number, status))`); err != nil {
		return 0, err
	}

	// Latest record per bill, terminal statuses only.
	rows, err := db.Query(`
		SELECT DISTINCT ON (raw->'bill'->>'state', raw->'bill'->>'bill_number')
		  raw->'bill'->>'state', raw->'bill'->>'bill_number',
		  (raw->'bill'->>'status')::int,
		  (raw->'bill'->>'status_date')::date,
		  title, COALESCE(raw->'bill'->>'description',''), COALESCE(raw->'bill'->>'url','')
		FROM pipeline.documents
		WHERE raw->'bill'->>'status' IS NOT NULL
		  AND (raw->'bill'->>'status')::int IN (4,5)
		  AND raw->'bill'->>'status_date' IS NOT NULL
		ORDER BY raw->'bill'->>'state', raw->'bill'->>'bill_number',
		         (raw->'bill'->>'status_date')::date DESC`)
	if err != nil {
		return 0, err
	}
	type ev struct {
		state, bill, title, desc, url string
		status                        int
		date                          time.Time
	}
	var evs []ev
	for rows.Next() {
		var e ev
		if rows.Scan(&e.state, &e.bill, &e.status, &e.date, &e.title, &e.desc, &e.url) == nil {
			evs = append(evs, e)
		}
	}
	rows.Close()

	newEvents, relevantNew := 0, 0
	for _, e := range evs {
		rel, note := twoaiBillRelevant(e.title, e.desc)
		res, err := db.Exec(`INSERT INTO twoai_bill_events
			(state, bill_number, status, status_label, status_date, title, description, url, relevant, relevance_note)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (state, bill_number, status) DO NOTHING`,
			e.state, e.bill, e.status, twoaiBillStatusLabel[e.status], e.date,
			e.title, e.desc, e.url, rel, note)
		if err != nil {
			return newEvents, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			newEvents++
			if rel {
				relevantNew++
			}
		}
	}

	var pendingPub, pendingNews, pendingSocial, pendingNotify int
	db.QueryRow(`SELECT
		count(*) FILTER (WHERE published_on IS NULL),
		count(*) FILTER (WHERE news_on IS NULL),
		count(*) FILTER (WHERE social_on IS NULL),
		count(*) FILTER (WHERE notified_on IS NULL)
		FROM twoai_bill_events WHERE relevant`).Scan(&pendingPub, &pendingNews, &pendingSocial, &pendingNotify)

	fmt.Printf("twoai_bill_events: scanned=%d new=%d relevant_new=%d pending pub=%d news=%d social=%d notify=%d\n",
		len(evs), newEvents, relevantNew, pendingPub, pendingNews, pendingSocial, pendingNotify)
	return newEvents, nil
}

// ---- Output 1: the enacted-law page --------------------------------------
//
// Renders every relevant enacted bill as one page grouped by state, so an
// enactment reaches the site the day it is detected rather than waiting for
// someone to write a page about it.

func twoaiBillEventsPublish(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	type law struct {
		State  string `json:"state"`
		Bill   string `json:"bill"`
		Title  string `json:"title"`
		Desc   string `json:"description"`
		Date   string `json:"status_date"`
		Status string `json:"status"`
		URL    string `json:"url"`
	}
	rows, err := db.Query(`SELECT state, bill_number, title, description, status_date::text, status_label, url
		FROM twoai_bill_events WHERE relevant AND status = 4
		ORDER BY status_date DESC, state, bill_number`)
	if err != nil {
		return 0, err
	}
	var laws []law
	for rows.Next() {
		var l law
		if rows.Scan(&l.State, &l.Bill, &l.Title, &l.Desc, &l.Date, &l.Status, &l.URL) == nil {
			// LegiScan titles arrive as "CA SB928: subject." - the prefix is
			// redundant once state and bill number are their own columns.
			l.Title = strings.TrimSpace(strings.TrimPrefix(l.Title, l.State+" "+l.Bill+":"))
			laws = append(laws, l)
		}
	}
	rows.Close()
	if len(laws) == 0 {
		return 0, nil
	}

	byState := map[string]int{}
	for _, l := range laws {
		byState[l.State]++
	}
	var recent []law
	for i, l := range laws {
		if i < 25 {
			recent = append(recent, l)
		}
	}

	doc := map[string]any{
		"uid": twoaiUID("section:enacted-ai-laws"), "tax": "enacted-ai-laws",
		"shape": "enacted-laws", "name": "Enacted AI Laws",
		"blurb":     "Every AI-related bill that has actually become law, detected from the legislative record on the day its status changes.",
		"generated": today, "total": len(laws), "states": len(byState),
		"laws": laws, "recent": recent,
	}
	if err := upsert("compliance/enacted-ai-laws.json", "compliance", doc); err != nil {
		return 0, err
	}
	if _, err := db.Exec(`UPDATE twoai_bill_events SET published_on = current_date
		WHERE relevant AND status = 4 AND published_on IS NULL`); err != nil {
		return 0, err
	}
	fmt.Printf("twoai_bill_events: enacted-laws page laws=%d states=%d\n", len(laws), len(byState))
	return 1, nil
}

// ---- Output 2: AI News ----------------------------------------------------
//
// Writes a news story per newly enacted law into twoai_news_stories, which the
// news archive already renders at /ai-news/{slug}/. Only bills enacted within
// the last 45 days become news: the backlog is published on the page above,
// but announcing a law from four months ago as news would be false.

func twoaiBillEventsNews(db *sql.DB, today string) (int, error) {
	rows, err := db.Query(`SELECT state, bill_number, title, description, status_date::text, url
		FROM twoai_bill_events
		WHERE relevant AND status = 4 AND news_on IS NULL
		  AND status_date >= current_date - INTERVAL '45 days'
		ORDER BY status_date DESC LIMIT 10`)
	if err != nil {
		return 0, err
	}
	type item struct{ state, bill, title, desc, date, url string }
	var items []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.state, &it.bill, &it.title, &it.desc, &it.date, &it.url) == nil {
			items = append(items, it)
		}
	}
	rows.Close()

	written := 0
	for _, it := range items {
		subject := strings.TrimSpace(strings.TrimPrefix(it.title, it.state+" "+it.bill+":"))
		subject = strings.TrimSuffix(subject, ".")
		headline := fmt.Sprintf("%s enacts %s: %s", it.state, it.bill, subject)
		slug := twoaiSlugify(headline)
		body := fmt.Sprintf(
			"%s %s became law on %s. %s The bill's full legislative record, including its votes and text versions, is on LegiScan.",
			it.state, it.bill, it.date, it.desc)
		story, _ := json.Marshal(map[string]any{
			"slug": slug, "headline": headline, "summary": body,
			"source_name": "LegiScan", "source_url": it.url,
			"published_on": it.date, "kind": "enacted-law",
			"state": it.state, "bill": it.bill,
		})
		if _, err := db.Exec(`INSERT INTO twoai_news_stories (slug, headline, story, published_on)
			VALUES ($1,$2,$3::jsonb,current_date) ON CONFLICT (slug) DO NOTHING`,
			slug, headline, string(story)); err != nil {
			return written, err
		}
		if _, err := db.Exec(`UPDATE twoai_bill_events SET news_on = current_date
			WHERE state=$1 AND bill_number=$2 AND status=4`, it.state, it.bill); err != nil {
			return written, err
		}
		written++
	}
	if written > 0 {
		fmt.Printf("twoai_bill_events: news stories written=%d\n", written)
	}
	return written, nil
}

var twoaiSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

func twoaiSlugify(s string) string {
	s = strings.ToLower(s)
	s = twoaiSlugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	return s
}

// ---- Outputs 3 and 4: social queue and the alert to Stephen ---------------
//
// Both are drafted here rather than posted here. A social post and an email
// are irreversible once sent, and an enactment misread as relevant would be
// public before anyone saw it. The stage writes the caption and the alert into
// a queue table with the source link attached; sending is a separate,
// reviewable step. The queue is the workflow.

func twoaiBillEventsQueue(db *sql.DB, today string) (int, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_bill_outbox (
		id serial PRIMARY KEY,
		channel text NOT NULL CHECK (channel IN ('social','alert')),
		state text NOT NULL, bill_number text NOT NULL,
		subject text NOT NULL DEFAULT '', body text NOT NULL,
		url text NOT NULL DEFAULT '',
		queued_on date NOT NULL DEFAULT current_date,
		sent_on date,
		UNIQUE (channel, state, bill_number))`); err != nil {
		return 0, err
	}

	rows, err := db.Query(`SELECT state, bill_number, title, description, status_date::text, url
		FROM twoai_bill_events
		WHERE relevant AND status = 4 AND (social_on IS NULL OR notified_on IS NULL)
		  AND status_date >= current_date - INTERVAL '45 days'
		ORDER BY status_date DESC LIMIT 10`)
	if err != nil {
		return 0, err
	}
	type item struct{ state, bill, title, desc, date, url string }
	var items []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.state, &it.bill, &it.title, &it.desc, &it.date, &it.url) == nil {
			items = append(items, it)
		}
	}
	rows.Close()

	queued := 0
	for _, it := range items {
		subject := strings.TrimSpace(strings.TrimPrefix(it.title, it.state+" "+it.bill+":"))
		subject = strings.TrimSuffix(subject, ".")

		social := fmt.Sprintf(
			"%s just enacted %s: %s\n\nIt became law on %s. We track every AI bill that actually passes, not just the ones that get headlines.\n\nFull record: theworldofai.org/ai-compliance/",
			it.state, it.bill, subject, it.date)
		if _, err := db.Exec(`INSERT INTO twoai_bill_outbox (channel, state, bill_number, subject, body, url)
			VALUES ('social',$1,$2,$3,$4,$5) ON CONFLICT (channel, state, bill_number) DO NOTHING`,
			it.state, it.bill, subject, social, it.url); err != nil {
			return queued, err
		}

		alert := fmt.Sprintf(
			"BILL ENACTED\n\n%s %s became law on %s.\n\nSubject: %s\n\nDescription: %s\n\nLegiScan record: %s\n\nThis was detected automatically from the legislative record. Before it goes into a client deliverable, open the LegiScan page and confirm the status and effective date against the state's own record.",
			it.state, it.bill, it.date, subject, it.desc, it.url)
		if _, err := db.Exec(`INSERT INTO twoai_bill_outbox (channel, state, bill_number, subject, body, url)
			VALUES ('alert',$1,$2,$3,$4,$5) ON CONFLICT (channel, state, bill_number) DO NOTHING`,
			it.state, it.bill, fmt.Sprintf("%s %s enacted: %s", it.state, it.bill, subject), alert, it.url); err != nil {
			return queued, err
		}

		if _, err := db.Exec(`UPDATE twoai_bill_events
			SET social_on = COALESCE(social_on, current_date), notified_on = COALESCE(notified_on, current_date)
			WHERE state=$1 AND bill_number=$2 AND status=4`, it.state, it.bill); err != nil {
			return queued, err
		}
		queued++
	}

	var unsent int
	db.QueryRow(`SELECT count(*) FROM twoai_bill_outbox WHERE sent_on IS NULL`).Scan(&unsent)
	if queued > 0 || unsent > 0 {
		fmt.Printf("twoai_bill_events: outbox queued=%d awaiting_send=%d (ANTHROPIC review required before posting)\n", queued, unsent)
	}
	if unsent > 0 && os.Getenv("TWOAI_ALERT_WEBHOOK") == "" {
		fmt.Printf("twoai_bill_events: %d alerts queued but TWOAI_ALERT_WEBHOOK is not set, so nothing was delivered\n", unsent)
	}
	return queued, nil
}
