package main

// Two more security references watched alongside MITRE ATLAS, both rendered
// on the AI Security and Risk hub.
//
// AVID, the AI Vulnerability Database (avidml.org). A catalogue of failure
// modes in general-purpose AI: reports, which are concrete occurrences with
// evidence, and vulns, which are recurring failure modes. Its own database
// repo, avidml/avid-db, is MIT licensed. The website terms are narrower than
// the data licence, so this site publishes only facts about the catalogue,
// the record identifiers, their type, the counts, and links back to AVID.
// No AVID descriptions are stored or rendered.
//
// OWASP Top 10 for LLM Applications (genai.owasp.org). The list every
// security team is asked about by name. The published per-risk pages are the
// authoritative web version; the newest edition ships first as a PDF, so the
// stage records the edition the site currently publishes AND the newest
// edition announced, rather than pretending the two are the same thing.
//
// Both are polled from pages that are stable and cheap. Neither harvest
// blocks the hub: a failure logs and leaves the previous rows in place.

import (
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	avidFeedURL  = "https://avidml.org/database/index.xml"
	avidSiteURL  = "https://avidml.org/database/"
	avidDataURL  = "https://github.com/avidml/avid-db"
	owaspListURL = "https://genai.owasp.org/llm-top-10/"
	owasp2026URL = "https://genai.owasp.org/resource/owasp-genai-llm-top-10-2026/"
)

// AVID identifiers look like AVID-2022-R0001 (report) or AVID-2023-V001
// (vulnerability). The year and class both come straight out of the id.
var avidIDRe = regexp.MustCompile(`AVID-(\d{4})-([RV])(\d+)`)

// The feed is large and its items are uniform, so identifiers and permalinks
// are read directly rather than unmarshalling a megabyte of descriptions we
// have decided not to store.
var avidFeedItemRe = regexp.MustCompile(`<title>(AVID-[0-9]{4}-[RV][0-9]+)</title><link>([^<]+)</link>`)

func twoaiAvidWatch(db *sql.DB) {
	b, err := twoaiGridGet(avidFeedURL)
	if err != nil {
		fmt.Println("twoai_avid: feed fetch failed:", err, "(keeping prior records)")
		return
	}
	matches := avidFeedItemRe.FindAllStringSubmatch(string(b), -1)
	if len(matches) == 0 {
		fmt.Println("twoai_avid: no records found in feed, keeping prior rows")
		return
	}
	stored := 0
	for _, m := range matches {
		id, link := m[1], m[2]
		parts := avidIDRe.FindStringSubmatch(id)
		if parts == nil {
			continue
		}
		year, _ := strconv.Atoi(parts[1])
		kind := "report"
		if parts[2] == "V" {
			kind = "vulnerability"
		}
		if _, err := db.Exec(`INSERT INTO twoai_avid_records (id, kind, year, url)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (id) DO UPDATE SET kind=EXCLUDED.kind, year=EXCLUDED.year,
				url=EXCLUDED.url, last_seen=now()`, id, kind, year, link); err == nil {
			stored++
		}
	}
	var reports, vulns int
	db.QueryRow(`SELECT count(*) FILTER (WHERE kind='report'), count(*) FILTER (WHERE kind='vulnerability')
		FROM twoai_avid_records`).Scan(&reports, &vulns)
	fmt.Printf("twoai_avid: feed records=%d stored=%d reports=%d vulns=%d\n",
		len(matches), stored, reports, vulns)
}

var owaspRiskRe = regexp.MustCompile(`href="(https://genai\.owasp\.org/llmrisk/[^"]+)"[^>]*>\s*(LLM(\d{2}):(\d{4})\s+[^<]{2,80}?)\s*</a>`)

func twoaiOwaspWatch(db *sql.DB) {
	b, err := twoaiGridGet(owaspListURL)
	if err != nil {
		fmt.Println("twoai_owasp: list fetch failed:", err, "(keeping prior rows)")
		return
	}
	matches := owaspRiskRe.FindAllStringSubmatch(string(b), -1)
	if len(matches) < 5 {
		fmt.Printf("twoai_owasp: only %d risks parsed, keeping prior rows\n", len(matches))
		return
	}
	seen := map[string]bool{}
	stored := 0
	edition := ""
	for _, m := range matches {
		url, label, num, year := m[1], strings.TrimSpace(m[2]), m[3], m[4]
		code := "LLM" + num + ":" + year
		if seen[code] {
			continue
		}
		seen[code] = true
		edition = year
		rank, _ := strconv.Atoi(num)
		// The label carries the code; the title is what follows it.
		title := strings.TrimSpace(strings.TrimPrefix(label, code))
		if title == "" {
			title = label
		}
		if _, err := db.Exec(`INSERT INTO twoai_owasp_risks (code, edition, rank, title, url)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (code) DO UPDATE SET edition=EXCLUDED.edition, rank=EXCLUDED.rank,
				title=EXCLUDED.title, url=EXCLUDED.url, last_seen=now()`,
			code, year, rank, title, url); err == nil {
			stored++
		}
	}
	fmt.Printf("twoai_owasp: risks parsed=%d stored=%d edition=%s\n", len(seen), stored, edition)
}

type owaspOut struct {
	Code  string `json:"code"`
	Rank  int    `json:"rank"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

type avidOut struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Year int    `json:"year"`
	URL  string `json:"url"`
}

func twoaiAvidDoc(db *sql.DB) map[string]any {
	var reports, vulns int
	if db.QueryRow(`SELECT count(*) FILTER (WHERE kind='report'), count(*) FILTER (WHERE kind='vulnerability')
		FROM twoai_avid_records`).Scan(&reports, &vulns) != nil || reports+vulns == 0 {
		return nil
	}
	newest := []avidOut{}
	rows, err := db.Query(`SELECT id, kind, COALESCE(year,0), url FROM twoai_avid_records
		ORDER BY year DESC, id DESC LIMIT 8`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var o avidOut
			if rows.Scan(&o.ID, &o.Kind, &o.Year, &o.URL) == nil {
				newest = append(newest, o)
			}
		}
	}
	var minYear, maxYear int
	db.QueryRow(`SELECT COALESCE(min(year),0), COALESCE(max(year),0) FROM twoai_avid_records`).Scan(&minYear, &maxYear)
	return map[string]any{
		"reports": reports, "vulnerabilities": vulns, "total": reports + vulns,
		"first_year": minYear, "latest_year": maxYear,
		"newest": newest, "site_url": avidSiteURL, "data_url": avidDataURL,
		"checked": time.Now().UTC().Format("2006-01-02"),
	}
}

func twoaiOwaspDoc(db *sql.DB) map[string]any {
	risks := []owaspOut{}
	rows, err := db.Query(`SELECT code, rank, title, url FROM twoai_owasp_risks ORDER BY rank`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var o owaspOut
		if rows.Scan(&o.Code, &o.Rank, &o.Title, &o.URL) == nil {
			risks = append(risks, o)
		}
	}
	if len(risks) == 0 {
		return nil
	}
	var edition string
	db.QueryRow(`SELECT edition FROM twoai_owasp_risks ORDER BY rank LIMIT 1`).Scan(&edition)
	return map[string]any{
		"edition": edition, "risks": risks,
		"list_url": owaspListURL, "newest_url": owasp2026URL,
		"newest_edition": "2026", "newest_published": "2026-08-03",
		"checked":        time.Now().UTC().Format("2006-01-02"),
	}
}
