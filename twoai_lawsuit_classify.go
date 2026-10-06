package main

// Classify tracked AI lawsuits by the court's own case type. 2026-09-21.
//
// intel promotes every new case as 'unclassified', and nothing read the docket
// afterwards, so Buist v. Anthropic PBC et al sat unclassified although the
// court files it under nature of suit 410 Anti-Trust. 27 cases were in that
// state. The federal civil cover sheet's nature of suit code is chosen by the
// filer and recorded by the clerk; CourtListener returns it on a docket search.
//
// ONLY UNAMBIGUOUS CODES ARE MAPPED. 820 Copyright is copyright. 360 "P.I.:
// Other" is not product liability, 440 "Civil Rights: Other" is not hiring
// discrimination, and 190 "Contract: Other" has no tracker category, so those
// stay unclassified for a person to decide. A category already set by a person
// is never touched: only rows still 'unclassified' are read.
//
// Rides at the start of twoai_lawsuit_fill, which runs directly behind intel,
// so a case promoted today is classified in the same run.
//
// SHARED CLIENT AND A TURN ORDER, 2026-10-05. This stage had its own client
// with a fixed 5, 10, 15 second ladder that ignored Retry-After, so behind
// intel, which had just been throttled, two of 13 reads came back HTTP 429
// and nothing was learned. It now reads through clFetch with a three minute
// budget, and cases are taken oldest nos_checked_at first, stamped as soon
// as CourtListener answers, so a run that stops on the budget leaves the
// unread ones first in line and the ones left for review go to the back.
//
// CACHE FIRST, 2026-10-06 (bridge row 518). The classifier has 10
// CourtListener calls a day (courtlistener_ledger.go). A nature of suit
// already in twoai_cl_docket_cache, which the docket refresh fills, costs
// nothing; a docket record is fetched at most once a UTC day across every
// stage; and a docket whose cached record carries no nature of suit is not
// asked again for thirty days, only searched.

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var lawsuitNOSCategory = map[string]string{
	"410": "antitrust",
	// One intellectual property category since 2026-09-21; the kind of right
	// is written as a tag, from lawsuitNOSTag below.
	"820": "intellectual property",
	"830": "intellectual property", "835": "intellectual property",
	"840": "intellectual property",
	"880": "trade secrets",
	"365": "product liability & wrongful death", "367": "product liability & wrongful death", "385": "product liability & wrongful death",
	"850": "securities fraud",
	"370": "consumer protection", "371": "consumer protection", "480": "consumer protection", "485": "consumer protection",
}

var lawsuitNOSTag = map[string]string{"820": "copyright", "830": "patent", "835": "patent", "840": "trademark"}

var lawsuitDocketIDRe = regexp.MustCompile(`courtlistener\.com/docket/(\d+)`)

func twoaiLawsuitClassify(db *sql.DB) error {
	// The page writer behind this step needs most of the stage's thirty
	// minutes, so CourtListener gets three of them.
	clUse(db, clBucketClassify)
	clSetBudget(3 * time.Minute)
	db.Exec(`ALTER TABLE ai_lawsuits ADD COLUMN IF NOT EXISTS nos_checked_at timestamptz`)
	rows, err := db.Query(`SELECT slug, courtlistener_url FROM ai_lawsuits
		WHERE category = 'unclassified' AND courtlistener_url LIKE '%courtlistener.com/docket/%'
		ORDER BY nos_checked_at ASC NULLS FIRST, slug`)
	if err != nil {
		return err
	}
	type c struct{ slug, id string }
	var cs []c
	for rows.Next() {
		var slug, u string
		if rows.Scan(&slug, &u) == nil {
			if m := lawsuitDocketIDRe.FindStringSubmatch(u); m != nil {
				cs = append(cs, c{slug, m[1]})
			}
		}
	}
	rows.Close()
	tok := os.Getenv("COURTLISTENER_TOKEN")
	read, classified, left := 0, 0, 0
	stopped := false
	for _, x := range cs {
		// The docket endpoint is the direct read and needs the token the
		// pipeline already holds; the search endpoint is the fallback. The
		// first run used search only and 12 of 22 came back empty, most likely
		// throttled, which the old code could not tell from no data.
		nos, status, err := lawsuitNOSFromDocket(db, tok, x.id)
		if nos == "" && !clIsBudget(err) {
			var s2 int
			nos, s2, err = lawsuitNOSFromSearch(x.id)
			if status == 0 {
				status = s2
			}
		}
		if clIsBudget(err) {
			// Out of CourtListener time for this run. Not stamped, so this
			// case and the ones after it are first in line next run.
			stopped = true
			break
		}
		read++
		// CourtListener answered one way or another, so the case goes to the
		// back of the line, a code left for review included.
		db.Exec(`UPDATE ai_lawsuits SET nos_checked_at = now() WHERE slug = $1`, x.slug)
		if nos == "" {
			fmt.Printf("twoai_lawsuit_classify: %s: no nature of suit returned (http %d)\n", x.slug, status)
			continue
		}
		code := strings.SplitN(nos, " ", 2)[0]
		cat, ok := lawsuitNOSCategory[code]
		if !ok {
			left++
			continue
		}
		if _, err := db.Exec(`UPDATE ai_lawsuits SET category = $2,
			tags = CASE WHEN $3::text = '' OR $3::text = ANY(COALESCE(tags,'{}')) THEN tags ELSE array_append(COALESCE(tags,'{}'), $3::text) END,
			updated_at = now()
			WHERE slug = $1 AND category = 'unclassified'`, x.slug, cat, lawsuitNOSTag[code]); err == nil {
			classified++
			fmt.Printf("twoai_lawsuit_classify: %s -> %s (nature of suit %s)\n", x.slug, cat, nos)
		} else {
			fmt.Printf("twoai_lawsuit_classify: %s: %v\n", x.slug, err)
		}
	}
	if stopped {
		fmt.Printf("twoai_lawsuit_classify: rate limited, %d of %d dockets done, rest next run ok=true\n", read, len(cs))
	}
	fmt.Printf("twoai_lawsuit_classify: unclassified=%d read=%d classified=%d left_for_review=%d ok=true\n", len(cs), read, classified, left)
	fmt.Println(clReportLine(db))
	return nil
}

// lawsuitCLGet reads one CourtListener URL through the shared client and
// returns the HTTP status alongside the error, so a run can still tell an
// empty answer from a refusal.
func lawsuitCLGet(u string, into any) (int, error) {
	return clFetch(u, into)
}

// lawsuitNOSFromDocket reads the nature of suit from the docket record,
// from twoai_cl_docket_cache when it holds one (status 200, no call made).
func lawsuitNOSFromDocket(db *sql.DB, tok, id string) (string, int, error) {
	if rec, at, ok := clCachedDocket(db, id); ok {
		if nos := strings.TrimSpace(rec.NatureOfSuit); nos != "" {
			return nos, 200, nil
		}
		if time.Since(at) < 30*24*time.Hour {
			// Asked within the month and the court recorded none; the
			// search fallback is the only other place to look.
			return "", 200, nil
		}
	}
	if tok == "" {
		return "", 0, nil
	}
	rec, s, err := clDocket(db, id)
	return strings.TrimSpace(rec.NatureOfSuit), s, err
}

func lawsuitNOSFromSearch(id string) (string, int, error) {
	var d struct {
		Results []struct {
			SuitNature string `json:"suitNature"`
		} `json:"results"`
	}
	q := url.Values{"type": {"r"}, "q": {"docket_id:" + id}}
	s, err := lawsuitCLGet(clAPIBase+"/search/?"+q.Encode(), &d)
	if len(d.Results) == 0 {
		return "", s, err
	}
	return strings.TrimSpace(d.Results[0].SuitNature), s, err
}
