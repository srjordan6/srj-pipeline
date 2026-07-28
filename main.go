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
	"regexp"
	"sort"
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
		for _, s := range []string{"federal_register", "legiscan", "gdelt", "publish_news", "publish_legislation"} {
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
	if src == "publish_legislation" {
		if err := publishLegislation(db); err != nil {
			fmt.Fprintln(os.Stderr, "publish_legislation:", err)
			os.Exit(1)
		}
		fmt.Println("publish_legislation: ok")
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
		title, _ := doc["title"].(string)
		abstract, _ := doc["abstract"].(string)

		// The API query already carries conditions[term]="artificial
		// intelligence", but conditions[term] searches the FULL TEXT of a
		// document. A submarine cable licensing rule that mentions AI once in
		// its body matches, and so does a fishery council meeting notice. On
		// 2026-07-28 only 15 of 101 stored documents mentioned AI anywhere, and
		// the other 86 were noise occupying the corpus.
		//
		// Keep the broad query, since recall at the API is free and cheap to
		// filter, then narrow here: a document earns a row only if AI appears in
		// its TITLE or ABSTRACT, which is the difference between a rule about AI
		// and a rule that happens to mention it.
		if !mentionsAI(title) && !mentionsAI(abstract) {
			continue
		}

		raw, _ := json.Marshal(doc)
		h := sha256.Sum256(raw)
		hash := hex.EncodeToString(h[:])
		extID, _ := doc["document_number"].(string)
		if extID == "" {
			continue
		}
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

// aiTerm matches AI as a subject, not AI as a passing mention. The \b on the
// bare "AI" is what stops it matching inside "said", "chair", or "maintain".
var aiTerm = regexp.MustCompile(`(?i)\bartificial intelligence\b|\bmachine learning\b|\bA\.?I\.?\b|\bgenerative ai\b|\balgorithmic\b|\bautomated decision\b`)

func mentionsAI(s string) bool { return s != "" && aiTerm.MatchString(s) }

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
			// Full raw line retained per retention policy: everything the
			// pipeline downloads is kept for future LLM development.
			meta := map[string]string{"url": docURL, "domain": c[3], "date": c[1],
				"persons": trunc(c[11], 800), "orgs": trunc(c[13], 800), "themes": trunc(c[7], 800), "title": title,
				"line": line}
			raw, _ := json.Marshal(meta)
			uh := sha256.Sum256([]byte(docURL))
			id := hex.EncodeToString(uh[:])[:32]
			var pub any
			// GDELT GKG stamps are YYYYMMDDHHMMSS. The date alone was being
			// stored, which flattened every story's coverage into a single
			// undifferentiated day and made an hour-level timeline on the site
			// impossible without inventing times. Keep the full stamp; the
			// column is timestamptz and always could have held it.
			if len(c[1]) >= 14 {
				pub = c[1][:4] + "-" + c[1][4:6] + "-" + c[1][6:8] + "T" +
					c[1][8:10] + ":" + c[1][10:12] + ":" + c[1][12:14] + "Z"
			} else if len(c[1]) >= 8 {
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

// publishNews clusters the day's gdelt coverage into top stories and
// publishes news/news.json to srj-content. Stories rank by breadth of
// coverage (unique outlets). Same GitHub-commit flow as before.
func publishNews(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	rows, err := db.Query(`SELECT d.title, d.url, to_char(d.published_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), d.raw->>'domain', coalesce(d.raw->>'persons',''), coalesce(d.raw->>'orgs','')
		FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
		WHERE s.key='gdelt' AND d.title <> '' AND d.fetched_at > now() - interval '36 hours'
		ORDER BY d.id DESC LIMIT 600`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type art struct{ Title, URL, Date, Domain, persons, orgs string }
	var arts []art
	for rows.Next() {
		var a art
		var d sql.NullString
		if rows.Scan(&a.Title, &a.URL, &d, &a.Domain, &a.persons, &a.orgs) == nil {
			a.Date = d.String
			arts = append(arts, a)
		}
	}

	stop := map[string]bool{"the": true, "a": true, "an": true, "of": true, "to": true, "in": true, "on": true, "for": true, "and": true, "with": true, "as": true, "at": true, "by": true, "is": true, "its": true, "ai": true, "artificial": true, "intelligence": true, "new": true, "how": true, "what": true, "why": true}
	toks := func(s string) map[string]bool {
		m := map[string]bool{}
		w := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !('a' <= r && r <= 'z' || '0' <= r && r <= '9') })
		for _, x := range w {
			if len(x) > 2 && !stop[x] {
				m[x] = true
			}
		}
		return m
	}
	sim := func(a, b map[string]bool) float64 {
		n := 0
		for k := range a {
			if b[k] {
				n++
			}
		}
		d := len(a)
		if len(b) < d {
			d = len(b)
		}
		if d == 0 {
			return 0
		}
		return float64(n) / float64(d)
	}

	type cluster struct {
		arts []art
		tk   map[string]bool
	}
	var cls []*cluster
	for _, a := range arts {
		tk := toks(a.Title)
		if len(tk) < 3 {
			continue
		}
		placed := false
		for _, c := range cls {
			if sim(tk, c.tk) >= 0.6 {
				c.arts = append(c.arts, a)
				for k := range tk {
					c.tk[k] = true
				}
				placed = true
				break
			}
		}
		if !placed {
			cls = append(cls, &cluster{arts: []art{a}, tk: tk})
		}
	}
	domains := func(c *cluster) int {
		m := map[string]bool{}
		for _, a := range c.arts {
			m[a.Domain] = true
		}
		return len(m)
	}
	sort.Slice(cls, func(i, j int) bool {
		if domains(cls[i]) != domains(cls[j]) {
			return domains(cls[i]) > domains(cls[j])
		}
		return len(cls[i].arts) > len(cls[j].arts)
	})
	if len(cls) > 10 {
		cls = cls[:10]
	}

	slugify := func(s string) string {
		s = strings.ToLower(s)
		var b strings.Builder
		dash := false
		for _, r := range s {
			if 'a' <= r && r <= 'z' || '0' <= r && r <= '9' {
				b.WriteRune(r)
				dash = false
			} else if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
			if b.Len() >= 60 {
				break
			}
		}
		return strings.Trim(b.String(), "-")
	}
	top := func(field func(art) string, n int) func(*cluster) []string {
		return func(c *cluster) []string {
			cnt := map[string]int{}
			for _, a := range c.arts {
				for _, p := range strings.Split(field(a), ";") {
					p = strings.TrimSpace(p)
					if p != "" {
						cnt[p]++
					}
				}
			}
			// Initialised, not declared nil. A nil slice marshals to JSON null,
			// not [], and the site prerenders one page per story: on 2026-07-28 a
			// story with no organizations shipped "Orgs": null, the Astro template
			// called .slice on it, and that single record failed the entire site
			// build. Empty must mean empty, everywhere this package emits JSON.
			ks := []string{}
			for k := range cnt {
				ks = append(ks, k)
			}
			sort.Slice(ks, func(i, j int) bool { return cnt[ks[i]] > cnt[ks[j]] })
			if len(ks) > n {
				ks = ks[:n]
			}
			return ks
		}
	}
	topPersons := top(func(a art) string { return a.persons }, 6)
	topOrgs := top(func(a art) string { return a.orgs }, 6)

	type story struct {
		Slug, Headline            string
		ArticleCount, DomainCount int
		Domains                   []string
		Persons, Orgs             []string
		Articles                  []map[string]string
	}
	var stories []story
	big := []string{}
	seen := map[string]bool{}
	for _, c := range cls {
		h := c.arts[0].Title
		sl := slugify(h)
		if sl == "" || seen[sl] {
			continue
		}
		seen[sl] = true
		dm := map[string]bool{}
		dl := []string{}
		as := []map[string]string{}
		for _, a := range c.arts {
			if !dm[a.Domain] {
				dm[a.Domain] = true
				dl = append(dl, a.Domain)
			}
			if len(as) < 15 {
				as = append(as, map[string]string{"Title": a.Title, "URL": a.URL, "Domain": a.Domain, "Date": a.Date})
			}
		}
		if len(dl) > 12 {
			dl = dl[:12]
		}
		stories = append(stories, story{Slug: sl, Headline: h, ArticleCount: len(c.arts), DomainCount: len(dm), Domains: dl, Persons: topPersons(c), Orgs: topOrgs(c), Articles: as})
		if len(big) < 4 {
			big = append(big, h)
		}
	}

	payload, _ := json.MarshalIndent(map[string]any{
		"generated":   time.Now().UTC().Format(time.RFC3339),
		"date":        time.Now().UTC().Format("2006-01-02"),
		"big_picture": big,
		"stories":     stories,
	}, "", " ")

	// THE GATE.
	//
	// This publish step commits straight to the default branch of srj-content,
	// and a push to srj-content is itself the site deploy trigger. There is no
	// review, no CI, and no staging step in between: whatever this function
	// writes is on the public site within minutes.
	//
	// That is fine while the data is well formed and unacceptable when it is
	// not, which is not hypothetical. On 2026-07-28 a single malformed story
	// failed the entire site build and blocked every deploy, including unrelated
	// ones, until the templates were patched by hand.
	//
	// So nothing is published unless it passes. A refusal to publish leaves
	// yesterday's briefing live, which is a good outcome: slightly stale beats
	// broken, and the failure is loud in the cron logs rather than silent.
	if err := validateNews(payload); err != nil {
		return fmt.Errorf("refusing to publish malformed news.json: %w", err)
	}

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
		var cur struct {
			SHA string `json:"sha"`
		}
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

// validateNews is the publish gate. It re-reads the marshalled payload the way
// the site will read it, and rejects anything the Astro build cannot survive.
//
// It parses the bytes rather than inspecting the in-memory structs on purpose.
// The failure this exists to prevent was a marshalling artefact, a nil slice
// becoming null, which is invisible from the Go side and only appears once the
// value has been through encoding/json. Checking the structs would have missed
// it. Check what actually ships.
//
// Every rule here maps to something that breaks a real page:
//
//	Slug         the story's URL. Empty means a page at a bad path.
//	Headline     the h1 and the <title>. Empty means an untitled page.
//	Articles     the entire body of the story page. Empty means a blank page.
//	null arrays  the 2026-07-28 build failure, exactly.
//	duplicates   two stories claiming one URL; the later one silently wins.
func validateNews(payload []byte) error {
	var doc struct {
		Generated  string             `json:"generated"`
		Date       string             `json:"date"`
		BigPicture *[]string          `json:"big_picture"`
		Stories    *[]json.RawMessage `json:"stories"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return fmt.Errorf("payload is not valid JSON: %w", err)
	}
	if doc.Generated == "" || doc.Date == "" {
		return fmt.Errorf("missing generated or date")
	}
	if doc.BigPicture == nil {
		return fmt.Errorf("big_picture is null, must be an array")
	}
	if doc.Stories == nil {
		return fmt.Errorf("stories is null, must be an array")
	}
	if len(*doc.Stories) == 0 {
		return fmt.Errorf("no stories: publishing an empty briefing would blank the page")
	}

	// Pointer fields distinguish "absent or null" from "present but empty",
	// which is the whole point of this check.
	seen := map[string]bool{}
	for i, raw := range *doc.Stories {
		var s struct {
			Slug         string               `json:"Slug"`
			Headline     string               `json:"Headline"`
			ArticleCount int                  `json:"ArticleCount"`
			DomainCount  int                  `json:"DomainCount"`
			Domains      *[]string            `json:"Domains"`
			Persons      *[]string            `json:"Persons"`
			Orgs         *[]string            `json:"Orgs"`
			Articles     *[]map[string]string `json:"Articles"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("story %d does not parse: %w", i, err)
		}
		where := fmt.Sprintf("story %d (%q)", i, s.Slug)
		if s.Slug == "" {
			return fmt.Errorf("%s: empty Slug, would render at a broken URL", where)
		}
		if seen[s.Slug] {
			return fmt.Errorf("%s: duplicate Slug, two stories cannot share one URL", where)
		}
		seen[s.Slug] = true
		if strings.TrimSpace(s.Headline) == "" {
			return fmt.Errorf("%s: empty Headline, would render an untitled page", where)
		}
		for name, arr := range map[string]*[]string{
			"Domains": s.Domains, "Persons": s.Persons, "Orgs": s.Orgs,
		} {
			if arr == nil {
				return fmt.Errorf("%s: %s is null, must be [] (this is the 2026-07-28 build break)", where, name)
			}
		}
		if s.Articles == nil {
			return fmt.Errorf("%s: Articles is null, must be []", where)
		}
		if len(*s.Articles) == 0 {
			return fmt.Errorf("%s: no articles, the story page would have no body", where)
		}
		for j, a := range *s.Articles {
			if strings.TrimSpace(a["URL"]) == "" || strings.TrimSpace(a["Title"]) == "" {
				return fmt.Errorf("%s: article %d missing URL or Title", where, j)
			}
		}
		if s.DomainCount < 1 || s.ArticleCount < 1 {
			return fmt.Errorf("%s: DomainCount and ArticleCount must both be at least 1", where)
		}
	}
	return nil
}

// publishLegislation writes the AI legislation tracker to srj-content.
//
// Source is LegiScan, which is the one regulatory adapter whose output is
// genuinely on topic: on 2026-07-28, 91 of its 100 stored documents were AI
// bills, against 15 of 101 for the Federal Register. It also carries exactly
// what a tracker needs and news.json does not: a jurisdiction, a bill number, a
// plain-language legislative stage, and the date that stage was reached.
//
// The AI filter is applied here as well as at fetch time, because the corpus is
// append-only and already holds rows fetched before the filter existed.
//
// Stage is LegiScan's own last_action string, verbatim. It is deliberately not
// mapped onto a tidy enum like "Committee" or "Passed": the mapping would be a
// guess dressed as a status, and "Signed by Governor" already says more than
// any bucket would.
func publishLegislation(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}

	rows, err := db.Query(`
		SELECT DISTINCT ON (d.raw->>'state', d.raw->>'bill_number')
		       coalesce(d.raw->>'state',''), coalesce(d.raw->>'bill_number',''),
		       d.title, d.url, coalesce(d.raw->>'last_action',''),
		       coalesce(d.raw->>'last_action_date',''), coalesce(d.raw->>'text_url','')
		FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
		WHERE s.key='legiscan' AND d.title <> ''
		ORDER BY d.raw->>'state', d.raw->>'bill_number', d.raw->>'last_action_date' DESC NULLS LAST, d.id DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type bill struct {
		State, Number, Title, URL, LastAction, LastActionDate, TextURL string
	}
	bills := []bill{}
	for rows.Next() {
		var b bill
		if rows.Scan(&b.State, &b.Number, &b.Title, &b.URL, &b.LastAction, &b.LastActionDate, &b.TextURL) != nil {
			continue
		}
		if b.State == "" || b.Number == "" || !mentionsAI(b.Title) {
			continue
		}
		bills = append(bills, b)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Most recently acted first: a tracker is only useful if movement is at
	// the top. Bills with no recorded action sort last rather than being
	// dropped, since "introduced, nothing since" is itself a status.
	sort.SliceStable(bills, func(i, j int) bool {
		if bills[i].LastActionDate != bills[j].LastActionDate {
			return bills[i].LastActionDate > bills[j].LastActionDate
		}
		if bills[i].State != bills[j].State {
			return bills[i].State < bills[j].State
		}
		return bills[i].Number < bills[j].Number
	})

	states := map[string]bool{}
	for _, b := range bills {
		states[b.State] = true
	}

	payload, _ := json.MarshalIndent(map[string]any{
		"generated":     time.Now().UTC().Format(time.RFC3339),
		"date":          time.Now().UTC().Format("2006-01-02"),
		"source":        "LegiScan",
		"jurisdictions": len(states),
		"count":         len(bills),
		"bills":         bills,
	}, "", " ")

	if err := validateLegislation(payload); err != nil {
		return fmt.Errorf("refusing to publish malformed legislation.json: %w", err)
	}
	return putToContent(tok, "legislation/legislation.json",
		"pipeline: AI legislation tracker refresh", payload)
}

// validateLegislation is the same gate discipline as validateNews: check the
// bytes that will ship, refuse rather than publish, and let yesterday's file
// stand. Every rule maps to something that breaks a rendered row.
func validateLegislation(payload []byte) error {
	var doc struct {
		Generated string             `json:"generated"`
		Date      string             `json:"date"`
		Bills     *[]json.RawMessage `json:"bills"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return fmt.Errorf("payload is not valid JSON: %w", err)
	}
	if doc.Generated == "" || doc.Date == "" {
		return fmt.Errorf("missing generated or date")
	}
	if doc.Bills == nil {
		return fmt.Errorf("bills is null, must be an array")
	}
	if len(*doc.Bills) == 0 {
		return fmt.Errorf("no bills: publishing an empty tracker would blank the page")
	}
	seen := map[string]bool{}
	for i, raw := range *doc.Bills {
		var b struct {
			State, Number, Title, URL string
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Errorf("bill %d does not parse: %w", i, err)
		}
		key := b.State + " " + b.Number
		if strings.TrimSpace(b.State) == "" || strings.TrimSpace(b.Number) == "" {
			return fmt.Errorf("bill %d: missing state or bill number, the row's identity", i)
		}
		if seen[key] {
			return fmt.Errorf("bill %d: duplicate %s, one bill cannot occupy two rows", i, key)
		}
		seen[key] = true
		if strings.TrimSpace(b.Title) == "" {
			return fmt.Errorf("bill %d (%s): empty title", i, key)
		}
		if !strings.HasPrefix(b.URL, "http") {
			return fmt.Errorf("bill %d (%s): URL is not a link, the row would cite nothing", i, key)
		}
	}
	return nil
}

// putToContent writes one file to srj-content via the GitHub contents API,
// reading the current blob SHA first so the write is an update rather than a
// rejected create. Shared by every publish step.
func putToContent(tok, path, message string, payload []byte) error {
	api := "https://api.github.com/repos/srjordan6/srj-content/contents/" + path
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
		var cur struct {
			SHA string `json:"sha"`
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && json.Unmarshal(b, &cur) == nil {
			sha = cur.SHA
		}
	}
	put := map[string]any{"message": message,
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
		return fmt.Errorf("github PUT %s %d: %.200s", path, resp.StatusCode, b)
	}
	return nil
}
