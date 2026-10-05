package main

// twoai_art: sectioned reference sections built from a list of pages and
// written by the model. Two run through it today:
//
//   The Art of AI   (section art, ecosystem-entities-market-and-operations)
//   The AI Lawyer   (section law, enterprise-applications-governance-and-tools)
//
// Each is a hub, ten sub-hubs and fifty topics, and every topic uses the same
// five-part frame: scope, what it runs on, how the work is done, rights and
// risk and provenance, where it is going.
//
// WHERE IT COMES FROM. Stephen gave both outlines, on 2026-09-22. The outline
// is held in twoai_art_nodes, one row per page, with section and category on
// each row; the seed columns are optional. That table is the record of what
// each page is about; nothing here invents a topic.
//
// WHAT THE MODEL DOES. Stephen, 2026-09-22: the model writes the seed too. A
// topic row need carry nothing but its name and the sub-hub it belongs to;
// Ollama writes all five sections from that, and the prompt carries what this
// site actually holds on the subject: how many image, video and audio models
// are tracked, how many tools, how many intellectual property lawsuits are
// live, and how many glossary terms exist. That is the difference between a
// page of opinion and a page of the site's own record, which is the standard
// the rest of theworldofai.org is held to. A reading is cached on a hash of
// the seed plus those figures, so it is written once and rewritten only when
// the seed or the data moves.
//
// WHAT IT REFUSES. A topic with no expansion yet publishes with whatever the
// editor row holds and is marked so; it is never padded. The stage never
// writes a figure the query did not return.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// twoaiArtCap bounds the model work inside the daily build, where the whole
// build has a deadline. Run on its own (pipeline.exe twoai_art) the stage
// takes twoaiArtCapAlone instead, so a new section can be written in one sitting
// rather than over ten days. Stephen, 2026-09-22.
var twoaiArtCap = 18

const twoaiArtCapAlone = 400

// A section's pages are written to the content folder its category route
// reads: the ecosystem-entities route sweeps content/ecosystem, the
// enterprise route sweeps content/industries.
var twoaiArtDirFor = map[string]string{
	"ecosystem-entities-market-and-operations":     "ecosystem",
	"enterprise-applications-governance-and-tools": "industries",
}

const twoaiArtDefaultCategory = "ecosystem-entities-market-and-operations"

type twoaiArtNode struct {
	Slug, Kind, Parent, Name           string
	Sort                               int
	Blurb                              string
	Scope, Infra, Method, Gov, Horizon string
	Section, Category                  string
	// Source is the foundation text a page is written from, when a section is
	// built on a book (twoai_art_sources). AI in Education, 2026-09-29, is
	// built on Stephen's Volume X, The AI Ready School.
	Source string
}

// twoaiArtFacts is what the site knows that bears on creative AI. Everything
// here is a count of rows this database holds, read fresh each run.
type twoaiArtFacts struct {
	ImageModels, VideoModels, AudioModels, Tools int
	IPCases, MusicCases                          int
	GlossaryTerms                                int
	AllCases, Compliance, CaseLaw, Bills         int
	SECCos, MAFilings, Tickers                   int
	MedModels, SciModels, PLCases                int
	Papers, Claims, BookTitles                   int
	DBMCP, MCPTotal                              int
}

func twoaiArtReadFacts(db *sql.DB) twoaiArtFacts {
	var f twoaiArtFacts
	// One row of counts. If any sub-select is wrong the whole row fails, so the
	// error is printed rather than swallowed: a section written against zeroes
	// would read as a site that holds nothing.
	if err := db.QueryRow(`SELECT
		(SELECT count(*) FROM twoai_model_catalog WHERE section = 'image-generation-models' AND delisted_at IS NULL),
		(SELECT count(*) FROM twoai_model_catalog WHERE section = 'video-models' AND delisted_at IS NULL),
		(SELECT count(*) FROM twoai_model_catalog WHERE section IN ('audio-speech-models','music-models') AND delisted_at IS NULL),
		(SELECT count(*) FROM synced_tools),
		(SELECT count(*) FROM ai_lawsuits WHERE is_active AND category = 'intellectual property'),
		(SELECT count(*) FROM ai_lawsuits WHERE is_active AND (lower(case_name) LIKE '%suno%' OR lower(case_name) LIKE '%uncharted%' OR lower(defendants) LIKE '%suno%')),
		(SELECT jsonb_array_length(data->'terms') FROM site_content WHERE path = 'resources/glossary.json'),
		(SELECT count(*) FROM ai_lawsuits WHERE is_active),
		(SELECT count(*) FROM twoai_pages WHERE path LIKE 'compliance/%'),
		(SELECT count(*) FROM twoai_pages WHERE path LIKE 'caselaw/%'),
		(SELECT count(*) FROM pipeline.documents d JOIN pipeline.sources s ON s.id = d.source_id WHERE s.name LIKE 'LegiScan%'),
		(SELECT count(*) FROM twoai_pages WHERE path LIKE 'companies/%'),
		(SELECT count(*) FROM twoai_ma_filings),
		(SELECT count(*) FROM twoai_stock_instruments),
		(SELECT count(*) FROM twoai_model_catalog WHERE section = 'medical-models' AND delisted_at IS NULL),
		(SELECT count(*) FROM twoai_model_catalog WHERE section = 'scientific-models' AND delisted_at IS NULL),
		(SELECT count(*) FROM ai_lawsuits WHERE is_active AND category LIKE 'product liability%'),
		(SELECT count(*) FROM twoai_research_papers),
		(SELECT count(*) FROM twoai_claims),
		(SELECT count(*) FROM twoai_book_catalog)`).
		Scan(&f.ImageModels, &f.VideoModels, &f.AudioModels, &f.Tools, &f.IPCases, &f.MusicCases, &f.GlossaryTerms,
			&f.AllCases, &f.Compliance, &f.CaseLaw, &f.Bills, &f.SECCos, &f.MAFilings, &f.Tickers,
			&f.MedModels, &f.SciModels, &f.PLCases, &f.Papers, &f.Claims, &f.BookTitles); err != nil {
		fmt.Println("twoai_art: facts:", err)
	}
	// AI and SQL, 2026-09-24: the database MCP servers this site tracks, by
	// the engines their pages name.
	db.QueryRow(`SELECT count(*) FILTER (WHERE data::text ~* '\m(postgres|postgresql|mysql|sqlite|sql server|snowflake|bigquery|clickhouse|duckdb|supabase)\M'),
		count(*) FROM twoai_pages WHERE path LIKE 'mcp/%' AND path NOT LIKE 'mcp/index%'`).Scan(&f.DBMCP, &f.MCPTotal)
	return f
}

// line is what the model is told this site holds, as {{token}} placeholders
// rather than figures (theworldofai row 467, Stephen 2026-10-05: no count of
// the site's own data is typed). The model copies a placeholder where it uses
// a figure, and the publisher fills it from the live data on every run, so a
// reading written once never freezes a count. The phrasing matches
// numericLine, the form readings were written under before, which
// twoaiArtTokenMigrate recognises.
func (f twoaiArtFacts) line(section string) string {
	const copyRule = " Every figure above is a placeholder in double braces: where you use one, copy the placeholder exactly, braces included, and never write a number in its place."
	switch section {
	case "sql":
		return "This site currently tracks {{art_mcp_total}} active Model Context Protocol servers, {{art_db_mcp}} of them for SQL databases and warehouses, {{art_papers}} research papers in its library, {{art_tools}} AI tools and {{glossary_terms}} glossary terms. Every page on this site is itself built from a PostgreSQL database." + copyRule
	case "edu":
		return "This site currently holds {{art_compliance_pages}} compliance and regulation pages (student privacy law among them), {{art_papers}} research papers in its library, {{art_tools}} AI tools and {{glossary_terms}} glossary terms." + copyRule
	case "eco":
		return "This site currently tracks {{art_tickers}} listed AI-related instruments with daily prices, {{art_ma_filings}} merger and acquisition filings, {{art_company_pages}} company pages, {{art_all_cases}} active AI lawsuits, {{art_compliance_pages}} compliance and regulation pages, {{art_tools}} AI tools and {{glossary_terms}} glossary terms." + copyRule
	case "res":
		return "This site currently holds {{art_papers}} research papers in its library, {{art_claims}} claims extracted from research works, {{art_book_titles}} AI books in its catalogue, {{art_sci_models}} scientific models, {{art_tools}} AI tools and {{glossary_terms}} glossary terms. Content on this site never links to the Consensus search tool; it links to the original paper." + copyRule
	case "med":
		return "This site currently tracks {{art_med_models}} medical AI models, {{art_sci_models}} scientific models, {{art_pl_cases}} active product liability and wrongful death lawsuits against AI companies, {{art_compliance_pages}} compliance and regulation pages, {{art_tools}} AI tools and {{glossary_terms}} glossary terms." + copyRule
	case "fin":
		return "This site currently tracks {{art_company_pages}} company pages, {{art_ma_filings}} merger and acquisition filings, {{art_tickers}} listed AI-related instruments, {{art_compliance_pages}} compliance and regulation pages, {{art_all_cases}} active AI lawsuits, {{art_tools}} AI tools and {{glossary_terms}} glossary terms." + copyRule
	case "law":
		return "This site currently tracks {{art_all_cases}} active AI lawsuits ({{art_ip_cases}} of them intellectual property), {{art_caselaw}} AI case law precedents, {{art_compliance_pages}} compliance and regulation pages, {{art_bills}} state AI bills, {{art_tools}} AI tools and {{glossary_terms}} glossary terms." + copyRule
	}
	return "This site currently tracks {{art_image_models}} live image generation models, {{art_video_models}} video models, {{art_audio_models}} audio models, {{art_tools}} AI tools, {{art_ip_cases}} active intellectual property lawsuits (of which {{art_music_cases}} involve AI music services), and {{glossary_terms}} glossary terms." + copyRule
}

// numericLine is what the model was told before row 467, with the figures
// written in. Each section gets the
// figures that bear on it, because a number that does not belong on the page
// is a number the model will reach for anyway.
func (f twoaiArtFacts) numericLine(section string) string {
	if section == "sql" {
		return fmt.Sprintf("This site currently tracks %d active Model Context Protocol servers, %d of them for SQL databases and warehouses, %d research papers in its library, %d AI tools and %d glossary terms. Every page on this site is itself built from a PostgreSQL database.",
			f.MCPTotal, f.DBMCP, f.Papers, f.Tools, f.GlossaryTerms)
	}
	if section == "edu" {
		return fmt.Sprintf("This site currently holds %d compliance and regulation pages (student privacy law among them), %d research papers in its library, %d AI tools and %d glossary terms.",
			f.Compliance, f.Papers, f.Tools, f.GlossaryTerms)
	}
	if section == "eco" {
		return fmt.Sprintf("This site currently tracks %d listed AI-related instruments with daily prices, %d merger and acquisition filings, %d company pages, %d active AI lawsuits, %d compliance and regulation pages, %d AI tools and %d glossary terms.",
			f.Tickers, f.MAFilings, f.SECCos, f.AllCases, f.Compliance, f.Tools, f.GlossaryTerms)
	}
	if section == "res" {
		return fmt.Sprintf("This site currently holds %d research papers in its library, %d claims extracted from research works, %d AI books in its catalogue, %d scientific models, %d AI tools and %d glossary terms. Content on this site never links to the Consensus search tool; it links to the original paper.",
			f.Papers, f.Claims, f.BookTitles, f.SciModels, f.Tools, f.GlossaryTerms)
	}
	if section == "med" {
		return fmt.Sprintf("This site currently tracks %d medical AI models, %d scientific models, %d active product liability and wrongful death lawsuits against AI companies, %d compliance and regulation pages, %d AI tools and %d glossary terms.",
			f.MedModels, f.SciModels, f.PLCases, f.Compliance, f.Tools, f.GlossaryTerms)
	}
	if section == "fin" {
		return fmt.Sprintf("This site currently tracks %d company pages, %d merger and acquisition filings, %d listed AI-related instruments, %d compliance and regulation pages, %d active AI lawsuits, %d AI tools and %d glossary terms.",
			f.SECCos, f.MAFilings, f.Tickers, f.Compliance, f.AllCases, f.Tools, f.GlossaryTerms)
	}
	if section == "law" {
		return fmt.Sprintf("This site currently tracks %d active AI lawsuits (%d of them intellectual property), %d AI case law precedents, %d compliance and regulation pages, %d state AI bills, %d AI tools and %d glossary terms.",
			f.AllCases, f.IPCases, f.CaseLaw, f.Compliance, f.Bills, f.Tools, f.GlossaryTerms)
	}
	return fmt.Sprintf("This site currently tracks %d live image generation models, %d video models, %d audio models, %d AI tools, %d active intellectual property lawsuits (of which %d involve AI music services), and %d glossary terms.",
		f.ImageModels, f.VideoModels, f.AudioModels, f.Tools, f.IPCases, f.MusicCases, f.GlossaryTerms)
}

// twoaiArtHubSystem writes the landing pages. A hub that is a heading and ten
// links is a thin page, which is the thing this site refuses to publish, so
// each hub and sub-hub gets an opening that says what the field is, what is
// actually changing in it, and how its pages fit together.
const twoaiArtHubSystem = `You write the opening of a reference section for The World of AI, an atlas of artificial intelligence.

You are given the section name, what it covers, and the pages under it. Write three paragraphs of four to six sentences each:
- what: what this field is and what artificial intelligence is actually doing in it now, not in theory.
- state: where the work stands, what is solved, what is not, and what the honest limits are.
- map: how the pages listed below fit together and what a reader would go to each for. Name them inside your own sentences rather than listing them.

Rules:
- Plain English. Commas, not dashes. No em dashes. No marketing language, no "in today's landscape", no exclamation.
- Use a figure from the site facts ONLY where it genuinely belongs. Never invent a number, a company, a product version, a case name or a date.
- Nothing here is advice, legal, financial or medical. Describe the work. On medical topics, never tell a reader what to do about their own health.

Answer with one JSON object and nothing else:
{"what": "", "state": "", "map": ""}`

const twoaiArtSystem = `You write reference pages for The World of AI, an atlas of artificial intelligence.

You are given a topic, the field it sits in, and sometimes short seed lines from the site's editor. Write ONE paragraph of three to five sentences for each of the five sections, in this order: scope, what it runs on, how the work is done, rights and risk and provenance, where it is going.

Rules:
- Where a seed line is given, keep its meaning: do not change the subject or contradict it. Where none is given, write the section yourself from what the topic plainly is.
- Plain English. Commas, not dashes. No em dashes. No marketing language, no "in today's landscape", no exclamation.
- Use a figure from the site facts ONLY where it genuinely belongs. Never invent a number, a company, a product version, a case name or a date.
- Name tools only where the seed names them or where the tool is unambiguous and well known.
- Write for a working professional in the field who is competent but not a machine learning engineer. Nothing here is legal, financial, investment or medical advice: describe practice, never advise a reader, never recommend a security, trade or allocation, and on medical topics never tell a reader what to do about their own health or treatment.

Answer with one JSON object and nothing else:
{"scope": "", "infra": "", "method": "", "governance": "", "horizon": ""}`

// eduChapterPapers: the research behind one chapter of The AI Ready School,
// from srj_edu_research (Stephen loaded 350 papers on 2026-09-29, each
// tagged with the chapter it speaks to and a key finding in our own words).
// Only the finding, the caveat and the citation record are published; the
// link goes to the paper's own home (DOI, then the paper's URL), never to an
// aggregator, and abstracts and full text are never reproduced.
type eduPaper struct {
	Title    string `json:"title"`
	Venue    string `json:"venue,omitempty"`
	Year     int    `json:"year,omitempty"`
	URL      string `json:"url,omitempty"`
	Finding  string `json:"finding,omitempty"`
	Why      string `json:"why,omitempty"`
	Caveat   string `json:"caveat,omitempty"`
	Theme    string `json:"theme,omitempty"`
	Cites    int    `json:"citations,omitempty"`
	ReadDeep string `json:"read_depth,omitempty"`
}

func eduChapterPapers(db *sql.DB, chapter int) []eduPaper {
	rows, err := db.Query(`SELECT title, COALESCE(venue,''), COALESCE(pub_year,0),
			CASE WHEN COALESCE(doi,'') <> '' THEN 'https://doi.org/' || regexp_replace(doi, '^https?://(dx\.)?doi\.org/', '')
			     WHEN COALESCE(url,'') <> '' AND url NOT ILIKE '%consensus.app%' THEN url ELSE '' END,
			COALESCE(key_finding,''), COALESCE(why_it_matters,''), COALESCE(caveat,''), COALESCE(theme,''),
			COALESCE(citations_at_capture,0), COALESCE(read_depth,'')
		FROM srj_edu_research
		WHERE COALESCE(theme,'') <> 'low-k12-relevance' AND COALESCE(key_finding,'') <> ''
		  AND $1 = ANY (regexp_split_to_array(COALESCE(book_chapter,''), '\s*,\s*'))
		ORDER BY citations_at_capture DESC NULLS LAST, pub_year DESC`, strconv.Itoa(chapter))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []eduPaper
	for rows.Next() {
		var p eduPaper
		if rows.Scan(&p.Title, &p.Venue, &p.Year, &p.URL, &p.Finding, &p.Why, &p.Caveat, &p.Theme, &p.Cites, &p.ReadDeep) == nil {
			out = append(out, p)
		}
	}
	return out
}

func eduChapterOf(slug string) int {
	if len(slug) == 8 && strings.HasPrefix(slug, "edu-ch") {
		n, _ := strconv.Atoi(slug[6:])
		return n
	}
	return 0
}

// Book-built sections. The page is written from the foundation text alone:
// the book is the source, the model is the writer, and nothing the text does
// not support goes on the page.
const twoaiArtEduSystem = `You write reference pages for The World of AI, an atlas of artificial intelligence, in its AI in Education section.

You are given one idea from the book The AI Ready School (Volume X of The Operating Discipline for AI Library, by Stephen R. Jordan) and the book's own text for it. Write a web page from that text for teachers, school leaders and parents.

Return five parts:
- answer: two or three sentences that stand alone as the complete answer to what this idea is and why it matters.
- idea: one paragraph of four to five sentences on what the idea is and the problem it solves.
- practice: one paragraph of four to five sentences on how it works in a school or classroom, by grade band where the text gives them.
- evidence: one paragraph on what the evidence says, keeping the book's own evidence label and any study, figure or finding exactly as the text states it. If the text gives no evidence, say plainly that this idea rests on practice rather than research.
- guardrails: one paragraph on the limits, risks and the decisions that stay with a person, as the text sets them out.

Rules:
- Use only the text supplied. Do not add studies, numbers, products, laws or dates it does not contain.
- Write in your own words. Do not copy sentences from the text.
- Full, readable editorial prose, never notes or slogans. Plain English. Commas, not dashes. No em dashes. No marketing language. No paragraph longer than five sentences.
- Nothing here is legal advice; describe practice.

Answer with one JSON object and nothing else:
{"answer": "", "idea": "", "practice": "", "evidence": "", "guardrails": ""}`

const twoaiArtEduHubSystem = `You write the opening of a section of The World of AI's AI in Education pages, built on the book The AI Ready School (Volume X of The Operating Discipline for AI Library, by Stephen R. Jordan).

You are given the section name, the book's own text that introduces it, and the pages under it. Write three paragraphs of four to five sentences each:
- what: what this part of school life is and what AI is actually doing in it, as the book describes it.
- state: what the book finds works, what does not, and the rule it holds to.
- map: how the pages listed below fit together and what a teacher, leader or parent would go to each for. Name them inside your own sentences rather than listing them.

Rules:
- Use only the text supplied. Do not add studies, numbers, products, laws or dates it does not contain. Write in your own words; do not copy sentences.
- Plain English. Commas, not dashes. No em dashes. No marketing language.

Answer with one JSON object and nothing else:
{"what": "", "state": "", "map": ""}`

// twoaiArtJSON pulls the object out of a reply that may still carry a fence or
// a sentence around it.
func twoaiArtJSON(raw string) (map[string]string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```")
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 {
		s = s[:j+1]
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// twoaiArtHash decides when a page is rewritten. It covers the page's own
// seed and the site figures handed to the model, with every figure rounded to
// two significant figures first. Before 2026-09-22 the figures went in raw, so
// a single new glossary term or lawsuit changed the hash of every page in a
// section and the daily build would have rewritten all 366 pages forever,
// twelve at a time. Rounded, a page is rewritten when a figure it was given
// moves by roughly a tenth, which is when its text could be wrong.
func twoaiArtHash(n twoaiArtNode, facts string) string {
	facts = twoaiArtNumRe.ReplaceAllStringFunc(facts, twoaiArtRound)
	parts := []string{n.Name, n.Scope, n.Infra, n.Method, n.Gov, n.Horizon, facts}
	if n.Source != "" {
		parts = append(parts, n.Source)
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])[:16]
}

var twoaiArtNumRe = regexp.MustCompile(`\d+`)

// twoaiArtLegacyHash is the hash as it was computed before figures were
// rounded, kept only to recognise pages written under it.
func twoaiArtLegacyHash(n twoaiArtNode, facts string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{n.Name, n.Scope, n.Infra, n.Method, n.Gov, n.Horizon, facts}, "|")))
	return hex.EncodeToString(h[:])[:16]
}

// twoaiArtRound rounds an integer to two significant figures: 92 stays 92,
// 732 becomes 730, 6789 becomes 6800.
func twoaiArtRound(s string) string {
	v, err := strconv.Atoi(s)
	if err != nil || v < 100 {
		return s
	}
	p := 1
	for v/p >= 100 {
		p *= 10
	}
	return strconv.Itoa(((v + p/2) / p) * p)
}

// twoaiArtStale says a page is due for rewriting because of its age alone:
// the field it describes has moved on even if none of its figures have.
const twoaiArtMaxAgeDays = 90

func twoaiArtExpand(db *sql.DB, nodes []twoaiArtNode, facts twoaiArtFacts) (int, int) {
	written, failed := 0, 0
	parentName := map[string]string{}
	rootFor := map[string]string{}
	for _, p := range nodes {
		parentName[p.Slug] = p.Name
		if p.Kind == "hub" {
			rootFor[p.Section] = p.Slug
		}
	}
	// The landing pages come first. A hub or sub-hub that is a heading and a
	// list of links is exactly the thin page this site refuses to publish, so
	// its opening is written before any more topics are.
	kidsOf := map[string][]string{}
	for _, c := range nodes {
		if c.Parent != "" {
			kidsOf[c.Parent] = append(kidsOf[c.Parent], c.Name)
		}
	}
	// Row 467: readings written with typed counts move to tokens first.
	twoaiArtTokenMigrate(db, nodes, facts, kidsOf)
	for _, n := range nodes {
		if n.Kind == "topic" || written >= twoaiArtCap {
			continue
		}
		hubExtra := facts.line(n.Section) + strings.Join(kidsOf[n.Slug], ",")
		if ch := eduChapterOf(n.Slug); ch > 0 {
			for _, p := range eduChapterPapers(db, ch) {
				hubExtra += "|" + p.Title
			}
		}
		want := twoaiArtHash(n, hubExtra)
		var have, haveModel string
		var ageDays int
		db.QueryRow(`SELECT data_hash, model, current_date - generated_on FROM twoai_art_readings WHERE slug = $1 AND block = 'hub'`, n.Slug).Scan(&have, &haveModel, &ageDays)
		// An opening written by hand is never overwritten by the model.
		if haveModel == "curated" {
			continue
		}
		if have != want && have == twoaiArtLegacyHash(n, hubExtra) {
			db.Exec(`UPDATE twoai_art_readings SET data_hash = $2 WHERE slug = $1 AND block = 'hub'`, n.Slug, want)
			have = want
		}
		if have == want && ageDays < twoaiArtMaxAgeDays {
			continue
		}
		user := fmt.Sprintf("Section: %s\nPart of: %s\nWhat it covers: %s\nSite facts: %s\n\nPages under it:\n%s\n\nAnswer now.",
			n.Name, parentName[rootFor[n.Section]], n.Blurb, facts.line(n.Section),
			"- "+strings.Join(kidsOf[n.Slug], "\n- "))
		hubSys := twoaiArtHubSystem
		if n.Source != "" {
			hubSys = twoaiArtEduHubSystem
			research := ""
			if ch := eduChapterOf(n.Slug); ch > 0 {
				var rb strings.Builder
				for i, p := range eduChapterPapers(db, ch) {
					if i >= 20 {
						break
					}
					fmt.Fprintf(&rb, "- %s (%s, %d): %s\n", p.Title, p.Venue, p.Year, p.Finding)
				}
				if rb.Len() > 0 {
					research = "\n\nResearch this site holds for this chapter, each with its key finding in our words (use these as evidence where they fit; cite by title, never invent):\n" + rb.String()
				}
			}
			user = fmt.Sprintf("Section: %s\nPart of: %s\n\nThe book's text for this section:\n%s%s\n\nPages under it:\n%s\n\nAnswer now.",
				n.Name, parentName[rootFor[n.Section]], trunc(n.Source, 9000), research, "- "+strings.Join(kidsOf[n.Slug], "\n- "))
		}
		out, model, err := twoaiGenerate("art", hubSys, user)
		if err != nil {
			failed++
			fmt.Printf("twoai_art: %s: %v\n", n.Slug, err)
			continue
		}
		got, perr := twoaiArtJSON(out)
		if perr != nil || len(got["what"]) < 120 || len(got["map"]) < 120 {
			failed++
			fmt.Printf("twoai_art: %s: opening not usable, nothing written\n", n.Slug)
			continue
		}
		b, _ := json.Marshal(got)
		if _, err := db.Exec(`INSERT INTO twoai_art_readings (slug, block, body, data_hash, model, generated_on)
			VALUES ($1,'hub',$2,$3,$4,current_date)
			ON CONFLICT (slug, block) DO UPDATE SET body = EXCLUDED.body, data_hash = EXCLUDED.data_hash,
				model = EXCLUDED.model, generated_on = current_date`, n.Slug, string(b), want, model); err != nil {
			failed++
			continue
		}
		written++
	}
	// Topics are taken one section at a time in turn, so a new section is not
	// left waiting behind every topic of an older one. Before 2026-09-22 they
	// ran in table order and the fifty law and fifty finance topics sat behind
	// the fifty art topics.
	var ordered []twoaiArtNode
	{
		bySection := map[string][]twoaiArtNode{}
		var sections []string
		for _, n := range nodes {
			if n.Kind != "topic" {
				continue
			}
			if _, ok := bySection[n.Section]; !ok {
				sections = append(sections, n.Section)
			}
			bySection[n.Section] = append(bySection[n.Section], n)
		}
		for i := 0; ; i++ {
			added := false
			for _, s := range sections {
				if i < len(bySection[s]) {
					ordered = append(ordered, bySection[s][i])
					added = true
				}
			}
			if !added {
				break
			}
		}
	}
	for _, n := range ordered {
		if written >= twoaiArtCap {
			break
		}
		want := twoaiArtHash(n, facts.line(n.Section))
		var have, haveModel string
		var ageDays int
		db.QueryRow(`SELECT data_hash, model, current_date - generated_on FROM twoai_art_readings WHERE slug = $1 AND block = 'all'`, n.Slug).Scan(&have, &haveModel, &ageDays)
		if haveModel == "curated" {
			continue
		}
		// Pages written before the figures were rounded carry the old hash. They
		// are current, so the hash is brought forward rather than the page
		// rewritten.
		if have != want && have == twoaiArtLegacyHash(n, facts.line(n.Section)) {
			db.Exec(`UPDATE twoai_art_readings SET data_hash = $2 WHERE slug = $1 AND block = 'all'`, n.Slug, want)
			have = want
		}
		if have == want && ageDays < twoaiArtMaxAgeDays {
			continue
		}
		seeds := "(none: write all five sections yourself)"
		if strings.TrimSpace(n.Scope+n.Infra+n.Method+n.Gov+n.Horizon) != "" {
			seeds = fmt.Sprintf("scope: %s\ninfra: %s\nmethod: %s\ngovernance: %s\nhorizon: %s",
				n.Scope, n.Infra, n.Method, n.Gov, n.Horizon)
		}
		user := fmt.Sprintf("Topic: %s\nField: %s, part of %s\nSite facts: %s\n\nEditor seed lines:\n%s\n\nAnswer now.",
			n.Name, parentName[n.Parent], parentName[rootFor[n.Section]], facts.line(n.Section), seeds)
		topicSys := twoaiArtSystem
		if n.Source != "" {
			topicSys = twoaiArtEduSystem
			user = fmt.Sprintf("Idea: %s\nChapter: %s\n\nThe book's text for this idea:\n%s\n\nAnswer now.",
				n.Name, parentName[n.Parent], trunc(n.Source, 9000))
		}
		out, model, err := twoaiGenerate("art", topicSys, user)
		if err != nil {
			failed++
			fmt.Printf("twoai_art: %s: %v\n", n.Slug, err)
			continue
		}
		s := strings.TrimSpace(out)
		s = strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		if i := strings.Index(s, "{"); i > 0 {
			s = s[i:]
		}
		if j := strings.LastIndex(s, "}"); j >= 0 {
			s = s[:j+1]
		}
		got, jerr := twoaiArtJSON(s)
		if jerr != nil {
			failed++
			fmt.Printf("twoai_art: %s: unreadable answer: %v\n", n.Slug, jerr)
			continue
		}
		if n.Source != "" {
			if len(got["answer"]) < 80 || len(got["idea"]) < 120 || len(got["practice"]) < 120 || len(got["guardrails"]) < 80 {
				failed++
				fmt.Printf("twoai_art: %s: answer too short, nothing written\n", n.Slug)
				continue
			}
		} else if len(got["scope"]) < 80 || len(got["governance"]) < 80 {
			failed++
			fmt.Printf("twoai_art: %s: answer too short, nothing written\n", n.Slug)
			continue
		}
		b, _ := json.Marshal(got)
		if _, err := db.Exec(`INSERT INTO twoai_art_readings (slug, block, body, data_hash, model, generated_on)
			VALUES ($1,'all',$2,$3,$4,current_date)
			ON CONFLICT (slug, block) DO UPDATE SET body = EXCLUDED.body, data_hash = EXCLUDED.data_hash,
				model = EXCLUDED.model, generated_on = current_date`, n.Slug, string(b), want, model); err != nil {
			failed++
			continue
		}
		written++
	}
	return written, failed
}

func twoaiArt(db *sql.DB, today string) error {
	rows, err := db.Query(`SELECT slug, kind, COALESCE(parent_slug,''), sort, name, COALESCE(blurb,''),
		COALESCE(scope,''), COALESCE(infra,''), COALESCE(method,''), COALESCE(governance,''), COALESCE(horizon,''),
		COALESCE(section,'art'), COALESCE(category,'')
		FROM twoai_art_nodes WHERE status = 'live' ORDER BY section, kind, sort`)
	if err != nil {
		return err
	}
	var nodes []twoaiArtNode
	for rows.Next() {
		var n twoaiArtNode
		if rows.Scan(&n.Slug, &n.Kind, &n.Parent, &n.Sort, &n.Name, &n.Blurb,
			&n.Scope, &n.Infra, &n.Method, &n.Gov, &n.Horizon, &n.Section, &n.Category) == nil {
			if n.Category == "" {
				n.Category = twoaiArtDefaultCategory
			}
			nodes = append(nodes, n)
		}
	}
	rows.Close()
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_art_sources (slug text PRIMARY KEY, source_title text, source_text text NOT NULL, book text, updated_at timestamptz DEFAULT now())`)
	srcOf := map[string]string{}
	if sr, serr := db.Query(`SELECT slug, source_text FROM twoai_art_sources`); serr == nil {
		for sr.Next() {
			var k, v string
			if sr.Scan(&k, &v) == nil {
				srcOf[k] = v
			}
		}
		sr.Close()
	}
	for i := range nodes {
		nodes[i].Source = srcOf[nodes[i].Slug]
	}
	if len(nodes) == 0 {
		fmt.Println("twoai_art: no nodes, nothing to build")
		return nil
	}

	facts := twoaiArtReadFacts(db)
	written, failed := twoaiArtExpand(db, nodes, facts)

	readings := map[string]map[string]string{}
	hubReadings := map[string]map[string]string{}
	rrows, err := db.Query(`SELECT slug, block, body FROM twoai_art_readings WHERE block IN ('all','hub')`)
	if err == nil {
		for rrows.Next() {
			var slug, block, body string
			if rrows.Scan(&slug, &block, &body) == nil {
				var m map[string]string
				if json.Unmarshal([]byte(body), &m) == nil {
					if block == "hub" {
						hubReadings[slug] = m
					} else {
						readings[slug] = m
					}
				}
			}
		}
		rrows.Close()
	}
	// A child's line in a list is the first sentence of its own scope, so a
	// landing page says what each page under it is about instead of listing
	// names. Written once the child has been written, never invented here.
	firstSentence := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" {
			return ""
		}
		if i := strings.Index(s, ". "); i > 40 {
			return s[:i+1]
		}
		if len(s) > 220 {
			return s[:220] + "..."
		}
		return s
	}

	uid := func(slug string) string { return twoaiUID("art:" + slug) }
	// A page's address is its own category's, and the root of a section is the
	// row with no parent in that section.
	catOf := map[string]string{}
	rootOf := map[string]string{}
	rootName := map[string]string{}
	for _, n := range nodes {
		catOf[n.Slug] = n.Category
		if n.Kind == "hub" {
			rootOf[n.Section] = n.Slug
			rootName[n.Section] = n.Name
		}
	}
	path := func(slug string) string {
		cat := catOf[slug]
		if cat == "" {
			cat = twoaiArtDefaultCategory
		}
		return "/ai-ecosystem/" + cat + "/" + uid(slug) + "/"
	}
	fileFor := func(n twoaiArtNode) string {
		dir := twoaiArtDirFor[n.Category]
		if dir == "" {
			dir = "ecosystem"
		}
		return dir + "/" + n.Section + "-" + n.Slug + ".json"
	}
	type kid struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		Blurb string `json:"blurb,omitempty"`
	}

	pages := 0
	write := func(file, tax string, doc map[string]any) error {
		b, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, url_count, updated_at)
			VALUES ($1,$2,$3,$4::jsonb,1,now())
			ON CONFLICT (path) DO UPDATE SET kind = EXCLUDED.kind, taxonomy_slug = EXCLUDED.taxonomy_slug,
				data = EXCLUDED.data, url_count = 1, updated_at = now()`,
			file, doc["shape"], tax, string(b)); err != nil {
			return err
		}
		pages++
		return nil
	}
	// Each section has its own taxonomy row, so the category page lists it and
	// the freshness contract can find its pages.
	taxFor := map[string]string{"art": "ai-art", "law": "ai-lawyer", "fin": "ai-accountant", "med": "ai-physician", "res": "ai-researcher", "eco": "ai-economist", "fut": "ai-future-professions", "sql": "ai-sql", "edu": "ai-education"}

	// BREADCRUMBS. Stephen, 2026-09-22, on Financial Reporting and Synthesis:
	// the trail stopped at the category, so a reader three levels down could
	// not see they were inside The AI Accountant, inside Knowledge Based
	// Professions and their Future. Each page now carries its full chain of
	// ancestors, nearest last, walked through parent_slug.
	byNode := map[string]twoaiArtNode{}
	for _, x := range nodes {
		byNode[x.Slug] = x
	}
	crumbsFor := func(n twoaiArtNode) []kid {
		var chain []kid
		seen := map[string]bool{n.Slug: true}
		for p := n.Parent; p != "" && !seen[p]; {
			seen[p] = true
			pn, ok := byNode[p]
			if !ok {
				break
			}
			chain = append([]kid{{Name: pn.Name, Path: path(pn.Slug)}}, chain...)
			p = pn.Parent
		}
		return chain
	}

	for _, n := range nodes {
		switch n.Kind {
		case "hub", "subhub":
			var kids []kid
			for _, c := range nodes {
				if c.Parent == n.Slug {
					blurb := c.Blurb
					if blurb == "" {
						blurb = c.Scope
					}
					if blurb == "" {
						if r := readings[c.Slug]; r != nil && r["answer"] != "" {
							blurb = firstSentence(r["answer"])
						} else if r := readings[c.Slug]; r != nil {
							blurb = firstSentence(r["scope"])
						} else if h := hubReadings[c.Slug]; h != nil {
							blurb = firstSentence(h["what"])
						}
					}
					kids = append(kids, kid{Name: c.Name, Path: path(c.Slug), Blurb: blurb})
				}
			}
			// A hub whose children are themselves sections, such as Knowledge
			// Based Professions and their Future, lists them alphabetically.
			// Stephen, 2026-09-22. Sub-hubs and topics keep their editorial order.
			allHubs := len(kids) > 0
			for _, c := range nodes {
				if c.Parent == n.Slug && c.Kind != "hub" {
					allHubs = false
				}
			}
			if allHubs {
				sort.SliceStable(kids, func(i, j int) bool {
					return strings.TrimPrefix(kids[i].Name, "The ") < strings.TrimPrefix(kids[j].Name, "The ")
				})
			}
			h := hubReadings[n.Slug]
			var opening []map[string]string
			if h != nil && h["s1"] != "" {
				// A hand-written opening carries its own headings, t1..t9 with
				// bodies s1..s9, in order.
				for i := 1; i <= 9; i++ {
					b := h[fmt.Sprintf("s%d", i)]
					if b == "" {
						continue
					}
					opening = append(opening, map[string]string{"heading": h[fmt.Sprintf("t%d", i)], "body": b})
				}
			} else if h != nil {
				if h["what"] != "" {
					opening = append(opening, map[string]string{"heading": "What this covers", "body": h["what"]})
				}
				if h["state"] != "" {
					opening = append(opening, map[string]string{"heading": "Where the work stands", "body": h["state"]})
				}
				if h["map"] != "" {
					opening = append(opening, map[string]string{"heading": "How these pages fit together", "body": h["map"]})
				}
			}
			doc := map[string]any{
				"uid": uid(n.Slug), "slug": n.Slug, "shape": "art-hub", "category": n.Category,
				"name": n.Name, "title": n.Name, "blurb": n.Blurb, "answer": n.Blurb,
				"children": kids, "sections": opening, "expanded": h != nil,
				// A page still waiting for its opening is live and linked, and out
				// of the search index until it has something to say.
				"noindex":     h == nil,
				"child_count": len(kids), "generated": today, "refresh_every_days": 90,
				"hub_name": rootName[n.Section], "crumbs": crumbsFor(n),
			}
			if n.Kind == "subhub" {
				// The parent is the row's own parent, so a sub-hub nested in
				// another (a chapter inside a part, AI in Education) points up
				// one level, not straight to the section root.
				root := n.Parent
				if root == "" {
					root = rootOf[n.Section]
				}
				for _, p := range nodes {
					if p.Slug == root {
						doc["parent_name"] = p.Name
					}
				}
				doc["parent_path"] = path(root)
			}
			if n.Section == "edu" {
				doc["book"] = map[string]string{"title": "The AI Ready School", "volume": "Volume X of The Operating Discipline for AI Library", "author": "Stephen R. Jordan"}
				if ch := eduChapterOf(n.Slug); ch > 0 {
					if papers := eduChapterPapers(db, ch); len(papers) > 0 {
						doc["papers"] = papers
					}
				}
				if n.Kind == "hub" {
					var total, chapters int
					db.QueryRow(`SELECT count(*), count(DISTINCT book_chapter) FROM srj_edu_research WHERE COALESCE(theme,'') <> 'low-k12-relevance' AND COALESCE(key_finding,'') <> ''`).Scan(&total, &chapters)
					doc["research_total"] = total
				}
			}
			if err := write(fileFor(n), taxFor[n.Section], doc); err != nil {
				return err
			}
		case "topic":
			r := readings[n.Slug]
			pick := func(key, seed string) string {
				if r != nil && strings.TrimSpace(r[key]) != "" {
					return r[key]
				}
				return seed
			}
			var parentName, parentPath string
			for _, p := range nodes {
				if p.Slug == n.Parent {
					parentName, parentPath = p.Name, path(p.Slug)
				}
			}
			var siblings []kid
			for _, s := range nodes {
				if s.Parent == n.Parent && s.Slug != n.Slug {
					siblings = append(siblings, kid{Name: s.Name, Path: path(s.Slug)})
				}
			}
			doc := map[string]any{
				"uid": uid(n.Slug), "slug": n.Slug, "shape": "art-topic", "category": n.Category,
				"name": n.Name, "title": n.Name, "answer": pick("scope", n.Scope),
				"sections": []map[string]string{
					{"heading": "Scope", "body": pick("scope", n.Scope)},
					{"heading": "What it runs on", "body": pick("infra", n.Infra)},
					{"heading": "How the work is done", "body": pick("method", n.Method)},
					{"heading": "Rights, risk and provenance", "body": pick("governance", n.Gov)},
					{"heading": "Where it is going", "body": pick("horizon", n.Horizon)},
				},
				"expanded": r != nil, "noindex": r == nil,
				"parent_name": parentName, "parent_path": parentPath,
				"siblings": siblings, "hub_path": path(rootOf[n.Section]),
				"hub_name": rootName[n.Section], "generated": today, "crumbs": crumbsFor(n),
				"refresh_every_days": 90,
			}
			if n.Section == "edu" {
				doc["book"] = map[string]string{"title": "The AI Ready School", "volume": "Volume X of The Operating Discipline for AI Library", "author": "Stephen R. Jordan"}
				if r != nil && r["idea"] != "" {
					doc["answer"] = r["answer"]
					doc["sections"] = []map[string]string{
						{"heading": "The idea", "body": r["idea"]},
						{"heading": "How it works in school", "body": r["practice"]},
						{"heading": "What the evidence says", "body": r["evidence"]},
						{"heading": "Guardrails", "body": r["guardrails"]},
					}
				} else {
					doc["answer"] = n.Blurb
					doc["sections"] = []map[string]string{}
				}
			}
			if err := write(fileFor(n), taxFor[n.Section], doc); err != nil {
				return err
			}
		}
	}

	fmt.Printf("twoai_art: pages=%d topics_expanded_this_run=%d failed=%d topics_with_a_reading=%d ok=true\n",
		pages, written, failed, len(readings))
	return nil
}
