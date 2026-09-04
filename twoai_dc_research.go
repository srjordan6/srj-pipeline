package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// EVERY FACILITY, EVERY PUBLISHED FIGURE, FOUND BY THE PIPELINE ITSELF.
//
// Stephen, 2026-09-04: I want everything, find a way to get it. About 230 of
// 1,635 facilities held a capacity figure and about 100 a density, because
// the figures live on 1,600 different operator sheets, county records and
// filings, and a human or Cowork reads them one at a time.
//
// This stage does what a researcher does, per facility, without one: it
// asks Claude with web search for the facility's published figures, asks
// for the URL and the exact phrase each figure came from, then FETCHES THAT
// URL ITSELF and checks the figure is on the page. A figure that the cited
// page does not contain is discarded, whatever the model said. A figure it
// does contain is stored with the page as its source. The model proposes;
// the page decides. That is the only way a search-driven pass stays inside
// the no-fabrication rule.
//
// Aggregator directories are excluded from citation. If the only place a
// figure appears is a listing that itself cites nothing, it is a lead, not
// a fact, and the stage does not store it.
//
// Cost: one Haiku call with web search per facility, about five cents.
// Capped per run by TWOAI_DC_RESEARCH_BUDGET (default 40, so roughly two
// dollars a run, twelve dollars a day across the every-3-hours schedule,
// and every facility reached within a fortnight). A facility is tried once
// per 30 days; the attempt is recorded either way.

var twoaiResearchAggregators = []string{
	"datacenters.com", "datacentermap.com", "datacenterhawk.com", "baxtel.com", "godatacenters.com",
	"ocolo.io", "inflect.com", "dchub.cloud", "cloudscene.com", "colocationamerica.com",
	"datacenterknowledge.com/directory", "wikipedia.org",
}

type twoaiResearchFact struct {
	Field string `json:"field"`
	Value string `json:"value"`
	Unit  string `json:"unit"`
	URL   string `json:"url"`
	Quote string `json:"quote"`
}

func twoaiDcResearch(db *sql.DB) error {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil
	}
	budget := 40
	if v, err := strconv.Atoi(os.Getenv("TWOAI_DC_RESEARCH_BUDGET")); err == nil && v >= 0 {
		budget = v
	}
	if budget == 0 {
		return nil
	}
	db.Exec(`ALTER TABLE twoai_dc_facilities ADD COLUMN IF NOT EXISTS research_tried_at timestamptz`)

	// Facilities with an operator and a locality, missing capacity or
	// density, not tried in 30 days. Named facilities first: a search for
	// "Equinix LA7" finds a sheet; a search for "Amazon Web Services,
	// Madison County" finds a county record at best.
	rows, err := db.Query(`SELECT id, name, COALESCE(operator,''), COALESCE(city,''), COALESCE(state,''),
			COALESCE(profile->>'address',''), COALESCE(profile->>'facility_codes','')
		FROM twoai_dc_facilities
		WHERE COALESCE(operator,'') <> '' AND (country='US' OR country IS NULL)
		  AND NOT (profile ? 'it_capacity_mw' AND profile ? 'power_density_kw_rack')
		  AND (research_tried_at IS NULL OR research_tried_at < now() - interval '30 days')
		  AND COALESCE(profile->>'unresolvable','') <> 'true'
		ORDER BY (name <> operator) DESC, (profile ? 'facility_codes') DESC, id
		LIMIT $1`, budget)
	if err != nil {
		return err
	}
	type fac struct{ id, name, op, city, st, addr, codes string }
	var facs []fac
	for rows.Next() {
		var f fac
		if rows.Scan(&f.id, &f.name, &f.op, &f.city, &f.st, &f.addr, &f.codes) == nil {
			facs = append(facs, f)
		}
	}
	rows.Close()

	stored, tried, discarded := 0, 0, 0
	client := &http.Client{Timeout: 120 * time.Second}
	for _, f := range facs {
		tried++
		db.Exec(`UPDATE twoai_dc_facilities SET research_tried_at = now() WHERE id = $1`, f.id)
		facts := twoaiResearchAsk(client, key, f.name, f.op, f.city, f.st, f.addr, f.codes)
		if len(facts) == 0 {
			continue
		}
		patch := map[string]any{}
		var sources []map[string]string
		seenSrc := map[string]bool{}
		for _, fa := range facts {
			if !twoaiResearchVerify(fa) {
				discarded++
				continue
			}
			k, v := twoaiResearchNormalize(fa)
			if k == "" {
				continue
			}
			patch[k] = v
			if !seenSrc[fa.URL] {
				seenSrc[fa.URL] = true
				sources = append(sources, map[string]string{
					"title": "Published figures for " + f.name, "publisher": publisherFromURL(fa.URL),
					"date": time.Now().UTC().Format("2006-01-02"), "url": fa.URL,
				})
			}
		}
		if len(patch) == 0 {
			continue
		}
		patch["research_pass"] = map[string]any{"on": time.Now().UTC().Format("2006-01-02"), "method": "search, then verified against the cited page", "fields": len(patch)}
		pj, _ := json.Marshal(patch)
		sj, _ := json.Marshal(sources)
		db.Exec(`UPDATE twoai_dc_facilities
			SET profile = profile || $2::jsonb
			              || jsonb_build_object('sources', COALESCE(profile->'sources','[]'::jsonb) || $3::jsonb),
			    status = CASE WHEN status IN ('draft','') THEN 'enriched' ELSE status END
			WHERE id = $1`, f.id, string(pj), string(sj))
		stored++
		time.Sleep(1200 * time.Millisecond)
	}
	fmt.Printf("twoai_dc_research: tried=%d facilities_with_verified_facts=%d facts_discarded_unverified=%d budget=%d\n", tried, stored, discarded, budget)
	return nil
}

// twoaiResearchAsk runs one Haiku call with web search and returns the
// proposed facts. Anything not parseable is dropped.
func twoaiResearchAsk(client *http.Client, key, name, op, city, st, addr, codes string) []twoaiResearchFact {
	q := fmt.Sprintf("%s (operator %s) data center, %s %s %s %s", name, op, addr, city, st, codes)
	prompt := "Find the PUBLISHED specifications for this data center facility: " + q + ".\n" +
		"Search the operator's own site first (spec sheets, campus pages), then SEC filings, county records, utility or regulatory filings, and local news. " +
		"Do NOT rely on directory or listing sites (datacenters.com, datacentermap, datacenterhawk, baxtel, godatacenters, ocolo, inflect); they are not sources.\n" +
		"Return ONLY a JSON array, no prose. Each element: {\"field\": one of it_capacity_mw | planned_it_capacity_mw | power_density_kw_rack | technical_space_sqft | building_sqft | year_opened | phone | certifications, " +
		"\"value\": the figure as a plain number or string, \"unit\": unit or empty, \"url\": the exact page the figure appears on, \"quote\": the exact short phrase from that page containing the figure (under 20 words)}.\n" +
		"Only include a field if you found it on a page you can cite. If nothing is published, return []."
	body, _ := json.Marshal(map[string]any{
		"model":      "claude-haiku-4-5",
		"max_tokens": 1200,
		"tools":      []map[string]any{{"type": "web_search_20250305", "name": "web_search", "max_uses": 5}},
		"messages":   []map[string]any{{"role": "user", "content": prompt}},
	})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != 200 {
		fmt.Fprintln(os.Stderr, "twoai_dc_research: api", resp.StatusCode, string(raw[:min(len(raw), 200)]))
		return nil
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	var text string
	for _, c := range out.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	i, j := strings.Index(text, "["), strings.LastIndex(text, "]")
	if i < 0 || j <= i {
		return nil
	}
	var facts []twoaiResearchFact
	if json.Unmarshal([]byte(text[i:j+1]), &facts) != nil {
		return nil
	}
	return facts
}

// twoaiResearchVerify fetches the cited page and requires the figure to be
// on it. Aggregators are refused before the fetch.
func twoaiResearchVerify(f twoaiResearchFact) bool {
	u := strings.TrimSpace(f.URL)
	if !strings.HasPrefix(u, "http") || f.Value == "" {
		return false
	}
	lu := strings.ToLower(u)
	for _, a := range twoaiResearchAggregators {
		if strings.Contains(lu, a) {
			return false
		}
	}
	page := twoaiResearchFetch(u)
	if page == "" {
		return false
	}
	text := strings.ToLower(regexp.MustCompile(`\s+`).ReplaceAllString(page, " "))
	// The figure itself must be present, in any of the ways a page writes
	// a number: 74,981 / 74981 / 74.981 / 12.0 / 12.
	digits := regexp.MustCompile(`[^0-9.]`).ReplaceAllString(f.Value, "")
	if digits == "" {
		return strings.Contains(text, strings.ToLower(f.Value))
	}
	candidates := []string{digits, strings.TrimSuffix(digits, ".0")}
	if len(digits) > 3 && !strings.Contains(digits, ".") {
		var withCommas string
		for i, r := range digits {
			if i > 0 && (len(digits)-i)%3 == 0 {
				withCommas += ","
			}
			withCommas += string(r)
		}
		candidates = append(candidates, withCommas, strings.ReplaceAll(withCommas, ",", "."))
	}
	// THE NUMBER MUST APPEAR WITH ITS UNIT. A bare "15" verified against
	// Equinix's SV1 page in testing because the page links to SV15; a bare
	// "2000" would verify against any page with a year on it. So a capacity
	// figure must be followed within a few characters by MW or megawatt, an
	// area by sq ft / square feet / ft² / SF / m², a density by kW or kVA, and
	// a year must stand alone as a four-digit token. Phone and certification
	// values are matched as text.
	unit := map[string]string{
		"it_capacity_mw":         `\s*(mw|megawatt)`,
		"planned_it_capacity_mw": `\s*(mw|megawatt)`,
		"power_density_kw_rack":  `\s*(kw|kva|kilowatt)`,
		"technical_space_sqft":   `\s*(sq\.? ?ft|square feet|square foot|ft²|ft2|sf\b|m²|m2|square met)`,
		"building_sqft":          `\s*(sq\.? ?ft|square feet|square foot|ft²|ft2|sf\b|m²|m2|square met)`,
		"year_opened":            `(?:[^0-9]|$)`,
	}[f.Field]
	if unit == "" {
		unit = `([^0-9]|$)`
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if regexp.MustCompile(`(^|[^0-9.,])` + regexp.QuoteMeta(c) + unit).MatchString(text) {
			return true
		}
	}
	return false
}

func twoaiResearchNormalize(f twoaiResearchFact) (string, any) {
	num := func() (float64, bool) {
		v, err := strconv.ParseFloat(strings.ReplaceAll(regexp.MustCompile(`[^0-9.]`).ReplaceAllString(f.Value, ""), ",", ""), 64)
		return v, err == nil && v > 0
	}
	switch f.Field {
	case "it_capacity_mw", "planned_it_capacity_mw":
		if v, ok := num(); ok {
			if strings.Contains(strings.ToLower(f.Unit), "kw") {
				v = v / 1000
			}
			if v > 0 && v < 5000 {
				return f.Field, v
			}
		}
	case "power_density_kw_rack":
		if v, ok := num(); ok && v > 0 && v < 500 {
			return f.Field, v
		}
	case "technical_space_sqft", "building_sqft":
		if v, ok := num(); ok {
			if strings.Contains(strings.ToLower(f.Unit), "m2") || strings.Contains(strings.ToLower(f.Unit), "m²") || strings.Contains(strings.ToLower(f.Unit), "sqm") {
				v = v * 10.7639
			}
			if v > 500 && v < 20000000 {
				return f.Field, float64(int(v))
			}
		}
	case "year_opened":
		if v, ok := num(); ok && v > 1950 && v <= float64(time.Now().Year()) {
			return f.Field, int(v)
		}
	case "phone":
		if strings.Count(f.Value, "") > 7 {
			return f.Field, strings.TrimSpace(f.Value)
		}
	case "certifications":
		parts := strings.Split(f.Value, ",")
		var out []string
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return f.Field, out
		}
	}
	return "", nil
}

// twoaiResearchFetch reads the cited page. Operator sites often refuse a
// plain fetch (equinix.com answers 403 to anything without a browser), so a
// refusal falls through a chain of renderers: Firecrawl while its monthly
// quota lasts (the first 402 or 429 of a run stops further attempts, since
// the quota ran out on 2026-09-04 and every retry was a wasted call), then
// Jina Reader, a free rendering proxy that needs no key and returned the
// Equinix SV1 page with its square footage intact when Firecrawl could not.
// A page no path can read verifies nothing, and the figure is not stored.
var twoaiFirecrawlExhausted bool

func twoaiResearchFetch(u string) string {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; theworldofai.org facility registry; verification fetch)")
	if resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req); err == nil {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == 200 && len(b) > 500 {
			return string(b)
		}
	}
	if k := os.Getenv("FIRECRAWL_API_KEY"); k != "" && !twoaiFirecrawlExhausted {
		body, _ := json.Marshal(map[string]any{"url": u, "formats": []string{"markdown"}, "onlyMainContent": false})
		req, _ = http.NewRequest("POST", "https://api.firecrawl.dev/v2/scrape", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+k)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req); err == nil {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if resp.StatusCode == 402 || resp.StatusCode == 429 {
				twoaiFirecrawlExhausted = true
				fmt.Fprintln(os.Stderr, "twoai_dc_research: firecrawl quota exhausted, using jina reader for the rest of the run")
			} else if resp.StatusCode == 200 {
				var out struct {
					Data struct {
						Markdown string `json:"markdown"`
					} `json:"data"`
				}
				if json.Unmarshal(raw, &out) == nil && len(out.Data.Markdown) > 200 {
					time.Sleep(1200 * time.Millisecond)
					return out.Data.Markdown
				}
			}
		}
	}
	// Jina Reader: prefix the URL, get the rendered page as text. Free tier
	// is rate-limited by IP, so one request every few seconds.
	req, _ = http.NewRequest("GET", "https://r.jina.ai/"+u, nil)
	req.Header.Set("User-Agent", "theworldofai.org facility registry verification")
	req.Header.Set("Accept", "text/plain")
	if k := os.Getenv("JINA_API_KEY"); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	if resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req); err == nil {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		time.Sleep(3 * time.Second)
		if resp.StatusCode == 200 && len(b) > 200 {
			return string(b)
		}
	}
	return ""
}
