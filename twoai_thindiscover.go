package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Finding the ONE page an operator publishes about a building, when the URL we
// hold points at their index instead.
//
// Stephen asked the obvious question: why not search a subject and scrape
// whatever the results turn up. The answer is that searching and scraping are
// two different acts, and only one of them is safe here.
//
// SEARCH IS A POINTER, NOT A SOURCE. Seventy facilities are stuck because we
// hold an operator's regional index and need the building's own page. A search
// engine finds that page in one query. What it must not do is supply the fact:
// a number lifted from the fourth result has no traceable origin, and "a page
// a search returned in August" is not an answer to "where did this come
// from?". Every figure on this site traces to a named primary source, and that
// is the whole reason it is worth citing.
//
// So this stage returns a URL and nothing else. The existing reader fetches
// it, applies the facility-versus-campus scope rules, and stores a figure only
// on an unambiguous match. Discovery widens what we can read; it does not
// widen what we will believe.
//
// THE DOMAIN RULE DOES THE REAL WORK. Only results on the operator's own
// registrable domain are considered, using the same twoaiRegistrableHost
// equality that governs company matching. A press release about a facility, a
// broker listing and a spec sheet all state megawatts and all mean different
// things; the operator's own page is the one we are entitled to read as their
// published figure. Everything else is somebody talking about them.
//
// Provider is licensed API only. Scraping a results page directly is against
// every major engine's terms, and unattended code that breaks a provider's
// terms is a poor look on a site about governance.

type discoverHit struct {
	url   string
	title string
}

// twoaiSearchWeb queries whichever licensed search API is configured. Absent a
// key the stage does nothing at all rather than falling back to scraping.
func twoaiSearchWeb(client *http.Client, q string) ([]discoverHit, error) {
	if k := os.Getenv("BRAVE_SEARCH_KEY"); k != "" {
		req, _ := http.NewRequest("GET",
			"https://api.search.brave.com/res/v1/web/search?count=10&q="+url.QueryEscape(q), nil)
		req.Header.Set("X-Subscription-Token", k)
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("brave HTTP %d", resp.StatusCode)
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var out struct {
			Web struct {
				Results []struct {
					URL   string `json:"url"`
					Title string `json:"title"`
				} `json:"results"`
			} `json:"web"`
		}
		if json.Unmarshal(b, &out) != nil {
			return nil, fmt.Errorf("brave: unreadable response")
		}
		var hits []discoverHit
		for _, r := range out.Web.Results {
			hits = append(hits, discoverHit{r.URL, r.Title})
		}
		return hits, nil
	}
	if k, cx := os.Getenv("GOOGLE_CSE_KEY"), os.Getenv("GOOGLE_CSE_CX"); k != "" && cx != "" {
		u := "https://www.googleapis.com/customsearch/v1?num=10&key=" + url.QueryEscape(k) +
			"&cx=" + url.QueryEscape(cx) + "&q=" + url.QueryEscape(q)
		resp, err := client.Get(u)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("cse HTTP %d", resp.StatusCode)
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var out struct {
			Items []struct {
				Link  string `json:"link"`
				Title string `json:"title"`
			} `json:"items"`
		}
		if json.Unmarshal(b, &out) != nil {
			return nil, fmt.Errorf("cse: unreadable response")
		}
		var hits []discoverHit
		for _, r := range out.Items {
			hits = append(hits, discoverHit{r.Link, r.Title})
		}
		return hits, nil
	}
	return nil, nil
}

// twoaiPickFacilityURL chooses the one result that is plausibly this
// building's own page, or empty.
//
// Two rules, both refusals rather than guesses. The result must sit on the
// operator's registrable domain, and it must be DEEPER than the index URL we
// already hold: an index is what we are trying to escape, so a result with the
// same or fewer path segments is the page we already have. When more than one
// candidate survives, none is returned, because picking the highest-ranked of
// several buildings is how DFW16's capacity ends up on DFW18's page.
func twoaiPickFacilityURL(hits []discoverHit, operatorURL string) string {
	want := twoaiRegistrableHost(operatorURL)
	if want == "" {
		return ""
	}
	depth := func(u string) int {
		i := strings.Index(u, "://")
		if i < 0 {
			return 0
		}
		p := u[i+3:]
		if j := strings.Index(p, "/"); j >= 0 {
			return strings.Count(strings.Trim(p[j:], "/"), "/") + 1
		}
		return 0
	}
	have := depth(operatorURL)
	var keep []string
	for _, h := range hits {
		if twoaiRegistrableHost(h.url) != want {
			continue
		}
		if depth(h.url) <= have {
			continue
		}
		keep = append(keep, h.url)
	}
	if len(keep) != 1 {
		return ""
	}
	return keep[0]
}

// twoaiThinDiscover finds facility URLs for rows retired because the website we
// hold is an operator index. The picking and matching above are pure functions
// so they stay testable without a database or a network.
func twoaiThinDiscover(db *sql.DB) {
	if os.Getenv("BRAVE_SEARCH_KEY") == "" &&
		(os.Getenv("GOOGLE_CSE_KEY") == "" || os.Getenv("GOOGLE_CSE_CX") == "") {
		fmt.Println("thinpages: discover: no search API key set, skipped")
		return
	}
	client := &http.Client{Timeout: 20 * time.Second}
	rows, err := db.Query(`SELECT q.path, q.ref, f.name, COALESCE(f.operator,''), f.website,
			COALESCE(f.city,''), COALESCE(f.state,'')
		FROM twoai_thin_queue q JOIN twoai_dc_facilities f ON f.id = q.ref
		WHERE q.kind='dc-facility'
		  AND q.unfillable_reason LIKE 'the operator publishes a regional index%'
		ORDER BY q.path LIMIT 40`)
	if err != nil {
		return
	}
	type job struct{ path, ref, name, op, site, city, state string }
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.path, &j.ref, &j.name, &j.op, &j.site, &j.city, &j.state) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()

	found, none := 0, 0
	for _, j := range jobs {
		who := j.op
		if who == "" {
			who = twoaiRegistrableHost(j.site)
		}
		q := strings.TrimSpace(who + " " + j.name + " " + j.city + " " + j.state + " data center")
		time.Sleep(1200 * time.Millisecond)
		hits, err := twoaiSearchWeb(client, q)
		if err != nil {
			continue
		}
		u := twoaiPickFacilityURL(hits, j.site)
		if u == "" {
			none++
			continue
		}
		// The query that found it is recorded beside the URL, so a wrong
		// candidate can be traced back to the search that produced it rather
		// than appearing in the row as an unexplained address.
		db.Exec(`UPDATE twoai_thin_queue SET source_url=$2, attempts=0, unfillable_reason=NULL,
				last_error='discovered by search on the operator''s own domain: '||$3
			WHERE path=$1`, j.path, u, q)
		db.Exec(`UPDATE twoai_dc_facilities SET website=$2 WHERE id=$1 AND website=$3`, j.ref, u, j.site)
		found++
	}
	fmt.Printf("thinpages: discover: %d facility URLs found on the operator's own domain, %d had no single clear match, of %d tried\n",
		found, none, len(jobs))
}
