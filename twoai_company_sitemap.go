package main

// twoai_company_sitemap: read a company's whole site from its sitemap, not
// just the home page, and turn what it publishes into its company page.
//
// theworldofai rows 530 and 531, 2026-10-07. Stephen, on SRI International:
// look at every page of sri.com. Its sitemap has 6,769 URLs: about 100 core
// pages, 673 press releases and posts, 288 people profiles, 4,918
// publications, 271 Japanese pages and smaller sections. The company harvest
// reads one page per company, which says what a company is but not what it
// has done, who works there, or what it publishes.
//
// WHICH COMPANIES. One row per company in twoai_company_sitemaps (uid,
// sitemap_url, active), seeded with SRI. Adding a company is one row.
//
// WHAT IS READ. The sitemap index and its child sitemaps are read every run
// (a few requests). Each URL gets a section, from the child sitemap's name
// and then from its path: core, press, people, publication, or other.
// Japanese pages (/ja/) are recorded and never fetched: they translate the
// English ones. Pages are fetched core first, then people, then press
// releases newest first, then publications, at most twoaiSitemapPerRun a run
// and twoaiSitemapGap apart, identifying ourselves, so the whole site takes a
// few days of runs. A page is fetched again only when its sitemap lastmod
// moves.
//
// PUBLICATIONS. Only those in AI, computer science, security, speech and
// computer vision are kept, as Stephen asked. A publication page names its
// subject in its title, description and opening text, so those are matched
// against twoaiSitemapResearchRe; a page that does not match keeps its URL
// and title only, never its text. A kept publication with a DOI is linked to
// twoai_works by that DOI, the hard identifier, and never by title.
//
// WHAT THE PAGE GETS (built by twoai_build from these tables, see
// twoaiCompanySiteSection):
//   - press releases, newest first, each also written to twoai_company_events
//     as kind press_release, confirmed by the company itself;
//   - Innovations: dated milestones read from the company's own timeline
//     pages (paths naming timeline or years-of-innovation) by the site's
//     model, kept only when the year and the milestone's words are in the
//     page text, so nothing is added that the company did not say;
//   - researchers: people profiles whose text is about AI or security,
//     linked to the person's page on this site when the name matches one
//     exactly;
//   - publications kept, newest first.
//
// NEVER DELETE. A URL that leaves the sitemap is marked gone_at, not removed.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	twoaiSitemapPerRun = 250
	twoaiSitemapGap    = 1500 * time.Millisecond
	twoaiSitemapUA     = "Mozilla/5.0 (compatible; theworldofai.org company directory; info@srjconsultingservices.com)"
)

// Subjects Stephen asked for: AI, computer science, security, speech and
// computer vision.
var twoaiSitemapResearchRe = regexp.MustCompile(`(?i)\b(artificial intelligence|machine learning|deep learning|neural network|\bAI\b|large language model|LLM|natural language|computer science|computing|software|algorithm|cyber|security|privacy|cryptograph|malware|intrusion|formal (methods|verification)|speech|speaker recognition|spoken language|voice|computer vision|image (recognition|understanding)|object detection|robot|autonomous)`)

// People profiles kept as researchers.
var twoaiSitemapPeopleRe = regexp.MustCompile(`(?i)\b(artificial intelligence|machine learning|deep learning|\bAI\b|computer vision|speech|natural language|cyber|security|privacy|formal methods|robotics|autonomous systems|computer scien)`)

var (
	twoaiSitemapDOIRe   = regexp.MustCompile(`\b10\.\d{4,9}/[^\s"'<>]+`)
	twoaiSitemapTitleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	twoaiSitemapH1Re    = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	twoaiSitemapMetaRe  = regexp.MustCompile(`(?is)<meta\s+[^>]*>`)
	twoaiSitemapAttrRe  = regexp.MustCompile(`(?is)(property|name|content|itemprop)\s*=\s*("([^"]*)"|'([^']*)')`)
	twoaiSitemapTimeRe  = regexp.MustCompile(`(?is)<time[^>]+datetime\s*=\s*["']([^"']+)["']`)
	twoaiSitemapYearRe  = regexp.MustCompile(`\b(1[89]\d\d|20\d\d)\b`)
)

func twoaiSitemapEnsure(db *sql.DB) {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS twoai_company_sitemaps (uid text PRIMARY KEY, sitemap_url text NOT NULL,
			active boolean NOT NULL DEFAULT true, note text, added_on date NOT NULL DEFAULT current_date)`,
		`CREATE TABLE IF NOT EXISTS twoai_company_site_urls (url text PRIMARY KEY, uid text NOT NULL, section text NOT NULL,
			sitemap text, lastmod text, first_seen timestamptz NOT NULL DEFAULT now(), gone_at timestamptz,
			fetched_at timestamptz, fetched_lastmod text, status int, title text, published_on date, description text,
			body text, body_hash text, keep boolean, doi text, work_id text, person_path text, note text)`,
		`CREATE INDEX IF NOT EXISTS twoai_company_site_urls_uid ON twoai_company_site_urls (uid, section)`,
		`CREATE TABLE IF NOT EXISTS twoai_company_innovations (uid text NOT NULL, year int NOT NULL, title text NOT NULL,
			detail text NOT NULL DEFAULT '', source_url text NOT NULL, model text, body_hash text,
			added_on date NOT NULL DEFAULT current_date, PRIMARY KEY (uid, year, title))`,
		// SRI International, theworldofai row 531.
		`INSERT INTO twoai_company_sitemaps (uid, sitemap_url, note) VALUES ('d7018517', 'https://www.sri.com/sitemap_index.xml',
			'theworldofai row 531, Stephen 2026-10-07: every page of sri.com') ON CONFLICT DO NOTHING`,
	} {
		if _, err := db.Exec(q); err != nil {
			fmt.Println("twoai_company_sitemap schema:", err)
		}
	}
}

type twoaiSitemapLoc struct {
	Loc     string `xml:"loc"`
	Lastmod string `xml:"lastmod"`
}

type twoaiSitemapDoc struct {
	XMLName  xml.Name
	Sitemaps []twoaiSitemapLoc `xml:"sitemap"`
	URLs     []twoaiSitemapLoc `xml:"url"`
}

var twoaiSitemapHTTP = &http.Client{Timeout: 30 * time.Second}

func twoaiSitemapGet(u string) ([]byte, int, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", twoaiSitemapUA)
	resp, err := twoaiSitemapHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return b, resp.StatusCode, err
}

// twoaiSitemapSection files a URL by its path first, then by the child
// sitemap that listed it. Path first because a WordPress site lists press
// releases, people and publications alike in its post sitemaps: sri.com's
// index (Yoast, read 2026-10-07) is post-sitemap.xml to post-sitemap8.xml,
// page-sitemap.xml, and category, post_tag and author sitemaps, which are
// listing pages and are not read.
func twoaiSitemapSection(sitemap, raw string) string {
	u, err := url.Parse(raw)
	path := "/"
	if err == nil {
		path = strings.ToLower(u.Path)
	}
	if strings.HasPrefix(path, "/ja/") || path == "/ja" {
		return "ja"
	}
	sm := strings.ToLower(sitemap)
	if i := strings.LastIndex(sm, "/"); i >= 0 {
		sm = sm[i+1:]
	}
	has := func(s string, words ...string) bool {
		for _, w := range words {
			if strings.Contains(s, w) {
				return true
			}
		}
		return false
	}
	if has(sm, "category-sitemap", "post_tag-sitemap", "tag-sitemap", "author-sitemap") {
		return "taxonomy"
	}
	switch {
	case has(path, "/publication"):
		return "publication"
	case has(path, "/people/", "/person/", "/bios/", "/staff/", "/experts/", "/team/"):
		return "people"
	case has(path, "/press", "/news/", "/newsroom/", "/blog/", "/featured-story", "/story/", "/announcement"):
		return "press"
	}
	switch {
	case has(sm, "publication"):
		return "publication"
	case has(sm, "people", "person", "staff", "expert"):
		return "people"
	case has(sm, "press", "news"):
		return "press"
	case has(sm, "page-sitemap"):
		return "core"
	}
	return "other"
}

// twoaiSitemapMeta reads the meta tags of a page into a map, keyed by
// property, name or itemprop.
func twoaiSitemapMeta(h string) map[string]string {
	out := map[string]string{}
	for _, tag := range twoaiSitemapMetaRe.FindAllString(h, -1) {
		var key, content string
		for _, m := range twoaiSitemapAttrRe.FindAllStringSubmatch(tag, -1) {
			v := m[3]
			if v == "" {
				v = m[4]
			}
			switch strings.ToLower(m[1]) {
			case "content":
				content = v
			default:
				if key == "" {
					key = strings.ToLower(v)
				}
			}
		}
		if key != "" && content != "" {
			if _, ok := out[key]; !ok {
				out[key] = htmlToText(content)
			}
		}
	}
	return out
}

// twoaiSitemapTitle is the page's own title without the site name.
func twoaiSitemapTitle(h string, meta map[string]string, site string) string {
	t := meta["og:title"]
	if t == "" {
		if m := twoaiSitemapH1Re.FindStringSubmatch(h); m != nil {
			t = htmlToText(m[1])
		}
	}
	if t == "" {
		if m := twoaiSitemapTitleRe.FindStringSubmatch(h); m != nil {
			t = htmlToText(m[1])
		}
	}
	for _, sep := range []string{" | ", " - ", " – "} {
		if i := strings.LastIndex(t, sep); i > 0 && strings.Contains(strings.ToLower(t[i:]), strings.ToLower(site)) {
			t = t[:i]
		}
	}
	return strings.TrimSpace(t)
}

// twoaiSitemapDate is the page's publication date, from its own metadata.
func twoaiSitemapDate(h string, meta map[string]string) string {
	for _, k := range []string{"article:published_time", "datepublished", "date", "pubdate", "publish_date", "citation_publication_date"} {
		if v := meta[k]; len(v) >= 10 {
			if _, err := time.Parse("2006-01-02", v[:10]); err == nil {
				return v[:10]
			}
		}
	}
	if m := twoaiSitemapTimeRe.FindStringSubmatch(h); m != nil && len(m[1]) >= 10 {
		if _, err := time.Parse("2006-01-02", m[1][:10]); err == nil {
			return m[1][:10]
		}
	}
	return ""
}

// twoaiCompanySitemap is the stage.
func twoaiCompanySitemap(db *sql.DB) error {
	twoaiSitemapEnsure(db)
	rows, err := db.Query(`SELECT s.uid, s.sitemap_url, COALESCE(p.name, s.uid) FROM twoai_company_sitemaps s
		LEFT JOIN twoai_company_profiles p ON p.uid = s.uid WHERE s.active`)
	if err != nil {
		return err
	}
	type site struct{ uid, sitemap, name string }
	var sites []site
	for rows.Next() {
		var s site
		if rows.Scan(&s.uid, &s.sitemap, &s.name) == nil {
			sites = append(sites, s)
		}
	}
	rows.Close()
	deadline := time.Now().Add(twoaiStageDeadlineDefault - 2*time.Minute)
	if d, ok := twoaiStageDeadline["twoai_company_sitemap"]; ok {
		deadline = time.Now().Add(d - 2*time.Minute)
	}
	for _, s := range sites {
		listed, err := twoaiSitemapList(db, s.uid, s.sitemap)
		if err != nil {
			fmt.Printf("twoai_company_sitemap %s: sitemap: %v\n", s.name, err)
		}
		fetched, kept := twoaiSitemapFetch(db, s.uid, s.name, deadline)
		events := twoaiSitemapEvents(db, s.uid, s.name)
		people := twoaiSitemapPeople(db, s.uid)
		works := twoaiSitemapWorks(db, s.uid)
		innov := twoaiSitemapInnovations(db, s.uid, s.name)
		var total, done int
		db.QueryRow(`SELECT count(*) FILTER (WHERE section NOT IN ('ja','taxonomy') AND gone_at IS NULL),
			count(*) FILTER (WHERE section NOT IN ('ja','taxonomy') AND gone_at IS NULL AND fetched_at IS NOT NULL)
			FROM twoai_company_site_urls WHERE uid=$1`, s.uid).Scan(&total, &done)
		fmt.Printf("twoai_company_sitemap %s: %d URLs listed, %d fetched this run (%d kept), %d of %d read so far; %d press events, %d researchers linked, %d publications matched to works, %d innovations ok=true\n",
			s.name, listed, fetched, kept, done, total, events, people, works, innov)
		// How the URLs were filed, and the first path segment of anything not
		// filed, so the section rules can be checked against the real site.
		if r, err := db.Query(`SELECT section || CASE WHEN section = 'other' THEN ' /' || split_part(regexp_replace(url, '^https?://[^/]+/', ''), '/', 1) ELSE '' END, count(*)
			FROM twoai_company_site_urls WHERE uid = $1 AND gone_at IS NULL GROUP BY 1 ORDER BY 2 DESC LIMIT 15`, s.uid); err == nil {
			var parts []string
			for r.Next() {
				var k string
				var n int
				if r.Scan(&k, &n) == nil {
					parts = append(parts, fmt.Sprintf("%s %d", k, n))
				}
			}
			r.Close()
			fmt.Printf("twoai_company_sitemap %s sections: %s\n", s.name, strings.Join(parts, ", "))
		}
	}
	return nil
}

// twoaiSitemapList reads the sitemap index and records every URL.
func twoaiSitemapList(db *sql.DB, uid, index string) (int, error) {
	b, status, err := twoaiSitemapGet(index)
	if err != nil {
		return 0, err
	}
	if status != 200 {
		return 0, fmt.Errorf("HTTP %d", status)
	}
	var doc twoaiSitemapDoc
	if err := xml.Unmarshal(b, &doc); err != nil {
		return 0, err
	}
	type entry struct{ sitemap, loc, lastmod string }
	var all []entry
	for _, u := range doc.URLs {
		all = append(all, entry{index, u.Loc, u.Lastmod})
	}
	for _, sm := range doc.Sitemaps {
		cb, cs, err := twoaiSitemapGet(strings.TrimSpace(sm.Loc))
		if err != nil || cs != 200 {
			fmt.Printf("twoai_company_sitemap: child sitemap %s: HTTP %d %v\n", sm.Loc, cs, err)
			continue
		}
		var child twoaiSitemapDoc
		if xml.Unmarshal(cb, &child) != nil {
			continue
		}
		for _, u := range child.URLs {
			all = append(all, entry{strings.TrimSpace(sm.Loc), u.Loc, u.Lastmod})
		}
		time.Sleep(twoaiSitemapGap)
	}
	if len(all) == 0 {
		return 0, fmt.Errorf("no URLs in %s", index)
	}
	seen := map[string]bool{}
	for _, e := range all {
		loc := strings.TrimSpace(e.loc)
		if loc == "" || seen[loc] {
			continue
		}
		seen[loc] = true
		db.Exec(`INSERT INTO twoai_company_site_urls (url, uid, section, sitemap, lastmod) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (url) DO UPDATE SET lastmod = EXCLUDED.lastmod, sitemap = EXCLUDED.sitemap, gone_at = NULL`,
			loc, uid, twoaiSitemapSection(e.sitemap, loc), e.sitemap, strings.TrimSpace(e.lastmod))
	}
	// Gone from the sitemap: marked, never deleted.
	var listed []string
	for u := range seen {
		listed = append(listed, u)
	}
	db.Exec(`UPDATE twoai_company_site_urls SET gone_at = now() WHERE uid = $1 AND gone_at IS NULL AND NOT (url = ANY($2))`,
		uid, pq.Array(listed))
	return len(seen), nil
}

// twoaiSitemapFetch reads due pages, in the order Stephen's ask ranks them.
func twoaiSitemapFetch(db *sql.DB, uid, name string, deadline time.Time) (int, int) {
	rows, err := db.Query(`SELECT url, section FROM twoai_company_site_urls
		WHERE uid=$1 AND gone_at IS NULL AND section NOT IN ('ja','taxonomy')
		  AND (fetched_at IS NULL OR fetched_lastmod IS DISTINCT FROM lastmod)
		ORDER BY CASE section WHEN 'core' THEN 0 WHEN 'people' THEN 1 WHEN 'press' THEN 2 WHEN 'other' THEN 3 ELSE 4 END,
		  lastmod DESC NULLS LAST, url
		LIMIT $2`, uid, twoaiSitemapPerRun)
	if err != nil {
		return 0, 0
	}
	type due struct{ url, section string }
	var list []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.url, &d.section) == nil {
			list = append(list, d)
		}
	}
	rows.Close()
	fetched, kept := 0, 0
	for _, d := range list {
		if time.Now().After(deadline) {
			break
		}
		b, status, err := twoaiSitemapGet(d.url)
		time.Sleep(twoaiSitemapGap)
		if err != nil || status != 200 {
			db.Exec(`UPDATE twoai_company_site_urls SET fetched_at = now(), fetched_lastmod = lastmod, status = $2, note = $3 WHERE url = $1`,
				d.url, status, trunc(fmt.Sprint(err), 200))
			fetched++
			continue
		}
		h := string(b)
		meta := twoaiSitemapMeta(h)
		title := twoaiSitemapTitle(h, meta, name)
		desc := meta["og:description"]
		if desc == "" {
			desc = meta["description"]
		}
		text := htmlToText(h)
		pub := twoaiSitemapDate(h, meta)
		keep := true
		switch d.section {
		case "publication":
			keep = twoaiSitemapResearchRe.MatchString(title + " " + desc + " " + trunc(text, 3000))
		case "people":
			keep = twoaiSitemapPeopleRe.MatchString(title + " " + desc + " " + trunc(text, 4000))
		case "other":
			// Not filed by path: kept in full only when it is on the
			// subjects asked for, like a publication.
			keep = twoaiSitemapResearchRe.MatchString(title + " " + desc + " " + trunc(text, 3000))
		}
		var body, hash, doi any
		if keep {
			body = trunc(text, 60000)
			sum := sha256.Sum256([]byte(text))
			hash = hex.EncodeToString(sum[:8])
			if m := twoaiSitemapDOIRe.FindString(meta["citation_doi"] + " " + text); m != "" {
				doi = strings.ToLower(strings.TrimRight(m, ".,;)"))
			}
			kept++
		}
		var pubOn any
		if pub != "" {
			pubOn = pub
		}
		db.Exec(`UPDATE twoai_company_site_urls SET fetched_at = now(), fetched_lastmod = lastmod, status = 200,
			title = $2, description = $3, published_on = $4, body = $5, body_hash = $6, keep = $7, doi = $8, note = NULL
			WHERE url = $1`, d.url, trunc(title, 500), trunc(desc, 1000), pubOn, body, hash, keep, doi)
		fetched++
	}
	return fetched, kept
}

// twoaiSitemapEvents writes each dated press release to the company events
// timeline. The company's own announcement, so confirmed by the principal.
func twoaiSitemapEvents(db *sql.DB, uid, name string) int {
	res, err := db.Exec(`INSERT INTO twoai_company_events (uid, entity_uid, kind, headline, detail, announced_on,
			confirmed_by_principal, source_name, source_url)
		SELECT substr(md5('press:' || url), 1, 8), uid, 'press_release', title, COALESCE(description, ''), published_on,
			true, $2 || ' press release', url
		FROM twoai_company_site_urls
		WHERE uid = $1 AND section = 'press' AND status = 200 AND published_on IS NOT NULL AND COALESCE(title, '') <> ''
		ON CONFLICT (uid) DO UPDATE SET headline = EXCLUDED.headline, detail = EXCLUDED.detail,
			announced_on = EXCLUDED.announced_on, updated_at = now()
		WHERE (twoai_company_events.headline, twoai_company_events.detail, twoai_company_events.announced_on)
			IS DISTINCT FROM (EXCLUDED.headline, EXCLUDED.detail, EXCLUDED.announced_on)`, uid, name)
	if err != nil {
		fmt.Println("twoai_company_sitemap events:", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// twoaiSitemapPeople links researcher profiles to person pages on this
// site, on an exact name match only.
func twoaiSitemapPeople(db *sql.DB, uid string) int {
	db.Exec(`UPDATE twoai_company_site_urls u SET person_path = '/ai-ecosystem/ecosystem-entities-market-and-operations/' || p.data->>'uid' || '/'
		FROM twoai_pages p
		WHERE u.uid = $1 AND u.section = 'people' AND u.keep AND p.path ~ '^people/[0-9a-f]{8}\.json$'
		  AND lower(p.data->>'name') = lower(u.title) AND COALESCE(p.data->>'uid','') <> ''
		  AND u.person_path IS DISTINCT FROM '/ai-ecosystem/ecosystem-entities-market-and-operations/' || p.data->>'uid' || '/'`, uid)
	var n int
	db.QueryRow(`SELECT count(*) FROM twoai_company_site_urls WHERE uid = $1 AND section = 'people' AND person_path IS NOT NULL`, uid).Scan(&n)
	return n
}

// twoaiSitemapWorks links kept publications to the works corpus by DOI.
func twoaiSitemapWorks(db *sql.DB, uid string) int {
	db.Exec(`UPDATE twoai_company_site_urls u SET work_id = w.id::text
		FROM twoai_works w
		WHERE u.uid = $1 AND u.section = 'publication' AND u.keep AND u.doi IS NOT NULL AND u.work_id IS NULL
		  AND lower(w.doi) = u.doi`, uid)
	var n int
	db.QueryRow(`SELECT count(*) FROM twoai_company_site_urls WHERE uid = $1 AND section = 'publication' AND work_id IS NOT NULL`, uid).Scan(&n)
	return n
}

// twoaiSitemapInnovations reads dated milestones from the company's own
// timeline pages, once per version of each page.
func twoaiSitemapInnovations(db *sql.DB, uid, name string) int {
	rows, err := db.Query(`SELECT url, body, body_hash FROM twoai_company_site_urls
		WHERE uid = $1 AND status = 200 AND body IS NOT NULL
		  AND (url ~* 'timeline' OR url ~* 'years-of-innovation' OR url ~* 'history')
		  AND NOT EXISTS (SELECT 1 FROM twoai_company_innovations i WHERE i.uid = $1 AND i.source_url = url AND i.body_hash = body_hash)`, uid)
	if err != nil {
		return twoaiSitemapInnovationCount(db, uid)
	}
	type page struct{ url, body, hash string }
	var pages []page
	for rows.Next() {
		var p page
		if rows.Scan(&p.url, &p.body, &p.hash) == nil {
			pages = append(pages, p)
		}
	}
	rows.Close()
	system := `You read a company's own history page and list its dated innovations. Return ONLY a JSON array, no prose, no fences:
[{"year": 1969, "title": "short name of the innovation, in the page's own words", "detail": "one plain sentence on what it was, from the page"}]
HARD RULES: only innovations the page itself states with a year; the year and the title words must appear in the page text; never add anything from general knowledge; no hyphens in prose.`
	for _, p := range pages {
		raw, model, err := twoaiGenerate("COMPANY_INNOVATIONS", system, fmt.Sprintf("COMPANY: %s\nPAGE: %s\n\nTEXT:\n%s", name, p.url, trunc(p.body, 24000)))
		if err != nil {
			fmt.Printf("twoai_company_sitemap innovations %s: %v\n", p.url, err)
			continue
		}
		if i := strings.Index(raw, "["); i >= 0 {
			if k := strings.LastIndex(raw, "]"); k > i {
				raw = raw[i : k+1]
			}
		}
		var items []struct {
			Year   int    `json:"year"`
			Title  string `json:"title"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal([]byte(raw), &items) != nil {
			continue
		}
		low := strings.ToLower(p.body)
		for _, it := range items {
			it.Title = strings.TrimSpace(it.Title)
			if it.Year < 1800 || it.Year > time.Now().Year() || it.Title == "" ||
				!strings.Contains(p.body, fmt.Sprint(it.Year)) || !twoaiSitemapWordsIn(it.Title, low) {
				continue
			}
			db.Exec(`INSERT INTO twoai_company_innovations (uid, year, title, detail, source_url, model, body_hash)
				VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (uid, year, title) DO UPDATE SET detail = EXCLUDED.detail,
				source_url = EXCLUDED.source_url, model = EXCLUDED.model, body_hash = EXCLUDED.body_hash`,
				uid, it.Year, trunc(it.Title, 200), trunc(strings.TrimSpace(it.Detail), 400), p.url, model, p.hash)
		}
		// Recorded as read even when nothing passed, so the page is not
		// sent again until it changes.
		db.Exec(`UPDATE twoai_company_innovations SET body_hash = $3 WHERE uid = $1 AND source_url = $2`, uid, p.url, p.hash)
	}
	return twoaiSitemapInnovationCount(db, uid)
}

func twoaiSitemapInnovationCount(db *sql.DB, uid string) int {
	var n int
	db.QueryRow(`SELECT count(*) FROM twoai_company_innovations WHERE uid = $1`, uid).Scan(&n)
	return n
}

// twoaiSitemapWordsIn says whether most of a title's significant words are
// in the text, so a milestone the model invented or reworded is dropped.
func twoaiSitemapWordsIn(title, lowText string) bool {
	words := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	sig, hit := 0, 0
	for _, w := range words {
		if len(w) < 4 {
			continue
		}
		sig++
		if strings.Contains(lowText, w) {
			hit++
		}
	}
	return sig > 0 && hit*3 >= sig*2
}

// twoaiCompanySiteSection is what the company page shows from the crawl:
// innovations, the newest press releases, researchers and publications.
// Nil when the company has no crawl.
func twoaiCompanySiteSection(db *sql.DB, uid string) map[string]any {
	var listed int
	if db.QueryRow(`SELECT count(*) FROM twoai_company_site_urls WHERE uid = $1 AND section NOT IN ('ja','taxonomy') AND gone_at IS NULL`, uid).Scan(&listed) != nil || listed == 0 {
		return nil
	}
	out := map[string]any{"listed": listed}
	var innovations []map[string]any
	if rows, err := db.Query(`SELECT year, title, detail, source_url FROM twoai_company_innovations WHERE uid = $1 ORDER BY year, title`, uid); err == nil {
		for rows.Next() {
			var y int
			var t, d, u string
			if rows.Scan(&y, &t, &d, &u) == nil {
				innovations = append(innovations, map[string]any{"year": y, "title": t, "detail": d, "url": u})
			}
		}
		rows.Close()
	}
	list := func(section, extra string, limit int) []map[string]any {
		var items []map[string]any
		rows, err := db.Query(`SELECT url, title, COALESCE(published_on::text, ''), COALESCE(person_path, ''), COALESCE(work_id, '')
			FROM twoai_company_site_urls WHERE uid = $1 AND section = $2 AND status = 200 AND keep AND gone_at IS NULL
			  AND COALESCE(title, '') <> '' `+extra+`
			ORDER BY published_on DESC NULLS LAST, title LIMIT $3`, uid, section, limit)
		if err != nil {
			return nil
		}
		defer rows.Close()
		for rows.Next() {
			var u, t, d, pp, w string
			if rows.Scan(&u, &t, &d, &pp, &w) == nil {
				it := map[string]any{"url": u, "title": t}
				if d != "" {
					it["date"] = d
				}
				if pp != "" {
					it["person_path"] = pp
				}
				items = append(items, it)
			}
		}
		return items
	}
	press := list("press", "AND published_on IS NOT NULL", 12)
	people := list("people", "", 60)
	pubs := list("publication", "", 12)
	sort.SliceStable(people, func(i, j int) bool {
		_, a := people[i]["person_path"]
		_, b := people[j]["person_path"]
		if a != b {
			return a
		}
		return fmt.Sprint(people[i]["title"]) < fmt.Sprint(people[j]["title"])
	})
	counts := map[string]int{}
	if rows, err := db.Query(`SELECT section, count(*) FROM twoai_company_site_urls WHERE uid = $1 AND status = 200 AND keep AND gone_at IS NULL GROUP BY 1`, uid); err == nil {
		for rows.Next() {
			var s string
			var n int
			if rows.Scan(&s, &n) == nil {
				counts[s] = n
			}
		}
		rows.Close()
	}
	if len(innovations) > 0 {
		out["innovations"] = innovations
	}
	if len(press) > 0 {
		out["press"] = press
	}
	if len(people) > 0 {
		out["researchers"] = people
	}
	if len(pubs) > 0 {
		out["publications"] = pubs
	}
	out["counts"] = counts
	return out
}
