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
	"bytes"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"os"
	"regexp"
	"strings"
	"time"
)

// LegiScan status codes. 4 and 5 are the terminal outcomes worth announcing;
// a bill that passed one chamber is not news to a compliance audience.
//
// 7 is ours, not LegiScan's: PRESENTED TO THE GOVERNOR, detected from the
// history array. See twoaiBillHistoryEvents for why the history is read at
// all - the short version is that LegiScan's status code lagged California
// SB 813's signing by days, the history did not, and the compliance page for
// California learned nothing from either.
var twoaiBillStatusLabel = map[int]string{
	1: "Introduced", 2: "Engrossed", 3: "Enrolled",
	4: "Passed", 5: "Vetoed", 6: "Failed",
	7: "Presented to Governor",
}

// Governor-stage actions as they appear in LegiScan history entries, across
// the phrasings the states use. "Signed" produces a status-4 event straight
// from the history so the enacted page, the news story, the social draft and
// the alert all fire the day the record shows it, not the day LegiScan's
// summary code catches up. "Presented" is the early warning: a bill on the
// governor's desk is days from being law, and the compliance page for that
// state should already be flagged for review.
var twoaiGovSignedRe = regexp.MustCompile(`(?i)(approved by (the )?governor|signed by (the )?governor|governor signed|signed into law|became law without|chaptered|chapter(ed)? (no\.? ?)?\d+|filed with (the )?secretary of state|act no\.? ?\d+)`)
var twoaiGovPresentedRe = regexp.MustCompile(`(?i)(presented to (the )?governor|enrolled and presented|sent to (the )?governor|transmitted to (the )?governor|delivered to (the )?governor|to governor)`)
var twoaiGovVetoedRe = regexp.MustCompile(`(?i)(vetoed by (the )?governor|governor veto|veto(ed)? by)`)

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

	// Second detector, reading the history instead of the status code.
	hist, err := twoaiBillHistoryEvents(db)
	if err != nil {
		return newEvents, err
	}
	newEvents += hist

	// Route every relevant governor-stage event to the state's compliance
	// page as an open review item.
	if _, err := twoaiBillPageReviews(db); err != nil {
		return newEvents, err
	}
	return newEvents, nil
}

// twoaiBillHistoryEvents reads the HISTORY array of each bill's latest
// snapshot and fires on governor-stage actions.
//
// Why this exists. On 2026-09-09 California's governor signed SB 813 and AB
// 1405, the first state AI auditing regime. The pipeline had ten hydrated
// snapshots of SB 813, the newest fetched hours after the signing, and its
// history array's top entry read "Enrolled and presented to the Governor at 4
// p.m." But LegiScan's summary status code still said 3, Engrossed, dated
// 2026-08-30, because their code lags their history by days. The detector
// above only reads the code, so a law the database already knew about
// produced no event, no news story, no alert, and no change to the California
// compliance page. Stephen found out from teleSUR.
//
// The history is where the truth arrives first, and it was already in the
// database. This reads it. A signing produces a status-4 event with the
// history date, so every existing output fires; "presented to the Governor"
// produces status 7, which reaches the alert and the page review but not the
// news or the social feed, because a bill on the desk is not yet a law.
//
// Idempotent on the same primary key as the status detector: if LegiScan's
// code later catches up and offers status 4 with its own date, ON CONFLICT
// keeps ours, which is the earlier and more accurate one.
func twoaiBillHistoryEvents(db *sql.DB) (int, error) {
	rows, err := db.Query(`
		SELECT DISTINCT ON (raw->'bill'->>'state', raw->'bill'->>'bill_number')
		  raw->'bill'->>'state', raw->'bill'->>'bill_number',
		  title, COALESCE(raw->'bill'->>'description',''), COALESCE(raw->'bill'->>'url',''),
		  COALESCE(raw->'bill'->'history', '[]'::jsonb)::text
		FROM pipeline.documents
		WHERE raw ? 'bill' AND raw->'bill'->>'state' IS NOT NULL
		  AND jsonb_typeof(raw->'bill'->'history') = 'array'
		  AND fetched_at > now() - interval '120 days'
		ORDER BY raw->'bill'->>'state', raw->'bill'->>'bill_number', fetched_at DESC`)
	if err != nil {
		return 0, err
	}
	type cand struct {
		state, bill, title, desc, url, hist string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.state, &c.bill, &c.title, &c.desc, &c.url, &c.hist) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()

	newEvents := 0
	for _, c := range cands {
		rel, note := twoaiBillRelevant(c.title, c.desc)
		if !rel {
			continue // the gate applies here exactly as it does to the status path
		}
		var hist []struct {
			Date   string `json:"date"`
			Action string `json:"action"`
		}
		if json.Unmarshal([]byte(c.hist), &hist) != nil {
			continue
		}
		// Strongest signal wins: a bill that was presented and then signed is a
		// signing, and the presented event is still recorded so the review item
		// carries the full sequence.
		var signedDate, presentedDate, vetoedDate string
		for _, h := range hist {
			switch {
			case twoaiGovVetoedRe.MatchString(h.Action):
				if h.Date > vetoedDate {
					vetoedDate = h.Date
				}
			case twoaiGovSignedRe.MatchString(h.Action):
				if h.Date > signedDate {
					signedDate = h.Date
				}
			case twoaiGovPresentedRe.MatchString(h.Action):
				if h.Date > presentedDate {
					presentedDate = h.Date
				}
			}
		}
		emit := func(status int, date string) error {
			if date == "" {
				return nil
			}
			res, err := db.Exec(`INSERT INTO twoai_bill_events
				(state, bill_number, status, status_label, status_date, title, description, url, relevant, relevance_note)
				VALUES ($1,$2,$3,$4,$5::date,$6,$7,$8,true,$9)
				ON CONFLICT (state, bill_number, status) DO NOTHING`,
				c.state, c.bill, status, twoaiBillStatusLabel[status], date,
				c.title, c.desc, c.url, note+"; detected from LegiScan history action, not status code")
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				newEvents++
				fmt.Printf("twoai_bill_events: history %s %s -> %s on %s\n", c.state, c.bill, twoaiBillStatusLabel[status], date)
			}
			return nil
		}
		if err := emit(7, presentedDate); err != nil {
			return newEvents, err
		}
		if err := emit(4, signedDate); err != nil {
			return newEvents, err
		}
		if err := emit(5, vetoedDate); err != nil {
			return newEvents, err
		}
	}
	if newEvents > 0 {
		fmt.Printf("twoai_bill_events: history detector new=%d\n", newEvents)
	}
	return newEvents, nil
}

// twoaiBillPageReviews turns a governor-stage event into an open review item
// on the compliance page it changes.
//
// This is the piece that was missing altogether. The four outputs above put
// an enactment on an aggregate page, in the news, on social and in Stephen's
// inbox. None of them touched the page a reader actually goes to - the
// state's own compliance page, /ai-compliance/california-ai-laws/ - which
// kept describing the law as it stood before the bill existed.
//
// A review item is not a link and not a rewrite. It is a visible flag on the
// page: "SB 813 was signed on 2026-09-09; this guidance predates it", with
// the primary source, until someone updates the prose and clears it. The
// reader sees that the guidance is behind, which is the honest state, rather
// than a page that looks current and is not.
func twoaiBillPageReviews(db *sql.DB) (int, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_page_reviews (
		id serial PRIMARY KEY,
		page_path text NOT NULL,
		trigger_kind text NOT NULL,
		trigger_ref text NOT NULL,
		trigger_date date NOT NULL,
		summary text NOT NULL,
		source_url text NOT NULL DEFAULT '',
		opened_on date NOT NULL DEFAULT current_date,
		cleared_on date,
		cleared_note text,
		UNIQUE (page_path, trigger_kind, trigger_ref))`); err != nil {
		return 0, err
	}

	// Which compliance page belongs to a state. The pages are named
	// compliance/{state-name}-ai-laws.json; the map is the two-letter code to
	// that slug. Puerto Rico and DC are included because pages exist for them.
	stateSlug := map[string]string{
		"AL": "alabama", "AK": "alaska", "AZ": "arizona", "AR": "arkansas", "CA": "california",
		"CO": "colorado", "CT": "connecticut", "DE": "delaware", "FL": "florida", "GA": "georgia",
		"HI": "hawaii", "ID": "idaho", "IL": "illinois", "IN": "indiana", "IA": "iowa",
		"KS": "kansas", "KY": "kentucky", "LA": "louisiana", "ME": "maine", "MD": "maryland",
		"MA": "massachusetts", "MI": "michigan", "MN": "minnesota", "MS": "mississippi", "MO": "missouri",
		"MT": "montana", "NE": "nebraska", "NV": "nevada", "NH": "new-hampshire", "NJ": "new-jersey",
		"NM": "new-mexico", "NY": "new-york", "NC": "north-carolina", "ND": "north-dakota", "OH": "ohio",
		"OK": "oklahoma", "OR": "oregon", "PA": "pennsylvania", "RI": "rhode-island", "SC": "south-carolina",
		"SD": "south-dakota", "TN": "tennessee", "TX": "texas", "UT": "utah", "VT": "vermont",
		"VA": "virginia", "WA": "washington", "WV": "west-virginia", "WI": "wisconsin", "WY": "wyoming",
		"DC": "dc", "PR": "puerto-rico",
	}

	rows, err := db.Query(`SELECT state, bill_number, status, status_label, status_date::text, title, url
		FROM twoai_bill_events WHERE relevant AND status IN (4,5,7)
		  AND status_date >= current_date - interval '180 days'`)
	if err != nil {
		return 0, err
	}
	type ev struct{ state, bill, label, date, title, url string; status int }
	var evs []ev
	for rows.Next() {
		var e ev
		if rows.Scan(&e.state, &e.bill, &e.status, &e.label, &e.date, &e.title, &e.url) == nil {
			evs = append(evs, e)
		}
	}
	rows.Close()

	opened := 0
	for _, e := range evs {
		slug, ok := stateSlug[e.state]
		if !ok {
			continue
		}
		path := "compliance/" + slug + "-ai-laws.json"
		var exists bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM twoai_pages WHERE path=$1)`, path).Scan(&exists)
		if !exists {
			continue
		}
		subject := strings.TrimSpace(strings.TrimPrefix(e.title, e.state+" "+e.bill+":"))
		var summary string
		switch e.status {
		case 7:
			summary = fmt.Sprintf("%s %s was presented to the Governor on %s: %s. This guidance was written before it reached the Governor's desk.", e.state, e.bill, e.date, subject)
		case 5:
			summary = fmt.Sprintf("%s %s was vetoed on %s: %s. If this page discusses the bill, it should say so.", e.state, e.bill, e.date, subject)
		default:
			summary = fmt.Sprintf("%s %s became law on %s: %s. This guidance predates it and has not been reviewed against the enacted text.", e.state, e.bill, e.date, subject)
		}
		res, err := db.Exec(`INSERT INTO twoai_page_reviews (page_path, trigger_kind, trigger_ref, trigger_date, summary, source_url)
			VALUES ($1,'bill:'||$2,$3,$4::date,$5,$6)
			ON CONFLICT (page_path, trigger_kind, trigger_ref) DO NOTHING`,
			path, strings.ToLower(e.label), e.state+" "+e.bill, e.date, summary, e.url)
		if err != nil {
			return opened, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			opened++
			fmt.Printf("twoai_bill_events: review opened on %s for %s %s (%s)\n", path, e.state, e.bill, e.label)
		}
	}
	var open int
	db.QueryRow(`SELECT count(*) FROM twoai_page_reviews WHERE cleared_on IS NULL`).Scan(&open)

	// AUTO-CLEAR. A review item lifts itself the moment the page's prose is
	// changed after the trigger. The prose lives in site_content, keyed
	// governance/{slug}.json, and its updated_at is the one date on this system
	// that means "a person changed the guidance" - the compliance page's own
	// generated field is the build date and moves every run. So the human step
	// is only the one that cannot be automated, updating the words; the flag
	// appears and disappears on its own. Cleared rows are kept with the reason.
	cleared, err := db.Exec(`UPDATE twoai_page_reviews r
		SET cleared_on = current_date,
		    cleared_note = 'guidance updated on '||c.updated_at::date||', after the trigger'
		FROM site_content c
		WHERE r.cleared_on IS NULL
		  AND c.path = replace(r.page_path, 'compliance/', 'governance/')
		  AND c.updated_at::date > r.trigger_date`)
	if err != nil {
		return opened, err
	}
	if n, _ := cleared.RowsAffected(); n > 0 {
		fmt.Printf("twoai_bill_events: page reviews auto-cleared=%d (guidance updated after trigger)\n", n)
		open -= int(n)
	}
	fmt.Printf("twoai_bill_events: page reviews opened=%d open_total=%d\n", opened, open)
	return opened, nil
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

	// Open page reviews, one document the compliance template reads to flag
	// its own page. Keyed by the page's slug so the template needs no join.
	// Cleared reviews are excluded here but never deleted from the table, so
	// the record of what changed a page and when survives the clearing.
	rrows, err := db.Query(`SELECT page_path, trigger_kind, trigger_ref, trigger_date::text, summary, source_url, opened_on::text
		FROM twoai_page_reviews WHERE cleared_on IS NULL ORDER BY trigger_date DESC`)
	if err != nil {
		return 0, err
	}
	reviews := map[string][]map[string]string{}
	for rrows.Next() {
		var path, kind, ref, date, summary, url, opened string
		if rrows.Scan(&path, &kind, &ref, &date, &summary, &url, &opened) != nil {
			continue
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(path, "compliance/"), ".json")
		reviews[slug] = append(reviews[slug], map[string]string{
			"kind": kind, "ref": ref, "date": date, "summary": summary, "source_url": url, "opened_on": opened,
		})
	}
	rrows.Close()

	// The date the GUIDANCE last changed, for every governance page, so the
	// compliance template can show it. This is the honest freshness stamp.
	// Each page's generated field is the build date and reads "verified today"
	// on prose nobody has touched for weeks - California's page said
	// 2026-09-10 on guidance last edited 2026-08-19, three weeks before SB 813
	// was signed. A reader deciding whether to trust a page needs the date the
	// words changed, not the date the server ran.
	prows, err := db.Query(`SELECT replace(replace(path,'governance/',''),'.json','') AS slug, updated_at::date::text FROM site_content WHERE path LIKE 'governance/%.json'`)
	if err != nil {
		return 0, err
	}
	reviewed := map[string]string{}
	for prows.Next() {
		var slug, date string
		if prows.Scan(&slug, &date) == nil {
			reviewed[slug] = date
		}
	}
	prows.Close()
	if err := upsert("compliance/page-reviews.json", "compliance-reviews", map[string]any{
		"generated": today, "pages": len(reviews), "reviews": reviews, "reviewed": reviewed,
	}); err != nil {
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

	// ONE SOCIAL POST PER DAY. Stephen's cadence: a newly enacted law is a
	// post, not a burst. If several laws land on the same day, the rest wait
	// their turn rather than flooding the feed and burning the audience's
	// attention on a single afternoon. Alerts are NOT throttled: he wants to
	// know about every enactment the day it is detected, and an inbox can
	// absorb what a feed cannot.
	var queuedToday int
	db.QueryRow(`SELECT count(*) FROM twoai_bill_outbox
		WHERE channel='social' AND queued_on = current_date`).Scan(&queuedToday)

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
		if queuedToday < 1 {
			res, err := db.Exec(`INSERT INTO twoai_bill_outbox (channel, state, bill_number, subject, body, url)
				VALUES ('social',$1,$2,$3,$4,$5) ON CONFLICT (channel, state, bill_number) DO NOTHING`,
				it.state, it.bill, subject, social, it.url)
			if err != nil {
				return queued, err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				queuedToday++
				db.Exec(`UPDATE twoai_bill_events SET social_on = current_date
					WHERE state=$1 AND bill_number=$2 AND status=4`, it.state, it.bill)
			}
		}

		alert := fmt.Sprintf(
			"BILL ENACTED\n\n%s %s became law on %s.\n\nSubject: %s\n\nDescription: %s\n\nLegiScan record: %s\n\nThis was detected automatically from the legislative record. Before it goes into a client deliverable, open the LegiScan page and confirm the status and effective date against the state's own record.",
			it.state, it.bill, it.date, subject, it.desc, it.url)
		if _, err := db.Exec(`INSERT INTO twoai_bill_outbox (channel, state, bill_number, subject, body, url)
			VALUES ('alert',$1,$2,$3,$4,$5) ON CONFLICT (channel, state, bill_number) DO NOTHING`,
			it.state, it.bill, fmt.Sprintf("%s %s enacted: %s", it.state, it.bill, subject), alert, it.url); err != nil {
			return queued, err
		}

		if _, err := db.Exec(`UPDATE twoai_bill_events SET notified_on = current_date
			WHERE state=$1 AND bill_number=$2 AND status=4 AND notified_on IS NULL`,
			it.state, it.bill); err != nil {
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

// ---- Email delivery of the alert -----------------------------------------
//
// Stephen wants enactments in his inbox. Sending is deliberately narrow: it
// only ever sends the alert channel, never the social post, and it marks each
// row sent the moment delivery succeeds so a retry cannot send twice.
//
// Configuration, all on the Render cron:
//
//	TWOAI_ALERT_SMTP_HOST   e.g. smtp.gmail.com
//	TWOAI_ALERT_SMTP_PORT   587 (STARTTLS) or 465 (implicit TLS)
//	TWOAI_ALERT_SMTP_USER   the sending mailbox
//	TWOAI_ALERT_SMTP_PASS   an app password, not the account password
//	TWOAI_ALERT_TO          recipient
//
// If any is missing the stage says so and leaves the queue intact, because a
// silently undelivered alert is worse than an obvious one: the whole point is
// that Stephen learns a bill passed without having to check.
func twoaiBillAlertsSend(db *sql.DB) (int, error) {
	host := os.Getenv("TWOAI_ALERT_SMTP_HOST")
	port := os.Getenv("TWOAI_ALERT_SMTP_PORT")
	user := os.Getenv("TWOAI_ALERT_SMTP_USER")
	pass := os.Getenv("TWOAI_ALERT_SMTP_PASS")
	to := os.Getenv("TWOAI_ALERT_TO")
	if host == "" || user == "" || pass == "" || to == "" {
		var pending int
		db.QueryRow(`SELECT count(*) FROM twoai_bill_outbox WHERE channel='alert' AND sent_on IS NULL`).Scan(&pending)
		if pending > 0 {
			fmt.Printf("twoai_bill_events: %d alerts queued but SMTP is not configured, nothing delivered\n", pending)
		}
		return 0, nil
	}
	if port == "" {
		port = "587"
	}

	rows, err := db.Query(`SELECT id, subject, body, url FROM twoai_bill_outbox
		WHERE channel='alert' AND sent_on IS NULL ORDER BY id LIMIT 20`)
	if err != nil {
		return 0, err
	}
	type msg struct {
		id                 int
		subject, body, url string
	}
	var msgs []msg
	for rows.Next() {
		var m msg
		if rows.Scan(&m.id, &m.subject, &m.body, &m.url) == nil {
			msgs = append(msgs, m)
		}
	}
	rows.Close()
	if len(msgs) == 0 {
		return 0, nil
	}

	addr := host + ":" + port
	auth := smtp.PlainAuth("", user, pass, host)
	sent := 0
	for _, m := range msgs {
		payload := []byte("From: " + user + "\r\n" +
			"To: " + to + "\r\n" +
			"Subject: [AI law enacted] " + m.subject + "\r\n" +
			"Content-Type: text/plain; charset=UTF-8\r\n\r\n" +
			m.body + "\r\n")
		var serr error
		if port == "465" {
			serr = twoaiSendImplicitTLS(addr, host, auth, user, to, payload)
		} else {
			serr = smtp.SendMail(addr, auth, user, []string{to}, payload)
		}
		if serr != nil {
			fmt.Printf("twoai_bill_events: alert send failed (%v), queue left intact\n", serr)
			break
		}
		if _, err := db.Exec(`UPDATE twoai_bill_outbox SET sent_on = current_date WHERE id=$1`, m.id); err != nil {
			return sent, err
		}
		sent++
	}
	if sent > 0 {
		fmt.Printf("twoai_bill_events: alerts emailed=%d to %s\n", sent, to)
	}
	return sent, nil
}

// Port 465 speaks TLS from the first byte, which smtp.SendMail does not do.
func twoaiSendImplicitTLS(addr, host string, auth smtp.Auth, from, to string, payload []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Quit()
	if err := c.Auth(auth); err != nil {
		return err
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return w.Close()
}

// ---- Marky delivery of the social post -----------------------------------
//
// The stage previously wrote captions into twoai_bill_outbox and stopped
// there, and I described that as "queued", which was wrong in the way that
// matters: a row in our own Postgres is not a social post. Stephen went
// looking in Marky and found nothing. This closes the gap.
//
// Posts are created with status NEW, never scheduled and never published.
// Stephen schedules. The daily throttle upstream already limits this to one
// bill per day, so this sends at most one draft per run.
//
// Platform selection is deliberate: everything except Instagram, Instagram
// Story, and TikTok. A statutory compliance update is not what those
// audiences are there for, and posting it anyway trains people to scroll past
// the account.
func twoaiBillSocialToMarky(db *sql.DB) (int, error) {
	key := os.Getenv("MARKY_API_KEY")
	bizID := os.Getenv("MARKY_BUSINESS_ID")
	if key == "" || bizID == "" {
		var pending int
		db.QueryRow(`SELECT count(*) FROM twoai_bill_outbox WHERE channel='social' AND sent_on IS NULL`).Scan(&pending)
		if pending > 0 {
			fmt.Printf("twoai_bill_events: %d social drafts queued but MARKY_API_KEY/MARKY_BUSINESS_ID unset, nothing reached Marky\n", pending)
		}
		return 0, nil
	}

	rows, err := db.Query(`SELECT id, state, bill_number, subject, body, url
		FROM twoai_bill_outbox WHERE channel='social' AND sent_on IS NULL
		ORDER BY id LIMIT 3`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id                              int
		state, bill, subject, body, url string
	}
	var items []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.state, &r.bill, &r.subject, &r.body, &r.url) == nil {
			items = append(items, r)
		}
	}
	rows.Close()

	client := &http.Client{Timeout: 45 * time.Second}
	created := 0
	for _, it := range items {
		payload := map[string]any{
			"caption": it.body,
			"link":    "https://theworldofai.org/ai-compliance/",
			"status":  "NEW",
			// X excluded: posting through the API is now a paid tier, and an
			// unpaid post either fails or costs money nobody approved. The
			// five below cover the audience this content is written for.
			"restrict_publish_to": []string{
				"linkedIn", "linkedInProfile", "facebook",
				"pinterest", "googleBusiness",
			},
			// Pinterest requires a title on the pin and silently produces a
			// poor result without one, so it gets a real title built from the
			// bill plus the destination link. No other platform override: the
			// single caption goes everywhere else as written.
			"platform_overrides": []map[string]any{
				{"platform": "pinterest",
					"title": fmt.Sprintf("%s %s enacted: %s", it.state, it.bill, it.subject),
					"link":  "https://theworldofai.org/ai-compliance/"},
			},
			"metadata": map[string]string{
				"state": it.state, "bill": it.bill,
				"kind": "enacted-law", "source": "twoai_bill_outbox",
			},
		}
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequest("POST",
			"https://api.mymarky.ai/api/businesses/"+bizID+"/posts", bytes.NewReader(body))
		req.Header.Set("content-type", "application/json")
		req.Header.Set("authorization", "Bearer "+key)
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("twoai_bill_events: marky post failed (%v), queue left intact\n", err)
			break
		}
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 && resp.StatusCode != 201 {
			fmt.Printf("twoai_bill_events: marky status %d (%.180s), queue left intact\n", resp.StatusCode, rb)
			break
		}
		if _, err := db.Exec(`UPDATE twoai_bill_outbox SET sent_on = current_date WHERE id=$1`, it.id); err != nil {
			return created, err
		}
		created++
	}
	if created > 0 {
		fmt.Printf("twoai_bill_events: marky drafts created=%d (status NEW, Stephen schedules)\n", created)
	}
	return created, nil
}
