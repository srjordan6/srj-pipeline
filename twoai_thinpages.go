package main

// THE THIN-PAGE SCRAPER: `./pipeline thinpages`, running on its own Render
// cron (srj-thinpage-scraper), separated from `pipeline all` at Stephen's
// direction on 2026-08-30 so the intelligence run and the scraping run never
// share a failure, a stage deadline, or a restart.
//
// The job: pages that exist but are thin get their data filled from the
// publisher's own site, and rows already enriched are re-read on a cadence so
// a change on the operator's site flows through without a human noticing
// first.
//
// HOW IT KNOWS WHAT TO SEED. Step 0 is a detector, not a list. It queries
// what the site publishes, applies a thinness test per page kind (a missing
// structured field, never a word count), and writes one row per thin page
// into twoai_thin_queue with the primary source that could fill it. The
// fillers below then work the queue by kind. A page leaves the queue when
// its test passes again, and a page that fails three attempts stays visible
// in the queue with its last error rather than being retried forever.
// "Thin" is therefore a query (SELECT * FROM twoai_thin_queue), the same
// principle as the changelog and the search-submission table.
//
// Fillers today, each idempotent:
//
//   1. mcp-server: package registry facts. 295 servers publish an npm or
//      PyPI package; the registries' public JSON APIs state the licence,
//      latest version, last publish date and weekly downloads. Facts land in
//      twoai_mcp_package_facts and the page builder in main.go renders them.
//   2. company: the company's own homepage JSON-LD Organization block, which
//      states headquarters and founding date when the publisher chose to
//      declare them. Only null profile fields are written, with the page
//      recorded in sources; a curated value is never overwritten.
//   3. Facility registry enrichment. Operator spec pages carry what
//      OpenStreetMap cannot: megawatts, square footage, certifications.
//      Each operator is an adapter; CyrusOne is the first. An adapter
//      re-runs only when its newest row is older than its cadence, so most
//      mornings this cron is a no-op that costs one SELECT per adapter.
//   4. Company profile seeding. Every company entity derived from the tool
//      catalog gets a twoai_company_profiles row with the tracked product's
//      site as its website, so the daily harvest has a URL to read and the
//      company page is never website-less. Purely additive.
//
// Rules, the same ones the incident and registry work established:
//   - robots.txt respected per adapter, with the check recorded in the
//     adapter's comment.
//   - Structured facts only: numbers, addresses, codes, certifications, a
//     source URL and a retrieval date. No publisher prose is stored.
//   - Never delete on failure. A fetch or parse error leaves prior rows
//     standing and says so in the log.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

const thinUA = "theworldofai.org facility registry (contact: stephen@srjconsultingservices.com)"

// THE BUDGETS. Stephen, 2026-08-30: it should be able to do all thin pages
// in a day. The first caps were sized for a stage sharing a cron with
// thirty-nine others, where a long scrape starved the rest. This cron has
// nothing else to do, so the budget is now the whole queue.
//
// The arithmetic that keeps it polite: 291 package reads at 0.4s is two
// minutes, 184 company sites at 1.5s is five, 352 facility pages at 1.5s is
// nine, and the readings are capped by the model call, not the queue. Twenty
// minutes of a daily cron, spread across hundreds of hosts, no host hit more
// than a handful of times.
//
// Each is overridable by environment variable, so a budget can be cut in one
// dashboard edit if a source ever objects, without a deploy.
func thinBudget(name string, def int) int {
	if v := os.Getenv("THIN_BUDGET_" + name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

func twoaiThinPages(db *sql.DB) {
	twoaiThinEnsureTables(db)
	twoaiThinCompanyProfiles(db)
	twoaiThinAudit(db)
	twoaiThinDetect(db)
	twoaiThinFillMCP(db)
	twoaiThinFillCompany(db)
	twoaiThinFillFacilities(db)
	defer thinReport(db)
	for _, a := range thinAdapters {
		var last sql.NullString
		db.QueryRow(`SELECT max(last_seen)::text FROM twoai_dc_facilities WHERE src=$1`, a.src).Scan(&last)
		stale := true
		if last.Valid && len(last.String) >= 10 {
			if t, err := time.Parse("2006-01-02", last.String[:10]); err == nil {
				stale = time.Since(t) >= time.Duration(a.staleDays)*24*time.Hour
			}
		}
		if !stale {
			fmt.Printf("thinpages: %s fresh (last %s), skipping\n", a.src, last.String[:10])
			continue
		}
		n, err := a.run(db)
		if err != nil {
			fmt.Printf("thinpages: %s FAILED after %d rows: %v (prior rows kept)\n", a.src, n, err)
			continue
		}
		fmt.Printf("thinpages: %s upserted %d facility rows\n", a.src, n)
	}
	twoaiThinSense(db)
	twoaiThinSensePages(db)
}

// The closing line of every run: what is left, and why. A queue that is not
// emptying should say so in the log rather than in a database query three
// days later.
func thinReport(db *sql.DB) {
	rows, err := db.Query(`SELECT kind, count(*), count(*) FILTER (WHERE attempts >= 3),
			count(*) FILTER (WHERE attempts = 0)
		FROM twoai_thin_queue GROUP BY kind ORDER BY kind`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var total, exhausted, untried int
		if rows.Scan(&kind, &total, &exhausted, &untried) == nil {
			fmt.Printf("thinpages: queue %s: %d remaining (%d never tried, %d gave up after 3 attempts)\n",
				kind, total, untried, exhausted)
		}
	}
}

// Adapters, one per publisher. Adding an operator is appending a row here;
// the cadence keeps each one polite regardless of how often the cron fires.
var thinAdapters = []struct {
	src       string
	staleDays int
	run       func(*sql.DB) (int, error)
}{
	{"cyrusone", 7, thinScrapeCyrusOne},
}

// Every company entity gets a profile row with at least a website, resolved
// from the tracked product's own URL (the same tier the page builder falls
// back to). INSERT-only: a profile that exists, however sparse, is never
// touched, because curated fields must not be overwritten by a seeder.
func twoaiThinCompanyProfiles(db *sql.DB) {
	res, err := db.Exec(`INSERT INTO twoai_company_profiles (uid, name, website, sources, verified_on)
		SELECT e.uid, e.name,
		       regexp_replace(t.url, '^(https?://[^/]+).*$', '\1') || '/',
		       jsonb_build_array(t.url), current_date
		FROM twoai_entities e
		JOIN LATERAL (
			SELECT tt->>'url' AS url
			FROM site_content sc, jsonb_array_elements(sc.data->'tools') tt
			WHERE sc.path='resources/tools.json' AND tt->>'vendor' = e.name
			  AND tt->>'url' LIKE 'http%'
			ORDER BY tt->>'url' LIMIT 1
		) t ON true
		WHERE e.kind='company'
		  AND NOT EXISTS (SELECT 1 FROM twoai_company_profiles p WHERE p.uid = e.uid)
		ON CONFLICT (uid) DO NOTHING`)
	if err != nil {
		fmt.Println("thinpages: company profile seed:", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		fmt.Printf("thinpages: seeded %d company profiles from tracked product URLs\n", n)
	}
	// A profile that exists without a website is the same gap one step
	// later: 143 of them on 2026-08-30, which kept the headquarters filler
	// blind to more than half the directory. Same tier-3 resolution, same
	// source recorded, only the empty field written.
	res, err = db.Exec(`UPDATE twoai_company_profiles p SET website = t.origin,
		sources = CASE WHEN p.sources @> to_jsonb(t.url) THEN p.sources ELSE p.sources || to_jsonb(t.url) END,
		updated_at = now()
		FROM (SELECT e.uid, regexp_replace(tt->>'url', '^(https?://[^/]+).*$', '\1') || '/' AS origin,
		             tt->>'url' AS url,
		             row_number() OVER (PARTITION BY e.uid ORDER BY tt->>'url') AS rn
		      FROM twoai_entities e
		      JOIN site_content sc ON sc.path='resources/tools.json'
		      JOIN LATERAL jsonb_array_elements(sc.data->'tools') tt
		        ON tt->>'vendor' = e.name AND tt->>'url' LIKE 'http%'
		      WHERE e.kind='company') t
		WHERE t.uid = p.uid AND t.rn = 1 AND coalesce(p.website,'') = ''`)
	if err != nil {
		fmt.Println("thinpages: company website fill:", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		fmt.Printf("thinpages: filled %d empty company websites from tracked product URLs\n", n)
	}
}

// ---- The queue ----------------------------------------------------------

func twoaiThinEnsureTables(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_thin_queue (
		path text PRIMARY KEY,
		kind text NOT NULL,
		ref text NOT NULL DEFAULT '',
		source_url text NOT NULL DEFAULT '',
		reason text NOT NULL,
		first_seen date NOT NULL DEFAULT current_date,
		last_attempt timestamptz,
		attempts int NOT NULL DEFAULT 0,
		last_error text NOT NULL DEFAULT '')`)
	db.Exec(`CREATE INDEX IF NOT EXISTS twoai_thin_queue_kind_idx ON twoai_thin_queue (kind, attempts)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_mcp_package_facts (
		name text PRIMARY KEY,
		registry text NOT NULL,
		identifier text NOT NULL,
		latest_version text NOT NULL DEFAULT '',
		license text NOT NULL DEFAULT '',
		last_publish date,
		weekly_downloads bigint,
		repo_url text NOT NULL DEFAULT '',
		source_url text NOT NULL,
		fetched_on date NOT NULL DEFAULT current_date)`)
}

type thinCandidate struct{ path, kind, ref, source, reason string }

// The detector. Each SELECT is one thinness test on what the site publishes;
// the queue is rebuilt per kind so a page that stops being thin leaves it,
// and a page that is still thin keeps its attempt history.
func twoaiThinDetect(db *sql.DB) {
	tests := []struct {
		kind string
		q    string
	}{
		// A server that ships a package whose registry facts we have never
		// read, or read more than two weeks ago.
		{"mcp-server", `SELECT 'mcp/'||s.slug||'.json', s.name,
			CASE WHEN p->>'registryType'='npm' THEN 'https://registry.npmjs.org/'||(p->>'identifier')
			     ELSE 'https://pypi.org/pypi/'||(p->>'identifier')||'/json' END,
			'no package facts from the ' || (p->>'registryType') || ' registry'
			FROM twoai_mcp_servers s
			JOIN LATERAL (SELECT x FROM jsonb_array_elements(s.packages) x
				WHERE x->>'registryType' IN ('npm','pypi') AND coalesce(x->>'identifier','')<>'' LIMIT 1) j(p) ON true
			WHERE s.description<>''
			  AND NOT EXISTS (SELECT 1 FROM twoai_mcp_package_facts f WHERE f.name=s.name AND f.fetched_on > current_date - 14)`},
		// A company page with a site but no stated headquarters.
		{"company", `SELECT 'companies/'||p.uid||'.json', p.uid, p.website, 'no headquarters in profile'
			FROM twoai_company_profiles p
			WHERE coalesce(p.website,'')<>'' AND coalesce(p.headquarters,'')=''`},
		// A mapped US facility with an operator site but no spec profile.
		// No generic filler exists yet; the queue makes the gap countable.
		{"dc-facility", `SELECT 'dc-fac/'||id, id, website, 'no operator profile'
			FROM twoai_dc_facilities
			WHERE country='US' AND coalesce(website,'')<>'' AND profile='{}'::jsonb`},
		// Google's kind of thin: an indexable page whose rendered main content
		// is under the 300-word floor, measured by the self-audit above. No
		// automated filler; these are template and editorial work, and the
		// queue is where that work is counted.
		{"thin-words", `SELECT 'audit:'||url, url, url,
			words::text || ' words in main content, below ' || ` + fmt.Sprint(thinAuditWords) + `::text
			FROM twoai_page_audit WHERE status=200 AND words < ` + fmt.Sprint(thinAuditWords)},
	}
	for _, t := range tests {
		rows, err := db.Query(t.q)
		if err != nil {
			fmt.Printf("thinpages: detect %s: %v\n", t.kind, err)
			continue
		}
		var cands []thinCandidate
		for rows.Next() {
			var c thinCandidate
			c.kind = t.kind
			if rows.Scan(&c.path, &c.ref, &c.source, &c.reason) == nil {
				cands = append(cands, c)
			}
		}
		rows.Close()
		paths := make([]string, 0, len(cands))
		for _, c := range cands {
			paths = append(paths, c.path)
			db.Exec(`INSERT INTO twoai_thin_queue (path, kind, ref, source_url, reason)
				VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (path) DO UPDATE SET source_url=EXCLUDED.source_url, reason=EXCLUDED.reason`,
				c.path, c.kind, c.ref, c.source, c.reason)
		}
		res, _ := db.Exec(`DELETE FROM twoai_thin_queue WHERE kind=$1 AND NOT (path = ANY($2))`, t.kind, pq.Array(paths))
		cleared := int64(0)
		if res != nil {
			cleared, _ = res.RowsAffected()
		}
		fmt.Printf("thinpages: detect %s: %d thin, %d cleared\n", t.kind, len(cands), cleared)
	}
}

// Rows the fillers may work on: never more than three attempts, and a week
// between attempts, so a dead registry entry costs three requests in total.
func thinDue(db *sql.DB, kind string, limit int) []thinCandidate {
	// Never attempted comes first, then the retries. A week between retries
	// is right for a site that was down; it must not delay a page that has
	// never been read at all, which is what ORDER BY attempts already does,
	// and the budget is now big enough that the whole queue is one pass.
	rows, err := db.Query(`SELECT path, ref, source_url, reason FROM twoai_thin_queue
		WHERE kind=$1 AND attempts < 3
		  AND (last_attempt IS NULL OR last_attempt < now() - interval '2 days')
		ORDER BY attempts, first_seen LIMIT $2`, kind, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []thinCandidate
	for rows.Next() {
		var c thinCandidate
		c.kind = kind
		if rows.Scan(&c.path, &c.ref, &c.source, &c.reason) == nil {
			out = append(out, c)
		}
	}
	return out
}

func thinAttempt(db *sql.DB, path string, errText string) {
	db.Exec(`UPDATE twoai_thin_queue SET attempts=attempts+1, last_attempt=now(), last_error=$2 WHERE path=$1`, path, errText)
}

// ---- mcp-server: registry facts ------------------------------------------

func twoaiThinFillMCP(db *sql.DB) {
	due := thinDue(db, "mcp-server", thinBudget("MCP", 1000))
	if len(due) == 0 {
		return
	}
	client := &http.Client{Timeout: 20 * time.Second}
	filled := 0
	for _, c := range due {
		time.Sleep(400 * time.Millisecond)
		var reg, ident string
		var latest, lic, repo, pub string
		var dl sql.NullInt64
		var err error
		if strings.HasPrefix(c.source, "https://registry.npmjs.org/") {
			reg = "npm"
			ident = strings.TrimPrefix(c.source, "https://registry.npmjs.org/")
			latest, lic, repo, pub, dl, err = thinNpmFacts(client, ident)
		} else {
			reg = "pypi"
			ident = strings.TrimSuffix(strings.TrimPrefix(c.source, "https://pypi.org/pypi/"), "/json")
			latest, lic, repo, pub, err = thinPyPIFacts(client, ident)
		}
		if err != nil {
			thinAttempt(db, c.path, err.Error())
			continue
		}
		var pubDate any
		if pub != "" {
			pubDate = pub
		}
		if _, err := db.Exec(`INSERT INTO twoai_mcp_package_facts
			(name, registry, identifier, latest_version, license, last_publish, weekly_downloads, repo_url, source_url, fetched_on)
			VALUES ($1,$2,$3,$4,$5,$6::date,$7,$8,$9,current_date)
			ON CONFLICT (name) DO UPDATE SET registry=EXCLUDED.registry, identifier=EXCLUDED.identifier,
				latest_version=EXCLUDED.latest_version, license=EXCLUDED.license, last_publish=EXCLUDED.last_publish,
				weekly_downloads=EXCLUDED.weekly_downloads, repo_url=EXCLUDED.repo_url, source_url=EXCLUDED.source_url,
				fetched_on=current_date`,
			c.ref, reg, ident, latest, lic, pubDate, dl, repo, c.source); err != nil {
			thinAttempt(db, c.path, "db: "+err.Error())
			continue
		}
		db.Exec(`DELETE FROM twoai_thin_queue WHERE path=$1`, c.path)
		filled++
	}
	fmt.Printf("thinpages: mcp-server package facts filled=%d of %d due\n", filled, len(due))
}

func thinLicenseString(v any) string {
	switch x := v.(type) {
	case string:
		if len(x) > 64 { // a licence field holding the whole licence text is not a name
			return ""
		}
		return x
	case map[string]any:
		if t, ok := x["type"].(string); ok && len(t) <= 64 {
			return t
		}
	}
	return ""
}

func thinNpmFacts(client *http.Client, ident string) (latest, lic, repo, pub string, dl sql.NullInt64, err error) {
	body, err := thinGet(client, "https://registry.npmjs.org/"+ident)
	if err != nil {
		return
	}
	var doc struct {
		DistTags   map[string]string `json:"dist-tags"`
		License    any               `json:"license"`
		// npm's time map is not all strings: an unpublished package carries
		// an object under "unpublished" (caught on the first cron run).
		Time       map[string]any `json:"time"`
		Repository any            `json:"repository"`
	}
	if err = json.Unmarshal([]byte(body), &doc); err != nil {
		return
	}
	latest = doc.DistTags["latest"]
	lic = thinLicenseString(doc.License)
	if t, ok := doc.Time[latest].(string); ok && len(t) >= 10 {
		pub = t[:10]
	} else if t, ok := doc.Time["modified"].(string); ok && len(t) >= 10 {
		pub = t[:10]
	}
	switch r := doc.Repository.(type) {
	case string:
		repo = thinRepoURL(r)
	case map[string]any:
		if u, ok := r["url"].(string); ok {
			repo = thinRepoURL(u)
		}
	}
	if b, e := thinGet(client, "https://api.npmjs.org/downloads/point/last-week/"+ident); e == nil {
		var d struct {
			Downloads int64 `json:"downloads"`
		}
		if json.Unmarshal([]byte(b), &d) == nil {
			dl = sql.NullInt64{Int64: d.Downloads, Valid: true}
		}
	}
	return
}

func thinPyPIFacts(client *http.Client, ident string) (latest, lic, repo, pub string, err error) {
	body, err := thinGet(client, "https://pypi.org/pypi/"+url.PathEscape(ident)+"/json")
	if err != nil {
		return
	}
	var doc struct {
		Info struct {
			Version           string            `json:"version"`
			License           string            `json:"license"`
			LicenseExpression string            `json:"license_expression"`
			ProjectURLs       map[string]string `json:"project_urls"`
		} `json:"info"`
		URLs []struct {
			UploadTime string `json:"upload_time"`
		} `json:"urls"`
	}
	if err = json.Unmarshal([]byte(body), &doc); err != nil {
		return
	}
	latest = doc.Info.Version
	lic = doc.Info.LicenseExpression
	if lic == "" {
		lic = thinLicenseString(doc.Info.License)
	}
	for _, k := range []string{"Source", "Repository", "Source Code", "Homepage"} {
		if u := doc.Info.ProjectURLs[k]; strings.Contains(u, "github.com") || strings.Contains(u, "gitlab.com") {
			repo = thinRepoURL(u)
			break
		}
	}
	for _, u := range doc.URLs {
		if len(u.UploadTime) >= 10 && u.UploadTime[:10] > pub {
			pub = u.UploadTime[:10]
		}
	}
	return
}

// git+ssh://git@github.com/x/y.git and friends become the page a reader can
// open. Anything that is not an https link to a known host is dropped.
func thinRepoURL(raw string) string {
	r := strings.TrimPrefix(raw, "git+")
	r = strings.TrimSuffix(r, ".git")
	r = strings.Replace(r, "git://", "https://", 1)
	r = strings.Replace(r, "ssh://git@", "https://", 1)
	r = strings.Replace(r, "git@github.com:", "https://github.com/", 1)
	if !strings.HasPrefix(r, "https://") {
		return ""
	}
	return r
}

// ---- company: the publisher's own JSON-LD -------------------------------

func twoaiThinFillCompany(db *sql.DB) {
	due := thinDue(db, "company", thinBudget("COMPANY", 400))
	if len(due) == 0 {
		return
	}
	client := &http.Client{Timeout: 25 * time.Second}
	filled := 0
	for _, c := range due {
		time.Sleep(1500 * time.Millisecond)
		hq, founded, src, err := thinOrgFacts(client, c.source)
		if err != nil {
			thinAttempt(db, c.path, err.Error())
			continue
		}
		if hq == "" && founded == 0 {
			thinAttempt(db, c.path, "no JSON-LD Organization address or foundingDate on site")
			continue
		}
		var fy any
		if founded > 0 {
			fy = founded
		}
		if _, err := db.Exec(`UPDATE twoai_company_profiles SET
			headquarters = CASE WHEN coalesce(headquarters,'')='' AND $2<>'' THEN $2 ELSE headquarters END,
			founded = COALESCE(founded, $3::int),
			sources = CASE WHEN sources @> to_jsonb($4::text) THEN sources ELSE sources || to_jsonb($4::text) END,
			verified_on = current_date, updated_at = now()
			WHERE uid=$1`, c.ref, hq, fy, src); err != nil {
			thinAttempt(db, c.path, "db: "+err.Error())
			continue
		}
		if hq != "" {
			db.Exec(`DELETE FROM twoai_thin_queue WHERE path=$1`, c.path)
			filled++
		} else {
			thinAttempt(db, c.path, "foundingDate only; no address declared")
		}
	}
	fmt.Printf("thinpages: company headquarters filled=%d of %d due\n", filled, len(due))
}

var thinLDRe = regexp.MustCompile(`(?is)<script[^>]+type\s*=\s*["']application/ld\+json["'][^>]*>(.*?)</script>`)

// Reads the homepage, then /about if the homepage declares nothing. Returns
// the headquarters as "City, Region, Country" from the first Organization
// (or subtype) block carrying a PostalAddress, and the founding year.
func thinOrgFacts(client *http.Client, site string) (hq string, founded int, src string, err error) {
	if !strings.HasPrefix(site, "http") {
		return "", 0, "", fmt.Errorf("no http website")
	}
	for _, p := range []string{"", "about", "about-us", "company"} {
		u := strings.TrimSuffix(site, "/") + "/" + p
		body, e := thinGet(client, u)
		if e != nil {
			if p == "" {
				err = e
			}
			continue
		}
		err = nil
		for _, m := range thinLDRe.FindAllStringSubmatch(body, -1) {
			var v any
			if json.Unmarshal([]byte(strings.TrimSpace(m[1])), &v) != nil {
				continue
			}
			for _, node := range thinLDNodes(v) {
				t := fmt.Sprint(node["@type"])
				if !strings.Contains(t, "Organization") && !strings.Contains(t, "Corporation") {
					continue
				}
				if a, ok := node["address"].(map[string]any); ok && hq == "" {
					var parts []string
					for _, k := range []string{"addressLocality", "addressRegion", "addressCountry"} {
						if s, ok := a[k].(string); ok && strings.TrimSpace(s) != "" {
							parts = append(parts, strings.TrimSpace(s))
						} else if o, ok := a[k].(map[string]any); ok {
							if s, ok := o["name"].(string); ok && s != "" {
								parts = append(parts, s)
							}
						}
					}
					if len(parts) > 0 {
						hq = strings.Join(parts, ", ")
					}
				}
				if fd, ok := node["foundingDate"].(string); ok && len(fd) >= 4 && founded == 0 {
					if y, e := strconv.Atoi(fd[:4]); e == nil && y > 1800 && y <= time.Now().Year() {
						founded = y
					}
				}
			}
		}
		if hq != "" || founded > 0 {
			return hq, founded, u, nil
		}
	}
	return hq, founded, "", err
}

// JSON-LD comes as one object, an array of them, or a @graph; flatten to the
// objects a caller can test.
func thinLDNodes(v any) []map[string]any {
	var out []map[string]any
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			out = append(out, thinLDNodes(e)...)
		}
	case map[string]any:
		out = append(out, x)
		if g, ok := x["@graph"]; ok {
			out = append(out, thinLDNodes(g)...)
		}
	}
	return out
}

// ---- CyrusOne ---------------------------------------------------------

// robots.txt checked 2026-08-30: disallows only /_hcms/preview/,
// /hs/manage-preferences/ and preview/cache-buster query paths. The index
// and facility pages are permitted. North America only; EMEA and APAC links
// on the same index are deliberately not ingested (the US registry is the
// scope of this section's per-facility program today).
func thinScrapeCyrusOne(db *sql.DB) (int, error) {
	client := &http.Client{Timeout: 45 * time.Second}
	body, err := thinGet(client, "https://www.cyrusone.com/data-centers")
	if err != nil {
		return 0, fmt.Errorf("index: %w", err)
	}
	linkRe := regexp.MustCompile(`href="(/data-centers/north-america/[a-z0-9-]+)`)
	seen := map[string]bool{}
	var slugs []string
	for _, m := range linkRe.FindAllStringSubmatch(body, -1) {
		p := m[1]
		if !seen[p] {
			seen[p] = true
			slugs = append(slugs, strings.TrimPrefix(p, "/data-centers/north-america/"))
		}
	}
	sort.Strings(slugs)
	if len(slugs) < 10 {
		// A near-empty link set means the index changed shape, not that
		// CyrusOne closed twenty campuses overnight. Keep prior rows.
		return 0, fmt.Errorf("index yielded only %d facility links, refusing to write", len(slugs))
	}
	n := 0
	for _, slug := range slugs {
		time.Sleep(1200 * time.Millisecond) // polite spacing
		page, err := thinGet(client, "https://www.cyrusone.com/data-centers/north-america/"+slug)
		if err != nil {
			fmt.Printf("thinpages: cyrusone %s: %v (skipped)\n", slug, err)
			continue
		}
		row, ok := thinParseCyrusOne(slug, page)
		if !ok {
			fmt.Printf("thinpages: cyrusone %s: page did not parse (skipped)\n", slug)
			continue
		}
		pj, _ := json.Marshal(row.profile)
		if _, err := db.Exec(`INSERT INTO twoai_dc_facilities
			(id, src, name, operator, city, state, website, profile, status, country, critical_it_mw)
			VALUES ($1,'cyrusone',$2,'CyrusOne',$3,$4,$5,$6::jsonb,'enriched','US',$7)
			ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, operator=EXCLUDED.operator,
				city=EXCLUDED.city, state=EXCLUDED.state, website=EXCLUDED.website,
				profile=EXCLUDED.profile, status=EXCLUDED.status,
				critical_it_mw=EXCLUDED.critical_it_mw, last_seen=current_date`,
			"cyrusone:"+slug, row.name, row.city, row.state, row.url, string(pj), row.mw); err == nil {
			n++
		} else {
			fmt.Printf("thinpages: cyrusone %s: upsert: %v\n", slug, err)
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("no facility page parsed; prior rows kept")
	}
	return n, nil
}

type thinC1Row struct {
	name, city, state, url string
	mw                     float64
	profile                map[string]any
}

var (
	thinTagRe  = regexp.MustCompile(`(?s)<(script|style|noscript|svg)[^>]*>.*?</(script|style|noscript|svg)>`)
	thinStrip  = regexp.MustCompile(`(?s)<[^>]+>`)
	thinZipRe  = regexp.MustCompile(`\b(\d{5})\b`)
	thinCodeRe = regexp.MustCompile(`\b([A-Z]{3})(\d+)\b`)
	thinNumRe  = regexp.MustCompile(`[\d,]+(?:\.\d+)?`)
)

// The certification vocabulary is a fixed token scan over the spec bullets:
// factual attributes come out, none of the operator's prose goes in.
var thinCerts = []struct{ name, pat string }{
	{"SOC 1 Type 2", `SOC\s*1\s*type\s*(?:2|II)`}, {"SOC 2 Type 2", `SOC\s*2\s*type\s*(?:2|II)`},
	{"PCI DSS", `PCI\s*DSS`}, {"HIPAA", `HIPAA`}, {"ISO 27001", `ISO\s*27001`},
	{"FISMA", `FISMA`}, {"SSAE 16 Type II", `SSAE\s*16`}, {"TIA-942", `TIA[- ]?942`},
	{"HITRUST", `HITRUST`}, {"Green Globes", `Green\s*Globe`},
}

func thinParseCyrusOne(slug, page string) (thinC1Row, bool) {
	var r thinC1Row
	r.url = "https://www.cyrusone.com/data-centers/north-america/" + slug
	txt := thinTagRe.ReplaceAllString(page, " ")
	txt = thinStrip.ReplaceAllString(txt, "\n")
	txt = html.UnescapeString(txt)
	var lines []string
	for _, l := range strings.Split(txt, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	// Header block: the lines between the "Data Centers" breadcrumb and the
	// press contact are location, campus code, and one address per building.
	loc, codes := "", ""
	var addrs []string
	press := -1
	for i, l := range lines {
		if l == "press@cyrusone.com" {
			press = i
			break
		}
	}
	if press < 0 {
		return r, false
	}
	for i := 0; i < press; i++ {
		if lines[i] == "Data Centers" && i+2 < press {
			loc, codes = lines[i+1], lines[i+2]
			break
		}
	}
	if loc == "" || !strings.Contains(loc, ",") {
		return r, false
	}
	for i := 0; i < press; i++ {
		l := lines[i]
		if thinZipRe.MatchString(l) && l != loc && !strings.Contains(l, "@") {
			addrs = append(addrs, l)
		}
	}
	parts := strings.SplitN(loc, ",", 2)
	r.city = strings.TrimSpace(parts[0])
	r.state = strings.TrimSpace(parts[1])
	r.name = "CyrusOne " + r.city + ", " + r.state + " (" + codes + ")"

	// Key figures: the value is the line BEFORE its label. Lebanon OH labels
	// megawatts "Total Megawatts IT Capacity" rather than "Total IT
	// Capacity", which is why the label match is a prefix+contains test; the
	// first scraper missed that page entirely (caught 2026-08-30).
	var sqft int64
	for i, l := range lines {
		if i == 0 {
			continue
		}
		if l == "Technical IT Space" {
			if m := thinNumRe.FindString(lines[i-1]); m != "" {
				v, _ := strconv.ParseInt(strings.ReplaceAll(m, ",", ""), 10, 64)
				sqft = v
			}
		}
		if strings.HasPrefix(l, "Total") && strings.Contains(l, "IT Capacity") {
			if m := thinNumRe.FindString(lines[i-1]); m != "" {
				r.mw, _ = strconv.ParseFloat(strings.ReplaceAll(m, ",", ""), 64)
			}
		}
	}
	if r.mw == 0 || sqft == 0 {
		return r, false
	}
	// Certifications scan over the specification and sustainability bullets.
	blob := strings.Join(lines, " ")
	var certs []string
	for _, c := range thinCerts {
		if regexp.MustCompile(`(?i)` + c.pat).MatchString(blob) {
			certs = append(certs, c.name)
		}
	}
	// Postal code: the LAST five-digit group in an address line is the ZIP;
	// the first can be a street number (Houston's 11003, caught 2026-08-30).
	postal := ""
	if len(addrs) > 0 {
		zips := thinZipRe.FindAllString(strings.Join(addrs, " "), -1)
		if len(zips) > 0 {
			postal = zips[len(zips)-1]
		}
	}
	// Facility codes: expand "PHX1-PHX8" to the list when the prefix agrees.
	var codeList []string
	found := thinCodeRe.FindAllStringSubmatch(codes, -1)
	if strings.Contains(codes, "-") && len(found) == 2 && found[0][1] == found[1][1] {
		lo, _ := strconv.Atoi(found[0][2])
		hi, _ := strconv.Atoi(found[1][2])
		for k := lo; k <= hi && hi-lo < 30; k++ {
			codeList = append(codeList, fmt.Sprintf("%s%d", found[0][1], k))
		}
	} else {
		for _, f := range found {
			codeList = append(codeList, f[1]+f[2])
		}
	}
	if certs == nil {
		certs = []string{}
	}
	r.profile = map[string]any{
		"operator": "CyrusOne", "campus": codes, "facility_codes": codeList,
		"address": addrs, "postal_code": postal,
		"it_capacity_mw": r.mw, "technical_space_sqft": sqft,
		"certifications": certs,
		"source": map[string]any{
			"publisher": "CyrusOne", "page": r.url,
			"retrieved": time.Now().UTC().Format("2006-01-02"),
			"basis":     "Operator spec page; structured facts only, no operator prose reproduced.",
		},
	}
	return r, true
}

func thinGet(client *http.Client, u string) (string, error) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", thinUA)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return string(b), err
}
