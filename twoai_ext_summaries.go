package main

// Our own summary page for every IntuitionLabs AI article, with its facts
// checked at their primary sources. theworldofai rows 453 and 456 (Stephen,
// 2026-10-04: "I want everything on this site in our system", then "fix it
// now").
//
// For each AI article in twoai_ext_library, 40 a run, the 133 general AI
// articles first, then the life sciences ones newest first:
//
//  1. The article is read and the links it cites are collected.
//  2. The model writes our own reading of it (never their wording), lists
//     the checkable facts it states with the cited source each rests on, and
//     for a general AI article names the hub of this site it belongs to.
//  3. Each fact is checked at its primary source: the source is fetched and
//     the model says whether that text states the fact. A fact whose source
//     cannot be read, or does not state it, is dropped, never published on
//     IntuitionLabs' word alone, because IntuitionLabs says its content may
//     be AI assisted.
//  4. The page is written as industries/ext-<uid>.json, kind source-summary,
//     credited to the article at the bottom, kept out of search until it
//     carries at least one checked fact.
//
// twoaiExtLibraryPublish then links the Healthcare list to these pages, and
// places the strongest checked facts as sourced points on Healthcare and on
// each general article's hub, with a further reading list there.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const extSumPerRun = 40
const extSumBudget = 30 * time.Minute

// The hubs a general AI article can be placed on: taxonomy slugs with a live
// page on this site.
var extSumHubs = []string{
	"foundation-models", "llms", "reasoning-models", "coding-models", "multimodal-models", "small-language-models",
	"agents-and-mcp", "autonomous-agents", "coding-and-browser-agents", "multi-agent-systems", "vibe-coding",
	"mcp-security", "ai-security-risk", "sec-prompt-injection", "sec-agent-security", "sec-ai-privacy",
	"governance-frameworks", "gpus", "memory-and-storage", "serving-providers", "ai-jobs-and-market",
	"prompt-engineering", "embedding-models",
}

// Any link, whatever its text holds. Citations often end in a #:~:text=
// fragment pointing at the passage, which an earlier pattern refused, so the
// fragment is cut, not the link.
var extHrefRe = regexp.MustCompile(`(?i)<a[^>]+href="(https?://[^"]+)"`)

func extFetch(u string, max int64) ([]byte, string, error) {
	client := &http.Client{Timeout: 45 * time.Second}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 SRJ-Consulting-intel-sync/1.0 (theworldofai.org)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max))
	return b, resp.Header.Get("Content-Type"), err
}

type extFact struct {
	Claim  string `json:"claim"`
	Source int    `json:"source"`
}

type extSummary struct {
	Title    string `json:"title"`
	Answer   string `json:"answer"`
	Sections []struct {
		Heading string `json:"heading"`
		Body    string `json:"body"`
	} `json:"sections"`
	Facts []extFact `json:"facts"`
	Hub   string    `json:"hub"`
}

func twoaiExtSummaries(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_ext_summaries (source text NOT NULL, slug text NOT NULL, uid text NOT NULL,
		hub_slug text, facts_total int, facts_verified int, facts_dropped int, doc jsonb, model text,
		written_on date, error text, tries int NOT NULL DEFAULT 0, PRIMARY KEY (source, slug))`)
	rows, err := db.Query(`SELECT l.slug, l.url, l.title, l.topic FROM twoai_ext_library l
		LEFT JOIN twoai_ext_summaries s ON s.source = l.source AND s.slug = l.slug
		WHERE l.source = $1 AND l.is_ai AND l.topic IS NOT NULL
		  AND (s.slug IS NULL OR (s.doc IS NULL AND s.tries < 3))
		ORDER BY (l.topic = 'none') DESC, l.lastmod DESC NULLS LAST LIMIT $2`, extLibSource, extSumPerRun)
	if err != nil {
		fmt.Println("twoai_ext_summaries:", err)
		return
	}
	type job struct{ slug, url, title, topic string }
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.slug, &j.url, &j.title, &j.topic) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	hubSet := map[string]bool{}
	for _, h := range extSumHubs {
		hubSet[h] = true
	}
	start := time.Now()
	// Browser reads for script-drawn sources, kept inside the included hours.
	browserLeft := 30
	db.Exec(`ALTER TABLE twoai_ext_summaries ADD COLUMN IF NOT EXISTS drops jsonb`)
	written, verified, dropped := 0, 0, 0
	for _, j := range jobs {
		if time.Since(start) > extSumBudget {
			break
		}
		h := sha256.Sum256([]byte("ext:" + extLibSource + ":" + j.slug))
		uid := hex.EncodeToString(h[:4])
		fail := func(msg string) {
			db.Exec(`INSERT INTO twoai_ext_summaries (source, slug, uid, error, tries) VALUES ($1,$2,$3,$4,1)
				ON CONFLICT (source, slug) DO UPDATE SET error = $4, tries = twoai_ext_summaries.tries + 1`, extLibSource, j.slug, uid, msg)
		}
		raw, _, err := extFetch(j.url, 8<<20)
		if err != nil {
			fail("article: " + err.Error())
			continue
		}
		_, text := crawlText(raw)
		if len(text) < 600 {
			fail("article text too short")
			continue
		}
		// The links the article cites, its own site excluded.
		var links []string
		seen := map[string]bool{}
		for _, m := range extHrefRe.FindAllStringSubmatch(string(raw), -1) {
			u := m[1]
			if k := strings.Index(u, "#"); k >= 0 {
				u = u[:k]
			}
			u = strings.ReplaceAll(u, "&amp;", "&")
			pu, err := url.Parse(u)
			if err != nil || strings.Contains(pu.Host, "intuitionlabs.ai") || seen[u] {
				continue
			}
			if strings.Contains(pu.Host, "linkedin.") || strings.Contains(pu.Host, "twitter.") || strings.Contains(pu.Host, "x.com") || strings.Contains(pu.Host, "facebook.") {
				continue
			}
			seen[u] = true
			links = append(links, u)
			if len(links) == 40 {
				break
			}
		}
		var lb strings.Builder
		for i, l := range links {
			fmt.Fprintf(&lb, "[%d] %s\n", i+1, l)
		}
		hubLine := ""
		if j.topic == "none" {
			hubLine = "\nAlso choose the one hub of the site this article belongs to, from this list, copied exactly, or none if it fits none: " +
				strings.Join(extSumHubs, ", ") + "."
		}
		system := "You write for The World of AI, a reference site about artificial intelligence. You are given an article by IntuitionLabs " +
			"and the numbered list of links it cites. Write OUR OWN reading of the article, never its wording: no copied sentences or phrases. " +
			"Return ONLY JSON: {\"title\": a plain title of at most 90 characters in our words, " +
			"\"answer\": two sentences saying what the article covers and its main finding, " +
			"\"sections\": two or three objects {\"heading\": a question, \"body\": 60 to 120 words}, " +
			"\"facts\": up to five checkable facts the article states, each {\"claim\": one sentence with the specific number, date or name, " +
			"\"source\": the number of the cited link the claim rests on, or 0 if the article cites none for it}" +
			", \"hub\": the hub slug or none}." + hubLine +
			" Plain English, commas rather than dashes, no marketing, nothing the article does not say."
		user := "Article title: " + j.title + "\nAddress: " + j.url + "\n\nCited links:\n" + lb.String() + "\nArticle text:\n" + trunc(text, 14000)
		out, model, gerr := twoaiGenerate("ext_summaries", system, user)
		if gerr != nil {
			fail("model: " + gerr.Error())
			if strings.Contains(gerr.Error(), "peak") {
				break
			}
			continue
		}
		if i, k := strings.Index(out, "{"), strings.LastIndex(out, "}"); i >= 0 && k > i {
			out = out[i : k+1]
		}
		var m extSummary
		if json.Unmarshal([]byte(out), &m) != nil || strings.TrimSpace(m.Answer) == "" {
			fail("model reply was not the JSON asked for")
			continue
		}
		// Check each fact at its primary source.
		type checked struct {
			Claim       string `json:"claim"`
			SourceURL   string `json:"source_url"`
			SourceTitle string `json:"source_title"`
		}
		var facts []checked
		nDropped := 0
		var drops []map[string]string
		for _, f := range m.Facts {
			if len(facts)+nDropped >= 5 {
				break
			}
			drop := func(why string) {
				nDropped++
				drops = append(drops, map[string]string{"claim": f.Claim, "why": why})
			}
			if f.Source < 1 || f.Source > len(links) || strings.TrimSpace(f.Claim) == "" {
				drop("the article cites no source for it")
				continue
			}
			src := links[f.Source-1]
			stitle, stext, why := extSourceText(src, &browserLeft)
			if why != "" {
				drop(why)
				continue
			}
			// PRIMARY SOURCES ONLY (row 456): the source must be where the
			// fact originates, not a report about it.
			jsys := "You check one claim against one source. Answer YES only if (1) the source text itself states the claim, " +
				"including its numbers, dates and names, and (2) the source is where the fact originates: the company, regulator, " +
				"standards body, court, researchers or official dataset it is about, not a news story, blog or market report repeating it. " +
				"Otherwise answer NO. Reply with YES or NO, then one short reason."
			ans, _, jerr := twoaiGenerate("ext_fact_check", jsys, "Claim: "+f.Claim+"\n\nSource address: "+src+"\nSource title: "+stitle+"\n\nSource text:\n"+trunc(stext, 12000))
			if jerr != nil {
				drop("check failed: " + trunc(jerr.Error(), 80))
				continue
			}
			if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(ans)), "YES") {
				drop("not confirmed: " + trunc(strings.TrimSpace(ans), 140))
				continue
			}
			if stitle == "" {
				stitle = src
			}
			facts = append(facts, checked{strings.TrimSpace(f.Claim), src, strings.TrimSpace(stitle)})
		}
		hub := ""
		if j.topic == "none" && hubSet[m.Hub] {
			hub = m.Hub
		}
		var sections []map[string]string
		for _, s := range m.Sections {
			if strings.TrimSpace(s.Body) != "" {
				sections = append(sections, map[string]string{"heading": strings.TrimSpace(s.Heading), "body": strings.TrimSpace(s.Body)})
			}
		}
		title := strings.TrimSpace(m.Title)
		if title == "" {
			title = j.title
		}
		doc := map[string]any{
			"uid": uid, "page_uid": uid, "shape": "art-topic", "slug": "ext-" + uid,
			"name": title, "title": title, "answer": strings.TrimSpace(m.Answer), "sections": sections,
			"category": "enterprise-applications-governance-and-tools",
			"kind":     "source-summary", "built_from": "article",
			"facts":      facts,
			"source_url": j.url, "source_name": j.title, "source_publisher": "IntuitionLabs",
			"source_read_on": time.Now().UTC().Format("2006-01-02"),
			"ext_topic":      j.topic, "ext_hub": hub,
			"generated": time.Now().UTC().Format("2006-01-02"), "built_at": time.Now().Format(time.RFC3339),
			"refresh_every_days": 180,
			// Kept out of search until it carries a fact checked at its source.
			"noindex": len(facts) == 0,
		}
		dj, _ := json.Marshal(doc)
		dropsJ, _ := json.Marshal(drops)
		if _, err := db.Exec(`INSERT INTO twoai_ext_summaries (source, slug, uid, hub_slug, facts_total, facts_verified, facts_dropped, doc, model, written_on, error, tries, drops)
			VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8::jsonb,$9,current_date,NULL,1,$10::jsonb)
			ON CONFLICT (source, slug) DO UPDATE SET uid=$3, hub_slug=NULLIF($4,''), facts_total=$5, facts_verified=$6, facts_dropped=$7,
				doc=$8::jsonb, model=$9, written_on=current_date, error=NULL, tries=twoai_ext_summaries.tries+1, drops=$10::jsonb`,
			extLibSource, j.slug, uid, hub, len(m.Facts), len(facts), nDropped, string(dj), model, string(dropsJ)); err != nil {
			fmt.Println("twoai_ext_summaries:", j.slug, err)
			continue
		}
		written++
		verified += len(facts)
		dropped += nDropped
	}
	fmt.Printf("twoai_ext_summaries: %d summaries written, %d facts checked at their source, %d dropped ok=true\n", written, verified, dropped)
}

const extSumBase = "/ai-ecosystem/enterprise-applications-governance-and-tools/"

// extPagePath finds the twoai_pages row behind a live path ending in a uid.
func extPagePath(db *sql.DB, livePath string) string {
	p := strings.TrimSuffix(livePath, "/")
	uid := p[strings.LastIndex(p, "/")+1:]
	var path string
	db.QueryRow(`SELECT path FROM twoai_pages WHERE (data->>'uid' = $1 OR data->>'page_uid' = $1)
		ORDER BY (path LIKE 'ecosystem/%') DESC, path LIMIT 1`, uid).Scan(&path)
	return path
}

// twoaiExtSummariesPublish writes the summary pages and places their checked
// facts and further reading lists.
func twoaiExtSummariesPublish(db *sql.DB) {
	var hcPath, hcName string
	db.QueryRow(`SELECT live_path, name FROM twoai_taxonomy WHERE slug = 'industry-healthcare'`).Scan(&hcPath, &hcName)
	if hcPath == "" {
		hcPath, hcName = extSumBase+"092b8864/", "Healthcare"
	}
	rows, err := db.Query(`SELECT s.slug, s.uid, s.doc::text, COALESCE(s.hub_slug,''), COALESCE(t.live_path,''), COALESCE(t.name,''),
			COALESCE(l.lastmod::text, l.first_seen::text)
		FROM twoai_ext_summaries s JOIN twoai_ext_library l ON l.source = s.source AND l.slug = s.slug
		LEFT JOIN twoai_taxonomy t ON t.slug = s.hub_slug AND t.status = 'live'
		WHERE s.source = $1 AND s.doc IS NOT NULL
		ORDER BY COALESCE(l.lastmod, l.first_seen) DESC`, extLibSource)
	if err != nil {
		fmt.Println("twoai_ext_summaries publish:", err)
		return
	}
	type placed struct {
		Claim       string `json:"claim"`
		SourceURL   string `json:"source_url"`
		SourceTitle string `json:"source_title"`
		Via         string `json:"via"`
		ViaTitle    string `json:"via_title"`
		Topic       string `json:"topic,omitempty"`
	}
	hubReading := map[string][]map[string]string{} // live path -> items
	hubName := map[string]string{}
	points := map[string][]placed{} // live path -> facts
	topicFacts := map[string]int{}
	pages, unplaced := 0, 0
	for rows.Next() {
		var slug, uid, raw, hub, hubPath, hName, date string
		if rows.Scan(&slug, &uid, &raw, &hub, &hubPath, &hName, &date) != nil {
			continue
		}
		var doc map[string]any
		if json.Unmarshal([]byte(raw), &doc) != nil {
			continue
		}
		topic, _ := doc["ext_topic"].(string)
		parentPath, parentName := hcPath, hcName
		if topic == "none" {
			if hubPath == "" {
				unplaced++
				parentPath, parentName = "", ""
			} else {
				parentPath, parentName = hubPath, hName
			}
		}
		if parentPath != "" {
			doc["parent_path"], doc["parent_name"] = parentPath, parentName
			doc["crumbs"] = []map[string]string{{"name": parentName, "path": parentPath}}
		}
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, url_count, updated_at)
			VALUES ($1, 'tech-section', $2::jsonb, 1, now())
			ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, updated_at = now()
			WHERE (twoai_pages.data - 'built_at') IS DISTINCT FROM (EXCLUDED.data - 'built_at')`,
			"industries/ext-"+uid+".json", string(j)); err == nil {
			pages++
		}
		ours := extSumBase + uid + "/"
		title, _ := doc["name"].(string)
		var facts []placed
		if fs, ok := doc["facts"].([]any); ok {
			for _, x := range fs {
				if f, ok := x.(map[string]any); ok {
					c, _ := f["claim"].(string)
					u, _ := f["source_url"].(string)
					t, _ := f["source_title"].(string)
					if c != "" && u != "" {
						facts = append(facts, placed{c, u, t, ours, title, ""})
					}
				}
			}
		}
		if topic == "none" {
			if hubPath == "" {
				continue
			}
			hubName[hubPath] = hName
			hubReading[hubPath] = append(hubReading[hubPath], map[string]string{"title": title, "url": ours, "date": date})
			for _, f := range facts {
				if len(points[hubPath]) < 4 {
					points[hubPath] = append(points[hubPath], f)
				}
			}
			continue
		}
		// Life sciences: up to three checked facts per topic on Healthcare.
		for _, f := range facts {
			if topicFacts[topic] < 3 {
				f.Topic = topic
				points[hcPath] = append(points[hcPath], f)
				topicFacts[topic]++
			}
		}
	}
	rows.Close()
	put := func(livePath, key string, v any) bool {
		path := extPagePath(db, livePath)
		if path == "" {
			return false
		}
		j, _ := json.Marshal(v)
		db.Exec(`INSERT INTO twoai_page_extras (page_path, key, value, updated_at) VALUES ($1,$2,$3::jsonb,now())
			ON CONFLICT (page_path, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
			WHERE twoai_page_extras.value IS DISTINCT FROM EXCLUDED.value`, path, key, string(j))
		return true
	}
	// Placements are rebuilt whole each run, so a page whose facts are gone
	// loses them. The Healthcare library is twoaiExtLibrary's and is kept.
	db.Exec(`DELETE FROM twoai_page_extras WHERE key = 'ext_points' OR (key = 'library' AND value->>'ours' = 'true')`)
	placedPts := 0
	for lp, ps := range points {
		if put(lp, "ext_points", map[string]any{"source": "IntuitionLabs", "items": ps}) {
			placedPts += len(ps)
		}
	}
	for lp, items := range hubReading {
		put(lp, "library", map[string]any{
			"source": "IntuitionLabs", "source_url": "https://intuitionlabs.ai/articles",
			"count": len(items), "as_of": time.Now().UTC().Format("2006-01-02"), "ours": true,
			"groups": []map[string]any{{"topic": hubName[lp], "count": len(items), "items": items}},
		})
	}
	fmt.Printf("twoai_ext_summaries: %d pages, %d checked facts placed, %d general articles with no hub ok=true\n", pages, placedPts, unplaced)
}

// extSourceText reads a cited source for the fact check: an HTML page, a PDF
// through pdftotext, or a page drawn by script through the browser reader
// while the run's browser allowance lasts. A non-empty why says why the
// source could not be read.
func extSourceText(src string, browserLeft *int) (title, text, why string) {
	raw, ctype, err := extFetch(src, 12<<20)
	isPDF := strings.Contains(strings.ToLower(ctype), "pdf") || strings.HasSuffix(strings.ToLower(src), ".pdf")
	if err == nil && isPDF {
		pt, perr := twoaiFindPdftotext()
		if perr != nil {
			return "", "", "PDF source, pdftotext not available"
		}
		dir, derr := os.MkdirTemp("", "extpdf")
		if derr != nil {
			return "", "", "PDF source, no temp folder"
		}
		defer os.RemoveAll(dir)
		pf, tf := filepath.Join(dir, "s.pdf"), filepath.Join(dir, "s.txt")
		os.WriteFile(pf, raw, 0o644)
		if out, cerr := exec.Command(pt, "-enc", "UTF-8", "-l", "60", pf, tf).CombinedOutput(); cerr != nil {
			return "", "", "PDF source unreadable: " + trunc(strings.TrimSpace(string(out)), 60)
		}
		b, _ := os.ReadFile(tf)
		t := strings.Join(strings.Fields(string(b)), " ")
		if len(t) < 300 {
			return "", "", "PDF source has no text layer"
		}
		return src[strings.LastIndex(src, "/")+1:], t, ""
	}
	if err == nil {
		title, text = crawlText(raw)
		if len(text) >= 300 {
			return title, text, ""
		}
	}
	// Refused or drawn by script: one try through the browser reader.
	if *browserLeft > 0 {
		*browserLeft--
		client := &http.Client{Timeout: 90 * time.Second}
		if code, braw, _, berr := crawlFetchBrowser(client, src); berr == nil && code == 200 {
			title, text = crawlText(braw)
			if len(text) >= 300 {
				return title, text, ""
			}
		}
	}
	if err != nil {
		return "", "", "source could not be fetched: " + trunc(err.Error(), 60)
	}
	return "", "", "source page has no readable text"
}
