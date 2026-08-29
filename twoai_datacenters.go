package main

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/lib/pq"
)

// DATA CENTERS: the physical layer of the AI boom, tracked in two registers
// and honest about which is which. The live register is money and filings,
// because that is what primary sources publish: quarterly capital expenditure
// read from each builder's own XBRL facts on data.sec.gov (the capex fetcher
// gained Equinix, Digital Realty, Oracle, and CoreWeave for exactly this
// page), and material 8-K events from the same registrants. The reference
// register is editorial and lives in SQL: the operations metrics a facility
// is actually measured by (PUE through five nines, twoai_dc_metrics) and the
// market variables everyone asks about (queues, PPAs, rack density,
// twoai_dc_metrics track=market), each definition in this site's own words
// with the standard-setter linked. Megawatt pipelines and colocation rates
// are published only by commercial research firms, so the page says who
// publishes them (twoai_dc_sources) and does not republish numbers it cannot
// verify. All three seed tables are editable in SQL and render on the next
// run; nothing on this page is written by hand into a file.
func twoaiDatacenters(db *sql.DB, today string) (int, error) {
	var name, blurb string
	if err := db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy
		WHERE slug='data-centers' AND status='live'`).Scan(&name, &blurb); err != nil {
		return 0, nil // section not defined, nothing to render
	}

	type metric struct {
		Track      string `json:"track"`
		Category   string `json:"category"`
		Metric     string `json:"metric"`
		Definition string `json:"definition"`
		Formula    string `json:"formula,omitempty"`
		Why        string `json:"why"`
		Target     string `json:"target,omitempty"`
		SourceName string `json:"source_name,omitempty"`
		SourceURL  string `json:"source_url,omitempty"`
	}
	var metrics []metric
	mrows, err := db.Query(`SELECT track, category, metric, definition,
			COALESCE(formula,''), why_it_matters, COALESCE(target_note,''),
			COALESCE(source_name,''), COALESCE(source_url,'')
		FROM twoai_dc_metrics ORDER BY sort`)
	if err != nil {
		return 0, err
	}
	for mrows.Next() {
		var m metric
		if mrows.Scan(&m.Track, &m.Category, &m.Metric, &m.Definition,
			&m.Formula, &m.Why, &m.Target, &m.SourceName, &m.SourceURL) == nil {
			metrics = append(metrics, m)
		}
	}
	mrows.Close()

	type src struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		URL       string `json:"url"`
		Publishes string `json:"publishes"`
		Access    string `json:"access"`
	}
	var srcs []src
	srows, err := db.Query(`SELECT kind, name, url, publishes, access
		FROM twoai_dc_sources ORDER BY sort`)
	if err != nil {
		return 0, err
	}
	for srows.Next() {
		var s src
		if srows.Scan(&s.Kind, &s.Name, &s.URL, &s.Publishes, &s.Access) == nil {
			srcs = append(srcs, s)
		}
	}
	srows.Close()

	// The builders whose capex IS the data center buildout: hyperscalers plus
	// the colocation giants plus the pure-play AI cloud. Series come from the
	// twoai_capex table the SEC fetcher maintains; a company added to the
	// fetcher shows up here on its next run with no further wiring.
	dcNames := []string{"Microsoft", "Amazon", "Alphabet", "Meta", "Oracle",
		"Equinix", "Digital Realty", "CoreWeave"}
	type cpx struct {
		Name   string           `json:"name"`
		Latest float64          `json:"latest"`
		End    string           `json:"end"`
		Series []map[string]any `json:"series"`
	}
	var capex []cpx
	dcCIKs := map[string]bool{}
	for _, n := range dcNames {
		var x cpx
		x.Name = n
		crows, err := db.Query(`SELECT cik, end_date, val FROM twoai_capex
			WHERE name=$1 ORDER BY end_date DESC LIMIT 8`, n)
		if err != nil {
			continue
		}
		for crows.Next() {
			var cik, end string
			var val float64
			if crows.Scan(&cik, &end, &val) == nil {
				dcCIKs[cik] = true
				if x.End == "" {
					x.End, x.Latest = end, val
				}
				x.Series = append(x.Series, map[string]any{"end": end, "val": val})
			}
		}
		crows.Close()
		if x.End != "" {
			capex = append(capex, x)
		}
	}

	type filing struct {
		Company string `json:"company"`
		Filed   string `json:"filed"`
		Items   string `json:"items"`
		DocURL  string `json:"doc_url"`
	}
	var filings []filing
	cikList := make([]string, 0, len(dcCIKs))
	for c := range dcCIKs {
		cikList = append(cikList, c)
	}
	if len(cikList) > 0 {
		frows, err := db.Query(`SELECT company, to_char(filed,'YYYY-MM-DD'), items, doc_url
			FROM twoai_ma_filings WHERE cik = ANY($1)
			ORDER BY filed DESC LIMIT 12`, pq.Array(cikList))
		if err == nil {
			for frows.Next() {
				var f filing
				if frows.Scan(&f.Company, &f.Filed, &f.Items, &f.DocURL) == nil {
					filings = append(filings, f)
				}
			}
			frows.Close()
		}
	}

	doc := map[string]any{
		"shape": "datacenters", "uid": twoaiUID("section:data-centers"),
		"tax": "data-centers", "generated": today, "name": name, "blurb": blurb,
		"metrics": metrics, "sources": srcs, "capex": capex, "filings": filings,
	}
	j, _ := json.Marshal(doc)
	if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
		VALUES ('tech/datacenters.json','tech-datacenters',$1::jsonb,'data-centers',1)
		ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
			taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
		string(j)); err != nil {
		return 0, err
	}
	fmt.Printf("twoai_build: datacenters metrics=%d sources=%d capex_cos=%d filings=%d\n",
		len(metrics), len(srcs), len(capex), len(filings))
	return 1, nil
}
