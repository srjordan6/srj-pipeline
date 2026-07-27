package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
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
		for _, s := range []string{"federal_register", "legiscan", "gdelt", "publish_news"} {
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

	if src == "publish_news" {
		if err := publishNews(db); err != nil {
			fmt.Fprintln(os.Stderr, "publish_news:", err)
			os.Exit(1)
		}
		fmt.Println("publish_news: ok")
		return
	}
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

// gdelt pulls the last 24h of global news via GDELT's raw 15-minute GKG
// files (data.gdeltproject.org, no throttle), filtered to AI relevance.
// DISCOVERY layer only: news is never fact evidence. Each matched line
// carries persons/orgs/themes, the seed data for AI-people and AI-orgs.
func gdelt(db *sql.DB, sourceID int) (fetched, added int, err error) {
	client := &http.Client{Timeout: 90 * time.Second}
	now := time.Now().UTC().Truncate(15 * time.Minute)
	for i := 96; i >= 1; i-- { // last 24h of 15-min slices
		ts := now.Add(-time.Duration(i) * 15 * time.Minute).Format("20060102150405")
		url := "http://data.gdeltproject.org/gdeltv2/" + ts + ".gkg.csv.zip"
		resp, e := client.Get(url)
		if e != nil {
			continue // transient slice failure; the day's other 95 carry it
		}
		body, e := io.ReadAll(resp.Body)
		resp.Body.Close()
		if e != nil || resp.StatusCode != 200 {
			continue
		}
		zr, e := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if e != nil || len(zr.File) == 0 {
			continue
		}
		f, e := zr.File[0].Open()
		if e != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			low := strings.ToLower(line)
			if !strings.Contains(low, "artificial intelligence") && !strings.Contains(low, "artificialintelligence") {
				continue
			}
			c := strings.Split(line, "\t")
			if len(c) < 15 {
				continue
			}
			docURL := c[4]
			if !strings.HasPrefix(docURL, "http") {
				continue
			}
			fetched++
			title := ""
			if j := strings.Index(line, "<PAGE_TITLE>"); j >= 0 {
				if k := strings.Index(line[j:], "</PAGE_TITLE>"); k > 12 {
					title = line[j+12 : j+k]
				}
			}
			meta := map[string]string{"url": docURL, "domain": c[3], "date": c[1],
				"persons": trunc(c[11], 800), "orgs": trunc(c[13], 800), "themes": trunc(c[7], 800), "title": title}
			raw, _ := json.Marshal(meta)
			uh := sha256.Sum256([]byte(docURL))
			id := hex.EncodeToString(uh[:])[:32]
			var pub any
			if len(c[1]) >= 8 {
				pub = c[1][:4] + "-" + c[1][4:6] + "-" + c[1][6:8]
			}
			ok, e := insertDoc(db, sourceID, id, id, docURL, title, pub, raw)
			if e != nil {
				f.Close()
				return fetched, added, e
			}
			if ok {
				added++
			}
		}
		f.Close()
		time.Sleep(300 * time.Millisecond)
	}
	return fetched, added, nil
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}


// publishNews pushes the latest AI news items into srj-content as
// news/news.json via the GitHub contents API. The srj-content push fires
// the site rebuild hook, so the news strip republishes itself daily.
// Env: GITHUB_TOKEN (fine-grained PAT, contents:write on srj-content).
func publishNews(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	rows, err := db.Query(`SELECT d.title, d.url, d.published_at::date::text, d.raw->>'domain'
		FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
		WHERE s.key='gdelt' AND d.title <> '' ORDER BY d.published_at DESC NULLS LAST, d.id DESC LIMIT 60`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type item struct{ Title, URL, Date, Domain string }
	var items []item
	for rows.Next() {
		var it item
		var date sql.NullString
		if rows.Scan(&it.Title, &it.URL, &date, &it.Domain) == nil {
			it.Date = date.String
			items = append(items, it)
		}
	}
	payload, _ := json.MarshalIndent(map[string]any{"generated": time.Now().UTC().Format(time.RFC3339), "items": items}, "", " ")

	api := "https://api.github.com/repos/srjordan6/srj-content/contents/news/news.json"
	client := &http.Client{Timeout: 60 * time.Second}
	gh := func(method, url string, body []byte) (*http.Response, error) {
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		return client.Do(req)
	}
	sha := ""
	if resp, e := gh("GET", api, nil); e == nil {
		var cur struct{ SHA string `json:"sha"` }
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && json.Unmarshal(b, &cur) == nil {
			sha = cur.SHA
		}
	}
	put := map[string]any{"message": "pipeline: daily news refresh",
		"content": base64.StdEncoding.EncodeToString(payload)}
	if sha != "" {
		put["sha"] = sha
	}
	pb, _ := json.Marshal(put)
	resp, e := gh("PUT", api, pb)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github PUT %d: %.200s", resp.StatusCode, b)
	}
	return nil
}
