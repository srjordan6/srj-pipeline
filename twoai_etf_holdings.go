package main

// twoai_etf_holdings: which companies the tracked AI funds actually hold.
//
// Stephen, 2026-09-08 and again 09-11: every constituent of the eight tracked
// AI funds must become a company page under Public AI Companies. This stage
// is the first half - it fetches each fund's published holdings file, stores
// every constituent with its identifiers and weight, and matches what it can
// to company pages that already exist. The second half, creating pages for
// the unmatched, works from the worklist this fills.
//
// ISSUER FILES, NOT SCRAPES. Every issuer publishes a machine-readable
// holdings file; the work is knowing the current URL. Global X embeds a dated
// CSV link in the fund page - the date is in the filename, so the page is
// read on every run to find today's file rather than guessing. The other six
// issuers are wired to a fetcher only once their URL has been confirmed to
// return a CSV rather than an HTML page: on 2026-09-10 every remembered
// iShares, ROBO and ARK URL returned HTML or did not resolve, and a stage
// built on a guessed URL fails silently forever. An issuer with no confirmed
// fetcher is logged as skipped, by name, every run, so the gap stays visible.
//
// EVERY SNAPSHOT IS KEPT. Holdings change; a company entering or leaving a
// fund is itself a fact worth having. Rows are keyed on fund, as-of date and
// constituent, and nothing is deleted.
//
// MATCHING IS BY HARD IDENTIFIER. A constituent resolves to a company page
// through its ticker against twoai_stock_instruments, which carries the
// SEC-verified tickers, and nothing else: never by name. Most constituents
// are non-US (Fanuc, Keyence, ABB) and will not match here; they go to the
// worklist with their SEDOL, ticker and the fund that holds them, for a page
// to be researched and created against a hard identifier.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var etfClient = &http.Client{Timeout: 60 * time.Second}

type etfHolding struct {
	ticker, name, sedol, isin, cusip, sector, country string
	weight, shares, value                             float64
}

type etfFile struct {
	asOf    string
	url     string
	rows    []etfHolding
	skipped string // reason, when the issuer is not yet fetchable
}

func etfGet(url string) ([]byte, int, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; SRJ-Consulting-research/1.0; srjconsultingservices.com)")
	resp, err := etfClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return b, resp.StatusCode, err
}

// Global X: the fund page carries a link like
// https://assets.globalxetfs.com/funds/holdings/botz_full-holdings_20260909.csv
// Header: two title lines, then
// % of Net Assets,Ticker,Name,SEDOL,Market Price ($),Shares Held,Market Value ($)
var globalXCSV = regexp.MustCompile(`https://assets\.globalxetfs\.com/funds/holdings/[a-z]+_full-holdings_(\d{8})\.csv`)

func fetchGlobalX(ticker string) etfFile {
	page, code, err := etfGet("https://www.globalxetfs.com/funds/" + strings.ToLower(ticker) + "/")
	if err != nil || code != 200 {
		return etfFile{skipped: fmt.Sprintf("fund page %d %v", code, err)}
	}
	m := globalXCSV.FindStringSubmatch(string(page))
	if m == nil {
		return etfFile{skipped: "no holdings csv link on fund page"}
	}
	csvURL, ymd := m[0], m[1]
	body, code, err := etfGet(csvURL)
	if err != nil || code != 200 || strings.HasPrefix(strings.TrimSpace(string(body)), "<") {
		return etfFile{skipped: fmt.Sprintf("csv %d %v (html=%v)", code, err, strings.HasPrefix(strings.TrimSpace(string(body)), "<"))}
	}
	f := etfFile{asOf: ymd[:4] + "-" + ymd[4:6] + "-" + ymd[6:], url: csvURL}
	r := csv.NewReader(strings.NewReader(string(body)))
	r.FieldsPerRecord = -1
	recs, _ := r.ReadAll()
	header := -1
	for i, rec := range recs {
		if len(rec) > 3 && strings.HasPrefix(rec[0], "% of Net Assets") {
			header = i
			break
		}
	}
	if header < 0 {
		return etfFile{skipped: "header row not found in csv"}
	}
	num := func(s string) float64 {
		s = strings.NewReplacer(",", "", "$", "", "%", "").Replace(strings.TrimSpace(s))
		v, _ := strconv.ParseFloat(s, 64)
		return v
	}
	for _, rec := range recs[header+1:] {
		if len(rec) < 7 || strings.TrimSpace(rec[2]) == "" {
			continue
		}
		f.rows = append(f.rows, etfHolding{
			weight: num(rec[0]), ticker: strings.TrimSpace(rec[1]), name: strings.TrimSpace(rec[2]),
			sedol: strings.TrimSpace(rec[3]), shares: num(rec[5]), value: num(rec[6]),
		})
	}
	return f
}

func twoaiEtfHoldings(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_company_worklist (
		uid text PRIMARY KEY,
		name text NOT NULL,
		ticker text, sedol text, isin text, cusip text,
		exchange text, country text,
		held_by text[] NOT NULL DEFAULT '{}',
		first_seen date NOT NULL DEFAULT current_date,
		last_seen date NOT NULL DEFAULT current_date,
		status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','researched','created','held','duplicate')),
		company_uid text, note text)`); err != nil {
		return err
	}

	fetchers := map[string]func(string) etfFile{
		"BOTZ": fetchGlobalX, "AIQ": fetchGlobalX,
		// ARTY (iShares), ROBO, ARKQ, CHAT, IGPT, WTAI: no confirmed CSV URL
		// yet - see project_bridge, "AI STOCKS, part 1". Added as each is
		// proven to return a CSV, not before.
	}

	rows, err := db.Query(`SELECT ticker, name FROM twoai_stock_instruments WHERE kind='index_etf' AND active ORDER BY ticker`)
	if err != nil {
		return err
	}
	type fund struct{ ticker, name string }
	var funds []fund
	for rows.Next() {
		var f fund
		if rows.Scan(&f.ticker, &f.name) == nil {
			funds = append(funds, f)
		}
	}
	rows.Close()

	stored, matched, queued := 0, 0, 0
	for _, fd := range funds {
		fetch, ok := fetchers[fd.ticker]
		if !ok {
			fmt.Printf("twoai_etf_holdings: %s skipped, no confirmed holdings URL yet\n", fd.ticker)
			continue
		}
		f := fetch(fd.ticker)
		if f.skipped != "" {
			fmt.Fprintf(os.Stderr, "twoai_etf_holdings: %s skipped: %s\n", fd.ticker, f.skipped)
			continue
		}
		for _, h := range f.rows {
			key := h.sedol
			if key == "" {
				key = h.ticker
			}
			sum := sha256.Sum256([]byte("holding:" + fd.ticker + ":" + key))
			uid := hex.EncodeToString(sum[:])[:8]

			// Hard-identifier match: the constituent's ticker against the
			// SEC-verified instrument list. Name is never used.
			var companyUID sql.NullString
			if h.ticker != "" {
				db.QueryRow(`SELECT company_uid FROM twoai_stock_instruments WHERE ticker=$1 AND company_uid IS NOT NULL`, h.ticker).Scan(&companyUID)
			}
			res, err := db.Exec(`INSERT INTO twoai_etf_holdings
				(uid, etf_ticker, as_of_date, constituent_ticker, constituent_name, sedol, weight_pct, shares, market_value, company_uid, source_url)
				VALUES ($1,$2,$3::date,$4,$5,NULLIF($6,''),$7,$8,$9,$10,$11)
				ON CONFLICT (etf_ticker, as_of_date, constituent_name) DO UPDATE SET
					weight_pct=EXCLUDED.weight_pct, shares=EXCLUDED.shares, market_value=EXCLUDED.market_value,
					company_uid=COALESCE(EXCLUDED.company_uid, twoai_etf_holdings.company_uid), fetched_at=now()`,
				uid, fd.ticker, f.asOf, h.ticker, h.name, h.sedol, h.weight, h.shares, h.value, companyUID, f.url)
			if err != nil {
				fmt.Fprintln(os.Stderr, "twoai_etf_holdings insert:", err)
				continue
			}
			if n, _ := res.RowsAffected(); n > 0 {
				stored++
			}
			if companyUID.Valid {
				matched++
				continue
			}
			// Unmatched: queue for research. One row per constituent across
			// all funds, accumulating which funds hold it.
			csum := sha256.Sum256([]byte("company-candidate:" + key))
			cuid := hex.EncodeToString(csum[:])[:8]
			r2, err := db.Exec(`INSERT INTO twoai_company_worklist (uid, name, ticker, sedol, held_by)
				VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),ARRAY[$5]::text[])
				ON CONFLICT (uid) DO UPDATE SET
					last_seen=current_date,
					held_by=(SELECT array_agg(DISTINCT x) FROM unnest(twoai_company_worklist.held_by || ARRAY[$5]::text[]) x)`,
				cuid, h.name, h.ticker, h.sedol, fd.ticker)
			if err != nil {
				fmt.Fprintln(os.Stderr, "twoai_etf_holdings worklist:", err)
				continue
			}
			if n, _ := r2.RowsAffected(); n > 0 {
				queued++
			}
		}
		fmt.Printf("twoai_etf_holdings: %s as_of=%s constituents=%d\n", fd.ticker, f.asOf, len(f.rows))
	}

	var distinct, pending int
	db.QueryRow(`SELECT count(DISTINCT COALESCE(sedol, constituent_ticker, constituent_name)) FROM twoai_etf_holdings`).Scan(&distinct)
	db.QueryRow(`SELECT count(*) FROM twoai_company_worklist WHERE status='pending'`).Scan(&pending)
	fmt.Printf("twoai_etf_holdings: stored=%d matched_to_pages=%d queued=%d distinct_constituents=%d worklist_pending=%d ok=true\n",
		stored, matched, queued, distinct, pending)
	return nil
}
