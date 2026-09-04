package main

// twoai_capex_ma.go - two sections grounded in SEC primary sources.
//
// Compute Telemetry (obs-gpu-availability): what can be lawfully measured
// about AI compute is money and disclosures, not spot GPU availability -
// provider quota APIs report only one's own account, so they are refused.
// This page tracks quarterly capital expenditure from the seven companies
// whose capex IS the AI buildout (NVIDIA, Microsoft, Amazon, Alphabet,
// Meta, AMD, Supermicro), read from each company's own XBRL facts on
// data.sec.gov. Companies tag capex differently (NVIDIA retired the
// standard tag in 2020 for PaymentsToAcquireProductiveAssets), so the
// fetcher resolves per-company concepts and keeps whichever is current.
//
// Acquisitions (company-acquisitions): 8-K items metadata from the EDGAR
// submissions API - Item 2.01 (completed acquisition or disposition) and
// Item 1.01 (material definitive agreement) - across every tracked SEC
// registrant. The items say an event class occurred; the page links the
// filing rather than summarizing it, and says plainly that private-to-
// private deals are invisible to this method.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

var twoaiCapexCompanies = []struct {
	CIK  string
	Name string
}{
	{"0001045810", "NVIDIA"},
	{"0000789019", "Microsoft"},
	{"0001018724", "Amazon"},
	{"0001652044", "Alphabet"},
	{"0001326801", "Meta"},
	{"0000002488", "AMD"},
	{"0001375365", "Super Micro Computer"},
	// The data center builders proper, added for the Data Centers section:
	// the two colocation giants, Oracle whose OCI buildout is capex-visible,
	// and CoreWeave, the pure-play AI cloud. CIKs verified against EDGAR's
	// company_tickers.json on 2026-08-29.
	{"0001101239", "Equinix"},
	{"0001297996", "Digital Realty"},
	{"0001341439", "Oracle"},
	{"0001769628", "CoreWeave"},
}

var twoaiCapexConcepts = []string{
	"PaymentsToAcquirePropertyPlantAndEquipment",
	"PaymentsToAcquireProductiveAssets",
	// Data center REITs report building spend as development of real estate,
	// not acquisition of equipment: Digital Realty files 140 quarters of
	// PaymentsToDevelopRealEstateAssets and nothing current under the two
	// concepts above (verified against its companyfacts, 2026-08-29). Listed
	// last so it can never preempt PP&E for an operator that files both.
	"PaymentsToDevelopRealEstateAssets",
}

func twoaiCapexMAEnsure(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_capex (
		cik text NOT NULL, name text NOT NULL, concept text NOT NULL,
		start_date text NOT NULL, end_date text NOT NULL,
		val double precision NOT NULL, form text NOT NULL,
		fetched_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (cik, end_date, start_date))`); err != nil {
		return err
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_ma_filings (
		cik text NOT NULL, company text NOT NULL, accession text NOT NULL,
		filed text NOT NULL, items text NOT NULL, doc_url text NOT NULL,
		fetched_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (cik, accession))`)
	return err
}

func twoaiCapexMA(db *sql.DB, today string) (int, error) {
	if err := twoaiCapexMAEnsure(db); err != nil {
		return 0, err
	}
	client := &http.Client{Timeout: 25 * time.Second}
	get := func(url string, out any) error {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("User-Agent", "theworldofai.org pipeline (contact: info@srjconsultingservices.com)")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}

	// ---- Capex: per company, resolve the concept that is currently in use
	// and store quarterly-duration facts. Failures keep prior rows.
	capexCos, capexFacts := 0, 0
	capexLatest := ""
	for _, c := range twoaiCapexCompanies {
		type fact struct {
			Start string  `json:"start"`
			End   string  `json:"end"`
			Val   float64 `json:"val"`
			Form  string  `json:"form"`
		}
		bestConcept, bestEnd := "", ""
		var bestFacts []fact
		for _, concept := range twoaiCapexConcepts {
			var doc struct {
				Units map[string][]fact `json:"units"`
			}
			url := "https://data.sec.gov/api/xbrl/companyconcept/CIK" + c.CIK + "/us-gaap/" + concept + ".json"
			if err := get(url, &doc); err != nil {
				continue
			}
			usd := doc.Units["USD"]
			if len(usd) == 0 {
				continue
			}
			maxEnd := ""
			for _, f := range usd {
				if f.End > maxEnd {
					maxEnd = f.End
				}
			}
			if maxEnd > bestEnd {
				bestEnd, bestConcept, bestFacts = maxEnd, concept, usd
			}
			time.Sleep(400 * time.Millisecond)
		}
		if bestConcept == "" {
			fmt.Fprintf(os.Stderr, "twoai_capex: %s no capex concept resolved, keeping prior rows\n", c.Name)
			continue
		}
		kept := 0
		for _, f := range bestFacts {
			if f.Form != "10-Q" && f.Form != "10-K" {
				continue
			}
			st, e1 := time.Parse("2006-01-02", f.Start)
			en, e2 := time.Parse("2006-01-02", f.End)
			if e1 != nil || e2 != nil {
				continue
			}
			days := en.Sub(st).Hours() / 24
			if days < 75 || days > 115 { // quarterly durations only
				continue
			}
			if f.End < "2023-01-01" {
				continue
			}
			db.Exec(`INSERT INTO twoai_capex (cik, name, concept, start_date, end_date, val, form)
				VALUES ($1,$2,$3,$4,$5,$6,$7)
				ON CONFLICT (cik, end_date, start_date) DO UPDATE SET val=EXCLUDED.val,
					concept=EXCLUDED.concept, form=EXCLUDED.form, fetched_at=now()`,
				c.CIK, c.Name, bestConcept, f.Start, f.End, f.Val, f.Form)
			kept++
		}
		// One line per company said the same eleven things every run and
		// buried the lines that change. The run summary below carries the
		// totals; a company that resolves NO capex concept still warns on
		// stderr above, because that is the case worth reading.
		capexCos++
		capexFacts += kept
		if bestEnd > capexLatest {
			capexLatest = bestEnd
		}
	}
	fmt.Printf("twoai_capex: companies=%d quarterly_facts=%d latest=%s\n", capexCos, capexFacts, capexLatest)

	// ---- M&A: 8-K items sweep across tracked registrants + the capex seven.
	ciks := map[string]string{}
	for _, c := range twoaiCapexCompanies {
		ciks[c.CIK] = c.Name
	}
	rows, err := db.Query(`SELECT COALESCE(cik,''), name FROM twoai_company_profiles WHERE edgar IS NOT NULL AND COALESCE(cik,'')<>''`)
	if err == nil {
		for rows.Next() {
			var cik, name string
			if rows.Scan(&cik, &name) == nil && cik != "" {
				for len(cik) < 10 {
					cik = "0" + cik
				}
				if _, dup := ciks[cik]; !dup {
					ciks[cik] = name
				}
			}
		}
		rows.Close()
	}
	swept, found := 0, 0
	for cik, name := range ciks {
		var sub struct {
			Filings struct {
				Recent struct {
					Form      []string `json:"form"`
					Items     []string `json:"items"`
					Filed     []string `json:"filingDate"`
					Accession []string `json:"accessionNumber"`
				} `json:"recent"`
			} `json:"filings"`
		}
		if err := get("https://data.sec.gov/submissions/CIK"+cik+".json", &sub); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_ma: %s submissions fetch failed: %v\n", name, err)
			time.Sleep(400 * time.Millisecond)
			continue
		}
		r := sub.Filings.Recent
		for i := range r.Form {
			if r.Form[i] != "8-K" || r.Filed[i] < "2024-01-01" {
				continue
			}
			items := r.Items[i]
			if !strings.Contains(items, "1.01") && !strings.Contains(items, "2.01") {
				continue
			}
			accNo := strings.ReplaceAll(r.Accession[i], "-", "")
			url := "https://www.sec.gov/Archives/edgar/data/" + strings.TrimLeft(cik, "0") + "/" + accNo + "/"
			db.Exec(`INSERT INTO twoai_ma_filings (cik, company, accession, filed, items, doc_url)
				VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (cik, accession) DO UPDATE SET
					filed=EXCLUDED.filed, items=EXCLUDED.items, fetched_at=now()`,
				cik, name, r.Accession[i], r.Filed[i], items, url)
			found++
		}
		swept++
		time.Sleep(400 * time.Millisecond)
	}
	fmt.Printf("twoai_ma: swept=%d registrants filings_1.01_or_2.01=%d\n", swept, found)

	// ---- Render both pages.
	count := 0
	writeDoc := func(slug, path, kind string, doc map[string]any) error {
		doc["uid"] = twoaiUID("section:" + slug)
		doc["tax"] = slug
		doc["generated"] = today
		var name, blurb string
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug=$1`, slug).Scan(&name, &blurb)
		doc["name"] = name
		doc["blurb"] = blurb
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,$2,$3::jsonb,$4,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			path, kind, string(j), slug); err != nil {
			return err
		}
		count++
		return nil
	}

	type cpx struct {
		Name    string           `json:"name"`
		Concept string           `json:"concept"`
		Latest  float64          `json:"latest"`
		End     string           `json:"end"`
		Series  []map[string]any `json:"series"`
	}
	var comps []cpx
	var latestTotal float64
	crows, err := db.Query(`SELECT DISTINCT name FROM twoai_capex ORDER BY name`)
	if err == nil {
		var names []string
		for crows.Next() {
			var n string
			if crows.Scan(&n) == nil {
				names = append(names, n)
			}
		}
		crows.Close()
		for _, n := range names {
			var x cpx
			x.Name = n
			srows, err := db.Query(`SELECT concept, end_date, val FROM twoai_capex WHERE name=$1 ORDER BY end_date DESC LIMIT 10`, n)
			if err != nil {
				continue
			}
			for srows.Next() {
				var concept, end string
				var val float64
				if srows.Scan(&concept, &end, &val) == nil {
					if x.End == "" {
						x.End, x.Latest, x.Concept = end, val, concept
					}
					x.Series = append(x.Series, map[string]any{"end": end, "val": val})
				}
			}
			srows.Close()
			if x.End != "" {
				comps = append(comps, x)
				latestTotal += x.Latest
			}
		}
	}
	if len(comps) > 0 {
		if err := writeDoc("obs-gpu-availability", "observatory/obs-gpu-availability.json", "obs-section", map[string]any{
			"companies": comps, "latest_total": latestTotal,
			"lambda_tracked": true, "shapekind": "capex",
			"refs": []map[string]string{
				{"name": "MLCommons MLPerf datacenter results", "url": "https://mlcommons.org/benchmarks/inference-datacenter/", "note": "Verified hardware configurations and cluster sizes from actual vendor submissions"},
				{"name": "Epoch AI open datasets", "url": "https://epoch.ai/data", "note": "Paper-backed compute scaling and cluster estimates, openly licensed"},
			},
		}); err != nil {
			return count, err
		}
	}

	type maRow struct {
		Company string `json:"company"`
		Filed   string `json:"filed"`
		Items   string `json:"items"`
		URL     string `json:"url"`
	}
	var completed, agreements []maRow
	mrows, err := db.Query(`SELECT company, filed, items, doc_url FROM twoai_ma_filings ORDER BY filed DESC LIMIT 200`)
	if err == nil {
		for mrows.Next() {
			var m maRow
			if mrows.Scan(&m.Company, &m.Filed, &m.Items, &m.URL) == nil {
				if strings.Contains(m.Items, "2.01") {
					completed = append(completed, m)
				} else {
					agreements = append(agreements, m)
				}
			}
		}
		mrows.Close()
	}
	if len(agreements) > 60 {
		agreements = agreements[:60]
	}
	if len(completed) > 0 || len(agreements) > 0 {
		if err := writeDoc("company-acquisitions", "companies/acquisitions.json", "company-ma", map[string]any{
			"completed": completed, "agreements": agreements,
			"registrants": len(ciks),
		}); err != nil {
			return count, err
		}
	}
	return count, nil
}
