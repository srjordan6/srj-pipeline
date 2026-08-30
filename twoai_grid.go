package main

// The Grid Observatory: harvest the public interconnection queues of the US
// grid operators every day, snapshot the topline numbers into our own time
// series, and have Claude interpret what the numbers mean for AI data
// centers. The queues are where the AI buildout is visible before it is
// built: every gigawatt of planned generation and storage waiting for a
// wire is a bet on load growth, and data centers are the load.
//
// Sources, verified 2026-08-29 against live probes (the gridstatus open
// source project documents the same endpoints):
//   CAISO  PublicQueueReport.xlsx, three sheets (active, completed,
//          withdrawn), no key.
//   SPP    GenerateSummaryCSV, one CSV, no key.
//   ERCOT  monthly GIS report, two-step: doc-list JSON, then newest
//          GIS_Report xlsx by DocID, no key.
//   MISO   api/giqueue/getprojects JSON. Cloudflare-challenged from some
//          IPs; degrade-and-log when blocked.
//   NYISO  Interconnection-Queue.xlsx. Returned 202/empty from the build
//          sandbox; degrade-and-log, may pass from the cron.
//   PJM    behind a free API key; harvested only when PJM_API_KEY is set.
//   EIA    behind a free API key; harvested only when EIA_API_KEY is set.
// Every snapshot row keeps its source URL. Nothing is estimated: a source
// that fails today keeps yesterday's snapshot and the page says when each
// number was last fetched.

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const twoaiGridUA = "Mozilla/5.0 (compatible; theworldofai.org grid observatory; contact stephen@srjconsultingservices.com)"

func twoaiGridGet(url string) ([]byte, error) {
	client := &http.Client{Timeout: 120 * time.Second}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", twoaiGridUA)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 40<<20))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	return b, nil
}

// fuelBucket maps a free-text generation or fuel type onto the five buckets
// the observatory publishes. Everything unrecognized is counted, not hidden.
func fuelBucket(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	// Exact code tokens first: ERCOT GIS fuel and technology codes (a
	// battery there is fuel OTH with technology BA, so the codes decide).
	codes := map[string]string{"sol": "solar", "pv": "solar", "win": "wind", "wt": "wind",
		"esr": "storage", "bat": "storage", "ba": "storage", "gas": "gas",
		"cc": "gas", "gt": "gas", "ic": "gas", "st": "gas"}
	for _, tok := range strings.FieldsFunc(t, func(r rune) bool {
		return r == ' ' || r == '/' || r == '-' || r == ',' || r == '(' || r == ')'
	}) {
		if b, ok := codes[tok]; ok {
			return b
		}
	}
	switch {
	case strings.Contains(t, "stor") || strings.Contains(t, "batter") || strings.Contains(t, "bess") || t == "bat":
		return "storage"
	case strings.Contains(t, "solar") || strings.Contains(t, "photovolt") || t == "sun":
		return "solar"
	case strings.Contains(t, "wind"):
		return "wind"
	case strings.Contains(t, "gas") || strings.Contains(t, "combustion") || strings.Contains(t, "combined cycle") || strings.Contains(t, "thermal") || strings.Contains(t, "reciprocating") || t == "ng" || t == "ct" || t == "ctg":
		return "gas"
	}
	return "other"
}

func gridNum(s string) float64 {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || v > 100000 {
		return 0
	}
	return v
}

type gridMetrics struct {
	Rows    int
	MW      float64
	ByFuel  map[string]float64
	Extra   map[string]any
	AsOfDoc string // the source's own stamp when it publishes one
}

func newGridMetrics() *gridMetrics {
	return &gridMetrics{ByFuel: map[string]float64{}, Extra: map[string]any{}}
}

// headerIndex finds the first row that contains want, and returns its index
// plus a column map of lowercased header cell -> column number.
func headerIndex(rows [][]string, want string) (int, map[string]int) {
	for i, r := range rows {
		for _, c := range r {
			if strings.Contains(strings.ToLower(c), strings.ToLower(want)) {
				cols := map[string]int{}
				for j, h := range r {
					h = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(h, "\r\n", " ")))
					if h != "" {
						cols[h] = j
					}
				}
				return i, cols
			}
		}
	}
	return -1, nil
}

func colLike(cols map[string]int, subs ...string) int {
	best := -1
	for h, j := range cols {
		for _, s := range subs {
			if strings.Contains(h, s) {
				if best == -1 || j < best {
					best = j
				}
			}
		}
	}
	return best
}

func cell(r []string, j int) string {
	if j < 0 || j >= len(r) {
		return ""
	}
	return r[j]
}

// ---- per-source harvesters ------------------------------------------------

func gridCAISO() (*gridMetrics, error) {
	b, err := twoaiGridGet("https://www.caiso.com/PublishedDocuments/PublicQueueReport.xlsx")
	if err != nil {
		return nil, err
	}
	return parseCAISO(b)
}

func parseCAISO(b []byte) (*gridMetrics, error) {
	sheets, err := readXLSX(b)
	if err != nil {
		return nil, err
	}
	m := newGridMetrics()
	active := sheets["Grid GenerationQueue"]
	if active == nil {
		return nil, fmt.Errorf("active sheet missing")
	}
	hi, cols := headerIndex(active.rows, "queue position")
	if hi < 0 {
		return nil, fmt.Errorf("header not found")
	}
	mwCol := colLike(cols, "mw")
	typeCol := colLike(cols, "type")
	for _, r := range active.rows[hi+1:] {
		if strings.TrimSpace(cell(r, cols["queue position"])) == "" && strings.TrimSpace(cell(r, 0)) == "" {
			continue
		}
		mw := gridNum(cell(r, mwCol))
		if mw == 0 && strings.TrimSpace(cell(r, typeCol)) == "" {
			continue
		}
		m.Rows++
		m.MW += mw
		m.ByFuel[fuelBucket(cell(r, typeCol))] += mw
	}
	if s := sheets["Completed Generation Projects"]; s != nil {
		m.Extra["completed_rows"] = maxInt(0, len(s.rows)-4)
	}
	if s := sheets["Withdrawn Generation Projects"]; s != nil {
		m.Extra["withdrawn_rows"] = maxInt(0, len(s.rows)-4)
	}
	return m, nil
}

func gridSPP() (*gridMetrics, error) {
	b, err := twoaiGridGet("https://opsportal.spp.org/Studies/GenerateSummaryCSV")
	if err != nil {
		return nil, err
	}
	return parseSPP(b)
}

func parseSPP(b []byte) (*gridMetrics, error) {
	rd := csv.NewReader(bytes.NewReader(b))
	rd.FieldsPerRecord = -1
	all, err := rd.ReadAll()
	if err != nil || len(all) < 3 {
		return nil, fmt.Errorf("csv parse: %v rows=%d", err, len(all))
	}
	m := newGridMetrics()
	if len(all[0]) > 1 {
		m.AsOfDoc = strings.TrimSpace(all[0][1])
	}
	cols := map[string]int{}
	for j, h := range all[1] {
		cols[strings.ToLower(strings.TrimSpace(h))] = j
	}
	mwCol := colLike(cols, "capacity")
	fuelCol := colLike(cols, "fuel type")
	if fuelCol < 0 {
		fuelCol = colLike(cols, "generation type")
	}
	statusCol := colLike(cols, "status")
	withdrawn := 0
	for _, r := range all[2:] {
		if len(r) < 3 {
			continue
		}
		st := strings.ToLower(cell(r, statusCol))
		if strings.Contains(st, "withdraw") {
			withdrawn++
			continue
		}
		m.Rows++
		mw := gridNum(cell(r, mwCol))
		m.MW += mw
		genCol := colLike(cols, "generation type")
		m.ByFuel[fuelBucket(strings.TrimSpace(cell(r, fuelCol)+" "+cell(r, genCol)))] += mw
	}
	m.Extra["withdrawn_rows"] = withdrawn
	return m, nil
}

func gridERCOT() (*gridMetrics, error) {
	b, err := twoaiGridGet("https://www.ercot.com/misapp/servlets/IceDocListJsonWS?reportTypeId=15933")
	if err != nil {
		return nil, err
	}
	var list struct {
		ListDocsByRptTypeRes struct {
			DocumentList []struct {
				Document struct {
					FriendlyName string `json:"FriendlyName"`
					DocID        string `json:"DocID"`
					PublishDate  string `json:"PublishDate"`
				} `json:"Document"`
			} `json:"DocumentList"`
		} `json:"ListDocsByRptTypeRes"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	docID, docName := "", ""
	for _, d := range list.ListDocsByRptTypeRes.DocumentList {
		if strings.HasPrefix(d.Document.FriendlyName, "GIS_Report") {
			docID, docName = d.Document.DocID, d.Document.FriendlyName
			break // list is newest-first
		}
	}
	if docID == "" {
		return nil, fmt.Errorf("no GIS_Report in doc list")
	}
	raw, err := twoaiGridGet("https://www.ercot.com/misdownload/servlets/mirDownload?doclookupId=" + docID)
	if err != nil {
		return nil, err
	}
	m, err := parseERCOT(raw)
	if err != nil {
		return nil, err
	}
	m.AsOfDoc = docName
	return m, nil
}

func parseERCOT(raw []byte) (*gridMetrics, error) {
	// mirDownload serves either the xlsx directly or a zip holding it.
	if !bytes.HasPrefix(raw, []byte("PK")) {
		return nil, fmt.Errorf("unexpected payload")
	}
	xl := raw
	if zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw))); err == nil {
		hasWB := false
		for _, f := range zr.File {
			if f.Name == "xl/workbook.xml" {
				hasWB = true
			}
		}
		if !hasWB {
			for _, f := range zr.File {
				if strings.HasSuffix(strings.ToLower(f.Name), ".xlsx") {
					rc, _ := f.Open()
					xl, _ = io.ReadAll(io.LimitReader(rc, 40<<20))
					rc.Close()
					break
				}
			}
		}
	}
	sheets, err := readXLSX(xl)
	if err != nil {
		return nil, err
	}
	sheet := sheets["Project Details - Large Gen"]
	if sheet == nil {
		return nil, fmt.Errorf("large gen sheet missing")
	}
	hi, cols := headerIndex(sheet.rows, "capacity (mw)")
	if hi < 0 {
		return nil, fmt.Errorf("header not found")
	}
	m := newGridMetrics()
	mwCol := colLike(cols, "capacity (mw)")
	fuelCol := colLike(cols, "fuel")
	techCol := colLike(cols, "technology")
	for _, r := range sheet.rows[hi+1:] {
		mw := gridNum(cell(r, mwCol))
		fuel := strings.TrimSpace(cell(r, fuelCol) + " " + cell(r, techCol))
		if mw == 0 && fuel == "" {
			continue
		}
		m.Rows++
		m.MW += mw
		m.ByFuel[fuelBucket(fuel)] += mw
	}
	return m, nil
}

func gridMISO() (*gridMetrics, error) {
	b, err := twoaiGridGet("https://www.misoenergy.org/api/giqueue/getprojects")
	if err != nil {
		return nil, err
	}
	var arr []map[string]any
	if err := json.Unmarshal(b, &arr); err != nil {
		// some deployments wrap the array
		var wrap map[string]any
		if json.Unmarshal(b, &wrap) == nil {
			for _, v := range wrap {
				if a, ok := v.([]any); ok {
					for _, e := range a {
						if o, ok := e.(map[string]any); ok {
							arr = append(arr, o)
						}
					}
					break
				}
			}
		}
		if len(arr) == 0 {
			return nil, fmt.Errorf("json shape unrecognized")
		}
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("empty project list")
	}
	// Autodetect the MW and fuel keys from the first records, and log them,
	// rather than guessing field names we have never seen.
	mwKey, fuelKey := "", ""
	for k := range arr[0] {
		lk := strings.ToLower(k)
		if mwKey == "" && strings.Contains(lk, "mw") {
			mwKey = k
		}
		if fuelKey == "" && (strings.Contains(lk, "fuel") || strings.Contains(lk, "type")) {
			fuelKey = k
		}
	}
	if mwKey == "" {
		return nil, fmt.Errorf("no MW field found")
	}
	m := newGridMetrics()
	m.Extra["mw_field"] = mwKey
	m.Extra["fuel_field"] = fuelKey
	for _, o := range arr {
		mw := 0.0
		switch v := o[mwKey].(type) {
		case float64:
			mw = v
		case string:
			mw = gridNum(v)
		}
		if mw < 0 || mw > 100000 {
			mw = 0
		}
		fuel := ""
		if fuelKey != "" {
			fuel, _ = o[fuelKey].(string)
		}
		m.Rows++
		m.MW += mw
		m.ByFuel[fuelBucket(fuel)] += mw
	}
	return m, nil
}

func gridNYISO() (*gridMetrics, error) {
	b, err := twoaiGridGet("https://www.nyiso.com/documents/20142/1407078/NYISO-Interconnection-Queue.xlsx")
	if err != nil {
		return nil, err
	}
	sheets, err := readXLSX(b)
	if err != nil {
		return nil, err
	}
	// Use the sheet with the most rows whose header mentions MW.
	var best *xlsxSheet
	for _, s := range sheets {
		hi, _ := headerIndex(s.rows, "mw")
		if hi >= 0 && (best == nil || len(s.rows) > len(best.rows)) {
			best = s
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no MW sheet found")
	}
	hi, cols := headerIndex(best.rows, "mw")
	mwCol := colLike(cols, "mw")
	typeCol := colLike(cols, "type")
	m := newGridMetrics()
	for _, r := range best.rows[hi+1:] {
		mw := gridNum(cell(r, mwCol))
		ty := strings.TrimSpace(cell(r, typeCol))
		if mw == 0 && ty == "" {
			continue
		}
		m.Rows++
		m.MW += mw
		m.ByFuel[fuelBucket(ty)] += mw
	}
	return m, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---- the stage ------------------------------------------------------------

type gridSourceDef struct {
	Slug, Name, URL, Scope string
	Fetch                  func() (*gridMetrics, error)
}

var twoaiGridSources = []gridSourceDef{
	{"caiso", "CAISO", "https://www.caiso.com/PublishedDocuments/PublicQueueReport.xlsx",
		"Active generator interconnection requests on the California ISO grid, from the public queue report. Completed and withdrawn projects are counted separately.", gridCAISO},
	{"ercot", "ERCOT", "https://www.ercot.com/mp/data-products/data-product-details?id=PG7-200-ER",
		"Large generators with a full interconnection study requested, from the monthly Generator Interconnection Status report for Texas. Includes projects from study through commissioning.", gridERCOT},
	{"spp", "Southwest Power Pool", "https://opsportal.spp.org/Studies/GenerateSummaryCSV",
		"Generator interconnection requests across the south-central US, all statuses as SPP publishes them, withdrawn requests counted separately.", gridSPP},
	{"miso", "MISO", "https://www.misoenergy.org/planning/resource-utility-interconnection/gi-interactive-queue/",
		"Projects in the Midcontinent generator interconnection queue, all statuses as MISO publishes them.", gridMISO},
	{"nyiso", "NYISO", "https://www.nyiso.com/interconnections",
		"Projects in the New York interconnection queue as NYISO publishes them.", gridNYISO},
}

func twoaiGridHarvest(db *sql.DB) int {
	today := time.Now().UTC().Format("2006-01-02")
	stored := 0
	for _, s := range twoaiGridSources {
		m, err := s.Fetch()
		if err != nil {
			fmt.Printf("twoai_grid: %s fetch failed: %v (keeping prior snapshot)\n", s.Slug, err)
			continue
		}
		detail := map[string]any{"by_fuel": m.ByFuel, "scope": s.Scope}
		for k, v := range m.Extra {
			detail[k] = v
		}
		if m.AsOfDoc != "" {
			detail["source_stamp"] = m.AsOfDoc
		}
		dj, _ := json.Marshal(detail)
		for metric, val := range map[string]float64{
			"requests_tracked": float64(m.Rows),
			"mw_tracked":       m.MW,
		} {
			unit := "MW"
			if metric == "requests_tracked" {
				unit = "requests"
			}
			if _, err := db.Exec(`INSERT INTO twoai_grid_obs (source, metric, value, unit, as_of, detail, source_url)
				VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7)
				ON CONFLICT (source, metric, as_of) DO UPDATE SET value=EXCLUDED.value, detail=EXCLUDED.detail, fetched_at=now()`,
				s.Slug, metric, val, unit, today, string(dj), s.URL); err == nil {
				stored++
			}
		}
		fmt.Printf("twoai_grid: %s requests=%d mw=%.0f fuels=%d\n", s.Slug, m.Rows, m.MW, len(m.ByFuel))
	}
	if os.Getenv("PJM_API_KEY") == "" {
		fmt.Println("twoai_grid: pjm skipped, PJM_API_KEY not set (free key, dataminer2 registration)")
	}
	if os.Getenv("EIA_API_KEY") == "" {
		fmt.Println("twoai_grid: eia skipped, EIA_API_KEY not set (free key, eia.gov/opendata)")
	}
	return stored
}

// twoaiGridPage assembles the observatory page from the latest snapshot per
// source, generates or reuses the Claude interpretation, and upserts the
// page. Called from twoai_build after the harvest.
func twoaiGridPage(db *sql.DB, today string) error {
	type srcOut struct {
		Slug   string             `json:"slug"`
		Name   string             `json:"name"`
		URL    string             `json:"url"`
		Scope  string             `json:"scope"`
		AsOf   string             `json:"as_of"`
		Rows   float64            `json:"requests"`
		MW     float64            `json:"mw"`
		ByFuel map[string]float64 `json:"by_fuel"`
		Stamp  string             `json:"source_stamp,omitempty"`
	}
	var srcs []srcOut
	for _, s := range twoaiGridSources {
		var o srcOut
		o.Slug, o.Name, o.URL, o.Scope = s.Slug, s.Name, s.URL, s.Scope
		var dj []byte
		err := db.QueryRow(`SELECT value, as_of::text, detail FROM twoai_grid_obs
			WHERE source=$1 AND metric='mw_tracked' ORDER BY as_of DESC LIMIT 1`, s.Slug).Scan(&o.MW, &o.AsOf, &dj)
		if err != nil {
			continue
		}
		db.QueryRow(`SELECT value FROM twoai_grid_obs
			WHERE source=$1 AND metric='requests_tracked' AND as_of=$2`, s.Slug, o.AsOf).Scan(&o.Rows)
		var det struct {
			ByFuel map[string]float64 `json:"by_fuel"`
			Stamp  string             `json:"source_stamp"`
		}
		json.Unmarshal(dj, &det)
		o.ByFuel, o.Stamp = det.ByFuel, det.Stamp
		srcs = append(srcs, o)
	}
	if len(srcs) == 0 {
		fmt.Println("twoai_grid: no snapshots yet, page skipped")
		return nil
	}
	sort.Slice(srcs, func(i, j int) bool { return srcs[i].MW > srcs[j].MW })

	var seriesDays int
	db.QueryRow(`SELECT count(DISTINCT as_of) FROM twoai_grid_obs`).Scan(&seriesDays)

	// Claude interpretation, cached by data hash so it regenerates only when
	// the numbers move.
	payload, _ := json.Marshal(map[string]any{"as_of": today, "sources": srcs, "series_days": seriesDays})
	h := sha256.Sum256(payload)
	dataHash := hex.EncodeToString(h[:8])
	var aModel, aBody, aOn string
	db.QueryRow(`SELECT model, body, generated_on::text FROM twoai_grid_analysis WHERE data_hash=$1`, dataHash).Scan(&aModel, &aBody, &aOn)
	if aBody == "" && os.Getenv("ANTHROPIC_API_KEY") != "" {
		system := "You are the analyst for theworldofai.org, an AI reference encyclopedia. " +
			"You are given today's snapshot of US grid interconnection queues, harvested from the grid operators' own public files. " +
			"Write 3 short paragraphs of plain-English interpretation for readers who care about AI data centers. " +
			"Rules you must not break: use ONLY the numbers provided, never introduce outside figures or estimates; " +
			"note that queue totals are planned generation and storage waiting to connect, not data center demand itself, but that storage and gas growth track data center load; " +
			"respect each source's scope note, the sources are not directly comparable; no headings, no bullet lists, no hype. " +
			"End with one sentence on what to watch next."
		if model, body, err := func() (string, string, error) {
			model := os.Getenv("TWOAI_ANALYSIS_MODEL")
			if model == "" {
				model = "claude-sonnet-4-6"
			}
			b, err := twoaiClaudeCall(model, system, "The data:\n"+string(payload)+"\n\nWrite the interpretation now.")
			return model, b, err
		}(); err == nil && strings.TrimSpace(body) != "" {
			aModel, aBody, aOn = model, strings.TrimSpace(body), today
			db.Exec(`INSERT INTO twoai_grid_analysis (data_hash, model, body, generated_on)
				VALUES ($1,$2,$3,current_date) ON CONFLICT (data_hash) DO NOTHING`, dataHash, aModel, aBody)
			fmt.Printf("twoai_grid: analysis generated hash=%s\n", dataHash)
		} else if err != nil {
			fmt.Println("twoai_grid: analysis skipped:", err)
			// fall back to the most recent analysis of any hash
			db.QueryRow(`SELECT model, body, generated_on::text FROM twoai_grid_analysis ORDER BY generated_on DESC LIMIT 1`).Scan(&aModel, &aBody, &aOn)
		}
	} else if aBody == "" {
		db.QueryRow(`SELECT model, body, generated_on::text FROM twoai_grid_analysis ORDER BY generated_on DESC LIMIT 1`).Scan(&aModel, &aBody, &aOn)
	}

	doc := map[string]any{
		"shape": "dc-grid", "uid": twoaiUID("dc-grid"), "tax": "data-centers", "generated": today,
		"sources": srcs, "series_days": seriesDays,
		"parent": map[string]any{"uid": twoaiUID("section:data-centers"), "name": "Data Centers"},
	}
	if aBody != "" {
		doc["analysis"] = map[string]any{"model": aModel, "body": aBody, "generated_on": aOn}
	}
	dj, _ := json.Marshal(doc)
	_, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
		VALUES ('tech/dc-grid.json','tech-dc-grid',$1::jsonb,'data-centers',1)
		ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
			taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`, string(dj))
	if err == nil {
		fmt.Printf("twoai_grid: page sources=%d series_days=%d analysis=%v\n", len(srcs), seriesDays, aBody != "")
	}
	return err
}
