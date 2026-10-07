package main

// COURTLISTENER ON THE FIVE-MINUTE TICK, theworldofai row 564 (2026-10-07).
//
// The webhook path worked end to end on its first day: five real docket
// alerts reached D1 between 19:40 and 21:18 UTC. Nothing read them, because
// cl_webhooks ran only inside the daily all run, so a push that CourtListener
// delivered in seconds would have waited until 10:00 the next morning. The
// pull and the apply now run on inkbox_tick, every five minutes, and the
// alert emails that Stephen forwards to the inkbox mailbox (row 565) are
// applied by the same merge, so the two paths cover each other.
//
// A change to a case does not reach a reader until the site rebuilds. The
// tick rebuilds the tracker page itself - lawsuits/lawsuits.json, published
// to R2, then the twoai deploy hook - at most once an hour, and never while a
// scheduled run holds the pipeline lock, because the publish bundles every
// page and a half-built page must not ship. A change the tick could not ship
// stays pending and ships on the next tick that can, or with the next
// scheduled run, whichever comes first.

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// Webhook-triggered builds are capped at one an hour (row 564 ask 1).
	clWebhookBuildEvery  = time.Hour
	clStateChangePending = "tick_change_pending"
	clStateBuildAt       = "tick_build_at"
)

// clTick pulls new pushes from D1 and applies them. It is quiet when there is
// nothing, so the runner keeps the tick out of the log.
func clTick(db *sql.DB) {
	if os.Getenv("CLOUDFLARE_API_TOKEN") == "" {
		return
	}
	clEnsureAlerts(db)
	pulled, err := clPullWebhooks(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cl_tick pull:", err)
	}
	applied, entries, unknown, err := clApplyWebhooks(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cl_tick apply:", err)
		return
	}
	if pulled > 0 || applied > 0 {
		fmt.Printf("cl_tick: %d pulled, %d applied, %d new docket entries, %d for dockets the tracker does not hold\n",
			pulled, applied, entries, unknown)
	}
	if entries > 0 {
		clNoteChange(db, fmt.Sprintf("webhook: %d entries", entries))
	}
}

// clNoteChange records that a case changed and the tracker page is stale.
func clNoteChange(db *sql.DB, why string) {
	clStateSet(db, clStateChangePending, time.Now().UTC().Format(time.RFC3339)+" "+why)
}

// pipelineLockPath is the runner's lock for the heavy runs. The tick has its
// own lock, so this file present and fresh means a scheduled run is building.
func pipelineLockPath() string {
	if p := strings.TrimSpace(os.Getenv("PIPELINE_LOCK")); p != "" {
		return p
	}
	return `C:\srj-data\pipeline.lock`
}

// clBuildIfPending rebuilds and ships the tracker page when a push or an
// alert email changed a case, within the hourly cap and outside heavy runs.
func clBuildIfPending(db *sql.DB) {
	pending := clStateGet(db, clStateChangePending)
	if pending == "" {
		return
	}
	if last := clStateGet(db, clStateBuildAt); last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil && time.Since(t) < clWebhookBuildEvery {
			return
		}
	}
	if st, err := os.Stat(pipelineLockPath()); err == nil && time.Since(st.ModTime()) < 3*time.Hour {
		fmt.Println("cl_tick: a scheduled run holds the pipeline lock; the tracker rebuild waits for it (the run ships the change itself)")
		return
	}
	if err := clWebhookBuild(db); err != nil {
		fmt.Fprintln(os.Stderr, "cl_tick build:", err)
		return
	}
	clStateSet(db, clStateBuildAt, time.Now().UTC().Format(time.RFC3339))
	clStateSet(db, clStateChangePending, "")
	fmt.Printf("cl_tick: tracker page rebuilt and deploy triggered (%s)\n", pending)
}

// clWebhookBuild refreshes lawsuits/lawsuits.json, publishes the bundle and
// fires the twoai deploy hook.
func clWebhookBuild(db *sql.DB) error {
	now := time.Now()
	today := now.Format("2006-01-02")
	n, err := twoaiLawsuitsPage(db, today, twoaiPageUpsert(db, now.Format(time.RFC3339), today))
	if err != nil {
		return fmt.Errorf("lawsuits page: %w", err)
	}
	if err := twoaiPublishR2(db); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	hook := strings.TrimSpace(os.Getenv("TWOAI_DEPLOY_HOOK"))
	if hook == "" {
		return fmt.Errorf("TWOAI_DEPLOY_HOOK not set: %d cases published to R2, the next scheduled deploy ships them", n)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Post(hook, "application/json", nil)
	if err != nil {
		return fmt.Errorf("deploy hook: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("deploy hook returned %d", resp.StatusCode)
	}
	return nil
}

// clWebhookCounts is the line the daily freshness row carries (row 564 ask
// 4): pushes held in D1 against pushes pulled and applied, so a pull gap is
// visible the day it opens, and the alert emails beside them.
func clWebhookCounts(db *sql.DB) string {
	var pulled, applied int
	var last sql.NullTime
	db.QueryRow(`SELECT count(*), count(applied_at), max(received_at) FROM twoai_cl_webhooks`).Scan(&pulled, &applied, &last)
	inD1, d1Note := -1, ""
	if os.Getenv("CLOUDFLARE_API_TOKEN") != "" {
		if rows, err := clD1Query(`SELECT count(*) AS n FROM cl_webhooks`); err == nil && len(rows) > 0 {
			if f, ok := rows[0]["n"].(float64); ok {
				inD1 = int(f)
			}
		} else if err != nil {
			d1Note = " (D1 count unavailable: " + trunc(err.Error(), 80) + ")"
		}
	}
	lastS := "none yet"
	if last.Valid {
		lastS = last.Time.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	pendingS := "unknown"
	if inD1 >= 0 {
		pendingS = fmt.Sprintf("%d", inD1-pulled)
	}
	var mails, parsed, mailEntries int
	var lastMail sql.NullTime
	db.QueryRow(`SELECT count(*), count(*) FILTER (WHERE parsed > 0), COALESCE(sum(applied),0), max(received_at)
		FROM cl_alert_emails`).Scan(&mails, &parsed, &mailEntries, &lastMail)
	lastM := "none yet"
	if lastMail.Valid {
		lastM = lastMail.Time.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	return fmt.Sprintf("CourtListener pushes: %d pulled from D1, %d applied, %s waiting in D1%s, last received %s. Alert emails: %d received, %d parsed, %d docket entries applied from them, last %s.",
		pulled, applied, pendingS, d1Note, lastS, mails, parsed, mailEntries, lastM)
}
