package main

// twoai_industry_hub.go - the Industry Use Cases hub page, and the first
// LLM-analysis loop in the pipeline.
//
// The loop, in order, with the no-fabrication guardrail at each step:
//
//  1. FETCH: the Census Bureau's Business Trends and Outlook Survey
//     national workbook from its stable URL. BTOS asks a representative
//     sample of US firms every two weeks whether they used AI - the
//     closest thing to an official adoption statistic. Parsed with the
//     stdlib (an xlsx is a zip of xml); the AI questions (Q7 current use,
//     Q24 expected use) are extracted with their full period history and
//     stored in twoai_industry_metrics with a content hash.
//
//  2. INTERPRET: when ANTHROPIC_API_KEY is configured and the data hash
//     has changed since the last analysis, ask Claude (default
//     claude-sonnet-4-6, override with TWOAI_ANALYSIS_MODEL) what the
//     numbers mean for the AI industry. The prompt contains ONLY the
//     fetched payload and forbids outside facts. The reply is validated
//     mechanically: every percentage figure in the analysis must appear
//     verbatim in the payload, or the reply is rejected and the previous
//     analysis keeps rendering. Analyses are stored per data-hash, so the
//     text regenerates exactly when the numbers change and never churns
//     in between.
//
//  3. RENDER: industries/index.json - the hub page - carrying the metric
//     series, the labeled analysis (model name and date on the page), the
//     curated cross-industry findings from twoai_industry_findings, and
//     the 21-sector index. If the key is missing the hub still ships with
//     metrics and findings; the analysis block appears when configured.

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const btosURL = "https://www.census.gov/hfp/btos/downloads/National.xlsx"

var twoaiNationalSystem = "You write analysis for theworldofai.org, a sourced AI reference site. " +
	"You are given survey data as JSON. Write what the numbers mean for the AI industry " +
	"in plain English prose: 3 to 4 short paragraphs, no headings, no bullet points, no preamble. " +
	"HARD RULES: use ONLY facts and figures present in the JSON. Do not bring in any outside " +
	"statistic, company, product, or event. Every percentage you mention must appear verbatim " +
	"in the JSON. Never compute, round, sum, subtract, or average figures: a percentage may only " +
	"be stated if those exact characters appear in the JSON. Describe direction and magnitude of change, what the gap between current and " +
	"expected use implies, and what the do-not-know share suggests, strictly from the data given. " +
	"Plain sentences, no hype, commas over dashes."

var twoaiSectorSystem = "You write sector analysis for theworldofai.org, a sourced AI reference site. " +
	"You are given JSON for one industry: its official Census adoption figure when the survey " +
	"covers it, this site's curated points, and source_page_excerpts, the text this site's " +
	"pipeline harvested from each cited source page. The excerpts are your primary material: " +
	"synthesize what these sources collectively show about AI use in this industry, in plain " +
	"English prose, 4 to 6 short paragraphs, no headings, no bullets, no preamble. Attribute " +
	"specific findings to their source organization by name as it appears in the JSON (per the " +
	"Census Bureau, per NRF, and so on). Cover, where the material supports it: measured " +
	"adoption, what AI is deployed for, where vendors and money concentrate, ROI evidence the " +
	"sources report, risks and the regulatory posture, and what a reader deciding whether to " +
	"deploy should take from it. HARD RULES: use ONLY facts in the JSON, name organizations " +
	"only if they appear in the JSON, every percentage must appear verbatim in the JSON, never " +
	"computed, rounded, summed, or averaged from it, and " +
	"where the sources are silent, say less rather than filling in. Plain sentences, no hype, " +
	"commas over dashes."

const btosSectorURL = "https://www.census.gov/hfp/btos/downloads/Sector.xlsx"

// NAICS 2-digit sector for each industry page that BTOS covers. BTOS surveys
// businesses, so defense, government, and pure-public sectors have no row and
// their analysis runs on curated points alone. Where a NAICS sector is
// broader than the page (professional services covers legal and accounting,
// finance covers banking and insurance, other services includes religious
// organizations), the label says so on the page.
var btosNAICS = map[string][2]string{
	"industry-manufacturing":  {"31", "Manufacturing (NAICS 31-33)"},
	"industry-healthcare":     {"62", "Health care and social assistance (NAICS 62)"},
	"industry-retail":         {"44", "Retail trade (NAICS 44-45)"},
	"industry-construction":   {"23", "Construction (NAICS 23)"},
	"industry-legal":          {"54", "Professional, scientific, and technical services (NAICS 54, includes legal)"},
	"industry-accounting":     {"54", "Professional, scientific, and technical services (NAICS 54, includes accounting)"},
	"industry-insurance":      {"52", "Finance and insurance (NAICS 52)"},
	"industry-banking":        {"52", "Finance and insurance (NAICS 52)"},
	"industry-education":      {"61", "Educational services (NAICS 61)"},
	"industry-real-estate":    {"53", "Real estate and rental and leasing (NAICS 53)"},
	"industry-hospitality":    {"72", "Accommodation and food services (NAICS 72)"},
	"industry-energy":         {"22", "Utilities (NAICS 22; oil and gas extraction reports under mining)"},
	"industry-transportation": {"48", "Transportation and warehousing (NAICS 48-49)"},
	"industry-agriculture":    {"11", "Agriculture, forestry, fishing and hunting (NAICS 11)"},
	"industry-mining":         {"21", "Mining, quarrying, and oil and gas extraction (NAICS 21)"},
	"industry-media":          {"51", "Information (NAICS 51)"},
	"industry-sports":         {"71", "Arts, entertainment, and recreation (NAICS 71)"},
	"industry-nonprofits":     {"81", "Other services (NAICS 81, includes grantmaking and civic organizations)"},
	"industry-churches":       {"81", "Other services (NAICS 81, includes religious organizations)"},
}

// ---- minimal xlsx reader (stdlib only) ------------------------------------

type xlsxSheet struct{ rows [][]string }

func readXLSX(data []byte) (map[string]*xlsxSheet, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	read := func(name string) []byte {
		for _, f := range zr.File {
			if f.Name == name {
				rc, err := f.Open()
				if err != nil {
					return nil
				}
				b, _ := io.ReadAll(rc)
				rc.Close()
				return b
			}
		}
		return nil
	}

	// shared strings
	var shared []string
	if b := read("xl/sharedStrings.xml"); b != nil {
		var ss struct {
			SI []struct {
				T string `xml:"t"`
				R []struct {
					T string `xml:"t"`
				} `xml:"r"`
			} `xml:"si"`
		}
		if xml.Unmarshal(b, &ss) == nil {
			for _, si := range ss.SI {
				t := si.T
				for _, r := range si.R {
					t += r.T
				}
				shared = append(shared, t)
			}
		}
	}

	// sheet name -> rId -> target
	var wb struct {
		Sheets struct {
			Sheet []struct {
				Name string `xml:"name,attr"`
				RID  string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
			} `xml:"sheet"`
		} `xml:"sheets"`
	}
	if err := xml.Unmarshal(read("xl/workbook.xml"), &wb); err != nil {
		return nil, err
	}
	var rels struct {
		Rel []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(read("xl/_rels/workbook.xml.rels"), &rels); err != nil {
		return nil, err
	}
	relMap := map[string]string{}
	for _, r := range rels.Rel {
		relMap[r.ID] = strings.TrimPrefix(r.Target, "/xl/")
	}

	colIdx := func(ref string) int {
		n := 0
		for _, ch := range ref {
			if ch >= 'A' && ch <= 'Z' {
				n = n*26 + int(ch-'A') + 1
			} else {
				break
			}
		}
		return n - 1
	}

	out := map[string]*xlsxSheet{}
	for _, sh := range wb.Sheets.Sheet {
		target := relMap[sh.RID]
		if target == "" {
			continue
		}
		b := read("xl/" + strings.TrimPrefix(target, "xl/"))
		if b == nil {
			continue
		}
		var sheet struct {
			Rows []struct {
				C []struct {
					R  string `xml:"r,attr"`
					T  string `xml:"t,attr"`
					V  string `xml:"v"`
					IS struct {
						T string `xml:"t"`
					} `xml:"is"`
				} `xml:"c"`
			} `xml:"sheetData>row"`
		}
		if xml.Unmarshal(b, &sheet) != nil {
			continue
		}
		s := &xlsxSheet{}
		for _, row := range sheet.Rows {
			var cells []string
			for _, c := range row.C {
				v := c.V
				if c.T == "s" {
					if i, err := strconv.Atoi(c.V); err == nil && i < len(shared) {
						v = shared[i]
					}
				} else if c.T == "inlineStr" {
					v = c.IS.T
				}
				idx := colIdx(c.R)
				for len(cells) <= idx {
					cells = append(cells, "")
				}
				if idx >= 0 {
					cells[idx] = strings.TrimSpace(v)
				}
			}
			s.rows = append(s.rows, cells)
		}
		out[sh.Name] = s
	}
	return out, nil
}

// ---- source harvest ---------------------------------------------------------
//
// The accumulation step Stephen asked for: fetch every source URL the sector
// points cite, extract what the page actually says about AI (paragraphs
// containing AI-related terms, falling back to the lead text), and store a
// bounded excerpt keyed by content hash. The per-sector analysis then reads
// the harvested source text, not just our one-line notes about it, and
// regenerates when a source page's AI content materially changes.

var ihTagRe = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]+>`)
var ihWsRe = regexp.MustCompile(`[ \t\r\f]+`)
var aiTermRe = regexp.MustCompile(`(?i)\b(AI|artificial intelligence|machine learning|automation|autonomous|algorithm|generative|LLM|model)\b`)

var ihBlockRe = regexp.MustCompile(`(?i)</(p|div|li|h1|h2|h3|h4|td|tr|section|article)>|<br\s*/?>`)
var ihMetaRe = regexp.MustCompile(`(?is)<meta[^>]+(?:name="description"|property="og:description")[^>]+content="([^"]{40,})"|<meta[^>]+content="([^"]{40,})"[^>]+(?:name="description"|property="og:description")`)
var ihTitleRe = regexp.MustCompile(`(?is)<title[^>]*>([^<]{5,200})</title>`)

func twoaiHarvestExtract(raw []byte) string {
	// block-level closers become newlines first, so single-line HTML still
	// yields paragraph structure for the filter below
	pre := ihBlockRe.ReplaceAllString(string(raw), "\n")
	txt := ihTagRe.ReplaceAllString(pre, " ")
	txt = strings.ReplaceAll(txt, "&amp;", "&")
	txt = strings.ReplaceAll(txt, "&nbsp;", " ")
	txt = strings.ReplaceAll(txt, "&#39;", "'")
	txt = strings.ReplaceAll(txt, "&quot;", "\"")
	txt = ihWsRe.ReplaceAllString(txt, " ")
	var paras []string
	for _, ln := range strings.Split(txt, "\n") {
		ln = strings.TrimSpace(ln)
		if len(ln) < 120 {
			continue
		}
		paras = append(paras, ln)
	}
	// prefer AI-relevant paragraphs; fall back to the lead of the page
	var keep []string
	seen := map[string]bool{}
	total := 0
	for _, pgh := range paras {
		if !aiTermRe.MatchString(pgh) {
			continue
		}
		key := pgh[:60]
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(pgh) > 900 {
			pgh = pgh[:900]
		}
		keep = append(keep, pgh)
		total += len(pgh)
		if total > 4500 {
			break
		}
	}
	if len(keep) == 0 {
		for _, pgh := range paras {
			if len(pgh) > 900 {
				pgh = pgh[:900]
			}
			keep = append(keep, pgh)
			total += len(pgh)
			if total > 2000 {
				break
			}
		}
	}
	// JS-rendered pages carry their substance in title and meta description;
	// harvest those so a single-page app still yields what the page says.
	var meta []string
	if m := ihTitleRe.FindSubmatch(raw); m != nil {
		meta = append(meta, strings.TrimSpace(string(m[1])))
	}
	for _, m := range ihMetaRe.FindAllSubmatch(raw, 2) {
		v := string(m[1])
		if v == "" {
			v = string(m[2])
		}
		if v != "" {
			meta = append(meta, strings.TrimSpace(v))
		}
	}
	if len(meta) > 0 {
		keep = append([]string{strings.Join(meta, " — ")}, keep...)
	}
	out := strings.Join(keep, "\n")
	if len(out) > 5200 {
		out = out[:5200]
	}
	return out
}

func twoaiHarvestSources(db *sql.DB) error {
	rows, err := db.Query(`SELECT i.slug, p->>'name', p->>'source'
		FROM twoai_industries i, jsonb_array_elements(i.points) p
		WHERE p->>'source' LIKE 'http%'`)
	if err != nil {
		return err
	}
	type job struct{ slug, name, url string }
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.slug, &j.name, &j.url) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	client := &http.Client{Timeout: 20 * time.Second}
	fetched, skipped, failed := 0, 0, 0
	for _, j := range jobs {
		// once per day per URL: reruns and multi-sector shared URLs are free
		var last string
		db.QueryRow(`SELECT fetched_on::text FROM twoai_source_harvest WHERE url=$1`, j.url).Scan(&last)
		if last == time.Now().UTC().Format("2006-01-02") {
			skipped++
			continue
		}
		req, _ := http.NewRequest("GET", j.url, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; theworldofai.org source harvest; info@srjconsultingservices.com)")
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		resp, ferr := client.Do(req)
		status, extract := 0, ""
		if ferr == nil {
			status = resp.StatusCode
			if status == 200 {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
				extract = twoaiHarvestExtract(body)
			}
			resp.Body.Close()
		}
		h := sha256.Sum256([]byte(extract))
		if status == 200 && extract != "" {
			fetched++
			if _, err := db.Exec(`INSERT INTO twoai_source_harvest (url, sector_slug, source_name, http_status, extract, content_hash, fetched_on)
				VALUES ($1,$2,$3,$4,$5,$6,current_date)
				ON CONFLICT (url) DO UPDATE SET sector_slug=$2, source_name=$3, http_status=$4,
					extract=$5, content_hash=$6, fetched_on=current_date`,
				j.url, j.slug, j.name, status, extract, hex.EncodeToString(h[:8])); err != nil {
				return err
			}
		} else {
			failed++
			// keep yesterday's extract; only bump status and date
			db.Exec(`INSERT INTO twoai_source_harvest (url, sector_slug, source_name, http_status, fetched_on)
				VALUES ($1,$2,$3,$4,current_date)
				ON CONFLICT (url) DO UPDATE SET http_status=$4, fetched_on=current_date`,
				j.url, j.slug, j.name, status)
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Printf("twoai_industry_hub: harvest fetched=%d unchanged_today=%d failed=%d of %d sources\n",
		fetched, skipped, failed, len(jobs))
	return nil
}

// ---- Anthropic messages call ----------------------------------------------

func twoaiClaudeAnalyze(payload string) (model, body string, err error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "", "", fmt.Errorf("not configured")
	}
	model = os.Getenv("TWOAI_ANALYSIS_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	system := twoaiNationalSystem
	body, err = twoaiClaudeCall(model, system, "The data:\n"+payload+"\n\nWrite the analysis now.")
	return model, body, err
}

func twoaiClaudeAnalyzeExtra(payload, extra string) (model, body string, err error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "", "", fmt.Errorf("not configured")
	}
	model = os.Getenv("TWOAI_ANALYSIS_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	system := twoaiNationalSystem
	body, err = twoaiClaudeCall(model, system, "The data:\n"+payload+"\n\nWrite the analysis now."+extra)
	return model, body, err
}

func twoaiClaudeAnalyzeSectorExtra(sector, payload, extra string) (model, body string, err error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "", "", fmt.Errorf("not configured")
	}
	model = os.Getenv("TWOAI_ANALYSIS_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	system := twoaiSectorSystem
	body, err = twoaiClaudeCall(model, system, "Sector: "+sector+"\nThe data:\n"+payload+"\n\nWrite the analysis now."+extra)
	return model, body, err
}

func twoaiClaudeAnalyzeSector(sector, payload string) (model, body string, err error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "", "", fmt.Errorf("not configured")
	}
	model = os.Getenv("TWOAI_ANALYSIS_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	system := twoaiSectorSystem
	body, err = twoaiClaudeCall(model, system, "Sector: "+sector+"\nThe data:\n"+payload+"\n\nWrite the analysis now.")
	return model, body, err
}

// twoaiClaudeCall posts a messages request with one retry on rate limits
// and overloads: 429 and 529 wait out the backoff and try once more, so a
// 22-call run does not lose its biggest payloads to a burst limit.
func twoaiClaudeCall(model, system, user string) (string, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	reqBody, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 1200, "system": system,
		"messages": []map[string]string{{"role": "user", "content": user}},
	})
	client := &http.Client{Timeout: 120 * time.Second}
	for attempt := 0; attempt < 2; attempt++ {
		req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody))
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := client.Do(req)
		if err != nil {
			if attempt == 0 {
				time.Sleep(25 * time.Second)
				continue
			}
			return "", err
		}
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == 429 || resp.StatusCode == 529 {
			if attempt == 0 {
				time.Sleep(30 * time.Second)
				continue
			}
			return "", fmt.Errorf("anthropic status %d after retry", resp.StatusCode)
		}
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("anthropic status %d: %.200s", resp.StatusCode, rb)
		}
		var out struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(rb, &out); err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, c := range out.Content {
			if c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
		return strings.TrimSpace(sb.String()), nil
	}
	return "", fmt.Errorf("unreachable")
}

// twoaiAnalyzeValidated runs a generation and, if the validator rejects it,
// makes ONE corrective retry that names the offending figure - the chronic
// rejection mode is the model deriving a number (a delta, a rounding, an
// average) rather than quoting one, and telling it exactly which figure to
// drop fixes nearly all of them.
func twoaiAnalyzeValidated(gen func(extra string) (string, string, error), payload string) (string, string, error) {
	model, body, err := gen("")
	if err != nil {
		return model, body, err
	}
	verr := twoaiValidateAnalysis(payload, body)
	if verr == nil {
		return model, body, nil
	}
	model, body, err = gen("\n\nYour previous draft was rejected because it contained a figure not present in the data: " +
		verr.Error() + ". Rewrite the analysis using only percentages that appear verbatim, character for character, in the JSON. Do not compute, round, sum, subtract, or average any figure.")
	if err != nil {
		return model, body, err
	}
	if verr2 := twoaiValidateAnalysis(payload, body); verr2 != nil {
		return model, body, fmt.Errorf("rejected after corrective retry: %v", verr2)
	}
	return model, body, nil
}

// Every percentage in the analysis must exist in the payload. The guard exists
// because the chronic failure mode is a model DERIVING a number - a delta, a
// rounding, an average - and presenting it as sourced.
//
// BUT THE PAYLOAD DOES NOT ONLY SPEAK IN DIGITS. Source pages write percentages
// three ways, and a validator that recognises only "57%" rejects correct work:
//
//   57%                      the digit form
//   57 percent               digits with the word
//   Fifty-seven percent      spelled out, which is house style at many
//                            publishers and is what CEP writes
//
// The nonprofits sector failed on exactly this on 2026-08-19: the CEP excerpt
// in the payload reads "Fifty-seven percent of leaders say foundation grants
// are harder to get", the model correctly rendered it as 57%, and the guard
// threw the whole analysis away twice - once on the first pass and again after
// a corrective retry that told the model to remove a figure that was never
// wrong. A false rejection is not a safe failure here: it silently drops a
// sector's analysis and leaves the page thinner, while teaching nobody
// anything.
//
// So the allowed set is built from all three forms. The check on the OUTPUT
// stays strict and digit-only, because our own prose should be precise.
var pctRe = regexp.MustCompile(`\d+(?:\.\d+)?%`)
var pctWordRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*percent`)
var spelledPctRe = regexp.MustCompile(`(?i)\b((?:twenty|thirty|forty|fifty|sixty|seventy|eighty|ninety)(?:[- ](?:one|two|three|four|five|six|seven|eight|nine))?|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|one|two|three|four|five|six|seven|eight|nine)\s+percent`)

var numWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
	"eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13,
	"fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18,
	"nineteen": 19, "twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

// spelledToNumber turns "fifty-seven" into 57, and "seven" into 7.
func spelledToNumber(s string) (int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == ' ' })
	total, ok := 0, false
	for _, part := range parts {
		v, found := numWords[part]
		if !found {
			return 0, false
		}
		total += v
		ok = true
	}
	return total, ok
}

func twoaiValidateAnalysis(payload, body string) error {
	have := map[string]bool{}
	for _, p := range pctRe.FindAllString(payload, -1) {
		have[p] = true
	}
	for _, m := range pctWordRe.FindAllStringSubmatch(payload, -1) {
		have[m[1]+"%"] = true
	}
	for _, m := range spelledPctRe.FindAllStringSubmatch(payload, -1) {
		if n, ok := spelledToNumber(m[1]); ok {
			have[fmt.Sprintf("%d%%", n)] = true
		}
	}
	for _, p := range pctRe.FindAllString(body, -1) {
		if !have[p] {
			return fmt.Errorf("figure %s not in source data", p)
		}
	}
	return nil
}

// ---- the stage --------------------------------------------------------------

func twoaiIndustryHub(db *sql.DB, today string) (int, error) {
	// 0. HARVEST the cited sources themselves --------------------------------
	if err := twoaiHarvestSources(db); err != nil {
		return 0, err
	}

	// 1. FETCH ---------------------------------------------------------------
	type answerSeries struct {
		Answer string            `json:"answer"`
		Series map[string]string `json:"series"` // period code -> value
	}
	type question struct {
		ID      string         `json:"id"`
		Text    string         `json:"text"`
		Answers []answerSeries `json:"answers"`
	}
	metrics := map[string]any{}

	req, _ := http.NewRequest("GET", btosURL, nil)
	req.Header.Set("User-Agent", "theworldofai.org data pipeline (info@srjconsultingservices.com)")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err == nil && resp.StatusCode == 200 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
		resp.Body.Close()
		sheets, perr := readXLSX(data)
		if perr == nil && sheets["Response Estimates"] != nil {
			re := sheets["Response Estimates"]
			var periods []string
			if len(re.rows) > 0 {
				for _, h := range re.rows[0][4:] {
					if h != "" {
						periods = append(periods, h)
					}
				}
			}
			grab := func(qid string) *question {
				q := &question{ID: qid}
				for _, r := range re.rows[1:] {
					if len(r) < 5 || r[0] != qid {
						continue
					}
					if q.Text == "" {
						q.Text = r[1]
					}
					a := answerSeries{Answer: r[3], Series: map[string]string{}}
					for i, p := range periods {
						col := 4 + i
						if col < len(r) && r[col] != "" {
							a.Series[p] = r[col]
						}
					}
					q.Answers = append(q.Answers, a)
				}
				if q.Text == "" {
					return nil
				}
				return q
			}
			// Q7 current AI use, Q24 expected AI use: the two AI questions in
			// the national file, verified against the live workbook.
			var qs []*question
			for _, id := range []string{"7", "24"} {
				if q := grab(id); q != nil {
					qs = append(qs, q)
				}
			}
			// publication date of the newest period, from the dates sheet
			latestPub := ""
			if ds := sheets["Collection and Reference Dates"]; ds != nil && len(periods) > 0 {
				sort.Strings(periods)
				newest := periods[len(periods)-1]
				for _, r := range ds.rows[1:] {
					if len(r) > 8 && r[3] == newest {
						latestPub = strings.Split(r[8], " ")[0]
					}
				}
			}
			if len(qs) == 2 {
				sort.Strings(periods)
				metrics = map[string]any{
					"source":     "US Census Bureau, Business Trends and Outlook Survey (national estimates)",
					"source_url": btosURL,
					"page_url":   "https://www.census.gov/hfp/btos/data",
					"cadence":    "biweekly",
					"periods":    periods,
					"latest_pub": latestPub,
					"questions":  qs,
				}
			}
		}
	} else if resp != nil {
		resp.Body.Close()
	}

	dataHash := ""
	if len(metrics) > 0 {
		pj, _ := json.Marshal(metrics)
		h := sha256.Sum256(pj)
		dataHash = hex.EncodeToString(h[:8])
		if _, err := db.Exec(`INSERT INTO twoai_industry_metrics (metric, source_name, source_url, payload, data_hash, fetched_on)
			VALUES ('btos-ai-use','US Census Bureau BTOS',$1,$2::jsonb,$3,current_date)
			ON CONFLICT (metric) DO UPDATE SET payload=EXCLUDED.payload, data_hash=EXCLUDED.data_hash, fetched_on=current_date`,
			btosURL, string(pj), dataHash); err != nil {
			return 0, err
		}
		fmt.Printf("twoai_industry_hub: btos fetched hash=%s periods=%v\n", dataHash, len(metrics["periods"].([]string)))
	} else {
		// fetch failed: yesterday's stored payload keeps rendering
		var pj string
		if db.QueryRow(`SELECT payload::text, data_hash FROM twoai_industry_metrics WHERE metric='btos-ai-use'`).Scan(&pj, &dataHash) == nil {
			json.Unmarshal([]byte(pj), &metrics)
			fmt.Println("twoai_industry_hub: btos fetch failed, previous payload keeps rendering")
		} else {
			fmt.Println("twoai_industry_hub: btos fetch failed, no previous payload")
		}
	}

	// 1b. FETCH sector-level BTOS: Q7 by NAICS 2-digit sector -----------------
	type naicsStat struct {
		Code         string `json:"code"`
		Label        string `json:"label"`
		Latest       string `json:"latest"`
		Prior        string `json:"prior"`
		First        string `json:"first"`
		LatestPeriod string `json:"latest_period"`
	}
	sectorStats := map[string]naicsStat{} // naics code -> stat
	{
		req2, _ := http.NewRequest("GET", btosSectorURL, nil)
		req2.Header.Set("User-Agent", "theworldofai.org data pipeline (info@srjconsultingservices.com)")
		resp2, err2 := client.Do(req2)
		if err2 == nil && resp2.StatusCode == 200 {
			data2, _ := io.ReadAll(io.LimitReader(resp2.Body, 30<<20))
			resp2.Body.Close()
			if sheets2, perr := readXLSX(data2); perr == nil && sheets2["Response Estimates"] != nil {
				re2 := sheets2["Response Estimates"]
				var periods2 []string
				if len(re2.rows) > 0 {
					for _, h := range re2.rows[0][5:] {
						if h != "" {
							periods2 = append(periods2, h)
						}
					}
				}
				// columns run newest-first in the workbook
				for _, r := range re2.rows[1:] {
					if len(r) < 7 || r[1] != "7" || r[4] != "Yes" {
						continue
					}
					st := naicsStat{Code: r[0]}
					if len(periods2) > 0 {
						st.LatestPeriod = periods2[0]
					}
					vals := r[5:]
					if len(vals) > 0 {
						st.Latest = vals[0]
					}
					if len(vals) > 1 {
						st.Prior = vals[1]
					}
					for i := len(vals) - 1; i >= 0; i-- {
						if vals[i] != "" {
							st.First = vals[i]
							break
						}
					}
					sectorStats[st.Code] = st
				}
				fmt.Printf("twoai_industry_hub: btos sector file naics_rows=%d\n", len(sectorStats))
			}
		} else if resp2 != nil {
			resp2.Body.Close()
		}
	}

	// 2. INTERPRET -----------------------------------------------------------
	if dataHash != "" {
		var exists int
		db.QueryRow(`SELECT count(*) FROM twoai_industry_analysis WHERE metric='btos-ai-use' AND data_hash=$1`, dataHash).Scan(&exists)
		if exists == 0 {
			pj, _ := json.Marshal(metrics)
			model, body, aerr := twoaiAnalyzeValidated(func(extra string) (string, string, error) {
				return twoaiClaudeAnalyzeExtra(string(pj), extra)
			}, string(pj))
			if aerr != nil {
				fmt.Printf("twoai_industry_hub: analysis skipped/rejected: %v\n", aerr)
			} else {
				if _, err := db.Exec(`INSERT INTO twoai_industry_analysis (metric, data_hash, model, body, generated_on)
					VALUES ('btos-ai-use',$1,$2,$3,current_date) ON CONFLICT (metric, data_hash) DO NOTHING`,
					dataHash, model, body); err != nil {
					return 0, err
				}
				fmt.Printf("twoai_industry_hub: analysis generated model=%s hash=%s\n", model, dataHash)
			}
		}
	}
	// 2b. PER-SECTOR: payload = the sector's own BTOS row (when NAICS-mapped)
	// plus its curated points; analysis regenerates when either changes, is
	// validated the same way, and is patched into the sector page the
	// industries writer produced earlier in this run.
	{
		type pt struct {
			Name   string `json:"name"`
			Desc   string `json:"desc"`
			Source string `json:"source"`
		}
		irows, err := db.Query(`SELECT i.slug, t.name, COALESCE(t.blurb,''), i.points::text
			FROM twoai_industries i JOIN twoai_taxonomy t ON t.slug=i.slug ORDER BY i.slug`)
		if err != nil {
			return 0, err
		}
		type sectorJob struct{ slug, name, blurb, pointsJSON string }
		var jobs []sectorJob
		for irows.Next() {
			var j sectorJob
			if irows.Scan(&j.slug, &j.name, &j.blurb, &j.pointsJSON) == nil {
				jobs = append(jobs, j)
			}
		}
		irows.Close()
		generated, patched := 0, 0
		for _, j := range jobs {
			var pts []pt
			json.Unmarshal([]byte(j.pointsJSON), &pts)
			type srcText struct {
				Source  string `json:"source_org"`
				URL     string `json:"url"`
				Excerpt string `json:"what_the_page_says"`
			}
			var harvested []srcText
			hrows, herr := db.Query(`SELECT source_name, url, extract FROM twoai_source_harvest
				WHERE sector_slug=$1 AND extract <> '' ORDER BY url`, j.slug)
			if herr == nil {
				for hrows.Next() {
					var st srcText
					if hrows.Scan(&st.Source, &st.URL, &st.Excerpt) == nil {
						harvested = append(harvested, st)
					}
				}
				hrows.Close()
			}
			payload := map[string]any{
				"sector": j.name, "what_this_page_covers": j.blurb, "sourced_points": pts,
				"source_page_excerpts": harvested,
			}
			var bt *naicsStat
			if m, ok := btosNAICS[j.slug]; ok {
				if st, ok2 := sectorStats[m[0]]; ok2 {
					st.Label = m[1]
					bt = &st
					payload["census_btos_ai_use"] = map[string]string{
						"naics": st.Label, "latest": st.Latest, "prior_period": st.Prior,
						"first_asked_sept_2023": st.First,
						"note":                  "share of firms answering Yes to: in the last two weeks, did this business use AI. S means suppressed.",
					}
				}
			}
			pj, _ := json.Marshal(payload)
			h := sha256.Sum256(pj)
			secHash := hex.EncodeToString(h[:8])
			metricKey := "sector-" + strings.TrimPrefix(j.slug, "industry-")
			db.Exec(`INSERT INTO twoai_industry_metrics (metric, source_name, source_url, payload, data_hash, fetched_on)
				VALUES ($1,'US Census Bureau BTOS (sector) + curated points',$2,$3::jsonb,$4,current_date)
				ON CONFLICT (metric) DO UPDATE SET payload=EXCLUDED.payload, data_hash=EXCLUDED.data_hash, fetched_on=current_date`,
				metricKey, btosSectorURL, string(pj), secHash)
			var exists int
			db.QueryRow(`SELECT count(*) FROM twoai_industry_analysis WHERE metric=$1 AND data_hash=$2`, metricKey, secHash).Scan(&exists)
			if exists == 0 && os.Getenv("ANTHROPIC_API_KEY") != "" {
				model, body, aerr := twoaiAnalyzeValidated(func(extra string) (string, string, error) {
					return twoaiClaudeAnalyzeSectorExtra(j.name, string(pj), extra)
				}, string(pj))
				if aerr != nil {
					fmt.Printf("twoai_industry_hub: %s analysis skipped/rejected: %v\n", j.slug, aerr)
				} else {
					db.Exec(`INSERT INTO twoai_industry_analysis (metric, data_hash, model, body, generated_on)
						VALUES ($1,$2,$3,$4,current_date) ON CONFLICT (metric, data_hash) DO NOTHING`,
						metricKey, secHash, model, body)
					generated++
					time.Sleep(1500 * time.Millisecond)
				}
			}
			// patch the sector page written earlier this run
			var sModel, sBody, sDate string
			db.QueryRow(`SELECT model, body, generated_on::text FROM twoai_industry_analysis
				WHERE metric=$1 ORDER BY generated_on DESC LIMIT 1`, metricKey).Scan(&sModel, &sBody, &sDate)
			patchDoc := map[string]any{}
			if sBody != "" {
				patchDoc["analysis"] = map[string]any{"model": sModel, "body": sBody, "generated_on": sDate}
			}
			if bt != nil {
				patchDoc["btos"] = bt
			}
			patchDoc["sources_read"] = len(harvested)
			patchDoc["sources_total"] = len(pts)
			if len(patchDoc) > 0 {
				pd, _ := json.Marshal(patchDoc)
				if res, err := db.Exec(`UPDATE twoai_pages SET data = data || $1::jsonb, updated_at=now()
					WHERE path = $2`, string(pd), "industries/"+j.slug+".json"); err == nil {
					if n, _ := res.RowsAffected(); n > 0 {
						patched++
					}
				}
			}
		}
		fmt.Printf("twoai_industry_hub: sector analyses generated=%d pages_patched=%d\n", generated, patched)
	}

	// 2c. CROSS-INDUSTRY synthesis for the hub: the 21 sector analyses plus
	// the national and sector Census figures, synthesized into findings.
	{
		type secAn struct {
			Sector string `json:"sector"`
			Body   string `json:"analysis"`
		}
		var all []secAn
		arows, aerr2 := db.Query(`SELECT DISTINCT ON (a.metric) t.name, a.body
			FROM twoai_industry_analysis a
			JOIN twoai_taxonomy t ON t.slug = 'industry-' || replace(a.metric,'sector-','')
			WHERE a.metric LIKE 'sector-%' ORDER BY a.metric, a.generated_on DESC`)
		if aerr2 == nil {
			for arows.Next() {
				var sa secAn
				if arows.Scan(&sa.Sector, &sa.Body) == nil {
					all = append(all, sa)
				}
			}
			arows.Close()
		}
		if len(all) > 0 {
			xp := map[string]any{"national_btos": metrics, "sector_analyses": all}
			xj, _ := json.Marshal(xp)
			xh := sha256.Sum256(xj)
			xHash := hex.EncodeToString(xh[:8])
			var exists int
			db.QueryRow(`SELECT count(*) FROM twoai_industry_analysis WHERE metric='cross-industry' AND data_hash=$1`, xHash).Scan(&exists)
			if exists == 0 && os.Getenv("ANTHROPIC_API_KEY") != "" {
				model, body, xerr := twoaiAnalyzeValidated(func(extra string) (string, string, error) {
					return twoaiClaudeAnalyzeSectorExtra("All 21 industries, synthesized", string(xj), extra)
				}, string(xj))
				if xerr != nil {
					fmt.Printf("twoai_industry_hub: cross-industry synthesis skipped/rejected: %v\n", xerr)
				} else {
					db.Exec(`INSERT INTO twoai_industry_analysis (metric, data_hash, model, body, generated_on)
						VALUES ('cross-industry',$1,$2,$3,current_date) ON CONFLICT (metric, data_hash) DO NOTHING`,
						xHash, model, body)
					fmt.Printf("twoai_industry_hub: cross-industry synthesis generated hash=%s\n", xHash)
				}
			}
		}
	}

	var anModel, anBody, anDate string
	db.QueryRow(`SELECT model, body, generated_on::text FROM twoai_industry_analysis
		WHERE metric='btos-ai-use' ORDER BY generated_on DESC LIMIT 1`).Scan(&anModel, &anBody, &anDate)
	var synModel, synBody, synDate string
	db.QueryRow(`SELECT model, body, generated_on::text FROM twoai_industry_analysis
		WHERE metric='cross-industry' ORDER BY generated_on DESC LIMIT 1`).Scan(&synModel, &synBody, &synDate)

	// 3. RENDER ----------------------------------------------------------------
	type finding struct {
		Heading string `json:"heading"`
		Body    string `json:"body"`
	}
	var findings []finding
	frows, err := db.Query(`SELECT heading, body FROM twoai_industry_findings ORDER BY sort`)
	if err != nil {
		return 0, err
	}
	for frows.Next() {
		var f finding
		if frows.Scan(&f.Heading, &f.Body) == nil {
			findings = append(findings, f)
		}
	}
	frows.Close()

	type sector struct {
		Name   string `json:"name"`
		Path   string `json:"path"`
		Points int    `json:"points"`
		Blurb  string `json:"blurb"`
		AIUse  string `json:"ai_use,omitempty"`
	}
	var sectors []sector
	totalPoints := 0
	srows, err := db.Query(`SELECT i.slug, t.name, COALESCE(t.live_path,''), jsonb_array_length(i.points), COALESCE(t.blurb,'')
		FROM twoai_industries i JOIN twoai_taxonomy t ON t.slug=i.slug ORDER BY t.name`)
	if err != nil {
		return 0, err
	}
	for srows.Next() {
		var s sector
		var slug string
		if srows.Scan(&slug, &s.Name, &s.Path, &s.Points, &s.Blurb) == nil {
			if m, ok := btosNAICS[slug]; ok {
				if st, ok2 := sectorStats[m[0]]; ok2 && st.Latest != "" && st.Latest != "S" {
					s.AIUse = st.Latest
				}
			}
			sectors = append(sectors, s)
			totalPoints += s.Points
		}
	}
	srows.Close()

	var name, blurb string
	db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug='industry-use-cases'`).Scan(&name, &blurb)

	doc := map[string]any{
		"uid": twoaiUID("section:industry-use-cases"), "tax": "industry-use-cases",
		"shape": "industry-hub", "name": name, "blurb": blurb, "generated": today,
		"metrics": metrics, "findings": findings, "sectors": sectors,
		"total_points": totalPoints,
	}
	if anBody != "" {
		doc["analysis"] = map[string]any{"model": anModel, "body": anBody, "generated_on": anDate}
	}
	if synBody != "" {
		doc["synthesis"] = map[string]any{"model": synModel, "body": synBody, "generated_on": synDate}
	}
	var hOK, hAll int
	db.QueryRow(`SELECT count(*) FILTER (WHERE extract <> ''), count(*) FROM twoai_source_harvest`).Scan(&hOK, &hAll)
	doc["sources_read"] = hOK
	doc["sources_total"] = hAll
	dj, _ := json.Marshal(doc)
	if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
		VALUES ('industries/index.json','industry-hub',$1::jsonb,'industry-use-cases',1)
		ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
			taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`, string(dj)); err != nil {
		return 0, err
	}
	fmt.Printf("twoai_industry_hub: sectors=%d points=%d findings=%d analysis=%v\n",
		len(sectors), totalPoints, len(findings), anBody != "")
	return 1, nil
}
