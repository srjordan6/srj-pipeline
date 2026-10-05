package main

// Count tokens for the reference sections, theworldofai row 467 (Stephen,
// 2026-10-05: "why do you keep hard coding numbers in this site, they all
// need to be dynamic").
//
// The art writer used to give the model the site's counts as figures, and the
// model wrote them into readings that are cached for months: "688 glossary
// terms" stayed on 15 pages while the glossary grew to 720. The writer now
// gives placeholders (twoaiArtFacts.line), and this file supplies their
// values to the publisher and converts the readings already written.

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// twoaiArtLiveCounts adds the art figures to the publisher's tokens, counted
// exactly as twoaiArtReadFacts counts them.
func twoaiArtLiveCounts(db *sql.DB, v map[string]string) {
	f := twoaiArtReadFacts(db)
	set := func(k string, n int) { v[k] = withCommas(int64(n)) }
	set("art_image_models", f.ImageModels)
	set("art_video_models", f.VideoModels)
	set("art_audio_models", f.AudioModels)
	set("art_tools", f.Tools)
	set("art_ip_cases", f.IPCases)
	set("art_music_cases", f.MusicCases)
	set("art_all_cases", f.AllCases)
	set("art_compliance_pages", f.Compliance)
	set("art_caselaw", f.CaseLaw)
	set("art_bills", f.Bills)
	set("art_company_pages", f.SECCos)
	set("art_ma_filings", f.MAFilings)
	set("art_tickers", f.Tickers)
	set("art_med_models", f.MedModels)
	set("art_sci_models", f.SciModels)
	set("art_pl_cases", f.PLCases)
	set("art_papers", f.Papers)
	set("art_claims", f.Claims)
	set("art_book_titles", f.BookTitles)
	set("art_db_mcp", f.DBMCP)
	set("art_mcp_total", f.MCPTotal)
}

// A typed figure in a reading, and the token that replaces it. The noun
// phrase is kept, only the number becomes the placeholder.
var twoaiArtTokenRules = []struct {
	re    *regexp.Regexp
	token string
}{
	{regexp.MustCompile(`\b\d[\d,]*( (?:compliance and )?regulation pages)`), "art_compliance_pages"},
	{regexp.MustCompile(`\b\d[\d,]*( active AI lawsuits)`), "art_all_cases"},
	{regexp.MustCompile(`\b\d[\d,]*( (?:active )?intellectual property lawsuits)`), "art_ip_cases"},
	{regexp.MustCompile(`\b\d[\d,]*( (?:AI )?case law precedents)`), "art_caselaw"},
	{regexp.MustCompile(`\b\d[\d,]*( AI tools)`), "art_tools"},
	{regexp.MustCompile(`\b\d[\d,]*( research papers)`), "art_papers"},
	{regexp.MustCompile(`\b\d[\d,]*( (?:live )?image (?:generation )?models)`), "art_image_models"},
	{regexp.MustCompile(`\b\d[\d,]*( medical AI models)`), "art_med_models"},
	{regexp.MustCompile(`\b\d[\d,]*( video models)`), "art_video_models"},
	{regexp.MustCompile(`\b\d[\d,]*( state AI bills)`), "art_bills"},
	{regexp.MustCompile(`\b\d[\d,]*( audio models)`), "art_audio_models"},
	{regexp.MustCompile(`\b\d[\d,]*( glossary terms)`), "glossary_terms"},
	{regexp.MustCompile(`\b\d[\d,]*( scientific models)`), "art_sci_models"},
	{regexp.MustCompile(`\b\d[\d,]*( extracted claims| claims extracted)`), "art_claims"},
	{regexp.MustCompile(`\b\d[\d,]*( company pages)`), "art_company_pages"},
	{regexp.MustCompile(`\b\d[\d,]*( merger and acquisition filings)`), "art_ma_filings"},
	{regexp.MustCompile(`\b\d[\d,]*( listed AI-related instruments)`), "art_tickers"},
	{regexp.MustCompile(`\b\d[\d,]*( (?:active |tracked )?(?:Model Context Protocol|MCP) servers)`), "art_mcp_total"},
	{regexp.MustCompile(`\b\d[\d,]*( of them for SQL)`), "art_db_mcp"},
	{regexp.MustCompile(`\b\d[\d,]*( AI books)`), "art_book_titles"},
	{regexp.MustCompile(`\b\d[\d,]*( (?:active )?product liability and wrongful death lawsuits)`), "art_pl_cases"},
}

// twoaiArtTokenize replaces typed site counts in a reading with tokens.
func twoaiArtTokenize(s string) string {
	for _, r := range twoaiArtTokenRules {
		s = r.re.ReplaceAllString(s, "{{"+r.token+"}}$1")
	}
	return s
}

// twoaiArtTokenMigrate converts the readings written under the figure-based
// line: their body gets tokens and their hash moves to the token line, so
// the change of line rewrites nothing. A reading with any other hash is left
// to the writer's usual rules.
func twoaiArtTokenMigrate(db *sql.DB, nodes []twoaiArtNode, facts twoaiArtFacts, kidsOf map[string][]string) {
	moved := 0
	for _, n := range nodes {
		block := "all"
		extra := ""
		if n.Kind != "topic" {
			block = "hub"
			extra = strings.Join(kidsOf[n.Slug], ",")
			if ch := eduChapterOf(n.Slug); ch > 0 {
				for _, p := range eduChapterPapers(db, ch) {
					extra += "|" + p.Title
				}
			}
		}
		var body, have string
		if db.QueryRow(`SELECT body, data_hash FROM twoai_art_readings WHERE slug = $1 AND block = $2`, n.Slug, block).Scan(&body, &have) != nil {
			continue
		}
		tok := twoaiArtTokenize(body)
		oldLine := facts.numericLine(n.Section) + extra
		tokLine := facts.tokenLine(n.Section) + extra
		if have == twoaiArtHash(n, oldLine) || have == twoaiArtLegacyHash(n, oldLine) || have == twoaiArtHash(n, tokLine) {
			have = twoaiArtHash(n, facts.line(n.Section)+extra)
		} else if tok == body {
			continue
		}
		// Every typed count becomes a token; only a reading written under the
		// figure line has its hash moved, the rest keep their own rules.
		db.Exec(`UPDATE twoai_art_readings SET body = $3, data_hash = $4 WHERE slug = $1 AND block = $2`,
			n.Slug, block, tok, have)
		moved++
	}
	if moved > 0 {
		fmt.Printf("twoai_art: %d readings moved to count tokens\n", moved)
	}
}
