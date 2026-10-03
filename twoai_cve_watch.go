package main

// twoai_cve_watch: the AI CVE tracker. Stephen, 2026-10-01: track every CVE
// related to AI, show the latest ten at the foot of /ai-news/, keep every one
// on /ai-news/cves/ (nothing is deleted, the oldest only drops off the ten),
// give each CVE its own page, cross-reference each one on the page of the
// product it names, and let the Ask box read the lot.
//
// SOURCES. cve.org (MITRE's CVE Program) is the record of truth and the first
// link on every page. NVD, NIST's enriched copy of that list, is what is read
// here, because it carries the CVSS score, the CWE and the affected products
// and its API answers a "modified since" window. CISA's Known Exploited
// Vulnerabilities list flags the ones under attack. Press is never a source.
//
// WHAT QUALIFIES (Stephen, 2026-10-02):
//   1. The affected product is an AI product in twoai_cve_products, or a
//      tool, model, hardware item, MCP server or repository the site already
//      has a page for. These qualify on their own.
//   2. A company the site profiles is named AND the text passes the AI term
//      test. The literal company rule was measured and refused: Oracle alone
//      had 2,735 CVEs in 90 days, Google 1,448, none of them about AI.
//   3. The description passes the AI term test alone: proposed, not
//      published, until Stephen approves it (status 'approved').
// People are not a match key: names do not appear in CVE text.
//
// FIRST RUN backfills 90 days through NVD keyword searches, one per product
// term, at NVD's unauthenticated rate (one request per six seconds). Every
// later run reads the CVEs NVD modified since the last run.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

type cveEntity struct {
	Kind, Name, Href, UID string
	re                    *regexp.Regexp
	auto                  bool
}

type cveHit struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Href string `json:"href"`
	UID  string `json:"uid,omitempty"`
}

var cveGenericTerms = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\blarge language models?\b`), regexp.MustCompile(`(?i)\bprompt injection\b`),
	regexp.MustCompile(`\bLLMs?\b`), regexp.MustCompile(`\bAI agents?\b`), regexp.MustCompile(`(?i)\bmachine[\- ]learning\b`),
	regexp.MustCompile(`(?i)\bartificial intelligence\b`), regexp.MustCompile(`(?i)\bgenerative AI\b`),
	regexp.MustCompile(`\bAI assistants?\b`), regexp.MustCompile(`\bAI[\- ]powered\b`), regexp.MustCompile(`(?i)\bagentic\b`),
	regexp.MustCompile(`(?i)\bmodel context protocol\b`), regexp.MustCompile(`\bMCP servers?\b`),
}

// Names too common to be a match key on their own.
var cveStopNames = map[string]bool{"cursor": true, "command": true, "flash": true, "sonnet": true, "opus": true, "nova": true, "phi": true, "grok": true,
	"gemini": true, "claude": true, "copilot": true, "chatgpt": true, "openai": true, "anthropic": true, "google": true, "meta": true, "apple": true,
	"amazon": true, "microsoft": true, "nvidia": true, "intel": true, "oracle": true, "cisco": true, "ibm": true, "python": true, "rust": true, "java": true,
	"javascript": true, "typescript": true, "go": true, "node": true, "docker": true, "linux": true, "windows": true, "chrome": true, "android": true,
	"gpt": true, "llama": true, "mistral": true, "gemma": true, "qwen": true, "deepseek": true, "codex": true, "assistant": true, "agent": true, "agents": true,
	"search": true, "fetch": true, "memory": true, "filesystem": true, "github": true, "gitlab": true, "slack": true, "notion": true, "stripe": true, "jira": true,
	// theworldofai, bridge row 357 (2026-10-02), from the first run: these
	// MCP registry segments, display titles and repo names are ordinary words
	// and matched almost every CVE. "remote attacker" alone published one.
	"remote": true, "server": true, "servers": true, "check": true, "platform": true, "directory": true, "intelligence": true, "plugin": true,
	"public": true, "knowledge": true, "valid": true, "schema": true, "knowledge base": true, "offers": true, "roster": true, "linear": true,
	"mcp-server": true, "mcp-api": true, "mcp-proxy": true, "web-search": true, "ai-toolkit": true, "company-search": true, "control-plane": true,
	"knowledge-base": true, "registry": true, "continue": true, "swarm": true, "inspector": true}

// cveGenericWords are the words a product name can be made of without
// saying which product it is. An MCP server title made only of these
// ("Knowledge Base", "Web Search Server") is not a match key; one with a
// word outside the list ("Acme Knowledge Base") is.
var cveGenericWords = map[string]bool{
	"mcp": true, "server": true, "servers": true, "remote": true, "check": true, "platform": true, "directory": true, "intelligence": true,
	"plugin": true, "plugins": true, "public": true, "knowledge": true, "base": true, "valid": true, "schema": true, "offers": true, "roster": true,
	"linear": true, "api": true, "apis": true, "proxy": true, "web": true, "search": true, "ai": true, "toolkit": true, "company": true, "control": true,
	"plane": true, "tool": true, "tools": true, "service": true, "services": true, "data": true, "cloud": true, "app": true, "apps": true,
	"agent": true, "agents": true, "assistant": true, "chat": true, "code": true, "file": true, "files": true, "system": true, "manager": true,
	"client": true, "hub": true, "core": true, "framework": true, "engine": true, "model": true, "models": true, "open": true, "source": true,
	"docs": true, "documentation": true, "integration": true, "connector": true, "gateway": true, "bridge": true, "local": true, "official": true,
	"sdk": true, "cli": true, "dev": true, "test": true, "demo": true, "example": true, "sample": true, "template": true, "store": true,
	"registry": true, "continue": true, "swarm": true, "inspector": true, "the": true, "a": true, "an": true, "for": true, "and": true, "of": true,
	"with": true, "to": true, "in": true, "on": true, "by": true, "via": true, "my": true, "your": true, "simple": true, "basic": true, "fast": true,
	"smart": true, "universal": true, "generic": true, "custom": true, "python": true, "node": true, "typescript": true, "javascript": true, "go": true,
	"rust": true, "java": true, "io": true, "http": true, "rest": true, "graphql": true, "database": true, "db": true, "sql": true, "query": true,
	"workflow": true, "automation": true, "monitor": true, "monitoring": true, "analytics": true, "report": true, "reports": true, "manage": true,
	"management": true, "runner": true, "executor": true, "helper": true, "utils": true, "utility": true, "utilities": true, "kit": true, "lab": true, "labs": true}

var cveWordRe = regexp.MustCompile(`[A-Za-z0-9]+`)

// cveDistinctive reports whether a name says which product it is: at least
// one word that is not a generic word, and at least two words in all unless
// the one word is clearly a coined name (mixed case inside it or digits).
func cveDistinctive(name string) bool {
	words := cveWordRe.FindAllString(name, -1)
	if len(words) == 0 {
		return false
	}
	specific := 0
	for _, w := range words {
		if !cveGenericWords[strings.ToLower(w)] && !cveStopNames[strings.ToLower(w)] {
			specific++
		}
	}
	if specific == 0 {
		return false
	}
	if len(words) >= 2 {
		return true
	}
	w := words[0]
	return len(w) >= 6 && (strings.ToLower(w) != w && strings.ToUpper(w) != w && strings.ToLower(w[1:]) != w[1:] || strings.ContainsAny(w, "0123456789"))
}

var cveLettersRe = regexp.MustCompile(`[A-Za-z]{3}`)

func cveNameRe(name string) *regexp.Regexp {
	n := strings.TrimSpace(name)
	if len(n) < 5 || cveStopNames[strings.ToLower(n)] || !cveLettersRe.MatchString(n) {
		return nil
	}
	r, err := regexp.Compile(`(?i)(^|[^A-Za-z0-9])` + regexp.QuoteMeta(n) + `($|[^A-Za-z0-9])`)
	if err != nil {
		return nil
	}
	return r
}

// cveEntityIndex builds the match list from the site's own pages, so it grows
// as pages are added. Companies carry auto=false: they need the AI term test.
func cveEntityIndex(db *sql.DB) []cveEntity {
	var out []cveEntity
	add := func(kind, name, href, uid string, auto bool) {
		if re := cveNameRe(name); re != nil {
			out = append(out, cveEntity{Kind: kind, Name: strings.TrimSpace(name), Href: href, UID: uid, re: re, auto: auto})
		}
	}
	// AI products Stephen keeps by hand (their own synonyms, no page needed).
	rows, err := db.Query(`SELECT product, coalesce(vendor,''), array_to_string(match_terms, '|'), auto_publish FROM twoai_cve_products`)
	if err == nil {
		for rows.Next() {
			var p, v, terms string
			var auto bool
			if rows.Scan(&p, &v, &terms, &auto) != nil {
				continue
			}
			for _, t := range strings.Split(terms, "|") {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				if r, err := regexp.Compile(`(?i)(^|[^A-Za-z0-9])` + regexp.QuoteMeta(t) + `($|[^A-Za-z0-9])`); err == nil {
					out = append(out, cveEntity{Kind: "product", Name: p, re: r, auto: auto})
				}
			}
		}
		rows.Close()
	}
	// Tools.
	rows, err = db.Query(`SELECT data->>'name', data->>'slug', coalesce(data->>'uid','') FROM twoai_pages WHERE kind='tool' AND data->>'slug' IS NOT NULL AND data->>'name' IS NOT NULL`)
	if err == nil {
		for rows.Next() {
			var n, s, u string
			if rows.Scan(&n, &s, &u) == nil {
				add("tool", n, "/ai-tools/"+s+"/", u, true)
			}
		}
		rows.Close()
	}
	// MCP servers: display title and the last segment of the registry name.
	rows, err = db.Query(`SELECT coalesce(data->'server'->>'title',''), coalesce(data->'server'->>'name',''), data->'server'->>'slug' FROM twoai_pages WHERE kind='mcp-server' AND data->'server'->>'slug' IS NOT NULL`)
	if err == nil {
		for rows.Next() {
			var t, n, s string
			if rows.Scan(&t, &n, &s) == nil {
				href := "/mcp/" + s + "/"
				// Row 357: a title is a key only when it names the product
				// (cveDistinctive). The registry name's last segment is a key
				// only when it carries the vendor token, the namespace owner
				// in io.github.<owner>/<segment>, so "acme-search" under
				// io.github.acme matches and "web-search" never does.
				if cveDistinctive(t) {
					add("mcp_server", t, href, "", true)
				}
				if i := strings.LastIndex(n, "/"); i >= 0 && n[i+1:] != t {
					seg := n[i+1:]
					ns := n[:i]
					if j := strings.LastIndex(ns, "."); j >= 0 {
						ns = ns[j+1:]
					}
					ns = strings.ToLower(ns)
					if len(ns) >= 4 && !cveGenericWords[ns] && !cveStopNames[ns] && strings.Contains(strings.ToLower(seg), ns) && cveDistinctive(seg) {
						add("mcp_server", seg, href, "", true)
					}
				}
			}
		}
		rows.Close()
	}
	// Companies: AI term test required.
	rows, err = db.Query(`SELECT data->'company'->>'name', replace(replace(path,'companies/',''),'.json','') FROM twoai_pages WHERE kind='company' AND data->'company'->>'name' IS NOT NULL`)
	if err == nil {
		for rows.Next() {
			var n, u string
			if rows.Scan(&n, &u) == nil {
				add("company", n, "/companies/"+u+"/", u, false)
			}
		}
		rows.Close()
	}
	// Items inside the sections of the five hubs: Foundation Models, AI
	// Infrastructure and Hardware, AI APIs and Integrations, Programming
	// Languages and Frameworks, AI Agents and the MCP Ecosystem.
	rows, err = db.Query(`WITH hubs AS (
			SELECT jsonb_array_elements(data->'children')->>'href' href FROM twoai_pages
			WHERE path IN ('industries/foundation-models.json','industries/ai-infrastructure-and-hardware.json',
			               'industries/ai-apis-and-integrations.json','industries/languages-and-frameworks.json','industries/agents-and-mcp.json')),
		secs AS (SELECT p.data->>'uid' uid, h.href, p.data FROM hubs h JOIN twoai_pages p ON h.href LIKE '%/' || (p.data->>'uid') || '/')
		SELECT href, uid, it->>'name', coalesce(it->>'slug',''), coalesce(it->>'full_name','')
		FROM secs, LATERAL jsonb_array_elements(coalesce(data->'items', data->'models', data->'repos', '[]'::jsonb)) it
		WHERE it->>'name' IS NOT NULL`)
	if err == nil {
		for rows.Next() {
			var href, uid, n, slug, full string
			if rows.Scan(&href, &uid, &n, &slug, &full) == nil {
				h := href
				if slug != "" {
					h += "#" + slug
				}
				add("section_item", n, h, uid, true)
				if full != "" && strings.Contains(full, "/") {
					add("section_item", full, h, uid, true)
				}
			}
		}
		rows.Close()
	}
	return out
}

type nvdCVE struct {
	ID           string `json:"id"`
	Published    string `json:"published"`
	LastModified string `json:"lastModified"`
	Descriptions []struct {
		Lang  string `json:"lang"`
		Value string `json:"value"`
	} `json:"descriptions"`
	Metrics map[string][]struct {
		CvssData struct {
			BaseScore    float64 `json:"baseScore"`
			BaseSeverity string  `json:"baseSeverity"`
			VectorString string  `json:"vectorString"`
		} `json:"cvssData"`
		BaseSeverity string `json:"baseSeverity"`
	} `json:"metrics"`
	Weaknesses []struct {
		Description []struct {
			Value string `json:"value"`
		} `json:"description"`
	} `json:"weaknesses"`
	References []struct {
		URL  string   `json:"url"`
		Tags []string `json:"tags"`
	} `json:"references"`
	Configurations []struct {
		Nodes []struct {
			CpeMatch []struct {
				Criteria              string `json:"criteria"`
				Vulnerable            bool   `json:"vulnerable"`
				VersionStartIncluding string `json:"versionStartIncluding"`
				VersionStartExcluding string `json:"versionStartExcluding"`
				VersionEndIncluding   string `json:"versionEndIncluding"`
				VersionEndExcluding   string `json:"versionEndExcluding"`
			} `json:"cpeMatch"`
		} `json:"nodes"`
	} `json:"configurations"`
}

// cveAffected is one affected-product range from NVD's configurations, kept
// so the defence writer can name the fixed version (versionEndExcluding is
// the first version that is not affected) without inventing one.
type cveAffected struct {
	Criteria       string `json:"criteria"`
	StartIncluding string `json:"start_including,omitempty"`
	StartExcluding string `json:"start_excluding,omitempty"`
	EndIncluding   string `json:"end_including,omitempty"`
	EndExcluding   string `json:"end_excluding,omitempty"`
}

type cveRef struct {
	URL  string   `json:"url"`
	Tags []string `json:"tags"`
}

// cveRecordExtras pulls the affected ranges and the tagged references out of
// an NVD record, capped so a CVE with hundreds of CPE rows stays small.
func cveRecordExtras(c nvdCVE) ([]cveAffected, []cveRef) {
	aff := []cveAffected{}
	for _, cfg := range c.Configurations {
		for _, n := range cfg.Nodes {
			for _, m := range n.CpeMatch {
				if !m.Vulnerable || len(aff) >= 40 {
					continue
				}
				aff = append(aff, cveAffected{Criteria: m.Criteria, StartIncluding: m.VersionStartIncluding, StartExcluding: m.VersionStartExcluding,
					EndIncluding: m.VersionEndIncluding, EndExcluding: m.VersionEndExcluding})
			}
		}
	}
	refs := []cveRef{}
	for i, r := range c.References {
		if i >= 12 {
			break
		}
		tags := r.Tags
		if tags == nil {
			tags = []string{}
		}
		refs = append(refs, cveRef{URL: r.URL, Tags: tags})
	}
	return aff, refs
}

type nvdPage struct {
	TotalResults    int `json:"totalResults"`
	ResultsPerPage  int `json:"resultsPerPage"`
	StartIndex      int `json:"startIndex"`
	Vulnerabilities []struct {
		CVE nvdCVE `json:"cve"`
	} `json:"vulnerabilities"`
}

func nvdGet(params url.Values, into *nvdPage) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, _ := http.NewRequest("GET", "https://services.nvd.nist.gov/rest/json/cves/2.0?"+params.Encode(), nil)
		req.Header.Set("User-Agent", "theworldofai.org cve watch (srj@srjconsultingservices.com)")
		if k := os.Getenv("NVD_API_KEY"); k != "" {
			req.Header.Set("apiKey", k)
		}
		resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return json.Unmarshal(body, into)
			}
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(12 * time.Second)
	}
	return lastErr
}

func cveKEV() map[string]string {
	out := map[string]string{}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Get("https://www.cisa.gov/sites/default/files/feeds/known_exploited_vulnerabilities.json")
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	var k struct {
		Vulnerabilities []struct {
			CveID     string `json:"cveID"`
			DateAdded string `json:"dateAdded"`
		} `json:"vulnerabilities"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&k) == nil {
		for _, v := range k.Vulnerabilities {
			out[v.CveID] = v.DateAdded
		}
	}
	return out
}

func cveDescription(c nvdCVE) string {
	for _, d := range c.Descriptions {
		if d.Lang == "en" {
			return d.Value
		}
	}
	return ""
}

// cveClassify applies the three rules. It returns the hits, the status, a
// readable reason, and the vendor and product it settled on; an empty status
// means the CVE does not qualify.
func cveClassify(c nvdCVE, idx []cveEntity) ([]cveHit, string, string, string, string) {
	desc := cveDescription(c)
	cpes := ""
	for _, cfg := range c.Configurations {
		for _, n := range cfg.Nodes {
			for _, m := range n.CpeMatch {
				cpes += " " + strings.ReplaceAll(m.Criteria, "_", " ")
			}
		}
	}
	hay := desc + " " + cpes
	var hits []cveHit
	seen := map[string]bool{}
	var reasons []string
	auto, company, held := false, false, false
	vendor, product := "", ""
	for _, e := range idx {
		if !e.re.MatchString(hay) {
			continue
		}
		key := e.Kind + ":" + e.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		if e.Kind == "company" {
			company = true
			if vendor == "" {
				vendor = e.Name
			}
		} else {
			if e.auto {
				auto = true
			}
			// A product Stephen marked not to auto-publish (n8n, Jupyter,
			// Copilot, ChatGPT and the rest) holds the CVE at proposed unless
			// the AI term test passes too. Row 357: a page-derived co-match
			// was lifting these to published on its own.
			if e.Kind == "product" && !e.auto {
				held = true
			}
			if product == "" {
				product = e.Name
			}
		}
		reasons = append(reasons, e.Kind+":"+e.Name)
		if e.Href != "" {
			hits = append(hits, cveHit{Kind: e.Kind, Name: e.Name, Href: e.Href, UID: e.UID})
		}
	}
	terms := 0
	for _, g := range cveGenericTerms {
		if g.MatchString(desc) {
			terms++
		}
	}
	if terms > 0 {
		reasons = append(reasons, fmt.Sprintf("ai-terms:%d", terms))
	}
	status := ""
	switch {
	case auto && !held:
		status = "published"
	case auto && held && terms > 0:
		status = "published"
	case company && terms > 0:
		status = "published"
	case len(reasons) > 0:
		status = "proposed"
	}
	if product == "" {
		product = vendor
	}
	return hits, status, strings.Join(reasons, "; "), vendor, product
}

func cveStore(db *sql.DB, c nvdCVE, hits []cveHit, status, reason, vendor, product string, kev map[string]string) {
	desc := cveDescription(c)
	var score sql.NullFloat64
	sev, vec := "", ""
	for _, k := range []string{"cvssMetricV40", "cvssMetricV31", "cvssMetricV30"} {
		if m := c.Metrics[k]; len(m) > 0 {
			score = sql.NullFloat64{Float64: m[0].CvssData.BaseScore, Valid: true}
			sev = m[0].CvssData.BaseSeverity
			if sev == "" {
				sev = m[0].BaseSeverity
			}
			vec = m[0].CvssData.VectorString
			break
		}
	}
	cwe := ""
	for _, w := range c.Weaknesses {
		for _, d := range w.Description {
			if strings.HasPrefix(d.Value, "CWE-") {
				cwe = d.Value
				break
			}
		}
		if cwe != "" {
			break
		}
	}
	refs := []string{}
	for i, r := range c.References {
		if i >= 12 {
			break
		}
		refs = append(refs, r.URL)
	}
	if hits == nil {
		hits = []cveHit{}
	}
	refJSON, _ := json.Marshal(refs)
	hitJSON, _ := json.Marshal(hits)
	aff, tagged := cveRecordExtras(c)
	affJSON, _ := json.Marshal(aff)
	taggedJSON, _ := json.Marshal(tagged)
	kevAdded := sql.NullString{String: kev[c.ID], Valid: kev[c.ID] != ""}
	// Status is never downgraded by a refresh: a CVE Stephen approved stays
	// approved, one he rejected stays rejected, and a published one stays
	// published.
	if _, err := db.Exec(`INSERT INTO twoai_cves (cve_id, uid, published, last_modified, description, vendor, product, cvss_score, cvss_severity, cvss_vector, cwe, kev, kev_added, status, match_reason, cve_org_url, nvd_url, references_json, entities_json, affected_json, references_tagged, updated_at)
		VALUES ($1,$2,$3::timestamptz,$4::timestamptz,$5,NULLIF($6,''),NULLIF($7,''),$8,NULLIF($9,''),NULLIF($10,''),NULLIF($11,''),$12,$13::date,$14,$15,$16,$17,$18::jsonb,$19::jsonb,$20::jsonb,$21::jsonb,now())
		ON CONFLICT (cve_id) DO UPDATE SET last_modified=EXCLUDED.last_modified, description=EXCLUDED.description,
			affected_json=EXCLUDED.affected_json, references_tagged=EXCLUDED.references_tagged,
			vendor=coalesce(EXCLUDED.vendor, twoai_cves.vendor), product=coalesce(EXCLUDED.product, twoai_cves.product),
			cvss_score=coalesce(EXCLUDED.cvss_score, twoai_cves.cvss_score), cvss_severity=coalesce(EXCLUDED.cvss_severity, twoai_cves.cvss_severity),
			cvss_vector=coalesce(EXCLUDED.cvss_vector, twoai_cves.cvss_vector), cwe=coalesce(EXCLUDED.cwe, twoai_cves.cwe),
			kev=EXCLUDED.kev OR twoai_cves.kev, kev_added=coalesce(EXCLUDED.kev_added, twoai_cves.kev_added),
			status=CASE WHEN twoai_cves.status IN ('rejected','approved','published') THEN twoai_cves.status
				WHEN twoai_cves.status='proposed' AND EXCLUDED.status='published' AND twoai_cves.match_reason IS NOT DISTINCT FROM EXCLUDED.match_reason THEN 'proposed'
				ELSE EXCLUDED.status END,
			match_reason=EXCLUDED.match_reason, references_json=EXCLUDED.references_json, entities_json=EXCLUDED.entities_json, updated_at=now()`,
		c.ID, twoaiUID("cve:"+c.ID), c.Published, c.LastModified, desc, vendor, product, score, sev, vec, cwe, kevAdded.Valid, kevAdded,
		status, reason, "https://www.cve.org/CVERecord?id="+c.ID, "https://nvd.nist.gov/vuln/detail/"+c.ID, string(refJSON), string(hitJSON), string(affJSON), string(taggedJSON)); err != nil {
		fmt.Fprintln(os.Stderr, "twoai_cve_watch store", c.ID, ":", err)
	}
}

func twoaiCVEWatch(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_cves (
		cve_id text PRIMARY KEY, uid text NOT NULL, published timestamptz, last_modified timestamptz, description text,
		vendor text, product text, cvss_score numeric, cvss_severity text, cvss_vector text, cwe text,
		kev boolean NOT NULL DEFAULT false, kev_added date, status text NOT NULL DEFAULT 'proposed', match_reason text,
		cve_org_url text, nvd_url text, references_json jsonb, first_seen timestamptz NOT NULL DEFAULT now(),
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE twoai_cves ADD COLUMN IF NOT EXISTS entities_json jsonb`); err != nil {
		return err
	}
	// Row 366 (2026-10-02): a written headline and defence section per CVE,
	// and the record detail the writer needs (affected ranges, tagged
	// references). twoai_cve_write.go owns the writing columns.
	for _, col := range []string{"affected_json jsonb", "references_tagged jsonb", "headline text", "defense jsonb",
		"written_on date", "written_hash text", "write_attempts int NOT NULL DEFAULT 0"} {
		if _, err := db.Exec(`ALTER TABLE twoai_cves ADD COLUMN IF NOT EXISTS ` + col); err != nil {
			return err
		}
	}
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_cve_state (key text PRIMARY KEY, value text NOT NULL, updated_at timestamptz NOT NULL DEFAULT now())`)
	idx := cveEntityIndex(db)
	kev := cveKEV()
	var n int
	db.QueryRow(`SELECT count(*) FROM twoai_cves`).Scan(&n)
	var lastRun string
	db.QueryRow(`SELECT value FROM twoai_cve_state WHERE key='nvd_last_modified'`).Scan(&lastRun)
	seen := map[string]bool{}
	scanned, matched := 0, 0
	consider := func(c nvdCVE) {
		if c.ID == "" || seen[c.ID] {
			return
		}
		seen[c.ID] = true
		scanned++
		hits, status, reason, vendor, product := cveClassify(c, idx)
		if status == "" {
			return
		}
		matched++
		cveStore(db, c, hits, status, reason, vendor, product, kev)
	}
	page := func(params url.Values) {
		start := 0
		for {
			params.Set("startIndex", fmt.Sprint(start))
			params.Set("resultsPerPage", "2000")
			var p nvdPage
			if err := nvdGet(params, &p); err != nil {
				fmt.Fprintln(os.Stderr, "twoai_cve_watch nvd:", err)
				return
			}
			for _, v := range p.Vulnerabilities {
				consider(v.CVE)
			}
			start += p.ResultsPerPage
			if p.ResultsPerPage == 0 || start >= p.TotalResults {
				return
			}
			time.Sleep(6500 * time.Millisecond)
		}
	}
	now := time.Now().UTC()
	const stamp = "2006-01-02T15:04:05.000"
	if n == 0 || lastRun == "" {
		// Backfill: one keyword search per hand-kept product term, 90 days.
		var terms []string
		rows, err := db.Query(`SELECT unnest(match_terms) FROM twoai_cve_products`)
		if err == nil {
			for rows.Next() {
				var t string
				if rows.Scan(&t) == nil {
					terms = append(terms, t)
				}
			}
			rows.Close()
		}
		terms = append(terms, "large language model", "prompt injection", "LLM", "AI agent", "machine learning model", "artificial intelligence", "generative AI", "AI assistant", "model context protocol")
		for _, t := range terms {
			params := url.Values{}
			params.Set("keywordSearch", t)
			params.Set("pubStartDate", now.AddDate(0, 0, -90).Format(stamp))
			params.Set("pubEndDate", now.Format(stamp))
			page(params)
			time.Sleep(6500 * time.Millisecond)
		}
	} else {
		since, err := time.Parse(time.RFC3339, lastRun)
		if err != nil || now.Sub(since) > 100*24*time.Hour {
			since = now.AddDate(0, 0, -7)
		}
		since = since.Add(-2 * time.Hour)
		params := url.Values{}
		params.Set("lastModStartDate", since.Format(stamp))
		params.Set("lastModEndDate", now.Format(stamp))
		page(params)
	}
	db.Exec(`INSERT INTO twoai_cve_state (key, value, updated_at) VALUES ('nvd_last_modified', $1, now())
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, now.Format(time.RFC3339))
	// Refresh the KEV flag on everything already stored.
	for id, added := range kev {
		db.Exec(`UPDATE twoai_cves SET kev=true, kev_added=coalesce(kev_added,$2::date), updated_at=now() WHERE cve_id=$1 AND NOT kev`, id, added)
	}
	// Headlines and defence sections for the newest published CVEs, a few
	// a run, before the pages are written so this run's pages carry them.
	twoaiCVEWrite(db)
	built := twoaiCVEPages(db)
	fmt.Printf("twoai_cve_watch: scanned=%d matched=%d pages=%d ok=true\n", scanned, matched, built)
	return nil
}

// twoaiCVEPages writes one page row per published CVE and the list row the
// /ai-news/ foot and /ai-news/cves/ read. 'approved' counts as published.
func twoaiCVEPages(db *sql.DB) int {
	today := time.Now().Format("2006-01-02")
	rows, err := db.Query(`SELECT cve_id, uid, coalesce(published::text,''), coalesce(last_modified::text,''), coalesce(description,''), coalesce(vendor,''), coalesce(product,''),
		cvss_score, coalesce(cvss_severity,''), coalesce(cvss_vector,''), coalesce(cwe,''), kev, coalesce(kev_added::text,''), status,
		coalesce(match_reason,''), coalesce(cve_org_url,''), coalesce(nvd_url,''), coalesce(references_json,'[]'::jsonb)::text, coalesce(entities_json,'[]'::jsonb)::text,
		coalesce(headline,''), coalesce(defense,'null'::jsonb)::text, coalesce(affected_json,'[]'::jsonb)::text, coalesce(references_tagged,'[]'::jsonb)::text
		FROM twoai_cves WHERE status IN ('published','approved') ORDER BY published DESC NULLS LAST`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_cve_watch pages:", err)
		return 0
	}
	defer rows.Close()
	var list []map[string]any
	built, kevCount, headlined := 0, 0, 0
	// The weakness class by name, its page, and MITRE's first mitigations,
	// so the CVE page links its CWE and the defence section can quote it.
	cwes := cweIndex(db)
	for rows.Next() {
		var id, uid, pub, mod, desc, vendor, product, sev, vec, cwe, kevAdded, status, reason, cveURL, nvdURL, refs, ents, headline, defRaw, affRaw, taggedRaw string
		var score sql.NullFloat64
		var kev bool
		if rows.Scan(&id, &uid, &pub, &mod, &desc, &vendor, &product, &score, &sev, &vec, &cwe, &kev, &kevAdded, &status, &reason, &cveURL, &nvdURL, &refs, &ents, &headline, &defRaw, &affRaw, &taggedRaw) != nil {
			continue
		}
		refList := []string{}
		entList := []cveHit{}
		json.Unmarshal([]byte(refs), &refList)
		json.Unmarshal([]byte(ents), &entList)
		var defense any
		json.Unmarshal([]byte(defRaw), &defense)
		affected := []cveAffected{}
		json.Unmarshal([]byte(affRaw), &affected)
		tagged := []cveRef{}
		json.Unmarshal([]byte(taggedRaw), &tagged)
		if headline != "" {
			headlined++
		}
		if len(pub) > 10 {
			pub = pub[:10]
		}
		if len(mod) > 10 {
			mod = mod[:10]
		}
		var sc any
		if score.Valid {
			sc = score.Float64
		}
		title := id + ": " + product + " vulnerability"
		if headline != "" {
			title = headline + " (" + id + ")"
		}
		doc := map[string]any{
			"shape": "cve", "cve_id": id, "uid": uid, "published": pub, "last_modified": mod,
			"title":       title,
			"description": desc, "vendor": vendor, "product": product, "cvss_score": sc, "cvss_severity": sev,
			"cvss_vector": vec, "cwe": cwe, "kev": kev, "kev_added": kevAdded, "status": status, "match_reason": reason,
			"cve_org_url": cveURL, "nvd_url": nvdURL, "references": refList, "entities": entList, "generated": today,
			"headline": headline, "defense": defense, "affected": affected, "references_tagged": tagged,
		}
		if ci, ok := cwes[cwe]; ok {
			doc["cwe_name"] = ci.Name
			doc["cwe_page"] = ci.HasPage
			m := ci.Mitigations
			if len(m) > 3 {
				m = m[:3]
			}
			doc["cwe_mitigations"] = m
		}
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'cve',$2::jsonb,NULL,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data, url_count=1, updated_at=now()`,
			"news/cve-"+id+".json", string(j)); err == nil {
			built++
		}
		head := desc
		if len(head) > 220 {
			head = head[:220]
			if i := strings.LastIndex(head, " "); i > 150 {
				head = head[:i]
			}
			head += "..."
		}
		if kev {
			kevCount++
		}
		list = append(list, map[string]any{
			"cve_id": id, "uid": uid, "published": pub, "product": product, "vendor": vendor,
			"cvss_score": sc, "cvss_severity": sev, "kev": kev, "summary": head, "entities": entList, "headline": headline,
		})
	}
	fmt.Printf("twoai_cve_watch: pages with a written headline=%d of %d\n", headlined, len(list))
	sort.SliceStable(list, func(i, j int) bool { return list[i]["published"].(string) > list[j]["published"].(string) })
	lj, _ := json.Marshal(map[string]any{
		"shape": "cve-list", "name": "AI CVE tracker", "generated": today, "total": len(list), "kev": kevCount, "cves": list,
		"sources": map[string]string{"cve_org": "https://www.cve.org/", "nvd": "https://nvd.nist.gov/", "kev": "https://www.cisa.gov/known-exploited-vulnerabilities-catalog"},
	})
	db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count) VALUES ('news/cves.json','cve-list',$1::jsonb,NULL,1)
		ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data, url_count=1, updated_at=now()`, string(lj))
	return built
}
