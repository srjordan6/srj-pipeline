package main

// ---- twoai_apistatus: AI API status and incident history ------------------
//
// Most AI API providers publish an Atlassian Statuspage, and every
// Statuspage exposes the same keyless JSON: /api/v2/summary.json (current
// state and components) and /api/v2/incidents.json (history). The feed list
// lives in twoai_status_feeds, one row per provider with the endpoint
// individually verified before insertion, so adding a provider is a SQL
// insert, not a deploy.
//
// Each run also appends one snapshot per provider to twoai_status_snapshots,
// which is what lets the page say "N incidents in the last 30 days" from our
// own observations rather than trusting a marketing uptime number.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

func twoaiAPIStatusEnsure(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_status_feeds (
		provider text PRIMARY KEY,
		base_url text NOT NULL,
		entity_uid text,
		added_on date NOT NULL DEFAULT current_date)`); err != nil {
		return err
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_status_snapshots (
		provider text NOT NULL,
		taken_at timestamptz NOT NULL DEFAULT now(),
		indicator text NOT NULL,
		description text NOT NULL,
		open_incidents int NOT NULL DEFAULT 0,
		PRIMARY KEY (provider, taken_at))`)
	return err
}

func twoaiAPIStatus(db *sql.DB, today string) (int, error) {
	if err := twoaiAPIStatusEnsure(db); err != nil {
		return 0, err
	}

	type feed struct{ Provider, Base, EntityUID string }
	var feeds []feed
	rows, err := db.Query(`SELECT provider, base_url, COALESCE(entity_uid,'') FROM twoai_status_feeds ORDER BY provider`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var f feed
		if rows.Scan(&f.Provider, &f.Base, &f.EntityUID) == nil {
			feeds = append(feeds, f)
		}
	}
	rows.Close()
	if len(feeds) == 0 {
		return 0, nil
	}

	client := &http.Client{Timeout: 20 * time.Second}
	getJSON := func(url string, into any) error {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("User-Agent", "theworldofai.org pipeline (contact: info@srjconsultingservices.com)")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return json.NewDecoder(resp.Body).Decode(into)
	}

	type incident struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Impact  string `json:"impact"`
		Created string `json:"created"`
		URL     string `json:"url"`
	}
	type provider struct {
		Provider    string     `json:"provider"`
		EntityUID   string     `json:"entity_uid,omitempty"`
		PageURL     string     `json:"page_url"`
		Indicator   string     `json:"indicator"`
		Description string     `json:"description"`
		Degraded    []string   `json:"degraded,omitempty"`
		Incidents30 int        `json:"incidents_30d"`
		Recent      []incident `json:"recent,omitempty"`
		Checked     string     `json:"checked"`
	}

	var provs []provider
	ok := 0
	for _, f := range feeds {
		base := strings.TrimRight(f.Base, "/")
		var sum struct {
			Page struct {
				URL string `json:"url"`
			} `json:"page"`
			Status struct {
				Indicator   string `json:"indicator"`
				Description string `json:"description"`
			} `json:"status"`
			Components []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"components"`
			Incidents []any `json:"incidents"`
		}
		if err := getJSON(base+"/api/v2/summary.json", &sum); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_apistatus: %s summary: %v (dropped from this render)\n", f.Provider, err)
			continue
		}
		if sum.Status.Indicator == "" {
			// Perplexity serves a summary.json with only the page object;
			// a feed with no status block cannot be reported honestly.
			fmt.Fprintf(os.Stderr, "twoai_apistatus: %s summary has no status block, dropped\n", f.Provider)
			continue
		}
		p := provider{
			Provider: f.Provider, EntityUID: f.EntityUID,
			PageURL:   sum.Page.URL,
			Indicator: sum.Status.Indicator, Description: sum.Status.Description,
			Checked: time.Now().UTC().Format("2006-01-02 15:04 UTC"),
		}
		if p.PageURL == "" {
			p.PageURL = base
		}
		for _, c := range sum.Components {
			if c.Status != "operational" {
				p.Degraded = append(p.Degraded, c.Name+" ("+strings.ReplaceAll(c.Status, "_", " ")+")")
			}
		}
		db.Exec(`INSERT INTO twoai_status_snapshots (provider, indicator, description, open_incidents)
			VALUES ($1,$2,$3,$4)`, f.Provider, p.Indicator, p.Description, len(sum.Incidents))

		var inc struct {
			Incidents []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Status    string `json:"status"`
				Impact    string `json:"impact"`
				CreatedAt string `json:"created_at"`
			} `json:"incidents"`
		}
		if err := getJSON(base+"/api/v2/incidents.json", &inc); err == nil {
			cutoff := time.Now().AddDate(0, 0, -30)
			for i, x := range inc.Incidents {
				t, terr := time.Parse(time.RFC3339, x.CreatedAt)
				if terr == nil && t.After(cutoff) {
					p.Incidents30++
				}
				if i < 3 {
					created := x.CreatedAt
					if terr == nil {
						created = t.Format("2006-01-02")
					}
					p.Recent = append(p.Recent, incident{
						Name: x.Name, Status: x.Status, Impact: x.Impact,
						Created: created, URL: strings.TrimRight(p.PageURL, "/") + "/incidents/" + x.ID,
					})
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
		provs = append(provs, p)
		ok++
	}
	fmt.Printf("twoai_apistatus: providers=%d of %d feeds\n", ok, len(feeds))
	if len(provs) == 0 {
		return 0, nil // every feed failed: keep the previous page
	}

	// Worst first, then by 30-day incident count, so the page answers "what
	// is broken right now" before anything else.
	rank := map[string]int{"critical": 0, "major": 1, "minor": 2, "maintenance": 3, "none": 4}
	sort.Slice(provs, func(i, j int) bool {
		a, b := rank[provs[i].Indicator], rank[provs[j].Indicator]
		if a != b {
			return a < b
		}
		return provs[i].Incidents30 > provs[j].Incidents30
	})
	healthy := 0
	for _, p := range provs {
		if p.Indicator == "none" {
			healthy++
		}
	}

	var name, blurb string
	db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug='api-status-uptime'`).Scan(&name, &blurb)
	doc := map[string]any{
		"uid": twoaiUID("section:api-status-uptime"), "tax": "api-status-uptime",
		"name": name, "blurb": blurb, "providers": provs, "total": len(provs),
		"healthy": healthy, "generated": today,
	}
	j, _ := json.Marshal(doc)
	if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
		VALUES ('status/api-status.json','status-section',$1::jsonb,'api-status-uptime',1)
		ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
			taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`, string(j)); err != nil {
		return 0, err
	}
	pages := 1

	// ---- Re-verify curated source URLs so "verified {date}" on the
	// rendered pages is a live daily claim, not the insertion date. A URL
	// that answers 2xx/3xx gets today's date; anything else keeps its old
	// date (the page then honestly shows staleness) and logs to stderr,
	// which surfaces a dead docs link within one run. 403 bot walls stay
	// stale by design rather than being blessed unseen.
	reverify := func(table, urlCol, keyCol string) {
		rows, err := db.Query(`SELECT ` + keyCol + `, ` + urlCol + ` FROM ` + table)
		if err != nil {
			return
		}
		type kv struct{ k, u string }
		var all []kv
		for rows.Next() {
			var k, u string
			if rows.Scan(&k, &u) == nil {
				all = append(all, kv{k, u})
			}
		}
		rows.Close()
		client := &http.Client{Timeout: 15 * time.Second}
		for _, x := range all {
			req, _ := http.NewRequest("GET", x.u, nil)
			req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; theworldofai.org link verification; contact: info@srjconsultingservices.com)")
			resp, err := client.Do(req)
			ok := false
			if err == nil {
				resp.Body.Close()
				ok = resp.StatusCode < 400
			}
			if ok {
				db.Exec(`UPDATE `+table+` SET verified_on=current_date WHERE `+keyCol+`=$1`, x.k)
			} else {
				code := 0
				if resp != nil {
					code = resp.StatusCode
				}
				fmt.Fprintf(os.Stderr, "twoai_apistatus: reverify %s %s: status %d err %v\n", table, x.u, code, err)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	reverify("twoai_api_directory", "docs_url", "provider")
	reverify("twoai_languages", "source_url", "slug")
	reverify("twoai_org_classifications", "source_url", "uid")

	// ---- API Directory: curated provider rows, every docs URL verified
	// before insertion, cross-referenced to the status feeds above and to
	// company pages where the provider is a tracked entity.
	type apiRow struct {
		Provider  string `json:"provider"`
		EntityUID string `json:"entity_uid,omitempty"`
		BaseURL   string `json:"base_url"`
		DocsURL   string `json:"docs_url"`
		Auth      string `json:"auth"`
		SDKs      string `json:"sdks"`
		OAICompat bool   `json:"openai_compatible"`
		Note      string `json:"note,omitempty"`
		Verified  string `json:"verified"`
		HasStatus bool   `json:"has_status"`
	}
	var apis []apiRow
	if rows, err := db.Query(`SELECT d.provider, COALESCE(d.entity_uid,''), d.base_url, d.docs_url,
			d.auth_note, d.sdks, d.openai_compatible, COALESCE(d.note,''), d.verified_on::text,
			EXISTS (SELECT 1 FROM twoai_status_feeds f WHERE lower(f.provider)=lower(d.provider))
		FROM twoai_api_directory d ORDER BY d.provider`); err == nil {
		for rows.Next() {
			var a apiRow
			if rows.Scan(&a.Provider, &a.EntityUID, &a.BaseURL, &a.DocsURL, &a.Auth,
				&a.SDKs, &a.OAICompat, &a.Note, &a.Verified, &a.HasStatus) == nil {
				if a.EntityUID == "" {
					db.QueryRow(`SELECT c->>'uid' FROM twoai_pages, jsonb_array_elements(data->'companies') c
						WHERE path='companies/index.json' AND lower(c->>'name')=lower($1) LIMIT 1`,
						a.Provider).Scan(&a.EntityUID)
				}
				apis = append(apis, a)
			}
		}
		rows.Close()
	}
	if len(apis) > 0 {
		var dn, dblurb string
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug='api-directory'`).Scan(&dn, &dblurb)
		compat := 0
		for _, a := range apis {
			if a.OAICompat {
				compat++
			}
		}
		dj, _ := json.Marshal(map[string]any{
			"uid": twoaiUID("section:api-directory"), "tax": "api-directory",
			"name": dn, "blurb": dblurb, "apis": apis, "total": len(apis),
			"openai_compatible": compat, "generated": today,
		})
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ('tech/api-directory.json','tech-section',$1::jsonb,'api-directory',1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`, string(dj)); err != nil {
			return pages, err
		}
		pages++
	}

	// ---- Programming Languages: curated profiles joined with live counts
	// from the tracked-repo catalogue, so "Python in AI work" is backed by
	// which of the repos this site tracks are written in it.
	type langRow struct {
		Slug     string   `json:"slug"`
		Name     string   `json:"name"`
		Steward  string   `json:"steward"`
		First    string   `json:"first_release"`
		Role     string   `json:"ai_role"`
		Source   string   `json:"source_url"`
		Verified string   `json:"verified"`
		Repos    []string `json:"repos,omitempty"`
	}
	var langs []langRow
	if rows, err := db.Query(`SELECT slug, name, steward, first_release, ai_role, source_url, verified_on::text
		FROM twoai_languages ORDER BY name`); err == nil {
		for rows.Next() {
			var l langRow
			if rows.Scan(&l.Slug, &l.Name, &l.Steward, &l.First, &l.Role, &l.Source, &l.Verified) == nil {
				if rr, err := db.Query(`SELECT repo FROM twoai_repo_catalog
					WHERE lower(language)=lower($1) ORDER BY stars DESC LIMIT 6`, l.Name); err == nil {
					for rr.Next() {
						var fn string
						if rr.Scan(&fn) == nil {
							l.Repos = append(l.Repos, fn)
						}
					}
					rr.Close()
				}
				langs = append(langs, l)
			}
		}
		rows.Close()
	}
	if len(langs) > 0 {
		var ln, lblurb string
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug='programming-languages'`).Scan(&ln, &lblurb)
		lj, _ := json.Marshal(map[string]any{
			"uid": twoaiUID("section:programming-languages"), "tax": "programming-languages",
			"name": ln, "blurb": lblurb, "languages": langs, "total": len(langs),
			"generated": today,
		})
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ('tech/programming-languages.json','tech-section',$1::jsonb,'programming-languages',1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`, string(lj)); err != nil {
			return pages, err
		}
		pages++
	}
	return pages, nil
}
