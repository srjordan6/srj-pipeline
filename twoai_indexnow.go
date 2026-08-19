package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// twoai_indexnow submits newly published URLs to the IndexNow endpoint, which
// fans out to Bing, Yandex, Naver and Seznam.
//
// WHY THIS EXISTS. Sitemaps are an invitation: a crawler reads one when it next
// feels like visiting, which for a young site can be days. IndexNow is a push -
// we tell the engines what changed, within a minute of it changing. Google does
// not participate, so this does not replace Search Console; it covers everyone
// else, and Bing is what feeds ChatGPT and Copilot search, which matters more
// for this site than the raw traffic numbers suggest.
//
// WHAT IT SUBMITS. Only URLs the registry has never submitted. The registry is
// already the record of every URL the site has published, updated by the
// url_registry stage that runs just before this one, so "new since last time"
// is a column read rather than a guess. Re-submitting an unchanged URL is not
// an error but it is noise, and IndexNow's own guidance is to send changes.
//
// THE KEY. IndexNow proves ownership by having the key hosted at the site root:
// https://theworldofai.org/{key}.txt must return exactly the key. The file
// lives in twoai-site/public/, so it deploys with the site and cannot drift out
// of sync with the value here. It is not a secret - anyone can read it - it
// exists only to prove that whoever submits URLs for this host can also write
// to this host.
//
// FAILURE POSTURE. Any non-2xx leaves the rows unmarked, so the next run tries
// them again rather than losing them silently. The stage never blocks the build:
// like every other stage it runs as its own subprocess in the daily sequence.
const (
	indexNowKey      = "6c2bca607491f18a4ae1fa0d1c44bc4e"
	indexNowHost     = "theworldofai.org"
	indexNowEndpoint = "https://api.indexnow.org/indexnow"
	// IndexNow accepts up to 10,000 URLs per request. 1,000 keeps each request
	// small enough to retry cheaply and to read in a log.
	indexNowBatch = 1000
)

func twoaiIndexNow(db *sql.DB) error {
	rows, err := db.Query(`SELECT url FROM twoai_url_registry
		WHERE indexnow_submitted_on IS NULL
		  AND resolution IS DISTINCT FROM 'gone'
		  AND url LIKE 'https://theworldofai.org/%'
		ORDER BY first_seen_at DESC NULLS LAST
		LIMIT $1`, indexNowBatch)
	if err != nil {
		return err
	}
	var urls []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			urls = append(urls, u)
		}
	}
	rows.Close()

	if len(urls) == 0 {
		// Silent-ish: nothing changed is the normal state most days, and a line
		// that appears every day whatever happens is a line nobody reads.
		fmt.Println("twoai_indexnow: nothing new to submit")
		return nil
	}

	body, _ := json.Marshal(map[string]any{
		"host":        indexNowHost,
		"key":         indexNowKey,
		"keyLocation": "https://" + indexNowHost + "/" + indexNowKey + ".txt",
		"urlList":     urls,
	})

	client := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest("POST", indexNowEndpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "srj-pipeline/1.0 (theworldofai.org)")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 200 accepted, 202 accepted but the key is still being validated. Anything
	// else and we leave the rows unmarked so the next run retries them.
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		return fmt.Errorf("indexnow status %d (403 means the key file is not readable at %s, 422 means a URL did not match the host)",
			resp.StatusCode, "https://"+indexNowHost+"/"+indexNowKey+".txt")
	}

	batch := "indexnow " + time.Now().UTC().Format("2006-01-02")
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, u := range urls {
		if _, err := tx.Exec(`UPDATE twoai_url_registry
			SET indexnow_submitted_on=current_date, indexnow_batch=$2 WHERE url=$1`, u, batch); err != nil {
			tx.Rollback()
			return err
		}
	}
	// The same submission is recorded in site_search_submissions, which is the
	// single human-readable record of what has been sent to which engine. Two
	// separate records of submissions is exactly the drift that produced a
	// duplicate GSC batch on 2026-08-18, so both are written in one transaction.
	for _, u := range urls {
		if _, err := tx.Exec(`INSERT INTO site_search_submissions
			(url, submitted_on, batch, outcome, search_engine, notes)
			VALUES ($1, current_date, $2, 'submitted', 'indexnow',
				'Pushed automatically by the twoai_indexnow stage. IndexNow fans out to Bing, Yandex, Naver and Seznam; Google does not participate.')
			ON CONFLICT DO NOTHING`, u, batch); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	fmt.Printf("twoai_indexnow: submitted %d url(s), status %d, batch %q\n",
		len(urls), resp.StatusCode, batch)
	if len(urls) == indexNowBatch {
		fmt.Fprintln(os.Stderr, "twoai_indexnow: batch was full; more remain for the next run")
	}
	return nil
}

// verifyIndexNowKey confirms the key file is readable before the first
// submission. IndexNow answers 403 when it cannot fetch the key, and that
// failure is easy to misread as a bad URL list.
func verifyIndexNowKey() error {
	url := "https://" + indexNowHost + "/" + indexNowKey + ".txt"
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("key file %s returned %d; deploy the site before submitting", url, resp.StatusCode)
	}
	buf := make([]byte, 128)
	n, _ := resp.Body.Read(buf)
	if strings.TrimSpace(string(buf[:n])) != indexNowKey {
		return fmt.Errorf("key file %s does not contain the expected key", url)
	}
	fmt.Printf("twoai_indexnow: key verified at %s\n", url)
	return nil
}
