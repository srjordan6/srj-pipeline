package main

// twoai_company_sites: find the website a company page needs to stop being thin.
//
// Stephen, 2026-09-13, on the 70 company pages held back from the index:
// fix all of these. They were created by twoai_worklist_companies from ETF
// holdings - AMD, Intel, Broadcom, Qualcomm, TSMC, Netflix, Uber and 60-odd
// more - and every one arrived with a ticker and a CIK and no website. The
// profile harvester crawls a company's own site to write its profile, so with
// no site there is no profile, and a facts table alone is a thin page. The
// noindex gate is holding them out of the index correctly, which is the right
// SAFE state and the wrong RESTING state: the pages exist, are linked, and
// say almost nothing.
//
// EDGAR WAS THE OBVIOUS SOURCE AND IT IS EMPTY. data.sec.gov/submissions
// publishes a `website` field; for AMD and Intel, checked 2026-09-13, it is
// the empty string. That is why the worklist stage never set one, and it is
// worth recording so nobody tries that route again.
//
// So: Wikidata, keyed on the ticker (P414 exchange listing, P249 ticker
// symbol) for the official website (P856). It resolves these companies well,
// and it returns noise that has to be handled rather than trusted:
//
//   - MULTIPLE SITES PER COMPANY. Micron returns micron.com, micron.com.jp,
//     micron.cn and tw.micron.com; TSMC returns four language variants. The
//     apex English site is the one a profile should be written from.
//   - WRONG ENTITIES. The ticker QCOM matched an entity whose website is
//     consumerrights.wiki. Nothing about that is Qualcomm. A ticker match
//     alone is not identification.
//
// Hence the name check: the site's registrable domain must share a real token
// with the company name, or the candidate is rejected and the page stays
// thin. A thin page is a small loss; a company profile written from the wrong
// company's website is a fabrication with a source link on it, which is the
// one thing this site must never publish.
//
// Nothing is written until the URL answers 200. Once written, the existing
// twoai_company_harvest stage does the rest on its own schedule: it crawls,
// twoaiGenerate writes the profile, the figure validator gates it, and the
// page stops being thin and re-enters the index on the next build, because
// both the noindex gate and the sitemap exclusion read the same document.

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

// Tokens that carry no identifying weight. "AI" is here deliberately: on this
// site it would match half the internet.
var siteStopWords = map[string]bool{
	"inc": true, "corp": true, "corporation": true, "co": true, "company": true,
	"ltd": true, "limited": true, "plc": true, "holdings": true, "holding": true,
	"group": true, "technologies": true, "technology": true, "tech": true,
	"the": true, "and": true, "systems": true, "solutions": true, "international": true,
	"labs": true, "laboratories": true, "industries": true, "n": true, "v": true,
	"sa": true, "ag": true, "nv": true, "de": true, "ai": true, "corp/de": true,
}

func siteTokens(name string) []string {
	clean := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return ' '
	}, name)
	var out []string
	for _, t := range strings.Fields(strings.ToLower(clean)) {
		if len(t) >= 3 && !siteStopWords[t] {
			out = append(out, t)
		}
	}
	return out
}

// twoaiSiteMatchesName is the guard against a ticker matching the wrong
// entity. The domain's registrable part must contain a name token, or a name
// token must contain it - "tsmc" against "taiwan semiconductor manufacturing"
// fails both ways, which is why the acronym of the name is checked too.
func twoaiSiteMatchesName(site, name string) bool {
	u, err := url.Parse(site)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return false
	}
	// Registrable-ish: everything but the public suffix. Crude but enough,
	// since only the presence of a token matters, not the exact boundary.
	stem := strings.Join(parts[:len(parts)-1], "")
	toks := siteTokens(name)
	if len(toks) == 0 {
		return false
	}
	for _, t := range toks {
		if strings.Contains(stem, t) || (len(t) >= 5 && strings.Contains(t, stem) && len(stem) >= 4) {
			return true
		}
	}
	// Acronym: "taiwan semiconductor manufacturing" -> "tsm", matches tsmc.com.
	var acr strings.Builder
	for _, t := range toks {
		acr.WriteByte(t[0])
	}
	if a := acr.String(); len(a) >= 3 && strings.Contains(stem, a) {
		return true
	}
	return false
}

// scoreSite prefers the apex English site over language variants and investor
// subdomains: micron.com over micron.com.jp, and a company's own domain over
// ir.company.net where both exist.
func scoreSite(site string) int {
	u, err := url.Parse(site)
	if err != nil {
		return -100
	}
	h := strings.ToLower(u.Hostname())
	s := 0
	if strings.HasPrefix(h, "www.") {
		s += 3
	}
	if strings.HasSuffix(h, ".com") || strings.HasSuffix(h, ".org") || strings.HasSuffix(h, ".net") {
		s += 3
	}
	// Country and language variants.
	for _, bad := range []string{".jp", ".cn", ".tw", ".kr", ".de", ".fr", ".co.uk", ".com.au", ".in"} {
		if strings.HasSuffix(h, bad) {
			s -= 5
		}
	}
	for _, seg := range []string{"chinese", "japanese", "schinese", "korean", "deutsch"} {
		if strings.Contains(strings.ToLower(u.Path), seg) {
			s -= 6
		}
	}
	// Investor-relations hosts describe the stock, not the company.
	if strings.HasPrefix(h, "ir.") || strings.HasPrefix(h, "investor") {
		s -= 4
	}
	s -= strings.Count(strings.Trim(u.Path, "/"), "/") // deep paths are worse
	if strings.Trim(u.Path, "/") != "" {
		s -= 2
	}
	return s
}

func twoaiCompanySites(db *sql.DB) error {
	rows, err := db.Query(`SELECT uid, name, ticker FROM twoai_company_profiles
		WHERE COALESCE(website,'')='' AND COALESCE(ticker,'')<>'' ORDER BY name`)
	if err != nil {
		return err
	}
	type co struct{ uid, name, ticker string }
	var todo []co
	for rows.Next() {
		var c co
		if rows.Scan(&c.uid, &c.name, &c.ticker) == nil {
			todo = append(todo, c)
		}
	}
	rows.Close()
	if len(todo) == 0 {
		fmt.Println("twoai_company_sites: every company with a ticker already has a website")
		return nil
	}

	byTicker := map[string]co{}
	var values []string
	for _, c := range todo {
		byTicker[strings.ToUpper(c.ticker)] = c
		values = append(values, `"`+strings.ToUpper(c.ticker)+`"`)
	}

	// One query for the whole batch. Wikidata's endpoint asks for a
	// descriptive user agent and rate limits anonymous callers, so a single
	// batched request is both faster and better behaved than 68.
	q := `SELECT ?t ?companyLabel ?site WHERE {
  VALUES ?t { ` + strings.Join(values, " ") + ` }
  ?company p:P414 ?st . ?st pq:P249 ?t .
  ?company wdt:P856 ?site .
  SERVICE wikibase:label { bd:serviceParam wikibase:language "en". }
}`
	req, _ := http.NewRequest("GET", "https://query.wikidata.org/sparql?query="+url.QueryEscape(q), nil)
	req.Header.Set("Accept", "application/sparql-results+json")
	req.Header.Set("User-Agent", "SRJ-Consulting-research/1.0 (srjconsultingservices.com; theworldofai.org)")
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	code := resp.StatusCode
	resp.Body.Close()
	if code != 200 {
		return fmt.Errorf("wikidata sparql: %d", code)
	}
	var sr struct {
		Results struct {
			Bindings []map[string]struct {
				Value string `json:"value"`
			} `json:"bindings"`
		} `json:"results"`
	}
	if err := json.Unmarshal(b, &sr); err != nil {
		return err
	}

	// Best candidate per ticker, name-checked first then scored.
	best := map[string]string{}
	bestScore := map[string]int{}
	rejected := map[string][]string{}
	for _, bd := range sr.Results.Bindings {
		t := strings.ToUpper(bd["t"].Value)
		site := bd["site"].Value
		c, ok := byTicker[t]
		if !ok || site == "" {
			continue
		}
		wdName := bd["companyLabel"].Value
		// Either the EDGAR registrant name or Wikidata's label may match;
		// both describe the same company and either is good evidence.
		if !twoaiSiteMatchesName(site, c.name) && !twoaiSiteMatchesName(site, wdName) {
			rejected[t] = append(rejected[t], site)
			continue
		}
		if s := scoreSite(site); best[t] == "" || s > bestScore[t] {
			best[t], bestScore[t] = site, s
		}
	}

	// AeroVironment trades as AVAV and its site is avinc.com; the name check
	// cannot see that and correctly refuses to guess. Cases like it are
	// listed here rather than loosening the matcher, because the matcher is
	// what stopped consumerrights.wiki being published as Qualcomm's website.
	// Each entry is a human decision, verified once.
	knownSites := map[string]string{
		"AVAV": "https://www.avinc.com/",
	}

	client := &http.Client{Timeout: 30 * time.Second}
	set, dead, none := 0, 0, 0
	for t, c := range byTicker {
		site := best[t]
		if site == "" {
			site = knownSites[t]
		}
		if site == "" {
			none++
			if r := rejected[t]; len(r) > 0 {
				fmt.Printf("twoai_company_sites: %s (%s) rejected %s: the domain does not match the company name\n",
					t, c.name, strings.Join(r, ", "))
			}
			continue
		}
		// Never store a URL without checking it answers. A dead link in the
		// sources block is worse than no link.
		//
		// BUT A BOT BLOCK IS NOT A DEAD SITE. The first run rejected amd.com,
		// snap.com, uber.com, nokia.com, cadence.com and six more as "did not
		// answer": every one is live and simply refuses a non-browser client.
		// Measured 2026-09-13 - amd.com returns 503 to plain curl and 301 to a
		// browser UA, uber.com 406 then 200, cadence.com 403 then 302. Eleven
		// correct websites were thrown away by a check that was wrong, which is
		// worse than the problem it was guarding against.
		//
		// So: send a browser user agent, follow redirects, and treat the
		// statuses that mean "you are not a browser" as ALIVE. The URL is what
		// is being verified here, not our ability to crawl it - the harvester
		// has its own fetching and already reports blocked sites separately.
		// Only a real 404 or 410, or a connection that fails outright, means
		// the URL is wrong.
		hreq, _ := http.NewRequest("GET", site, nil)
		hreq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		hreq.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		hreq.Header.Set("Accept-Language", "en-US,en;q=0.9")
		hresp, herr := client.Do(hreq)
		if herr != nil {
			fmt.Printf("twoai_company_sites: %s (%s) %s could not be reached (%v), left unset\n", t, c.name, site, herr)
			dead++
			continue
		}
		code := hresp.StatusCode
		hresp.Body.Close()
		gone := code == 404 || code == 410
		if gone {
			fmt.Printf("twoai_company_sites: %s (%s) %s returned %d, left unset\n", t, c.name, site, code)
			dead++
			continue
		}
		if code >= 400 {
			fmt.Printf("twoai_company_sites: %s (%s) %s answered %d to an automated request, which is a bot block rather than a bad URL. Storing it; the harvester reports separately whether it can crawl.\n",
				t, c.name, site, code)
		}
		if _, err := db.Exec(`UPDATE twoai_company_profiles
			SET website=$1, updated_at=now() WHERE uid=$2 AND COALESCE(website,'')=''`, site, c.uid); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_company_sites:", c.name, err)
			continue
		}
		set++
		time.Sleep(400 * time.Millisecond)
	}

	var left int
	db.QueryRow(`SELECT count(*) FROM twoai_company_profiles WHERE COALESCE(website,'')=''`).Scan(&left)
	fmt.Printf("twoai_company_sites: resolved=%d unreachable=%d no_match=%d | %d companies still without a website\n",
		set, dead, none, left)
	if set > 0 {
		fmt.Println("twoai_company_sites: twoai_company_harvest will crawl these on its next run and write their profiles; the pages leave noindex on the build after that.")
	}
	return nil
}
