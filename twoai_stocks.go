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
