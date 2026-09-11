package main

// twoai_stocks: daily closing prices for every tracked AI security.
//
// Stephen, 2026-09-08: a stock call-out box on every publicly traded AI
// company page, the closing price history of each stock tracked, and a box on
// the Public AI Companies section for the funds that track AI stocks.
//
// SOURCE. Twelve Data, free Basic plan: 800 API credits a day, 8 requests a
// minute, US-listed instruments. 45 instruments cost 45 credits for a daily
// refresh, about 6% of the allowance, so the history can be seeded and then
// topped up daily without ever approaching the limit. Attribution is required
// by their terms and is rendered on every page that shows a price.
//
// THE FREE TIER IS US-LISTED ONLY, and that is a deliberate accepted limit,
// not an oversight. Every instrument tracked today is US-listed. When the ETF
// constituents land - Fanuc, Keyence, ABB and several hundred others - those
// companies get a page with NO price box, and the page says prices are not
// tracked for that listing rather than showing a blank a reader would read as
// zero. If a free non-US source is ever found it slots in behind the same
// interface without touching the tables or the pages.
//
// WHY THE HISTORY IS STORED RATHER THAN FETCHED ON RENDER. A price shown
// without a date is a rumour. Every row carries its trade date, its source and
// the moment it was fetched, so a page can say what it knows and when it knew
// it, and a chart can be drawn from our own record rather than from a live
// call that might fail mid-build.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	twoaiStockSource = "Twelve Data"
	// Eight requests a minute on the free plan. Batches of 8 symbols cost 8
	// credits but count as ONE request, so a batch per minute is both inside
	// the request limit and the cheapest shape available.
	twoaiStockBatch = 8
)

var twoaiStockClient = &http.Client{Timeout: 60 * time.Second}

// twoaiStocks fetches daily closes and upserts them. An instrument with no
// history is seeded with 250 trading days, enough to draw a line through;
// after that five days a run covers weekends and holidays without refetching
// what we already hold.
func twoaiStocks(db *sql.DB) error {
	key := strings.TrimSpace(os.Getenv("STOCK_API_KEY"))
	if key == "" {
		fmt.Fprintln(os.Stderr, "twoai_stocks: STOCK_API_KEY unset, skipping (no prices will be published)")
		return nil
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_stock_closes (
		uid text NOT NULL, ticker text NOT NULL, trade_date date NOT NULL,
		close numeric(14,4) NOT NULL,
		open numeric(14,4), high numeric(14,4), low numeric(14,4), volume bigint,
		source text NOT NULL, fetched_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (ticker, trade_date))`); err != nil {
		return err
	}

	type inst struct {
		uid, ticker string
		have        int
	}
	rows, err := db.Query(`SELECT i.uid, i.ticker,
		(SELECT count(*) FROM twoai_stock_closes c WHERE c.ticker = i.ticker)
		FROM twoai_stock_instruments i WHERE i.active ORDER BY i.ticker`)
	if err != nil {
		return err
	}
	var all []inst
	for rows.Next() {
		var x inst
		if rows.Scan(&x.uid, &x.ticker, &x.have) == nil {
			all = append(all, x)
		}
	}
	rows.Close()
	if len(all) == 0 {
		fmt.Println("twoai_stocks: no active instruments")
		return nil
	}

	saved, failed := 0, 0
	byTicker := map[string]string{}
	for _, x := range all {
		byTicker[x.ticker] = x.uid
	}

	for i := 0; i < len(all); i += twoaiStockBatch {
		end := i + twoaiStockBatch
		if end > len(all) {
			end = len(all)
		}
		batch := all[i:end]

		// Seed length is decided per batch by its emptiest member, because one
		// request covers the whole batch. Asking for 250 days when only one
		// instrument is new costs the same single request.
		size := 5
		for _, x := range batch {
			if x.have == 0 {
				size = 250
				break
			}
		}
		syms := make([]string, 0, len(batch))
		for _, x := range batch {
			syms = append(syms, x.ticker)
		}
		url := fmt.Sprintf(
			"https://api.twelvedata.com/time_series?symbol=%s&interval=1day&outputsize=%d&apikey=%s",
			strings.Join(syms, ","), size, key)

		resp, err := twoaiStockClient.Get(url)
		if err != nil {
			fmt.Fprintf(os.Stderr, "twoai_stocks: batch %s: %v\n", strings.Join(syms, ","), err)
			failed += len(batch)
			continue
		}
		var raw json.RawMessage
		derr := json.NewDecoder(resp.Body).Decode(&raw)
		resp.Body.Close()
		if derr != nil {
			fmt.Fprintf(os.Stderr, "twoai_stocks: batch decode: %v\n", derr)
			failed += len(batch)
			continue
		}

		// A single symbol returns the series object directly; several return a
		// map keyed by symbol. Both shapes are handled rather than assuming the
		// batch shape, because a batch of one is a real case at the tail.
		type series struct {
			Meta struct {
				Symbol string `json:"symbol"`
			} `json:"meta"`
			Values []struct {
				Datetime string `json:"datetime"`
				Open     string `json:"open"`
				High     string `json:"high"`
				Low      string `json:"low"`
				Close    string `json:"close"`
				Volume   string `json:"volume"`
			} `json:"values"`
			Status  string `json:"status"`
			Message string `json:"message"`
			Code    int    `json:"code"`
		}
		seriesFor := map[string]series{}
		if len(batch) == 1 {
			var s series
			if json.Unmarshal(raw, &s) == nil {
				seriesFor[batch[0].ticker] = s
			}
		} else {
			var m map[string]series
			if json.Unmarshal(raw, &m) == nil {
				for k, v := range m {
					seriesFor[k] = v
				}
			}
		}
		if len(seriesFor) == 0 {
			fmt.Fprintf(os.Stderr, "twoai_stocks: batch returned no series: %.200s\n", raw)
			failed += len(batch)
			continue
		}

		num := func(s string) (float64, bool) {
			if s == "" {
				return 0, false
			}
			f, perr := strconv.ParseFloat(s, 64)
			return f, perr == nil
		}
		for ticker, s := range seriesFor {
			uid := byTicker[ticker]
			if s.Status == "error" || len(s.Values) == 0 {
				if s.Message != "" {
					fmt.Fprintf(os.Stderr, "twoai_stocks: %s: %s\n", ticker, s.Message)
				}
				failed++
				continue
			}
			for _, v := range s.Values {
				cl, ok := num(v.Close)
				if !ok || len(v.Datetime) < 10 {
					continue
				}
				var op, hi, lo any
				if f, ok2 := num(v.Open); ok2 {
					op = f
				}
				if f, ok2 := num(v.High); ok2 {
					hi = f
				}
				if f, ok2 := num(v.Low); ok2 {
					lo = f
				}
				var vol any
				if n, nerr := strconv.ParseInt(v.Volume, 10, 64); nerr == nil {
					vol = n
				}
				// Prices are restated by the vendor after splits and
				// dividends, so an existing row is updated rather than kept:
				// the newest fetch is the vendor's current view of that day.
				// The WHERE clause stops an unchanged row being rewritten,
				// which is the same no-op-write guard twoai_pages needed.
				if _, uerr := db.Exec(`INSERT INTO twoai_stock_closes
					(uid, ticker, trade_date, close, open, high, low, volume, source)
					VALUES ($1,$2,$3::date,$4,$5,$6,$7,$8,$9)
					ON CONFLICT (ticker, trade_date) DO UPDATE SET
						close=EXCLUDED.close, open=EXCLUDED.open, high=EXCLUDED.high,
						low=EXCLUDED.low, volume=EXCLUDED.volume, fetched_at=now()
					WHERE twoai_stock_closes.close IS DISTINCT FROM EXCLUDED.close`,
					uid, ticker, v.Datetime[:10], cl, op, hi, lo, vol, twoaiStockSource); uerr != nil {
					fmt.Fprintf(os.Stderr, "twoai_stocks: upsert %s %s: %v\n", ticker, v.Datetime[:10], uerr)
					continue
				}
				saved++
			}
		}

		// Eight requests a minute. Sleep between batches, not after the last.
		if end < len(all) {
			time.Sleep(62 * time.Second)
		}
	}

	var tickers, rowsTotal int
	var newest sql.NullString
	db.QueryRow(`SELECT count(DISTINCT ticker), count(*), max(trade_date)::text FROM twoai_stock_closes`).
		Scan(&tickers, &rowsTotal, &newest)
	fmt.Printf("twoai_stocks: upserts=%d failed=%d tickers=%d rows=%d newest=%s ok=%v\n",
		saved, failed, tickers, rowsTotal, newest.String, failed == 0)
	return nil
}

// twoaiStocksDoc renders the prices into one content document the site reads:
// companies/stocks.json, keyed by company uid for the company pages and by
// fund ticker for the Public AI Companies section. Called from twoai_build
// after the fetch has run, so the day's close is in it.
//
// Each entry carries what a call-out box needs and nothing it does not: the
// latest close and its date, the previous close so the change can be shown,
// the 52-week range, and a 30-point series for a sparkline. The full history
// stays in twoai_stock_closes and is served through the API rather than
// duplicated onto every page.
//
// No entry, no box. A company page with no entry here renders no stock box at
// all, and a page whose instrument is not US-listed says prices are not
// tracked, which is the free tier's honest limit rather than a blank.
func twoaiStocksDoc(db *sql.DB, today string, upsert func(path, kind string, v any) error) error {
	rows, err := db.Query(`
		WITH latest AS (
		  SELECT DISTINCT ON (ticker) ticker, trade_date, close
		  FROM twoai_stock_closes ORDER BY ticker, trade_date DESC),
		prev AS (
		  SELECT DISTINCT ON (c.ticker) c.ticker, c.close
		  FROM twoai_stock_closes c JOIN latest l ON l.ticker=c.ticker AND c.trade_date < l.trade_date
		  ORDER BY c.ticker, c.trade_date DESC),
		yr AS (
		  SELECT ticker, min(close) AS lo, max(close) AS hi, count(*) AS n, min(trade_date) AS since
		  FROM twoai_stock_closes WHERE trade_date > current_date - interval '365 days' GROUP BY ticker),
		spark AS (
		  SELECT ticker, jsonb_agg(close ORDER BY trade_date) AS pts FROM (
		    SELECT ticker, trade_date, close, row_number() OVER (PARTITION BY ticker ORDER BY trade_date DESC) AS rn
		    FROM twoai_stock_closes) s WHERE rn <= 30 GROUP BY ticker)
		SELECT i.uid, i.ticker, i.name, i.kind, COALESCE(i.exchange,''), COALESCE(i.company_uid,''), COALESCE(i.issuer,''), COALESCE(i.note,''),
		  l.trade_date::text, l.close, COALESCE(p.close, l.close), y.lo, y.hi, y.n, y.since::text, COALESCE(s.pts::text,'[]'),
		  (SELECT count(*) FROM twoai_stock_closes c WHERE c.ticker=i.ticker),
		  (SELECT min(trade_date)::text FROM twoai_stock_closes c WHERE c.ticker=i.ticker)
		FROM twoai_stock_instruments i
		JOIN latest l ON l.ticker=i.ticker
		LEFT JOIN prev p ON p.ticker=i.ticker
		LEFT JOIN yr y ON y.ticker=i.ticker
		LEFT JOIN spark s ON s.ticker=i.ticker
		WHERE i.active ORDER BY i.ticker`)
	if err != nil {
		return err
	}
	defer rows.Close()

	byCompany := map[string]any{}
	funds := []map[string]any{}
	n := 0
	for rows.Next() {
		var uid, ticker, name, kind, exchange, companyUID, issuer, note, date, since, pts, firstDate string
		var close, prev float64
		var lo, hi sql.NullFloat64
		var yrN sql.NullInt64
		var histN int
		if err := rows.Scan(&uid, &ticker, &name, &kind, &exchange, &companyUID, &issuer, &note,
			&date, &close, &prev, &lo, &hi, &yrN, &since, &pts, &histN, &firstDate); err != nil {
			return err
		}
		var spark []float64
		_ = json.Unmarshal([]byte(pts), &spark)
		change := close - prev
		pct := 0.0
		if prev != 0 {
			pct = change / prev * 100
		}
		entry := map[string]any{
			"uid": uid, "ticker": ticker, "name": name, "kind": kind, "exchange": exchange,
			"close": close, "close_date": date, "prev_close": prev,
			"change": math.Round(change*100) / 100, "change_pct": math.Round(pct*100) / 100,
			"spark": spark, "history_points": histN, "history_since": firstDate,
			"source": twoaiStockSource, "source_url": "https://twelvedata.com/",
		}
		if lo.Valid && hi.Valid {
			entry["year_low"] = lo.Float64
			entry["year_high"] = hi.Float64
			entry["year_points"] = yrN.Int64
			entry["year_since"] = since
		}
		if issuer != "" {
			entry["issuer"] = issuer
		}
		if note != "" {
			entry["note"] = note
		}
		if kind == "index_etf" {
			funds = append(funds, entry)
		} else if companyUID != "" {
			byCompany[companyUID] = entry
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := upsert("companies/stocks.json", "stocks", map[string]any{
		"generated": today, "instruments": n, "by_company": byCompany, "funds": funds,
		"source": twoaiStockSource, "source_url": "https://twelvedata.com/",
		"coverage": "US-listed instruments only. Prices are end-of-day closes, not live quotes.",
	}); err != nil {
		return err
	}
	fmt.Printf("twoai_stocks: doc companies=%d funds=%d\n", len(byCompany), len(funds))
	return nil
}
