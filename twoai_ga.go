package main

// twoai_ga_top: the site's most-visited pages, from the GA4 Data API, refreshed
// daily. Feeds the footer's "Most visited" list so the panel reflects what
// readers actually open rather than a hand-picked guess that goes stale.
//
// WINDOW. The query covers the trailing 7 days ending yesterday, refreshed
// every run. A single day on a young site is a handful of pageviews of noise;
// a week is stable enough to mean something and still moves daily. Override
// with GA_TOP_WINDOW (GA4 date token, e.g. "yesterday", "30daysAgo").
//
// SETUP (owner actions, one time):
//   1. GA4 Admin -> Property Settings -> copy the NUMERIC property id, set it
//      on the cron as GA4_PROPERTY_ID.
//   2. GA4 Admin -> Property Access Management -> add the service-account
//      email (GOOGLE_SA_EMAIL) as Viewer. No domain-wide delegation involved:
//      the SA acts as itself, which is why the JWT here carries no sub claim,
//      unlike email_route's Gmail token.
//   3. GOOGLE_SA_EMAIL and GOOGLE_SA_KEY (PEM) on the cron, shared with
//      email_route.
//
// FAILS OPEN. Missing env, an API refusal, or a malformed response logs one
// line and returns nil: the footer keeps rendering the last stored day (or its
// static fallback), and the pipeline run continues. Popularity data is never
// worth failing a publish over.
//
// NEVER DELETE. Each run appends today's ranking; history accumulates in
// twoai_ga_top_pages keyed (day, rank), so "what was popular in August" stays
// answerable forever.

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// gaSAToken is erSAToken without the delegation sub claim: Analytics grants
// access to the service account itself via Property Access Management.
func gaSAToken(scope string) (string, error) {
	email, key := twoaiEnv("GOOGLE_SA_EMAIL"), twoaiEnv("GOOGLE_SA_KEY")
	if key == "" {
		return "", fmt.Errorf("GOOGLE_SA_KEY must be set")
	}
	// A PASTED SERVICE-ACCOUNT JSON FILE NAMES ITS OWN ACCOUNT, so when the
	// key is JSON, client_email from inside it is authoritative and
	// GOOGLE_SA_EMAIL is ignored. The 2026-08-20/21 failures were a valid key
	// signed under a different account's GOOGLE_SA_EMAIL; Google reports that
	// mismatch as "Invalid JWT Signature", which reads like a broken key and
	// is not. Two env vars that must agree will eventually disagree - reading
	// the pair from one file removes the class of failure, not one instance.
	if strings.Contains(key, "client_email") {
		var sa struct {
			ClientEmail string `json:"client_email"`
			PrivateKey  string `json:"private_key"`
		}
		if json.Unmarshal([]byte(key), &sa) == nil && sa.ClientEmail != "" && sa.PrivateKey != "" {
			email, key = sa.ClientEmail, sa.PrivateKey
		}
	}
	if email == "" {
		return "", fmt.Errorf("no service-account email: set GOOGLE_SA_EMAIL or paste the full JSON key")
	}
	block, _ := pem.Decode([]byte(strings.ReplaceAll(key, `\n`, "\n")))
	if block == nil {
		return "", fmt.Errorf("GOOGLE_SA_KEY is not valid PEM")
	}
	var priv *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		priv = k.(*rsa.PrivateKey)
	} else if k2, err2 := x509.ParsePKCS1PrivateKey(block.Bytes); err2 == nil {
		priv = k2
	} else {
		return "", fmt.Errorf("cannot parse service-account key: %v", err)
	}
	now := time.Now().Unix()
	header := erB64([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iss": email, "aud": erTokenURL,
		"scope": scope, "iat": now, "exp": now + 3600,
	})
	signing := header + "." + erB64(claims)
	h := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	resp, err := http.PostForm(erTokenURL, url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signing + "." + erB64(sig)},
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("token: %d %s", resp.StatusCode, string(body[:min(len(body), 200)]))
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &t) != nil || t.AccessToken == "" {
		return "", fmt.Errorf("token response unusable")
	}
	return t.AccessToken, nil
}

// twoaiEnv reads an environment variable and strips a matching pair of
// surrounding quotes.
//
// pipeline.env is read by run-pipeline.ps1 with a regex that captures
// everything after the first '=' verbatim, quotes included. A value written
// as KEY='value' therefore arrives with the apostrophes attached, and the
// consumer sees a string that is almost right, which is the worst kind of
// wrong. On 2026-09-14 three variables were quoted and all three failed
// silently for different-looking reasons:
//
//   GOOGLE_SA_KEY         -> "GOOGLE_SA_KEY is not valid PEM", because PEM
//                            must begin with five dashes and this began with
//                            an apostrophe. The Most Visited panel had been
//                            nine days stale.
//   TWOAI_ALERT_SMTP_PASS -> Gmail 535 BadCredentials on every run this week.
//                            Bill alerts queued and never delivered.
//   USAJOBS_API_KEY       -> 401 on every USAJOBS query.
//
// Three unrelated-looking failures, one cause. Quoting a value in an env
// file is a completely reasonable thing for a person to do, so the code
// tolerates it rather than the person having to remember.
func twoaiEnv(name string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if len(v) >= 2 {
		if (v[0] == '\'' && v[len(v)-1] == '\'') || (v[0] == '"' && v[len(v)-1] == '"') {
			v = v[1 : len(v)-1]
		}
	}
	return v
}

func twoaiGATop(db *sql.DB) error {
	prop := twoaiEnv("GA4_PROPERTY_ID")
	if prop == "" {
		fmt.Println("twoai_ga_top: skipped, GA4_PROPERTY_ID not set (see twoai_ga.go SETUP)")
		return nil
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_ga_top_pages (
		day date NOT NULL,
		rank int NOT NULL,
		path text NOT NULL,
		views int NOT NULL,
		title text,
		window_days int NOT NULL DEFAULT 7,
		fetched_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (day, rank))`); err != nil {
		return err
	}

	token, err := gaSAToken("https://www.googleapis.com/auth/analytics.readonly")
	if err != nil {
		fmt.Printf("twoai_ga_top: skipped, %v\n", err)
		return nil
	}

	start := os.Getenv("GA_TOP_WINDOW")
	if start == "" {
		start = "7daysAgo"
	}
	reqBody, _ := json.Marshal(map[string]any{
		"dateRanges": []map[string]string{{"startDate": start, "endDate": "yesterday"}},
		"dimensions": []map[string]string{{"name": "pagePath"}},
		"metrics":    []map[string]string{{"name": "screenPageViews"}},
		"limit":      50,
	})
	req, _ := http.NewRequest("POST",
		"https://analyticsdata.googleapis.com/v1beta/properties/"+prop+":runReport",
		bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("twoai_ga_top: skipped, GA4 unreachable: %v\n", err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Printf("twoai_ga_top: skipped, GA4 %d %s\n", resp.StatusCode, string(body[:min(len(body), 200)]))
		return nil
	}
	var rr struct {
		Rows []struct {
			DimensionValues []struct{ Value string }
			MetricValues    []struct{ Value string }
		}
	}
	if err := json.Unmarshal(body, &rr); err != nil {
		fmt.Printf("twoai_ga_top: skipped, response unusable: %v\n", err)
		return nil
	}

	// Keep reference pages a reader would want surfaced. Utility paths, the
	// home page (always #1, tells the reader nothing), query strings, and
	// anything not ending in a slash (assets) are noise here.
	skip := func(p string) bool {
		if p == "/" || strings.Contains(p, "?") || !strings.HasSuffix(p, "/") {
			return true
		}
		for _, pre := range []string{"/api/", "/404", "/search", "/contact", "/about", "/privacy", "/terms", "/disclosure", "/disclaimer"} {
			if strings.HasPrefix(p, pre) {
				return true
			}
		}
		return false
	}

	type row struct {
		path  string
		views int
	}
	var top []row
	for _, r := range rr.Rows {
		if len(r.DimensionValues) == 0 || len(r.MetricValues) == 0 {
			continue
		}
		p := r.DimensionValues[0].Value
		if skip(p) {
			continue
		}
		var v int
		fmt.Sscanf(r.MetricValues[0].Value, "%d", &v)
		if v <= 0 {
			continue
		}
		top = append(top, row{p, v})
		if len(top) == 5 {
			break
		}
	}
	if len(top) == 0 {
		fmt.Println("twoai_ga_top: 0 qualifying pages in the window, keeping prior data")
		return nil
	}

	// Titles come from twoai_pages, which is where they are.
	//
	// This used to read twoai_url_registry.title. That column exists and is
	// empty on all 12,907 rows - the registry is built from the sitemap, which
	// carries URLs and not titles, so the lookup returned null every time and
	// the footer fell back to deriving a name from the path. That is why the
	// list read "995676ef" and "B441a27b" instead of company names: a UID is
	// all a path has when the page is an ecosystem entry. Stephen caught it on
	// 2026-09-14.
	//
	// A page document knows its own title. The lookup maps the public path
	// back to the document, tries the two shapes the site uses, and leaves the
	// title null if neither matches, which keeps the old fallback for anything
	// genuinely unknown.
	for i, r := range top {
		var title sql.NullString
		db.QueryRow(`SELECT COALESCE(NULLIF(p.data->>'title',''), NULLIF(p.data->'company'->>'name',''),
		                    NULLIF(p.data->>'name',''), NULLIF(p.data->>'headline',''))
			FROM twoai_pages p
			WHERE p.data->>'live_path' = $1 OR p.data->>'path' = $1
			   OR ('/' || regexp_replace(p.path, '\.json$', '') || '/') = $1
			LIMIT 1`, r.path).Scan(&title)
		if !title.Valid || strings.TrimSpace(title.String) == "" {
			// Second chance: the taxonomy knows the live path of every
			// ecosystem page, which is exactly the shape that produced a bare
			// UID in the footer.
			db.QueryRow(`SELECT name FROM twoai_taxonomy WHERE live_path = $1 LIMIT 1`, r.path).Scan(&title)
		}
		if !title.Valid || strings.TrimSpace(title.String) == "" {
			// Third: lawsuits are 108 cases inside ONE document, so no page row
			// has their path. Matched on the slug in the URL.
			if s := strings.Trim(strings.TrimPrefix(r.path, "/ai-lawsuits/"), "/"); s != "" && strings.HasPrefix(r.path, "/ai-lawsuits/") {
				db.QueryRow(`SELECT COALESCE(NULLIF(c->>'short_name',''), c->>'case_name')
					FROM twoai_pages p, jsonb_array_elements(p.data->'cases') c
					WHERE p.kind='lawsuits' AND c->>'slug' = $1 LIMIT 1`, s).Scan(&title)
			}
		}
		if _, err := db.Exec(`INSERT INTO twoai_ga_top_pages (day, rank, path, views, title)
			VALUES (current_date, $1, $2, $3, $4)
			ON CONFLICT (day, rank) DO UPDATE SET path=EXCLUDED.path,
				views=EXCLUDED.views, title=EXCLUDED.title, fetched_at=now()`,
			i+1, r.path, r.views, title); err != nil {
			return err
		}
	}
	fmt.Printf("twoai_ga_top: stored top %d for %s window\n", len(top), start)
	return nil
}
