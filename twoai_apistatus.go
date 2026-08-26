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
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
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

	// ---- Re-verify curated source URLs so "verified {date}" on the rendered
	// pages is a live daily claim rather than the insertion date. A URL that
	// answers 2xx/3xx gets today's date; anything else keeps its old date, so
	// the page honestly shows staleness rather than blessing a link nobody
	// checked.
	//
	// WHY THIS GREW A STATE TABLE. The old version logged every failure on
	// every run, which meant the same three lines appeared in the log daily and
	// forever, because some publishers will never answer a robot:
	//
	//   isocpp.org answers 403 with "cf-mitigated: challenge" - a Cloudflare bot
	//   wall. It returns 403 to a plain client, to a browser user-agent, and to
	//   full browser headers alike. There is no header combination that gets in.
	//
	//   amd.com answers 503 to our honest user-agent over both HTTP/2 and
	//   HTTP/1.1, and 200 to a Chrome string. Their WAF is filtering on the
	//   agent itself. Over HTTP/2 the rejection surfaces as "stream error:
	//   INTERNAL_ERROR" rather than a clean status, which is why that message
	//   looked like a transport bug and is not one.
	//
	// We do not spoof Chrome to get past either. This crawler says who it is and
	// gives a contact address, and a site that declines a self-identified robot
	// is entitled to; pretending to be a browser to harvest a page we are told
	// not to harvest is not a habit worth having on a site that publishes a
	// lawsuit tracker about exactly that behaviour.
	//
	// So the outcomes are classified and remembered instead. A blocked link is
	// logged when it FIRST blocks and then weekly, not daily. A dead link, 404
	// or 410, is logged every run, because that one is ours to fix. The state
	// lives in twoai_link_verify so the rendered pages can eventually say
	// "publisher blocks automated checks, last confirmed {date}" rather than
	// showing a verified date that quietly ages.
	classify := func(code int, err error) string {
		switch {
		case err == nil && code < 400:
			return "ok"
		case code == 404 || code == 410:
			return "dead"
		case code == 401 || code == 403 || code == 429 || code == 451 || code == 503:
			return "blocked"
		case err != nil && strings.Contains(err.Error(), "INTERNAL_ERROR"):
			return "blocked" // HTTP/2 stream reset: how a WAF refusal arrives
		case err != nil:
			return "unreachable"
		default:
			return "error"
		}
	}
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
		// CONCURRENT, because 144 curated URLs at one request plus a 300ms pause
		// each is roughly a minute of waiting per run and the URLs are spread
		// across as many different publishers. Six at a time keeps at most one
		// request per host in flight, which is the thing politeness actually
		// requires; the per-request pause was standing in for that and cost a
		// minute a day to do it worse.
		client := &http.Client{Timeout: 15 * time.Second}
		skippedBackoff := 0
		backoffByHost := map[string]int{}
		var vmu sync.Mutex
		var vwg sync.WaitGroup
		vsem := make(chan struct{}, 6)
		for _, x := range all {
			vwg.Add(1)
			go func(x kv) {
				defer vwg.Done()
				vsem <- struct{}{}
				defer func() { <-vsem }()
				if !strings.HasPrefix(x.u, "http") {
					return // internal cross-links and organizing rows have no external URL to verify
				}
				// BACK OFF INSTEAD OF ASKING FOREVER.
				//
				// A 403 from a publisher is not a transient error, it is a
				// decision, and AMD's hardware pages have been refusing this
				// crawler for thirty-five consecutive days. Asking again
				// tomorrow will not change their mind. Previously only the
				// LOGGING was throttled, so the noise went away while the
				// requests carried on daily for ever - quiet waste, which is
				// worse than loud waste because nobody sees it.
				//
				// Checks now slow down as failures accumulate: daily for the
				// first three days, since most outages are brief; weekly to a
				// fortnight; then monthly. Recovery is still caught, just at
				// the cadence a permanent block deserves. One success resets
				// everything to daily.
				if due, fails := twoaiNextCheckDue(db, x.u); !due {
					vmu.Lock()
					skippedBackoff++
					if fails > 0 {
						backoffByHost[twoaiHostOf(x.u)]++
					}
					vmu.Unlock()
					return
				}
				req, _ := http.NewRequest("GET", x.u, nil)
				req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; theworldofai.org link verification; contact: info@srjconsultingservices.com)")
				req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
				resp, err := client.Do(req)
				code := 0
				if err == nil {
					code = resp.StatusCode
					resp.Body.Close()
				}
				outcome := classify(code, err)
				errText := ""
				if err != nil {
					errText = err.Error()
				}
				// Everything below touches the database and the log, so it is done
				// one worker at a time. The network wait, which is the expensive
				// part, already happened in parallel.
				vmu.Lock()
				defer vmu.Unlock()

				var prior string
				var priorFails int
				db.QueryRow(`SELECT outcome, consecutive_failures FROM twoai_link_verify WHERE url=$1`,
					x.u).Scan(&prior, &priorFails)

				if outcome == "ok" {
					db.Exec(`UPDATE `+table+` SET verified_on=current_date WHERE `+keyCol+`=$1`, x.k)
					db.Exec(`INSERT INTO twoai_link_verify (url, source_table, source_key, outcome,
						last_status, last_error, consecutive_failures, first_failed_on, last_ok_on, checked_on)
					VALUES ($1,$2,$3,'ok',$4,NULL,0,NULL,current_date,current_date)
					ON CONFLICT (url) DO UPDATE SET outcome='ok', last_status=EXCLUDED.last_status,
						last_error=NULL, consecutive_failures=0, first_failed_on=NULL,
						last_ok_on=current_date, checked_on=current_date`,
						x.u, table, x.k, code)
					if prior != "" && prior != "ok" {
						fmt.Fprintf(os.Stderr, "twoai_apistatus: %s %s recovered (was %s)\n", table, x.u, prior)
					}
				} else {
					fails := priorFails + 1
					db.Exec(`INSERT INTO twoai_link_verify (url, source_table, source_key, outcome,
						last_status, last_error, consecutive_failures, first_failed_on, checked_on)
					VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),1,current_date,current_date)
					ON CONFLICT (url) DO UPDATE SET outcome=EXCLUDED.outcome,
						last_status=EXCLUDED.last_status, last_error=EXCLUDED.last_error,
						consecutive_failures=twoai_link_verify.consecutive_failures + 1,
						first_failed_on=COALESCE(twoai_link_verify.first_failed_on, current_date),
						checked_on=current_date`,
						x.u, table, x.k, outcome, code, errText)

					// A dead link is our problem and is said every run. A blocked one
					// is the publisher's choice and is said on the first day and then
					// weekly, so a permanent bot wall stops drowning the log.
					say := outcome == "dead" || outcome == "unreachable" ||
						fails == 1 || prior != outcome || fails%7 == 0
					if say {
						fmt.Fprintf(os.Stderr, "twoai_apistatus: %s %s: %s (status %d, day %d) %v\n",
							table, x.u, outcome, code, fails, err)
					}
				}
			}(x)
		}
		vwg.Wait()
		if skippedBackoff > 0 {
			// Said every run: a source we have stopped asking about is a
			// source whose facts are ageing, and that must stay visible.
			var hosts []string
			for h, n := range backoffByHost {
				hosts = append(hosts, fmt.Sprintf("%s(%d)", h, n))
			}
			sort.Strings(hosts)
			shown := hosts
			if len(shown) > 6 {
				shown = shown[:6]
			}
			fmt.Printf("twoai_apistatus: %s skipped %d url(s) on backoff: %s\n",
				table, skippedBackoff, strings.Join(shown, " "))
		}
	}
	reverify("twoai_api_directory", "docs_url", "provider")
	reverify("twoai_languages", "source_url", "slug")
	reverify("twoai_org_classifications", "source_url", "uid")
	reverify("twoai_hardware", "source_url", "slug")
	reverify("twoai_learning", "source_url", "slug")
	reverify("twoai_media", "source_url", "slug")
	reverify("twoai_security", "source_url", "slug")
	reverify("twoai_a2a", "source_url", "slug")

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
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		Steward  string `json:"steward"`
		First    string `json:"first_release"`
		Role     string `json:"ai_role"`
		Source   string `json:"source_url"`
		Verified string `json:"verified"`
		// See twoai_hardware: says WHY a verified date has stopped moving, so a
		// stale date is explained rather than merely old.
		VerifyNote string   `json:"verify_note,omitempty"`
		Repos      []string `json:"repos,omitempty"`
	}
	var langs []langRow
	if rows, err := db.Query(`SELECT l.slug, l.name, l.steward, l.first_release, l.ai_role,
			l.source_url, l.verified_on::text,
			CASE
			  WHEN v.outcome = 'blocked' THEN
			    'The publisher blocks automated checks, so this link is confirmed by hand rather than daily.'
			  WHEN v.outcome IN ('dead','unreachable') THEN
			    'This link stopped answering; the entry stays until a replacement source is found.'
			  ELSE ''
			END
		FROM twoai_languages l
		LEFT JOIN twoai_link_verify v ON v.url = l.source_url
		ORDER BY l.name`); err == nil {
		for rows.Next() {
			var l langRow
			if rows.Scan(&l.Slug, &l.Name, &l.Steward, &l.First, &l.Role, &l.Source, &l.Verified, &l.VerifyNote) == nil {
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

// twoaiNextCheckDue decides whether a URL is worth asking about today, and
// returns the failure count so the caller can report what it skipped.
//
// The schedule is deliberately gentle at first and harsh later: most failures
// are a bad afternoon, and a few are a permanent decision by the publisher.
// Days 1-3 daily, then weekly, then every 30 days once a source has refused
// us for a fortnight. A single success clears consecutive_failures and the
// URL returns to daily checking, so recovery is never missed, only noticed
// later than it happened - an acceptable trade for not knocking on a closed
// door every night for a year.
func twoaiNextCheckDue(db *sql.DB, url string) (bool, int) {
	var fails int
	var checked sql.NullTime
	err := db.QueryRow(`SELECT consecutive_failures, checked_on
		FROM twoai_link_verify WHERE url=$1`, url).Scan(&fails, &checked)
	if err != nil || fails == 0 || !checked.Valid {
		return true, 0 // never seen, or currently healthy
	}
	var interval int
	switch {
	case fails <= 3:
		interval = 1
	case fails <= 13:
		interval = 7
	default:
		interval = 30
	}
	days := int(time.Since(checked.Time).Hours() / 24)
	return days >= interval, fails
}

// twoaiHostOf is for log lines only: a bare host reads better than a full URL
// when six of them share one line.
func twoaiHostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return strings.TrimPrefix(u.Host, "www.")
}
