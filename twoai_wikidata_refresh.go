package main

// Refresher for twoai_wikidata_models, theworldofai row 444 (2026-10-04: yes,
// add a refresher on its 90 day cadence).
//
// The table was seeded once on 2026-08-18 from one Wikidata query: every item
// that is a large language model or a subclass of one (P31/P279* Q115305900).
// The family histories use its English Wikipedia titles. Once the oldest row
// is 90 days old the same query runs again: new models are added, labels,
// descriptions, developers, inception dates, licences and Wikipedia titles are
// refreshed, and a value Wikidata no longer returns is kept rather than
// blanked. developer_uid and parameters are this site's own and are left alone.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const wikidataModelsQuery = `SELECT ?item ?itemLabel ?itemDescription ?dev ?devLabel ?inception ?licenseLabel ?article WHERE {
  ?item wdt:P31/wdt:P279* wd:Q115305900 .
  OPTIONAL { ?item wdt:P178 ?dev }
  OPTIONAL { ?item wdt:P571 ?inception }
  OPTIONAL { ?item wdt:P275 ?license }
  OPTIONAL { ?article schema:about ?item ; schema:isPartOf <https://en.wikipedia.org/> }
  SERVICE wikibase:label { bd:serviceParam wikibase:language "en". }
}`

func twoaiWikidataModelsRefresh(db *sql.DB) {
	var due bool
	if db.QueryRow(`SELECT COALESCE(min(fetched_on) <= current_date - 90, true) FROM twoai_wikidata_models`).Scan(&due) != nil || !due {
		return
	}
	raw, err := benchGet("https://query.wikidata.org/sparql?format=json&query=" + url.QueryEscape(wikidataModelsQuery))
	if err != nil {
		fmt.Println("wikidata_models: query failed, keeping the current rows:", err)
		return
	}
	var res struct {
		Results struct {
			Bindings []map[string]struct {
				Value string `json:"value"`
			} `json:"bindings"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		fmt.Println("wikidata_models: unreadable answer:", err)
		return
	}
	qid := func(u string) string { return u[strings.LastIndex(u, "/")+1:] }
	type row struct{ label, desc, dev, devQID, inception, license, enwiki string }
	rows := map[string]*row{}
	for _, b := range res.Results.Bindings {
		id := qid(b["item"].Value)
		if !strings.HasPrefix(id, "Q") {
			continue
		}
		r := rows[id]
		if r == nil {
			r = &row{}
			rows[id] = r
		}
		// An item with several developers or licences comes back once per
		// combination; the first non-empty value is kept.
		set := func(dst *string, v string) {
			if *dst == "" && v != "" {
				*dst = v
			}
		}
		set(&r.label, b["itemLabel"].Value)
		set(&r.desc, b["itemDescription"].Value)
		set(&r.dev, b["devLabel"].Value)
		if b["dev"].Value != "" {
			set(&r.devQID, qid(b["dev"].Value))
		}
		if len(b["inception"].Value) >= 10 {
			set(&r.inception, b["inception"].Value[:10])
		}
		set(&r.license, b["licenseLabel"].Value)
		if a := b["article"].Value; a != "" {
			if t, err := url.PathUnescape(a[strings.LastIndex(a, "/")+1:]); err == nil {
				set(&r.enwiki, t)
			}
		}
	}
	// A failed or truncated query must not stand in for a refresh: the seed
	// held 171 items.
	if len(rows) < 100 {
		fmt.Printf("wikidata_models: only %d items returned, keeping the current rows\n", len(rows))
		return
	}
	basis := "Wikidata P31/P279* Q115305900 (large language model), fetched from query.wikidata.org " + time.Now().UTC().Format("2006-01-02")
	nz := func(s string) any {
		if s == "" || (strings.HasPrefix(s, "Q") && s == strings.TrimSpace(s) && len(s) > 1 && strings.Trim(s[1:], "0123456789") == "") {
			// An unlabelled entity comes back as its bare QID; that is not a name.
			return nil
		}
		return s
	}
	orNil := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	added, updated := 0, 0
	for id, r := range rows {
		if nz(r.label) == nil {
			continue
		}
		var inserted bool
		err := db.QueryRow(`INSERT INTO twoai_wikidata_models (qid, label, description, developer, developer_qid, inception, license, enwiki_title, match_basis, fetched_on)
			VALUES ($1,$2,$3,$4,$5,$6::date,$7,$8,$9,current_date)
			ON CONFLICT (qid) DO UPDATE SET label = EXCLUDED.label,
				description = COALESCE(EXCLUDED.description, twoai_wikidata_models.description),
				developer = COALESCE(EXCLUDED.developer, twoai_wikidata_models.developer),
				developer_qid = COALESCE(EXCLUDED.developer_qid, twoai_wikidata_models.developer_qid),
				inception = COALESCE(EXCLUDED.inception, twoai_wikidata_models.inception),
				license = COALESCE(EXCLUDED.license, twoai_wikidata_models.license),
				enwiki_title = COALESCE(EXCLUDED.enwiki_title, twoai_wikidata_models.enwiki_title),
				match_basis = EXCLUDED.match_basis, fetched_on = current_date
			RETURNING (xmax = 0)`,
			id, r.label, nz(r.desc), nz(r.dev), orNil(r.devQID), orNil(r.inception), nz(r.license), nz(r.enwiki), basis).Scan(&inserted)
		if err != nil {
			fmt.Printf("wikidata_models: %s: %v\n", id, err)
			continue
		}
		if inserted {
			added++
		} else {
			updated++
		}
	}
	// Rows Wikidata no longer classes as a language model keep their data but
	// are marked checked, so the cadence is measured from this refresh.
	db.Exec(`UPDATE twoai_wikidata_models SET fetched_on = current_date WHERE fetched_on < current_date`)
	fmt.Printf("wikidata_models: %d items, %d new, %d refreshed ok=true\n", len(rows), added, updated)
}
