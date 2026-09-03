package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Filling company founding dates and headquarters from Wikidata.
//
// 144 companies were retired because their own sites publish no JSON-LD
// Organization block. That is a fact about their websites, not about the
// world: the founding year of a company is not a secret, it is simply not in
// the markup we were reading.
//
// WHY WIKIDATA AND NOT A SEARCH ENGINE. Wikidata claims are CC0, so they are
// ours to publish rather than merely to read, which is not true of the
// publisher prose a search would return. They are structured, so "founded in
// 2019" arrives as a date rather than a sentence to be parsed. And every claim
// carries an item id, so an answer can be audited later: the QID is stored
// beside the value, and a wrong claim can be traced to the exact item it came
// from. Wikipedia PROSE stays out under CC BY-SA share-alike; the claims do
// not carry that condition.
//
// WHY SEARCH-THEN-VERIFY AND NOT SPARQL. The obvious query is "give me every
// item whose official website is one of these 144 domains". It times out, and
// it deserves to: ?item wdt:P856 ?site walks every item on Wikidata that has a
// website before the domain filter can narrow anything. Searching by name and
// then checking the domain of each candidate is two small requests per company
// instead of one enormous one.
//
// NEVER MATCH ON NAME. Searching Wikidata for "Ideogram" returns Q138619 as
// its top hit, which is a linguistics term with no website at all. Accepting
// the best text match would have stamped an unrelated item's founding date
// onto an AI company's page. So a candidate is used only when its official
// website (P856) has the SAME REGISTRABLE DOMAIN as the website we already
// hold, the identical rule that governs company matching elsewhere in this
// pipeline. The domain is the hard identifier; the name is a hint for finding
// candidates and nothing more.

const wdAgent = "theworldofai.org/1.0 (contact@theworldofai.org)"

type wdFacts struct {
	qid     string
	founded int
	hqQID   string
	hqLabel string
	website string
}

func wdGet(client *http.Client, u string) (map[string]any, error) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", wdAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("wikidata HTTP %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil, fmt.Errorf("wikidata: unreadable response")
	}
	return out, nil
}

// wdClaimValue pulls the first value of a property, whatever its datatype.
func wdClaimValue(claims map[string]any, prop string) any {
	arr, _ := claims[prop].([]any)
	if len(arr) == 0 {
		return nil
	}
	first, _ := arr[0].(map[string]any)
	snak, _ := first["mainsnak"].(map[string]any)
	dv, _ := snak["datavalue"].(map[string]any)
	return dv["value"]
}

// twoaiWikidataCompany finds the Wikidata item for one company, or nothing.
func twoaiWikidataCompany(client *http.Client, name, website string) (wdFacts, error) {
	var f wdFacts
	want := twoaiRegistrableHost(website)
	if want == "" || strings.TrimSpace(name) == "" {
		return f, nil
	}
	sr, err := wdGet(client, "https://www.wikidata.org/w/api.php?action=wbsearchentities&format=json"+
		"&language=en&limit=5&search="+url.QueryEscape(name))
	if err != nil {
		return f, err
	}
	hits, _ := sr["search"].([]any)
	var ids []string
	for _, h := range hits {
		m, _ := h.(map[string]any)
		if id, ok := m["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return f, nil
	}
	er, err := wdGet(client, "https://www.wikidata.org/w/api.php?action=wbgetentities&format=json"+
		"&props=claims&ids="+url.QueryEscape(strings.Join(ids, "|")))
	if err != nil {
		return f, err
	}
	ents, _ := er["entities"].(map[string]any)
	// Candidates are checked in search order, but only the domain decides.
	for _, id := range ids {
		e, _ := ents[id].(map[string]any)
		claims, _ := e["claims"].(map[string]any)
		if claims == nil {
			continue
		}
		site, _ := wdClaimValue(claims, "P856").(string)
		if site == "" || twoaiRegistrableHost(site) != want {
			continue
		}
		f.qid, f.website = id, site
		wdFactsFromClaims(claims, &f)
		return f, nil
	}
	return f, nil
}

// wdFactsFromClaims reads founding year and headquarters item from a claims
// map into f. Shared by the name-then-domain matcher and the by-QID path.
func wdFactsFromClaims(claims map[string]any, f *wdFacts) {
	if t, ok := wdClaimValue(claims, "P571").(map[string]any); ok {
		if ts, ok := t["time"].(string); ok && len(ts) >= 5 {
			// "+2019-00-00T00:00:00Z"; the year is the only part these
			// items reliably carry, and a fabricated month would read as
			// precision we do not have.
			y := 0
			fmt.Sscanf(ts[1:5], "%d", &y)
			if y > 1800 && y <= time.Now().Year() {
				f.founded = y
			}
		}
	}
	if hq, ok := wdClaimValue(claims, "P159").(map[string]any); ok {
		if q, ok := hq["id"].(string); ok {
			f.hqQID = q
		}
	}
}

// twoaiWikidataByQID reads facts straight from a known item. No search, no
// domain test: the QID was verified by domain equality when it was stored
// (2026-09-01 bootstrap, 122 of 269 profiles), so it is the hard identifier.
func twoaiWikidataByQID(client *http.Client, qid string) (wdFacts, error) {
	var f wdFacts
	er, err := wdGet(client, "https://www.wikidata.org/w/api.php?action=wbgetentities&format=json"+
		"&props=claims&ids="+url.QueryEscape(qid))
	if err != nil {
		return f, err
	}
	ents, _ := er["entities"].(map[string]any)
	e, _ := ents[qid].(map[string]any)
	claims, _ := e["claims"].(map[string]any)
	if claims == nil {
		return f, nil
	}
	f.qid = qid
	f.website, _ = wdClaimValue(claims, "P856").(string)
	wdFactsFromClaims(claims, &f)
	return f, nil
}

// wdLabel resolves an item id to its English label, for headquarters.
func wdLabel(client *http.Client, qid string) string {
	if qid == "" {
		return ""
	}
	r, err := wdGet(client, "https://www.wikidata.org/w/api.php?action=wbgetentities&format=json"+
		"&props=labels&languages=en&ids="+url.QueryEscape(qid))
	if err != nil {
		return ""
	}
	ents, _ := r["entities"].(map[string]any)
	e, _ := ents[qid].(map[string]any)
	labels, _ := e["labels"].(map[string]any)
	en, _ := labels["en"].(map[string]any)
	s, _ := en["value"].(string)
	return s
}

// twoaiThinFillWikidata fills founding year and headquarters for companies
// whose own sites carry no structured data.
func twoaiThinFillWikidata(db *sql.DB) {
	// QID FIRST, COMPANY DOMAIN SECOND, PRODUCT URL NEVER. This stage logged
	// "0 of 60 matched" on every run for a week because it matched on the
	// profile's website column, which for the companies that matter holds the
	// PRODUCT: Anthropic is claude.ai, OpenAI is chatgpt.com, Vercel is v0.dev.
	// No Wikidata item has those as its official website, so the domain test
	// failed by design. Since 2026-09-01 the profiles carry company_domain
	// (260 of 269) and wikidata_qid (122, each verified by domain equality).
	// A known QID is read directly - no search, no guessing; otherwise the
	// name search is decided by the company domain, as before.
	rows, err := db.Query(`SELECT q.path, q.ref, c.name, COALESCE(c.company_domain,''), COALESCE(c.wikidata_qid,'')
		FROM twoai_thin_queue q JOIN twoai_company_profiles c ON c.uid = q.ref
		WHERE q.kind='company'
		  AND (COALESCE(c.company_domain,'') <> '' OR COALESCE(c.wikidata_qid,'') <> '')
		  AND (COALESCE(c.founded::text,'')='' OR COALESCE(c.headquarters,'')='')
		ORDER BY (COALESCE(c.wikidata_qid,'') <> '') DESC, q.path LIMIT 60`)
	if err != nil {
		return
	}
	type job struct{ path, uid, name, site, qid string }
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.path, &j.uid, &j.name, &j.site, &j.qid) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	if len(jobs) == 0 {
		return
	}

	client := &http.Client{Timeout: 20 * time.Second}
	matched, filled, unmatched := 0, 0, 0
	for _, j := range jobs {
		time.Sleep(700 * time.Millisecond) // courteous against a free service
		var f wdFacts
		var err error
		if j.qid != "" {
			f, err = twoaiWikidataByQID(client, j.qid)
		} else {
			f, err = twoaiWikidataCompany(client, j.name, "https://"+j.site)
		}
		if err != nil {
			continue
		}
		if f.qid == "" {
			unmatched++
			continue
		}
		matched++
		if f.hqQID != "" {
			time.Sleep(400 * time.Millisecond)
			f.hqLabel = wdLabel(client, f.hqQID)
		}
		// The QID is stored even when the item carried no founding date or
		// headquarters. That is the useful negative: it records that this
		// company WAS matched and Wikidata simply does not hold the fact, so
		// the next run does not spend two more calls rediscovering it.
		if _, err := db.Exec(`UPDATE twoai_company_profiles SET
				wikidata_qid = $2,
				founded      = COALESCE(founded, NULLIF($3,0)),
				headquarters = CASE WHEN COALESCE(headquarters,'')='' AND $4<>'' THEN $4 ELSE headquarters END,
				sources      = CASE WHEN sources @> to_jsonb($5::text) THEN sources ELSE sources || to_jsonb($5::text) END,
				verified_on  = current_date, updated_at = now()
			WHERE uid=$1`, j.uid, f.qid, f.founded, f.hqLabel,
			"https://www.wikidata.org/wiki/"+f.qid); err != nil {
			continue
		}
		if f.founded > 0 || f.hqLabel != "" {
			filled++
			db.Exec(`DELETE FROM twoai_thin_queue WHERE path=$1`, j.path)
		} else {
			thinPermanent(db, j.path,
				"neither this company's own site nor its Wikidata item publishes a founding date or headquarters")
		}
	}
	fmt.Printf("thinpages: wikidata: %d companies matched on QID or company domain, %d gained a founding date or headquarters, %d had no matching item, of %d tried\n",
		matched, filled, unmatched, len(jobs))
}
