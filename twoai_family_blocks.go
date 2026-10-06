package main

// The data blocks on a model family page that need no model call:
// theworldofai rows 493 and 494 (Stephen, 2026-10-05, "the model pages are
// still a little thin"). Each is built from records the site already holds
// and is empty when there is nothing to show, so the page renders only the
// blocks that have data.
//
//   price_limits   every live version side by side, cheapest and largest
//                  context marked, from the OpenRouter catalog
//   bench_best     the family's best entry on each benchmark the site tracks
//   hf_repos       the developer's own open-weight repositories across every
//                  Hugging Face section of the catalog, with downloads
//   licences       the licences those repositories declare, with links
//   maker          the company record behind the developer
//   related        news, papers, CVEs, lawsuits and glossary terms that name
//                  the family, five each, newest first
//   faq            common questions answered in sentences built from the
//                  fields above, so a reader and a parser see the same answer

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/lib/pq"
)

// famFindCompany finds the company record behind a family's developer.
func famFindCompany(companies map[string]famCompany, g *famGroup) (famCompany, bool) {
	tries := []string{strings.ToLower(g.DevName), strings.ToLower(g.LineName)}
	if a := famMakerAlias[g.Dev]; a != "" {
		tries = append(tries, a)
	}
	if w := famDevWord(g); w != g.DevName {
		tries = append(tries, strings.ToLower(w))
	}
	for _, t := range tries {
		if c, ok := companies[t]; ok && t != "" {
			return c, true
		}
	}
	return famCompany{}, false
}

// famDevWord is the developer's name without the line it ends with:
// "ByteDance Seed" with line Seed gives ByteDance.
func famDevWord(g *famGroup) string {
	d, l := strings.TrimSpace(g.DevName), strings.TrimSpace(g.LineName)
	if l != "" && len(d) > len(l)+1 && strings.HasSuffix(strings.ToLower(d), " "+strings.ToLower(l)) {
		return strings.TrimSpace(d[:len(d)-len(l)-1])
	}
	return d
}

func famComma(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func famUSD(v float64) string {
	if v == 0 {
		return "free"
	}
	return "$" + strconv.FormatFloat(v, 'f', -1, 64)
}

func famBigUSD(v int64) string {
	f := float64(v)
	switch {
	case f >= 1e9:
		return "$" + strconv.FormatFloat(float64(int64(f/1e8))/10, 'f', -1, 64) + " billion"
	case f >= 1e6:
		return "$" + strconv.FormatFloat(float64(int64(f/1e5))/10, 'f', -1, 64) + " million"
	}
	return "$" + famComma(v)
}

// famJoin joins with commas and "and".
func famJoin(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	case 2:
		return xs[0] + " and " + xs[1]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

func famShortName(n string) string {
	if i := strings.Index(n, ":"); i >= 0 {
		return strings.TrimSpace(n[i+1:])
	}
	return strings.TrimSpace(n)
}

// famPriceLimits puts the live versions side by side and marks the cheapest
// (lowest input plus output price among versions with a listed price) and
// the largest context window.
func famPriceLimits(g *famGroup, name string) map[string]any {
	l := g.live()
	cheap, large := -1, -1
	minIn, maxIn, minOut, maxOut := -1.0, -1.0, -1.0, -1.0
	for i, m := range l {
		if m.PromptPM >= 0 && m.CompletionPM >= 0 {
			if cheap < 0 || m.PromptPM+m.CompletionPM < l[cheap].PromptPM+l[cheap].CompletionPM {
				cheap = i
			}
			if minIn < 0 || m.PromptPM < minIn {
				minIn = m.PromptPM
			}
			if m.PromptPM > maxIn {
				maxIn = m.PromptPM
			}
			if minOut < 0 || m.CompletionPM < minOut {
				minOut = m.CompletionPM
			}
			if m.CompletionPM > maxOut {
				maxOut = m.CompletionPM
			}
		}
		if m.Context > 0 && (large < 0 || m.Context > l[large].Context) {
			large = i
		}
	}
	rows := []map[string]any{}
	for i, m := range l {
		rows = append(rows, map[string]any{"id": m.ID, "name": famShortName(m.Name), "url": "https://openrouter.ai/" + m.ID,
			"prompt_pm": m.PromptPM, "completion_pm": m.CompletionPM, "context": m.Context, "max_output": m.MaxOut,
			"input": m.In, "output": m.Out, "reasoning": m.Reasoning, "open_weights": m.HFID != "",
			"cheapest": i == cheap, "largest_context": i == large})
	}
	var parts []string
	if cheap >= 0 {
		if len(l) > 1 && (minIn != maxIn || minOut != maxOut) {
			parts = append(parts, fmt.Sprintf("Across the %d versions, input prices run from %s to %s per million tokens and output prices from %s to %s.",
				len(l), famUSD(minIn), famUSD(maxIn), famUSD(minOut), famUSD(maxOut)))
		}
		c := l[cheap]
		parts = append(parts, fmt.Sprintf("%s is the cheapest, at %s per million input tokens and %s per million output tokens.",
			famShortName(c.Name), famUSD(c.PromptPM), famUSD(c.CompletionPM)))
	} else {
		parts = append(parts, fmt.Sprintf("The OpenRouter catalog lists no fixed per-token price for %s.", name))
	}
	if large >= 0 {
		parts = append(parts, fmt.Sprintf("%s has the largest context window, %s tokens.", famShortName(l[large].Name), famComma(l[large].Context)))
	}
	return map[string]any{"rows": rows, "summary": strings.Join(parts, " "),
		"min_in": minIn, "max_in": maxIn, "min_out": minOut, "max_out": maxOut, "cheapest": famIdx(l, cheap), "largest_context": famIdx(l, large)}
}

func famIdx(l []famMember, i int) string {
	if i < 0 {
		return ""
	}
	return famShortName(l[i].Name)
}

var famScoreRe = regexp.MustCompile(`-?\d+(?:\.\d+)?`)

// famBenchBest keeps the family's best entry on each benchmark. Every board
// the site tracks reads higher as better (accuracy, rating, success rate,
// time horizon).
func famBenchBest(bench []map[string]string) []map[string]string {
	best := map[string]map[string]string{}
	val := map[string]float64{}
	count := map[string]int{}
	for _, b := range bench {
		m := famScoreRe.FindString(b["score"])
		if m == "" {
			continue
		}
		v, _ := strconv.ParseFloat(m, 64)
		k := b["benchmark"]
		count[k]++
		if cur, ok := val[k]; !ok || v > cur {
			val[k] = v
			best[k] = b
		}
	}
	out := []map[string]string{}
	for k, b := range best {
		r := map[string]string{}
		for kk, vv := range b {
			r[kk] = vv
		}
		r["entries"] = strconv.Itoa(count[k])
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["benchmark"] < out[j]["benchmark"] })
	return out
}

// Licences the repositories declare, by their Hugging Face licence id. An id
// not listed here is shown as written and linked to the repository's card,
// which carries the licence text.
var famLicenceNames = map[string][2]string{
	"apache-2.0":                {"Apache 2.0", "https://www.apache.org/licenses/LICENSE-2.0"},
	"mit":                       {"MIT", "https://opensource.org/license/mit"},
	"gemma":                     {"Gemma Terms of Use", "https://ai.google.dev/gemma/terms"},
	"cc-by-4.0":                 {"CC BY 4.0", "https://creativecommons.org/licenses/by/4.0/"},
	"cc-by-sa-4.0":              {"CC BY-SA 4.0", "https://creativecommons.org/licenses/by-sa/4.0/"},
	"cc-by-nc-4.0":              {"CC BY-NC 4.0", "https://creativecommons.org/licenses/by-nc/4.0/"},
	"cc-by-nc-sa-4.0":           {"CC BY-NC-SA 4.0", "https://creativecommons.org/licenses/by-nc-sa/4.0/"},
	"bsd-3-clause":              {"BSD 3-Clause", "https://opensource.org/license/bsd-3-clause"},
	"nvidia-open-model-license": {"NVIDIA Open Model License", "https://www.nvidia.com/en-us/agreements/enterprise-software/nvidia-open-model-license/"},
	// "other" is Hugging Face's id for a licence of the publisher's own; its
	// text is on the repository's card, which is where the link goes.
	"other": {"Custom licence", ""},
}

// famHFRepos lists the developer's own repositories in every Hugging Face
// section of the catalog whose name carries the line, most downloaded first.
func famHFRepos(db *sql.DB, g *famGroup) []map[string]any {
	orgs := map[string]bool{strings.ToLower(g.Dev): true, strings.ReplaceAll(strings.ToLower(g.Dev), "-", ""): true}
	for _, m := range g.Members {
		if i := strings.Index(m.HFID, "/"); i > 0 {
			orgs[strings.ToLower(m.HFID[:i])] = true
		}
	}
	list := []string{}
	for o := range orgs {
		list = append(list, o)
	}
	out := []map[string]any{}
	rows, err := db.Query(`SELECT DISTINCT ON (ext_id) ext_id, coalesce((data->>'downloads')::bigint,0), coalesce((data->>'likes')::bigint,0),
			coalesce(data->>'pipeline_tag',''), coalesce(left(data->>'createdAt',10),''), coalesce(data->'tags','[]'::jsonb)::text
		FROM twoai_model_catalog WHERE source='huggingface' AND delisted_at IS NULL AND lower(split_part(ext_id,'/',1)) = ANY($1)
		ORDER BY ext_id`, pq.Array(list))
	if err != nil {
		return out
	}
	defer rows.Close()
	line := strings.ToLower(g.Line)
	for rows.Next() {
		var id, task, created, tr string
		var dl, likes int64
		if rows.Scan(&id, &dl, &likes, &task, &created, &tr) != nil {
			continue
		}
		name := id[strings.Index(id, "/")+1:]
		hit := false
		for _, t := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
			if famVersionTail.ReplaceAllString(t, "") == line {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		var tags []string
		json.Unmarshal([]byte(tr), &tags)
		lic := ""
		for _, t := range tags {
			if strings.HasPrefix(t, "license:") {
				lic = strings.TrimPrefix(t, "license:")
			}
		}
		out = append(out, map[string]any{"id": id, "url": "https://huggingface.co/" + id, "downloads": dl, "likes": likes, "task": task, "created": created, "licence": lic})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["downloads"].(int64) > out[j]["downloads"].(int64) })
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// famLicences gathers the licences the repositories declare.
func famLicences(repos []map[string]any) []map[string]any {
	type agg struct {
		id   string
		n    int
		repo string
	}
	m := map[string]*agg{}
	var order []string
	for _, r := range repos {
		id, _ := r["licence"].(string)
		if id == "" {
			continue
		}
		a := m[id]
		if a == nil {
			a = &agg{id: id, repo: r["url"].(string)}
			m[id] = a
			order = append(order, id)
		}
		a.n++
	}
	out := []map[string]any{}
	for _, id := range order {
		a := m[id]
		name, u := id, a.repo
		if v, ok := famLicenceNames[id]; ok {
			name = v[0]
			if v[1] != "" {
				u = v[1]
			}
		}
		out = append(out, map[string]any{"id": id, "name": name, "url": u, "repos": a.n})
	}
	return out
}

var famCountries = map[string]string{"usa": "United States", "us": "United States", "united states": "United States", "u.s.": "United States",
	"china": "China", "france": "France", "canada": "Canada", "united kingdom": "United Kingdom", "uk": "United Kingdom", "japan": "Japan",
	"south korea": "South Korea", "korea": "South Korea", "germany": "Germany", "israel": "Israel", "india": "India", "singapore": "Singapore",
	"switzerland": "Switzerland", "united arab emirates": "United Arab Emirates", "uae": "United Arab Emirates", "netherlands": "Netherlands",
	"sweden": "Sweden", "taiwan": "Taiwan", "finland": "Finland", "spain": "Spain", "italy": "Italy", "australia": "Australia"}

// famCountry reads the country from the end of a headquarters string, and
// only when that end is a country name.
func famCountry(hq string) string {
	parts := strings.Split(hq, ",")
	if len(parts) < 2 {
		return ""
	}
	return famCountries[strings.ToLower(strings.TrimSpace(parts[len(parts)-1]))]
}

// famMaker is the company block: its record's headline facts, funding and
// country, with the company page and the record's own sources.
func famMaker(db *sql.DB, uid, href string) map[string]any {
	var name, org, hq, website, round, roundDate, ticker, srcRaw string
	var founded, employees sql.NullInt64
	var funding, valuation sql.NullInt64
	err := db.QueryRow(`SELECT name, coalesce(org_type,''), founded, coalesce(headquarters,''), coalesce(website,''), total_funding_usd,
			coalesce(last_round,''), coalesce(last_round_date::text,''), valuation_usd, employees, coalesce(ticker,''), coalesce(sources,'[]'::jsonb)::text
		FROM twoai_company_profiles WHERE uid=$1`, uid).Scan(&name, &org, &founded, &hq, &website, &funding, &round, &roundDate, &valuation, &employees, &ticker, &srcRaw)
	if err != nil {
		return nil
	}
	kind := map[string]string{"public-company": "a public company", "startup": "a startup", "research-lab": "a research lab",
		"nonprofit": "a nonprofit", "non-profit": "a nonprofit"}[org]
	if kind == "" {
		kind = "a company"
	}
	s := name + " is " + kind
	if founded.Valid && founded.Int64 > 0 {
		s += fmt.Sprintf(" founded in %d", founded.Int64)
	}
	if hq != "" {
		if founded.Valid && founded.Int64 > 0 {
			s += " and"
		}
		s += " headquartered in " + hq
	}
	s += "."
	if funding.Valid && funding.Int64 > 0 {
		s += " It has raised " + famBigUSD(funding.Int64) + " in total"
		if round != "" {
			s += ", most recently a " + round + " round"
			if roundDate != "" {
				s += " on " + roundDate
			}
		}
		s += "."
	}
	if valuation.Valid && valuation.Int64 > 0 {
		s += " Its last reported valuation is " + famBigUSD(valuation.Int64) + "."
	}
	if ticker != "" {
		s += " Its shares trade under the ticker " + ticker + "."
	}
	facts := []map[string]string{}
	add := func(k, v string) {
		if v != "" {
			facts = append(facts, map[string]string{"label": k, "value": v})
		}
	}
	if founded.Valid && founded.Int64 > 0 {
		add("Founded", strconv.FormatInt(founded.Int64, 10))
	}
	add("Headquarters", hq)
	add("Country", famCountry(hq))
	if funding.Valid && funding.Int64 > 0 {
		add("Total funding", famBigUSD(funding.Int64))
	}
	if valuation.Valid && valuation.Int64 > 0 {
		add("Valuation", famBigUSD(valuation.Int64))
	}
	if employees.Valid && employees.Int64 > 0 {
		add("Employees", famComma(employees.Int64))
	}
	add("Ticker", ticker)
	srcs := []map[string]string{}
	var raw []any
	json.Unmarshal([]byte(srcRaw), &raw)
	for _, x := range raw {
		switch v := x.(type) {
		case string:
			srcs = append(srcs, map[string]string{"name": v, "url": v})
		case map[string]any:
			u, _ := v["url"].(string)
			n, _ := v["name"].(string)
			if n == "" {
				n = u
			}
			if u != "" {
				srcs = append(srcs, map[string]string{"name": n, "url": u})
			}
		}
	}
	return map[string]any{"uid": uid, "name": name, "href": href, "summary": s, "facts": facts, "website": website,
		"country": famCountry(hq), "sources": srcs}
}

var famGlossOnce sync.Once
var famGloss []sectionGlossTerm

// famRelated gathers records elsewhere on the site that name the family.
// The text must name the line and, unless the line is the developer's own
// name, the developer or its company too: Seed, Command and Step are
// ordinary words.
func famRelated(db *sql.DB, g *famGroup, company string, cves []map[string]any) map[string]any {
	rel := map[string]any{}
	line := strings.ToLower(g.LineName)
	if line == "" {
		return rel
	}
	dev := famDevWord(g)
	alts := []string{strings.ToLower(dev)}
	if company != "" {
		alts = append(alts, strings.ToLower(company))
		if w := famSrcFirstWord(company); len(w) >= 5 && !strings.EqualFold(w, dev) {
			alts = append(alts, strings.ToLower(w))
		}
	}
	if a := famMakerAlias[g.Dev]; a != "" {
		alts = append(alts, a)
	}
	var q []string
	for _, a := range alts {
		q = append(q, regexp.QuoteMeta(a))
	}
	devRe := `(` + strings.Join(q, "|") + `)`
	needDev := !strings.EqualFold(dev, g.LineName)
	lineRe := `\m` + regexp.QuoteMeta(line) + `\M`
	if rows, err := db.Query(`SELECT uid, headline, coalesce(published_on::text,'') FROM twoai_news_stories
		WHERE retired_at IS NULL AND uid IS NOT NULL AND published_on > current_date - 730
		  AND lower(headline || ' ' || coalesce(story->>'Summary', story->>'summary', '')) ~ $1
		  AND ($2 = false OR lower(headline || ' ' || coalesce(story->>'Summary', story->>'summary', '')) ~ $3)
		ORDER BY published_on DESC NULLS LAST LIMIT 5`, lineRe, needDev, devRe); err == nil {
		var out []map[string]any
		for rows.Next() {
			var u, h, d string
			if rows.Scan(&u, &h, &d) == nil {
				out = append(out, map[string]any{"title": h, "path": "/ai-news/" + u + "/", "date": trunc(d, 10)})
			}
		}
		rows.Close()
		if len(out) > 0 {
			rel["news"] = out
		}
	}
	// Papers by the developer: an institution of the paper is the developer
	// or its company, and the paper names the line.
	instRe := `"name": "` + devRe
	if rows, err := db.Query(`SELECT title, coalesce(pub_date::text,''), coalesce(doi,''), coalesce(oa_url,''), openalex_id, coalesce(cited_by,0)
		FROM twoai_works
		WHERE to_tsvector('english', COALESCE(title,'') || ' ' || COALESCE(abstract,'')) @@ plainto_tsquery('english', $1)
		  AND institutions::text ~* $2 AND duplicate_of IS NULL AND excluded_reason IS NULL AND coalesce(title,'') <> ''
		ORDER BY pub_date DESC NULLS LAST LIMIT 5`, g.LineName, instRe); err == nil {
		var out []map[string]any
		for rows.Next() {
			var t, d, doi, oa, oid string
			var cited int
			if rows.Scan(&t, &d, &doi, &oa, &oid, &cited) != nil {
				continue
			}
			u := oa
			if doi != "" {
				u = "https://doi.org/" + strings.TrimPrefix(doi, "https://doi.org/")
			} else if u == "" {
				u = "https://openalex.org/" + strings.TrimPrefix(oid, "https://openalex.org/")
			}
			out = append(out, map[string]any{"title": t, "date": trunc(d, 10), "url": u, "cited_by": cited})
		}
		rows.Close()
		if len(out) > 0 {
			rel["papers"] = out
		}
	}
	if len(cves) > 0 {
		c := cves
		if len(c) > 5 {
			c = c[:5]
		}
		rel["cves"] = c
	}
	if rows, err := db.Query(`SELECT slug, case_name, coalesce(status,''), coalesce(filed_date::text,'') FROM ai_lawsuits
		WHERE is_active AND coalesce(slug,'') <> ''
		  AND lower(case_name || ' ' || coalesce(summary,'')) ~ $1
		  AND ($2 = false OR lower(case_name || ' ' || coalesce(summary,'')) ~ $3)
		ORDER BY filed_date DESC NULLS LAST LIMIT 5`, lineRe, needDev, devRe); err == nil {
		var out []map[string]any
		for rows.Next() {
			var slug, n, st, d string
			if rows.Scan(&slug, &n, &st, &d) == nil {
				out = append(out, map[string]any{"title": n, "path": "/ai-lawsuits/" + slug + "/", "status": st, "date": trunc(d, 10)})
			}
		}
		rows.Close()
		if len(out) > 0 {
			rel["lawsuits"] = out
		}
	}
	famGlossOnce.Do(func() { famGloss = sectionLoadIndex(db).terms })
	var terms []map[string]any
	for _, t := range famGloss {
		text := t.Term + " " + t.Line
		if !sectionHasWord(text, g.LineName) {
			continue
		}
		if needDev {
			low := strings.ToLower(text)
			hit := false
			for _, a := range alts {
				if sectionHasWord(low, a) {
					hit = true
				}
			}
			if !hit {
				continue
			}
		}
		terms = append(terms, map[string]any{"title": t.Term, "path": "/ai-glossary/" + t.Slug + "/", "line": t.Line})
		if len(terms) == 5 {
			break
		}
	}
	if len(terms) > 0 {
		rel["terms"] = terms
	}
	return rel
}

// famBlocks adds every data block to the page document. Run after the
// developer blocks are attached, because the questions draw on them.
func famBlocks(db *sql.DB, g *famGroup, doc map[string]any) {
	name, _ := doc["name"].(string)
	doc["price_limits"] = famPriceLimits(g, name)
	if bs, ok := doc["benchmarks"].([]map[string]string); ok {
		doc["bench_best"] = famBenchBest(bs)
	}
	repos := famHFRepos(db, g)
	doc["hf_repos"] = repos
	doc["licences"] = famLicences(repos)
	company := ""
	if dc, ok := doc["developer_company"].(map[string]string); ok {
		if mk := famMaker(db, dc["uid"], dc["href"]); mk != nil {
			doc["maker"] = mk
			company = dc["name"]
		}
	}
	cves, _ := doc["cves"].([]map[string]any)
	doc["related"] = famRelated(db, g, company, cves)
	doc["faq"] = famFAQ(doc)
}

// famFAQ answers the common questions from the page's own fields.
func famFAQ(doc map[string]any) []map[string]string {
	name, _ := doc["name"].(string)
	var out []map[string]string
	add := func(q, a string) {
		if a != "" {
			out = append(out, map[string]string{"q": q, "a": a})
		}
	}
	ms, _ := doc["members"].([]map[string]any)
	live, open, reasoning := 0, 0, 0
	var minCtx, maxCtx int64
	for _, m := range ms {
		if d, _ := m["delisted"].(bool); d {
			continue
		}
		live++
		if o, _ := m["open_weights"].(bool); o {
			open++
		}
		if r, _ := m["reasoning"].(bool); r {
			reasoning++
		}
		if c, _ := m["context"].(int64); c > 0 {
			if minCtx == 0 || c < minCtx {
				minCtx = c
			}
			if c > maxCtx {
				maxCtx = c
			}
		}
	}
	if live == 0 {
		return out
	}
	licNames := []string{}
	custom := false
	if ls, ok := doc["licences"].([]map[string]any); ok {
		for _, l := range ls {
			if l["id"] == "other" {
				custom = true
				continue
			}
			licNames = append(licNames, l["name"].(string))
		}
	}
	repos, _ := doc["hf_repos"].([]map[string]any)

	// Open source.
	var a string
	switch {
	case open == live:
		a = fmt.Sprintf("Every one of the %d versions of %s in the OpenRouter catalog publishes open weights.", live, name)
	case open == 0:
		a = fmt.Sprintf("No. All %d versions of %s in the OpenRouter catalog are served over APIs only, with no open weights.", live, name)
		if len(repos) > 0 {
			a += fmt.Sprintf(" The developer does publish open-weight repositories under the %s name on Hugging Face, the most downloaded being %s.", doc["line"], repos[0]["id"])
		}
	default:
		a = fmt.Sprintf("Partly. %d of the %d versions of %s in the OpenRouter catalog publish open weights, and %d are served over APIs only.", open, live, name, live-open)
	}
	if open > 0 || len(repos) > 0 {
		switch {
		case len(licNames) > 0 && custom:
			a += fmt.Sprintf(" The family's repositories on Hugging Face declare the %s %s, and some carry a custom licence set out on the model card.", famJoin(licNames), map[bool]string{true: "licence", false: "licences"}[len(licNames) == 1])
		case len(licNames) > 0:
			a += fmt.Sprintf(" The family's repositories on Hugging Face declare the %s %s.", famJoin(licNames), map[bool]string{true: "licence", false: "licences"}[len(licNames) == 1])
		case custom:
			a += " The family's repositories on Hugging Face carry a custom licence set out on each model card."
		}
	}
	add(fmt.Sprintf("Is %s open source?", name), a)

	// Cost.
	if pl, ok := doc["price_limits"].(map[string]any); ok {
		minIn, _ := pl["min_in"].(float64)
		maxIn, _ := pl["max_in"].(float64)
		minOut, _ := pl["min_out"].(float64)
		maxOut, _ := pl["max_out"].(float64)
		cheap, _ := pl["cheapest"].(string)
		if cheap == "" {
			add(fmt.Sprintf("How much does %s cost?", name), fmt.Sprintf("The OpenRouter catalog lists no fixed per-token price for %s.", name))
		} else if minIn == maxIn && minOut == maxOut {
			add(fmt.Sprintf("How much does %s cost?", name), fmt.Sprintf("Through the OpenRouter catalog, %s costs %s per million input tokens and %s per million output tokens.", name, famUSD(minIn), famUSD(minOut)))
		} else {
			add(fmt.Sprintf("How much does %s cost?", name), fmt.Sprintf("Through the OpenRouter catalog, %s costs from %s to %s per million input tokens and from %s to %s per million output tokens, depending on the version. The cheapest is %s.",
				name, famUSD(minIn), famUSD(maxIn), famUSD(minOut), famUSD(maxOut), cheap))
		}
	}

	// Newest.
	latest, _ := doc["latest_model"].(string)
	latestOn, _ := doc["latest_release"].(string)
	a = fmt.Sprintf("%s is the newest version in the OpenRouter catalog, added on %s.", famShortName(latest), latestOn)
	if dl, ok := doc["dev_lines"].([]map[string]any); ok {
		for _, l := range dl {
			if d, _ := l["announced"].(string); d != "" {
				dev, _ := doc["developer"].(string)
				if mk, ok := doc["maker"].(map[string]any); ok {
					dev, _ = mk["name"].(string)
				}
				a += fmt.Sprintf(" The newest dated model line on %s's own site is %s, dated %s.", dev, l["name"], d)
				break
			}
		}
	}
	add(fmt.Sprintf("What is the newest %s model?", name), a)

	// Capabilities.
	ins, _ := doc["input_modalities"].([]string)
	outs, _ := doc["output_modalities"].([]string)
	if len(ins) > 0 && len(outs) > 0 {
		a = fmt.Sprintf("%s versions accept %s as input and produce %s.", name, famJoin(ins), famJoin(outs))
		if reasoning == live {
			a += fmt.Sprintf(" All %d versions support reasoning", live)
		} else {
			a += fmt.Sprintf(" %d of the %d versions support reasoning", reasoning, live)
		}
		if maxCtx > 0 && minCtx == maxCtx {
			a += fmt.Sprintf(", and every version has a context window of %s tokens.", famComma(maxCtx))
		} else if maxCtx > 0 {
			a += fmt.Sprintf(", and context windows run from %s to %s tokens.", famComma(minCtx), famComma(maxCtx))
		} else {
			a += "."
		}
		add(fmt.Sprintf("What can %s do?", name), a)
	}

	// Maker.
	if mk, ok := doc["maker"].(map[string]any); ok {
		add(fmt.Sprintf("Who makes %s?", name), fmt.Sprintf("%s is developed by %s. %s", name, mk["name"], mk["summary"]))
	}

	// Where to use it.
	var parts []string
	if acc, ok := doc["access"].([]map[string]any); ok && len(acc) > 0 {
		var ch []string
		for i, x := range acc {
			if i == 6 {
				break
			}
			ch = append(ch, x["channel"].(string))
		}
		who, _ := doc["developer"].(string)
		if mk, ok := doc["maker"].(map[string]any); ok {
			who, _ = mk["name"].(string)
		}
		parts = append(parts, fmt.Sprintf("%s's own site names these ways to use its models: %s.", who, famJoin(ch)))
	}
	if ps, ok := doc["providers"].([]map[string]any); ok && len(ps) > 0 {
		if len(ps) == 1 {
			parts = append(parts, fmt.Sprintf("In the OpenRouter catalog, one provider serves it, %s.", ps[0]["provider"]))
		} else {
			parts = append(parts, fmt.Sprintf("In the OpenRouter catalog, %d providers serve it.", len(ps)))
		}
	}
	if len(parts) > 0 {
		add(fmt.Sprintf("Where can I use %s?", name), strings.Join(parts, " "))
	}
	return out
}
