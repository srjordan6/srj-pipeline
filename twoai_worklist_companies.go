package main

// twoaiWorklistCompanies: US-listed companies discovered as ETF constituents,
// created by TICKER, never by name.
//
// Stephen, 2026-09-11: "yes" to building the 60 US-listed pages from
// twoai_company_worklist. Every path into twoai_company_profiles until now
// started from a name in the tools catalog and matched SEC EDGAR by
// normalising that name - the right approach when the name is a vendor a
// human typed into the catalog, and the wrong one here, where the ticker
// already came from the issuer's own holdings file and is more precise than
// any name match could be. This looks up EDGAR by ticker directly.
//
// SCOPE: US-listed only, exchange IS NULL in the worklist (the Bloomberg
// exchange suffix, split out in twoai_etf_holdings). The 59 non-US rows need
// a different identifier - SEDOL to Wikidata - and are not this stage.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

func twoaiWorklistCompanies(db *sql.DB, today string) (int, error) {
	rows, err := db.Query(`SELECT uid, name, ticker FROM twoai_company_worklist
		WHERE status='pending' AND exchange IS NULL AND ticker IS NOT NULL ORDER BY ticker`)
	if err != nil {
		return 0, err
	}
	type wl struct{ uid, name, ticker string }
	var todo []wl
	for rows.Next() {
		var w wl
		if rows.Scan(&w.uid, &w.name, &w.ticker) == nil {
			todo = append(todo, w)
		}
	}
	rows.Close()
	if len(todo) == 0 {
		fmt.Println("twoai_worklist_companies: nothing pending")
		return 0, nil
	}

	type reg struct {
		CIK   int
		Title string
	}
	raw, err := twoaiJobsGet("https://www.sec.gov/files/company_tickers.json", nil)
	if err != nil {
		return 0, fmt.Errorf("edgar tickers fetch: %w", err)
	}
	// company_tickers.json is keyed by an arbitrary integer per row; the
	// ticker lives in a field on each value, never the map key. Read as
	// RawMessage first and index by the TICKER FIELD, not the file's key.
	var rawAll map[string]json.RawMessage
	if json.Unmarshal(raw, &rawAll) != nil || len(rawAll) == 0 {
		return 0, fmt.Errorf("edgar tickers parse failed")
	}
	byTicker := map[string]reg{}
	for _, v := range rawAll {
		var r struct {
			CIK    int    `json:"cik_str"`
			Ticker string `json:"ticker"`
			Title  string `json:"title"`
		}
		if json.Unmarshal(v, &r) == nil && r.Ticker != "" {
			byTicker[strings.ToUpper(r.Ticker)] = reg{CIK: r.CIK, Title: r.Title}
		}
	}

	created, skipped := 0, 0
	for _, w := range todo {
		r, ok := byTicker[strings.ToUpper(w.ticker)]
		if !ok {
			skipped++
			continue
		}
		cik10 := fmt.Sprintf("%010d", r.CIK)
		sub, err := twoaiJobsGet("https://data.sec.gov/submissions/CIK"+cik10+".json", nil)
		time.Sleep(150 * time.Millisecond) // EDGAR fair-use: <10 req/s
		if err != nil {
			fmt.Fprintf(os.Stderr, "twoai_worklist_companies: edgar submissions %s (%s): %v\n", w.ticker, cik10, err)
			skipped++
			continue
		}
		var s struct {
			Name      string   `json:"name"`
			Tickers   []string `json:"tickers"`
			Exchanges []string `json:"exchanges"`
			SIC       string   `json:"sicDescription"`
			Addresses struct {
				Business struct {
					City           string `json:"city"`
					StateOrCountry string `json:"stateOrCountry"`
				} `json:"business"`
			} `json:"addresses"`
			Filings struct {
				Recent struct {
					Form       []string `json:"form"`
					FilingDate []string `json:"filingDate"`
					Accession  []string `json:"accessionNumber"`
					PrimaryDoc []string `json:"primaryDocument"`
				} `json:"recent"`
			} `json:"filings"`
		}
		if json.Unmarshal(sub, &s) != nil || s.Name == "" {
			skipped++
			continue
		}

		keep := map[string]bool{"10-K": true, "10-Q": true, "8-K": true,
			"20-F": true, "6-K": true, "DEF 14A": true, "S-1": true}
		type filing struct{ Form, Date, URL string }
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
		exchange := ""
		if len(s.Exchanges) > 0 {
			exchange = s.Exchanges[0]
		}
		headquarters := ""
		if s.Addresses.Business.City != "" {
			headquarters = s.Addresses.Business.City
			if s.Addresses.Business.StateOrCountry != "" {
				headquarters += ", " + s.Addresses.Business.StateOrCountry
			}
		}

		fj, _ := json.Marshal(filings)
		src, _ := json.Marshal([]map[string]string{{
			"name": "SEC EDGAR", "url": "https://www.sec.gov/cgi-bin/browse-edgar?action=getcompany&CIK=" + cik10,
			"note": "registrant " + s.Name + ", found by ticker " + w.ticker + " against an AI-fund holdings file, matched " + today,
		}})
		enrich, _ := json.Marshal(map[string]any{
			"registrant": s.Name, "ticker": w.ticker, "exchange": exchange,
			"cik": cik10, "industry": s.SIC, "filings": json.RawMessage(fj),
			"verified": today,
		})

		if _, err := db.Exec(`INSERT INTO twoai_company_profiles
			(uid, name, org_type, for_profit, ticker, cik, headquarters, sources, edgar, verified_on, updated_at)
			VALUES ($1,$2,'public-company',true,$3,$4,NULLIF($5,''),$6::jsonb,$7::jsonb,$8::date, now())
			ON CONFLICT (uid) DO UPDATE SET org_type='public-company', for_profit=true,
				ticker=EXCLUDED.ticker, cik=EXCLUDED.cik, headquarters=COALESCE(EXCLUDED.headquarters, twoai_company_profiles.headquarters),
				sources=EXCLUDED.sources, edgar=EXCLUDED.edgar, verified_on=EXCLUDED.verified_on, updated_at=now()`,
			w.uid, s.Name, w.ticker, cik10, headquarters, string(src), string(enrich), today); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_worklist_companies: profile upsert %s: %v\n", w.ticker, err)
			skipped++
			continue
		}

		// Register as a tracked instrument too, so the price fetcher and the
		// rail box pick it up on the next twoai_stocks run: a company page
		// created from a fund holding should end up tracked the same way
		// the original 37 are, not as a second-class entry.
		if _, err := db.Exec(`INSERT INTO twoai_stock_instruments (uid, ticker, name, kind, exchange, company_uid, note)
			VALUES ($1,$2,$3,'company',$4,$5,$6)
			ON CONFLICT (ticker) DO UPDATE SET company_uid=EXCLUDED.company_uid`,
			twoaiUID("stock:"+w.ticker), w.ticker, s.Name, exchange, w.uid,
			"Discovered as an AI-fund constituent, registered "+today+"."); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_worklist_companies: instrument upsert %s: %v\n", w.ticker, err)
		}

		// Backfill: any holdings row already fetched for this ticker before
		// the company page existed gets the uid now, so the fund page's
		// "held by" list is complete without waiting for the next holdings
		// fetch.
		db.Exec(`UPDATE twoai_etf_holdings SET company_uid=$1 WHERE constituent_ticker=$2 AND company_uid IS NULL`, w.uid, w.ticker)

		db.Exec(`UPDATE twoai_company_worklist SET status='created', company_uid=$1, last_seen=current_date WHERE uid=$1`, w.uid)
		created++
	}
	fmt.Printf("twoai_worklist_companies: created=%d skipped=%d of %d pending ok=true\n", created, skipped, len(todo))
	return created, nil
}
