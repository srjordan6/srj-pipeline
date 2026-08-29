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
// Twelve dockets per run, least recently scanned first, three pages of
// documents per docket, so a full sweep of the tracker takes about nine days
// and then stays current as new filings land. A docket whose documents carry
// no extracted text produces nothing, and that is the correct output; text
// that was never read is never cited. Uses the same clGet client, token and
// Retry-After discipline as the docket refresh.
func twoaiRecapCitations(db *sql.DB) error {
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
	rows, err := db.Query(`SELECT l.slug, l.courtlistener_url
		FROM ai_lawsuits l
		LEFT JOIN twoai_recap_scan s ON s.lawsuit_slug = l.slug
		WHERE l.is_active AND COALESCE(l.courtlistener_url,'') ~ '/docket/\d+'
		ORDER BY s.scanned_at ASC NULLS FIRST, l.filed_date DESC NULLS LAST
		LIMIT 12`)
	if err != nil {
		return err
	}
	type job struct {
		slug   string
		docket string
	}
	var jobs []job
	for rows.Next() {
		var slug, cu string
		if rows.Scan(&slug, &cu) == nil {
			if m := docketRe.FindStringSubmatch(cu); m != nil {
				jobs = append(jobs, job{slug, m[1]})
			}
		}
	}
	rows.Close()

	totalHits, totalDocs, totalText := 0, 0, 0
	rateLimited := 0 // consecutive rate-limited page fetches across dockets
	done := 0
	for _, j := range jobs {
		if rateLimited >= 2 {
			// CourtListener is throttling this token right now. Sleeping into
			// the stage deadline harvests nothing and burns eight minutes of
			// cron, which is exactly what the first production run did. Stop
			// cleanly; unscanned dockets keep their place at the front of the
			// rotation and tomorrow's budget is fresh.
			fmt.Printf("twoai_recap: rate limited, stopping after %d of %d dockets\n", done, len(jobs))
			break
		}
		seen, withText, hits := 0, 0, 0
		pagesOK := 0
		next := "/recap-documents/"
		params := map[string]string{
			"docket_entry__docket": j.docket,
			"fields":               "id,description,plain_text,is_available,absolute_url",
			"page_size":            "20",
			"order_by":             "-id",
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
				fmt.Println("twoai_recap:", j.slug, "page", page, err)
				if strings.Contains(err.Error(), "rate limited") {
					rateLimited++
				}
				break
			}
			pagesOK++
			rateLimited = 0
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
					if _, err := db.Exec(`INSERT INTO twoai_precedent_citations
						(lawsuit_slug, precedent_slug, recap_doc_id, doc_description, doc_url, quoted_by, snippet)
						VALUES ($1,$2,$3,$4,$5,$6,$7)
						ON CONFLICT (lawsuit_slug, precedent_slug, recap_doc_id) DO NOTHING`,
						j.slug, p.slug, d.ID, strings.TrimSpace(d.Description), docURL, by, snippet); err != nil {
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
			time.Sleep(1200 * time.Millisecond)
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
	fmt.Printf("twoai_recap: dockets=%d docs=%d with_text=%d new_hits=%d total: lawsuits=%d links=%d\n",
		len(jobs), totalDocs, totalText, totalHits, lawsuits, links)
	return nil
}
