package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

	_ "github.com/lib/pq"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pipeline <source_key>")
		os.Exit(2)
	}
	src := os.Args[1]
	if src == "all" {
		for _, s := range []string{"federal_register", "legiscan", "gdelt"} {
			cmd := exec.Command(os.Args[0], s)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			cmd.Run() // a failing source must not block the others
		}
		return
	}
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "db open:", err)
		os.Exit(1)
	}
	defer db.Close()

	var sourceID int
	if err := db.QueryRow(`SELECT id FROM pipeline.sources WHERE key=$1`, src).Scan(&sourceID); err != nil {
		fmt.Fprintln(os.Stderr, "unknown source:", src, err)
		os.Exit(1)
	}
	var runID int64
	if err := db.QueryRow(`INSERT INTO pipeline.runs (source_id) VALUES ($1) RETURNING id`, sourceID).Scan(&runID); err != nil {
		fmt.Fprintln(os.Stderr, "run insert:", err)
		os.Exit(1)
	}

	var fetched, added int
	var runErr error
	switch src {
	case "federal_register":
		fetched, added, runErr = federalRegister(db, sourceID)
	case "legiscan":
		fetched, added, runErr = legiscan(db, sourceID)
	case "gdelt":
		fetched, added, runErr = gdelt(db, sourceID)
	default:
		runErr = fmt.Errorf("no adapter for source %q", src)
	}

	status, errText := "ok", sql.NullString{}
	if runErr != nil {
		status = "error"
		errText = sql.NullString{String: runErr.Error(), Valid: true}
		fmt.Fprintln(os.Stderr, "run error:", runErr)
	}
	db.Exec(`UPDATE pipeline.runs SET finished_at=now(), status=$1, docs_fetched=$2, docs_new=$3, error=$4 WHERE id=$5`,
		status, fetched, added, errText, runID)
	fmt.Printf("run %d: %s fetched=%d new=%d status=%s\n", runID, src, fetched, added, status)
	if runErr != nil {
		os.Exit(1)
	}
}

func federalRegister(db *sql.DB, sourceID int) (fetched, added int, err error) {
	url := "https://www.federalregister.gov/api/v1/documents.json" +
		"?conditions%5Bterm%5D=%22artificial+intelligence%22" +
		"&order=newest&per_page=100" +
		"&fields%5B%5D=document_number&fields%5B%5D=title&fields%5B%5D=type" +
		"&fields%5B%5D=abstract&fields%5B%5D=publication_date&fields%5B%5D=agencies" +
		"&fields%5B%5D=html_url&fields%5B%5D=pdf_url&fields%5B%5D=raw_text_url"

	client := &http.Client{Timeout: 60 * time.Second}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "srj-pipeline/1.0 (srjconsultingservices.com)")
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, 0, fmt.Errorf("FR API status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, err
	}

	var payload struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, 0, err
	}

	for _, doc := range payload.Results {
		fetched++
		raw, _ := json.Marshal(doc)
		h := sha256.Sum256(raw)
		hash := hex.EncodeToString(h[:])
		extID, _ := doc["document_number"].(string)
		if extID == "" {
			continue
		}
		title, _ := doc["title"].(string)
		htmlURL, _ := doc["html_url"].(string)
		var pub any
		if p, ok := doc["publication_date"].(string); ok && p != "" {
			pub = p
		}
		res, e := db.Exec(`INSERT INTO pipeline.documents (source_id, external_id, change_hash, url, title, published_at, raw)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (source_id, external_id, change_hash) DO NOTHING`,
			sourceID, extID, hash, htmlURL, title, pub, raw)
		if e != nil {
			return fetched, added, e
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return fetched, added, nil
}


// insertDoc appends one document to the corpus with change_hash dedupe.
func insertDoc(db *sql.DB, sourceID int, extID, hash, url, title string, pub any, raw []byte) (bool, error) {
	res, err := db.Exec(`INSERT INTO pipeline.documents (source_id, external_id, change_hash, url, title, published_at, raw)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (source_id, external_id, change_hash) DO NOTHING`,
		sourceID, extID, hash, url, title, pub, raw)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// legiscan pulls state AI bills. LegiScan supplies its own change_hash per
// bill, which slots directly into the dedupe key. Per the API registration:
// texts only, all states, one daily batch. Key from LEGISCAN_API_KEY.
func legiscan(db *sql.DB, sourceID int) (fetched, added int, err error) {
	key := os.Getenv("LEGISCAN_API_KEY")
	if key == "" {
		return 0, 0, fmt.Errorf("LEGISCAN_API_KEY not set")
	}
	client := &http.Client{Timeout: 60 * time.Second}
	for page := 1; page <= 2; page++ {
		url := fmt.Sprintf("https://api.legiscan.com/?key=%s&op=getSearch&state=ALL&query=%s&page=%d",
			key, "%22artificial+intelligence%22", page)
		resp, e := client.Get(url)
		if e != nil {
			return fetched, added, e
		}
		body, e := io.ReadAll(resp.Body)
		resp.Body.Close()
		if e != nil {
			return fetched, added, e
		}
		var payload struct {
			Status       string                     `json:"status"`
			SearchResult map[string]json.RawMessage `json:"searchresult"`
		}
		if e := json.Unmarshal(body, &payload); e != nil {
			return fetched, added, e
		}
		if payload.Status != "OK" {
			return fetched, added, fmt.Errorf("legiscan status %s", payload.Status)
		}
		got := 0
		for k, v := range payload.SearchResult {
			if k == "summary" {
				continue
			}
			var b struct {
				BillID     int    `json:"bill_id"`
				ChangeHash string `json:"change_hash"`
				URL        string `json:"url"`
				State      string `json:"state"`
				BillNumber string `json:"bill_number"`
				Title      string `json:"title"`
				LastAction string `json:"last_action_date"`
			}
			if json.Unmarshal(v, &b) != nil || b.BillID == 0 {
				continue
			}
			fetched++
			got++
			var pub any
			if b.LastAction != "" {
				pub = b.LastAction
			}
			ok, e := insertDoc(db, sourceID, fmt.Sprintf("%d", b.BillID), b.ChangeHash,
				b.URL, b.State+" "+b.BillNumber+": "+b.Title, pub, v)
			if e != nil {
				return fetched, added, e
			}
			if ok {
				added++
			}
		}
		if got < 50 {
			break // last page
		}
		time.Sleep(2 * time.Second)
	}
	return fetched, added, nil
}

// gdelt pulls the last 24h of global AI-governance news coverage as the
// DISCOVERY layer. News is never fact evidence (admissible_for_facts=false);
// it exists to trigger primary-source verification. Courtesy rate: one
// request per 5 seconds.
func gdelt(db *sql.DB, sourceID int) (fetched, added int, err error) {
	queries := []string{
		"%22artificial%20intelligence%22%20(regulation%20OR%20law%20OR%20governance)",
		"%22AI%20act%22%20OR%20%22AI%20regulation%22",
	}
	client := &http.Client{Timeout: 60 * time.Second}
	for i, q := range queries {
		if i > 0 {
			time.Sleep(10 * time.Second)
		}
		url := "https://api.gdeltproject.org/api/v2/doc/doc?query=" + q +
			"&mode=artlist&maxrecords=100&format=json&timespan=24h"
		var body []byte
		var e error
		for attempt := 1; attempt <= 3; attempt++ {
			req, _ := http.NewRequest("GET", url, nil)
			req.Header.Set("User-Agent", "srj-pipeline/1.0 (srjconsultingservices.com)")
			var resp *http.Response
			resp, e = client.Do(req)
			if e == nil {
				body, e = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			if e == nil {
				// GDELT throttling returns HTTP 200 with plain text, not JSON.
				trimmed := bytes.TrimSpace(body)
				if len(trimmed) == 0 || trimmed[0] != '{' {
					e = fmt.Errorf("gdelt non-JSON response (throttled?): %.80s", trimmed)
				}
			}
			if e == nil {
				break
			}
			time.Sleep(time.Duration(attempt*30) * time.Second) // throttle + egress blips
		}
		if e != nil {
			return fetched, added, e
		}
		var payload struct {
			Articles []struct {
				URL      string `json:"url"`
				Title    string `json:"title"`
				SeenDate string `json:"seendate"`
				Domain   string `json:"domain"`
			} `json:"articles"`
		}
		if e := json.Unmarshal(body, &payload); e != nil {
			return fetched, added, e
		}
		for _, a := range payload.Articles {
			if a.URL == "" {
				continue
			}
			fetched++
			uh := sha256.Sum256([]byte(a.URL))
			ch := sha256.Sum256([]byte(a.Title + a.SeenDate))
			var pub any
			if len(a.SeenDate) >= 8 {
				pub = a.SeenDate[:4] + "-" + a.SeenDate[4:6] + "-" + a.SeenDate[6:8]
			}
			raw, _ := json.Marshal(a)
			ok, e := insertDoc(db, sourceID, hex.EncodeToString(uh[:])[:32],
				hex.EncodeToString(ch[:]), a.URL, a.Title, pub, raw)
			if e != nil {
				return fetched, added, e
			}
			if ok {
				added++
			}
		}
	}
	return fetched, added, nil
}
