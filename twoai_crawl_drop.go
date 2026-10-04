package main

// Pages Stephen saves by hand from his own browser, for sites that refuse the
// crawler (2026-10-04: "just tell me which sites you need and i will do it").
//
// He saves each page in Chrome with Ctrl+S, "Webpage, HTML Only", into
// C:\srj-data\crawl-drop\<domain>\ (for example crawl-drop\deere.com\). Each
// run, before the digest step, every .html or .htm file there is read the way
// the crawler reads a page and stored in twoai_site_crawl_pages, fetched_via
// 'manual'. The site is marked crawled today with status read-manual, so the
// digest summarises it and the source page is written. Imported files move
// to crawl-drop\_done\<domain>\.
//
// The page's own address comes from Chrome's "saved from url" note, the
// canonical link or og:url in the file, in that order, and otherwise from the
// file name under the domain.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const crawlDropDir = `C:\srj-data\crawl-drop`

var (
	dropSavedFromRe = regexp.MustCompile(`(?i)<!--\s*saved from url=\(\d+\)(\S+?)\s*-->`)
	dropCanonRe     = regexp.MustCompile(`(?i)<link[^>]+rel=["']canonical["'][^>]*href=["']([^"']+)["']`)
	dropCanonRe2    = regexp.MustCompile(`(?i)<link[^>]+href=["']([^"']+)["'][^>]*rel=["']canonical["']`)
	dropOgURLRe     = regexp.MustCompile(`(?i)<meta[^>]+property=["']og:url["'][^>]*content=["']([^"']+)["']`)
)

// dropPageURL finds the address a saved page came from.
func dropPageURL(raw []byte, domain, file string) string {
	for _, re := range []*regexp.Regexp{dropSavedFromRe, dropCanonRe, dropCanonRe2, dropOgURLRe} {
		if m := re.FindSubmatch(raw); m != nil {
			u := strings.TrimSpace(string(m[1]))
			if strings.HasPrefix(u, "http") {
				return u
			}
		}
	}
	base := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	return "https://" + domain + "/#saved-" + strings.ReplaceAll(strings.ToLower(base), " ", "-")
}

func twoaiCrawlDropImport(db *sql.DB) {
	entries, err := os.ReadDir(crawlDropDir)
	if err != nil {
		return // no drop folder, nothing saved
	}
	total := 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		domain := strings.ToLower(strings.TrimPrefix(e.Name(), "www."))
		dir := filepath.Join(crawlDropDir, e.Name())
		files, _ := os.ReadDir(dir)
		pages, relevant := 0, 0
		for _, f := range files {
			ext := strings.ToLower(filepath.Ext(f.Name()))
			if f.IsDir() || (ext != ".html" && ext != ".htm") {
				continue
			}
			path := filepath.Join(dir, f.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			title, text := crawlText(raw)
			if len(text) < 200 {
				fmt.Printf("crawl_drop: %s/%s has no readable text, left in place\n", domain, f.Name())
				continue
			}
			u := dropPageURL(raw, domain, f.Name())
			score := len(aiTermRe.FindAllString(text, -1))
			h := sha256.Sum256([]byte(text))
			if _, err := db.Exec(`INSERT INTO twoai_site_crawl_pages (url, domain, title, text, ai_score, http_status, fetched_on, content_hash, fetched_via)
				VALUES ($1,$2,$3,$4,$5,200,current_date,$6,'manual')
				ON CONFLICT (url) DO UPDATE SET domain=$2, title=$3, text=$4, ai_score=$5, http_status=200, fetched_on=current_date, content_hash=$6, fetched_via='manual'`,
				u, domain, title, text, score, hex.EncodeToString(h[:8])); err != nil {
				fmt.Printf("crawl_drop: %s/%s: %v\n", domain, f.Name(), err)
				continue
			}
			pages++
			if score >= 3 && len(text) > 800 {
				relevant++
			}
			done := filepath.Join(crawlDropDir, "_done", e.Name())
			os.MkdirAll(done, 0o755)
			os.Rename(path, filepath.Join(done, f.Name()))
		}
		if pages == 0 {
			continue
		}
		db.Exec(`INSERT INTO twoai_site_crawl (domain, start_url, pages_fetched, pages_relevant, crawled_on, crawl_status)
			VALUES ($1, $2, $3, $4, current_date, 'read-manual')
			ON CONFLICT (domain) DO UPDATE SET crawled_on=current_date, crawl_status='read-manual',
				pages_fetched=GREATEST(COALESCE(twoai_site_crawl.pages_fetched,0), EXCLUDED.pages_fetched),
				pages_relevant=(SELECT count(*) FROM twoai_site_crawl_pages p WHERE p.domain=$1 AND p.http_status=200 AND p.ai_score >= 3 AND length(p.text) > 800)`,
			domain, "https://"+domain+"/", pages, relevant)
		fmt.Printf("crawl_drop: %s imported %d saved page(s), %d with AI material\n", domain, pages, relevant)
		total += pages
	}
	if total > 0 {
		fmt.Printf("crawl_drop: imported=%d ok=true\n", total)
	}
}
