package main

// ---- twoai_companyfacts: SEC EDGAR registrants and USPTO patent floors ----
//
// Fills twoai_company_profiles (public-company facts) and
// twoai_company_patents (recent patents per company), then renders the
// Public AI Companies and Patents sections and enriches each company page.
//
//   SEC EDGAR  https://www.sec.gov/files/company_tickers.json
//              https://data.sec.gov/submissions/CIK{10}.json
//     Free, keyless, 10 req/s with a declared User-Agent. Vendors are
//     matched to registrants by tight name normalisation; where the vendor
//     name and the registrant name differ (Google vs Alphabet Inc.), the
//     mapping comes from twoai_company_aliases, a table of rows each carrying
//     a human-verified note, never from fuzzy matching. A vendor with no
//     exact match and no alias is treated as not SEC-registered.
//
//   USPTO Open Data Portal  https://api.uspto.gov/api/v1/patent/applications/search
//     Free with an API key (USPTO_ODP_API_KEY from data.uspto.gov).
//     PatentsView, the original source here, decommissioned its API host
//     when it migrated into ODP on 2026-03-20. Counts are granted patents
//     since 2001 whose applicant name matches a fully normalised phrase;
//     applicant names fragment the same company across variants, so every
//     count is published as a floor with the exact phrase stored in
//     patents_source. Companies whose name is a common English word are
//     skipped rather than overcounted.
//
// The stage degrades per source: a failed EDGAR or USPTO fetch leaves
// the previous rows rendering.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

var cfSuffix = regexp.MustCompile(`\b(inc|corp|corporation|incorporated|co|company|ltd|limited|plc|holdings|holding|hldgs|se|sa|nv|ag)\b`)
var cfNonAlnum = regexp.MustCompile(`[^a-z0-9 ]`)
var cfSpaces = regexp.MustCompile(`\s+`)

func cfNorm(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, "&", "and"))
	s = cfNonAlnum.ReplaceAllString(s, " ")
	s = cfSuffix.ReplaceAllString(s, "")
	return strings.TrimSpace(cfSpaces.ReplaceAllString(s, " "))
}

// Vendor names that are common English words: a phrase match on these would
// count strangers' patents, which is worse than publishing nothing.
var cfGenericNames = map[string]bool{
	"box": true, "read": true, "rev": true, "make": true, "grain": true,
	"writer": true, "sierra": true, "pitch": true, "comet": true,
	"consensus": true, "captions": true, "altered": true, "fathom": true,
	"gamma": true, "harvey": true, "loom": true, "munch": true, "murf": true,
	"phrase": true, "pinecone": true, "splice": true, "suno": true,
	"surfer": true, "tome": true, "saul": true, "elai": true, "krea": true,
	"lovable": true, "paradox": true, "runway": true, "glean": true,
	"gong": true, "frase": true, "dify": true,
}

func twoaiCompanyFactsEnsure(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_company_patents (
		uid text NOT NULL,
		patent_id text NOT NULL,
		title text NOT NULL,
		granted date,
		fetched_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (uid, patent_id))`)
	return err
}

func twoaiCompanyFacts(db *sql.DB, today string) (int, error) {
	if err := twoaiCompanyFactsEnsure(db); err != nil {
		return 0, err
	}

	// Companies come from the directory the site already publishes, so the
	// two sections can never disagree with it about who exists.
	type vendor struct{ UID, Name string }
	var vendors []vendor
	var hub string
	if err := db.QueryRow(`SELECT data::text FROM twoai_pages WHERE path='companies/index.json'`).Scan(&hub); err != nil {
		return 0, nil
	}
	var h struct {
		Companies []struct {
			UID  string `json:"uid"`
			Name string `json:"name"`
		} `json:"companies"`
	}
	if json.Unmarshal([]byte(hub), &h) != nil || len(h.Companies) == 0 {
		return 0, nil
	}
	for _, c := range h.Companies {
		if c.UID != "" && c.Name != "" {
			vendors = append(vendors, vendor{c.UID, c.Name})
		}
	}

	aliases := map[string]string{} // uid -> edgar normalized name
	if rows, err := db.Query(`SELECT uid, edgar_norm FROM twoai_company_aliases`); err == nil {
		for rows.Next() {
			var uid, n string
			if rows.Scan(&uid, &n) == nil {
				aliases[uid] = n
			}
		}
		rows.Close()
	}

	// ---- EDGAR: match registrants, pull submissions for the matches.
	type reg struct {
		CIK    int    `json:"cik_str"`
		Ticker string `json:"ticker"`
		Title  string `json:"title"`
	}
	matched := 0
	if raw, err := twoaiJobsGet("https://www.sec.gov/files/company_tickers.json", nil); err != nil {
		fmt.Fprintf(os.Stderr, "twoai_companyfacts: edgar tickers fetch failed, keeping prior data: %v\n", err)
	} else {
		var all map[string]reg
		if json.Unmarshal(raw, &all) != nil || len(all) == 0 {
			fmt.Fprintf(os.Stderr, "twoai_companyfacts: edgar tickers parse failed, keeping prior data\n")
		} else {
			byNorm := map[string]reg{}
			for _, r := range all {
				n := cfNorm(r.Title)
				if _, seen := byNorm[n]; !seen { // first listing (usually class A) wins
					byNorm[n] = r
				}
			}
			for _, v := range vendors {
				key := cfNorm(v.Name)
				if a, ok := aliases[v.UID]; ok {
					key = a
				}
				r, ok := byNorm[key]
				if !ok {
					continue
				}
				cik10 := fmt.Sprintf("%010d", r.CIK)
				sub, err := twoaiJobsGet("https://data.sec.gov/submissions/CIK"+cik10+".json", nil)
				time.Sleep(150 * time.Millisecond) // EDGAR fair-use: <10 req/s
				if err != nil {
					fmt.Fprintf(os.Stderr, "twoai_companyfacts: edgar submissions %s (%s): %v\n", v.Name, cik10, err)
					continue
				}
				var s struct {
					Name      string   `json:"name"`
					Tickers   []string `json:"tickers"`
					Exchanges []string `json:"exchanges"`
					SIC       string   `json:"sicDescription"`
					Filings   struct {
						Recent struct {
							Form       []string `json:"form"`
							FilingDate []string `json:"filingDate"`
							Accession  []string `json:"accessionNumber"`
							PrimaryDoc []string `json:"primaryDocument"`
						} `json:"recent"`
					} `json:"filings"`
				}
				if json.Unmarshal(sub, &s) != nil || s.Name == "" {
					continue
				}
				// The filings a reader actually wants: periodic and current
				// reports and the proxy, newest first, capped at eight.
				keep := map[string]bool{"10-K": true, "10-Q": true, "8-K": true,
					"20-F": true, "6-K": true, "DEF 14A": true, "S-1": true}
				type filing struct {
					Form string `json:"form"`
					Date string `json:"date"`
					URL  string `json:"url"`
				}
				var filings []filing
				rec := s.Filings.Recent
				for i := range rec.Form {
					if !keep[rec.Form[i]] || len(filings) >= 8 {
						continue
					}
					acc := strings.ReplaceAll(rec.Accession[i], "-", "")
					url := fmt.Sprintf("https://www.sec.gov/Archives/edgar/data/%d/%s/%s", r.CIK, acc, rec.PrimaryDoc[i])
					filings = append(filings, filing{rec.Form[i], rec.FilingDate[i], url})
				}
				ticker := r.Ticker
				if len(s.Tickers) > 0 {
					ticker = s.Tickers[0]
				}
				exchange := ""
				if len(s.Exchanges) > 0 {
					exchange = s.Exchanges[0]
				}
				fj, _ := json.Marshal(filings)
				src, _ := json.Marshal([]map[string]string{{
					"name": "SEC EDGAR", "url": "https://www.sec.gov/cgi-bin/browse-edgar?action=getcompany&CIK=" + cik10,
					"note": "registrant " + s.Name + ", matched " + today,
				}})
				enrich, _ := json.Marshal(map[string]any{
					"registrant": s.Name, "ticker": ticker, "exchange": exchange,
					"cik": cik10, "industry": s.SIC, "filings": json.RawMessage(fj),
					"verified": today,
				})
				if _, err := db.Exec(`INSERT INTO twoai_company_profiles
					(uid, name, org_type, for_profit, ticker, cik, sources, edgar, verified_on, updated_at)
					VALUES ($1,$2,'public-company',true,$3,$4,$5::jsonb,$6::jsonb,$7::date, now())
					ON CONFLICT (uid) DO UPDATE SET org_type='public-company', for_profit=true,
						ticker=EXCLUDED.ticker, cik=EXCLUDED.cik, sources=EXCLUDED.sources,
						edgar=EXCLUDED.edgar, verified_on=EXCLUDED.verified_on, updated_at=now()`,
					v.UID, v.Name, ticker, cik10, string(src), string(enrich), today); err != nil {
					return matched, err
				}
				// Only ~60 of the 261 companies have full pages; the attach
				// silently matches zero rows for the rest, which is why the
				// section renders from profiles.edgar, not from the pages.
				db.Exec(`UPDATE twoai_pages SET data=jsonb_set(data,'{sec}',$1::jsonb), updated_at=now()
					WHERE path=$2`, string(enrich), "companies/"+v.UID+".json")
				matched++
			}
			fmt.Printf("twoai_companyfacts: edgar matched=%d of %d vendors\n", matched, len(vendors))
		}
	}

	// ---- USPTO Open Data Portal: granted-patent floor per company.
	//
	// PatentsView's API host was decommissioned when the project migrated to
	// the USPTO Open Data Portal on 2026-03-20, so patent facts come from the
	// ODP Patent File Wrapper search: granted patents since 2001 whose
	// applicant name matches the company. Different scope than the old
	// assignee match (post-2001 applications, applicant not assignee), and
	// the page copy says so. Requires USPTO_ODP_API_KEY from data.uspto.gov.
	odpKey := os.Getenv("USPTO_ODP_API_KEY")
	odpDone := 0
	if odpKey == "" {
		fmt.Fprintf(os.Stderr, "twoai_companyfacts: USPTO_ODP_API_KEY unset, skipping patents\n")
	} else {
		client := &http.Client{Timeout: 60 * time.Second}
		odpQuery := func(phrase string) (int, []map[string]any, error) {
			esc := strings.ReplaceAll(phrase, `"`, ``)
			// Body shape per the ODP spec and the PTAB mapping examples:
			// sort is an array of {field, order} objects, pagination is a
			// nested object, and date constraints go in rangeFilters, not
			// in q. The first sweep sent a sort string, top-level limit,
			// and a [* TO *] existence clause, and every query 400'd.
			// The grantDate range floor doubles as the "granted only,
			// since 2001" scope the pages state.
			body, _ := json.Marshal(map[string]any{
				"q": `applicationMetaData.firstApplicantName:"` + esc + `"`,
				"rangeFilters": []map[string]string{{
					"field":     "applicationMetaData.grantDate",
					"valueFrom": "2001-01-01", "valueTo": "2035-12-31",
				}},
				"sort": []map[string]string{{
					"field": "applicationMetaData.grantDate", "order": "Desc",
				}},
				"pagination": map[string]int{"offset": 0, "limit": 5},
				// firstApplicantName is requested so the applicant string the
				// match was actually made on is stored alongside the patent.
				// Without it the attribution cannot be audited after the fact,
				// which is exactly how the MCP substring bug survived: a join
				// that discards its own evidence cannot be checked.
				"fields": []string{"applicationMetaData.patentNumber",
					"applicationMetaData.inventionTitle", "applicationMetaData.grantDate",
					"applicationMetaData.firstApplicantName"},
			})
			req, _ := http.NewRequest("POST", "https://api.uspto.gov/api/v1/patent/applications/search", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-KEY", odpKey)
			req.Header.Set("User-Agent", "theworldofai.org pipeline (contact: info@srjconsultingservices.com)")
			resp, err := client.Do(req)
			if err != nil {
				return 0, nil, err
			}
			defer resp.Body.Close()
			if resp.StatusCode == 429 {
				return 0, nil, fmt.Errorf("rate limited")
			}
			// ODP answers a zero-result search with 404, not an empty 200:
			// the first fixed sweep 404'd on 01.AI, Copy.ai, and other
			// companies that plausibly hold no granted US patents, while
			// returning real counts for 16 others in the same run. A 404
			// is a finding of nothing, not a failure - and since the
			// section only lists companies with patents, a mistaken zero
			// can only omit, never fabricate.
			if resp.StatusCode == 404 {
				return 0, nil, nil
			}
			if resp.StatusCode != 200 {
				return 0, nil, fmt.Errorf("status %d", resp.StatusCode)
			}
			var out struct {
				Count     int `json:"count"`
				TotalHits int `json:"totalNumFound"`
				Bag       []struct {
					Meta struct {
						PatentNumber       string `json:"patentNumber"`
						InventionTitle     string `json:"inventionTitle"`
						GrantDate          string `json:"grantDate"`
						FirstApplicantName string `json:"firstApplicantName"`
					} `json:"applicationMetaData"`
				} `json:"patentFileWrapperDataBag"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				return 0, nil, err
			}
			total := out.TotalHits
			if total == 0 && out.Count > 0 {
				total = out.Count
			}
			var pats []map[string]any
			for _, b := range out.Bag {
				pats = append(pats, map[string]any{
					"patent_id": b.Meta.PatentNumber, "patent_title": b.Meta.InventionTitle,
					"patent_date": b.Meta.GrantDate, "applicant": b.Meta.FirstApplicantName,
				})
			}
			return total, pats, nil
		}
		hardFails := 0
		for _, v := range vendors {
			// The phrase is the fully normalised registrant name where one
			// exists ("meta platforms", never "meta platforms, ."), else the
			// normalised vendor name.
			phrase := cfNorm(v.Name)
			var registrant sql.NullString
			db.QueryRow(`SELECT edgar->>'registrant' FROM twoai_company_profiles
				WHERE uid=$1`, v.UID).Scan(&registrant)
			if registrant.Valid && registrant.String != "" {
				phrase = cfNorm(registrant.String)
			}
			if cfGenericNames[strings.ToLower(v.Name)] && !registrant.Valid {
				continue
			}
			if len(phrase) < 4 {
				continue
			}
			hits, pats, err := odpQuery(phrase)
			time.Sleep(1000 * time.Millisecond)
			if err != nil {
				hardFails++
				fmt.Fprintf(os.Stderr, "twoai_companyfacts: uspto odp %q: %v\n", phrase, err)
				if strings.Contains(err.Error(), "rate limited") || hardFails >= 5 {
					fmt.Fprintf(os.Stderr, "twoai_companyfacts: aborting patent sweep after %d failures, prior rows keep rendering\n", hardFails)
					break
				}
				continue
			}
			hardFails = 0
			if _, err := db.Exec(`INSERT INTO twoai_company_profiles (uid, name, patents_count, patents_source, updated_at)
				VALUES ($1,$2,$3,$4,now())
				ON CONFLICT (uid) DO UPDATE SET patents_count=EXCLUDED.patents_count,
					patents_source=EXCLUDED.patents_source, updated_at=now()`,
				v.UID, v.Name, hits,
				fmt.Sprintf("USPTO Open Data Portal, granted patents since 2001, applicant phrase %q, %s", phrase, today)); err != nil {
				return matched, err
			}
			db.Exec(`DELETE FROM twoai_company_patents WHERE uid=$1`, v.UID)
			for _, p := range pats {
				pid, _ := p["patent_id"].(string)
				title, _ := p["patent_title"].(string)
				date, _ := p["patent_date"].(string)
				applicant, _ := p["applicant"].(string)
				if pid == "" {
					continue
				}
				// applicant and match_phrase record WHY this row was attributed to
				// this company: the applicant string USPTO returned, and the phrase
				// we searched on. Both are needed to audit the join later.
				db.Exec(`INSERT INTO twoai_company_patents (uid, patent_id, title, granted, applicant, match_phrase)
					VALUES ($1,$2,$3,NULLIF($4,'')::date,NULLIF($5,''),$6)
					ON CONFLICT (uid, patent_id) DO UPDATE SET title=EXCLUDED.title,
						granted=EXCLUDED.granted, applicant=EXCLUDED.applicant,
						match_phrase=EXCLUDED.match_phrase`,
					v.UID, pid, title, date, applicant, phrase)
			}
			odpDone++
		}
		fmt.Printf("twoai_companyfacts: uspto odp queried=%d\n", odpDone)
	}

	// ---- Render the two sections from what SQL now holds.
	count := 0
	nameByUID := map[string]string{}
	for _, v := range vendors {
		nameByUID[v.UID] = v.Name
	}

	// Public AI Companies.
	type pub struct {
		UID      string          `json:"uid"`
		Name     string          `json:"name"`
		Ticker   string          `json:"ticker"`
		CIK      string          `json:"cik"`
		Sec      json.RawMessage `json:"sec,omitempty"`
		Industry string          `json:"industry,omitempty"`
		Exchange string          `json:"exchange,omitempty"`
		Reg      string          `json:"registrant,omitempty"`
	}
	var pubs []pub
	if rows, err := db.Query(`SELECT p.uid, p.name, COALESCE(p.ticker,''), COALESCE(p.cik,''),
			COALESCE(p.edgar->>'industry',''), COALESCE(p.edgar->>'exchange',''),
			COALESCE(p.edgar->>'registrant',''), COALESCE(p.edgar->'filings','[]')
		FROM twoai_company_profiles p
		WHERE p.org_type='public-company' AND p.cik IS NOT NULL AND p.edgar IS NOT NULL
			AND p.verified_on >= current_date - 7 ORDER BY p.name`); err == nil {
		for rows.Next() {
			var x pub
			var filings string
			if rows.Scan(&x.UID, &x.Name, &x.Ticker, &x.CIK, &x.Industry, &x.Exchange, &x.Reg, &filings) != nil {
				continue
			}
			x.Sec = json.RawMessage(filings)
			pubs = append(pubs, x)
		}
		rows.Close()
	}
	if len(pubs) > 0 {
		industries := map[string]int{}
		for _, p := range pubs {
			if p.Industry != "" {
				industries[p.Industry]++
			}
		}
		type ind struct {
			Industry string `json:"industry"`
			Count    int    `json:"count"`
		}
		var inds []ind
		for k, n := range industries {
			inds = append(inds, ind{k, n})
		}
		sort.Slice(inds, func(i, j int) bool { return inds[i].Count > inds[j].Count })
		var name, blurb string
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug='company-public'`).Scan(&name, &blurb)
		doc := map[string]any{
			"uid": twoaiUID("section:company-public"), "tax": "company-public",
			"name": name, "blurb": blurb, "companies": pubs, "total": len(pubs),
			"tracked": len(vendors), "industries": inds, "generated": today,
		}
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ('companies/public.json','company-section',$1::jsonb,'company-public',1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`, string(j)); err != nil {
			return matched, err
		}
		count++
	}

	// Patents.
	type patCo struct {
		UID    string           `json:"uid"`
		Name   string           `json:"name"`
		Count  int              `json:"count"`
		Source string           `json:"source"`
		Recent []map[string]any `json:"recent,omitempty"`
		Public bool             `json:"public"`
	}
	var patCos []patCo
	if rows, err := db.Query(`SELECT uid, name, patents_count, COALESCE(patents_source,''),
			org_type='public-company' AS is_public
		FROM twoai_company_profiles WHERE patents_count IS NOT NULL AND patents_count > 0
		ORDER BY patents_count DESC`); err == nil {
		for rows.Next() {
			var x patCo
			if rows.Scan(&x.UID, &x.Name, &x.Count, &x.Source, &x.Public) != nil {
				continue
			}
			prow, err := db.Query(`SELECT patent_id, title, COALESCE(granted::text,'')
				FROM twoai_company_patents WHERE uid=$1 ORDER BY granted DESC NULLS LAST LIMIT 5`, x.UID)
			if err == nil {
				for prow.Next() {
					var pid, title, date string
					if prow.Scan(&pid, &title, &date) == nil {
						x.Recent = append(x.Recent, map[string]any{
							"id": pid, "title": title, "date": date,
							"url": "https://patents.google.com/patent/US" + pid,
						})
					}
				}
				prow.Close()
			}
			patCos = append(patCos, x)
		}
		rows.Close()
	}
	if len(patCos) > 0 {
		totalPatents := 0
		for _, p := range patCos {
			totalPatents += p.Count
		}
		var name, blurb string
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug='company-patents'`).Scan(&name, &blurb)
		doc := map[string]any{
			"uid": twoaiUID("section:company-patents"), "tax": "company-patents",
			"name": name, "blurb": blurb, "companies": patCos, "total": len(patCos),
			"patents_total": totalPatents, "generated": today,
		}
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ('companies/patents.json','company-section',$1::jsonb,'company-patents',1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`, string(j)); err != nil {
			return matched, err
		}
		count++
		// Patent facts also belong on each company's own page.
		for _, p := range patCos {
			enrich, _ := json.Marshal(map[string]any{
				"count": p.Count, "source": p.Source, "recent": p.Recent, "verified": today,
			})
			db.Exec(`UPDATE twoai_pages SET data=jsonb_set(data,'{patents}',$1::jsonb), updated_at=now()
				WHERE path=$2`, string(enrich), "companies/"+p.UID+".json")
		}
	}
	fmt.Printf("twoai_companyfacts: sections=%d public=%d patented=%d\n", count, len(pubs), len(patCos))
	return count, nil
}
