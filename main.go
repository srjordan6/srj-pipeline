package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
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
		for _, s := range []string{"federal_register", "legiscan", "gdelt", "intel", "archive_news", "publish_news", "publish_legislation", "publish_leaderboard", "publish_lawsuits", "publish_intel", "sync_people", "sync_content", "twoai_build", "twoai_publish", "arxiv_watch", "export_corpus", "deploy_site"} {
			cmd := exec.Command(os.Args[0], s)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			cmd.Run() // a failing source must not block the others
		}
		return
	}

	// publish_leaderboard needs no database: it is a pure HTTP fetch of a
	// public leaderboard mirror. Handled before the db open so it still runs
	// on a host with no DATABASE_URL set.
	if src == "publish_leaderboard" {
		if err := publishLeaderboard(); err != nil {
			fmt.Fprintln(os.Stderr, "publish_leaderboard:", err)
			os.Exit(1)
		}
		fmt.Println("publish_leaderboard: ok")
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
	if src == "publish_lawsuits" {
		if err := publishLawsuits(db); err != nil {
			fmt.Fprintln(os.Stderr, "publish_lawsuits:", err)
			os.Exit(1)
		}
		fmt.Println("publish_lawsuits: ok")
		return
	}
	if src == "publish_intel" {
		if err := publishIntel(db); err != nil {
			fmt.Fprintln(os.Stderr, "publish_intel:", err)
			os.Exit(1)
		}
		fmt.Println("publish_intel: ok")
		return
	}
	if src == "intel" {
		if err := intelSync(db); err != nil {
			fmt.Fprintln(os.Stderr, "intel:", err)
			os.Exit(1)
		}
		fmt.Println("intel: ok")
		return
	}
	if src == "arxiv_watch" {
		if err := arxivWatch(db); err != nil {
			fmt.Fprintln(os.Stderr, "arxiv_watch:", err)
			os.Exit(1)
		}
		return
	}

	if src == "sync_people" {
		if err := syncPeople(db); err != nil {
			fmt.Fprintln(os.Stderr, "sync_people:", err)
			os.Exit(1)
		}
		return
	}

	if src == "sync_content" {
		if err := syncContent(db); err != nil {
			fmt.Fprintln(os.Stderr, "sync_content:", err)
			os.Exit(1)
		}
		return
	}

	if src == "twoai_build" {
		if err := twoaiBuild(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_build:", err)
			os.Exit(1)
		}
		return
	}

	if src == "twoai_publish" {
		if err := twoaiPublish(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_publish:", err)
			os.Exit(1)
		}
		return
	}

	if src == "deploy_site" {
		if err := deploySite(); err != nil {
			fmt.Fprintln(os.Stderr, "deploy_site:", err)
			os.Exit(1)
		}
		fmt.Println("deploy_site: ok")
		return
	}

	if src == "email_route" {
		if err := emailRoute(db); err != nil {
			fmt.Fprintln(os.Stderr, "email_route:", err)
			os.Exit(1)
		}
		return
	}

	if src == "export_corpus" {
		if err := exportCorpus(db); err != nil {
			fmt.Fprintln(os.Stderr, "export_corpus:", err)
			os.Exit(1)
		}
		return
	}

	if src == "archive_news" {
		if err := archiveNews(db); err != nil {
			fmt.Fprintln(os.Stderr, "archive_news:", err)
			os.Exit(1)
		}
		fmt.Println("archive_news: ok")
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

// legiscan pulls state AI bills, completely. getSearchRaw returns the FULL
// result set for the query (up to 2,000 ids per page with LegiScan's own
// change_hash), where the old getSearch capped at the top ~100 by relevance
// and a brand-new bill could in theory sit below the fold for days. Only ids
// that are new or changed get hydrated through getBill, so a quiet day costs
// one search call; the hydration budget caps a heavy first pass and carries
// the remainder to the next run. Relevance below 50 is a passing mention,
// not an AI bill. Key from LEGISCAN_API_KEY.
func legiscan(db *sql.DB, sourceID int) (fetched, added int, err error) {
	key := os.Getenv("LEGISCAN_API_KEY")
	if key == "" {
		return 0, 0, fmt.Errorf("LEGISCAN_API_KEY not set")
	}
	client := &http.Client{Timeout: 60 * time.Second}
	const hydrateBudget = 300
	hydrated := 0
	for page := 1; page <= 3; page++ {
		url := fmt.Sprintf("https://api.legiscan.com/?key=%s&op=getSearchRaw&state=ALL&query=%s&page=%d",
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
			Status       string `json:"status"`
			SearchResult struct {
				Summary struct {
					PageTotal int `json:"page_total"`
				} `json:"summary"`
				Results []struct {
					Relevance  int    `json:"relevance"`
					BillID     int    `json:"bill_id"`
					ChangeHash string `json:"change_hash"`
				} `json:"results"`
			} `json:"searchresult"`
		}
		if e := json.Unmarshal(body, &payload); e != nil {
			return fetched, added, e
		}
		if payload.Status != "OK" {
			return fetched, added, fmt.Errorf("legiscan status %s", payload.Status)
		}
		for _, r := range payload.SearchResult.Results {
			if r.BillID == 0 || r.Relevance < 50 {
				continue
			}
			fetched++
			extID := fmt.Sprintf("%d", r.BillID)
			var exists bool
			if e := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pipeline.documents
				WHERE source_id=$1 AND external_id=$2 AND change_hash=$3)`,
				sourceID, extID, r.ChangeHash).Scan(&exists); e != nil {
				return fetched, added, e
			}
			if exists {
				continue
			}
			if hydrated >= hydrateBudget {
				continue // picked up on the next run
			}
			bu := fmt.Sprintf("https://api.legiscan.com/?key=%s&op=getBill&id=%d", key, r.BillID)
			br, e := client.Get(bu)
			if e != nil {
				fmt.Fprintln(os.Stderr, "legiscan getBill", r.BillID, ":", e)
				continue
			}
			bb, e := io.ReadAll(br.Body)
			br.Body.Close()
			if e != nil {
				continue
			}
			var bp struct {
				Status string `json:"status"`
				Bill   struct {
					BillID     int    `json:"bill_id"`
					State      string `json:"state"`
					BillNumber string `json:"bill_number"`
					Title      string `json:"title"`
					URL        string `json:"url"`
					StatusDate string `json:"status_date"`
				} `json:"bill"`
			}
			if json.Unmarshal(bb, &bp) != nil || bp.Status != "OK" || bp.Bill.BillID == 0 {
				fmt.Fprintln(os.Stderr, "legiscan getBill", r.BillID, ": bad payload")
				continue
			}
			hydrated++
			var pub any
			if bp.Bill.StatusDate != "" {
				pub = bp.Bill.StatusDate
			}
			ok, e := insertDoc(db, sourceID, extID, r.ChangeHash, bp.Bill.URL,
				bp.Bill.State+" "+bp.Bill.BillNumber+": "+bp.Bill.Title, pub, bb)
			if e != nil {
				return fetched, added, e
			}
			if ok {
				added++
			}
			time.Sleep(500 * time.Millisecond)
		}
		if page >= payload.SearchResult.Summary.PageTotal {
			break
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
	// Per-URL summary and lead text, for the story summary at the top of each
	// news page. Own-words summaries only; the site never republishes bodies.
	type docText struct{ summary, text string }
	docs := map[string]docText{}
	{
		drows, derr := db.Query(`SELECT d.url, COALESCE(d.summary,''), COALESCE(substr(d.fulltext,1,12000),'')
			FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
			WHERE s.key='gdelt' AND d.fetched_at > now() - interval '36 hours'
			AND (d.summary IS NOT NULL OR d.fulltext IS NOT NULL)`)
		if derr == nil {
			for drows.Next() {
				var u, sm, tx string
				if drows.Scan(&u, &sm, &tx) == nil {
					docs[u] = docText{summary: sm, text: tx}
				}
			}
			drows.Close()
		}
	}
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
		Summary                   string
		SummaryURL, SummaryDomain string
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
		summary, sumURL, sumDomain := "", "", ""
		for _, a := range c.arts {
			dt, okd := docs[a.URL]
			if !okd || (dt.summary == "" && dt.text == "") {
				continue
			}
			if dt.summary == "" {
				s2, serr := anthropicSummarize(h, dt.text)
				if serr != nil {
					fmt.Fprintln(os.Stderr, "publish_news summarize:", serr)
					continue
				}
				dt.summary = s2
				db.Exec(`UPDATE pipeline.documents SET summary=$1 WHERE url=$2`, s2, a.URL)
			}
			summary, sumURL, sumDomain = dt.summary, a.URL, a.Domain
			break
		}
		stories = append(stories, story{Slug: sl, Headline: h, Summary: summary, SummaryURL: sumURL, SummaryDomain: sumDomain, ArticleCount: len(c.arts), DomainCount: len(dm), Domains: dl, Persons: topPersons(c), Orgs: topOrgs(c), Articles: as})
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

// publishLeaderboard writes the model leaderboard to srj-content.
//
// WHY THIS ADAPTER EXISTS, and it is worth stating plainly. The request that
// prompted it arrived with hand-pasted benchmark figures: "Claude Opus 4.8
// leads at ~1,580 Elo, followed by GPT-5.5 Pro and Gemini 3.1 Pro." Checked
// against arena.ai the same day, every clause was wrong. The actual #1 was
// claude-fable-5 at 1508. Opus 4.8 Thinking sat at #14 on 1484. GPT-5.5 Pro was
// not on the text board at all. The 1580 figure appears to be the CODE board's
// range misread as the text board's.
//
// The numbers traced back to an SEO content farm, not to arena.ai. That is the
// whole argument for fetching rather than typing: a leaderboard is the most
// perishable content on the site, it moves weekly, and a hand-pasted table is
// wrong within days and then stays wrong. On a site whose competitive position
// is correction rather than currency, publishing a stale table copied from an
// aggregator would undo the thing the governance library is for.
//
// SOURCE. arena.ai (formerly LMSYS Chatbot Arena) publishes no API; its own
// mirror repo says so. This reads the daily GitHub snapshot, which carries the
// upstream fetched_at and source_url in every file, so the provenance chain
// stays legible: arena.ai is the source, the mirror is the access route, and
// both are named on the rendered page.
//
// TWO UPSTREAM FACTS LEARNED BY RUNNING IT, both of which broke the first
// version and neither of which is documented in the mirror's schema:
//
//  1. Scores arrive as JSON floats (1508.0), not integers, despite the schema
//     table saying int. Decoding into *int fails the whole board silently.
//  2. The agent board is RANK-ONLY. All ten entries carry a null score, ci, and
//     votes. That is a real property of the upstream board, not corruption, so
//     the gate must allow it while still catching a scored board that has lost
//     its numbers.
//
// The second is handled by requiring internal consistency rather than presence:
// a board must be all-scored or all-unscored. Half a board losing its scores is
// a malformation; a board that never had them is a format.
//
// WHAT IS DELIBERATELY NOT COLLECTED. MMLU-Pro, SWE-bench Verified, GPQA
// Diamond, MATH, tokens-per-second, and context-window figures were all in the
// original paste. None are in this feed, none could be verified against a
// primary source in the same pass, and the ones that could be checked were
// wrong. They are omitted rather than carried across on trust. If a verified
// machine-readable source for any of them is found later, it gets its own
// adapter and its own gate. An unsourced number on this site is worse than a
// missing one.
func publishLeaderboard() error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	const mirror = "https://raw.githubusercontent.com/oolong-tea-2026/arena-ai-leaderboards/main/data"
	client := &http.Client{Timeout: 60 * time.Second}

	get := func(url string) ([]byte, error) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("GET %s: %d", url, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}

	// latest.json is the pointer to the newest dated snapshot directory.
	// Following it rather than constructing today's date means a day the
	// upstream fetch failed yields yesterday's board, correctly stamped,
	// instead of a 404 that would blank the page.
	var ptr struct {
		Date string `json:"date"`
		Path string `json:"path"`
	}
	b, err := get(mirror + "/latest.json")
	if err != nil {
		return fmt.Errorf("latest pointer: %w", err)
	}
	if err := json.Unmarshal(b, &ptr); err != nil {
		return fmt.Errorf("latest pointer parse: %w", err)
	}
	if ptr.Path == "" {
		return fmt.Errorf("latest pointer carries no path")
	}

	// All numerics are float64 because the upstream emits floats regardless
	// of what its schema table claims. Rendering formats them.
	type model struct {
		Rank    int      `json:"rank"`
		Model   string   `json:"model"`
		Vendor  *string  `json:"vendor"`
		License *string  `json:"license"`
		Score   *float64 `json:"score"`
		CI      *float64 `json:"ci"`
		Votes   *float64 `json:"votes"`
	}
	type board struct {
		Key       string  `json:"key"`
		Label     string  `json:"label"`
		Note      string  `json:"note"`
		Scored    bool    `json:"scored"`
		SourceURL string  `json:"source_url"`
		FetchedAt string  `json:"fetched_at"`
		Count     int     `json:"count"`
		Models    []model `json:"models"`
	}

	// Only the boards a Chat & General LLM reader is actually choosing
	// between. The image, video, and edit boards belong to different
	// catalog categories and would be noise here.
	wanted := []struct{ key, label, note string }{
		{"text", "Overall text and chat",
			"Head-to-head human preference on general conversation. The closest thing the field has to a general-purpose ranking."},
		{"code", "Code generation",
			"The same vote mechanic restricted to coding prompts. Ranks differ sharply from the text board, which is the reason to read both."},
		{"agent", "Agentic use",
			"Multi-step tool-using tasks rather than single answers. The board that matters if the model will act rather than reply. Published as an order only, with no ratings."},
		{"vision", "Vision and multimodal",
			"Image understanding and mixed text-image prompts."},
		{"search", "Search-grounded answers",
			"Models answering with live retrieval, where citation quality matters as much as fluency."},
	}

	boards := []board{}
	for _, w := range wanted {
		raw, err := get(fmt.Sprintf("%s/%s/%s.json", mirror, ptr.Path, w.key))
		if err != nil {
			// A single missing board is not fatal. The upstream index
			// varies by day and a partial page beats no page.
			fmt.Fprintf(os.Stderr, "leaderboard: skipping %s: %v\n", w.key, err)
			continue
		}
		var f struct {
			Meta struct {
				Leaderboard string `json:"leaderboard"`
				SourceURL   string `json:"source_url"`
				FetchedAt   string `json:"fetched_at"`
				ModelCount  int    `json:"model_count"`
			} `json:"meta"`
			Models []model `json:"models"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			fmt.Fprintf(os.Stderr, "leaderboard: %s does not parse: %v\n", w.key, err)
			continue
		}
		// Top 12 per board. The full boards run past 100 models; a
		// reference page that reprints all of them is a worse read than
		// the source, and the source is linked on every board.
		top := f.Models
		if len(top) > 12 {
			top = top[:12]
		}
		scored := len(top) > 0
		for _, m := range top {
			if m.Score == nil {
				scored = false
				break
			}
		}
		boards = append(boards, board{
			Key: w.key, Label: w.label, Note: w.note, Scored: scored,
			SourceURL: f.Meta.SourceURL, FetchedAt: f.Meta.FetchedAt,
			Count: f.Meta.ModelCount, Models: top,
		})
	}

	payload, _ := json.MarshalIndent(map[string]any{
		"generated":   time.Now().UTC().Format(time.RFC3339),
		"date":        ptr.Date,
		"source":      "arena.ai (formerly LMSYS Chatbot Arena)",
		"source_url":  "https://arena.ai/leaderboard/",
		"access_note": "arena.ai publishes no API. Read via the daily public snapshot mirror at github.com/oolong-tea-2026/arena-ai-leaderboards, which preserves upstream fetched_at and source_url per board.",
		"method":      "Crowdsourced blind pairwise voting, scored with a Bradley-Terry model and reported as an Elo-style rating with a 95 percent confidence interval. Gaps under roughly 10 points sit inside the noise floor and should not be read as a ranking.",
		"boards":      boards,
	}, "", " ")

	if err := validateLeaderboard(payload); err != nil {
		return fmt.Errorf("refusing to publish malformed leaderboard.json: %w", err)
	}
	return putToContent(tok, "leaderboard/leaderboard.json",
		"pipeline: model leaderboard refresh", payload)
}

// validateLeaderboard applies the same gate discipline as validateNews and
// validateLegislation: parse the bytes that will actually ship, refuse rather
// than publish, and leave yesterday's file standing. Stale beats broken.
//
// Every rule maps to something visibly wrong on the rendered page. A board with
// no models renders an empty table. A missing fetched_at strips the page of the
// one thing that makes a perishable table trustworthy, which is the date it was
// true. Ranks that skip or repeat render a table that silently misorders.
//
// The scored rule is consistency, not presence, because the agent board is
// legitimately rank-only upstream. All-scored and all-unscored are both valid;
// a mix means a scored board lost its numbers mid-fetch.
func validateLeaderboard(payload []byte) error {
	var doc struct {
		Generated string `json:"generated"`
		Date      string `json:"date"`
		Source    string `json:"source"`
		Boards    *[]struct {
			Key       string `json:"key"`
			Label     string `json:"label"`
			Scored    bool   `json:"scored"`
			FetchedAt string `json:"fetched_at"`
			SourceURL string `json:"source_url"`
			Models    *[]struct {
				Rank  int      `json:"rank"`
				Model string   `json:"model"`
				Score *float64 `json:"score"`
			} `json:"models"`
		} `json:"boards"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return fmt.Errorf("payload is not valid JSON: %w", err)
	}
	if doc.Generated == "" || doc.Date == "" || doc.Source == "" {
		return fmt.Errorf("missing generated, date, or source")
	}
	if doc.Boards == nil {
		return fmt.Errorf("boards is null, must be an array")
	}
	if len(*doc.Boards) == 0 {
		return fmt.Errorf("no boards: publishing this would blank the leaderboard section")
	}
	seen := map[string]bool{}
	for i, b := range *doc.Boards {
		if strings.TrimSpace(b.Key) == "" || strings.TrimSpace(b.Label) == "" {
			return fmt.Errorf("board %d: missing key or label", i)
		}
		if seen[b.Key] {
			return fmt.Errorf("board %d: duplicate key %q", i, b.Key)
		}
		seen[b.Key] = true
		if strings.TrimSpace(b.FetchedAt) == "" {
			return fmt.Errorf("board %q: no fetched_at, an undated leaderboard is not a fact", b.Key)
		}
		if !strings.HasPrefix(b.SourceURL, "http") {
			return fmt.Errorf("board %q: source_url is not a link, the table would cite nothing", b.Key)
		}
		if b.Models == nil {
			return fmt.Errorf("board %q: models is null, must be []", b.Key)
		}
		if len(*b.Models) == 0 {
			return fmt.Errorf("board %q: no models, the table would render empty", b.Key)
		}
		for j, m := range *b.Models {
			if strings.TrimSpace(m.Model) == "" {
				return fmt.Errorf("board %q model %d: empty model name", b.Key, j)
			}
			if m.Rank != j+1 {
				return fmt.Errorf("board %q model %d (%s): rank is %d, ranks must run 1..n without gaps or repeats",
					b.Key, j, m.Model, m.Rank)
			}
			if b.Scored && m.Score == nil {
				return fmt.Errorf("board %q model %d (%s): board is scored but this row has none, so a scored board has lost its numbers",
					b.Key, j, m.Model)
			}
			if !b.Scored && m.Score != nil {
				return fmt.Errorf("board %q model %d (%s): board is marked unscored but carries a score",
					b.Key, j, m.Model)
			}
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

// ---- intel: AI Lawsuit Database + AI intel sync ----------------------------
//
// Keeps the ai_lawsuits table current from CourtListener, fills docket numbers
// still marked pending, queues newly filed AI lawsuits into
// ai_lawsuit_candidates, and watches Hugging Face plus vendor feeds for new
// models and terminology into ai_intel_candidates. Results log to
// srj_intel_log. COURTLISTENER_TOKEN is required for docket-detail reads
// (search works anonymously); without it the refresh job logs and moves on.

func clGet(path string, params map[string]string, out any) error {
	req, err := http.NewRequest("GET", "https://www.courtlistener.com/api/rest/v4"+path, nil)
	if err != nil {
		return err
	}
	q := req.URL.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	req.URL.RawQuery = q.Encode()
	req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (srjconsultingservices.com)")
	if tok := os.Getenv("COURTLISTENER_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Token "+tok)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == 429 {
			resp.Body.Close()
			time.Sleep(time.Duration(15*attempt) * time.Second)
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("courtlistener %s: %s", path, resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return fmt.Errorf("courtlistener rate limited: %s", path)
}

type clSearch struct {
	Results []struct {
		CaseName     string `json:"caseName"`
		DocketNumber string `json:"docketNumber"`
		DocketID     int64  `json:"docket_id"`
		Court        string `json:"court"`
		DateFiled    string `json:"dateFiled"`
		Snippet      string `json:"snippet"`
	} `json:"results"`
}

var docketIDRe = regexp.MustCompile(`/docket/(\d+)/`)

// intelRefresh updates timeline + latest development for tracked cases whose
// docket has moved since the stored latest_development_date.
func intelRefresh(db *sql.DB) (checked, updated int, err error) {
	rows, err := db.Query(`SELECT id, slug, courtlistener_url, COALESCE(latest_development_date::text,''), COALESCE(timeline::text,'[]')
		FROM ai_lawsuits WHERE is_active AND courtlistener_url IS NOT NULL`)
	if err != nil {
		return 0, 0, err
	}
	type caseRow struct {
		id                 int64
		slug, clURL, since string
		timeline           string
	}
	var cases []caseRow
	for rows.Next() {
		var c caseRow
		if err := rows.Scan(&c.id, &c.slug, &c.clURL, &c.since, &c.timeline); err != nil {
			rows.Close()
			return checked, updated, err
		}
		cases = append(cases, c)
	}
	rows.Close()
	for _, c := range cases {
		m := docketIDRe.FindStringSubmatch(c.clURL)
		if m == nil {
			continue
		}
		did := m[1]
		checked++
		var docket struct {
			DateLastFiling string `json:"date_last_filing"`
		}
		if err := clGet("/dockets/"+did+"/", nil, &docket); err != nil {
			fmt.Fprintln(os.Stderr, "intel refresh", c.slug, "docket fetch:", err)
			time.Sleep(2 * time.Second)
			continue
		}
		if docket.DateLastFiling == "" || (c.since != "" && docket.DateLastFiling <= c.since) {
			time.Sleep(2 * time.Second)
			continue
		}
		var entries struct {
			Results []struct {
				DateFiled   string          `json:"date_filed"`
				EntryNumber json.RawMessage `json:"entry_number"`
				Description string          `json:"description"`
			} `json:"results"`
		}
		if err := clGet("/docket-entries/", map[string]string{
			"docket": did, "order_by": "-date_filed", "page_size": "5",
		}, &entries); err != nil {
			fmt.Fprintln(os.Stderr, "intel refresh", c.slug, "entries fetch:", err)
			time.Sleep(2 * time.Second)
			continue
		}
		var existing []map[string]any
		json.Unmarshal([]byte(c.timeline), &existing)
		seen := map[string]bool{}
		for _, e := range existing {
			d, _ := e["date"].(string)
			n, _ := e["doc_no"].(string)
			seen[d+"|"+n] = true
		}
		var fresh []map[string]any
		for _, en := range entries.Results {
			desc := strings.TrimSpace(en.Description)
			docNo := strings.Trim(string(en.EntryNumber), `"null`)
			if en.DateFiled == "" || desc == "" || seen[en.DateFiled+"|"+docNo] {
				continue
			}
			fresh = append(fresh, map[string]any{
				"date":   en.DateFiled,
				"title":  trunc(desc, 300),
				"doc_no": docNo,
				"url":    "https://www.courtlistener.com/docket/" + did + "/",
			})
		}
		if len(fresh) > 0 {
			merged := append(fresh, existing...)
			sort.Slice(merged, func(i, j int) bool {
				di, _ := merged[i]["date"].(string)
				dj, _ := merged[j]["date"].(string)
				return di > dj
			})
			payload, _ := json.Marshal(merged)
			newest := fresh[0]
			if _, err := db.Exec(`UPDATE ai_lawsuits SET timeline=$1, latest_development=$2,
				latest_development_date=$3, updated_at=now() WHERE id=$4`,
				payload, newest["title"], newest["date"], c.id); err != nil {
				fmt.Fprintln(os.Stderr, "intel refresh", c.slug, "update:", err)
				continue
			}
			updated++
			fmt.Printf("intel refresh %s: %d new docket entries through %v\n", c.slug, len(fresh), newest["date"])
		}
		time.Sleep(2 * time.Second)
	}
	return checked, updated, nil
}

// intelResolve fills docket numbers still marked pending verification straight
// from CourtListener search.
func intelResolve(db *sql.DB) (resolved int, err error) {
	rows, err := db.Query(`SELECT id, slug, case_name, COALESCE(defendants,'')
		FROM ai_lawsuits WHERE docket ILIKE '%pending%' AND is_active`)
	if err != nil {
		return 0, err
	}
	type pend struct {
		id                         int64
		slug, caseName, defendants string
	}
	var pending []pend
	for rows.Next() {
		var p pend
		if err := rows.Scan(&p.id, &p.slug, &p.caseName, &p.defendants); err != nil {
			rows.Close()
			return resolved, err
		}
		pending = append(pending, p)
	}
	rows.Close()
	paren := regexp.MustCompile(`\(.*?\)`)
	for _, p := range pending {
		q := strings.TrimSpace(paren.ReplaceAllString(p.caseName, ""))
		var res clSearch
		if err := clGet("/search/", map[string]string{
			"type": "r", "q": `"` + q + `"`, "order_by": "score desc",
		}, &res); err != nil {
			fmt.Fprintln(os.Stderr, "intel resolve", p.slug, "search:", err)
			continue
		}
		surname := strings.ToLower(strings.TrimSpace(strings.Split(strings.Split(p.defendants, ";")[0], ",")[0]))
		if i := strings.Index(surname, " "); i > 0 {
			surname = surname[:i]
		}
		for i, h := range res.Results {
			if i >= 5 {
				break
			}
			if h.DocketNumber == "" || h.DocketID == 0 || surname == "" ||
				!strings.Contains(strings.ToLower(h.CaseName), surname) {
				continue
			}
			clURL := fmt.Sprintf("https://www.courtlistener.com/docket/%d/", h.DocketID)
			if _, err := db.Exec(`UPDATE ai_lawsuits SET docket=$1, courtlistener_url=$2, updated_at=now() WHERE id=$3`,
				h.DocketNumber, clURL, p.id); err == nil {
				resolved++
				fmt.Printf("intel resolve %s: docket %s\n", p.slug, h.DocketNumber)
			}
			break
		}
		time.Sleep(2 * time.Second)
	}
	return resolved, nil
}

// intelDiscover queues newly filed AI lawsuits as candidates for review, then
// auto-promotes the unambiguous ones into ai_lawsuits so the tracker stays
// current without a human in the loop.
//
// Two searches run, because they fail in opposite directions. The SUBJECT
// search finds AI litigation against defendants nobody has heard of yet, but a
// broad topical query returns thousands of rows where "machine learning" or
// "discrimination" appear in unrelated boilerplate: a live check on 2026-08-02
// returned 2,925 hits, most of them pharmaceutical product-liability suits. The
// DEFENDANT search is the opposite, precise and shallow: caseName against the
// known AI defendants returns almost pure signal (Anthropic 39 dockets, Workday
// 19, Clearview 9, Character Technologies 8, Stability AI 6).
//
// The claim vocabulary also widened past copyright. Copyright and training data
// were the first wave, but the categories now growing fastest are chatbot
// wrongful death and product liability, AI hiring discrimination, right of
// publicity and deepfakes, biometric privacy, and AI-washing securities fraud.
// A tracker that only watched copyright would have shown a shrinking field while
// the actual field expanded.
func intelDiscover(db *sql.DB) (added int, err error) {
	since := time.Now().AddDate(0, 0, -45).Format("2006-01-02")

	// Defendants worth watching by name. Precision comes from the party, so
	// these need no topical qualifier at all.
	defendants := []string{
		"OpenAI", "Anthropic", "Midjourney", "Stability AI", "Uncharted Labs",
		"Suno", "Perplexity", "Character Technologies", "Clearview AI",
		"Workday", "HireVue", "Minimax", "Runway AI", "ElevenLabs",
		"Nvidia", "Hugging Face", "Scale AI", "Cohere", "Mistral AI",
	}

	// Subject queries, one per claim family rather than one giant OR, so a
	// noisy family cannot swamp the others in a single ranked result set.
	subjects := []string{
		`("artificial intelligence" OR "generative AI" OR "large language model") AND (copyright OR "training data" OR infringement)`,
		`(chatbot OR "companion AI" OR "AI companion") AND ("wrongful death" OR suicide OR "product liability" OR "failure to warn")`,
		`("artificial intelligence" OR algorithm OR "automated decision") AND ("employment discrimination" OR "disparate impact" OR "hiring discrimination" OR ADEA OR "Title VII")`,
		`(deepfake OR "digital replica" OR "voice clone" OR "AI-generated likeness") AND ("right of publicity" OR defamation OR Lanham)`,
		`("facial recognition" OR biometric OR "face template") AND (BIPA OR "biometric privacy" OR "Illinois Biometric")`,
		`("artificial intelligence" OR "AI-powered") AND ("securities fraud" OR "materially false" OR "misled investors" OR "AI washing")`,
	}

	// Relevance scoring. "Artificial intelligence" appears in patent and
	// trademark boilerplate constantly; the first live run queued NASCAR
	// trademark chaff alongside a real new Anthropic suit. Score on the
	// signals that separate AI-subject litigation from passing mentions,
	// and queue only what clears the bar. The score is stored so review
	// can sort by it.
	aiParty := regexp.MustCompile(`(?i)openai|anthropic|meta platforms|midjourney|stability ai|suno|uncharted labs|udio|perplexity|x\.?ai|google|alphabet|microsoft|nvidia|hugging face|character\.?ai|character technologies|deepseek|mistral|runway|eleven ?labs|minimax|clearview|workday|hirevue|scale ai|cohere`)
	aiSubject := regexp.MustCompile(`(?i)training data|generative|large language|chatbot|machine learning|neural|copyright|infring|scrap(e|ing)|dataset|deepfake|right of publicity|biometric|wrongful death|product liability|disparate impact|securities fraud|ai washing|facial recognition|automated decision`)
	patentNoise := regexp.MustCompile(`(?i)patent|'\d{3} patent|licensing, llc|innovations ltd|ip pty|technology licensing`)

	queue := func(h struct {
		CaseName     string `json:"caseName"`
		DocketNumber string `json:"docketNumber"`
		DocketID     int64  `json:"docket_id"`
		Court        string `json:"court"`
		DateFiled    string `json:"dateFiled"`
		Snippet      string `json:"snippet"`
	}, base int) {
		if h.DocketID == 0 {
			return
		}
		score := base
		if aiParty.MatchString(h.CaseName) {
			score += 3
		}
		if aiSubject.MatchString(h.Snippet) || aiSubject.MatchString(h.CaseName) {
			score += 2
		}
		if patentNoise.MatchString(h.CaseName) {
			score -= 3
		}
		if score < 2 {
			return
		}
		var tracked bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM ai_lawsuits WHERE courtlistener_url LIKE $1)`,
			fmt.Sprintf("%%/docket/%d/%%", h.DocketID)).Scan(&tracked)
		if tracked {
			return
		}
		r, e := db.Exec(`INSERT INTO ai_lawsuit_candidates
			(source, source_id, case_name, court, docket, filed_date, url, snippet, score)
			VALUES ('courtlistener', $1, $2, $3, $4, NULLIF($5,'')::date, $6, $7, $8)
			ON CONFLICT (source_id) DO NOTHING`,
			fmt.Sprintf("cl-docket-%d", h.DocketID), h.CaseName, h.Court, h.DocketNumber,
			h.DateFiled, fmt.Sprintf("https://www.courtlistener.com/docket/%d/", h.DocketID),
			trunc(h.Snippet, 500), score)
		if e != nil {
			return
		}
		if n, _ := r.RowsAffected(); n > 0 {
			added++
			fmt.Printf("intel discover: queued (score %d) %s %s\n", score, h.CaseName, h.DocketNumber)
		}
	}

	for _, q := range subjects {
		var res clSearch
		if e := clGet("/search/", map[string]string{
			"type": "r", "q": q, "filed_after": since, "order_by": "dateFiled desc",
		}, &res); e != nil {
			fmt.Fprintln(os.Stderr, "intel discover subject:", e)
			continue
		}
		for i, h := range res.Results {
			if i >= 25 {
				break
			}
			queue(h, 0)
		}
		time.Sleep(2 * time.Second)
	}

	// Defendant sweep runs on a longer window: a suit against a known AI
	// company is worth tracking whenever it was filed, not only in the last
	// 45 days. The base score of 3 reflects that the party alone is the
	// evidence.
	dsince := time.Now().AddDate(0, 0, -365).Format("2006-01-02")
	for _, d := range defendants {
		var res clSearch
		if e := clGet("/search/", map[string]string{
			"type": "r", "q": fmt.Sprintf(`caseName:("%s")`, d),
			"filed_after": dsince, "order_by": "dateFiled desc",
		}, &res); e != nil {
			fmt.Fprintln(os.Stderr, "intel discover defendant", d, ":", e)
			continue
		}
		for i, h := range res.Results {
			if i >= 20 {
				break
			}
			queue(h, 3)
		}
		time.Sleep(2 * time.Second)
	}

	promoted, perr := intelPromote(db)
	if perr != nil {
		return added, perr
	}
	if promoted > 0 {
		fmt.Printf("intel discover: promoted %d candidates to the tracker\n", promoted)
	}
	return added, nil
}

// intelPromote publishes high-confidence candidates into ai_lawsuits so a newly
// filed case appears on the tracker without waiting on a human.
//
// Only the verified fields carry over: case name, court, docket number, filing
// date, the parties as they appear in the caption, and the CourtListener link.
// Nothing interpretive is generated here. executive_summary, why_it_matters,
// and claims stay empty until a person writes them, because a machine-written
// characterisation of somebody's lawsuit is exactly the kind of confident
// invention this platform exists not to publish. The page renders what it has
// and says less where it has nothing.
//
// The timeline fills itself: intelRefresh reads the docket for every active
// case on the next run, so a promoted case gains its history automatically.
func intelPromote(db *sql.DB) (int, error) {
	rows, err := db.Query(`SELECT id, case_name, court, COALESCE(docket,''),
			COALESCE(filed_date::text,''), url, COALESCE(snippet,'')
		FROM ai_lawsuit_candidates
		WHERE status='new' AND score >= 5
		ORDER BY filed_date DESC NULLS LAST LIMIT 20`)
	if err != nil {
		return 0, err
	}
	type cand struct {
		id                                       int64
		name, court, docket, filed, url, snippet string
	}
	var cs []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.name, &c.court, &c.docket, &c.filed, &c.url, &c.snippet) == nil {
			cs = append(cs, c)
		}
	}
	rows.Close()

	nonSlug := regexp.MustCompile(`[^a-z0-9]+`)
	promoted := 0
	for _, c := range cs {
		parties := strings.SplitN(c.name, " v. ", 2)
		plaintiff := strings.TrimSpace(parties[0])
		defendant := ""
		if len(parties) == 2 {
			defendant = strings.TrimSpace(parties[1])
		}
		short := func(s string) string {
			f := strings.Fields(nonSlug.ReplaceAllString(strings.ToLower(s), " "))
			if len(f) > 2 {
				f = f[:2]
			}
			return strings.Join(f, "-")
		}
		slug := strings.Trim(short(plaintiff)+"-v-"+short(defendant), "-")
		if slug == "" || slug == "-v-" {
			continue
		}
		// Slug collisions are real: two Concord actions against Anthropic
		// already share a caption. Suffix rather than overwrite.
		base := slug
		for n := 2; n < 10; n++ {
			var taken bool
			db.QueryRow(`SELECT EXISTS(SELECT 1 FROM ai_lawsuits WHERE slug=$1)`, slug).Scan(&taken)
			if !taken {
				break
			}
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		var filed any
		if c.filed != "" {
			filed = c.filed
		}
		var summary any
		if c.snippet != "" {
			summary = c.snippet
		}
		if _, err := db.Exec(`INSERT INTO ai_lawsuits
			(slug, case_name, court, docket, filed_date, plaintiffs, defendants, category,
			 status, status_badge, courtlistener_url, source_url, is_active, display_order,
			 verified_date, summary)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'copyright',
			 'Filed; docket monitoring active, no development recorded yet by this tracker',
			 'Active Litigation',$8,$8,true,
			 (SELECT COALESCE(MAX(display_order),0)+10 FROM ai_lawsuits),
			 current_date,$9)
			ON CONFLICT (slug) DO NOTHING`,
			slug, c.name, c.court, c.docket, filed, plaintiff, defendant, c.url, summary); err != nil {
			fmt.Fprintln(os.Stderr, "intel promote", slug, ":", err)
			continue
		}
		db.Exec(`UPDATE ai_lawsuit_candidates SET status='promoted' WHERE id=$1`, c.id)
		promoted++
	}
	return promoted, nil
}

// intelAIWatch queues new Hugging Face models and AI vendor news as intel
// candidates, reusing the pipeline's aiTerm subject filter for the feeds.
func intelAIWatch(db *sql.DB) (added int, err error) {
	req, _ := http.NewRequest("GET", "https://huggingface.co/api/models?sort=createdAt&direction=-1&limit=25", nil)
	req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (srjconsultingservices.com)")
	if resp, herr := http.DefaultClient.Do(req); herr == nil {
		var models []struct {
			ID          string `json:"id"`
			ModelID     string `json:"modelId"`
			Downloads   int    `json:"downloads"`
			PipelineTag string `json:"pipeline_tag"`
		}
		if derr := json.NewDecoder(resp.Body).Decode(&models); derr == nil {
			for _, m := range models {
				mid := m.ModelID
				if mid == "" {
					mid = m.ID
				}
				if mid == "" || m.Downloads < 50 {
					continue
				}
				name, vendor := mid, ""
				if i := strings.Index(mid, "/"); i > 0 {
					vendor, name = mid[:i], mid[i+1:]
				}
				r, ierr := db.Exec(`INSERT INTO ai_intel_candidates (kind, name, vendor, url, summary, source, source_id)
					VALUES ('model', $1, NULLIF($2,''), $3, $4, 'huggingface', $5)
					ON CONFLICT (source_id) DO NOTHING`,
					name, vendor, "https://huggingface.co/"+mid,
					fmt.Sprintf("pipeline: %s, downloads: %d", m.PipelineTag, m.Downloads),
					"hf-"+mid)
				if ierr != nil {
					continue
				}
				if n, _ := r.RowsAffected(); n > 0 {
					added++
				}
			}
		}
		resp.Body.Close()
	} else {
		fmt.Fprintln(os.Stderr, "intel ai_watch huggingface:", herr)
	}
	// Phase 1 of the watch-everything directive (Stephen, July 31 2026,
	// changelog seq 138): every free source with a working RSS/Atom feed.
	// Feedless sources (BAAI, CAC, IMDA, Naver, 36Kr, QbitAI, Shanghai AI
	// Lab) are phase 2, scraper-based. Paid APIs (Dealroom, Tracxn) are
	// excluded on Stephen's instruction. A dead feed logs and is skipped, so
	// one source rotting never blocks the rest.
	feeds := []struct{ vendor, url string }{
		{"OpenAI", "https://openai.com/news/rss.xml"},
		{"Google DeepMind", "https://deepmind.google/blog/rss.xml"},
		{"Hugging Face", "https://huggingface.co/blog/feed.xml"},
		// Research and labs. Probed July 31: Aleph Alpha, KAIST, OECD.AI, and
		// Canada ISED expose no working feed (HTML pages or connection resets)
		// and move to the phase-2 scraper list.
		{"Mistral AI", "https://mistral.ai/rss.xml"},
		{"Stability AI (coverage)", "https://news.google.com/rss/search?q=%22Stability+AI%22&hl=en-US&gl=US&ceid=US:en"},
		{"AI21 Labs (coverage)", "https://news.google.com/rss/search?q=%22AI21+Labs%22&hl=en-US&gl=US&ceid=US:en"},
		{"The Alan Turing Institute", "https://www.turing.ac.uk/rss.xml"},
		{"INRIA", "https://inria.fr/en/rss.xml"},
		{"RIKEN AIP", "https://www.riken.jp/en/feed/"},
		{"MBZUAI", "https://mbzuai.ac.ae/news/feed/"},
		{"AI Singapore", "https://aisingapore.org/feed/"},
		// Policy, regulation, standards
		{"European Commission AI", "https://digital-strategy.ec.europa.eu/en/rss.xml"},
		{"UK DSIT", "https://www.gov.uk/government/organisations/department-for-science-innovation-and-technology.atom"},
		{"UK AI Safety Institute (coverage)", "https://news.google.com/rss/search?q=%22AI+Safety+Institute%22+UK&hl=en-US&gl=US&ceid=US:en"},
		{"UNESCO (coverage)", "https://news.google.com/rss/search?q=UNESCO+%22artificial+intelligence%22&hl=en-US&gl=US&ceid=US:en"},
		{"CIFAR", "https://cifar.ca/feed/"},
		// Media and industry
		{"The Register", "https://www.theregister.com/software/ai_ml/headlines.atom"},
		{"Rest of World", "https://restofworld.org/feed/latest/"},
		{"Synced", "https://syncedreview.com/feed/"},
		{"Sifted", "https://sifted.eu/feed"},
		{"Tech in Asia", "https://www.techinasia.com/rss"},
		{"KrASIA", "https://kr-asia.com/feed"},
		{"The Yuan (coverage)", "https://news.google.com/rss/search?q=%22The+Yuan%22+AI+site:the-yuan.com+OR+%22the-yuan.com%22&hl=en-US&gl=US&ceid=US:en"},
		{"Computing UK", "https://www.computing.co.uk/feeds/rss"},
		{"Heise", "https://www.heise.de/rss/heise-atom.xml"},
		{"L'Usine Digitale", "https://www.usine-digitale.fr/rss"},
		// Phase 2 of the watch-everything directive: sources with no working
		// feed of their own, watched through Google News RSS coverage
		// queries. This trades first-party immediacy for zero scraper
		// maintenance and free English-language handling of the
		// Chinese-language set; a first-party scraper can replace any of
		// these later without schema changes.
		{"BAAI (coverage)", "https://news.google.com/rss/search?q=%22Beijing+Academy+of+Artificial+Intelligence%22&hl=en-US&gl=US&ceid=US:en"},
		{"Shanghai AI Lab (coverage)", "https://news.google.com/rss/search?q=%22Shanghai+AI+Laboratory%22&hl=en-US&gl=US&ceid=US:en"},
		{"China CAC (coverage)", "https://news.google.com/rss/search?q=%22Cyberspace+Administration+of+China%22+AI&hl=en-US&gl=US&ceid=US:en"},
		{"Singapore IMDA (coverage)", "https://news.google.com/rss/search?q=IMDA+Singapore+AI&hl=en-US&gl=US&ceid=US:en"},
		{"Naver Clova (coverage)", "https://news.google.com/rss/search?q=Naver+HyperCLOVA+OR+%22Naver+AI%22&hl=en-US&gl=US&ceid=US:en"},
		{"36Kr (coverage)", "https://news.google.com/rss/search?q=36Kr+AI&hl=en-US&gl=US&ceid=US:en"},
		{"QbitAI (coverage)", "https://news.google.com/rss/search?q=QbitAI+OR+%22%E9%87%8F%E5%AD%90%E4%BD%8D%22&hl=en-US&gl=US&ceid=US:en"},
		{"Aleph Alpha (coverage)", "https://news.google.com/rss/search?q=%22Aleph+Alpha%22&hl=en-US&gl=US&ceid=US:en"},
		{"KAIST AI (coverage)", "https://news.google.com/rss/search?q=KAIST+AI&hl=en-US&gl=US&ceid=US:en"},
		{"OECD AI (coverage)", "https://news.google.com/rss/search?q=%22OECD%22+AI+policy&hl=en-US&gl=US&ceid=US:en"},
		{"Canada AI policy (coverage)", "https://news.google.com/rss/search?q=Canada+ISED+OR+CIFAR+AI&hl=en-US&gl=US&ceid=US:en"},
	}
	for _, f := range feeds {
		req, _ := http.NewRequest("GET", f.url, nil)
		req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (srjconsultingservices.com)")
		resp, ferr := http.DefaultClient.Do(req)
		if ferr != nil {
			fmt.Fprintln(os.Stderr, "intel ai_watch feed", f.vendor, ":", ferr)
			continue
		}
		var feed struct {
			Items []struct {
				Title string `xml:"title"`
				Link  string `xml:"link"`
			} `xml:"channel>item"`
		}
		// RSS in the wild is full of HTML entities (&mdash;) and sloppy
		// markup that Go's strict XML parser rejects (July 31 run: 7 of the
		// new feeds failed on entities alone). Lenient mode with the HTML
		// entity table rescues those; feeds serving actual HTML pages still
		// fail and need their URLs corrected instead.
		dec := xml.NewDecoder(resp.Body)
		dec.Strict = false
		dec.Entity = xml.HTMLEntity
		derr := dec.Decode(&feed)
		resp.Body.Close()
		if derr != nil {
			fmt.Fprintln(os.Stderr, "intel ai_watch feed", f.vendor, "parse:", derr)
			continue
		}
		for _, it := range feed.Items {
			title, link := strings.TrimSpace(it.Title), strings.TrimSpace(it.Link)
			if title == "" || link == "" || !mentionsAI(title) {
				continue
			}
			r, ierr := db.Exec(`INSERT INTO ai_intel_candidates (kind, name, vendor, url, source, source_id)
				VALUES ('vendor-news', $1, $2, $3, 'rss', $4)
				ON CONFLICT (source_id) DO NOTHING`,
				trunc(title, 300), f.vendor, link, "rss-"+link)
			if ierr != nil {
				continue
			}
			if n, _ := r.RowsAffected(); n > 0 {
				added++
			}
		}
	}
	return added, nil
}

// intelSync runs the four intel jobs, each independent, and logs the run.
func intelSync(db *sql.DB) error {
	ok := true
	var details []string
	checked, updated, err := intelRefresh(db)
	if err != nil {
		ok = false
		details = append(details, "refresh: "+err.Error())
	}
	resolved, err := intelResolve(db)
	if err != nil {
		ok = false
		details = append(details, "resolve: "+err.Error())
	}
	lawAdded, err := intelDiscover(db)
	if err != nil {
		ok = false
		details = append(details, "discover: "+err.Error())
	}
	intelAdded, err := intelAIWatch(db)
	if err != nil {
		ok = false
		details = append(details, "ai_watch: "+err.Error())
	}
	detail := sql.NullString{String: strings.Join(details, "; "), Valid: len(details) > 0}
	db.Exec(`INSERT INTO srj_intel_log (job, ok, dockets_checked, dockets_updated,
		lawsuit_candidates_added, intel_candidates_added, detail)
		VALUES ('daily-sync', $1, $2, $3, $4, $5, $6)`,
		ok, checked, updated, lawAdded, intelAdded, detail)
	fmt.Printf("intel: checked=%d updated=%d resolved=%d lawsuit_candidates=%d intel_candidates=%d ok=%v\n",
		checked, updated, resolved, lawAdded, intelAdded, ok)
	if !ok {
		return fmt.Errorf("intel jobs failed: %s", strings.Join(details, "; "))
	}
	return nil
}

// ---- archive_news: full-text corpus archival + summaries -------------------
//
// Everything the news discovery layer finds is downloaded once and kept:
// article HTML and extracted text go to the PRIVATE R2 bucket (srj-uploads,
// corpus/ prefix) through the site Worker's /api/archive endpoint, and the
// extracted text is also held in pipeline.documents.fulltext for summarization
// and future LLM work. The public site only ever shows own-words summaries.
//
// Environment: ARCHIVE_ENDPOINT (https://srjconsultingservices.com/api/archive),
// ARCHIVE_TOKEN (bearer), ANTHROPIC_API_KEY (summaries; publish_news degrades
// gracefully without it).

var (
	scriptRe = regexp.MustCompile(`(?is)<(script|style|noscript|svg|nav|header|footer|form)[^>]*>.*?</\s*(script|style|noscript|svg|nav|header|footer|form)\s*>`)
	tagRe    = regexp.MustCompile(`(?s)<[^>]*>`)
	wsRe     = regexp.MustCompile(`[ \t\r\f]+`)
	nlRe     = regexp.MustCompile(`\n{3,}`)
)

// htmlToText is a deliberately simple extractor: strip the chrome-bearing
// elements, drop tags, decode the common entities, collapse whitespace. Good
// enough for summarization and corpus search; the raw HTML is archived too,
// so a better extractor can always re-run later.
func htmlToText(h string) string {
	h = scriptRe.ReplaceAllString(h, " ")
	h = strings.ReplaceAll(h, "</p>", "\n\n")
	h = strings.ReplaceAll(h, "<br>", "\n")
	h = strings.ReplaceAll(h, "<br/>", "\n")
	h = tagRe.ReplaceAllString(h, " ")
	for k, v := range map[string]string{"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`, "&#39;": "'", "&rsquo;": "'", "&lsquo;": "'", "&ldquo;": `"`, "&rdquo;": `"`, "&nbsp;": " ", "&mdash;": ",", "&ndash;": "-"} {
		h = strings.ReplaceAll(h, k, v)
	}
	h = wsRe.ReplaceAllString(h, " ")
	lines := strings.Split(h, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	h = strings.Join(lines, "\n")
	return strings.TrimSpace(nlRe.ReplaceAllString(h, "\n\n"))
}

// archivePut writes one object through the Worker's bearer-gated endpoint.
func archivePut(endpoint, token, key, contentType string, body []byte) error {
	req, err := http.NewRequest("PUT", endpoint+"?key="+key, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("archive PUT %s: %d %s", key, resp.StatusCode, b)
	}
	return nil
}

// archiveNews downloads article bodies for recent gdelt documents that have
// not been archived yet, stores HTML + text in R2, and keeps the text in
// pipeline.documents.fulltext. Failures mark fetch_failed_at so a dead URL is
// tried once, not daily forever.
func archiveNews(db *sql.DB) error {
	endpoint, token := os.Getenv("ARCHIVE_ENDPOINT"), os.Getenv("ARCHIVE_TOKEN")
	if endpoint == "" || token == "" {
		return fmt.Errorf("ARCHIVE_ENDPOINT and ARCHIVE_TOKEN must be set")
	}
	rows, err := db.Query(`SELECT d.id, d.external_id, d.url
		FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
		WHERE s.key='gdelt' AND d.r2_key IS NULL AND d.fetch_failed_at IS NULL
		AND d.fetched_at > now() - interval '10 days'
		ORDER BY d.id DESC LIMIT 80`)
	if err != nil {
		return err
	}
	type doc struct {
		id      int64
		ext, ur string
	}
	var todo []doc
	for rows.Next() {
		var d doc
		if rows.Scan(&d.id, &d.ext, &d.ur) == nil {
			todo = append(todo, d)
		}
	}
	rows.Close()
	client := &http.Client{Timeout: 25 * time.Second}
	archived, failed := 0, 0
	for _, d := range todo {
		req, rerr := http.NewRequest("GET", d.ur, nil)
		if rerr != nil {
			db.Exec(`UPDATE pipeline.documents SET fetch_failed_at=now() WHERE id=$1`, d.id)
			failed++
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; SRJ-archive/1.0; +https://srjconsultingservices.com)")
		resp, gerr := client.Do(req)
		if gerr != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			db.Exec(`UPDATE pipeline.documents SET fetch_failed_at=now() WHERE id=$1`, d.id)
			failed++
			time.Sleep(500 * time.Millisecond)
			continue
		}
		htmlB, rderr := io.ReadAll(io.LimitReader(resp.Body, 3*1024*1024))
		resp.Body.Close()
		if rderr != nil || len(htmlB) == 0 {
			db.Exec(`UPDATE pipeline.documents SET fetch_failed_at=now() WHERE id=$1`, d.id)
			failed++
			continue
		}
		text := htmlToText(string(htmlB))
		if len(text) > 200*1024 {
			text = text[:200*1024]
		}
		keyBase := "corpus/news/" + d.ext
		if err := archivePut(endpoint, token, keyBase+".html", "text/html; charset=utf-8", htmlB); err != nil {
			// A 403 here is the edge WAF challenging the article body itself
			// (seen live July 31), which will fail identically every day; mark
			// the doc failed so it is tried once, not forever. Transient
			// errors leave the doc eligible for the next run.
			if strings.Contains(err.Error(), ": 403 ") {
				db.Exec(`UPDATE pipeline.documents SET fetch_failed_at=now() WHERE id=$1`, d.id)
			}
			fmt.Fprintln(os.Stderr, "archive_news:", err)
			failed++
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err := archivePut(endpoint, token, keyBase+".txt", "text/plain; charset=utf-8", []byte(text)); err != nil {
			fmt.Fprintln(os.Stderr, "archive_news:", err)
		}
		// Postgres rejects invalid UTF-8 (seen live July 31: "invalid byte
		// sequence 0xbb" from mis-declared article encodings), so sanitize
		// before the write; the raw bytes are already preserved in R2.
		if _, err := db.Exec(`UPDATE pipeline.documents SET r2_key=$1, fulltext=$2 WHERE id=$3`,
			keyBase+".html", strings.ToValidUTF8(text, "\uFFFD"), d.id); err != nil {
			fmt.Fprintln(os.Stderr, "archive_news db:", err)
			continue
		}
		archived++
		time.Sleep(400 * time.Millisecond)
	}
	fmt.Printf("archive_news: archived=%d failed=%d of %d\n", archived, failed, len(todo))
	return nil
}

// anthropicSummarize writes a two-paragraph, own-words news summary. House
// style: plain English, commas rather than dashes, no reproduced passages.
func anthropicSummarize(headline, text string) (string, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "", fmt.Errorf("ANTHROPIC_API_KEY not set")
	}
	if len(text) < 400 {
		return "", fmt.Errorf("article text too short to summarize")
	}
	prompt := "Summarize this news article in two short paragraphs, 120 to 180 words total, entirely in your own words. " +
		"State what happened, who is involved, the key numbers, and what happens next if the article says. " +
		"Plain English. Use commas rather than dashes. Do not quote more than a few words. Do not repeat the headline. " +
		"Do not add opinions or information that is not in the article. Output only the summary paragraphs.\n\n" +
		"Headline: " + headline + "\n\nArticle text:\n" + text
	body, _ := json.Marshal(map[string]any{
		"model":      "claude-haiku-4-5",
		"max_tokens": 400,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("anthropic %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, c := range out.Content {
		sb.WriteString(c.Text)
	}
	s := strings.TrimSpace(sb.String())
	if s == "" {
		return "", fmt.Errorf("empty summary")
	}
	return s, nil
}

// ---- twoai: theworldofai.org, SQL -> twoai-content ------------------------
//
// The consumer property renders the same database. twoai_pages is a render
// cache: path is the exact repo path inside twoai-content, data is everything
// the Astro template needs. Dropping the table and re-running the pipeline
// must always reproduce it. twoaiBuild fills it from existing tables (bills,
// glossary, lawsuits); twoaiPublish exports rows whose git blob sha differs,
// so a quiet day is one tree call and zero commits, same as sync_content.
var twoaiStates = map[string]string{
	"AL": "Alabama", "AK": "Alaska", "AZ": "Arizona", "AR": "Arkansas", "CA": "California",
	"CO": "Colorado", "CT": "Connecticut", "DE": "Delaware", "FL": "Florida", "GA": "Georgia",
	"HI": "Hawaii", "ID": "Idaho", "IL": "Illinois", "IN": "Indiana", "IA": "Iowa",
	"KS": "Kansas", "KY": "Kentucky", "LA": "Louisiana", "ME": "Maine", "MD": "Maryland",
	"MA": "Massachusetts", "MI": "Michigan", "MN": "Minnesota", "MS": "Mississippi", "MO": "Missouri",
	"MT": "Montana", "NE": "Nebraska", "NV": "Nevada", "NH": "New Hampshire", "NJ": "New Jersey",
	"NM": "New Mexico", "NY": "New York", "NC": "North Carolina", "ND": "North Dakota", "OH": "Ohio",
	"OK": "Oklahoma", "OR": "Oregon", "PA": "Pennsylvania", "RI": "Rhode Island", "SC": "South Carolina",
	"SD": "South Dakota", "TN": "Tennessee", "TX": "Texas", "UT": "Utah", "VT": "Vermont",
	"VA": "Virginia", "WA": "Washington", "WV": "West Virginia", "WI": "Wisconsin", "WY": "Wyoming",
	"DC": "District of Columbia", "PR": "Puerto Rico", "US": "United States Congress",
}

func twoaiSlug(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

func twoaiBuild(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_pages (
		path text PRIMARY KEY, kind text NOT NULL, data jsonb NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	today := time.Now().UTC().Format("2006-01-02")

	// ---- F1: AI laws by state, from the LegiScan corpus. One row per bill
	// (latest change wins), grouped by the "ST NUM: Title" prefix.
	rows, err := db.Query(`SELECT DISTINCT ON (external_id) external_id, title, url,
			COALESCE(to_char(published_at,'YYYY-MM-DD'),'') 
		FROM pipeline.documents WHERE source_id = 2
		ORDER BY external_id, id DESC`)
	if err != nil {
		return err
	}
	type bill struct {
		Number string `json:"number"`
		Title  string `json:"title"`
		URL    string `json:"url"`
		Date   string `json:"date"`
	}
	byState := map[string][]bill{}
	for rows.Next() {
		var ext, title, url, date string
		if err := rows.Scan(&ext, &title, &url, &date); err != nil {
			rows.Close()
			return err
		}
		parts := strings.SplitN(title, ":", 2)
		head := strings.Fields(parts[0])
		if len(head) < 2 {
			continue
		}
		code := strings.ToUpper(head[0])
		if _, ok := twoaiStates[code]; !ok {
			continue
		}
		b := bill{Number: strings.Join(head[1:], " "), URL: url, Date: date}
		if len(parts) == 2 {
			b.Title = strings.TrimSpace(parts[1])
		}
		byState[code] = append(byState[code], b)
	}
	rows.Close()

	type stateIdx struct {
		Code  string `json:"code"`
		Name  string `json:"name"`
		Slug  string `json:"slug"`
		Count int    `json:"count"`
	}
	var index []stateIdx
	total := 0
	upsert := func(path, kind string, v any) error {
		j, _ := json.Marshal(v)
		_, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data) VALUES ($1,$2,$3::jsonb)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data, updated_at=now()`,
			path, kind, string(j))
		return err
	}
	for code, name := range twoaiStates {
		bills := byState[code]
		if bills == nil {
			bills = []bill{}
		}
		sort.Slice(bills, func(i, j int) bool { return bills[i].Date > bills[j].Date })
		total += len(bills)
		slug := twoaiSlug(name)
		index = append(index, stateIdx{code, name, slug, len(bills)})
		if err := upsert("laws/"+slug+".json", "state-law", map[string]any{
			"code": code, "name": name, "slug": slug, "count": len(bills),
			"bills": bills, "generated": today,
		}); err != nil {
			return err
		}
	}
	sort.Slice(index, func(i, j int) bool { return index[i].Name < index[j].Name })
	if err := upsert("laws/index.json", "hub", map[string]any{
		"states": index, "total": total, "generated": today,
	}); err != nil {
		return err
	}

	// ---- F2: glossary, straight from the library already in site_content.
	var glossary string
	if err := db.QueryRow(`SELECT data::text FROM site_content WHERE path='resources/glossary.json'`).Scan(&glossary); err == nil && glossary != "" {
		var g map[string]any
		if json.Unmarshal([]byte(glossary), &g) == nil {
			g["generated"] = today
			if err := upsert("glossary/glossary.json", "glossary", g); err != nil {
				return err
			}
		}
	}

	// ---- F3: living lawsuit tracker from ai_lawsuits.
	lr, err := db.Query(`SELECT COALESCE(slug,''), case_name, court, COALESCE(docket,''),
			COALESCE(to_char(filed_date,'YYYY-MM-DD'),''), plaintiffs, defendants, category,
			status, COALESCE(status_badge,''), COALESCE(latest_development,''),
			COALESCE(to_char(latest_development_date,'YYYY-MM-DD'),''),
			COALESCE(executive_summary,''), COALESCE(why_it_matters,''), COALESCE(summary,''),
			COALESCE(claims,'[]'::jsonb)::text, COALESCE(timeline,'[]'::jsonb)::text,
			COALESCE(courtlistener_url,''), COALESCE(source_url,''), COALESCE(judge,'')
		FROM ai_lawsuits WHERE is_active IS NOT FALSE AND slug IS NOT NULL
		ORDER BY display_order, case_name`)
	if err != nil {
		return err
	}
	var cases []map[string]any
	for lr.Next() {
		var slug, name, court, docket, filed, pl, de, cat, status, badge, dev, devDate,
			exec, why, sum, claims, timeline, clURL, srcURL, judge string
		if err := lr.Scan(&slug, &name, &court, &docket, &filed, &pl, &de, &cat, &status, &badge,
			&dev, &devDate, &exec, &why, &sum, &claims, &timeline, &clURL, &srcURL, &judge); err != nil {
			lr.Close()
			return err
		}
		var cj, tj any
		json.Unmarshal([]byte(claims), &cj)
		json.Unmarshal([]byte(timeline), &tj)
		cases = append(cases, map[string]any{
			"slug": slug, "case_name": name, "court": court, "docket": docket,
			"filed_date": filed, "plaintiffs": pl, "defendants": de, "category": cat,
			"status": status, "status_badge": badge, "latest_development": dev,
			"latest_development_date": devDate, "executive_summary": exec,
			"why_it_matters": why, "summary": sum, "claims": cj, "timeline": tj,
			"courtlistener_url": clURL, "source_url": srcURL, "judge": judge,
		})
	}
	lr.Close()
	if err := upsert("lawsuits/lawsuits.json", "lawsuits", map[string]any{
		"cases": cases, "count": len(cases), "generated": today,
	}); err != nil {
		return err
	}

	// ---- Static pages (about, contact, privacy, terms, disclaimer, disclosure).
	// Copy lives in site_content under twoai/static/*.json so nothing is typed
	// into a template; this stage only renders it into the twoai-content repo.
	sr, err := db.Query(`SELECT path, data::text FROM site_content
		WHERE path LIKE 'twoai/static/%' ORDER BY path`)
	if err != nil {
		return err
	}
	statics := 0
	for sr.Next() {
		var sp, sd string
		if err := sr.Scan(&sp, &sd); err != nil {
			sr.Close()
			return err
		}
		var doc map[string]any
		if json.Unmarshal([]byte(sd), &doc) != nil {
			continue
		}
		slug, _ := doc["slug"].(string)
		if slug == "" {
			slug = strings.TrimSuffix(sp[strings.LastIndex(sp, "/")+1:], ".json")
		}
		doc["generated"] = today
		if err := upsert("static/"+slug+".json", "static", doc); err != nil {
			sr.Close()
			return err
		}
		statics++
	}
	sr.Close()

	// ---- F4 tools directory. Catalog and deep profiles both already live in
	// site_content; this renders a hub, one page per category, and one page per
	// profiled tool. Tools with only a catalog row get a listing, not a page:
	// a page with nothing on it but a name and a link is thin by definition.
	tools, cats, profiles := twoaiToolData(db)
	toolPages := 0
	if len(tools) > 0 {
		byCat := map[string][]map[string]any{}
		for _, t := range tools {
			cn, _ := t["category"].(string)
			byCat[cn] = append(byCat[cn], t)
		}
		catIdx := []map[string]any{}
		for _, c := range cats {
			name, _ := c["name"].(string)
			cslug, _ := c["slug"].(string)
			list := byCat[name]
			if len(list) == 0 {
				continue
			}
			sort.Slice(list, func(i, j int) bool {
				a, _ := list[i]["name"].(string)
				b, _ := list[j]["name"].(string)
				return strings.ToLower(a) < strings.ToLower(b)
			})
			if err := upsert("tools/cat-"+cslug+".json", "tool-category", map[string]any{
				"name": name, "slug": cslug, "generated": today, "tools": list,
			}); err != nil {
				return err
			}
			catIdx = append(catIdx, map[string]any{"name": name, "slug": cslug, "count": len(list)})
			toolPages++
		}
		// Deep profiles, joined to the catalog row for the vendor link.
		byName := map[string]map[string]any{}
		for _, t := range tools {
			n, _ := t["name"].(string)
			byName[strings.ToLower(n)] = t
		}
		profiled := []map[string]any{}
		for slug, p := range profiles {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			cn, _ := pm["catalog_name"].(string)
			if cn == "" {
				cn, _ = pm["name"].(string)
			}
			if row := byName[strings.ToLower(cn)]; row != nil {
				pm["url"] = row["url"]
				pm["category"] = row["category"]
			}
			pm["slug"] = slug
			pm["generated"] = today
			if err := upsert("tools/"+slug+".json", "tool", pm); err != nil {
				return err
			}
			nm, _ := pm["name"].(string)
			tl, _ := pm["tagline"].(string)
			profiled = append(profiled, map[string]any{"slug": slug, "name": nm, "tagline": tl, "category": pm["category"]})
			toolPages++
		}
		sort.Slice(profiled, func(i, j int) bool {
			a, _ := profiled[i]["name"].(string)
			b, _ := profiled[j]["name"].(string)
			return strings.ToLower(a) < strings.ToLower(b)
		})
		sort.Slice(catIdx, func(i, j int) bool {
			a, _ := catIdx[i]["name"].(string)
			b, _ := catIdx[j]["name"].(string)
			return a < b
		})
		if err := upsert("tools/index.json", "tool-hub", map[string]any{
			"generated": today, "total": len(tools), "categories": catIdx,
			"profiled": profiled, "tools": tools,
		}); err != nil {
			return err
		}
		toolPages++
	}

	weeks, err := twoaiWeeks(db, today, upsert)
	if err != nil {
		return err
	}

	fmt.Printf("twoai_build: states=%d bills=%d glossary=%v cases=%d statics=%d tools=%d weeks=%d ok=true\n",
		len(index), total, glossary != "", len(cases), statics, toolPages, weeks)
	return nil
}

// twoaiToolData reads the AI tool catalog and the deep tool profiles out of
// site_content. Both are maintained for the SRJ site and are read only here.
// Any absence is tolerated: a missing row means the tools factory emits
// nothing rather than half a directory.
func twoaiToolData(db *sql.DB) (tools []map[string]any, cats []map[string]any, profiles map[string]any) {
	profiles = map[string]any{}
	var catalog string
	if err := db.QueryRow(`SELECT data::text FROM site_content WHERE path='resources/tools.json'`).Scan(&catalog); err == nil {
		var c struct {
			Tools      []map[string]any `json:"tools"`
			Categories []map[string]any `json:"categories"`
		}
		if json.Unmarshal([]byte(catalog), &c) == nil {
			tools, cats = c.Tools, c.Categories
		}
	}
	var prof string
	if err := db.QueryRow(`SELECT data::text FROM site_content WHERE path='resources/tool-profiles.json'`).Scan(&prof); err == nil {
		var p struct {
			Profiles map[string]any `json:"profiles"`
			AsOf     string         `json:"as_of"`
		}
		if json.Unmarshal([]byte(prof), &p) == nil && p.Profiles != nil {
			profiles = p.Profiles
		}
	}
	return tools, cats, profiles
}

// twoaiThemes classifies a bill or docket title into the recurring subjects of
// AI legislation. The point is not taxonomy for its own sake: a reader looking
// at forty bill rows cannot see a pattern, and the pattern is the story. What
// the categories are, and the keywords that mark them, are the same ones the
// bills use about themselves, so a bill lands in a bucket because of its own
// words rather than an editorial judgement about it.
//
// A bill can match more than one theme, and does not have to match any. An
// unmatched bill still appears in the table; it just does not contribute to
// the thematic summary, which is better than forcing it into a bucket that
// would misdescribe it.
var twoaiThemeRules = []struct {
	Name, Blurb string
	Re          *regexp.Regexp
}{
	{"Deepfakes and likeness", "Synthetic images, voices, and video of real people, and who owns a likeness once a machine can copy it.",
		regexp.MustCompile(`(?i)deepfake|synthetic media|digital replica|likeness|voice clon|impersonat|nonconsensual|sexually explicit`)},
	{"Elections", "AI-generated political content, disclosure on campaign material, and interference with voting.",
		regexp.MustCompile(`(?i)election|campaign|ballot|candidate|political advertis`)},
	{"Children and minors", "Companion chatbots, age verification, school use, and protections for people under eighteen.",
		regexp.MustCompile(`(?i)\bminor|child|kids|student|school|age verification|companion chatbot`)},
	{"Health care", "Clinical decision support, utilization review, mental health chatbots, and AI in diagnosis or coverage decisions.",
		regexp.MustCompile(`(?i)health|medical|clinical|patient|mental health|insur(er|ance) (review|decision)|prior authorization|therapist`)},
	{"Employment and hiring", "Automated screening of applicants, workplace surveillance, and decisions about pay or promotion.",
		regexp.MustCompile(`(?i)employ|hiring|applicant|worker|workplace|labor|resume|personnel decision`)},
	{"Government use", "How agencies themselves buy, deploy, and account for AI, including inventories and procurement rules.",
		regexp.MustCompile(`(?i)state agenc|government use|procurement|public sector|inventory of|task force|advisory (council|committee)|study committee`)},
	{"Transparency and disclosure", "Labeling AI-generated output, telling people when they are talking to a machine, and impact assessments.",
		regexp.MustCompile(`(?i)disclos|transparen|label|watermark|notice to consumers|impact assessment|audit`)},
	{"Consumer protection and discrimination", "Algorithmic decisions that affect credit, housing, insurance pricing, or that produce unlawful bias.",
		regexp.MustCompile(`(?i)discriminat|algorithmic (pricing|bias)|consumer protection|credit|housing|unfair|deceptive`)},
	{"Privacy and data", "Biometrics, training data, and what may be collected or fed into a model.",
		regexp.MustCompile(`(?i)privacy|biometric|personal (data|information)|training data|facial recognition|surveillance`)},
	{"Criminal law", "New offenses, penalties, and evidence rules for conduct carried out with AI.",
		regexp.MustCompile(`(?i)criminal|penalt|offense|felony|misdemeanor|fraud|prosecut`)},
	{"Safety and frontier models", "Obligations aimed at the most capable systems, including testing, incident reporting, and catastrophic risk.",
		regexp.MustCompile(`(?i)frontier|catastrophic|safety (standard|protocol)|critical infrastructure|foundation model|general.purpose`)},
	{"Infrastructure and energy", "Data centers, the power they draw, and the local cost of hosting them.",
		regexp.MustCompile(`(?i)data cent(er|re)|energy|electric|grid|water use|utility`)},
	{"Workforce and education", "Training people to use AI, apprenticeships, curriculum, and public literacy programs.",
		regexp.MustCompile(`(?i)workforce|apprentice|curriculum|literacy|training program|community college|scholarship`)},
	{"Intellectual property", "Copyright, authorship, and the use of protected work to build models.",
		regexp.MustCompile(`(?i)copyright|intellectual property|authorship|royalt|licens(e|ing) of works`)},
}

func twoaiClassify(text string) []string {
	var out []string
	for _, r := range twoaiThemeRules {
		if r.Re.MatchString(text) {
			out = append(out, r.Name)
		}
	}
	return out
}

// twoaiWeeks builds the weekly digest, one page per ISO week, from our own
// verified tables rather than from a news feed.
//
// This is deliberately NOT built on ai_intel_candidates. That table is
// headline-only: summary is null on every row, every URL is an opaque
// news.google.com redirect rather than a publisher link, and the vendor field
// is frequently wrong because coverage queries attribute a story to whichever
// search found it. A page assembled from that would be a wall of other
// people's headlines, which is both worthless to a reader and the exact shape
// of content ad networks reject. When the intel pipeline resolves real
// publisher URLs and writes summaries, a daily news factory becomes possible;
// until then the honest weekly is legislative movement, federal action, and
// docket movement, all of which we verify ourselves.
//
// Each item carries the explanatory text we already hold and can stand behind:
// LegiScan's own bill description, the Federal Register's abstract, which is a
// government-written summary in the public domain, and the why_it_matters
// paragraph from ai_lawsuits. Nothing on the page is written about an item we
// have not read. On top of that the stage computes a thematic breakdown, so a
// reader sees what the week was ABOUT rather than thirty unrelated rows.
//
// Quiet weeks are published as quiet, not padded. A week with two bills and
// nothing else says so, because a tracker that always claims a busy week is a
// tracker nobody can use to tell busy from quiet.
func twoaiWeeks(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	type weekItem struct {
		State  string   `json:"state,omitempty"`
		Number string   `json:"number,omitempty"`
		Title  string   `json:"title"`
		URL    string   `json:"url"`
		Date   string   `json:"date"`
		Note   string   `json:"note,omitempty"`
		Slug   string   `json:"slug,omitempty"`
		Detail string   `json:"detail,omitempty"`
		Agency string   `json:"agency,omitempty"`
		Themes []string `json:"themes,omitempty"`
	}

	// Monday of the current ISO week, in UTC, then walk back eight weeks.
	now := time.Now().UTC()
	offset := (int(now.Weekday()) + 6) % 7 // Monday = 0
	thisMonday := time.Date(now.Year(), now.Month(), now.Day()-offset, 0, 0, 0, 0, time.UTC)

	type wk struct {
		Slug, Label, Start, End string
		Bills, Federal, Courts  []weekItem
	}
	var built []wk

	for back := 0; back < 8; back++ {
		start := thisMonday.AddDate(0, 0, -7*back)
		end := start.AddDate(0, 0, 7)
		iso, isoWeek := start.ISOWeek()
		w := wk{
			Slug:  fmt.Sprintf("%d-w%02d", iso, isoWeek),
			Label: fmt.Sprintf("Week %d, %d", isoWeek, iso),
			Start: start.Format("2006-01-02"),
			End:   end.AddDate(0, 0, -1).Format("2006-01-02"),
		}
		w.Bills = []weekItem{}
		w.Federal = []weekItem{}
		w.Courts = []weekItem{}

		br, err := db.Query(`SELECT DISTINCT ON (d.external_id) d.title, d.url,
				COALESCE(to_char(d.published_at,'YYYY-MM-DD'),''),
				COALESCE(d.raw->'bill'->>'description','')
			FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
			WHERE s.key='legiscan' AND d.published_at >= $1 AND d.published_at < $2
			ORDER BY d.external_id, d.id DESC`, start, end)
		if err != nil {
			return 0, err
		}
		for br.Next() {
			var title, url, date, descr string
			if br.Scan(&title, &url, &date, &descr) != nil {
				continue
			}
			parts := strings.SplitN(title, ":", 2)
			head := strings.Fields(parts[0])
			if len(head) < 2 {
				continue
			}
			code := strings.ToUpper(head[0])
			name, ok := twoaiStates[code]
			if !ok {
				continue
			}
			item := weekItem{State: name, Number: strings.Join(head[1:], " "), URL: url, Date: date}
			if len(parts) == 2 {
				item.Title = strings.TrimSpace(parts[1])
			}
			// LegiScan's description repeats the title on most bills. Carry it
			// only when it actually adds something, so the page does not print
			// the same sentence twice under a heading that promises more.
			if d := strings.TrimSpace(descr); d != "" && !strings.EqualFold(d, item.Title) {
				item.Detail = d
			}
			item.Themes = twoaiClassify(item.Title + " " + item.Detail)
			item.Slug = twoaiSlug(name)
			w.Bills = append(w.Bills, item)
		}
		br.Close()

		// The Federal Register corpus predates the mentionsAI filter, so it
		// still holds pre-filter rows: a Caribbean fishery council meeting is
		// in there because its full text says "artificial intelligence" once.
		// Filter again on read, or the weekly reports fishery meetings as AI
		// policy. The abstract is a government-written summary and public
		// domain, so it can be shown in full.
		fr, err := db.Query(`SELECT d.title, d.url, COALESCE(to_char(d.published_at,'YYYY-MM-DD'),''),
				COALESCE(d.raw->>'type',''), COALESCE(d.raw->>'abstract',''),
				COALESCE(d.raw->'agencies'->0->>'name','')
			FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
			WHERE s.key='federal_register' AND d.published_at >= $1 AND d.published_at < $2
			ORDER BY d.published_at DESC`, start, end)
		if err != nil {
			return 0, err
		}
		for fr.Next() {
			var it weekItem
			if fr.Scan(&it.Title, &it.URL, &it.Date, &it.Note, &it.Detail, &it.Agency) != nil {
				continue
			}
			if it.Title == "" || (!mentionsAI(it.Title) && !mentionsAI(it.Detail)) {
				continue
			}
			it.Themes = twoaiClassify(it.Title + " " + it.Detail)
			w.Federal = append(w.Federal, it)
		}
		fr.Close()

		cr, err := db.Query(`SELECT COALESCE(slug,''), case_name, COALESCE(court,''),
				COALESCE(latest_development,''), to_char(latest_development_date,'YYYY-MM-DD'),
				COALESCE(NULLIF(why_it_matters,''), COALESCE(executive_summary,'')), COALESCE(category,'')
			FROM ai_lawsuits
			WHERE is_active IS NOT FALSE AND latest_development_date >= $1 AND latest_development_date < $2
			ORDER BY latest_development_date DESC`, start, end)
		if err != nil {
			return 0, err
		}
		for cr.Next() {
			var it weekItem
			var court, category string
			if cr.Scan(&it.Slug, &it.Title, &court, &it.Note, &it.Date, &it.Detail, &category) == nil && it.Title != "" {
				it.State = court
				it.Agency = category
				it.URL = "/ai-lawsuits/" + it.Slug + "/"
				w.Courts = append(w.Courts, it)
			}
		}
		cr.Close()

		// An empty archive week is a page with nothing on it. The current week
		// always publishes, because "nothing yet this week" is a real answer;
		// older empty weeks are skipped rather than shipped hollow.
		if back > 0 && len(w.Bills)+len(w.Federal)+len(w.Courts) == 0 {
			continue
		}
		built = append(built, w)
	}

	idx := []map[string]any{}
	themeTotals := map[string]int{}
	for _, w := range built {
		// Thematic and jurisdictional breakdown for this week. Counts come from
		// the items themselves, so the summary sentence on the page can never
		// drift from the table below it.
		themeCount := map[string]int{}
		themeEg := map[string]string{}
		jur := map[string]int{}
		agency := map[string]int{}
		unthemed := 0
		for _, b := range w.Bills {
			jur[b.State]++
			if len(b.Themes) == 0 {
				unthemed++
			}
			for _, t := range b.Themes {
				themeCount[t]++
				themeTotals[t]++
				if themeEg[t] == "" {
					themeEg[t] = b.State + " " + b.Number
				}
			}
		}
		for _, f := range w.Federal {
			if f.Agency != "" {
				agency[f.Agency]++
			}
			for _, t := range f.Themes {
				themeCount[t]++
				themeTotals[t]++
			}
		}
		rank := func(m map[string]int) []map[string]any {
			ks := []string{}
			for k := range m {
				ks = append(ks, k)
			}
			sort.Slice(ks, func(i, j int) bool {
				if m[ks[i]] != m[ks[j]] {
					return m[ks[i]] > m[ks[j]]
				}
				return ks[i] < ks[j]
			})
			out := []map[string]any{}
			for _, k := range ks {
				row := map[string]any{"name": k, "count": m[k]}
				if eg := themeEg[k]; eg != "" {
					row["example"] = eg
				}
				for _, tr := range twoaiThemeRules {
					if tr.Name == k {
						row["blurb"] = tr.Blurb
					}
				}
				out = append(out, row)
			}
			return out
		}
		themes := rank(themeCount)
		if len(themes) > 8 {
			themes = themes[:8]
		}
		jurs := rank(jur)
		if len(jurs) > 8 {
			jurs = jurs[:8]
		}
		analysis := map[string]any{
			"themes": themes, "jurisdictions": jurs, "agencies": rank(agency),
			"jurisdiction_count": len(jur), "unthemed_bills": unthemed,
		}

		if err := upsert("week/"+w.Slug+".json", "week", map[string]any{
			"slug": w.Slug, "label": w.Label, "start": w.Start, "end": w.End,
			"bills": w.Bills, "federal": w.Federal, "courts": w.Courts,
			"analysis": analysis,
			"counts": map[string]int{
				"bills": len(w.Bills), "federal": len(w.Federal), "courts": len(w.Courts),
				"total": len(w.Bills) + len(w.Federal) + len(w.Courts),
			},
			"generated": today,
		}); err != nil {
			return 0, err
		}
		topTheme := ""
		if len(themes) > 0 {
			topTheme, _ = themes[0]["name"].(string)
		}
		idx = append(idx, map[string]any{
			"slug": w.Slug, "label": w.Label, "start": w.Start, "end": w.End,
			"bills": len(w.Bills), "federal": len(w.Federal), "courts": len(w.Courts),
			"total": len(w.Bills) + len(w.Federal) + len(w.Courts),
			"jurisdictions": len(jur), "top_theme": topTheme,
		})
	}
	if len(idx) == 0 {
		return 0, nil
	}
	// Archive-wide totals, so the hub can say what the whole period was about
	// rather than only listing weeks.
	tks := []string{}
	for k := range themeTotals {
		tks = append(tks, k)
	}
	sort.Slice(tks, func(i, j int) bool {
		if themeTotals[tks[i]] != themeTotals[tks[j]] {
			return themeTotals[tks[i]] > themeTotals[tks[j]]
		}
		return tks[i] < tks[j]
	})
	overall := []map[string]any{}
	for _, k := range tks {
		row := map[string]any{"name": k, "count": themeTotals[k]}
		for _, tr := range twoaiThemeRules {
			if tr.Name == k {
				row["blurb"] = tr.Blurb
			}
		}
		overall = append(overall, row)
		if len(overall) >= 10 {
			break
		}
	}
	grand := 0
	for _, w := range idx {
		grand += w["total"].(int)
	}
	if err := upsert("week/index.json", "week-hub", map[string]any{
		"weeks": idx, "latest": idx[0]["slug"], "generated": today,
		"themes": overall, "total_items": grand,
	}); err != nil {
		return 0, err
	}
	return len(built), nil
}

// twoaiPublish exports twoai_pages to the twoai-content repo, sha-compared
// against the tree so unchanged rows cost nothing. Export-only: SQL is the
// origin here, there is nothing to backfill.
func twoaiPublish(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	client := &http.Client{Timeout: 120 * time.Second}
	get := func(url string) ([]byte, int, error) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b, resp.StatusCode, nil
	}
	blobSha := func(b []byte) string {
		h := sha1.New()
		fmt.Fprintf(h, "blob %d", len(b))
		h.Write([]byte{0})
		h.Write(b)
		return hex.EncodeToString(h.Sum(nil))
	}
	repoSha := map[string]string{}
	if tb, code, err := get("https://api.github.com/repos/srjordan6/twoai-content/git/trees/main?recursive=1"); err == nil && code == 200 {
		var tree struct {
			Tree []struct {
				Path string `json:"path"`
				Type string `json:"type"`
				Sha  string `json:"sha"`
			} `json:"tree"`
		}
		if json.Unmarshal(tb, &tree) == nil {
			for _, e := range tree.Tree {
				if e.Type == "blob" {
					repoSha[e.Path] = e.Sha
				}
			}
		}
	}
	rows, err := db.Query(`SELECT path, jsonb_pretty(data) FROM twoai_pages ORDER BY path`)
	if err != nil {
		return err
	}
	defer rows.Close()
	exported, unchanged := 0, 0
	for rows.Next() {
		var path, pretty string
		if err := rows.Scan(&path, &pretty); err != nil {
			return err
		}
		payload := []byte(pretty + "\n")
		if repoSha[path] == blobSha(payload) {
			unchanged++
			continue
		}
		put := map[string]any{
			"message": fmt.Sprintf("twoai: %s from twoai_pages %s", path, time.Now().UTC().Format("2006-01-02")),
			"content": base64.StdEncoding.EncodeToString(payload),
		}
		if sha := repoSha[path]; sha != "" {
			put["sha"] = sha
		}
		pb, _ := json.Marshal(put)
		req, _ := http.NewRequest("PUT", "https://api.github.com/repos/srjordan6/twoai-content/contents/"+path, bytes.NewReader(pb))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		pr, err := client.Do(req)
		if err != nil {
			return err
		}
		prb, _ := io.ReadAll(pr.Body)
		pr.Body.Close()
		if pr.StatusCode != 200 && pr.StatusCode != 201 {
			return fmt.Errorf("github PUT %s %d: %.200s", path, pr.StatusCode, prb)
		}
		exported++
	}
	fmt.Printf("twoai_publish: exported=%d unchanged=%d ok=true\n", exported, unchanged)
	return nil
}

// ---- sync_content: every remaining content file, SQL -> srj-content --------
//
// Completes the July 31 directive for the whole repo: governance, resources,
// migrated pages, books, and roster all live in site_content (path, data
// jsonb) in srj-audit-db, and the repo is a generated artifact. Out of scope:
// .github (CI code, not content), people/{slug}.json (owned by site_people),
// and the five files the publish stages regenerate daily from their own SQL
// tables (news, legislation, leaderboard, lawsuits, intel).
//
// The tree API supplies every path with its git blob sha, so the export
// compares shas instead of downloading content; a quiet day costs one tree
// call and zero commits. The backfill imports any in-scope repo file that has
// no SQL row yet (ON CONFLICT DO NOTHING), which absorbs the pre-directive
// library once and is a no-op forever after; SQL wins from then on.
func syncContent(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS site_content (
		path text PRIMARY KEY, data jsonb NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	outOfScope := func(p string) bool {
		if !strings.HasSuffix(p, ".json") || strings.HasPrefix(p, ".github/") {
			return true
		}
		if strings.HasPrefix(p, "people/") && p != "people/roster.json" {
			return true
		}
		switch p {
		case "news/news.json", "legislation/legislation.json", "leaderboard/leaderboard.json",
			"lawsuits/lawsuits.json", "intel/intel.json":
			return true
		}
		return false
	}

	client := &http.Client{Timeout: 120 * time.Second}
	get := func(url string) ([]byte, int, error) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b, resp.StatusCode, nil
	}
	blobSha := func(b []byte) string {
		h := sha1.New()
		fmt.Fprintf(h, "blob %d", len(b))
		h.Write([]byte{0})
		h.Write(b)
		return hex.EncodeToString(h.Sum(nil))
	}

	tb, code, err := get("https://api.github.com/repos/srjordan6/srj-content/git/trees/main?recursive=1")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("trees API returned %d", code)
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Sha  string `json:"sha"`
			URL  string `json:"url"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(tb, &tree); err != nil {
		return err
	}
	repoSha := map[string]string{}

	imported := 0
	for _, e := range tree.Tree {
		if e.Type != "blob" || outOfScope(e.Path) {
			continue
		}
		repoSha[e.Path] = e.Sha
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM site_content WHERE path=$1)`, e.Path).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		// Blob API handles any size (the contents API caps at 1MB, and
		// migrated-pages.json is 1.7MB).
		bb, bc, err := get(e.URL)
		if err != nil || bc != 200 {
			fmt.Fprintf(os.Stderr, "sync_content: blob %s: status %d err %v\n", e.Path, bc, err)
			continue
		}
		var blob struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(bb, &blob) != nil {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
		if err != nil || !json.Valid(raw) {
			fmt.Fprintf(os.Stderr, "sync_content: skip %s: not valid JSON\n", e.Path)
			continue
		}
		if _, err := db.Exec(`INSERT INTO site_content (path, data) VALUES ($1, $2::jsonb)
			ON CONFLICT (path) DO NOTHING`, e.Path, string(raw)); err != nil {
			return err
		}
		imported++
	}

	rows, err := db.Query(`SELECT path, jsonb_pretty(data) FROM site_content ORDER BY path`)
	if err != nil {
		return err
	}
	defer rows.Close()
	exported, unchanged := 0, 0
	for rows.Next() {
		var path, pretty string
		if err := rows.Scan(&path, &pretty); err != nil {
			return err
		}
		payload := []byte(pretty + "\n")
		if repoSha[path] == blobSha(payload) {
			unchanged++
			continue
		}
		// PUT directly with the sha from the tree: the contents GET inside
		// putToContent 403s on files over 1MB (migrated-pages.json is 1.7MB),
		// which would strip the sha and turn the update into a 422.
		put := map[string]any{
			"message": fmt.Sprintf("content: %s from site_content %s", path, time.Now().UTC().Format("2006-01-02")),
			"content": base64.StdEncoding.EncodeToString(payload),
		}
		if sha := repoSha[path]; sha != "" {
			put["sha"] = sha
		}
		pb, _ := json.Marshal(put)
		req, _ := http.NewRequest("PUT", "https://api.github.com/repos/srjordan6/srj-content/contents/"+path, bytes.NewReader(pb))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		pr, err := client.Do(req)
		if err != nil {
			return err
		}
		prb, _ := io.ReadAll(pr.Body)
		pr.Body.Close()
		if pr.StatusCode != 200 && pr.StatusCode != 201 {
			return fmt.Errorf("github PUT %s %d: %.200s", path, pr.StatusCode, prb)
		}
		exported++
	}
	fmt.Printf("sync_content: imported=%d exported=%d unchanged=%d ok=true\n", imported, exported, unchanged)
	return nil
}

// ---- sync_people: AI Movers and Shakers, SQL -> srj-content ----------------
//
// site_people (slug, data jsonb) is the single source of truth for the people
// directory, per the July 31 directive that ALL content lives in SQL and no
// content files ever land on a local machine. This stage makes the repo a
// generated artifact of the table:
//
//   1. Ensures the table exists.
//   2. One-time backfill: any people/{slug}.json already in srj-content that
//      has no SQL row is imported (ON CONFLICT DO NOTHING, so SQL always
//      wins afterward). This absorbs the 37 pre-directive profiles without a
//      manual load and is a no-op on every later run.
//   3. Exports every SQL row to people/{slug}.json via the GitHub API,
//      skipping files whose content is already identical, so quiet days
//      produce zero commits.
//
// roster.json is not a person and is left alone in the repo.
func syncPeople(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS site_people (
		slug text PRIMARY KEY, data jsonb NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	client := &http.Client{Timeout: 60 * time.Second}
	gh := func(method, url string, body []byte) (*http.Response, error) {
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		return client.Do(req)
	}

	// 2. Backfill from the repo listing.
	resp, err := gh("GET", "https://api.github.com/repos/srjordan6/srj-content/contents/people", nil)
	if err != nil {
		return err
	}
	var listing []struct {
		Name        string `json:"name"`
		DownloadURL string `json:"download_url"`
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 {
		_ = json.Unmarshal(b, &listing)
	}
	imported := 0
	for _, f := range listing {
		if !strings.HasSuffix(f.Name, ".json") || f.Name == "roster.json" {
			continue
		}
		slug := strings.TrimSuffix(f.Name, ".json")
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM site_people WHERE slug=$1)`, slug).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		fr, err := gh("GET", f.DownloadURL, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sync_people: fetch %s: %v\n", f.Name, err)
			continue
		}
		fb, _ := io.ReadAll(fr.Body)
		fr.Body.Close()
		if fr.StatusCode != 200 || !json.Valid(fb) {
			fmt.Fprintf(os.Stderr, "sync_people: skip %s: status %d or invalid JSON\n", f.Name, fr.StatusCode)
			continue
		}
		if _, err := db.Exec(`INSERT INTO site_people (slug, data) VALUES ($1, $2::jsonb)
			ON CONFLICT (slug) DO NOTHING`, slug, string(fb)); err != nil {
			return err
		}
		imported++
	}

	// 3. Export SQL -> repo, skipping identical files.
	rows, err := db.Query(`SELECT slug, jsonb_pretty(data) FROM site_people ORDER BY slug`)
	if err != nil {
		return err
	}
	defer rows.Close()
	exported, unchanged := 0, 0
	for rows.Next() {
		var slug, pretty string
		if err := rows.Scan(&slug, &pretty); err != nil {
			return err
		}
		payload := []byte(pretty + "\n")
		path := "people/" + slug + ".json"
		cur, err := gh("GET", "https://api.github.com/repos/srjordan6/srj-content/contents/"+path, nil)
		if err == nil {
			var meta struct {
				Content string `json:"content"`
			}
			cb, _ := io.ReadAll(cur.Body)
			cur.Body.Close()
			if cur.StatusCode == 200 && json.Unmarshal(cb, &meta) == nil {
				if dec, e := base64.StdEncoding.DecodeString(strings.ReplaceAll(meta.Content, "\n", "")); e == nil && bytes.Equal(dec, payload) {
					unchanged++
					continue
				}
			}
		}
		if err := putToContent(tok, path,
			fmt.Sprintf("people: %s from site_people %s", slug, time.Now().UTC().Format("2006-01-02")), payload); err != nil {
			return err
		}
		exported++
	}
	fmt.Printf("sync_people: imported=%d exported=%d unchanged=%d ok=true\n", imported, exported, unchanged)
	return nil
}

// ---- deploy_site: rebuild the website so today's data actually ships -------
//
// The publish stages write JSON to srj-content, but the website only rebuilds
// when srj-site itself is pushed, so without this stage a day's lawsuits,
// vendor news, and roundup sit in the content repo and never reach a visitor.
// This fires srj-site's existing "Trigger Cloudflare build" workflow through
// the GitHub API, using the token the pipeline already carries. No new secret,
// and the workflow already supports workflow_dispatch.
//
// A failure here is reported but should never be read as "the data is wrong":
// the data is published either way, it is the rebuild that did not happen.
func deploySite() error {
	// Preferred path: POST the Cloudflare deploy hook directly. Set
	// CLOUDFLARE_DEPLOY_HOOK on the Render service (same URL srj-site keeps
	// as a GitHub secret); it needs no GitHub permissions at all.
	if hook := strings.TrimSpace(os.Getenv("CLOUDFLARE_DEPLOY_HOOK")); hook != "" {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Post(hook, "application/json", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return fmt.Errorf("deploy hook returned %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
		}
		// theworldofai.org rebuilds on the same trigger when its hook is set.
		if th := strings.TrimSpace(os.Getenv("TWOAI_DEPLOY_HOOK")); th != "" {
			tr, terr := client.Post(th, "application/json", nil)
			if terr != nil {
				fmt.Fprintln(os.Stderr, "twoai deploy hook:", terr)
			} else {
				trb, _ := io.ReadAll(tr.Body)
				tr.Body.Close()
				if tr.StatusCode < 200 || tr.StatusCode > 299 {
					fmt.Fprintf(os.Stderr, "twoai deploy hook returned %d: %s\n", tr.StatusCode, strings.TrimSpace(string(trb)))
				}
			}
		}
		return nil
	}
	// Fallback: dispatch srj-site's deploy workflow. Requires the PAT to
	// carry workflow scope; a 403 here means add that scope or set the
	// CLOUDFLARE_DEPLOY_HOOK env var above.
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set and CLOUDFLARE_DEPLOY_HOOK not set")
	}
	const api = "https://api.github.com/repos/srjordan6/srj-site/actions/workflows/deploy.yml/dispatches"
	body := []byte(`{"ref":"main"}`)
	req, err := http.NewRequest("POST", api, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "srj-pipeline/1.0")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	// 204 No Content is the documented success for a workflow dispatch.
	if resp.StatusCode != 204 && resp.StatusCode != 201 && resp.StatusCode != 200 {
		return fmt.Errorf("workflow dispatch returned %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	return nil
}

// ---- publish_lawsuits: AI Lawsuit Database -> srj-content ------------------
//
// Exports every active case in ai_lawsuits, in display order, as one JSON
// document the Astro build consumes for /ai-governance/ai-lawsuits/. Runs
// after the intel stage in `all`, so each night's docket refresh reaches the
// site on its next build. json.RawMessage passes the timeline/claims/tags
// JSONB and the array columns through verbatim instead of re-modeling them.
func publishLawsuits(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	rows, err := db.Query(`
		SELECT json_build_object(
		  'slug', slug, 'case_name', case_name, 'court', court, 'docket', docket,
		  'judge', judge, 'filed_date', filed_date, 'plaintiffs', plaintiffs,
		  'defendants', defendants, 'category', category, 'status', status,
		  'status_badge', status_badge, 'latest_development', latest_development,
		  'latest_development_date', latest_development_date,
		  'courtlistener_url', courtlistener_url,
		  'executive_summary', executive_summary, 'why_it_matters', why_it_matters,
		  'target_models', target_models, 'disputed_datasets', disputed_datasets,
		  'materials_at_issue', materials_at_issue,
		  'plaintiff_counsel', plaintiff_counsel, 'defendant_counsel', defendant_counsel,
		  'claims', claims, 'timeline', timeline, 'tags', tags,
		  'related_slugs', related_slugs, 'display_order', display_order,
		  'verified_date', verified_date)
		FROM ai_lawsuits WHERE is_active ORDER BY display_order`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var cases []json.RawMessage
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		cases = append(cases, json.RawMessage(raw))
	}
	if len(cases) == 0 {
		return fmt.Errorf("ai_lawsuits returned no active cases; refusing to publish an empty database")
	}
	out, _ := json.MarshalIndent(map[string]any{
		"generated": time.Now().UTC().Format(time.RFC3339),
		"cases":     cases,
	}, "", "  ")
	return putToContent(tok, "lawsuits/lawsuits.json",
		fmt.Sprintf("lawsuits: %d cases %s", len(cases), time.Now().UTC().Format("2006-01-02")), out)
}

// ---- publish_intel: AI watch feed -> srj-content ---------------------------
//
// Publishes the newest non-ignored rows from ai_intel_candidates (new models,
// tools, terminology, vendor announcements) for the Everything else AI page.
// Ignored rows never ship; everything else does, newest first, capped so the
// page and the JSON stay small.
func publishIntel(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	rows, err := db.Query(`
		SELECT json_build_object(
		  'kind', kind, 'name', name, 'vendor', vendor, 'url', url,
		  'summary', summary, 'source', source, 'discovered_at', discovered_at)
		FROM ai_intel_candidates
		WHERE status <> 'ignored'
		ORDER BY discovered_at DESC LIMIT 120`)
	if err != nil {
		return err
	}
	defer rows.Close()
	items := []json.RawMessage{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		items = append(items, json.RawMessage(raw))
	}
	out, _ := json.MarshalIndent(map[string]any{
		"generated": time.Now().UTC().Format(time.RFC3339),
		"items":     items,
	}, "", "  ")
	return putToContent(tok, "intel/intel.json",
		fmt.Sprintf("intel: %d items %s", len(items), time.Now().UTC().Format("2006-01-02")), out)
}

// arxivWatch is phase 3 of the watch-everything directive: affiliation
// tracking on new arXiv preprints. Pulls the newest submissions in cs.AI,
// cs.CL, and cs.LG from the official arXiv Atom API and keeps only papers
// whose title or abstract names a tracked institution, so the volume that
// lands in ai_intel_candidates stays reviewable instead of flooding it with
// every preprint. arXiv terms permit this use; the API is free and keyless.
func arxivWatch(db *sql.DB) error {
	orgs := []string{
		"Tsinghua", "Peking University", "BAAI", "Beijing Academy",
		"Shanghai AI Lab", "Chinese Academy of Sciences", "RIKEN", "AIST",
		"KAIST", "Naver", "MBZUAI", "AI21", "Weizmann", "Hebrew University",
		"Mistral", "Aleph Alpha", "Stability AI", "Alan Turing Institute",
		"Max Planck", "INRIA", "ELLIS", "AI Singapore", "DeepMind", "OpenAI",
		"Anthropic", "Hugging Face", "Zhipu", "Moonshot", "DeepSeek", "Qwen",
		"Alibaba", "Tencent", "ByteDance", "Huawei", "Baidu",
	}
	type entry struct {
		Title   string `xml:"title"`
		Summary string `xml:"summary"`
		ID      string `xml:"id"`
	}
	var parsed struct {
		Entries []entry `xml:"entry"`
	}
	added := 0
	for _, cat := range []string{"cs.AI", "cs.CL", "cs.LG"} {
		u := "https://export.arxiv.org/api/query?search_query=cat:" + cat +
			"&sortBy=submittedDate&sortOrder=descending&max_results=40"
		// arXiv's API is occasionally slow to first byte (Aug 1: cs.AI timed
		// out at 30s and the whole category skipped for the day). One retry
		// with a longer timeout keeps a slow response from costing a category.
		fetch := func(timeout time.Duration) (*http.Response, error) {
			req, _ := http.NewRequest("GET", u, nil)
			req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (srjconsultingservices.com)")
			client := &http.Client{Timeout: timeout}
			return client.Do(req)
		}
		resp, err := fetch(30 * time.Second)
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			time.Sleep(5 * time.Second)
			resp, err = fetch(90 * time.Second)
		}
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			fmt.Fprintln(os.Stderr, "arxiv_watch", cat, ":", err)
			continue
		}
		parsed.Entries = nil
		dec := xml.NewDecoder(resp.Body)
		dec.Strict = false
		dec.Entity = xml.HTMLEntity
		derr := dec.Decode(&parsed)
		resp.Body.Close()
		if derr != nil {
			fmt.Fprintln(os.Stderr, "arxiv_watch", cat, "parse:", derr)
			continue
		}
		for _, e := range parsed.Entries {
			text := e.Title + " " + e.Summary
			var hit string
			for _, o := range orgs {
				if strings.Contains(text, o) {
					hit = o
					break
				}
			}
			if hit == "" || e.ID == "" {
				continue
			}
			title := strings.Join(strings.Fields(e.Title), " ")
			r, ierr := db.Exec(`INSERT INTO ai_intel_candidates (kind, name, vendor, url, source, source_id)
				VALUES ('paper', $1, $2, $3, 'arxiv', $4)
				ON CONFLICT (source_id) DO NOTHING`,
				trunc(title, 300), hit, e.ID, "arxiv-"+e.ID)
			if ierr != nil {
				continue
			}
			if n, _ := r.RowsAffected(); n > 0 {
				added++
			}
		}
		time.Sleep(3 * time.Second) // arXiv API courtesy delay
	}
	fmt.Printf("arxiv_watch: papers_added=%d ok=true\n", added)
	return nil
}
