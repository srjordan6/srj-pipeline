package main

// ---- twoai_orgfacts: org types and SEC Form D funding disclosures ---------
//
// Finishes the AI Companies domain with what free, lawful sources can carry:
//
//   Org types  twoai_org_classifications is a curated table, one row per
//     company, each carrying a human-verified note and a source URL checked
//     before insertion. The stage syncs it into twoai_company_profiles and
//     renders the Research Labs, University Labs, Government Labs, and
//     Nonprofits sections from it. Nothing is classified by inference.
//
//   Funding  SEC Form D filings (exempt-offering disclosures) via EDGAR
//     full-text search. The trap here is third-party SPVs: a search for
//     "Anthropic" returns over a hundred filings, nearly all feeder funds
//     with the target's name in theirs. Only filings whose ISSUER name
//     normalises to exactly the company name are kept. Absence of a Form D
//     means "no US exempt-offering disclosure on record", never "no
//     funding", and the pages say so. Amounts are reported per filing,
//     never summed, because amendments restate the same offering.
//
//   Private AI Companies  membership by verified elimination: a directory
//     company that is not an EDGAR registrant and not classified as a lab,
//     nonprofit, or foundation is private.
//
// Acquisitions stays unbuilt: no lawful free structured source.

import (
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

var ofCIK = regexp.MustCompile(`\(CIK\s+(\d+)\)`)

var ofIssuerSuffix = []string{"inc", "corp", "llc", "lp", "pbc", "co", "ltd",
	"company", "corporation", "incorporated", "limited"}

func ofNorm(s string) string {
	s = cfNorm(s) // lowercase, strip punctuation and the shared suffix list
	for _, suf := range ofIssuerSuffix {
		s = strings.TrimSuffix(strings.TrimSpace(s), " "+suf)
	}
	return strings.TrimSpace(s)
}

func twoaiOrgFactsEnsure(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_company_formd (
		uid text NOT NULL,
		accession text NOT NULL,
		issuer text NOT NULL,
		filed date,
		first_sale date,
		total_offering numeric,
		total_sold numeric,
		amendment boolean NOT NULL DEFAULT false,
		doc_url text NOT NULL,
		fetched_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (uid, accession))`)
	return err
}

func twoaiOrgFacts(db *sql.DB, today string) (int, error) {
	if err := twoaiOrgFactsEnsure(db); err != nil {
		return 0, err
	}

	// ---- Sync curated classifications into profiles.
	if _, err := db.Exec(`INSERT INTO twoai_company_profiles (uid, name, org_type, for_profit, sources, verified_on, updated_at)
		SELECT c.uid,
			(SELECT x->>'name' FROM twoai_pages, jsonb_array_elements(data->'companies') x
				WHERE path='companies/index.json' AND x->>'uid'=c.uid LIMIT 1),
			c.org_type, c.for_profit,
			jsonb_build_array(jsonb_build_object('name','Curated classification','url',c.source_url,'note',c.note)),
			c.verified_on, now()
		FROM twoai_org_classifications c
		ON CONFLICT (uid) DO UPDATE SET org_type=EXCLUDED.org_type, for_profit=EXCLUDED.for_profit,
			sources=EXCLUDED.sources, verified_on=EXCLUDED.verified_on, updated_at=now()`); err != nil {
		return 0, err
	}

	// ---- Directory and current public set.
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
	publicSet := map[string]bool{}
	classified := map[string]string{} // uid -> org_type
	if rows, err := db.Query(`SELECT uid, org_type FROM twoai_company_profiles
		WHERE org_type='public-company' AND cik IS NOT NULL AND verified_on >= current_date - 7`); err == nil {
		for rows.Next() {
			var uid, t string
			if rows.Scan(&uid, &t) == nil {
				publicSet[uid] = true
			}
		}
		rows.Close()
	}
	if rows, err := db.Query(`SELECT uid, org_type FROM twoai_org_classifications`); err == nil {
		for rows.Next() {
			var uid, t string
			if rows.Scan(&uid, &t) == nil {
				classified[uid] = t
			}
		}
		rows.Close()
	}

	// ---- Form D sweep over private companies.
	//
	// Name collisions are real: six directory companies share an exact
	// issuer name with an unrelated filer (Coda Automotive vs coda.io,
	// Cognition Corp of Lexington MA vs Cognition AI, and so on). Verified
	// collisions live in twoai_formd_exclusions by uid+CIK and are skipped
	// here; each row records how it was verified.
	excluded := map[string]bool{}
	if rows, err := db.Query(`SELECT uid, cik FROM twoai_formd_exclusions`); err == nil {
		for rows.Next() {
			var uid, cik string
			if rows.Scan(&uid, &cik) == nil {
				excluded[uid+":"+strings.TrimLeft(cik, "0")] = true
			}
		}
		rows.Close()
	}
	type formd struct {
		Accession, Issuer, Filed, FirstSale, DocURL string
		TotalOffering, TotalSold                    float64
		Amendment                                   bool
	}
	swept, matchedCos := 0, 0
	for _, v := range vendors {
		if publicSet[v.UID] || classified[v.UID] != "" {
			continue
		}
		if len(ofNorm(v.Name)) < 4 {
			continue
		}
		q := url.QueryEscape(`"` + v.Name + `"`)
		raw, err := twoaiJobsGet("https://efts.sec.gov/LATEST/search-index?q="+q+"&forms=D", nil)
		time.Sleep(200 * time.Millisecond)
		if err != nil {
			fmt.Fprintf(os.Stderr, "twoai_orgfacts: formd search %q: %v\n", v.Name, err)
			continue
		}
		var res struct {
			Hits struct {
				Hits []struct {
					ID     string `json:"_id"`
					Source struct {
						DisplayNames []string `json:"display_names"`
						FileDate     string   `json:"file_date"`
					} `json:"_source"`
				} `json:"hits"`
			} `json:"hits"`
		}
		if json.Unmarshal(raw, &res) != nil {
			continue
		}
		target := ofNorm(v.Name)
		var filings []formd
		for _, hit := range res.Hits.Hits {
			// The CIK is only carried inline in the display name, e.g.
			// "Groq LLC  (CIK 0001686725)" - there is no _source.cik field.
			issuerOK, issuerName, cik := false, "", ""
			for _, dn := range hit.Source.DisplayNames {
				clean := dn
				if i := strings.Index(clean, "(CIK"); i > 0 {
					if m := ofCIK.FindStringSubmatch(dn); m != nil {
						cik = strings.TrimLeft(m[1], "0")
					}
					clean = clean[:i]
				}
				if ofNorm(clean) == target && cik != "" && !excluded[v.UID+":"+cik] {
					issuerOK, issuerName = true, strings.TrimSpace(clean)
					break
				}
			}
			if !issuerOK || len(filings) >= 8 {
				continue
			}
			parts := strings.SplitN(hit.ID, ":", 2)
			if len(parts) != 2 {
				continue
			}
			acc := parts[0]
			docURL := "https://www.sec.gov/Archives/edgar/data/" + cik + "/" + strings.ReplaceAll(acc, "-", "") + "/" + parts[1]
			xraw, err := twoaiJobsGet(docURL, nil)
			time.Sleep(150 * time.Millisecond)
			if err != nil {
				continue
			}
			var fd struct {
				Offering struct {
					Type struct {
						IsAmendment bool `xml:"newOrAmendment>isAmendment"`
						FirstSale   struct {
							Value string `xml:"value"`
						} `xml:"dateOfFirstSale"`
					} `xml:"typeOfFiling"`
					Amounts struct {
						TotalOffering string `xml:"totalOfferingAmount"`
						TotalSold     string `xml:"totalAmountSold"`
					} `xml:"offeringSalesAmounts"`
				} `xml:"offeringData"`
			}
			if xml.Unmarshal(xraw, &fd) != nil {
				continue
			}
			var offering, sold float64
			fmt.Sscanf(fd.Offering.Amounts.TotalOffering, "%f", &offering)
			fmt.Sscanf(fd.Offering.Amounts.TotalSold, "%f", &sold)
			filings = append(filings, formd{
				Accession: acc, Issuer: issuerName, Filed: hit.Source.FileDate,
				FirstSale: fd.Offering.Type.FirstSale.Value, DocURL: docURL,
				TotalOffering: offering, TotalSold: sold,
				Amendment: fd.Offering.Type.IsAmendment,
			})
		}
		swept++
		if len(filings) == 0 {
			continue
		}
		// NEVER-DELETE: filings persist even when a later SEC search no longer
		// returns them; the (uid, accession) upsert keeps returned rows fresh and
		// fetched_at dates the ones that stopped appearing.
		for _, f := range filings {
			db.Exec(`INSERT INTO twoai_company_formd
				(uid, accession, issuer, filed, first_sale, total_offering, total_sold, amendment, doc_url)
				VALUES ($1,$2,$3,NULLIF($4,'')::date,NULLIF($5,'')::date,$6,$7,$8,$9)
				ON CONFLICT (uid, accession) DO UPDATE SET issuer=EXCLUDED.issuer, filed=EXCLUDED.filed,
					first_sale=EXCLUDED.first_sale, total_offering=EXCLUDED.total_offering,
					total_sold=EXCLUDED.total_sold, amendment=EXCLUDED.amendment,
					doc_url=EXCLUDED.doc_url, fetched_at=now()`,
				v.UID, f.Accession, f.Issuer, f.Filed, f.FirstSale, f.TotalOffering, f.TotalSold, f.Amendment, f.DocURL)
		}
		matchedCos++
	}
	fmt.Printf("twoai_orgfacts: formd swept=%d matched_companies=%d\n", swept, matchedCos)

	// ---- Render.
	count := 0
	nameOf := map[string]string{}
	for _, v := range vendors {
		nameOf[v.UID] = v.Name
	}
	upsertSection := func(path, tax string, doc map[string]any) error {
		var name, blurb string
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug=$1`, tax).Scan(&name, &blurb)
		doc["uid"] = twoaiUID("section:" + tax)
		doc["tax"] = tax
		doc["name"] = name
		doc["blurb"] = blurb
		doc["generated"] = today
		j, _ := json.Marshal(doc)
		_, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'company-section',$2::jsonb,$3,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`, path, string(j), tax)
		return err
	}

	// Latest Form D per company, for the private-companies section rows.
	type fdSummary struct {
		Filed     string  `json:"filed"`
		Sold      float64 `json:"sold"`
		Offering  float64 `json:"offering"`
		Amendment bool    `json:"amendment"`
		URL       string  `json:"url"`
		Issuer    string  `json:"issuer"`
		N         int     `json:"filings"`
	}
	latestFD := map[string]*fdSummary{}
	if rows, err := db.Query(`SELECT DISTINCT ON (uid) uid, COALESCE(filed::text,''), COALESCE(total_sold,0),
			COALESCE(total_offering,0), amendment, doc_url, issuer,
			(SELECT count(*) FROM twoai_company_formd f2 WHERE f2.uid=f.uid)
		FROM twoai_company_formd f ORDER BY uid, filed DESC NULLS LAST`); err == nil {
		for rows.Next() {
			var uid string
			var s fdSummary
			if rows.Scan(&uid, &s.Filed, &s.Sold, &s.Offering, &s.Amendment, &s.URL, &s.Issuer, &s.N) == nil {
				latestFD[uid] = &s
			}
		}
		rows.Close()
	}

	// Private AI Companies.
	type privCo struct {
		UID   string     `json:"uid"`
		Name  string     `json:"name"`
		FormD *fdSummary `json:"formd,omitempty"`
	}
	var privs []privCo
	for _, v := range vendors {
		if publicSet[v.UID] || classified[v.UID] != "" {
			continue
		}
		privs = append(privs, privCo{v.UID, v.Name, latestFD[v.UID]})
	}
	sort.Slice(privs, func(i, j int) bool {
		a, b := privs[i].FormD != nil, privs[j].FormD != nil
		if a != b {
			return a
		}
		if a && b && privs[i].FormD.Filed != privs[j].FormD.Filed {
			return privs[i].FormD.Filed > privs[j].FormD.Filed
		}
		return privs[i].Name < privs[j].Name
	})
	withFD := 0
	for _, p := range privs {
		if p.FormD != nil {
			withFD++
		}
	}
	if len(privs) > 0 {
		if err := upsertSection("companies/private.json", "company-startups", map[string]any{
			"companies": privs, "total": len(privs), "with_formd": withFD,
			"tracked": len(vendors), "public": len(publicSet),
		}); err != nil {
			return count, err
		}
		count++
	}

	// The four classified sections.
	classDocs := map[string][]map[string]any{}
	if rows, err := db.Query(`SELECT uid, org_type, note, source_url, verified_on::text
		FROM twoai_org_classifications ORDER BY org_type, uid`); err == nil {
		for rows.Next() {
			var uid, typ, note, src, ver string
			if rows.Scan(&uid, &typ, &note, &src, &ver) != nil {
				continue
			}
			tax := map[string]string{
				"research-lab": "company-research-labs", "university-lab": "company-university-labs",
				"government-lab": "company-government-labs", "nonprofit": "company-nonprofits",
				"open-source-foundation": "company-nonprofits",
			}[typ]
			if tax == "" || nameOf[uid] == "" {
				continue
			}
			classDocs[tax] = append(classDocs[tax], map[string]any{
				"uid": uid, "name": nameOf[uid], "org_type": typ,
				"note": note, "source_url": src, "verified": ver,
			})
		}
		rows.Close()
	}
	for tax, orgs := range classDocs {
		if err := upsertSection("companies/"+strings.TrimPrefix(tax, "company-")+".json", tax, map[string]any{
			"orgs": orgs, "total": len(orgs),
		}); err != nil {
			return count, err
		}
		count++
	}

	// Funding: every filing, newest first.
	type filingRow struct {
		UID       string  `json:"uid"`
		Name      string  `json:"name"`
		Issuer    string  `json:"issuer"`
		Filed     string  `json:"filed"`
		FirstSale string  `json:"first_sale,omitempty"`
		Sold      float64 `json:"sold"`
		Offering  float64 `json:"offering"`
		Amendment bool    `json:"amendment"`
		URL       string  `json:"url"`
	}
	var allFilings []filingRow
	if rows, err := db.Query(`SELECT uid, issuer, COALESCE(filed::text,''), COALESCE(first_sale::text,''),
			COALESCE(total_sold,0), COALESCE(total_offering,0), amendment, doc_url
		FROM twoai_company_formd ORDER BY filed DESC NULLS LAST LIMIT 200`); err == nil {
		for rows.Next() {
			var f filingRow
			if rows.Scan(&f.UID, &f.Issuer, &f.Filed, &f.FirstSale, &f.Sold, &f.Offering, &f.Amendment, &f.URL) == nil {
				f.Name = nameOf[f.UID]
				allFilings = append(allFilings, f)
			}
		}
		rows.Close()
	}
	if len(allFilings) > 0 {
		if err := upsertSection("companies/funding.json", "company-funding", map[string]any{
			"filings": allFilings, "total": len(allFilings), "companies": matchedCos,
		}); err != nil {
			return count, err
		}
		count++
		// Attach the filing list to company pages that exist.
		byUID := map[string][]filingRow{}
		for _, f := range allFilings {
			byUID[f.UID] = append(byUID[f.UID], f)
		}
		for uid, fs := range byUID {
			enrich, _ := json.Marshal(map[string]any{"filings": fs, "verified": today})
			db.Exec(`UPDATE twoai_pages SET data=jsonb_set(data,'{formd}',$1::jsonb), updated_at=now()
				WHERE path=$2`, string(enrich), "companies/"+uid+".json")
		}
	}
	fmt.Printf("twoai_orgfacts: sections=%d private=%d formd_companies=%d classified=%d\n",
		count, len(privs), matchedCos, len(classified))
	return count, nil
}
