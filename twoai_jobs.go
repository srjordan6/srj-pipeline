package main

// ---- twoai_jobs: AI + security job listings and market dynamics ------------
//
// Fills twoai_jobs (individual listings) and twoai_jobs_market (aggregate
// market data) from free sources whose terms permit republication with
// attribution. The Indeed and Dice MCP integrations are deliberately NOT used
// here: their terms license job search for a user, not syndication on a site.
//
// Sources, all free:
//   No key      Remotive, RemoteOK, Arbeitnow, The Muse, and the public ATS
//               boards (Greenhouse / Ashby / Lever) of tracked AI and security
//               employers.
//   Key (env)   USAJobs (USAJOBS_API_KEY + USAJOBS_EMAIL), Adzuna
//               (ADZUNA_APP_ID + ADZUNA_APP_KEY), Jooble (JOOBLE_API_KEY).
//               A missing key skips that source with a stderr note; the stage
//               never fails because a key is absent.
//   Market      Indeed Hiring Lab AI tracker (share of postings mentioning
//               AI, daily series, free with attribution) and the InfoSec Job
//               Board public stats JSON (free with attribution).
//
// Every listing links to the ORIGINAL posting (source-integrity rule). A row's
// last_seen is bumped each run it is still present at its source; the page
// renders only rows seen within the freshness window, so vanished listings
// fall off without deletes (same reaper idea as twoaiPeople, kept as history
// instead of removed).

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// twoaiJobsFreshDays is the render window: a listing not re-seen at its source
// within this many days stops rendering. Three days tolerates a source being
// briefly down without dropping its listings (MCP-outage lesson generalised).
const twoaiJobsFreshDays = 3

var twoaiJobsClient = &http.Client{Timeout: 90 * time.Second}

func twoaiJobsGet(rawurl string, hdr map[string]string) ([]byte, error) {
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "theworldofai.org jobs pipeline (contact: stephen@srjconsultingservices.com)")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := twoaiJobsClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %d", rawurl, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// twoaiJobCategory classifies a title as "ai", "security", "ai-security", or
// "" (out of scope). Title-only on purpose: descriptions mention "security"
// and "AI" incidentally so often that matching them floods the set.
func twoaiJobCategory(title string) string {
	t := " " + strings.ToLower(title) + " "
	t = strings.NewReplacer("/", " ", ",", " ", "-", " ", "(", " ", ")", " ", "&", " ").Replace(t)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(t, w) {
				return true
			}
		}
		return false
	}
	ai := has(" ai ", " ml ", "machine learning", "artificial intelligence", "deep learning",
		" llm", "genai", "generative", "data scientist", "computer vision", " nlp ",
		"mlops", "ai engineer", "research scientist", "research engineer", "prompt engineer",
		"foundation model", "reinforcement learning", "neural")
	sec := has("security", "cybersecurity", "infosec", "penetration test", "pentest",
		" soc ", "threat", "appsec", "devsecops", "vulnerab", "incident response",
		" ciso", "red team", "blue team", "purple team", "zero trust", "malware",
		"forensic", "grc ", "identity and access", " iam ", "detection engineer",
		"security engineer", "security analyst", "cryptograph")
	switch {
	case ai && sec:
		return "ai-security"
	case ai:
		return "ai"
	case sec:
		return "security"
	}
	return ""
}

func twoaiJobsFetch(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_jobs (
		id bigserial PRIMARY KEY,
		source text NOT NULL,
		external_id text NOT NULL,
		title text NOT NULL,
		company text NOT NULL DEFAULT '',
		location text NOT NULL DEFAULT '',
		remote boolean NOT NULL DEFAULT false,
		salary text NOT NULL DEFAULT '',
		category text NOT NULL,
		url text NOT NULL,
		posted_on date,
		first_seen timestamptz NOT NULL DEFAULT now(),
		last_seen timestamptz NOT NULL DEFAULT now(),
		UNIQUE (source, external_id))`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_jobs_market (
		key text PRIMARY KEY, data jsonb NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	saved, skipped := 0, 0
	save := func(source, extID, title, company, location string, remote bool, salary, link, posted string) {
		title = strings.TrimSpace(title)
		if title == "" || link == "" || extID == "" {
			return
		}
		cat := twoaiJobCategory(title)
		if cat == "" {
			skipped++
			return
		}
		var postedOn any
		if posted != "" {
			if len(posted) >= 10 {
				if _, err := time.Parse("2006-01-02", posted[:10]); err == nil {
					postedOn = posted[:10]
				}
			}
		}
		if _, err := db.Exec(`INSERT INTO twoai_jobs
			(source, external_id, title, company, location, remote, salary, category, url, posted_on)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::date)
			ON CONFLICT (source, external_id) DO UPDATE SET
				title=EXCLUDED.title, company=EXCLUDED.company, location=EXCLUDED.location,
				remote=EXCLUDED.remote, salary=EXCLUDED.salary, category=EXCLUDED.category,
				url=EXCLUDED.url, posted_on=COALESCE(EXCLUDED.posted_on, twoai_jobs.posted_on),
				last_seen=now()`,
			source, extID, strings.TrimSpace(title), strings.TrimSpace(company),
			strings.TrimSpace(location), remote, strings.TrimSpace(salary), cat, link, postedOn); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_jobs: upsert:", err)
			return
		}
		saved++
	}
	warn := func(src string, err error) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "twoai_jobs: %s: %v (source skipped, existing rows age out naturally)\n", src, err)
		}
	}

	// ---- Public ATS boards of tracked AI + security employers. Slugs
	// verified 2026-08-11; a 404 means the company moved ATS and is logged,
	// never fatal. Extend these lists freely.
	greenhouse := map[string]string{
		"anthropic": "Anthropic", "cloudflare": "Cloudflare", "databricks": "Databricks",
		"xai": "xAI", "scaleai": "Scale AI", "okta": "Okta", "datadog": "Datadog",
		"sentinellabs": "SentinelOne",
	}
	for slug, name := range greenhouse {
		b, err := twoaiJobsGet("https://boards-api.greenhouse.io/v1/boards/"+slug+"/jobs", nil)
		if err != nil {
			warn("greenhouse/"+slug, err)
			continue
		}
		var d struct {
			Jobs []struct {
				ID       json.Number `json:"id"`
				Title    string      `json:"title"`
				URL      string      `json:"absolute_url"`
				Location struct {
					Name string `json:"name"`
				} `json:"location"`
				FirstPublished string `json:"first_published"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			warn("greenhouse/"+slug, err)
			continue
		}
		for _, j := range d.Jobs {
			loc := j.Location.Name
			save("greenhouse", slug+":"+j.ID.String(), j.Title, name, loc,
				strings.Contains(strings.ToLower(loc), "remote"), "", j.URL, j.FirstPublished)
		}
	}

	ashby := map[string]string{
		"openai": "OpenAI", "elevenlabs": "ElevenLabs", "cursor": "Cursor",
		"sierra": "Sierra", "runway": "Runway",
	}
	for slug, name := range ashby {
		b, err := twoaiJobsGet("https://api.ashbyhq.com/posting-api/job-board/"+slug, nil)
		if err != nil {
			warn("ashby/"+slug, err)
			continue
		}
		var d struct {
			Jobs []struct {
				ID          string `json:"id"`
				Title       string `json:"title"`
				Location    string `json:"location"`
				IsRemote    bool   `json:"isRemote"`
				IsListed    bool   `json:"isListed"`
				JobURL      string `json:"jobUrl"`
				PublishedAt string `json:"publishedAt"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			warn("ashby/"+slug, err)
			continue
		}
		for _, j := range d.Jobs {
			if !j.IsListed {
				continue
			}
			save("ashby", slug+":"+j.ID, j.Title, name, j.Location, j.IsRemote, "", j.JobURL, j.PublishedAt)
		}
	}

	lever := map[string]string{"palantir": "Palantir"}
	for slug, name := range lever {
		b, err := twoaiJobsGet("https://api.lever.co/v0/postings/"+slug+"?mode=json", nil)
		if err != nil {
			warn("lever/"+slug, err)
			continue
		}
		var d []struct {
			ID         string `json:"id"`
			Text       string `json:"text"`
			HostedURL  string `json:"hostedUrl"`
			CreatedAt  int64  `json:"createdAt"`
			Categories struct {
				Location string `json:"location"`
			} `json:"categories"`
			WorkplaceType string `json:"workplaceType"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			warn("lever/"+slug, err)
			continue
		}
		for _, j := range d {
			posted := ""
			if j.CreatedAt > 0 {
				posted = time.UnixMilli(j.CreatedAt).UTC().Format("2006-01-02")
			}
			save("lever", slug+":"+j.ID, j.Text, name, j.Categories.Location,
				j.WorkplaceType == "remote", "", j.HostedURL, posted)
		}
	}

	// ---- Remotive: remote jobs, free API, attribution + link back.
	for _, q := range []string{"machine learning", "artificial intelligence", "security"} {
		b, err := twoaiJobsGet("https://remotive.com/api/remote-jobs?search="+url.QueryEscape(q), nil)
		if err != nil {
			warn("remotive", err)
			continue
		}
		var d struct {
			Jobs []struct {
				ID       json.Number `json:"id"`
				Title    string      `json:"title"`
				Company  string      `json:"company_name"`
				Loc      string      `json:"candidate_required_location"`
				URL      string      `json:"url"`
				Salary   string      `json:"salary"`
				PubDate  string      `json:"publication_date"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			warn("remotive", err)
			continue
		}
		for _, j := range d.Jobs {
			save("remotive", j.ID.String(), j.Title, j.Company, j.Loc, true, j.Salary, j.URL, j.PubDate)
		}
	}

	// ---- RemoteOK: free JSON feed; element 0 is a legal notice, kept out by
	// the required-fields guard in save().
	if b, err := twoaiJobsGet("https://remoteok.com/api", nil); err != nil {
		warn("remoteok", err)
	} else {
		var d []struct {
			ID       json.Number `json:"id"`
			Position string      `json:"position"`
			Company  string      `json:"company"`
			Location string      `json:"location"`
			URL      string      `json:"url"`
			Date     string      `json:"date"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			warn("remoteok", err)
		} else {
			for _, j := range d {
				save("remoteok", j.ID.String(), j.Position, j.Company, j.Location, true, "", j.URL, j.Date)
			}
		}
	}

	// ---- Arbeitnow: free job board API, Europe-heavy.
	if b, err := twoaiJobsGet("https://www.arbeitnow.com/api/job-board-api", nil); err != nil {
		warn("arbeitnow", err)
	} else {
		var d struct {
			Data []struct {
				Slug      string `json:"slug"`
				Title     string `json:"title"`
				Company   string `json:"company_name"`
				Location  string `json:"location"`
				Remote    bool   `json:"remote"`
				URL       string `json:"url"`
				CreatedAt int64  `json:"created_at"`
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			warn("arbeitnow", err)
		} else {
			for _, j := range d.Data {
				posted := ""
				if j.CreatedAt > 0 {
					posted = time.Unix(j.CreatedAt, 0).UTC().Format("2006-01-02")
				}
				save("arbeitnow", j.Slug, j.Title, j.Company, j.Location, j.Remote, "", j.URL, posted)
			}
		}
	}

	// ---- The Muse: free public API, first pages of the tech categories.
	for page := 0; page < 5; page++ {
		b, err := twoaiJobsGet(fmt.Sprintf(
			"https://www.themuse.com/api/public/jobs?page=%d&category=%s&category=%s&category=%s",
			page, url.QueryEscape("Data and Analytics"), url.QueryEscape("Software Engineering"),
			url.QueryEscape("IT")), nil)
		if err != nil {
			warn("themuse", err)
			break
		}
		var d struct {
			Results []struct {
				ID      json.Number `json:"id"`
				Name    string      `json:"name"`
				PubDate string      `json:"publication_date"`
				Company struct {
					Name string `json:"name"`
				} `json:"company"`
				Locations []struct {
					Name string `json:"name"`
				} `json:"locations"`
				Refs struct {
					LandingPage string `json:"landing_page"`
				} `json:"refs"`
			} `json:"results"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			warn("themuse", err)
			break
		}
		for _, j := range d.Results {
			locs := []string{}
			remote := false
			for _, l := range j.Locations {
				locs = append(locs, l.Name)
				if strings.Contains(strings.ToLower(l.Name), "remote") {
					remote = true
				}
			}
			save("themuse", j.ID.String(), j.Name, j.Company.Name, strings.Join(locs, "; "),
				remote, "", j.Refs.LandingPage, j.PubDate)
		}
		if len(d.Results) == 0 {
			break
		}
	}

	// ---- USAJobs: the whole federal AI + cyber hiring pipeline. Free key,
	// most permissive republication terms of any job API.
	if key := os.Getenv("USAJOBS_API_KEY"); key == "" {
		fmt.Fprintln(os.Stderr, "twoai_jobs: USAJOBS_API_KEY unset, skipping USAJobs")
	} else {
		email := os.Getenv("USAJOBS_EMAIL")
		if email == "" {
			email = "stephen@srjconsultingservices.com"
		}
		for _, q := range []string{"artificial intelligence", "machine learning", "cybersecurity", "information security"} {
			b, err := twoaiJobsGet("https://data.usajobs.gov/api/search?ResultsPerPage=500&Keyword="+url.QueryEscape(q),
				map[string]string{"Authorization-Key": key, "Host": "data.usajobs.gov", "User-Agent": email})
			if err != nil {
				warn("usajobs", err)
				continue
			}
			var d struct {
				SearchResult struct {
					Items []struct {
						Desc struct {
							PositionID    string `json:"PositionID"`
							PositionTitle string `json:"PositionTitle"`
							OrgName       string `json:"OrganizationName"`
							PositionURI   string `json:"PositionURI"`
							Locations     []struct {
								LocationName string `json:"LocationName"`
							} `json:"PositionLocation"`
							Remuneration []struct {
								Min      string `json:"MinimumRange"`
								Max      string `json:"MaximumRange"`
								Interval string `json:"RateIntervalCode"`
							} `json:"PositionRemuneration"`
							PubStart string `json:"PublicationStartDate"`
						} `json:"MatchedObjectDescriptor"`
					} `json:"SearchResultItems"`
				} `json:"SearchResult"`
			}
			if err := json.Unmarshal(b, &d); err != nil {
				warn("usajobs", err)
				continue
			}
			for _, it := range d.SearchResult.Items {
				j := it.Desc
				loc := ""
				if len(j.Locations) > 0 {
					loc = j.Locations[0].LocationName
					if len(j.Locations) > 1 {
						loc += fmt.Sprintf(" +%d more", len(j.Locations)-1)
					}
				}
				sal := ""
				if len(j.Remuneration) > 0 && j.Remuneration[0].Min != "" {
					sal = "$" + j.Remuneration[0].Min + "–$" + j.Remuneration[0].Max
					if j.Remuneration[0].Interval == "PA" {
						sal += "/yr"
					}
				}
				save("usajobs", j.PositionID, j.PositionTitle, j.OrgName, loc,
					strings.Contains(strings.ToLower(loc), "remote"), sal, j.PositionURI, j.PubStart)
			}
		}
	}

	// ---- Adzuna: free 1,000 calls/month; eight calls a day is well inside.
	if id, k := os.Getenv("ADZUNA_APP_ID"), os.Getenv("ADZUNA_APP_KEY"); id == "" || k == "" {
		fmt.Fprintln(os.Stderr, "twoai_jobs: ADZUNA_APP_ID/ADZUNA_APP_KEY unset, skipping Adzuna")
	} else {
		for _, q := range []string{"artificial intelligence", "machine learning", "cybersecurity", "security engineer"} {
			b, err := twoaiJobsGet(fmt.Sprintf(
				"https://api.adzuna.com/v1/api/jobs/us/search/1?app_id=%s&app_key=%s&results_per_page=50&what=%s&content-type=application/json",
				id, k, url.QueryEscape(q)), nil)
			if err != nil {
				warn("adzuna", err)
				continue
			}
			var d struct {
				Results []struct {
					ID      string  `json:"id"`
					Title   string  `json:"title"`
					Created string  `json:"created"`
					URL     string  `json:"redirect_url"`
					SalMin  float64 `json:"salary_min"`
					SalMax  float64 `json:"salary_max"`
					Company struct {
						Name string `json:"display_name"`
					} `json:"company"`
					Location struct {
						Name string `json:"display_name"`
					} `json:"location"`
				} `json:"results"`
			}
			if err := json.Unmarshal(b, &d); err != nil {
				warn("adzuna", err)
				continue
			}
			for _, j := range d.Results {
				sal := ""
				if j.SalMin > 0 {
					sal = fmt.Sprintf("$%.0f–$%.0f/yr", j.SalMin, j.SalMax)
				}
				save("adzuna", j.ID, j.Title, j.Company.Name, j.Location.Name, false, sal, j.URL, j.Created)
			}
		}
	}

	// ---- Jooble: free partner API, POST with the key in the path.
	if key := os.Getenv("JOOBLE_API_KEY"); key == "" {
		fmt.Fprintln(os.Stderr, "twoai_jobs: JOOBLE_API_KEY unset, skipping Jooble")
	} else {
		for _, q := range []string{"artificial intelligence", "cybersecurity"} {
			body, _ := json.Marshal(map[string]string{"keywords": q, "location": "USA"})
			req, _ := http.NewRequest("POST", "https://jooble.org/api/"+key, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := twoaiJobsClient.Do(req)
			if err != nil {
				warn("jooble", err)
				continue
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
			resp.Body.Close()
			if resp.StatusCode != 200 {
				warn("jooble", fmt.Errorf("HTTP %d", resp.StatusCode))
				continue
			}
			var d struct {
				Jobs []struct {
					ID       json.Number `json:"id"`
					Title    string      `json:"title"`
					Company  string      `json:"company"`
					Location string      `json:"location"`
					Link     string      `json:"link"`
					Salary   string      `json:"salary"`
					Updated  string      `json:"updated"`
				} `json:"jobs"`
			}
			if err := json.Unmarshal(b, &d); err != nil {
				warn("jooble", err)
				continue
			}
			for _, j := range d.Jobs {
				save("jooble", j.ID.String(), j.Title, j.Company, j.Location, false, j.Salary, j.Link, j.Updated)
			}
		}
	}

	// ---- Market dynamics 1: Indeed Hiring Lab AI tracker. Daily share of
	// job postings mentioning AI, by country. Free for public use with
	// Hiring Lab cited as the source.
	if b, err := twoaiJobsGet("https://raw.githubusercontent.com/hiring-lab/ai-tracker/main/AI_posting.csv", nil); err != nil {
		warn("hiring_lab", err)
	} else {
		rd := csv.NewReader(bytes.NewReader(b))
		recs, err := rd.ReadAll()
		if err != nil || len(recs) < 2 {
			warn("hiring_lab", fmt.Errorf("csv parse: %v", err))
		} else {
			latest := map[string][2]string{} // country -> [date, share]
			var usSeries [][2]string
			for _, r := range recs[1:] {
				if len(r) < 3 {
					continue
				}
				date, country, share := r[0], r[1], r[2]
				if cur, ok := latest[country]; !ok || date > cur[0] {
					latest[country] = [2]string{date, share}
				}
				if country == "US" {
					usSeries = append(usSeries, [2]string{date, share})
				}
			}
			sort.Slice(usSeries, func(i, j int) bool { return usSeries[i][0] < usSeries[j][0] })
			// Month ends only, last 24 points, so the page carries a trend
			// without shipping seven years of daily rows.
			var trend []map[string]string
			seen := map[string]bool{}
			for i := len(usSeries) - 1; i >= 0 && len(trend) < 24; i-- {
				m := usSeries[i][0][:7]
				if seen[m] {
					continue
				}
				seen[m] = true
				trend = append([]map[string]string{{"date": usSeries[i][0], "share": usSeries[i][1]}}, trend...)
			}
			countries := []map[string]string{}
			for c, v := range latest {
				countries = append(countries, map[string]string{"country": c, "date": v[0], "share": v[1]})
			}
			sort.Slice(countries, func(i, j int) bool { return countries[i]["country"] < countries[j]["country"] })
			j, _ := json.Marshal(map[string]any{
				"source": "Indeed Hiring Lab AI Tracker",
				"url":    "https://github.com/hiring-lab/ai-tracker",
				"note":   "Share of job postings containing AI-related terms, seven-day trailing average.",
				"latest": countries, "us_trend": trend,
			})
			db.Exec(`INSERT INTO twoai_jobs_market (key, data) VALUES ('hiring_lab_ai_share', $1::jsonb)
				ON CONFLICT (key) DO UPDATE SET data=EXCLUDED.data, updated_at=now()`, string(j))
		}
	}

	// ---- Market dynamics 2: InfoSec Job Board aggregate stats. Free cached
	// JSON, attribution to infosecjobboard.com required and rendered.
	if b, err := twoaiJobsGet("https://www.infosecjobboard.com/api/public/stats", nil); err != nil {
		warn("infosecjobboard", err)
	} else if json.Valid(b) {
		db.Exec(`INSERT INTO twoai_jobs_market (key, data) VALUES ('infosec_stats', $1::jsonb)
			ON CONFLICT (key) DO UPDATE SET data=EXCLUDED.data, updated_at=now()`, string(b))
	}

	var active int
	db.QueryRow(`SELECT count(*) FROM twoai_jobs WHERE last_seen > now() - ($1||' days')::interval`,
		twoaiJobsFreshDays).Scan(&active)
	fmt.Printf("twoai_jobs: saved=%d out_of_scope=%d active=%d ok=true\n", saved, skipped, active)
	return nil
}

// twoaiJobs renders jobs/index.json, the AI Jobs and Market Dynamics hub
// published at /ai-ecosystem/ecosystem-entities-market-and-operations/{uid}/.
// Rendering reads twoai_jobs only, so `pipeline twoai` (the no-fetch fast
// path) keeps working; freshness comes from the fetch stage in the daily run.
func twoaiJobs(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	type job struct {
		Title    string `json:"title"`
		Company  string `json:"company"`
		Location string `json:"location,omitempty"`
		Remote   bool   `json:"remote,omitempty"`
		Salary   string `json:"salary,omitempty"`
		Category string `json:"category"`
		URL      string `json:"url"`
		Posted   string `json:"posted,omitempty"`
		Source   string `json:"source"`
	}
	rows, err := db.Query(`SELECT title, company, location, remote, salary, category, url,
			COALESCE(to_char(posted_on,'YYYY-MM-DD'),''), source
		FROM twoai_jobs
		WHERE last_seen > now() - ($1||' days')::interval
		ORDER BY posted_on DESC NULLS LAST, last_seen DESC
		LIMIT 800`, twoaiJobsFreshDays)
	if err != nil {
		return 0, err
	}
	jobs := []job{}
	counts := map[string]int{}
	bySource := map[string]int{}
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.Title, &j.Company, &j.Location, &j.Remote, &j.Salary,
			&j.Category, &j.URL, &j.Posted, &j.Source); err != nil {
			rows.Close()
			return 0, err
		}
		jobs = append(jobs, j)
		counts[j.Category]++
		bySource[j.Source]++
	}
	rows.Close()

	market := map[string]any{}
	mrows, err := db.Query(`SELECT key, data::text FROM twoai_jobs_market`)
	if err == nil {
		for mrows.Next() {
			var k, d string
			if mrows.Scan(&k, &d) == nil {
				var v any
				if json.Unmarshal([]byte(d), &v) == nil {
					market[k] = v
				}
			}
		}
		mrows.Close()
	}

	// Attribution block, rendered on the page: these terms are what make the
	// listings publishable at all, so the credits are data, not decoration.
	sources := []map[string]string{
		{"name": "USAJobs", "url": "https://www.usajobs.gov/", "note": "US federal openings, official OPM API"},
		{"name": "Adzuna", "url": "https://www.adzuna.com/", "note": "aggregated listings"},
		{"name": "Jooble", "url": "https://jooble.org/", "note": "aggregated listings"},
		{"name": "Remotive", "url": "https://remotive.com/", "note": "remote jobs"},
		{"name": "Remote OK", "url": "https://remoteok.com/", "note": "remote jobs"},
		{"name": "Arbeitnow", "url": "https://www.arbeitnow.com/", "note": "European listings"},
		{"name": "The Muse", "url": "https://www.themuse.com/", "note": "listings and employer profiles"},
		{"name": "Company career boards", "url": "", "note": "Greenhouse, Ashby, and Lever public postings, linked directly"},
		{"name": "Indeed Hiring Lab", "url": "https://www.hiringlab.org/", "note": "AI posting-share tracker (market data)"},
		{"name": "InfoSec Job Board", "url": "https://www.infosecjobboard.com/", "note": "cybersecurity hiring aggregates (market data)"},
	}

	if len(jobs) == 0 {
		fmt.Fprintln(os.Stderr, "twoai_build: no fresh jobs in twoai_jobs, keeping the existing page")
		return 0, nil
	}
	if err := upsert("jobs/index.json", "jobs-hub", map[string]any{
		"uid":       twoaiUID("section:ai-jobs"),
		"generated": today,
		"total":     len(jobs),
		"counts": map[string]int{
			"ai": counts["ai"], "security": counts["security"], "ai_security": counts["ai-security"],
		},
		"by_source": bySource,
		"jobs":      jobs,
		"market":    market,
		"sources":   sources,
	}); err != nil {
		return 0, err
	}
	return len(jobs), nil
}
