package main

// twoai_cwe: MITRE's weakness list, a page for every weakness class that a
// tracked AI CVE carries, and the ranked list of them.
//
// theworldofai, bridge row 385 (Stephen, 2026-10-02): track CWE numbers the
// way CVE numbers are tracked. The Ask box, asked what a CWE number is, had no
// page to answer from and cited a data centre section instead. Our 614 AI CVEs
// fall into 107 weakness classes, and the ranking is a story of its own: AI
// software is failing on old, well-known web flaws.
//
// Source: MITRE's Research Concepts view, cwe.mitre.org/data/csv/1000.csv.zip,
// 944 weaknesses. CWE releases a few times a year, so the file is fetched when
// the table is empty or a month old, not every run. The CWE terms of use allow
// reproduction with attribution: the name, definition and MITRE's mitigations
// are quoted and attributed, everything else on the page is written here.
// CWE-189 and other categories are not weaknesses and are not in the view, so
// they get no page; a CVE classed under one keeps its plain CWE label.
//
// Paths: news/cwe-CWE-918.json per weakness, news/cwes.json for the list.
// Pages render at /ai-news/cwes/CWE-918/; the ranked list renders on its own
// page under Application and Product Security,
// /ai-ecosystem/enterprise-applications-governance-and-tools/aa6058ad/
// (Stephen, 2026-10-03; uid sha256("cwe-list")[:8]), and /ai-news/cwes/
// redirects there.

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
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

const cweCSVURL = "https://cwe.mitre.org/data/csv/1000.csv.zip"

type cweMitigation struct {
	Phase         []string `json:"phase,omitempty"`
	Strategy      string   `json:"strategy,omitempty"`
	Description   string   `json:"description"`
	Effectiveness string   `json:"effectiveness,omitempty"`
}

type cweConsequence struct {
	Scope  []string `json:"scope,omitempty"`
	Impact []string `json:"impact,omitempty"`
	Note   string   `json:"note,omitempty"`
}

type cweAlternate struct {
	Term        string `json:"term"`
	Description string `json:"description,omitempty"`
}

func cweEnsureTable(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_cwes (
		cwe_id text PRIMARY KEY, num int NOT NULL, uid text NOT NULL, name text NOT NULL,
		abstraction text, status text, description text, extended_description text, likelihood text,
		consequences jsonb, mitigations jsonb, alternate_terms jsonb,
		fetched_at timestamptz NOT NULL DEFAULT now(),
		ai_text text, ai_model text, ai_written_on date, ai_hash text, ai_attempts int NOT NULL DEFAULT 0)`)
}

// cweFields splits one of MITRE's list fields, "::KEY:value:KEY:value::KEY:...",
// into entries of key to values. A value can hold a colon (a URL, "C++ std::"),
// so the field is cut only at the known keys: "::" before a key starts a new
// entry, ":" before a key starts a new pair in the same entry.
func cweFields(field string, keys ...string) []map[string][]string {
	if strings.TrimSpace(field) == "" {
		return nil
	}
	re := regexp.MustCompile(`(::|:|^)(` + strings.Join(keys, "|") + `):`)
	locs := re.FindAllStringSubmatchIndex(field, -1)
	var out []map[string][]string
	var cur map[string][]string
	for i, l := range locs {
		sep := field[l[2]:l[3]]
		key := field[l[4]:l[5]]
		end := len(field)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		val := strings.TrimSpace(strings.TrimSuffix(field[l[1]:end], "::"))
		if cur == nil || sep == "::" || sep == "" {
			cur = map[string][]string{}
			out = append(out, cur)
		}
		if val != "" {
			cur[key] = append(cur[key], val)
		}
	}
	return out
}

func cweFirst(m map[string][]string, k string) string {
	if v := m[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// twoaiCWELoad fetches MITRE's list when the table is empty or a month old
// and upserts every weakness. The model-written paragraph is kept.
func twoaiCWELoad(db *sql.DB) int {
	cweEnsureTable(db)
	var n int
	var newest sql.NullTime
	db.QueryRow(`SELECT count(*), max(fetched_at) FROM twoai_cwes`).Scan(&n, &newest)
	if n > 0 && newest.Valid && time.Since(newest.Time) < 30*24*time.Hour {
		return 0
	}
	req, _ := http.NewRequest("GET", cweCSVURL, nil)
	req.Header.Set("User-Agent", "theworldofai.org pipeline (srj@srjconsultingservices.com)")
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_cwe load:", err)
		return 0
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 40<<20))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "twoai_cwe load: HTTP %d\n", resp.StatusCode)
		return 0
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil || len(zr.File) == 0 {
		fmt.Fprintln(os.Stderr, "twoai_cwe load: not a zip:", err)
		return 0
	}
	f, err := zr.File[0].Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_cwe load:", err)
		return 0
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	head, err := r.Read()
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_cwe load: header:", err)
		return 0
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimPrefix(strings.TrimSpace(h), string(rune(0xFEFF)))] = i
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	loaded := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		num, perr := strconv.Atoi(get(rec, "CWE-ID"))
		name := get(rec, "Name")
		if perr != nil || name == "" {
			continue
		}
		id := "CWE-" + strconv.Itoa(num)
		mits := []cweMitigation{}
		for _, m := range cweFields(get(rec, "Potential Mitigations"), "PHASE", "STRATEGY", "DESCRIPTION", "EFFECTIVENESS_NOTES", "EFFECTIVENESS") {
			if d := cweFirst(m, "DESCRIPTION"); d != "" {
				mits = append(mits, cweMitigation{Phase: m["PHASE"], Strategy: cweFirst(m, "STRATEGY"), Description: d, Effectiveness: cweFirst(m, "EFFECTIVENESS")})
			}
		}
		cons := []cweConsequence{}
		for _, m := range cweFields(get(rec, "Common Consequences"), "SCOPE", "IMPACT", "LIKELIHOOD", "NOTE") {
			cons = append(cons, cweConsequence{Scope: m["SCOPE"], Impact: m["IMPACT"], Note: cweFirst(m, "NOTE")})
		}
		alts := []cweAlternate{}
		for _, m := range cweFields(get(rec, "Alternate Terms"), "TERM", "DESCRIPTION") {
			if t := cweFirst(m, "TERM"); t != "" {
				alts = append(alts, cweAlternate{Term: t, Description: cweFirst(m, "DESCRIPTION")})
			}
		}
		mj, _ := json.Marshal(mits)
		cj, _ := json.Marshal(cons)
		aj, _ := json.Marshal(alts)
		if _, err := db.Exec(`INSERT INTO twoai_cwes (cwe_id, num, uid, name, abstraction, status, description, extended_description, likelihood, consequences, mitigations, alternate_terms, fetched_at)
			VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),$10::jsonb,$11::jsonb,$12::jsonb,now())
			ON CONFLICT (cwe_id) DO UPDATE SET name=EXCLUDED.name, abstraction=EXCLUDED.abstraction, status=EXCLUDED.status,
				description=EXCLUDED.description, extended_description=EXCLUDED.extended_description, likelihood=EXCLUDED.likelihood,
				consequences=EXCLUDED.consequences, mitigations=EXCLUDED.mitigations, alternate_terms=EXCLUDED.alternate_terms, fetched_at=now()`,
			id, num, twoaiUID("cwe:"+id), name, get(rec, "Weakness Abstraction"), get(rec, "Status"), get(rec, "Description"),
			get(rec, "Extended Description"), get(rec, "Likelihood of Exploit"), string(cj), string(mj), string(aj)); err == nil {
			loaded++
		}
	}
	fmt.Printf("twoai_cwe: loaded %d weaknesses from MITRE ok=true\n", loaded)
	return loaded
}

type cweCVE struct {
	ID, Headline, Product, Severity, Published, Summary string
	Score                                               sql.NullFloat64
	KEV                                                 bool
}

// cweUsage returns the AI CVEs in each weakness class, newest first.
func cweUsage(db *sql.DB) map[string][]cweCVE {
	out := map[string][]cweCVE{}
	rows, err := db.Query(`SELECT cwe, cve_id, coalesce(headline,''), coalesce(product,''), coalesce(cvss_severity,''), coalesce(published::date::text,''),
			coalesce(description,''), cvss_score, kev
		FROM twoai_cves WHERE status IN ('published','approved') AND coalesce(cwe,'') LIKE 'CWE-%'
		ORDER BY published DESC NULLS LAST, cve_id`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_cwe usage:", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var cwe string
		var c cweCVE
		if rows.Scan(&cwe, &c.ID, &c.Headline, &c.Product, &c.Severity, &c.Published, &c.Summary, &c.Score, &c.KEV) == nil {
			out[cwe] = append(out[cwe], c)
		}
	}
	return out
}

// twoaiCWEWrite writes, for each weakness class that holds AI CVEs, one
// paragraph on what it looks like in AI software, from MITRE's definition
// and those CVEs only. Rewritten when the class's CVEs change and the text is
// over a month old. A bulk stage, so it waits for off-peak hours.
func twoaiCWEWrite(db *sql.DB) int {
	cweEnsureTable(db)
	perRun := 10
	if !twoaiPeakAt(time.Now()) {
		perRun = 40
	}
	if v := strings.TrimSpace(os.Getenv("TWOAI_CWE_WRITE_PER_RUN")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			perRun = n
		}
	}
	if perRun == 0 {
		return 0
	}
	usage := cweUsage(db)
	type cand struct {
		id, name, desc, hash string
		cves                 []cweCVE
	}
	var todo []cand
	rows, err := db.Query(`SELECT cwe_id, name, coalesce(description,''), coalesce(ai_hash,''), coalesce(ai_written_on::text,''), ai_attempts FROM twoai_cwes`)
	if err != nil {
		return 0
	}
	for rows.Next() {
		var id, name, desc, oldHash, written string
		var attempts int
		if rows.Scan(&id, &name, &desc, &oldHash, &written, &attempts) != nil {
			continue
		}
		cves := usage[id]
		if len(cves) == 0 || attempts >= 3 {
			continue
		}
		ids := []string{}
		for _, c := range cves {
			ids = append(ids, c.ID)
		}
		sort.Strings(ids)
		h := md5.Sum([]byte(strings.Join(ids, ",")))
		hash := hex.EncodeToString(h[:8])
		if hash == oldHash {
			continue
		}
		if written != "" {
			if t, perr := time.Parse("2006-01-02", written[:10]); perr == nil && time.Since(t) < 30*24*time.Hour {
				continue
			}
		}
		todo = append(todo, cand{id, name, desc, hash, cves})
	}
	rows.Close()
	sort.SliceStable(todo, func(i, j int) bool { return len(todo[i].cves) > len(todo[j].cves) })

	system := `You write for The World of AI, a reference site read by business leaders, developers and security teams. You are given one weakness class from MITRE's CWE list and the AI-related CVEs filed under it. Use only what is given.
Write one paragraph, 70 to 140 words, on what this weakness looks like in AI software specifically: which kinds of AI products the CVEs are in (agent frameworks, MCP servers, LLM applications, model serving, notebooks and so on, only as the CVEs show), the usual way it is reached in them, and what it lets an attacker do. Name two or three products from the CVEs as examples. Plain English, short sentences, commas rather than dashes, no markdown, no lists, no questions, no predictions, no advice to any named company, and never exploit detail, payloads or steps an attacker could reuse.
Return only JSON: {"text": "..."}`
	written, held := 0, 0
	for _, c := range todo {
		if written+held >= perRun {
			break
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Weakness: %s, %s\nMITRE definition: %s\n\nAI CVEs in this class (%d), newest first:\n", c.id, c.name, c.desc, len(c.cves))
		for i, v := range c.cves {
			if i >= 25 {
				break
			}
			line := v.Headline
			if line == "" {
				line = trunc(v.Summary, 200)
			}
			fmt.Fprintf(&sb, "- %s (%s): %s\n", v.ID, fallback(v.Product, "product not named"), line)
		}
		out, model, gerr := twoaiGenerate("cwe_writer", system, sb.String())
		if gerr != nil {
			if strings.Contains(gerr.Error(), "deferred") {
				break
			}
			continue
		}
		text := strings.TrimSpace(out)
		if i := strings.Index(text, "{"); i >= 0 {
			text = text[i:]
		}
		if k := strings.LastIndex(text, "}"); k >= 0 {
			text = text[:k+1]
		}
		var got struct {
			Text string `json:"text"`
		}
		para := ""
		if json.Unmarshal([]byte(text), &got) == nil {
			para = strings.TrimSpace(twoaiStripMarkdown(got.Text))
		}
		problem := ""
		switch n := len([]rune(para)); {
		case para == "":
			problem = "no text"
		case n < 250 || n > 1100:
			problem = "length"
		case strings.Contains(para, "?"):
			problem = "question"
		case strings.Contains(para, "—") || strings.Contains(para, " - "):
			problem = "dash"
		}
		if problem != "" {
			db.Exec(`UPDATE twoai_cwes SET ai_attempts = ai_attempts + 1 WHERE cwe_id=$1`, c.id)
			held++
			fmt.Printf("twoai_cwe_write: %s held (%s)\n", c.id, problem)
			continue
		}
		db.Exec(`UPDATE twoai_cwes SET ai_text=$2, ai_model=$3, ai_written_on=current_date, ai_hash=$4, ai_attempts=0 WHERE cwe_id=$1`, c.id, para, model, c.hash)
		written++
		fmt.Printf("twoai_cwe_write: %s: %s\n", c.id, trunc(para, 100))
	}
	fmt.Printf("twoai_cwe_write: candidates=%d written=%d held=%d ok=true\n", len(todo), written, held)
	return written + held
}

// twoaiCWEPages writes a page row for every weakness class that holds at
// least one AI CVE, the ranked list row, and the AI News menu entry.
func twoaiCWEPages(db *sql.DB) int {
	cweEnsureTable(db)
	today := time.Now().Format("2006-01-02")
	usage := cweUsage(db)
	type rowT struct {
		id, name, abstraction, status, desc, ext, likelihood, uid, aiText, aiModel, aiOn string
		num                                                                              int
		cons, mits, alts                                                                 string
	}
	known := map[string]rowT{}
	rows, err := db.Query(`SELECT cwe_id, num, uid, name, coalesce(abstraction,''), coalesce(status,''), coalesce(description,''), coalesce(extended_description,''),
			coalesce(likelihood,''), coalesce(consequences,'[]'::jsonb)::text, coalesce(mitigations,'[]'::jsonb)::text, coalesce(alternate_terms,'[]'::jsonb)::text,
			coalesce(ai_text,''), coalesce(ai_model,''), coalesce(ai_written_on::text,'')
		FROM twoai_cwes`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_cwe pages:", err)
		return 0
	}
	for rows.Next() {
		var r rowT
		if rows.Scan(&r.id, &r.num, &r.uid, &r.name, &r.abstraction, &r.status, &r.desc, &r.ext, &r.likelihood, &r.cons, &r.mits, &r.alts, &r.aiText, &r.aiModel, &r.aiOn) == nil {
			known[r.id] = r
		}
	}
	rows.Close()
	if len(known) == 0 {
		return 0
	}
	totalCVEs := 0
	var list []map[string]any
	var unlisted []map[string]any
	built := 0
	for id, cves := range usage {
		totalCVEs += len(cves)
		kev, crit, high := 0, 0, 0
		cl := []map[string]any{}
		for _, c := range cves {
			if c.KEV {
				kev++
			}
			switch strings.ToUpper(c.Severity) {
			case "CRITICAL":
				crit++
			case "HIGH":
				high++
			}
			var sc any
			if c.Score.Valid {
				sc = c.Score.Float64
			}
			cl = append(cl, map[string]any{"cve_id": c.ID, "headline": c.Headline, "product": c.Product, "cvss_score": sc,
				"cvss_severity": c.Severity, "kev": c.KEV, "published": c.Published, "summary": trunc(c.Summary, 220)})
		}
		r, ok := known[id]
		if !ok {
			// A category (CWE-189) or a retired id: listed, no page.
			unlisted = append(unlisted, map[string]any{"cwe_id": id, "count": len(cves)})
			continue
		}
		var cons, mits, alts any
		json.Unmarshal([]byte(r.cons), &cons)
		json.Unmarshal([]byte(r.mits), &mits)
		json.Unmarshal([]byte(r.alts), &alts)
		latest := ""
		if len(cves) > 0 {
			latest = cves[0].Published
		}
		doc := map[string]any{
			"shape": "cwe", "cwe_id": id, "num": r.num, "uid": r.uid, "name": r.name,
			"title":       id + ": " + r.name,
			"abstraction": r.abstraction, "status": r.status, "description": r.desc, "extended_description": r.ext,
			"likelihood": r.likelihood, "consequences": cons, "mitigations": mits, "alternate_terms": alts,
			"ai_text": r.aiText, "ai_model": r.aiModel, "ai_written_on": r.aiOn,
			"count": len(cves), "kev": kev, "critical": crit, "high": high, "latest": latest, "cves": cl,
			"mitre_url": fmt.Sprintf("https://cwe.mitre.org/data/definitions/%d.html", r.num),
			"generated": today,
		}
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count) VALUES ($1,'cwe',$2::jsonb,NULL,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data, url_count=1, updated_at=now()`,
			"news/cwe-"+id+".json", string(j)); err == nil {
			built++
		}
		list = append(list, map[string]any{"cwe_id": id, "num": r.num, "uid": r.uid, "name": r.name, "abstraction": r.abstraction,
			"count": len(cves), "kev": kev, "critical": crit, "high": high, "latest": latest,
			"summary": trunc(r.desc, 240)})
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i]["count"].(int), list[j]["count"].(int)
		if a != b {
			return a > b
		}
		return list[i]["num"].(int) < list[j]["num"].(int)
	})
	for i := range list {
		list[i]["rank"] = i + 1
	}
	var noCWE int
	db.QueryRow(`SELECT count(*) FROM twoai_cves WHERE status IN ('published','approved') AND coalesce(cwe,'') NOT LIKE 'CWE-%'`).Scan(&noCWE)
	lj, _ := json.Marshal(map[string]any{
		"shape": "cwe-list", "name": "The weaknesses behind AI vulnerabilities", "uid": twoaiUID("cwe-list"), "generated": today,
		"total": len(list), "cves_classed": totalCVEs, "cves_unclassed": noCWE, "mitre_total": len(known),
		"cwes": list, "unlisted": unlisted,
		"sources": map[string]string{"cwe": "https://cwe.mitre.org/", "list": cweCSVURL},
	})
	db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count) VALUES ('news/cwes.json','cwe-list',$1::jsonb,NULL,1)
		ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data, url_count=1, updated_at=now()`, string(lj))
	// No AI News menu entry: Stephen took Latest AI CWEs out of the menu on
	// 2026-10-03, and the row the first version inserted is retired.
	fmt.Printf("twoai_cwe: pages=%d classes_without_mitre_entry=%d cves_classed=%d unclassed=%d ok=true\n", built, len(unlisted), totalCVEs, noCWE)
	return built
}

// cweIndex is the name and mitigations of every weakness that has a page,
// for the CVE pages to link and quote.
type cweInfo struct {
	Name        string
	HasPage     bool
	Mitigations []cweMitigation
}

func cweIndex(db *sql.DB) map[string]cweInfo {
	out := map[string]cweInfo{}
	// Every weakness in MITRE's list that a tracked CVE carries gets a page
	// in the same run (twoaiCWEPages), so a known id is a page to link.
	rows, err := db.Query(`SELECT c.cwe_id, c.name, coalesce(c.mitigations,'[]'::jsonb)::text, true
		FROM twoai_cwes c WHERE c.cwe_id IN (SELECT DISTINCT cwe FROM twoai_cves WHERE cwe IS NOT NULL)`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, mraw string
		var has bool
		if rows.Scan(&id, &name, &mraw, &has) == nil {
			var m []cweMitigation
			json.Unmarshal([]byte(mraw), &m)
			out[id] = cweInfo{Name: name, HasPage: has, Mitigations: m}
		}
	}
	return out
}
