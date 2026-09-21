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

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var lawsuitNOSCategory = map[string]string{
	"410": "antitrust",
	"820": "copyright",
	"830": "patent", "835": "patent",
	"880": "trade secrets",
	"365": "product liability & wrongful death", "367": "product liability & wrongful death", "385": "product liability & wrongful death",
	"850": "securities fraud",
	"370": "consumer protection", "371": "consumer protection", "480": "consumer protection", "485": "consumer protection",
}

var lawsuitDocketIDRe = regexp.MustCompile(`courtlistener\.com/docket/(\d+)`)

func twoaiLawsuitClassify(db *sql.DB) error {
	rows, err := db.Query(`SELECT slug, courtlistener_url FROM ai_lawsuits
		WHERE category = 'unclassified' AND courtlistener_url LIKE '%courtlistener.com/docket/%'`)
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
	client := &http.Client{Timeout: 30 * time.Second}
	tok := os.Getenv("COURTLISTENER_TOKEN")
	read, classified, left := 0, 0, 0
	for _, x := range cs {
		// The docket endpoint is the direct read and needs the token the
		// pipeline already holds; the search endpoint is the fallback. The
		// first run used search only and 12 of 22 came back empty, most likely
		// throttled, which the old code could not tell from no data.
		nos, status := lawsuitNOSFromDocket(client, tok, x.id)
		if nos == "" {
			var s2 int
			nos, s2 = lawsuitNOSFromSearch(client, tok, x.id)
			if status == 0 {
				status = s2
			}
		}
		time.Sleep(700 * time.Millisecond)
		read++
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
		if _, err := db.Exec(`UPDATE ai_lawsuits SET category = $2, updated_at = now()
			WHERE slug = $1 AND category = 'unclassified'`, x.slug, cat); err == nil {
			classified++
			fmt.Printf("twoai_lawsuit_classify: %s -> %s (nature of suit %s)\n", x.slug, cat, nos)
		} else {
			fmt.Printf("twoai_lawsuit_classify: %s: %v\n", x.slug, err)
		}
	}
	fmt.Printf("twoai_lawsuit_classify: unclassified=%d read=%d classified=%d left_for_review=%d ok=true\n", len(cs), read, classified, left)
	return nil
}

func lawsuitCLGet(client *http.Client, tok, u string, into any) int {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "theworldofai.org lawsuit tracker (srj@srjconsultingservices.com)")
	if tok != "" {
		req.Header.Set("Authorization", "Token "+tok)
	}
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := client.Do(req)
		if err != nil {
			return 0
		}
		if resp.StatusCode == 429 {
			resp.Body.Close()
			time.Sleep(time.Duration(5*(attempt+1)) * time.Second)
			continue
		}
		if resp.StatusCode == 200 {
			json.NewDecoder(resp.Body).Decode(into)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	return 429
}

func lawsuitNOSFromDocket(client *http.Client, tok, id string) (string, int) {
	if tok == "" {
		return "", 0
	}
	var d struct {
		NatureOfSuit string `json:"nature_of_suit"`
	}
	s := lawsuitCLGet(client, tok, "https://www.courtlistener.com/api/rest/v4/dockets/"+id+"/?fields=nature_of_suit", &d)
	return strings.TrimSpace(d.NatureOfSuit), s
}

func lawsuitNOSFromSearch(client *http.Client, tok, id string) (string, int) {
	var d struct {
		Results []struct {
			SuitNature string `json:"suitNature"`
		} `json:"results"`
	}
	q := url.Values{"type": {"r"}, "q": {"docket_id:" + id}}
	s := lawsuitCLGet(client, tok, "https://www.courtlistener.com/api/rest/v4/search/?"+q.Encode(), &d)
	if len(d.Results) == 0 {
		return "", s
	}
	return strings.TrimSpace(d.Results[0].SuitNature), s
}
