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
	system := "You write analysis for theworldofai.org, a sourced AI reference site. " +
		"You are given survey data as JSON. Write what the numbers mean for the AI industry " +
		"in plain English prose: 3 to 4 short paragraphs, no headings, no bullet points, no preamble. " +
		"HARD RULES: use ONLY facts and figures present in the JSON. Do not bring in any outside " +
		"statistic, company, product, or event. Every percentage you mention must appear verbatim " +
		"in the JSON. Describe direction and magnitude of change, what the gap between current and " +
		"expected use implies, and what the do-not-know share suggests, strictly from the data given. " +
		"Plain sentences, no hype, commas over dashes."
	reqBody, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1200,
		"system":     system,
		"messages": []map[string]string{
			{"role": "user", "content": "The data:\n" + payload + "\n\nWrite the analysis now."},
		},
	})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return model, "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return model, "", fmt.Errorf("anthropic status %d: %.200s", resp.StatusCode, rb)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return model, "", err
	}
	var sb strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return model, strings.TrimSpace(sb.String()), nil
}

// every percentage in the analysis must exist verbatim in the payload
var pctRe = regexp.MustCompile(`\d+(?:\.\d+)?%`)

func twoaiValidateAnalysis(payload, body string) error {
	have := map[string]bool{}
	for _, p := range pctRe.FindAllString(payload, -1) {
		have[p] = true
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

	// 2. INTERPRET -----------------------------------------------------------
	if dataHash != "" {
		var exists int
		db.QueryRow(`SELECT count(*) FROM twoai_industry_analysis WHERE metric='btos-ai-use' AND data_hash=$1`, dataHash).Scan(&exists)
		if exists == 0 {
			pj, _ := json.Marshal(metrics)
			model, body, aerr := twoaiClaudeAnalyze(string(pj))
			if aerr != nil {
				fmt.Printf("twoai_industry_hub: analysis skipped: %v\n", aerr)
			} else if verr := twoaiValidateAnalysis(string(pj), body); verr != nil {
				fmt.Printf("twoai_industry_hub: analysis REJECTED (%v), previous keeps rendering\n", verr)
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
	var anModel, anBody, anDate string
	db.QueryRow(`SELECT model, body, generated_on::text FROM twoai_industry_analysis
		WHERE metric='btos-ai-use' ORDER BY generated_on DESC LIMIT 1`).Scan(&anModel, &anBody, &anDate)

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
	}
	var sectors []sector
	totalPoints := 0
	srows, err := db.Query(`SELECT t.name, COALESCE(t.live_path,''), jsonb_array_length(i.points), COALESCE(t.blurb,'')
		FROM twoai_industries i JOIN twoai_taxonomy t ON t.slug=i.slug ORDER BY t.name`)
	if err != nil {
		return 0, err
	}
	for srows.Next() {
		var s sector
		if srows.Scan(&s.Name, &s.Path, &s.Points, &s.Blurb) == nil {
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
