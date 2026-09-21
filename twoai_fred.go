package main

// twoai_fred: a live number behind six metrics that currently only have a
// definition.
//
// Stephen, 2026-09-20, after asking whether FRED was worth using: do it, but
// rank it below the three items already queued, scope it to half a dozen named
// series that back specific metrics on the datacenter and grid pages, cite the
// originating agency alongside FRED, and run it last.
//
// WHY NARROW. FRED carries 800,000 series. Ingesting it generally would add
// volume without adding a sourced claim to any page, which is the opposite of
// how this site works. Each series below exists because a metric on the
// datacenter or grid page describes something and cannot say what it is. No
// series is added without a metric that needs it.
//
// FRED IS A REPUBLISHER AND THE PAGE SAYS SO. Every series here originates at
// BLS, Census or the Federal Reserve. FRED is an official Federal Reserve
// product with full provenance, so the hop is defensible, but a reader is
// entitled to know whose number it is. Each row carries the originating agency
// and the page renders it.
//
// NOT VERIFIED AGAINST THE LIVE API. FRED rejects every request without a
// 32-character key, so the series identifiers below come from FRED's published
// catalogue and not from a response anyone has seen. A wrong identifier is the
// obvious failure, so it is handled the loud way: a series that does not
// resolve is reported by name and stored with its error, and NOTHING is
// published for it. A metric with no number keeps its definition rather than
// showing a blank.
//
// RUNS LAST, as asked. It reads nothing else in the pipeline and nothing else
// waits on it.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Each series is here because a named metric needs it. `metric` is the exact
// metric label on the datacenter or grid page, so the join is explicit rather
// than a guess at render time.
type fredSeries struct {
	ID     string
	Label  string
	Agency string // whose number this actually is
	Metric string // the page metric it backs
	Units  string
}

var twoaiFredSeries = []fredSeries{
	{"APU000072610", "Electricity, average price per kWh, US city average",
		"Bureau of Labor Statistics", "Power usage effectiveness", "US dollars per kWh"},
	{"PCU335311335311", "Producer price index: power, distribution and specialty transformers",
		"Bureau of Labor Statistics", "Critical equipment lead times", "index, 1982 = 100"},
	{"PCU3353133353132", "Producer price index: switchgear and switchboard apparatus",
		"Bureau of Labor Statistics", "Critical equipment lead times", "index"},
	{"IPG3344S", "Industrial production: semiconductors and electronic components",
		"Federal Reserve Board", "HBM supply concentration", "index, 2017 = 100"},
	{"PCU334413334413", "Producer price index: semiconductors and related devices",
		"Bureau of Labor Statistics", "Memory price direction", "index"},
	{"TLNRESCONS", "Total private nonresidential construction spending",
		"US Census Bureau", "Hyperscaler and colocation capital expenditure", "millions of dollars"},
}

func twoaiFred(db *sql.DB) error {
	key := strings.TrimSpace(os.Getenv("FRED_API_KEY"))
	if key == "" {
		fmt.Println("twoai_fred: FRED_API_KEY not set, skipping")
		return nil
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_fred_series (
		series_id text PRIMARY KEY,
		label text NOT NULL,
		agency text NOT NULL,
		metric text NOT NULL,
		units text,
		latest_value numeric,
		latest_date date,
		prior_value numeric,
		prior_date date,
		observations int NOT NULL DEFAULT 0,
		last_error text,
		checked_on date NOT NULL DEFAULT current_date,
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	ok, bad := 0, 0
	for _, s := range twoaiFredSeries {
		q := url.Values{}
		q.Set("series_id", s.ID)
		q.Set("api_key", key)
		q.Set("file_type", "json")
		q.Set("sort_order", "desc")
		q.Set("limit", "2") // latest and the one before it, nothing more
		resp, err := client.Get("https://api.stlouisfed.org/fred/series/observations?" + q.Encode())
		if err != nil {
			bad++
			twoaiFredFail(db, s, "fetch: "+err.Error())
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		code := resp.StatusCode
		resp.Body.Close()
		if code != 200 {
			// A bad series id returns 400 with a message naming it. Reported
			// rather than swallowed: a metric backed by a series that does not
			// exist must not quietly show nothing.
			var e struct {
				Message string `json:"error_message"`
			}
			json.Unmarshal(body, &e)
			msg := e.Message
			if msg == "" {
				msg = fmt.Sprintf("HTTP %d", code)
			}
			bad++
			fmt.Fprintf(os.Stderr, "twoai_fred: %s (%s): %s\n", s.ID, s.Metric, msg)
			twoaiFredFail(db, s, msg)
			continue
		}
		var out struct {
			Observations []struct {
				Date  string `json:"date"`
				Value string `json:"value"`
			} `json:"observations"`
		}
		if json.Unmarshal(body, &out) != nil || len(out.Observations) == 0 {
			bad++
			twoaiFredFail(db, s, "no observations returned")
			continue
		}
		// FRED writes "." for a period with no value. That is missing data,
		// not zero, and storing it as zero would put a false number on a page.
		var vals []struct {
			d string
			v float64
		}
		for _, o := range out.Observations {
			if o.Value == "." || strings.TrimSpace(o.Value) == "" {
				continue
			}
			f, err := strconv.ParseFloat(o.Value, 64)
			if err != nil {
				continue
			}
			vals = append(vals, struct {
				d string
				v float64
			}{o.Date, f})
		}
		if len(vals) == 0 {
			bad++
			twoaiFredFail(db, s, "every observation was missing")
			continue
		}
		var priorV any
		var priorD any
		if len(vals) > 1 {
			priorV, priorD = vals[1].v, vals[1].d
		}
		if _, err := db.Exec(`INSERT INTO twoai_fred_series
			(series_id, label, agency, metric, units, latest_value, latest_date,
			 prior_value, prior_date, observations, last_error, checked_on, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULL,current_date,now())
			ON CONFLICT (series_id) DO UPDATE SET label=$2, agency=$3, metric=$4, units=$5,
				latest_value=$6, latest_date=$7, prior_value=$8, prior_date=$9,
				observations=$10, last_error=NULL, checked_on=current_date, updated_at=now()`,
			s.ID, s.Label, s.Agency, s.Metric, s.Units,
			vals[0].v, vals[0].d, priorV, priorD, len(vals)); err != nil {
			bad++
			fmt.Fprintln(os.Stderr, "twoai_fred store:", err)
			continue
		}
		ok++
		fmt.Printf("twoai_fred: %s = %g (%s) backs \"%s\"\n", s.ID, vals[0].v, vals[0].d, s.Metric)
		time.Sleep(400 * time.Millisecond)
	}
	fmt.Printf("twoai_fred: %d of %d series current, %d unavailable\n", ok, len(twoaiFredSeries), bad)
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "twoai_fred: %d series could not be read. The metrics they back keep their definition and show no number, which is correct; a series id that never resolves should be removed or replaced rather than left failing daily.\n", bad)
	}
	return nil
}

// twoaiFredFail records why a series is unavailable and, crucially, clears the
// stored value. A stale number left behind after the series stopped resolving
// would be published with a fresh "checked" date and no indication it is old.
func twoaiFredFail(db *sql.DB, s fredSeries, why string) {
	db.Exec(`INSERT INTO twoai_fred_series
		(series_id, label, agency, metric, units, last_error, checked_on, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,current_date,now())
		ON CONFLICT (series_id) DO UPDATE SET label=$2, agency=$3, metric=$4, units=$5,
			latest_value=NULL, latest_date=NULL, prior_value=NULL, prior_date=NULL,
			observations=0, last_error=$6, checked_on=current_date, updated_at=now()`,
		s.ID, s.Label, s.Agency, s.Metric, s.Units, why)
}
