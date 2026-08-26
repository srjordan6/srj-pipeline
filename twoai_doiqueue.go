package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// A WAY INTO THE SPINE FOR PAPERS THE FILTER DID NOT FIND.
//
// The works spine harvests by subfield, which is the right default and will
// always be incomplete at the edges: OpenAlex assigns one primary topic, and
// a paper about LLM agents can land under computer vision, health informatics
// or computational linguistics depending on what it was applied to. Widening
// the subfield list helps and does not close the gap.
//
// This queue is the manual door. Anything that surfaces a DOI - a Consensus
// search, a citation in a lawsuit filing, a reader's question, a paper named
// in the news - can be dropped into twoai_doi_queue and this stage resolves
// it through OpenAlex into twoai_works with the same shape, provenance and
// licence class as everything the backfill collected.
//
// It is deliberately a QUEUE AND NOT AN API CALL AT DISCOVERY TIME. Whoever
// finds the paper records the DOI and why; the resolution happens on the
// pipeline's schedule, is rate-limited politely, retries on its own, and
// leaves a row explaining what happened to each request. A discovery that
// fails to resolve stays visible as a pending row rather than vanishing.
//
// CONSENSUS IS A DISCOVERY LAYER, NOT A SOURCE. Its results carry DOIs and
// resolve through OpenAlex, so nothing is ingested from Consensus itself and
// no licence question arises: the metadata still comes from OpenAlex, CC0,
// and the abstract is still held cite_only.

func twoaiDOIQueue(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_doi_queue (
		doi text PRIMARY KEY,
		note text,
		found_via text,
		requested_on date NOT NULL DEFAULT current_date,
		status text NOT NULL DEFAULT 'pending',
		resolved_on date,
		openalex_id text,
		attempts int NOT NULL DEFAULT 0,
		last_error text)`); err != nil {
		return fmt.Errorf("doi_queue create: %w", err)
	}
	if err := twoaiOAEnsureTables(db); err != nil {
		return err
	}

	// Five attempts is enough to ride out a bad day at OpenAlex; past that a
	// DOI is almost certainly not indexed there, and endless retries would
	// hide that fact behind a pending row that never changes.
	rows, err := db.Query(`SELECT doi FROM twoai_doi_queue
		WHERE status='pending' AND attempts < 5 ORDER BY requested_on LIMIT 50`)
	if err != nil {
		return fmt.Errorf("doi_queue read: %w", err)
	}
	var dois []string
	for rows.Next() {
		var d string
		if rows.Scan(&d) == nil {
			dois = append(dois, d)
		}
	}
	rows.Close()
	if len(dois) == 0 {
		return nil // say nothing when there is nothing to do
	}

	client := &http.Client{Timeout: 45 * time.Second}
	resolved, failed, notFound := 0, 0, 0
	for _, doi := range dois {
		clean := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(doi)), "https://doi.org/")
		u := fmt.Sprintf("https://api.openalex.org/works/doi:%s?select=%s&mailto=%s",
			url.QueryEscape(clean), url.QueryEscape(twoaiOASelect), twoaiOAMailto)
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", "theworldofai.org works-spine (mailto:"+twoaiOAMailto+")")
		resp, err := client.Do(req)
		if err != nil {
			db.Exec(`UPDATE twoai_doi_queue SET attempts=attempts+1, last_error=$1 WHERE doi=$2`,
				truncate(err.Error(), 200), doi)
			failed++
			time.Sleep(1500 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		code := resp.StatusCode
		resp.Body.Close()

		if code == 404 {
			// A definitive answer, not a failure: OpenAlex does not have it.
			// Recorded as such so nobody re-queues it hoping for better luck.
			db.Exec(`UPDATE twoai_doi_queue SET status='not_in_openalex',
				attempts=attempts+1, resolved_on=current_date,
				last_error='OpenAlex has no work with this DOI' WHERE doi=$1`, doi)
			notFound++
			time.Sleep(1500 * time.Millisecond)
			continue
		}
		if code != 200 {
			db.Exec(`UPDATE twoai_doi_queue SET attempts=attempts+1, last_error=$1 WHERE doi=$2`,
				fmt.Sprintf("http %d", code), doi)
			failed++
			time.Sleep(3 * time.Second) // back off harder on 429 and 5xx
			continue
		}

		var w twoaiOADoc
		if err := json.Unmarshal(body, &w); err != nil {
			db.Exec(`UPDATE twoai_doi_queue SET attempts=attempts+1, last_error=$1 WHERE doi=$2`,
				truncate(err.Error(), 200), doi)
			failed++
			continue
		}
		oid, uerr := twoaiOAUpsert(db, w)
		if uerr != nil {
			db.Exec(`UPDATE twoai_doi_queue SET attempts=attempts+1, last_error=$1 WHERE doi=$2`,
				truncate(uerr.Error(), 200), doi)
			failed++
			continue
		}
		db.Exec(`UPDATE twoai_doi_queue SET status='resolved', resolved_on=current_date,
			openalex_id=$1, attempts=attempts+1, last_error=NULL WHERE doi=$2`, oid, doi)
		resolved++
		time.Sleep(1500 * time.Millisecond)
	}

	var pending int
	db.QueryRow(`SELECT count(*) FROM twoai_doi_queue WHERE status='pending' AND attempts < 5`).Scan(&pending)
	fmt.Printf("doi_queue: resolved=%d not_in_openalex=%d failed=%d pending=%d\n",
		resolved, notFound, failed, pending)
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "doi_queue: %d DOI(s) failed this run; they retry until 5 attempts\n", failed)
	}
	return nil
}
