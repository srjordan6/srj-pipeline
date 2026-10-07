package main

// COURTLISTENER ALERT EMAILS, theworldofai row 565 (2026-10-07).
//
// Stephen forwards alerts@courtlistener.com mail to the inkbox mailbox the
// tick already reads. Each mail is one docket alert: the subject reads
// "N New Docket Entry for <case> (<docket number>)", the body carries the
// View Docket link (/docket/<id>/), and for each entry a document number, a
// filing date and the entry's description. The description matters: the
// webhook payload's description was an empty string for every real push on
// the first day, so the mail is the better source for the text, and the
// webhook the better source for speed. Both go through clMergeEntries, which
// keys an entry on date and document number, so whichever arrives first
// writes it and the other is a no-op.
//
// These mails are not for the bridge: they are logged in cl_alert_emails
// with the raw text, parsed count and applied count, and counted in the daily
// freshness row. The raw text is kept because the parser below was written
// from Stephen's description of the mail, not from a sample; the first real
// mail will show whether it reads the layout, and the row will hold the text
// to fix it against.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	clMailSubjectRe = regexp.MustCompile(`(?i)new docket entr(?:y|ies) for (.+?)(?:\s*\(([^)]*)\))?\s*$`)
	clMailPathRe    = regexp.MustCompile(`^/docket/(\d+)/`)
	clMailDocNoRe   = regexp.MustCompile(`(?i)^(?:document|doc\.?|entry)\s*(?:number|no\.?|#)?\s*[:#]?\s*(\d+)\s*$`)
	clMailDateRe    = regexp.MustCompile(`(?i)^(?:date\s+)?filed\s*[:]?\s*(.+?)\s*$`)
	clMailDescRe    = regexp.MustCompile(`(?i)^description\s*[:]\s*(.+?)\s*$`)
	clMailURLRe     = regexp.MustCompile(`^https?://`)
)

// clIsAlertMail says whether a mail in the inkbox is a CourtListener docket
// alert, by sender or by subject (a forward keeps the subject).
func clIsAlertMail(from, subject string) bool {
	return clMailDomain(from) == "courtlistener.com" || clMailSubjectRe.MatchString(strings.TrimSpace(subject))
}

// clMailDomain is the domain of a From header, whatever its display form:
// "CourtListener <alerts@courtlistener.com>" and "alerts@courtlistener.com"
// both give courtlistener.com. The whole domain is compared, never a
// substring, so no other host that contains the name passes.
func clMailDomain(from string) string {
	from = strings.ToLower(strings.TrimSpace(from))
	if i := strings.LastIndex(from, "@"); i >= 0 {
		from = from[i+1:]
	}
	return strings.Trim(strings.TrimSpace(from), "<>\"'")
}

func clEnsureAlertMail(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS cl_alert_emails (
		id serial PRIMARY KEY,
		message_id text UNIQUE NOT NULL,
		received_at timestamptz NOT NULL DEFAULT now(),
		from_addr text, subject text,
		docket_id text, case_name text,
		entries jsonb, parsed int NOT NULL DEFAULT 0, applied int NOT NULL DEFAULT 0,
		note text, raw text)`)
}

// clMailDate reads the dates CourtListener writes in mail and on the site.
func clMailDate(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "Sept.", "Sep.")
	for _, layout := range []string{"2006-01-02", "Jan. 2, 2006", "Jan 2, 2006", "January 2, 2006",
		"01/02/2006", "1/2/2006", "Monday, January 2, 2006", "2 January 2006", "Jan. 2 2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return ""
}

// clParseAlertMail reads the entries out of one alert mail. Labelled lines
// are read by label; a description with no label is the last line of text
// before the entry's document number or date.
func clParseAlertMail(text string) (docket string, entries []clEntry) {
	docket = clMailDocketID(text)
	var cur clEntry
	var curNo, lastText string
	flush := func() {
		if cur.DateFiled != "" && (cur.Description != "" || curNo != "") {
			if cur.Description == "" {
				cur.Description = lastText
			}
			if cur.Description != "" {
				cur.EntryNumber = json.RawMessage(`"` + curNo + `"`)
				cur.Docket = json.RawMessage(docket)
				entries = append(entries, cur)
			}
		}
		cur, curNo = clEntry{}, ""
	}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || clMailURLRe.MatchString(line) {
			continue
		}
		switch {
		case clMailDocNoRe.MatchString(line):
			if curNo != "" || (cur.DateFiled != "" && cur.Description != "") {
				flush()
			}
			curNo = clMailDocNoRe.FindStringSubmatch(line)[1]
		case clMailDateRe.MatchString(line) && clMailDate(clMailDateRe.FindStringSubmatch(line)[1]) != "":
			if cur.DateFiled != "" {
				flush()
			}
			cur.DateFiled = clMailDate(clMailDateRe.FindStringSubmatch(line)[1])
		case clMailDescRe.MatchString(line):
			cur.Description = strings.TrimSpace(clMailDescRe.FindStringSubmatch(line)[1])
		default:
			if cur.DateFiled != "" && cur.Description == "" && curNo != "" {
				// The description follows the labels in some layouts.
				cur.Description = line
				continue
			}
			lastText = line
		}
	}
	flush()
	return docket, entries
}

// clAlertMail stores one alert mail and applies its entries to the case the
// docket id names. It returns how many entries were new to the timeline.
func clAlertMail(db *sql.DB, msgID, from, subject, text string) (int, error) {
	clEnsureAlertMail(db)
	caseName := ""
	if m := clMailSubjectRe.FindStringSubmatch(strings.TrimSpace(subject)); m != nil {
		caseName = strings.TrimSpace(m[1])
	}
	docket, entries := clParseAlertMail(text)
	ej, _ := json.Marshal(entries)
	res, err := db.Exec(`INSERT INTO cl_alert_emails (message_id, from_addr, subject, docket_id, case_name, entries, parsed, raw)
		VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), $6::jsonb, $7, $8) ON CONFLICT (message_id) DO NOTHING`,
		msgID, from, subject, docket, caseName, string(ej), len(entries), trunc(text, 20000))
	if err != nil {
		return 0, err
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return 0, nil // seen before
	}
	if docket == "" {
		db.Exec(`UPDATE cl_alert_emails SET note = 'no /docket/<id>/ link in the mail; unparsed, raw kept' WHERE message_id = $1`, msgID)
		return 0, nil
	}
	if len(entries) == 0 {
		db.Exec(`UPDATE cl_alert_emails SET note = 'docket ' || $2 || ': no entries read from the text; raw kept for the parser' WHERE message_id = $1`, msgID, docket)
		return 0, nil
	}
	var id int64
	var slug, url, timeline string
	if db.QueryRow(`SELECT id, slug, courtlistener_url, COALESCE(timeline::text, '[]') FROM ai_lawsuits
		WHERE courtlistener_url ~ ('/docket/' || $1 || '(/|$)') ORDER BY is_active DESC, id LIMIT 1`, docket).
		Scan(&id, &slug, &url, &timeline) != nil {
		db.Exec(`UPDATE cl_alert_emails SET note = 'docket ' || $2 || ': not on the tracker' WHERE message_id = $1`, msgID, docket)
		return 0, nil
	}
	n, newest, err := clMergeEntries(db, id, timeline, url, entries)
	if err != nil {
		db.Exec(`UPDATE cl_alert_emails SET note = 'docket ' || $2 || ' ' || $3 || ': ' || $4 WHERE message_id = $1`, msgID, docket, slug, trunc(err.Error(), 200))
		return 0, nil
	}
	db.Exec(`UPDATE ai_lawsuits SET docket_ok_at = now(), docket_checked_at = now() WHERE id = $1`, id)
	db.Exec(`UPDATE cl_alert_emails SET applied = $2, note = 'docket ' || $3 || ' ' || $4 || ': ' || $2 || ' new of ' || $5 WHERE message_id = $1`,
		msgID, n, docket, slug, len(entries))
	if n > 0 {
		fmt.Printf("cl_mail %s: %d new docket entries through %s\n", slug, n, newest)
		clNoteChange(db, fmt.Sprintf("email: %d entries for %s", n, slug))
	}
	return n, nil
}

// clMailDocketID finds the CourtListener docket id in the mail: the first
// link whose host is courtlistener.com and whose path is /docket/<id>/. The
// host is compared whole after parsing, not matched by a pattern in the
// text, so a link to another site that mentions the name does not count.
func clMailDocketID(text string) string {
	for _, tok := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == '<' || r == '>' || r == '"' || r == ')' || r == '('
	}) {
		if !strings.HasPrefix(tok, "http://") && !strings.HasPrefix(tok, "https://") {
			continue
		}
		u, err := url.Parse(tok)
		if err != nil {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if host != "courtlistener.com" && host != "www.courtlistener.com" {
			continue
		}
		if m := clMailPathRe.FindStringSubmatch(u.Path); m != nil {
			return m[1]
		}
	}
	return ""
}
