package main

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// VERIFIED QUOTATIONS, DOCUMENT BY DOCUMENT. The precedent pages answer the
// question Stephen actually asked: which decisions are the live AI cases
// quoting. The lawsuit rows hold no brief text and the district-court orders
// live in RECAP as documents rather than in the opinion index, so the only
// honest way to that answer is to read the filings. This stage does exactly
// that: it walks the RECAP documents on each tracked docket, scans whatever
// extracted text CourtListener holds for each precedent's curated pattern
// (twoai_precedents.match_re, editable in SQL), and records every hit with
// the document it came from, so a claim on the site is a claim with a docket
// citation behind it.
//
// Least recently scanned first, three pages of documents per docket, as many
// dockets as the stage budget allows (up to recapMaxDockets), each recorded in
// twoai_recap_scan the moment it is done. A docket whose documents carry no
// extracted text produces nothing, and that is the correct output; text that
// was never read is never cited. Uses the shared clGet client, which waits
// out CourtListener's short throttles while the budget allows.
//
// It used to take twelve dockets and stop at the second throttled page. On
// 2026-10-05 that was the second docket: CourtListener asked for 47 seconds,
// the old client called that a spent quota, and the stage ended after 14
// seconds of its eight minutes with 10 of 12 dockets unread.
//
// RATIONED, 2026-10-06 (bridge row 518). The stage has 25 CourtListener
// calls a day (courtlistener_ledger.go) and runs every pass until they are
// spent. Dockets with a filing since their last scan go first, read from the
// tracker's own latest_development_date and the docket record cache, then
// dockets never scanned, then the oldest scans; a docket scanned in the last
// fourteen days with nothing filed since is skipped. A docket scanned before
// is asked only for the documents modified since that scan
// (date_modified__gt), so most dockets cost one page.
const recapMaxDockets = 40

// recapSkipDays is how long a docket with nothing new filed is left alone.
const recapSkipDays = 14

func twoaiRecapCitations(db *sql.DB) error {
	clUse(db, clBucketRecap)
	// Stop CourtListener work 45 seconds before the stage deadline, so the
	// last scan row and the summary line are always written.
	clSetStageDeadline("twoai_recap", 45*time.Second)
	type prec struct {
		slug string
		re   *regexp.Regexp
	}
	var precs []prec
	prows, err := db.Query(`SELECT slug, match_re FROM twoai_precedents
		WHERE status='live' AND COALESCE(match_re,'')<>''`)
	if err != nil {
		return err
	}
	for prows.Next() {
		var slug, pat string
		if prows.Scan(&slug, &pat) != nil {
			continue
		}
		re, rerr := regexp.Compile(pat)
		if rerr != nil {
			fmt.Println("twoai_recap: bad match_re for", slug, rerr)
			continue
		}
		precs = append(precs, prec{slug, re})
	}
	prows.Close()
	if len(precs) == 0 {
		fmt.Println("twoai_recap: no precedent patterns, nothing to do")
		return nil
	}

	docketRe := regexp.MustCompile(`/docket/(\d+)`)
	// changed: the docket refresh has answered since the last scan and
	// found a filing on or after the day of that scan, by the tracker's
	// timeline or the cached docket record. Tying it to the refresh keeps a
	// docket with a filing today from being scanned again on every pass.
	rows, err := db.Query(`WITH j AS (
			SELECT l.slug, l.courtlistener_url, s.scanned_at, l.filed_date,
				s.scanned_at IS NOT NULL AND l.docket_ok_at > s.scanned_at AND GREATEST(l.latest_development_date,
					CASE WHEN c.body->>'date_last_filing' ~ '^\d{4}-\d{2}-\d{2}' THEN left(c.body->>'date_last_filing',10)::date END)
					>= (s.scanned_at AT TIME ZONE 'UTC')::date AS changed
			FROM ai_lawsuits l
			LEFT JOIN twoai_recap_scan s ON s.lawsuit_slug = l.slug
			LEFT JOIN twoai_cl_docket_cache c ON c.docket_id::text = substring(l.courtlistener_url from '/docket/(\d+)')
			WHERE l.is_active AND COALESCE(l.courtlistener_url,'') ~ '/docket/\d+')
		SELECT slug, courtlistener_url, scanned_at FROM j
		WHERE scanned_at IS NULL OR changed IS TRUE OR scanned_at < now() - make_interval(days => $2)
		ORDER BY changed IS TRUE DESC, scanned_at ASC NULLS FIRST, filed_date DESC NULLS LAST
		LIMIT $1`, recapMaxDockets, recapSkipDays)
	if err != nil {
		return err
	}
	type job struct {
		slug    string
		docket  string
		scanned time.Time
	}
	var jobs []job
	for rows.Next() {
		var slug, cu string
		var at sql.NullTime
		if rows.Scan(&slug, &cu, &at) == nil {
			if m := docketRe.FindStringSubmatch(cu); m != nil {
				jobs = append(jobs, job{slug, m[1], at.Time})
			}
		}
	}
	rows.Close()

	totalHits, totalDocs, totalText := 0, 0, 0
	done := 0
	outOfBudget := false
	for _, j := range jobs {
		if outOfBudget {
			// The budget ran out on the last docket. Sleeping into the stage
			// deadline harvests nothing and gets the stage killed; unscanned
			// dockets keep their place at the front of the rotation.
			break
		}
		seen, withText, hits := 0, 0, 0
		pagesOK := 0
		next := "/recap-documents/"
		params := map[string]string{
			"docket_entry__docket": j.docket,
			"fields":               "id,description,plain_text,is_available,absolute_url",
			"page_size":            "20", // the endpoint's fixed page size
			"order_by":             "-id",
		}
		if !j.scanned.IsZero() {
			// Only what changed since the last scan, an hour of overlap.
			params["date_modified__gt"] = j.scanned.Add(-time.Hour).UTC().Format(time.RFC3339)
		}
		for page := 0; page < 3 && next != ""; page++ {
			var out struct {
				Next    string `json:"next"`
				Results []struct {
					ID          int64  `json:"id"`
					Description string `json:"description"`
					PlainText   string `json:"plain_text"`
					AbsoluteURL string `json:"absolute_url"`
				} `json:"results"`
			}
			if err := clGet(next, params, &out); err != nil {
				if clIsBudget(err) {
					// Pages already read are kept: they are the newest
					// documents, and the scan row below records them.
					outOfBudget = true
				} else {
					fmt.Println("twoai_recap:", j.slug, "page", page, err)
				}
				break
			}
			pagesOK++
			// clGet takes path-relative requests; a "next" URL from the API is
			// absolute, so after page one we pass its path and drop our params.
			for _, d := range out.Results {
				seen++
				txt := d.PlainText
				if len(txt) < 2000 {
					continue // cover sheets, notices, unscanned PDFs
				}
				withText++
				lower := strings.ToLower(d.Description)
				by := "filing"
				switch {
				case strings.Contains(lower, "order") || strings.Contains(lower, "opinion") ||
					strings.Contains(lower, "judgment") || strings.Contains(lower, "findings"):
					by = "court"
				case strings.Contains(lower, "opposition") || strings.Contains(lower, "complaint") ||
					strings.Contains(lower, "plaintiff"):
					by = "plaintiff"
				case strings.Contains(lower, "motion to dismiss") || strings.Contains(lower, "answer") ||
					strings.Contains(lower, "defendant") || strings.Contains(lower, "reply"):
					by = "defense"
				}
				for _, p := range precs {
					loc := p.re.FindStringIndex(txt)
					if loc == nil {
						continue
					}
					start := loc[0] - 70
					if start < 0 {
						start = 0
					}
					end := loc[1] + 90
					if end > len(txt) {
						end = len(txt)
					}
					snippet := strings.Join(strings.Fields(txt[start:end]), " ")
					docURL := d.AbsoluteURL
					if docURL != "" && !strings.HasPrefix(docURL, "http") {
						docURL = "https://www.courtlistener.com" + docURL
					}
					// INVALID UTF-8 KILLS THE STAGE. A RECAP filing arrived with a
					// 0x80 byte on 2026-09-13 - a mis-encoded PDF extraction is the
					// usual cause - and Postgres refused the row, which aborted the
					// whole sweep rather than the one citation. archive_news hit the
					// identical error on July 31 and guards with ToValidUTF8; this
					// path never did. The raw text is CourtListener's own record and
					// stays with them; replacing the bad byte here loses nothing that
					// could be read anyway.
					if _, err := db.Exec(`INSERT INTO twoai_precedent_citations
						(lawsuit_slug, precedent_slug, recap_doc_id, doc_description, doc_url, quoted_by, snippet)
						VALUES ($1,$2,$3,$4,$5,$6,$7)
						ON CONFLICT (lawsuit_slug, precedent_slug, recap_doc_id) DO NOTHING`,
						j.slug, p.slug, d.ID,
						strings.ToValidUTF8(strings.TrimSpace(d.Description), "\uFFFD"),
						docURL, by,
						strings.ToValidUTF8(snippet, "\uFFFD")); err != nil {
						return err
					}
					hits++
				}
			}
			if out.Next == "" {
				next = ""
			} else {
				next = strings.TrimPrefix(out.Next, "https://www.courtlistener.com/api/rest/v4")
				params = map[string]string{}
			}
		}
		if pagesOK == 0 {
			// Page zero never answered, so nothing about this docket was
			// learned. Writing a scan row here would send it to the back of a
			// nine-day rotation as the price of an API hiccup; leaving it
			// unwritten keeps it first in line tomorrow.
			continue
		}
		done++
		if _, err := db.Exec(`INSERT INTO twoai_recap_scan
			(lawsuit_slug, docket_id, scanned_at, docs_seen, docs_with_text, hits)
			VALUES ($1, $2, now(), $3, $4, $5)
			ON CONFLICT (lawsuit_slug) DO UPDATE SET docket_id=EXCLUDED.docket_id,
				scanned_at=now(), docs_seen=EXCLUDED.docs_seen,
				docs_with_text=EXCLUDED.docs_with_text, hits=EXCLUDED.hits`,
			j.slug, j.docket, seen, withText, hits); err != nil {
			return err
		}
		totalDocs += seen
		totalText += withText
		totalHits += hits
	}
	var lawsuits, links int
	db.QueryRow(`SELECT count(DISTINCT lawsuit_slug), count(*) FROM twoai_precedent_citations`).Scan(&lawsuits, &links)
	if outOfBudget {
		fmt.Printf("twoai_recap: rate limited, %d of %d dockets done, rest next run ok=true\n", done, len(jobs))
	}
	fmt.Printf("twoai_recap: dockets=%d of %d docs=%d with_text=%d new_hits=%d total: lawsuits=%d links=%d\n",
		done, len(jobs), totalDocs, totalText, totalHits, lawsuits, links)
	fmt.Println(clReportLine(db))
	return nil
}
