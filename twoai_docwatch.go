package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// KEEPING AUTHORED DOCUMENTS HONEST AFTER THE DAY THEY WERE WRITTEN.
//
// The downloads repository holds documents this site wrote: checklists, a
// committee charter, an audit programme, a vendor scorecard, a position
// paper. Each carries a reviewed_on date and each cites a public framework -
// NIST AI RMF, ISO/IEC 42001, the EU AI Act. Nothing looked at any of them
// again after they were written, which is exactly the failure the white paper
// in that repository describes: the page stays up, reads fine, and quietly
// stops being true. A reviewed_on date that nobody recomputes is a claim, not
// a control.
//
// TWO SIGNALS, DELIBERATELY SEPARATED. Reporting both as one number would
// bury the one that matters:
//
//   - CALENDAR DUE. reviewed_on is older than review_interval_days. Routine,
//     expected, and at a 7-day interval it will be true of almost everything
//     almost always. Reported as a single count, never as a list, because a
//     warning that fires every run is the warning people stop reading - and
//     suppressing the message while leaving the condition is the worst
//     outcome available.
//
//   - SOURCE CHANGED. The public framework a document cites no longer hashes
//     the same. That is rare, actionable, and named loudly with the documents
//     affected. When NIST revises the AI RMF, every document citing it needs
//     a human, and this is the line that says so on the morning it happens.
//
// WHAT IS HASHED, AND WHY IT IS NOT THE WHOLE PAGE. Framework pages carry
// navigation, banners and rotating promotional blocks; hashing raw HTML would
// report a change every day and mean nothing. The hash covers visible text
// with scripts, styles and tags stripped, digits and whitespace normalised.
// It is deliberately blunt: it will miss a subtle wording change and it will
// not cry wolf over a changed menu. A blunt signal that gets read beats a
// precise one that gets muted. Verified against the two sources actually
// cited: NIST and the EU AI Act portal both hash identically on refetch, and
// adding a single sentence changes the hash.
//
// FIRST RUN NEVER FIRES. A document with no stored hash records one and says
// nothing, because "we have never checked this before" is not a change.

var twoaiDocTagRe = regexp.MustCompile(`(?s)<(script|style|noscript)[^>]*>.*?</(script|style|noscript)>`)
var twoaiDocAngleRe = regexp.MustCompile(`<[^>]+>`)
var twoaiDocSpaceRe = regexp.MustCompile(`\s+`)
var twoaiDocDigitRe = regexp.MustCompile(`\d+`)

// twoaiDocFingerprint reduces a page to something that only changes when the
// prose does.
func twoaiDocFingerprint(body []byte) string {
	s := twoaiDocTagRe.ReplaceAllString(string(body), " ")
	s = twoaiDocAngleRe.ReplaceAllString(s, " ")
	s = twoaiDocDigitRe.ReplaceAllString(s, "#") // dates and counters churn
	s = strings.ToLower(twoaiDocSpaceRe.ReplaceAllString(s, " "))
	s = strings.TrimSpace(s)
	if len(s) < 400 {
		// Too little text to judge: a login wall, an error page or a redirect
		// stub. Returning empty makes the caller skip rather than record a
		// fingerprint that would later look like a change.
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func twoaiDocWatch(db *sql.DB) error {
	rows, err := db.Query(`SELECT slug, title, COALESCE(based_on_url,''), COALESCE(source_hash,''),
			reviewed_on, review_interval_days
		FROM twoai_downloads WHERE status='live' ORDER BY section_slug, sort`)
	if err != nil {
		return fmt.Errorf("docwatch query: %w", err)
	}
	type doc struct {
		slug, title, url, hash string
		reviewed               sql.NullTime
		interval               int
	}
	var docs []doc
	for rows.Next() {
		var d doc
		if rows.Scan(&d.slug, &d.title, &d.url, &d.hash, &d.reviewed, &d.interval) == nil {
			docs = append(docs, d)
		}
	}
	rows.Close()
	if len(docs) == 0 {
		fmt.Println("docwatch: no live documents")
		return nil
	}

	client := &http.Client{Timeout: 25 * time.Second}
	checked, firstSeen, unreachable := 0, 0, 0
	var changed []string
	dueCount := 0
	var oldest string
	oldestDays := -1

	for _, d := range docs {
		// Calendar side first: cheap, and independent of the network.
		if d.reviewed.Valid {
			days := int(time.Since(d.reviewed.Time).Hours() / 24)
			if days >= d.interval {
				dueCount++
				if days > oldestDays {
					oldestDays, oldest = days, d.title
				}
			}
		} else {
			dueCount++
		}

		if d.url == "" || !strings.HasPrefix(d.url, "http") {
			continue
		}
		req, _ := http.NewRequest("GET", d.url, nil)
		req.Header.Set("User-Agent", "theworldofai.org document source watch (info@srjconsultingservices.com)")
		resp, err := client.Do(req)
		if err != nil {
			unreachable++
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			unreachable++
			continue
		}
		fp := twoaiDocFingerprint(body)
		if fp == "" {
			unreachable++
			continue
		}
		checked++

		if d.hash == "" {
			// Baseline. Never a change.
			db.Exec(`UPDATE twoai_downloads SET source_hash=$1, source_checked_on=current_date
				WHERE slug=$2`, fp, d.slug)
			firstSeen++
			continue
		}
		if fp != d.hash {
			db.Exec(`UPDATE twoai_downloads SET source_hash=$1, source_checked_on=current_date,
				source_changed_on=current_date,
				source_note=$2 WHERE slug=$3`,
				fp, "cited source text changed on "+time.Now().UTC().Format("2006-01-02")+
					"; document needs a human read against "+d.url, d.slug)
			changed = append(changed, d.slug)
			continue
		}
		db.Exec(`UPDATE twoai_downloads SET source_checked_on=current_date WHERE slug=$1`, d.slug)
	}

	// The loud line. Only printed when something actually moved.
	if len(changed) > 0 {
		sort.Strings(changed)
		fmt.Printf("docwatch: SOURCE CHANGED for %d document(s), each needs a human read: %s\n",
			len(changed), strings.Join(changed, " "))
	}
	// The quiet line. One count, never a list, because at a 7-day interval
	// this is true of nearly everything nearly always and a list would train
	// the reader to skip the whole stage.
	fmt.Printf("docwatch: docs=%d sources_checked=%d baselined=%d unreachable=%d review_due=%d",
		len(docs), checked, firstSeen, unreachable, dueCount)
	if oldestDays > 0 {
		fmt.Printf(" oldest=%dd (%s)", oldestDays, truncate(oldest, 40))
	}
	fmt.Println()
	return nil
}
