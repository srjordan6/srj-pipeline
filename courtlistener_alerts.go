package main

// DOCKET ALERTS AND WEBHOOKS, 2026-10-07 (bridge rows 526, 527 and 529).
//
// Stephen took a CourtListener Tier 1 membership on 2026-10-07, which has
// unlimited docket alerts. A docket alert makes CourtListener push every new
// entry on a docket to our webhook within seconds of the filing, so a docket
// on an alert costs nothing of the day's calls to keep current. The poll
// becomes a weekly backstop for those dockets, and the day's calls go to the
// dockets that cannot be alerted, RECAP, discovery and classification.
//
// THREE PARTS.
//
//  1. cl_alerts subscribes every active federal docket on the tracker to a
//     docket alert, most read cases first (twoai_ga_pages, the last 28 days),
//     then the refresh priority order. Each subscription is one counted POST,
//     charged to the alerts share (60 a day on Tier 1), so the ~150 dockets
//     take three days. The account's existing alerts are read first, once a
//     day, so nothing is subscribed twice and alerts made by hand count.
//     On the free tier only five alerts are allowed; CL_ALERT_LIMIT overrides.
//
//  2. The site Worker (twoai-site, POST /api/cl-webhook/<secret>) accepts the
//     webhook only from CourtListener's two published sending addresses
//     (34.210.230.218 and 54.189.59.91) at a long secret path, stores the raw
//     event in D1 table cl_webhooks keyed by its Idempotency-Key, and answers
//     at once: CourtListener gives up on a delivery after three seconds. The
//     Worker cannot write Postgres (its Hyperdrive role is read only), so D1
//     is the inbox, the same pattern as answer_log and talent signups.
//
//  3. cl_webhooks pulls new events from D1 into twoai_cl_webhooks, every run,
//     and applies each docket alert to its case exactly as the refresh would:
//     new entries merged into the timeline, latest_development moved, and
//     docket_ok_at set, since a push carries every entry the poll would have
//     read. An event for a docket the tracker does not hold (CourtListener's
//     test event uses a 2022 docket) is kept and marked, never dropped.
//
// THE BACKSTOP. A docket on an alert is polled weekly, not daily, but only
// while webhooks are demonstrably arriving: at least one in the last three
// days. If they stop, for a Cloudflare rule, a lapsed membership or a
// disabled endpoint, every docket goes back to its normal cadence on the
// next run by itself, and the daily report says when the last one came.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// Webhooks count as arriving when one came in this long ago or less.
	clWebhookLive = 3 * clDay
	// The poll interval for a docket on an alert while webhooks arrive.
	clAlertBackstop = 7 * clDay
	// Docket alerts a free account may hold.
	clFreeAlertLimit = 5
)

// clEntry is one docket entry, as the docket-entries endpoint and the docket
// alert webhook both give it.
type clEntry struct {
	Docket      json.RawMessage `json:"docket"`
	DateFiled   string          `json:"date_filed"`
	EntryNumber json.RawMessage `json:"entry_number"`
	Description string          `json:"description"`
	// THE PUSH CARRIES ITS TEXT ON THE DOCUMENT, NOT THE ENTRY. Row 564
	// (2026-10-07): results[0].description was "" on all five real pushes of
	// the first day, while recap_documents[0].description held the entry's
	// short title ("Order on Motion for Leave to File Document"). The docket
	// entries endpoint fills description; the alert payload does not yet.
	RecapDocuments []struct {
		Description    string `json:"description"`
		DocumentNumber string `json:"document_number"`
	} `json:"recap_documents"`
}

// clEntryText is the entry's description, or the first document's when the
// entry has none, so a push never writes an entry with no text.
func clEntryText(en clEntry) string {
	if d := strings.TrimSpace(en.Description); d != "" {
		return d
	}
	for _, rd := range en.RecapDocuments {
		if d := strings.TrimSpace(rd.Description); d != "" {
			return d
		}
	}
	return ""
}

// clMergeEntries adds the entries the timeline does not already hold, newest
// first, and moves latest_development to the newest entry of the merged
// timeline. It returns how many were added and the newest date.
func clMergeEntries(db *sql.DB, lawsuitID int64, timeline, caseURL string, entries []clEntry) (int, string, error) {
	var existing []map[string]any
	json.Unmarshal([]byte(timeline), &existing)
	seen := map[string]bool{}
	for _, e := range existing {
		d, _ := e["date"].(string)
		n, _ := e["doc_no"].(string)
		seen[d+"|"+n] = true
	}
	var fresh []map[string]any
	for _, en := range entries {
		desc := clEntryText(en)
		docNo := strings.Trim(string(en.EntryNumber), `"null`)
		if docNo == "" {
			for _, rd := range en.RecapDocuments {
				if rd.DocumentNumber != "" {
					docNo = rd.DocumentNumber
					break
				}
			}
		}
		date := en.DateFiled
		if len(date) > 10 {
			date = date[:10]
		}
		if date == "" || desc == "" || seen[date+"|"+docNo] {
			continue
		}
		seen[date+"|"+docNo] = true
		fresh = append(fresh, map[string]any{
			"date":   date,
			"title":  trunc(desc, 300),
			"doc_no": docNo,
			"url":    caseURL,
		})
	}
	if len(fresh) == 0 {
		return 0, "", nil
	}
	merged := append(fresh, existing...)
	sort.SliceStable(merged, func(i, j int) bool {
		di, _ := merged[i]["date"].(string)
		dj, _ := merged[j]["date"].(string)
		return di > dj
	})
	payload, _ := json.Marshal(merged)
	// The newest entry of the merged timeline, not of this page: an entry
	// modified today can have been filed long ago.
	newest := merged[0]
	if _, err := db.Exec(`UPDATE ai_lawsuits SET timeline=$1, latest_development=$2,
		latest_development_date=$3, updated_at=now() WHERE id=$4`,
		payload, newest["title"], newest["date"], lawsuitID); err != nil {
		return 0, "", err
	}
	d, _ := newest["date"].(string)
	return len(fresh), d, nil
}

func clEnsureAlerts(db *sql.DB) {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS twoai_cl_alerts (docket_id bigint PRIMARY KEY, alert_id bigint,
			lawsuit_id int, slug text, subscribed_at timestamptz NOT NULL DEFAULT now(), source text NOT NULL,
			last_push_at timestamptz, pushes int NOT NULL DEFAULT 0, seen_at timestamptz)`,
		`CREATE TABLE IF NOT EXISTS twoai_cl_webhooks (d1_rowid bigint PRIMARY KEY, idempotency_key text UNIQUE,
			received_at timestamptz, ip text, event_type int, body jsonb, pulled_at timestamptz NOT NULL DEFAULT now(),
			applied_at timestamptz, note text)`,
	} {
		if _, err := db.Exec(q); err != nil {
			fmt.Println("CourtListener alerts schema:", err)
		}
	}
}

// clAlertLimit is how many docket alerts the membership allows; 0 means no
// limit.
func clAlertLimit() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CL_ALERT_LIMIT"))); err == nil && v >= 0 {
		return v
	}
	if clTierName == "free" {
		return clFreeAlertLimit
	}
	return 0
}

// clAnyID reads a docket id that may come as a number or as an API URL.
func clAnyID(raw json.RawMessage) string {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return ""
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return s
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '/' })
	for i := len(parts) - 1; i >= 0; i-- {
		if _, err := strconv.ParseInt(parts[i], 10, 64); err == nil {
			return parts[i]
		}
	}
	return ""
}

// clSyncAlerts reads the account's docket alerts into twoai_cl_alerts, once
// a UTC day: alerts made by hand on the website count, and nothing is
// subscribed twice. It returns how many subscriptions the account holds.
func clSyncAlerts(db *sql.DB) (int, error) {
	today := clUTCDay(time.Now()).Format("2006-01-02")
	if clStateGet(db, "alerts_synced_on") == today {
		var n int
		db.QueryRow(`SELECT count(*) FROM twoai_cl_alerts`).Scan(&n)
		return n, nil
	}
	next := clAPIBase + "/docket-alerts/"
	held := 0
	for next != "" {
		var page struct {
			Next    *string `json:"next"`
			Results []struct {
				ID        int64           `json:"id"`
				Docket    json.RawMessage `json:"docket"`
				AlertType int             `json:"alert_type"`
			} `json:"results"`
		}
		if _, err := clFetch(next, &page); err != nil {
			return held, err
		}
		for _, a := range page.Results {
			did := clAnyID(a.Docket)
			if did == "" || a.AlertType != 1 {
				continue
			}
			held++
			db.Exec(`INSERT INTO twoai_cl_alerts (docket_id, alert_id, source, seen_at) VALUES ($1, $2, 'found', now())
				ON CONFLICT (docket_id) DO UPDATE SET alert_id = EXCLUDED.alert_id, seen_at = now()`, did, a.ID)
		}
		next = ""
		if page.Next != nil {
			next = *page.Next
		}
	}
	clStateSet(db, "alerts_synced_on", today)
	return held, nil
}

// clGAViews is page views per lawsuit slug over the last 28 days, from
// twoai_ga_pages. Empty when the table is not there yet.
func clGAViews(db *sql.DB) map[string]int {
	out := map[string]int{}
	rows, err := db.Query(`SELECT substring(path from '^/ai-lawsuits/([^/]+)/?$'), sum(views)
		FROM twoai_ga_pages WHERE day > current_date - 28 AND path ~ '^/ai-lawsuits/[^/]+/?$' GROUP BY 1`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var slug sql.NullString
		var n int
		if rows.Scan(&slug, &n) == nil && slug.Valid {
			out[slug.String] = n
		}
	}
	return out
}

// clAlertsStage is the cl_alerts stage: subscribe the tracked dockets.
func clAlertsStage(db *sql.DB) error {
	clUse(db, clBucketAlerts)
	clSetStageDeadline("cl_alerts", 30*time.Second)
	clEnsureAlerts(db)
	held, err := clSyncAlerts(db)
	if err != nil {
		if clIsBudget(err) {
			fmt.Println("cl_alerts: alerts share spent before the account's alerts were read, next run ok=true")
			return nil
		}
		return fmt.Errorf("reading the account's docket alerts: %w", err)
	}
	now := time.Now()
	plan, err := clDocketPlan(db, now)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	if rows, err := db.Query(`SELECT docket_id::text FROM twoai_cl_alerts`); err == nil {
		for rows.Next() {
			var d string
			if rows.Scan(&d) == nil {
				have[d] = true
			}
		}
		rows.Close()
	}
	var want []clCase
	tracked := 0
	for _, c := range plan {
		if c.DocketID == "" {
			continue
		}
		tracked++
		if !have[c.DocketID] {
			want = append(want, c)
		} else {
			db.Exec(`UPDATE twoai_cl_alerts SET lawsuit_id = $2, slug = $3 WHERE docket_id = $1 AND (lawsuit_id IS NULL OR slug IS DISTINCT FROM $3)`,
				c.DocketID, c.ID, c.Slug)
		}
	}
	// Most read first, then the refresh's own priority.
	clSortByPriority(want, now)
	views := clGAViews(db)
	sort.SliceStable(want, func(i, j int) bool { return views[want[i].Slug] > views[want[j].Slug] })
	limit := clAlertLimit()
	added, refused := 0, 0
	stopped := ""
	for _, c := range want {
		if limit > 0 && len(have)+added >= limit {
			stopped = fmt.Sprintf("the membership allows %d docket alerts", limit)
			break
		}
		var out struct {
			ID int64 `json:"id"`
		}
		status, err := clPost("/docket-alerts/", map[string]any{"docket": c.DocketID, "alert_type": 1}, &out)
		if err != nil {
			if clIsBudget(err) {
				stopped = "alerts share spent for today"
				break
			}
			// A 400 that says the alert exists is a success we did not know
			// about; anything else on this docket is logged and skipped.
			if status == http.StatusBadRequest && strings.Contains(strings.ToLower(err.Error()), "unique") {
				db.Exec(`INSERT INTO twoai_cl_alerts (docket_id, lawsuit_id, slug, source, seen_at) VALUES ($1, $2, $3, 'found', now())
					ON CONFLICT (docket_id) DO NOTHING`, c.DocketID, c.ID, c.Slug)
				continue
			}
			refused++
			fmt.Fprintln(os.Stderr, "cl_alerts", c.Slug, "subscribe:", err)
			if status == http.StatusForbidden || status == http.StatusUnauthorized {
				// The account itself was refused, not this docket: a lapsed
				// membership or a token problem. Asking again changes nothing.
				stopped = fmt.Sprintf("CourtListener refused the account (HTTP %d)", status)
				break
			}
			continue
		}
		db.Exec(`INSERT INTO twoai_cl_alerts (docket_id, alert_id, lawsuit_id, slug, source, seen_at) VALUES ($1, $2, $3, $4, 'created', now())
			ON CONFLICT (docket_id) DO UPDATE SET alert_id = EXCLUDED.alert_id, lawsuit_id = EXCLUDED.lawsuit_id, slug = EXCLUDED.slug`,
			c.DocketID, out.ID, c.ID, c.Slug)
		added++
	}
	on := 0
	for _, c := range plan {
		if c.DocketID != "" && (have[c.DocketID]) {
			on++
		}
	}
	on += added
	line := fmt.Sprintf("cl_alerts: %d of %d tracked dockets on docket alerts (%d alerts on the account), %d subscribed this run",
		on, tracked, held+added, added)
	if refused > 0 {
		line += fmt.Sprintf(", %d refused", refused)
	}
	if stopped != "" && on < tracked {
		line += ", stopped: " + stopped
	}
	fmt.Println(line + " ok=true")
	fmt.Println(clReportLine(db))
	return nil
}

// clWebhooksStage is the cl_webhooks stage: pull the Worker's inbox from D1
// and apply every docket alert not yet applied. It makes no CourtListener
// call.
func clWebhooksStage(db *sql.DB) error {
	clEnsureAlerts(db)
	pulled, err := clPullWebhooks(db)
	if err != nil {
		// The pull failing must not stop events already pulled being applied.
		fmt.Fprintln(os.Stderr, "cl_webhooks pull:", err)
	}
	applied, entries, unknown, err := clApplyWebhooks(db)
	if err != nil {
		return err
	}
	last := "none yet"
	var at sql.NullTime
	db.QueryRow(`SELECT max(received_at) FROM twoai_cl_webhooks`).Scan(&at)
	if at.Valid {
		last = at.Time.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	fmt.Printf("cl_webhooks: %d pulled from D1, %d applied (%d new docket entries, %d for dockets the tracker does not hold), last received %s ok=true\n",
		pulled, applied, entries, unknown, last)
	return nil
}

// clD1Query runs one statement against the assistant's D1 database.
func clD1Query(sqlText string, params ...any) ([]map[string]any, error) {
	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("CLOUDFLARE_API_TOKEN not set")
	}
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{"sql": sqlText, "params": params})
	u := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/d1/database/%s/query", d1AccountID, d1DatabaseID)
	req, _ := http.NewRequest("POST", u, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
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
		return nil, fmt.Errorf("d1 http %d: %s", resp.StatusCode, trunc(string(raw), 200))
	}
	if !out.Success {
		msg := fmt.Sprintf("http %d", resp.StatusCode)
		if len(out.Errors) > 0 {
			msg = out.Errors[0].Message
		}
		return nil, fmt.Errorf("d1: %s", msg)
	}
	if len(out.Result) == 0 {
		return nil, nil
	}
	return out.Result[0].Results, nil
}

// clPullWebhooks copies new rows of D1 cl_webhooks into twoai_cl_webhooks.
// The D1 table is append only, so the highest rowid copied is a complete
// watermark.
func clPullWebhooks(db *sql.DB) (int, error) {
	var since int64
	db.QueryRow(`SELECT COALESCE(max(d1_rowid), 0) FROM twoai_cl_webhooks`).Scan(&since)
	rows, err := clD1Query(`SELECT rowid AS d1_rowid, received_at, idempotency_key, ip, event_type, body
		FROM cl_webhooks WHERE rowid > ? ORDER BY rowid LIMIT 500`, since)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			// The Worker creates it with the first webhook.
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, r := range rows {
		id, ok := r["d1_rowid"].(float64)
		if !ok {
			continue
		}
		str := func(k string) any {
			if s, ok := r[k].(string); ok && s != "" {
				return s
			}
			return nil
		}
		var et any
		if f, ok := r["event_type"].(float64); ok {
			et = int(f)
		}
		body, _ := r["body"].(string)
		if !json.Valid([]byte(body)) {
			body = fmt.Sprintf(`{"unparsed": %q}`, trunc(body, 20000))
		}
		res, err := db.Exec(`INSERT INTO twoai_cl_webhooks (d1_rowid, received_at, idempotency_key, ip, event_type, body)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
			int64(id), str("received_at"), str("idempotency_key"), str("ip"), et, body)
		if err != nil {
			return n, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	return n, nil
}

// clApplyWebhooks applies every pulled docket alert not yet applied.
func clApplyWebhooks(db *sql.DB) (applied, entries, unknown int, err error) {
	rows, err := db.Query(`SELECT d1_rowid, COALESCE(event_type, 0), body::text FROM twoai_cl_webhooks
		WHERE applied_at IS NULL ORDER BY d1_rowid`)
	if err != nil {
		return 0, 0, 0, err
	}
	type ev struct {
		id   int64
		typ  int
		body string
	}
	var evs []ev
	for rows.Next() {
		var e ev
		if rows.Scan(&e.id, &e.typ, &e.body) == nil {
			evs = append(evs, e)
		}
	}
	rows.Close()
	mark := func(id int64, note string) {
		db.Exec(`UPDATE twoai_cl_webhooks SET applied_at = now(), note = $2 WHERE d1_rowid = $1`, id, note)
		applied++
	}
	for _, e := range evs {
		var w struct {
			Payload struct {
				Results []clEntry `json:"results"`
			} `json:"payload"`
			Webhook struct {
				EventType int `json:"event_type"`
			} `json:"webhook"`
		}
		if err := json.Unmarshal([]byte(e.body), &w); err != nil {
			mark(e.id, "unreadable: "+trunc(err.Error(), 200))
			continue
		}
		typ := e.typ
		if typ == 0 {
			typ = w.Webhook.EventType
		}
		if typ != 1 {
			mark(e.id, fmt.Sprintf("event type %d, not a docket alert", typ))
			continue
		}
		byDocket := map[string][]clEntry{}
		for _, en := range w.Payload.Results {
			if d := clAnyID(en.Docket); d != "" {
				byDocket[d] = append(byDocket[d], en)
			}
		}
		var notes []string
		for did, ens := range byDocket {
			var id int64
			var slug, url, timeline string
			// The docket id is the hard identifier; the case is matched on it
			// and nothing else, and the note keeps it.
			if db.QueryRow(`SELECT id, slug, courtlistener_url, COALESCE(timeline::text, '[]') FROM ai_lawsuits
				WHERE courtlistener_url ~ ('/docket/' || $1 || '(/|$)') ORDER BY is_active DESC, id LIMIT 1`, did).
				Scan(&id, &slug, &url, &timeline) != nil {
				unknown++
				notes = append(notes, "docket "+did+": not on the tracker")
				continue
			}
			n, newest, err := clMergeEntries(db, id, timeline, url, ens)
			if err != nil {
				notes = append(notes, "docket "+did+" "+slug+": "+trunc(err.Error(), 120))
				continue
			}
			db.Exec(`UPDATE ai_lawsuits SET docket_ok_at = now(), docket_checked_at = now() WHERE id = $1`, id)
			db.Exec(`UPDATE twoai_cl_alerts SET last_push_at = now(), pushes = pushes + 1 WHERE docket_id = $1`, did)
			entries += n
			notes = append(notes, fmt.Sprintf("docket %s %s: %d new of %d", did, slug, n, len(ens)))
			if n > 0 {
				fmt.Printf("cl_webhooks %s: %d new docket entries through %s\n", slug, n, newest)
			}
		}
		if len(byDocket) == 0 {
			notes = append(notes, "no docket entries in the payload")
		}
		sort.Strings(notes)
		mark(e.id, trunc(strings.Join(notes, "; "), 1000))
	}
	return applied, entries, unknown, nil
}

// clAlertedDockets returns the dockets on an alert while webhooks are
// arriving, and when the last webhook came. The map is empty when they are
// not arriving, so every docket keeps its normal cadence.
func clAlertedDockets(db *sql.DB, now time.Time) (map[string]bool, time.Time) {
	out := map[string]bool{}
	var last sql.NullTime
	if db.QueryRow(`SELECT max(received_at) FROM twoai_cl_webhooks`).Scan(&last) != nil || !last.Valid {
		return out, time.Time{}
	}
	if now.Sub(last.Time) > clWebhookLive {
		return out, last.Time
	}
	rows, err := db.Query(`SELECT docket_id::text FROM twoai_cl_alerts`)
	if err != nil {
		return out, last.Time
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if rows.Scan(&d) == nil {
			out[d] = true
		}
	}
	return out, last.Time
}

// clAlertReport is the alerts part of the daily bridge row: how many
// dockets are on alerts, whether webhooks are arriving, and the cases that
// pushed most recently.
func clAlertReport(db *sql.DB, limit int) string {
	var on, pushed int
	if db.QueryRow(`SELECT count(*), count(last_push_at) FROM twoai_cl_alerts`).Scan(&on, &pushed) != nil {
		return ""
	}
	var b strings.Builder
	var last sql.NullTime
	db.QueryRow(`SELECT max(received_at) FROM twoai_cl_webhooks`).Scan(&last)
	state := "no webhook received yet, every docket keeps its normal poll"
	if last.Valid {
		state = "last webhook " + last.Time.UTC().Format("2006-01-02 15:04") + " UTC"
		if time.Since(last.Time) > clWebhookLive {
			state += ", over three days ago, so alerted dockets are back on their normal poll"
		} else {
			state += ", alerted dockets polled weekly as a backstop"
		}
	}
	fmt.Fprintf(&b, "CourtListener docket alerts: %d dockets on alerts, %d have pushed; %s\n", on, pushed, state)
	rows, err := db.Query(`SELECT COALESCE(slug, docket_id::text), last_push_at, pushes FROM twoai_cl_alerts
		WHERE last_push_at IS NOT NULL ORDER BY last_push_at DESC LIMIT $1`, limit)
	if err == nil {
		for rows.Next() {
			var slug string
			var at time.Time
			var n int
			if rows.Scan(&slug, &at, &n) == nil {
				fmt.Fprintf(&b, "- %s, last pushed %s UTC (%d pushes)\n", slug, at.UTC().Format("2006-01-02 15:04"), n)
			}
		}
		rows.Close()
	}
	return b.String()
}
