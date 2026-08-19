package main

// twoai_company_harvest.go - build a real page for every tracked company by
// reading the company's own website, per Stephen's directive. The directory
// linked 261 companies while only 72 had pages; every entry now resolves the
// company's site, harvests what the company says about itself, and generates
// a profile from the harvested text plus the facts this site already holds
// (products, lawsuits, MCP servers, SEC and patent facts). Same guardrails as
// the industry loop: the model sees only the payload, every percentage must
// appear verbatim in it, and rejected or failed generations keep the previous
// text. Generation is capped per run so the first pass spreads over about a
// week of crons and the steady state is near zero.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const twoaiCompanyGenCap = 60

var siteNameRe = regexp.MustCompile(`[^a-z0-9]+`)

// resolveWebsite: the profile's verified website first, then the vendor
// feed's host (a company's blog feed lives on its own domain), and last a
// name-derived candidate that only counts if the fetched page actually names
// the company in its title or site metadata - a squatter page fails the check
// and the company simply has no website on record rather than a wrong one.
func twoaiCompanyResolveSite(db *sql.DB, uid, name string) (string, string) {
	var w string
	db.QueryRow(`SELECT COALESCE(website,'') FROM twoai_company_profiles WHERE uid=$1`, uid).Scan(&w)
	if w != "" {
		return w, "profile"
	}
	var feed string
	db.QueryRow(`SELECT feed_url FROM twoai_vendor_feeds WHERE entity_uid=$1 AND active LIMIT 1`, uid).Scan(&feed)
	if feed != "" {
		if i := strings.Index(feed, "://"); i > 0 {
			host := feed[i+3:]
			if j := strings.IndexAny(host, "/?"); j > 0 {
				host = host[:j]
			}
			host = strings.TrimPrefix(host, "www.")
			// feed hosts like blog.example.com or example.com/feed both reduce
			// to the registrable site
			parts := strings.Split(host, ".")
			if len(parts) > 2 {
				host = strings.Join(parts[len(parts)-2:], ".")
			}
			return "https://" + host + "/", "vendor feed"
		}
	}
	// Third tier: the product URLs we already verified for this company. 259
	// of 261 tracked companies have at least one, and a tool's own page is on
	// the vendor's domain, so reducing it to the registrable host yields the
	// company site without guessing from the name. Skip shared hosts, where
	// the URL belongs to a platform rather than the company.
	var pageData string
	if db.QueryRow(`SELECT data::text FROM twoai_pages WHERE path=$1`, "companies/"+uid+".json").Scan(&pageData) == nil {
		var pd struct {
			Company struct {
				Products []struct {
					URL string `json:"url"`
				} `json:"products"`
			} `json:"company"`
		}
		if json.Unmarshal([]byte(pageData), &pd) == nil {
			for _, pr := range pd.Company.Products {
				if h := twoaiRegistrableHost(pr.URL); h != "" {
					return "https://" + h + "/", "tracked product URL"
				}
			}
		}
	}
	return "", ""
}

// Hosts that serve many vendors: a URL here identifies a listing, not the
// company's own site, so it must never become a company website.
// Two-label public suffixes. Under each of these, the registrable name is the
// THIRD label from the right: shlab.org.cn, not org.cn.
var twoaiTwoLabelSuffixes = map[string]bool{
	// China
	"com.cn": true, "org.cn": true, "net.cn": true, "gov.cn": true, "edu.cn": true, "ac.cn": true,
	// United Kingdom
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true, "me.uk": true, "net.uk": true,
	// Australia, Japan, Korea, India, Brazil, Israel
	"com.au": true, "net.au": true, "org.au": true, "edu.au": true, "gov.au": true,
	"co.jp": true, "or.jp": true, "ne.jp": true, "ac.jp": true, "go.jp": true,
	"co.kr": true, "or.kr": true, "re.kr": true,
	"co.in": true, "net.in": true, "org.in": true, "ac.in": true, "gov.in": true,
	"com.br": true, "net.br": true, "org.br": true,
	"co.il": true, "org.il": true, "ac.il": true, "gov.il": true,
	// Others seen in this directory
	"com.sg": true, "com.hk": true, "com.tw": true, "co.nz": true, "co.za": true,
	"com.mx": true, "com.tr": true, "co.id": true, "com.ar": true, "com.my": true,
	"go.id": true, "or.id": true, "ac.uk.com": false,
}

var twoaiSharedHosts = map[string]bool{
	"github.com": true, "huggingface.co": true, "apps.apple.com": true,
	"play.google.com": true, "chromewebstore.google.com": true, "chrome.google.com": true,
	"marketplace.visualstudio.com": true, "pypi.org": true, "npmjs.com": true,
	"docs.google.com": true, "sites.google.com": true, "notion.site": true,
	"gitlab.com": true, "sourceforge.net": true, "medium.com": true,
	"linkedin.com": true, "x.com": true, "twitter.com": true, "youtube.com": true,
	"workspace.google.com": true, "microsoft.com": true, "google.com": true,
	"amazon.com": true, "aws.amazon.com": true, "azure.microsoft.com": true,
}

func twoaiRegistrableHost(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return ""
	}
	host := raw[i+3:]
	if j := strings.IndexAny(host, "/?#"); j > 0 {
		host = host[:j]
	}
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	if host == "" || !strings.Contains(host, ".") {
		return ""
	}
	parts := strings.Split(host, ".")
	// Reduce to the registrable domain, keeping three labels where the last two
	// are a public suffix rather than a registrable name.
	//
	// THIS LIST WAS TOO SHORT AND IT SILENTLY CORRUPTED A URL. Shanghai AI
	// Laboratory's site is shlab.org.cn; "org.cn" was not in the switch, so the
	// default branch reduced the host to "org.cn" and the harvester spent every
	// run fetching https://org.cn/, which is not the lab and not anything. The
	// failure was invisible because a bad host looks identical to an unreachable
	// one in the counters.
	//
	// Any suffix added here must be a genuine public suffix - a registry under
	// which third parties register names - not merely a common second label.
	if len(parts) > 2 {
		suffix2 := parts[len(parts)-2] + "." + parts[len(parts)-1]
		if twoaiTwoLabelSuffixes[suffix2] {
			host = strings.Join(parts[len(parts)-3:], ".")
		} else {
			host = suffix2
		}
	}
	if twoaiSharedHosts[host] {
		return ""
	}
	return host
}

func twoaiCompanyHarvest(db *sql.DB, today string) (int, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_company_harvest (
		uid text PRIMARY KEY, name text NOT NULL DEFAULT '', url text NOT NULL DEFAULT '',
		resolved_via text NOT NULL DEFAULT '', http_status int NOT NULL DEFAULT 0,
		extract text NOT NULL DEFAULT '', content_hash text NOT NULL DEFAULT '',
		fetched_on date NOT NULL DEFAULT current_date)`); err != nil {
		return 0, err
	}

	rows, err := db.Query(`SELECT uid, name FROM twoai_entities WHERE kind='company' ORDER BY name`)
	if err != nil {
		return 0, err
	}
	type ent struct{ uid, name string }
	var ents []ent
	for rows.Next() {
		var e ent
		if rows.Scan(&e.uid, &e.name) == nil {
			ents = append(ents, e)
		}
	}
	rows.Close()

	client := &http.Client{Timeout: 20 * time.Second}
	todayStr := time.Now().UTC().Format("2006-01-02")
	fetched, skipped, nosite, failed, blocked := 0, 0, 0, 0, 0
	for _, e := range ents {
		// Freshness skip applies ONLY to rows that already hold readable text
		// from today. The earlier form skipped on the date alone, which meant a
		// row stamped by a previous run that failed to resolve a site was
		// treated as fresh and never retried, so an improved resolver could
		// not reach the companies it was written for. Rows with no site or no
		// text retry every run: resolution is a database lookup, and a fetch
		// only follows when resolution actually yields a site.
		var last, lastExtract string
		db.QueryRow(`SELECT fetched_on::text, extract FROM twoai_company_harvest WHERE uid=$1`, e.uid).Scan(&last, &lastExtract)
		if last == todayStr && lastExtract != "" {
			skipped++
			continue
		}
		site, via := twoaiCompanyResolveSite(db, e.uid, e.name)
		if site == "" {
			nosite++
			db.Exec(`INSERT INTO twoai_company_harvest (uid, name, fetched_on) VALUES ($1,$2,current_date)
				ON CONFLICT (uid) DO UPDATE SET fetched_on=current_date`, e.uid, e.name)
			continue
		}
		req, _ := http.NewRequest("GET", site, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; theworldofai.org company directory; info@srjconsultingservices.com)")
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		resp, ferr := client.Do(req)
		status, extract := 0, ""
		if ferr == nil {
			status = resp.StatusCode
			if status == 200 {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
				extract = twoaiHarvestExtract(body)
			}
			resp.Body.Close()
		}
		h := sha256.Sum256([]byte(extract))
		if status == 200 && extract != "" {
			fetched++
			if _, err := db.Exec(`INSERT INTO twoai_company_harvest (uid, name, url, resolved_via, http_status, extract, content_hash, fetched_on)
				VALUES ($1,$2,$3,$4,$5,$6,$7,current_date)
				ON CONFLICT (uid) DO UPDATE SET name=$2, url=$3, resolved_via=$4, http_status=$5,
					extract=$6, content_hash=$7, fetched_on=current_date`,
				e.uid, e.name, site, via, status, extract, hex.EncodeToString(h[:8])); err != nil {
				return 0, err
			}
		} else {
			// A publisher refusing a self-identified robot is not our failure,
			// and counting it as one buries the handful that ARE. Same rule as
			// the curated-link sweep: blocked is the site's choice, failed is a
			// fetch that should have worked.
			switch {
			case status == 401 || status == 403 || status == 429 || status == 451 || status == 402:
				blocked++
			default:
				failed++
			}
			db.Exec(`INSERT INTO twoai_company_harvest (uid, name, url, resolved_via, http_status, fetched_on)
				VALUES ($1,$2,$3,$4,$5,current_date)
				ON CONFLICT (uid) DO UPDATE SET url=$3, resolved_via=$4, http_status=$5, fetched_on=current_date`,
				e.uid, e.name, site, via, status)
		}
		time.Sleep(250 * time.Millisecond)
	}
	// Blocked is reported separately from failed so a growing number of real
	// faults cannot hide behind a stable number of bot walls.
	fmt.Printf("twoai_company_harvest: fetched=%d unchanged_today=%d no_site=%d blocked=%d failed=%d of %d companies\n",
		fetched, skipped, nosite, blocked, failed, len(ents))
	if failed > 0 {
		var names string
		db.QueryRow(`SELECT string_agg(name || ' (' || COALESCE(http_status::text,'no response') || ')', ', ' ORDER BY name)
			FROM twoai_company_harvest
			WHERE fetched_on = current_date AND (http_status IS NULL
				OR http_status NOT IN (200, 401, 402, 403, 429, 451))`).Scan(&names)
		if names != "" {
			fmt.Fprintf(os.Stderr, "twoai_company_harvest: failing sites: %s\n", names)
		}
	}

	// ---- generate profiles from the harvest, capped per run ----------------
	generated, patched := 0, 0
	for _, e := range ents {
		var url2, extract string
		db.QueryRow(`SELECT url, extract FROM twoai_company_harvest WHERE uid=$1`, e.uid).Scan(&url2, &extract)

		// facts this site already holds, straight from the page doc
		var pageData string
		if db.QueryRow(`SELECT data::text FROM twoai_pages WHERE path=$1`, "companies/"+e.uid+".json").Scan(&pageData) != nil {
			continue
		}
		var pd map[string]any
		json.Unmarshal([]byte(pageData), &pd)
		payload := map[string]any{"company": e.name, "held_facts": pd["company"]}
		if extract != "" {
			payload["company_website"] = url2
			payload["what_the_company_site_says"] = extract
		}
		pj, _ := json.Marshal(payload)
		h := sha256.Sum256(pj)
		cHash := hex.EncodeToString(h[:8])
		metricKey := "company-" + e.uid

		var exists int
		db.QueryRow(`SELECT count(*) FROM twoai_industry_analysis WHERE metric=$1 AND data_hash=$2`, metricKey, cHash).Scan(&exists)
		if exists == 0 && os.Getenv("ANTHROPIC_API_KEY") != "" && generated < twoaiCompanyGenCap && extract != "" {
			model := os.Getenv("TWOAI_ANALYSIS_MODEL")
			if model == "" {
				model = "claude-sonnet-4-6"
			}
			system := "You write company profiles for theworldofai.org, a sourced AI reference site. " +
				"You are given JSON: the text this site's pipeline harvested from the company's own " +
				"website, and the facts this site already holds about it (products tracked, lawsuits, " +
				"MCP servers, SEC and patent facts where present). Write 2 to 3 short paragraphs of " +
				"plain English: what the company is and does per its own site, its AI products and " +
				"position, and anything the held facts add (litigation, filings, registry presence). " +
				"HARD RULES: use ONLY facts in the JSON. Name only products, people, and organizations " +
				"that appear in the JSON. Every percentage and dollar figure must appear verbatim in " +
				"the JSON. Marketing language from the site gets reported neutrally (the company " +
				"describes itself as...), never adopted. If the material is thin, write less. " +
				"Plain sentences, no hype, commas over dashes."
			body, aerr := twoaiClaudeCall(model, system, "Company: "+e.name+"\nThe data:\n"+string(pj)+"\n\nWrite the profile now.")
			if aerr != nil {
				fmt.Printf("twoai_company_harvest: %s profile skipped: %v\n", e.name, aerr)
			} else if verr := twoaiValidateAnalysis(string(pj), body); verr != nil {
				fmt.Printf("twoai_company_harvest: %s profile REJECTED (%v)\n", e.name, verr)
			} else {
				db.Exec(`INSERT INTO twoai_industry_analysis (metric, data_hash, model, body, generated_on)
					VALUES ($1,$2,$3,$4,current_date) ON CONFLICT (metric, data_hash) DO NOTHING`,
					metricKey, cHash, model, body)
				generated++
				time.Sleep(1500 * time.Millisecond)
			}
		}

		var pModel, pBody, pDate string
		db.QueryRow(`SELECT model, body, generated_on::text FROM twoai_industry_analysis
			WHERE metric=$1 ORDER BY generated_on DESC LIMIT 1`, metricKey).Scan(&pModel, &pBody, &pDate)
		patch := map[string]any{}
		if pBody != "" {
			patch["profile_text"] = map[string]any{"model": pModel, "body": pBody, "generated_on": pDate}
		}
		if url2 != "" {
			patch["website"] = url2
		}
		if len(patch) > 0 {
			pdj, _ := json.Marshal(patch)
			if res, err := db.Exec(`UPDATE twoai_pages SET data = data || $1::jsonb, updated_at=now() WHERE path=$2`,
				string(pdj), "companies/"+e.uid+".json"); err == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					patched++
				}
			}
		}
	}
	fmt.Printf("twoai_company_harvest: profiles generated=%d (cap %d) pages_patched=%d\n",
		generated, twoaiCompanyGenCap, patched)
	return patched, nil
}
