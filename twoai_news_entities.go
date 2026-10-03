package main

// News intake fixes for the story that was missed, theworldofai bridge row
// 409, 2026-10-03.
//
// AMD agreed on 2026-09-28 to buy World Labs for about $8.2 billion. Four
// GDELT articles and three vendor-news candidates covered it over three days
// and no story was published. Replaying every scheduled run showed why:
//
//   - publishNews read only the newest 600 GDELT rows of a 36 hour window,
//     and the window held up to 1,122 rows, so older coverage fell off the
//     read before any filter ran. At most two AMD articles were ever in view
//     at once.
//   - Ranking kept only the top 10 clusters, and a two-article cluster cannot
//     beat the Bill Gates, Trump and OpenAI clusters of that week.
//   - "AMD to Acquire World Labs" and "AMD Stock: World Labs Acquisition"
//     share only three title words, below every merge threshold.
//   - The vendor-news candidates (RTHK, The Business Times) were never read by
//     the briefing at all. Nothing promotes them; status 'new' is a default.
//   - One RTHK URL was stored with a literal \u003d in it, so the same article
//     was filed twice.
//
// This file holds the pieces the fix needs: URL canonicalisation, the entity
// vocabulary and matcher used to merge clusters that name the same two
// subjects, and the daily safety check that tells theworldofai when a subject
// was covered widely and no story exists.

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var newsUEscRe = regexp.MustCompile(`\\u([0-9a-fA-F]{4})`)

// Query parameters that never change which article a URL names: campaign
// tags, click ids and page-furniture switches such as RTHK's spTabChangeable.
var newsDropParams = map[string]bool{
	"sptabchangeable": true, "ocid": true, "cmpid": true, "fbclid": true,
	"gclid": true, "mc_cid": true, "mc_eid": true, "ref": true, "smid": true,
}

// newsCanonURL returns the form of u used to recognise one article reached
// two ways. It decodes JSON \uXXXX escapes left in by a single unquote,
// lowercases the host, drops the fragment and drops tracking parameters.
// Path and remaining query are kept as written.
func newsCanonURL(u string) string {
	u = strings.TrimSpace(u)
	u = newsUEscRe.ReplaceAllStringFunc(u, func(m string) string {
		v, err := strconv.ParseUint(m[2:], 16, 32)
		if err != nil {
			return m
		}
		return string(rune(v))
	})
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return u
	}
	p.Host = strings.ToLower(p.Host)
	p.Fragment = ""
	if p.RawQuery != "" {
		q := p.Query()
		changed := false
		for k := range q {
			lk := strings.ToLower(k)
			if strings.HasPrefix(lk, "utm_") || newsDropParams[lk] {
				q.Del(k)
				changed = true
			}
		}
		if changed {
			p.RawQuery = q.Encode()
		}
	}
	return p.String()
}

// newsEntWord folds a word for entity matching: lowercase alphanumerics, and a
// plain plural s dropped so "World Labs" in a headline matches the "world lab"
// GDELT extracted from the body.
func newsEntWord(w string) string {
	if len(w) >= 4 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
		return w[:len(w)-1]
	}
	return w
}

// newsEntNorm turns a name or a headline into space-separated folded words.
func newsEntNorm(s string) string {
	ws := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9')
	})
	for i, w := range ws {
		ws[i] = newsEntWord(w)
	}
	return strings.Join(ws, " ")
}

// Names too generic to identify anything when they appear in a headline.
var newsEntGeneric = map[string]bool{
	"ai": true, "the": true, "new": true, "data": true, "cloud": true, "tech": true,
	"technology": true, "news": true, "group": true, "inc": true, "lab": true,
	"intelligence": true, "artificial intelligence": true, "agent": true, "model": true,
	"market": true, "world": true, "global": true, "president": true, "government": true,
	"company": true, "chip": true, "service": true, "security": true, "research": true,
	"fair": true, "city": true, "state": true, "council": true, "university": true,
}

// newsEntityDisplay returns an entity as a headline wrote it ("Thales", not
// the folded "thale"), or the folded key when the headline does not hold it.
func newsEntityDisplay(title, key string) string {
	orig := strings.FieldsFunc(title, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9')
	})
	kw := strings.Fields(key)
	for i := 0; i+len(kw) <= len(orig); i++ {
		ok := true
		for j, w := range kw {
			if newsEntWord(strings.ToLower(orig[i+j])) != w {
				ok = false
				break
			}
		}
		if ok {
			return strings.Join(orig[i:i+len(kw)], " ")
		}
	}
	return key
}

// newsEntityDict loads the names the site already knows as entities:
// companies, people, models and data-centre operators, names and aliases.
func newsEntityDict(db *sql.DB) map[string]string {
	out := map[string]string{}
	if db == nil {
		return out
	}
	rows, err := db.Query(`SELECT kind, name, COALESCE(aliases,'[]'::jsonb)::text FROM twoai_entities
		WHERE kind IN ('company','person','model','dc_operator')`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var kind, name, aliases string
		if rows.Scan(&kind, &name, &aliases) != nil {
			continue
		}
		names := []string{name}
		for _, a := range strings.Split(strings.Trim(aliases, "[]"), ",") {
			if a = strings.Trim(strings.TrimSpace(a), `"`); a != "" {
				names = append(names, a)
			}
		}
		for _, n := range names {
			k := newsEntNorm(n)
			if len(k) < 3 || newsEntGeneric[k] || kind == "person" && !strings.Contains(k, " ") {
				continue
			}
			if _, ok := out[k]; !ok {
				out[k] = kind
			}
		}
	}
	return out
}

// newsEntityVocab adds to the dictionary every GDELT-extracted person or
// organisation that appears in at least one headline in the window. GDELT
// reads the article body, so its lists are noisy, but a name that also sits
// in a headline is a subject of that coverage. This is how "World Labs",
// which had no entity record, became matchable in an RTHK headline that
// carried no GDELT fields at all.
func newsEntityVocab(dict map[string]string, titles []string, gdeltNames []string) map[string]string {
	v := make(map[string]string, len(dict)+64)
	for k, kind := range dict {
		v[k] = kind
	}
	all := " " + strings.Join(func() []string {
		o := make([]string, len(titles))
		for i, t := range titles {
			o[i] = newsEntNorm(t)
		}
		return o
	}(), " | ") + " "
	for _, g := range gdeltNames {
		k := newsEntNorm(g)
		if len(k) < 4 || newsEntGeneric[k] || !strings.Contains(k, " ") {
			continue
		}
		if _, ok := v[k]; ok {
			continue
		}
		if strings.Contains(all, " "+k+" ") {
			v[k] = "gdelt"
		}
	}
	return v
}

// newsTitleEntities returns the vocabulary entries found in a headline, as
// whole folded words, longest first so "world lab" wins over a shorter name
// inside it.
func newsTitleEntities(title string, vocab map[string]string) []string {
	orig := strings.FieldsFunc(title, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9')
	})
	ws := make([]string, len(orig))
	for i, w := range orig {
		ws[i] = newsEntWord(strings.ToLower(w))
	}
	found := map[string]bool{}
	used := make([]bool, len(ws))
	for n := 4; n >= 1; n-- {
		for i := 0; i+n <= len(ws); i++ {
			skip := false
			for j := i; j < i+n; j++ {
				if used[j] {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			k := strings.Join(ws[i:i+n], " ")
			if _, ok := vocab[k]; ok && !newsEntGeneric[k] {
				// A name counts only when the headline writes it as one:
				// "Pitch" the company, not "pitch" the verb, and not "the AI
				// stack" read as the company Stack AI.
				if c := orig[i][0]; !('A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
					continue
				}
				found[k] = true
				for j := i; j < i+n; j++ {
					used[j] = true
				}
			}
		}
	}
	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// newsDealWord folds the ways a headline says one company is buying another
// into one token, so "acquire", "buys", "acquisition" and "takeover" count as
// the same word when clusters are compared.
var newsDealWord = map[string]bool{
	"acquire": true, "acquir": true, "acquisition": true, "buy": true, "buys": true,
	"buying": true, "bought": true, "purchase": true, "purchas": true, "takeover": true,
}

// newsSharedEntities counts entities both clusters name that are not
// ubiquitous in the window.
func newsSharedEntities(a, b map[string]bool, ubiquitous map[string]bool) int {
	n := 0
	for e := range a {
		if b[e] && !ubiquitous[e] {
			n++
		}
	}
	return n
}

// newsPreview makes publishNews print its stories and stop (news_preview).
var newsPreview bool

// newsAsOf is the moment the briefing is built for: now, or TWOAI_NEWS_ASOF
// for a preview of a past day.
func newsAsOf() time.Time {
	if v := strings.TrimSpace(os.Getenv("TWOAI_NEWS_ASOF")); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}

// newsGapItem is one subject the safety check reports.
type newsGapItem struct {
	Key      string
	Names    []string
	Display  []string
	Outlets  int
	Examples []string
}

// twoaiNewsGapCheck is the daily safety check of bridge row 409: any company,
// or any pair of named subjects, carried in headlines by three or more
// outlets in 72 hours with no story in the last five days naming it goes to
// theworldofai on the bridge, so a miss is seen within a day. Once a day; a
// subject already reported in the last three days is not reported again.
func twoaiNewsGapCheck(db *sql.DB) error {
	return twoaiNewsGap(db, time.Now().UTC(), false)
}

// twoaiNewsGap runs the check as of asOf. preview prints and writes nothing.
func twoaiNewsGap(db *sql.DB, asOf time.Time, preview bool) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_news_gap_runs (
		run_date date PRIMARY KEY, flagged int NOT NULL DEFAULT 0, created_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_news_gap_flags (
		gap_key text PRIMARY KEY, outlets int, first_flagged timestamptz NOT NULL DEFAULT now(),
		last_flagged timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	today := asOf.Format("2006-01-02")
	var done bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM twoai_news_gap_runs WHERE run_date=$1::date)`, today).Scan(&done)
	if done && !preview {
		fmt.Println("news_gap: already checked today")
		return nil
	}

	type item struct{ title, url, domain, names string }
	var items []item
	seen := map[string]bool{}
	add := func(it item) {
		c := newsCanonURL(it.url)
		if c == "" || seen[c] || !twoaiTitleIsAI(it.title) || twoaiIsIndexPage(it.title, c) {
			return
		}
		seen[c] = true
		it.url = c
		items = append(items, it)
	}
	rows, err := db.Query(`SELECT d.title, d.url, COALESCE(d.raw->>'domain',''),
			COALESCE(d.raw->>'persons','') || ';' || COALESCE(d.raw->>'orgs','')
		FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
		WHERE s.key='gdelt' AND d.title <> '' AND d.fetched_at > $1::timestamptz - interval '72 hours' AND d.fetched_at <= $1::timestamptz`, asOf)
	if err != nil {
		return err
	}
	for rows.Next() {
		var it item
		if rows.Scan(&it.title, &it.url, &it.domain, &it.names) == nil {
			add(it)
		}
	}
	rows.Close()
	for _, c := range newsCandidateArticles(db, asOf, 72) {
		add(item{title: c.Title, url: c.URL, domain: c.Domain})
	}

	titles := make([]string, len(items))
	var gnames []string
	for i, it := range items {
		titles[i] = it.title
		for _, n := range strings.Split(it.names, ";") {
			if n = strings.TrimSpace(n); n != "" {
				gnames = append(gnames, n)
			}
		}
	}
	dict := newsEntityDict(db)
	vocab := newsEntityVocab(dict, titles, gnames)

	// What the published stories already cover: headline and summary of every
	// live story of the last five days, folded the same way.
	var covered []string
	srows, err := db.Query(`SELECT headline || ' ' || COALESCE(story->>'Summary','')
		FROM twoai_news_stories WHERE retired_at IS NULL AND published_on >= $1::date - 5 AND first_published <= $1::timestamptz`, asOf)
	if err == nil {
		for srows.Next() {
			var t string
			if srows.Scan(&t) == nil {
				covered = append(covered, " "+newsEntNorm(t)+" ")
			}
		}
		srows.Close()
	}
	isCovered := func(names ...string) bool {
		for _, c := range covered {
			all := true
			for _, n := range names {
				if !strings.Contains(c, " "+n+" ") {
					all = false
					break
				}
			}
			if all {
				return true
			}
		}
		return false
	}

	type acc struct {
		outlets  map[string]bool
		examples []string
	}
	groups := map[string]*acc{}
	display := map[string]string{}
	note := func(key string, it item) {
		g := groups[key]
		if g == nil {
			g = &acc{outlets: map[string]bool{}}
			groups[key] = g
		}
		dom := it.domain
		if dom == "" {
			dom = publisherFromURL(it.url)
		}
		if !g.outlets[dom] {
			g.outlets[dom] = true
			if len(g.examples) < 3 {
				g.examples = append(g.examples, fmt.Sprintf("%s (%s) %s", trunc(it.title, 110), dom, it.url))
			}
		}
	}
	for _, it := range items {
		ents := newsTitleEntities(it.title, vocab)
		for _, e := range ents {
			if display[e] == "" {
				display[e] = newsEntityDisplay(it.title, e)
			}
		}
		for _, e := range ents {
			if vocab[e] == "company" {
				note(e, it)
			}
		}
		for i := 0; i < len(ents); i++ {
			for j := i + 1; j < len(ents); j++ {
				note(ents[i]+" + "+ents[j], it)
			}
		}
	}

	var gaps []newsGapItem
	for key, g := range groups {
		if len(g.outlets) < 3 {
			continue
		}
		names := strings.Split(key, " + ")
		if isCovered(names...) {
			continue
		}
		disp := make([]string, len(names))
		for i, n := range names {
			disp[i] = display[n]
			if disp[i] == "" {
				disp[i] = n
			}
		}
		gaps = append(gaps, newsGapItem{Key: key, Names: names, Display: disp, Outlets: len(g.outlets), Examples: g.examples})
	}
	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].Outlets != gaps[j].Outlets {
			return gaps[i].Outlets > gaps[j].Outlets
		}
		return gaps[i].Key < gaps[j].Key
	})
	// A pair already explains its two single names: drop a single company
	// when a reported pair contains it with as many outlets.
	keep := gaps[:0]
	for _, g := range gaps {
		if len(g.Names) == 1 {
			dup := false
			for _, p := range gaps {
				if len(p.Names) == 2 && p.Outlets >= g.Outlets && (p.Names[0] == g.Names[0] || p.Names[1] == g.Names[0]) {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
		}
		var recent bool
		db.QueryRow(`SELECT EXISTS(SELECT 1 FROM twoai_news_gap_flags WHERE gap_key=$1 AND last_flagged > now() - interval '3 days')`, g.Key).Scan(&recent)
		if recent && !preview {
			continue
		}
		keep = append(keep, g)
	}
	gaps = keep
	if len(gaps) > 12 {
		gaps = gaps[:12]
	}

	if preview {
		fmt.Printf("news_gap preview as of %s: articles=%d vocabulary=%d subjects=%d would flag=%d\n", asOf.Format(time.RFC3339), len(items), len(vocab), len(groups), len(gaps))
		for _, g := range gaps {
			fmt.Printf("news_gap preview: %s, %d outlets\n", strings.Join(g.Display, " and "), g.Outlets)
		}
		return nil
	}
	if len(gaps) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "Daily news safety check, %s (srj-pipeline news_gap, bridge row 409). These subjects were in the headlines of three or more outlets in the last 72 hours and no live story from the last five days names them. Each is either a story the briefing missed or one you can ignore.\n", today)
		for i, g := range gaps {
			fmt.Fprintf(&b, "\n%d. %s, %d outlets\n", i+1, strings.Join(g.Display, " and "), g.Outlets)
			for _, ex := range g.Examples {
				fmt.Fprintf(&b, "   - %s\n", ex)
			}
		}
		b.WriteString("\nA subject reported here is not reported again for three days.")
		if _, err := db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body) VALUES ('srj','theworldofai',$1,$2)`,
			fmt.Sprintf("News safety check: %d subject%s with wide coverage and no story", len(gaps), map[bool]string{true: "", false: "s"}[len(gaps) == 1]),
			b.String()); err != nil {
			return err
		}
		for _, g := range gaps {
			db.Exec(`INSERT INTO twoai_news_gap_flags (gap_key, outlets) VALUES ($1,$2)
				ON CONFLICT (gap_key) DO UPDATE SET outlets=EXCLUDED.outlets, last_flagged=now()`, g.Key, g.Outlets)
		}
	}
	db.Exec(`INSERT INTO twoai_news_gap_runs (run_date, flagged) VALUES ($1::date,$2) ON CONFLICT (run_date) DO NOTHING`, today, len(gaps))
	fmt.Printf("news_gap: articles=%d vocabulary=%d subjects=%d flagged=%d ok=true\n", len(items), len(vocab), len(groups), len(gaps))
	for _, g := range gaps {
		fmt.Printf("news_gap: %s, %d outlets\n", strings.Join(g.Display, " and "), g.Outlets)
	}
	return nil
}

// newsCandidate is a vendor-news intake row read as coverage.
type newsCandidate struct {
	Title, URL, Domain, Date string
	Fresh                    bool
}

// newsCandidateArticles reads the vendor-news candidates of the last `hours`
// as articles. The Google News coverage feeds (RTHK, The Business Times,
// Straits Times and the rest) land here and nowhere else; until 2026-10-03
// the briefing never read them. Excluded: rows still on a news.google.com
// redirect, the accounting feeds and techgig, which have their own story
// stages. The title loses the trailing " - Publisher" Google News appends.
func newsCandidateArticles(db *sql.DB, asOf time.Time, hours int) []newsCandidate {
	var out []newsCandidate
	rows, err := db.Query(`SELECT name, url, COALESCE(published_on::text, discovered_at::date::text),
			discovered_at > $2::timestamptz - interval '36 hours'
		FROM ai_intel_candidates
		WHERE kind='vendor-news' AND discovered_at > $2::timestamptz - make_interval(hours => $1) AND discovered_at <= $2::timestamptz
		  AND url NOT LIKE '%news.google.com%' AND vendor NOT LIKE '%(accounting)%' AND url NOT LIKE '%techgig.com%'
		ORDER BY discovered_at DESC`, hours, asOf)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var c newsCandidate
		if rows.Scan(&c.Title, &c.URL, &c.Date, &c.Fresh) != nil {
			continue
		}
		if m := newsOutletTailRe.FindStringSubmatch(strings.TrimSpace(c.Title)); m != nil {
			c.Title = strings.TrimSpace(m[1])
		}
		c.URL = newsCanonURL(c.URL)
		c.Domain = publisherFromURL(c.URL)
		if c.Title == "" || c.Domain == "" {
			continue
		}
		out = append(out, c)
	}
	return out
}
