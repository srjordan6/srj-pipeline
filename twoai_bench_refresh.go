package main

// Benchmark refreshers, theworldofai bridge row 438 (Stephen, 2026-10-03: are
// all model benchmarks up to date? No).
//
// Each refresher reads the maintainer's own published leaderboard, never a
// vendor's launch post (row 386), and stores the top of it with two dates:
// as_of, the date the maintainer's data carries (a publish date, a release,
// the newest submission), and retrieved, the day this pipeline read it. A
// board that has not changed since its last release is current once it has
// been re-read, so the freshness audit measures retrieved.
//
//	lmarena            LMArena's official dataset on Hugging Face, text arena with style control
//	livebench          LiveBench's published table for its newest release
//	osworld            the OSWorld-Verified results file on os-world.github.io
//	gaia               the GAIA test-set results dataset on Hugging Face
//	tau-bench          Sierra's tau2-bench submissions bucket behind taubench.com
//	webarena           the leaderboard sheet linked from webarena.dev
//	metr-time-horizon  METR's benchmark_results YAML behind metr.org/time-horizons
//
// MLPerf publishes in rounds, so it is watched rather than scraped: each run
// compares the newest results repository in the mlcommons GitHub organisation
// (and the newest MLPerf Client release) with the snapshot date. An unchanged
// round stamps retrieved; a newer round leaves it, so the audit reports the
// snapshot overdue with the round named.
//
// A refresher that fails keeps the last snapshot and says so in the log; the
// freshness audit escalates it after three runs.

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const benchUA = "SRJ-Consulting-intel-sync/1.0 (theworldofai.org)"

func benchGet(u string) ([]byte, error) {
	client := &http.Client{Timeout: 90 * time.Second}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", benchUA)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: status %d", u, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

type benchRow struct {
	system, detail string
	score          float64
	scoreText      string
}

// benchTop sorts, keeps the best row per system and returns the top n.
func benchTop(rows []benchRow, n int) []map[string]any {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].score > rows[j].score })
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, r := range rows {
		k := strings.ToLower(strings.TrimSpace(r.system))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		m := map[string]any{"system": r.system, "score": r.scoreText}
		if r.detail != "" {
			m["detail"] = r.detail
		}
		out = append(out, m)
		if len(out) == n {
			break
		}
	}
	return out
}

func benchResult(asOf, source, sourceURL, metric, note string, rows []map[string]any) map[string]any {
	return map[string]any{
		"as_of": asOf, "retrieved": time.Now().UTC().Format("2006-01-02"),
		"source": source, "source_url": sourceURL, "metric": metric, "note": note, "rows": rows,
	}
}

func benchNum(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// hfRows reads rows from the Hugging Face dataset viewer API.
func hfRows(dataset, config, split string, offset, length int) ([]map[string]any, int, error) {
	q := url.Values{"dataset": {dataset}, "config": {config}, "split": {split},
		"offset": {strconv.Itoa(offset)}, "length": {strconv.Itoa(length)}}
	b, err := benchGet("https://datasets-server.huggingface.co/rows?" + q.Encode())
	if err != nil {
		return nil, 0, err
	}
	var d struct {
		Rows []struct {
			Row map[string]any `json:"row"`
		} `json:"rows"`
		Total int `json:"num_rows_total"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, 0, err
	}
	out := make([]map[string]any, 0, len(d.Rows))
	for _, r := range d.Rows {
		out = append(out, r.Row)
	}
	return out, d.Total, nil
}

func benchFetchLMArena() (map[string]any, error) {
	rows, _, err := hfRows("lmarena-ai/leaderboard-dataset", "text_style_control", "latest", 0, 100)
	if err != nil {
		return nil, err
	}
	var rs []benchRow
	pub := ""
	for _, r := range rows {
		if fmt.Sprint(r["category"]) != "overall" {
			continue
		}
		rating, ok := benchNum(r["rating"])
		name := fmt.Sprint(r["model_name"])
		if !ok || name == "" || rating < 500 || rating > 3000 {
			continue
		}
		if d := fmt.Sprint(r["leaderboard_publish_date"]); d > pub {
			pub = d
		}
		org := fmt.Sprint(r["organization"])
		votes, _ := benchNum(r["vote_count"])
		rank, _ := benchNum(r["rank"])
		detail := fmt.Sprintf("rank %d, %s, %.0f votes", int(rank), org, votes)
		rs = append(rs, benchRow{system: name, score: rating, scoreText: fmt.Sprintf("%.0f", rating), detail: detail})
	}
	if len(rs) < 10 || pub == "" {
		return nil, fmt.Errorf("%d overall rows, expected 10 or more, the dataset shape may have changed", len(rs))
	}
	return benchResult(pub, "LMArena official leaderboard dataset (lmarena-ai/leaderboard-dataset)",
		"https://lmarena.ai/leaderboard/text",
		"Arena rating, text arena, overall, style control on (Bradley-Terry, relative)",
		"Ratings are relative to the other models on the board, and models within a few points of each other are statistically tied; the rank shown is LMArena's own.",
		benchTop(rs, 10)), nil
}

var (
	lbBundleRe   = regexp.MustCompile(`src="\./(static/js/main\.[0-9a-f]+\.js)"`)
	lbReleasesRe = regexp.MustCompile(`\["2024-06-24"(?:,"\d{4}-\d{2}-\d{2}")*\]`)
	lbDateRe     = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
)

func benchFetchLiveBench() (map[string]any, error) {
	home, err := benchGet("https://livebench.ai/")
	if err != nil {
		return nil, err
	}
	m := lbBundleRe.FindSubmatch(home)
	if m == nil {
		return nil, fmt.Errorf("no app bundle on livebench.ai")
	}
	js, err := benchGet("https://livebench.ai/" + string(m[1]))
	if err != nil {
		return nil, err
	}
	rel := lbReleasesRe.Find(js)
	if rel == nil {
		return nil, fmt.Errorf("no release list in the LiveBench bundle")
	}
	dates := lbDateRe.FindAll(rel, -1)
	release := string(dates[len(dates)-1])
	key := strings.ReplaceAll(release, "-", "_")
	tbl, err := benchGet("https://livebench.ai/table_" + key + ".csv")
	if err != nil {
		return nil, err
	}
	catRaw, err := benchGet("https://livebench.ai/categories_" + key + ".json")
	if err != nil {
		return nil, err
	}
	var cats map[string][]string
	if err := json.Unmarshal(catRaw, &cats); err != nil {
		return nil, err
	}
	recs, err := csv.NewReader(bytes.NewReader(tbl)).ReadAll()
	if err != nil || len(recs) < 11 {
		return nil, fmt.Errorf("table_%s.csv: %d rows, %v", key, len(recs), err)
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[h] = i
	}
	var rs []benchRow
	for _, rec := range recs[1:] {
		var catSum float64
		nCat := 0
		for _, tasks := range cats {
			var s float64
			n := 0
			for _, t := range tasks {
				if i, ok := col[t]; ok && i < len(rec) {
					if v, err := strconv.ParseFloat(rec[i], 64); err == nil {
						s += v
						n++
					}
				}
			}
			if n == len(tasks) && n > 0 {
				catSum += s / float64(n)
				nCat++
			}
		}
		// A model missing a category is not ranked against complete ones.
		if nCat != len(cats) || nCat == 0 {
			continue
		}
		avg := catSum / float64(nCat)
		rs = append(rs, benchRow{system: rec[0], score: avg, scoreText: fmt.Sprintf("%.1f", avg)})
	}
	if len(rs) < 10 {
		return nil, fmt.Errorf("%d complete models in the %s release, expected 10 or more", len(rs), release)
	}
	names := make([]string, 0, len(cats))
	for c := range cats {
		names = append(names, c)
	}
	sort.Strings(names)
	return benchResult(release, "LiveBench published results, release "+release, "https://livebench.ai/",
		"Global average: the mean of LiveBench's category averages ("+strings.Join(names, ", ")+"), computed from the published table",
		"LiveBench publishes in releases with new questions each time; scores are comparable within a release, not across releases.",
		benchTop(rs, 10)), nil
}

// xlsxRows reads the first sheet of an xlsx file as rows of column letter to
// cell text. Shared strings are resolved; nothing else is interpreted.
func xlsxRows(raw []byte) ([]map[string]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	read := func(name string) ([]byte, error) {
		for _, f := range zr.File {
			if f.Name == name {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, fmt.Errorf("%s not in workbook", name)
	}
	var shared []string
	if b, err := read("xl/sharedStrings.xml"); err == nil {
		var sst struct {
			SI []struct {
				T string `xml:"t"`
				R []struct {
					T string `xml:"t"`
				} `xml:"r"`
			} `xml:"si"`
		}
		if xml.Unmarshal(b, &sst) == nil {
			for _, si := range sst.SI {
				s := si.T
				for _, r := range si.R {
					s += r.T
				}
				shared = append(shared, s)
			}
		}
	}
	b, err := read("xl/worksheets/sheet1.xml")
	if err != nil {
		return nil, err
	}
	var ws struct {
		Rows []struct {
			C []struct {
				R string `xml:"r,attr"`
				T string `xml:"t,attr"`
				V string `xml:"v"`
				I string `xml:"is>t"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal(b, &ws); err != nil {
		return nil, err
	}
	out := make([]map[string]string, 0, len(ws.Rows))
	for _, r := range ws.Rows {
		row := map[string]string{}
		for _, c := range r.C {
			colName := strings.TrimRight(c.R, "0123456789")
			v := c.V
			switch c.T {
			case "s":
				if i, err := strconv.Atoi(v); err == nil && i >= 0 && i < len(shared) {
					v = shared[i]
				}
			case "inlineStr":
				v = c.I
			}
			row[colName] = v
		}
		out = append(out, row)
	}
	return out, nil
}

// excelDate turns a spreadsheet serial day into a date.
func excelDate(s string) string {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 30000 || f > 80000 {
		return ""
	}
	return time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(f)).Format("2006-01-02")
}

func benchFetchOSWorld() (map[string]any, error) {
	raw, err := benchGet("https://os-world.github.io/static/data/osworld_verified_results.xlsx")
	if err != nil {
		return nil, err
	}
	rows, err := xlsxRows(raw)
	if err != nil || len(rows) < 20 {
		return nil, fmt.Errorf("osworld sheet: %d rows, %v", len(rows), err)
	}
	head := rows[0]
	find := func(name string) string {
		for k, v := range head {
			if strings.EqualFold(strings.TrimSpace(v), name) {
				return k
			}
		}
		return ""
	}
	cModel, cInst, cSteps, cDate, cRate := find("Model"), find("Institution"), find("Max steps"), find("Date"), find("Success rate")
	if cModel == "" || cRate == "" {
		return nil, fmt.Errorf("osworld sheet has no Model or Success rate column")
	}
	var rs []benchRow
	newest := ""
	for _, r := range rows[1:] {
		v, err := strconv.ParseFloat(r[cRate], 64)
		if err != nil || v <= 0 || v > 100 || r[cModel] == "" {
			continue
		}
		d := excelDate(r[cDate])
		if d > newest {
			newest = d
		}
		var parts []string
		if s := r[cInst]; s != "" {
			parts = append(parts, s)
		}
		if s := r[cSteps]; s != "" {
			parts = append(parts, s+" steps")
		}
		if d != "" {
			parts = append(parts, "submitted "+d)
		}
		rs = append(rs, benchRow{system: r[cModel], score: v, scoreText: fmt.Sprintf("%.1f%%", v), detail: strings.Join(parts, ", ")})
	}
	if len(rs) < 10 || newest == "" {
		return nil, fmt.Errorf("osworld: %d scored rows", len(rs))
	}
	return benchResult(newest, "OSWorld-Verified results published by the OSWorld team", "https://os-world.github.io/",
		"Success rate on the 361 OSWorld-Verified tasks, best entry per system",
		"Entries differ in step budget and in whether the result was verified by the OSWorld team or reported by the submitter; the step budget is shown with each entry.",
		benchTop(rs, 10)), nil
}

func benchFetchGAIA() (map[string]any, error) {
	var all []map[string]any
	total := 1
	for off := 0; off < total && off < 10000; off += 100 {
		rows, t, err := hfRows("gaia-benchmark/results_public", "2023", "test", off, 100)
		if err != nil {
			return nil, err
		}
		total = t
		all = append(all, rows...)
		if len(rows) == 0 {
			break
		}
	}
	var rs []benchRow
	newest := ""
	for _, r := range all {
		v, ok := benchNum(r["score"])
		name := strings.TrimSpace(fmt.Sprint(r["model"]))
		if !ok || name == "" || v <= 0 || v > 1 {
			continue
		}
		d := fmt.Sprint(r["date"])
		if lbDateRe.MatchString(d) && d > newest {
			newest = d[:10]
		}
		var parts []string
		if s := strings.TrimSpace(fmt.Sprint(r["organisation"])); s != "" && s != "<nil>" {
			parts = append(parts, s)
		}
		if s := strings.TrimSpace(fmt.Sprint(r["model_family"])); s != "" && s != "<nil>" {
			parts = append(parts, "models: "+trunc(s, 80))
		}
		if lbDateRe.MatchString(d) {
			parts = append(parts, "submitted "+d[:10])
		}
		rs = append(rs, benchRow{system: name, score: v * 100, scoreText: fmt.Sprintf("%.1f%%", v*100), detail: strings.Join(parts, ", ")})
	}
	if len(rs) < 10 || newest == "" {
		return nil, fmt.Errorf("gaia: %d scored rows of %d", len(rs), len(all))
	}
	return benchResult(newest, "GAIA public leaderboard results (gaia-benchmark/results_public)",
		"https://huggingface.co/spaces/gaia-benchmark/leaderboard",
		"Accuracy on the GAIA test set, all three levels",
		"Entries are complete agent systems as submitted, often combining several models; the models each submitter lists are shown with the entry.",
		benchTop(rs, 10)), nil
}

func benchFetchTau() (map[string]any, error) {
	const base = "https://sierra-tau-bench-public.s3.us-west-2.amazonaws.com/submissions"
	b, err := benchGet(base + "/manifest.json")
	if err != nil {
		return nil, err
	}
	var man struct {
		Submissions []string `json:"submissions"`
	}
	if err := json.Unmarshal(b, &man); err != nil || len(man.Submissions) == 0 {
		return nil, fmt.Errorf("tau manifest: %v", err)
	}
	var rs []benchRow
	newest := ""
	for _, s := range man.Submissions {
		raw, err := benchGet(base + "/" + url.PathEscape(s) + "/submission.json")
		if err != nil {
			continue
		}
		var sub struct {
			Model   string                    `json:"model_name"`
			Org     string                    `json:"model_organization"`
			By      string                    `json:"submitting_organization"`
			Date    string                    `json:"submission_date"`
			Type    string                    `json:"submission_type"`
			Results map[string]map[string]any `json:"results"`
		}
		if json.Unmarshal(raw, &sub) != nil || sub.Model == "" {
			continue
		}
		// Since May 2026 every submission reports the banking_knowledge
		// domain and most report nothing else, so it is the domain on which
		// current models can be compared.
		avg, ok := benchNum(sub.Results["banking_knowledge"]["pass_1"])
		if !ok || avg <= 0 || avg > 100 {
			continue
		}
		if sub.Date > newest {
			newest = sub.Date
		}
		detail := sub.Org
		if sub.By != "" && sub.By != sub.Org {
			detail += ", submitted by " + sub.By
		}
		if sub.Type != "" && sub.Type != "standard" {
			detail += ", " + sub.Type + " submission"
		}
		if sub.Date != "" {
			detail += ", " + sub.Date
		}
		rs = append(rs, benchRow{system: sub.Model, score: avg, scoreText: fmt.Sprintf("%.1f%%", avg), detail: strings.TrimPrefix(detail, ", ")})
	}
	if len(rs) < 10 || newest == "" {
		return nil, fmt.Errorf("tau: %d complete submissions of %d", len(rs), len(man.Submissions))
	}
	return benchResult(newest, "tau2-bench leaderboard submissions published by Sierra", "https://taubench.com/",
		"pass^1 on the banking_knowledge domain: the share of tasks solved on a single try",
		"Taken from the submissions behind taubench.com. Since May 2026 submissions report the banking_knowledge domain only, so the earlier airline, retail and telecom scores are not shown; the submitter and date are shown with each entry.",
		benchTop(rs, 10)), nil
}

var waMonthRe = regexp.MustCompile(`^(\d{1,2})/(\d{4})$`)

func benchFetchWebArena() (map[string]any, error) {
	raw, err := benchGet("https://docs.google.com/spreadsheets/d/1M801lEpBbKSNwP-vDBkC_pF7LdyGU1f_ufZb_NWNBZQ/export?format=csv")
	if err != nil {
		return nil, err
	}
	r := csv.NewReader(bytes.NewReader(raw))
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil || len(recs) < 11 {
		return nil, fmt.Errorf("webarena sheet: %d rows, %v", len(recs), err)
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[strings.TrimSpace(h)] = i
	}
	iModel, ok1 := col["Model"]
	iRate, ok2 := col["Success Rate (%)"]
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("webarena sheet columns changed: %v", recs[0])
	}
	iSrc, hasSrc := col["Result Source"]
	newest := ""
	var rs []benchRow
	for _, rec := range recs[1:] {
		if iRate >= len(rec) || iModel >= len(rec) {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(rec[iRate]), 64)
		if err != nil || v <= 0 || v > 100 || strings.TrimSpace(rec[iModel]) == "" {
			continue
		}
		var parts []string
		if m := waMonthRe.FindStringSubmatch(strings.TrimSpace(rec[0])); m != nil {
			mo, _ := strconv.Atoi(m[1])
			d := fmt.Sprintf("%s-%02d", m[2], mo)
			if d > newest {
				newest = d
			}
			parts = append(parts, d)
		}
		if hasSrc && iSrc < len(rec) && strings.TrimSpace(rec[iSrc]) != "" {
			parts = append(parts, "source: "+strings.TrimSpace(rec[iSrc]))
		}
		rs = append(rs, benchRow{system: strings.TrimSpace(rec[iModel]), score: v, scoreText: fmt.Sprintf("%.1f%%", v), detail: strings.Join(parts, ", ")})
	}
	if len(rs) < 10 || newest == "" {
		return nil, fmt.Errorf("webarena: %d scored rows", len(rs))
	}
	return benchResult(newest+"-01", "WebArena leaderboard maintained by the WebArena team (linked from webarena.dev)", "https://webarena.dev/",
		"Task success rate on the 812 WebArena tasks",
		"The board mixes results verified by the WebArena team with self-reported ones; the source of each result is shown with the entry. Dates are the month of the result.",
		benchTop(rs, 10)), nil
}

func benchFetchMETR() (map[string]any, error) {
	raw, err := benchGet("https://metr.org/assets/benchmark_results_1_1.yaml")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Name    string `yaml:"benchmark_name"`
		Results map[string]struct {
			Metrics struct {
				P50 struct {
					Estimate float64 `yaml:"estimate"`
					Low      float64 `yaml:"ci_low"`
					High     float64 `yaml:"ci_high"`
				} `yaml:"p50_horizon_length"`
			} `yaml:"metrics"`
			Release string `yaml:"release_date"`
		} `yaml:"results"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	dur := func(min float64) string {
		if min >= 120 {
			return fmt.Sprintf("%.1f hours", min/60)
		}
		return fmt.Sprintf("%.0f minutes", min)
	}
	var rs []benchRow
	newest := ""
	for name, r := range doc.Results {
		e := r.Metrics.P50.Estimate
		if e <= 0 {
			continue
		}
		if r.Release > newest {
			newest = r.Release
		}
		detail := fmt.Sprintf("95%% CI %s to %s", dur(r.Metrics.P50.Low), dur(r.Metrics.P50.High))
		if r.Release != "" {
			detail += ", model released " + r.Release
		}
		rs = append(rs, benchRow{system: name, score: e, scoreText: dur(e), detail: detail})
	}
	if len(rs) < 10 || newest == "" {
		return nil, fmt.Errorf("metr: %d models", len(rs))
	}
	return benchResult(newest, "METR time horizon results ("+doc.Name+")", "https://metr.org/time-horizons/",
		"50% time horizon: the length of task, in human expert time, the model completes with 50% reliability",
		"The 95% confidence interval METR publishes is shown with each model; intervals are wide, so models with overlapping intervals are not clearly ranked.",
		benchTop(rs, 10)), nil
}

// benchMLPerfWatch confirms an MLPerf snapshot is still on the newest round,
// and returns the newer round when it is not.
func benchMLPerfWatch(prefix, asOf string) (confirmed bool, newer string, err error) {
	newestName, newestAt := "", ""
	for page := 1; page <= 4; page++ {
		b, err := benchGet(fmt.Sprintf("https://api.github.com/orgs/mlcommons/repos?per_page=100&page=%d", page))
		if err != nil {
			return false, "", err
		}
		var repos []struct {
			Name    string `json:"name"`
			Created string `json:"created_at"`
		}
		if json.Unmarshal(b, &repos) != nil || len(repos) == 0 {
			break
		}
		for _, r := range repos {
			if strings.HasPrefix(r.Name, prefix) && r.Created > newestAt {
				newestName, newestAt = r.Name, r.Created
			}
		}
	}
	if newestName == "" {
		return false, "", fmt.Errorf("no %s repository found", prefix)
	}
	if len(newestAt) >= 10 && len(asOf) >= 10 && newestAt[:10] > asOf[:10] {
		return false, newestName + " (" + newestAt[:10] + ")", nil
	}
	return true, "", nil
}

// benchRefreshAll runs the refreshers of row 438 inside bench_results.
func benchRefreshAll(db *sql.DB) int {
	updated := 0
	for slug, fetch := range map[string]func() (map[string]any, error){
		"lmarena": benchFetchLMArena, "livebench": benchFetchLiveBench, "osworld": benchFetchOSWorld,
		"gaia": benchFetchGAIA, "tau-bench": benchFetchTau, "webarena": benchFetchWebArena,
		"metr-time-horizon": benchFetchMETR,
	} {
		// Once a day is plenty for boards that move weekly.
		var last string
		db.QueryRow(`SELECT COALESCE(results->>'retrieved','') FROM twoai_benchmarks WHERE slug=$1`, slug).Scan(&last)
		if last == time.Now().UTC().Format("2006-01-02") {
			continue
		}
		res, err := fetch()
		if err != nil {
			fmt.Printf("bench_results: %s: %v (keeping the last snapshot)\n", slug, err)
			continue
		}
		// A note the editor wrote for this board is kept over the default.
		var oldNote string
		db.QueryRow(`SELECT COALESCE(results->>'note','') FROM twoai_benchmarks WHERE slug=$1`, slug).Scan(&oldNote)
		if strings.TrimSpace(oldNote) != "" {
			res["note"] = oldNote
		}
		if err := benchStore(db, slug, res); err != nil {
			fmt.Printf("bench_results: %s: store: %v\n", slug, err)
			continue
		}
		fmt.Printf("bench_results: %s refreshed, as of %v, %d rows\n", slug, res["as_of"], len(res["rows"].([]map[string]any)))
		updated++
	}
	for slug, prefix := range map[string]string{"mlperf-inference": "inference_results_v", "mlperf-training": "training_results_v"} {
		var asOf string
		db.QueryRow(`SELECT COALESCE(left(results->>'as_of',10),'') FROM twoai_benchmarks WHERE slug=$1`, slug).Scan(&asOf)
		ok, newer, err := benchMLPerfWatch(prefix, asOf)
		switch {
		case err != nil:
			fmt.Printf("bench_results: %s round watch: %v\n", slug, err)
		case ok:
			db.Exec(`UPDATE twoai_benchmarks SET results = results || jsonb_build_object('retrieved', current_date::text) WHERE slug=$1 AND results IS NOT NULL`, slug)
		default:
			db.Exec(`UPDATE twoai_benchmarks SET results = results || jsonb_build_object('newer_round', $2::text) WHERE slug=$1 AND results IS NOT NULL`, slug, newer)
			fmt.Printf("bench_results: %s: MLCommons published %s after the %s snapshot, needs a refresh\n", slug, newer, asOf)
		}
	}
	return updated
}

// famBenchDistinct are family line names that are not ordinary words. Any
// other line name (Command, Solar, Seed) counts for a benchmark row only when
// the row names the developer too.
var famBenchDistinct = map[string]bool{"claude": true, "gemini": true, "gpt": true, "grok": true, "qwen": true,
	"kimi": true, "glm": true, "deepseek": true, "nemotron": true, "mimo": true, "minimax": true}

// famBenchMatch reports whether a benchmark row (its system name and detail)
// is a model of the family with this line and developer.
func famBenchMatch(line, dev, system, detail string) bool {
	if line == "" || system == "" {
		return false
	}
	// METR's ids join words with underscores (claude_opus_4_6_inspect),
	// which a word boundary does not split.
	system = strings.ReplaceAll(system, "_", " ")
	if !regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(line) + `\b`).MatchString(system) {
		return false
	}
	if famBenchDistinct[strings.ToLower(line)] || strings.EqualFold(dev, line) {
		return true
	}
	hay := strings.ToLower(system + " " + detail)
	return dev != "" && strings.Contains(hay, strings.ToLower(dev))
}

// benchSyncPages writes every benchmark row to its page, so a row added to
// twoai_benchmarks gets a page (metr-time-horizon, re-bench and hcast had
// none, row 438), and links each result row to its model family page. The
// page date is the later of the editor's review and the day the results
// were read, never the build date, so a stale board cannot look current.
func benchSyncPages(db *sql.DB) error {
	type fam struct{ line, dev, name, href string }
	var fams []fam
	if fr, err := db.Query(`SELECT data->>'line', COALESCE(data->>'developer',''), data->>'name', data->>'uid', COALESCE(data->>'hub_path','')
		FROM twoai_pages WHERE path LIKE 'tech/family-%'`); err == nil {
		for fr.Next() {
			var f fam
			var uid, hub string
			if fr.Scan(&f.line, &f.dev, &f.name, &uid, &hub) == nil && hub != "" {
				hub = strings.TrimSuffix(hub, "/")
				f.href = hub[:strings.LastIndex(hub, "/")+1] + uid + "/"
				fams = append(fams, f)
			}
		}
		fr.Close()
	}
	sectionNames := map[string]string{"bench-model": "Model Benchmarks", "bench-hardware": "Hardware Benchmarks",
		"bench-frameworks": "Framework and Inference Benchmarks", "bench-vector-db": "Vector Database Benchmarks",
		"bench-agents": "Agent Benchmarks"}
	rows, err := db.Query(`SELECT slug, ((to_jsonb(b) - 'updated_at' - 'last_reviewed')
			|| jsonb_build_object('last_reviewed', b.last_reviewed::date::text))::text
		FROM twoai_benchmarks b ORDER BY section, sort, slug`)
	if err != nil {
		return err
	}
	type item struct {
		slug string
		doc  map[string]any
	}
	var items []item
	for rows.Next() {
		var slug, raw string
		var doc map[string]any
		if rows.Scan(&slug, &raw) != nil || json.Unmarshal([]byte(raw), &doc) != nil {
			continue
		}
		items = append(items, item{slug, doc})
	}
	rows.Close()
	now := time.Now().Format(time.RFC3339)
	written := 0
	for _, it := range items {
		doc := it.doc
		for k, v := range doc {
			if v == nil {
				delete(doc, k)
			}
		}
		sec, _ := doc["section"].(string)
		doc["section_name"] = sectionNames[sec]
		gen, _ := doc["last_reviewed"].(string)
		if res, ok := doc["results"].(map[string]any); ok {
			if r, _ := res["retrieved"].(string); len(r) >= 10 && r[:10] > gen {
				gen = r[:10]
			}
			if rs, ok := res["rows"].([]any); ok {
				for _, x := range rs {
					row, ok := x.(map[string]any)
					if !ok {
						continue
					}
					sys, _ := row["system"].(string)
					det, _ := row["detail"].(string)
					delete(row, "family")
					for _, f := range fams {
						if famBenchMatch(f.line, f.dev, sys, det) {
							row["family"] = map[string]string{"name": f.name, "href": f.href}
							break
						}
					}
				}
			}
		}
		if gen == "" {
			gen = time.Now().Format("2006-01-02")
		}
		path := "benchmarks/" + it.slug + ".json"
		doc["generated"] = gen
		doc["built_at"] = now
		doc["page_uid"] = twoaiUID("page:" + path)
		j, _ := json.Marshal(doc)
		res, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, updated_at) VALUES ($1, 'benchmark', $2::jsonb, now())
			ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data || jsonb_build_object('refresh_every_days', twoai_pages.data->'refresh_every_days'), updated_at = now()
			WHERE (twoai_pages.data - 'built_at' - 'refresh_every_days') IS DISTINCT FROM (EXCLUDED.data - 'built_at' - 'refresh_every_days')`,
			path, string(j))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			written++
		}
	}
	// The hub lists every benchmark by section; the rest of its document is
	// the editor's and is kept.
	order := []string{"bench-model", "bench-hardware", "bench-frameworks", "bench-vector-db", "bench-agents"}
	var sections []map[string]any
	for _, s := range order {
		var list []map[string]any
		for _, it := range items {
			if it.doc["section"] == s {
				list = append(list, map[string]any{"name": it.doc["name"], "slug": it.slug, "measures": it.doc["measures"], "maintainer": it.doc["maintainer"]})
			}
		}
		if len(list) > 0 {
			sections = append(sections, map[string]any{"name": sectionNames[s], "slug": s, "items": list})
		}
	}
	sj, _ := json.Marshal(sections)
	db.Exec(`UPDATE twoai_pages SET data = data || jsonb_build_object('sections', $1::jsonb, 'total', $2::int), updated_at = now()
		WHERE path = 'benchmarks/index.json' AND (data->'sections' IS DISTINCT FROM $1::jsonb OR (data->>'total')::int IS DISTINCT FROM $2::int)`,
		string(sj), len(items))
	fmt.Printf("bench_pages: %d benchmark pages, %d rewritten ok=true\n", len(items), written)
	return nil
}
