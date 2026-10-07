package main

// twoai_gsc: Search Console in SQL, daily (theworldofai rows 548 and 549,
// 2026-10-07).
//
// twoai_gsc_daily stopped on 2026-09-03 because it was never a feed: it was
// loaded by hand from Search Console exports. This stage reads the two
// Search Console APIs with the service account the GA4 feed already uses
// (GOOGLE_SA_EMAIL and GOOGLE_SA_KEY), so nothing new is needed but one
// grant: the service account must be a user on the Search Console property
// (Settings, Users and permissions, Add user, Restricted is enough). Until it
// is, the stage prints the account's email and stops cleanly.
//
// WHAT IT WRITES.
//   - twoai_gsc_pages: clicks, impressions, CTR and position per page per day
//     (Search Analytics, dimensions date and page). The last three days are
//     read again each run, since Search Console revises them.
//   - twoai_gsc_queries: the same per query per page per day, so "what did
//     people search to reach /ai-news/cves/" is one SQL query (row 549).
//   - twoai_gsc_daily.impressions and clicks: the property's own daily
//     totals. indexed and not_indexed are left as they are: those came from
//     the coverage report, which has no API, and are not filled with an
//     estimate.
//   - twoai_gsc_inspect: Google's index verdict, coverage state and last
//     crawl for each URL, from the URL Inspection API. Its quota is 2,000 a
//     day for the property, so the stage inspects at most
//     twoaiGSCInspectPerDay a UTC day, never inspected first, then the oldest
//     inspection; the ~21,000 live URLs take about eleven days a pass. The
//     per-section counts in twoai_gsc_sections are counts of what has been
//     inspected, and say so; they are not extrapolated to the whole site.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	twoaiGSCInspectPerDay = 1800
	twoaiGSCScope         = "https://www.googleapis.com/auth/webmasters.readonly"
)

var twoaiGSCHTTP = &http.Client{Timeout: 60 * time.Second}

func twoaiGSCEnsure(db *sql.DB) {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS twoai_gsc_pages (day date NOT NULL, page text NOT NULL, clicks int NOT NULL,
			impressions int NOT NULL, ctr double precision, position double precision,
			fetched_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (day, page))`,
		`CREATE TABLE IF NOT EXISTS twoai_gsc_queries (day date NOT NULL, page text NOT NULL, query text NOT NULL,
			clicks int NOT NULL, impressions int NOT NULL, position double precision,
			fetched_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (day, page, query))`,
		`CREATE TABLE IF NOT EXISTS twoai_gsc_inspect (url text PRIMARY KEY, section text, verdict text,
			coverage_state text, indexing_state text, robots_state text, page_fetch_state text,
			last_crawl timestamptz, google_canonical text, inspected_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS twoai_gsc_sections (day date NOT NULL, section text NOT NULL, inspected int NOT NULL,
			indexed int NOT NULL, not_indexed int NOT NULL, never_crawled int NOT NULL,
			PRIMARY KEY (day, section))`,
		`ALTER TABLE twoai_gsc_daily ADD COLUMN IF NOT EXISTS clicks int`,
	} {
		if _, err := db.Exec(q); err != nil {
			fmt.Println("twoai_gsc schema:", err)
		}
	}
}

// gaSAEmail is the service account's email, by the same rule gaSAToken
// uses: client_email inside a pasted JSON key wins.
func gaSAEmail() string {
	key := twoaiEnv("GOOGLE_SA_KEY")
	if strings.Contains(key, "client_email") {
		var sa struct {
			ClientEmail string `json:"client_email"`
		}
		if json.Unmarshal([]byte(key), &sa) == nil && sa.ClientEmail != "" {
			return sa.ClientEmail
		}
	}
	return twoaiEnv("GOOGLE_SA_EMAIL")
}

// twoaiGSCSection files a URL for the per-section counts: the first path
// segment, and the second under /ai-ecosystem/ and /ai-news/, where the
// families Stephen named live.
func twoaiGSCSection(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "other"
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		return "home"
	}
	if (segs[0] == "ai-ecosystem" || segs[0] == "ai-news" || segs[0] == "research") && len(segs) >= 2 {
		if len(segs) == 2 && segs[0] == "ai-news" {
			return "ai-news/story"
		}
		return segs[0] + "/" + segs[1]
	}
	return segs[0]
}

func twoaiGSCCall(token, method, u string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := twoaiGSCHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, trunc(string(b), 300))
	}
	return 200, json.Unmarshal(b, out)
}

// twoaiGSCSite is the property the service account can read for this site:
// GSC_SITE_URL when set, otherwise the domain property, otherwise the URL
// prefix property.
func twoaiGSCSite(token string) (string, error) {
	if v := twoaiEnv("GSC_SITE_URL"); v != "" {
		return v, nil
	}
	var out struct {
		SiteEntry []struct {
			SiteURL         string `json:"siteUrl"`
			PermissionLevel string `json:"permissionLevel"`
		} `json:"siteEntry"`
	}
	if _, err := twoaiGSCCall(token, "GET", "https://www.googleapis.com/webmasters/v3/sites", nil, &out); err != nil {
		return "", err
	}
	best := ""
	for _, s := range out.SiteEntry {
		if s.PermissionLevel == "siteUnverifiedUser" {
			continue
		}
		switch s.SiteURL {
		case "sc-domain:theworldofai.org":
			return s.SiteURL, nil
		case "https://theworldofai.org/", "https://www.theworldofai.org/":
			best = s.SiteURL
		}
	}
	if best == "" {
		return "", fmt.Errorf("no Search Console property for theworldofai.org is shared with %s", gaSAEmail())
	}
	return best, nil
}

// twoaiGSC is the stage.
func twoaiGSC(db *sql.DB) error {
	twoaiGSCEnsure(db)
	token, err := gaSAToken(twoaiGSCScope)
	if err != nil {
		fmt.Printf("twoai_gsc: skipped, %v\n", err)
		return nil
	}
	site, err := twoaiGSCSite(token)
	if err != nil {
		fmt.Printf("twoai_gsc: skipped, %v. Stephen: Search Console, Settings, Users and permissions, Add user %s with Restricted permission.\n", err, gaSAEmail())
		return nil
	}
	pages, queries, days, err := twoaiGSCAnalytics(db, token, site)
	if err != nil {
		fmt.Printf("twoai_gsc: search analytics: %v\n", err)
	}
	inspected, err := twoaiGSCInspect(db, token, site)
	if err != nil {
		fmt.Printf("twoai_gsc: url inspection: %v\n", err)
	}
	twoaiGSCSectionCounts(db)
	fmt.Printf("twoai_gsc: %s, %d page days and %d query rows over %d days, %d URLs inspected ok=true\n", site, pages, queries, days, inspected)
	return nil
}

type gscRow struct {
	Keys        []string `json:"keys"`
	Clicks      float64  `json:"clicks"`
	Impressions float64  `json:"impressions"`
	CTR         float64  `json:"ctr"`
	Position    float64  `json:"position"`
}

func twoaiGSCQuery(token, site, start, end string, dims []string) ([]gscRow, error) {
	var all []gscRow
	for startRow := 0; ; startRow += 25000 {
		var out struct {
			Rows []gscRow `json:"rows"`
		}
		body := map[string]any{"startDate": start, "endDate": end, "dimensions": dims,
			"rowLimit": 25000, "startRow": startRow, "dataState": "all"}
		if _, err := twoaiGSCCall(token, "POST", "https://www.googleapis.com/webmasters/v3/sites/"+url.PathEscape(site)+"/searchAnalytics/query", body, &out); err != nil {
			return all, err
		}
		all = append(all, out.Rows...)
		if len(out.Rows) < 25000 {
			return all, nil
		}
	}
}

// twoaiGSCAnalytics reads Search Analytics from three days before the last
// day stored (90 days back on the first run) through yesterday.
func twoaiGSCAnalytics(db *sql.DB, token, site string) (int, int, int, error) {
	end := time.Now().UTC().AddDate(0, 0, -1)
	start := end.AddDate(0, 0, -90)
	var last sql.NullTime
	db.QueryRow(`SELECT max(day) FROM twoai_gsc_pages`).Scan(&last)
	if last.Valid {
		start = last.Time.AddDate(0, 0, -3)
	}
	s, e := start.Format("2006-01-02"), end.Format("2006-01-02")
	totals, err := twoaiGSCQuery(token, site, s, e, []string{"date"})
	if err != nil {
		return 0, 0, 0, err
	}
	for _, r := range totals {
		if len(r.Keys) == 1 {
			db.Exec(`INSERT INTO twoai_gsc_daily (day, impressions, clicks) VALUES ($1, $2, $3)
				ON CONFLICT (day) DO UPDATE SET impressions = EXCLUDED.impressions, clicks = EXCLUDED.clicks, loaded_at = now()`,
				r.Keys[0], int(r.Impressions), int(r.Clicks))
		}
	}
	pageRows, err := twoaiGSCQuery(token, site, s, e, []string{"date", "page"})
	if err != nil {
		return 0, 0, len(totals), err
	}
	for _, r := range pageRows {
		if len(r.Keys) == 2 {
			db.Exec(`INSERT INTO twoai_gsc_pages (day, page, clicks, impressions, ctr, position) VALUES ($1,$2,$3,$4,$5,$6)
				ON CONFLICT (day, page) DO UPDATE SET clicks = EXCLUDED.clicks, impressions = EXCLUDED.impressions,
				ctr = EXCLUDED.ctr, position = EXCLUDED.position, fetched_at = now()`,
				r.Keys[0], trunc(r.Keys[1], 2000), int(r.Clicks), int(r.Impressions), r.CTR, r.Position)
		}
	}
	queryRows, err := twoaiGSCQuery(token, site, s, e, []string{"date", "page", "query"})
	if err != nil {
		return len(pageRows), 0, len(totals), err
	}
	for _, r := range queryRows {
		if len(r.Keys) == 3 {
			db.Exec(`INSERT INTO twoai_gsc_queries (day, page, query, clicks, impressions, position) VALUES ($1,$2,$3,$4,$5,$6)
				ON CONFLICT (day, page, query) DO UPDATE SET clicks = EXCLUDED.clicks, impressions = EXCLUDED.impressions,
				position = EXCLUDED.position, fetched_at = now()`,
				r.Keys[0], trunc(r.Keys[1], 2000), trunc(r.Keys[2], 500), int(r.Clicks), int(r.Impressions), r.Position)
		}
	}
	return len(pageRows), len(queryRows), len(totals), nil
}

// twoaiGSCInspect inspects live URLs within the day's quota.
func twoaiGSCInspect(db *sql.DB, token, site string) (int, error) {
	var today int
	db.QueryRow(`SELECT count(*) FROM twoai_gsc_inspect WHERE inspected_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`).Scan(&today)
	room := twoaiGSCInspectPerDay - today
	if room <= 0 {
		return 0, nil
	}
	rows, err := db.Query(`SELECT r.url FROM twoai_url_registry r LEFT JOIN twoai_gsc_inspect i ON i.url = r.url
		WHERE r.resolution IS NULL AND r.last_seen_at > now() - interval '3 days'
		ORDER BY i.inspected_at NULLS FIRST, r.url LIMIT $1`, room)
	if err != nil {
		return 0, err
	}
	var urls []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			urls = append(urls, u)
		}
	}
	rows.Close()
	n := 0
	deadline := time.Now().Add(12 * time.Minute)
	for _, u := range urls {
		if time.Now().After(deadline) {
			break
		}
		var out struct {
			InspectionResult struct {
				IndexStatusResult struct {
					Verdict         string `json:"verdict"`
					CoverageState   string `json:"coverageState"`
					IndexingState   string `json:"indexingState"`
					RobotsTxtState  string `json:"robotsTxtState"`
					PageFetchState  string `json:"pageFetchState"`
					LastCrawlTime   string `json:"lastCrawlTime"`
					GoogleCanonical string `json:"googleCanonical"`
				} `json:"indexStatusResult"`
			} `json:"inspectionResult"`
		}
		status, err := twoaiGSCCall(token, "POST", "https://searchconsole.googleapis.com/v1/urlInspection/index:inspect",
			map[string]string{"inspectionUrl": u, "siteUrl": site}, &out)
		if err != nil {
			if status == 429 || status == 403 {
				// Quota or permission: the day is over for inspections.
				return n, err
			}
			continue
		}
		r := out.InspectionResult.IndexStatusResult
		var crawled any
		if t, err := time.Parse(time.RFC3339, r.LastCrawlTime); err == nil {
			crawled = t
		}
		db.Exec(`INSERT INTO twoai_gsc_inspect (url, section, verdict, coverage_state, indexing_state, robots_state,
				page_fetch_state, last_crawl, google_canonical, inspected_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now())
			ON CONFLICT (url) DO UPDATE SET section = EXCLUDED.section, verdict = EXCLUDED.verdict,
				coverage_state = EXCLUDED.coverage_state, indexing_state = EXCLUDED.indexing_state,
				robots_state = EXCLUDED.robots_state, page_fetch_state = EXCLUDED.page_fetch_state,
				last_crawl = EXCLUDED.last_crawl, google_canonical = EXCLUDED.google_canonical, inspected_at = now()`,
			u, twoaiGSCSection(u), r.Verdict, r.CoverageState, r.IndexingState, r.RobotsTxtState, r.PageFetchState,
			crawled, r.GoogleCanonical)
		n++
		time.Sleep(150 * time.Millisecond)
	}
	return n, nil
}

// twoaiGSCSectionCounts records today's per-section counts of what has been
// inspected. Indexed means Google's verdict PASS; never crawled means no last
// crawl time.
func twoaiGSCSectionCounts(db *sql.DB) {
	db.Exec(`INSERT INTO twoai_gsc_sections (day, section, inspected, indexed, not_indexed, never_crawled)
		SELECT current_date, i.section, count(*), count(*) FILTER (WHERE i.verdict = 'PASS'),
			count(*) FILTER (WHERE i.verdict <> 'PASS'), count(*) FILTER (WHERE i.last_crawl IS NULL)
		FROM twoai_gsc_inspect i GROUP BY i.section
		ON CONFLICT (day, section) DO UPDATE SET inspected = EXCLUDED.inspected, indexed = EXCLUDED.indexed,
			not_indexed = EXCLUDED.not_indexed, never_crawled = EXCLUDED.never_crawled`)
}

// twoaiGSCReport is the Search Console part of the daily bridge row: the
// last seven days of search, and the index state of what has been
// inspected, by section. The page detail is added on Mondays (rows 548 and
// 549 ask for it weekly).
func twoaiGSCReport(db *sql.DB) string {
	var b strings.Builder
	var imp, clk sql.NullInt64
	var pos sql.NullFloat64
	if db.QueryRow(`SELECT sum(impressions), sum(clicks) FROM twoai_gsc_daily WHERE day > current_date - 8 AND clicks IS NOT NULL`).Scan(&imp, &clk) != nil || !imp.Valid {
		return ""
	}
	db.QueryRow(`SELECT sum(position * impressions) / NULLIF(sum(impressions), 0) FROM twoai_gsc_pages WHERE day > current_date - 8`).Scan(&pos)
	fmt.Fprintf(&b, "Search Console, last 7 days: %d impressions, %d clicks", imp.Int64, clk.Int64)
	if pos.Valid {
		fmt.Fprintf(&b, ", average position %.1f", pos.Float64)
	}
	b.WriteString("\n")
	if rows, err := db.Query(`SELECT section, inspected, indexed, never_crawled FROM twoai_gsc_sections
		WHERE day = (SELECT max(day) FROM twoai_gsc_sections) ORDER BY inspected DESC LIMIT 12`); err == nil {
		var parts []string
		var ti, tx int
		for rows.Next() {
			var s string
			var i, x, nc int
			if rows.Scan(&s, &i, &x, &nc) == nil {
				ti += i
				tx += x
				parts = append(parts, fmt.Sprintf("%s %d of %d indexed (%d never crawled)", s, x, i, nc))
			}
		}
		rows.Close()
		if ti > 0 {
			fmt.Fprintf(&b, "Index state of the %d URLs inspected so far: %d indexed. By section: %s\n", ti, tx, strings.Join(parts, "; "))
		}
	}
	if time.Now().UTC().Weekday() == time.Monday {
		if rows, err := db.Query(`SELECT page, sum(impressions), sum(clicks), sum(position * impressions) / NULLIF(sum(impressions), 0)
			FROM twoai_gsc_pages WHERE day > current_date - 8 GROUP BY page ORDER BY 2 DESC LIMIT 15`); err == nil {
			b.WriteString("Pages by impressions, last 7 days:\n")
			for rows.Next() {
				var p string
				var i, c int
				var ps sql.NullFloat64
				if rows.Scan(&p, &i, &c, &ps) == nil {
					fmt.Fprintf(&b, "- %s: %d impressions, %d clicks, position %.1f\n", p, i, c, ps.Float64)
				}
			}
			rows.Close()
		}
	}
	return b.String()
}
