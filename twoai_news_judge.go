package main

// Judging AI by the article, building stories the safety check finds, and
// keeping one outlet's articles apart. theworldofai bridge rows 414 to 417,
// 2026-10-03.
//
//   - Row 416 and 417 (Stephen): whether a story is about AI is judged from
//     the article text by the model, not by AI words in the headline. A chip,
//     data centre, power, robotics or labour story can be AI-driven without
//     saying so. Every verdict and its one-line reason is kept in
//     twoai_ai_verdicts, the safety check's and the followed companies' in
//     one table, so the rule can be reviewed and tuned.
//   - Row 415: a subject the daily safety check finds is a missed story, not
//     a question. If the judge says AI is the substance, the story is built
//     through the normal shape (headline written for the site, summary, the
//     outlet links, entity pins) and the list goes to the bridge. If not, it
//     is reported with the judge's reason.
//   - Row 414: one outlet's articles never make a story on their own merit.
//     The Manila Times bundle (9a54c265, seven unrelated pieces that shared
//     only the outlet name in their titles) and two different NTT DATA
//     releases inside 60c9e364 are the cases.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// newsStop is the stop list publishNews has always used for headline words.
var newsStop = map[string]bool{"the": true, "a": true, "an": true, "of": true, "to": true, "in": true, "on": true, "for": true, "and": true, "with": true, "as": true, "at": true, "by": true, "is": true, "its": true, "ai": true, "artificial": true, "intelligence": true, "new": true, "how": true, "what": true, "why": true}

// newsTitleTokens is the headline word set publishNews compares: lowercase
// words over two letters, stop words out, a light suffix strip, and the
// ways of saying "buys" folded into one word. Callers strip the outlet's
// name first (newsStripOutlet): the Manila Times bundle formed because
// " | The Manila Times" put "manila" and "times" into every short headline.
func newsTitleTokens(s string) map[string]bool {
	m := map[string]bool{}
	w := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !('a' <= r && r <= 'z' || '0' <= r && r <= '9') })
	for _, x := range w {
		if len(x) > 2 && !newsStop[x] {
			// LIGHT STEMMING. "enacts", "enacted" and "enacting" are one
			// token. Without this, California's auditing laws ran as two
			// stories on 2026-09-10.
			for _, suf := range []string{"ing", "ed", "es", "s"} {
				if len(x) > len(suf)+3 && strings.HasSuffix(x, suf) {
					x = strings.TrimSuffix(x, suf)
					break
				}
			}
			if newsDealWord[x] {
				x = "acquire"
			}
			m[x] = true
		}
	}
	return m
}

// newsTokSim is overlap over the smaller set, the measure publishNews uses.
func newsTokSim(a, b map[string]bool) float64 {
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	d := len(a)
	if len(b) < d {
		d = len(b)
	}
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// newsIsOutletEntity reports a name that is the publisher itself: "NTT DATA"
// on nttdata.com, "Manila Times" on manilatimes.net. Two articles from one
// outlet always share that name, so it is no evidence they share a story.
func newsIsOutletEntity(e, domain string) bool {
	n := newsNorm(e)
	d := newsNorm(domain)
	return len(n) >= 3 && d != "" && strings.Contains(d, n)
}

// newsSharedNonOutlet counts entities two sets share, the outlet excluded.
func newsSharedNonOutlet(a, b map[string]bool, domain string) int {
	n := 0
	for e := range a {
		if b[e] && !newsIsOutletEntity(e, domain) {
			n++
		}
	}
	return n
}

// newsSameOutletStory is the row 414 test for two items from one outlet:
// they are one story only when they share a named subject other than the
// outlet and their headlines agree on the topic.
func newsSameOutletStory(tkA, tkB map[string]bool, entA, entB map[string]bool, domain string) bool {
	return newsSharedNonOutlet(entA, entB, domain) >= 1 && newsTokSim(tkA, tkB) >= 0.6
}

// newsBundle reports a story whose articles all come from one outlet and do
// not agree on the topic: a bundle, not a story.
func newsBundle(titles []string, domains map[string]bool) bool {
	if len(domains) != 1 || len(titles) < 2 {
		return false
	}
	var dom string
	for d := range domains {
		dom = d
	}
	lead := newsTitleTokens(newsStripOutlet(titles[0], dom))
	for _, t := range titles[1:] {
		if newsTokSim(lead, newsTitleTokens(newsStripOutlet(t, dom))) < 0.6 {
			return true
		}
	}
	return false
}

// newsBundleRepair undoes what one-outlet bundles did before row 414's rule:
// a merge whose loser is a bundle is reversed (the survivor gives back the
// bundle's links, the loser and its pins come back), then every live bundle
// of the last fortnight keeps only the articles that agree with its own
// headline. Each part keeps its own pins: nothing is moved, only the stray
// links leave. Runs each build, before the dedupe pass, and is idempotent.
func newsBundleRepair(db *sql.DB) {
	newsUnmergeWhere(db, "bundle", func(loser *dedupeStory) bool { return newsBundle(loser.titles, loser.domains) })
	newsBundleChainStrip(db)
	rows, err := db.Query(`SELECT uid, headline, story::text FROM twoai_news_stories
		WHERE retired_at IS NULL AND published_on > current_date - 14`)
	if err != nil {
		return
	}
	type fix struct{ uid, raw string }
	var fixes []fix
	trimmed := 0
	for rows.Next() {
		var uid, head, raw string
		if rows.Scan(&uid, &head, &raw) != nil {
			continue
		}
		var st map[string]any
		if json.Unmarshal([]byte(raw), &st) != nil {
			continue
		}
		d := newsDedupeFrom(uid, "", head, st)
		if !newsBundle(d.titles, d.domains) {
			continue
		}
		var dom string
		for x := range d.domains {
			dom = x
		}
		lead := newsTitleTokens(newsStripOutlet(head, dom))
		arts, _ := st["Articles"].([]any)
		keep := []any{}
		for _, a := range arts {
			m, _ := a.(map[string]any)
			t, _ := m["Title"].(string)
			if newsTokSim(lead, newsTitleTokens(newsStripOutlet(t, dom))) >= 0.6 {
				keep = append(keep, a)
			}
		}
		if len(keep) == 0 && len(arts) > 0 {
			keep = arts[:1]
		}
		if len(keep) == len(arts) {
			continue
		}
		st["Articles"] = keep
		st["ArticleCount"] = len(keep)
		st["bundle_trimmed"] = fmt.Sprintf("%d unrelated %s articles removed %s (bridge row 414)", len(arts)-len(keep), dom, time.Now().UTC().Format("2006-01-02"))
		if b, err := json.Marshal(st); err == nil {
			fixes = append(fixes, fix{uid, string(b)})
		}
	}
	rows.Close()
	for _, f := range fixes {
		if _, err := db.Exec(`UPDATE twoai_news_stories SET story=$2::jsonb WHERE uid=$1`, f.uid, f.raw); err == nil {
			trimmed++
			fmt.Printf("news_bundle: trimmed %s to the articles that match its headline\n", f.uid)
		}
	}
	fmt.Printf("news_bundle: trimmed=%d ok=true\n", trimmed)
}

// twoaiAIVerdictsEnsure creates the one table every AI-substance verdict is
// kept in (rows 416 and 417).
func twoaiAIVerdictsEnsure(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_ai_verdicts (
		id bigserial PRIMARY KEY, source text NOT NULL, subject text NOT NULL, url text,
		verdict boolean NOT NULL, reason text NOT NULL, model text, judged_at timestamptz NOT NULL DEFAULT now())`)
}

func twoaiAIVerdictLog(db *sql.DB, source, subject, url string, yes bool, reason, model string) {
	twoaiAIVerdictsEnsure(db)
	db.Exec(`INSERT INTO twoai_ai_verdicts (source, subject, url, verdict, reason, model) VALUES ($1,$2,NULLIF($3,''),$4,$5,$6)`,
		source, subject, url, yes, reason, model)
}

const twoaiAIJudgeSystem = `You decide whether artificial intelligence is the substance of a news story, for The World of AI, a reference site about AI read by business leaders, developers and policy readers. AI is the substance when the story is about AI models, AI products or features, the AI business of a company, chips or compute for AI, data centres or power built for AI, robotics or automation driven by AI, laws, policy or regulation of AI, or the effect of AI on work, even when the headline never says AI. AI is not the substance when it is only a passing mention, or when the story is mainly about something else, such as a phone or earbuds launch where AI is one feature among many, results or deals with no AI angle, sport, or general politics. Judge only from the text given. Reply with one line: YES or NO, a colon, then one reason under 25 words.`

// twoaiAIJudge asks the model whether AI is the substance of the story told
// by these headlines and this text. err means no verdict: try again later.
func twoaiAIJudge(stage, subject string, titles []string, text string) (bool, string, string, error) {
	if len(text) > 9000 {
		text = text[:9000]
	}
	user := "Subject: " + subject + "\nHeadlines:\n"
	for _, t := range titles {
		user += "- " + t + "\n"
	}
	user += "\nArticle text:\n" + text
	out, model, err := twoaiGenerate(stage, twoaiAIJudgeSystem, user)
	if err != nil {
		return false, "", model, err
	}
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	line = strings.TrimLeft(line, "*# ")
	up := strings.ToUpper(line)
	var yes bool
	var rest string
	switch {
	case strings.HasPrefix(up, "YES"):
		yes, rest = true, line[3:]
	case strings.HasPrefix(up, "NO"):
		rest = line[2:]
	default:
		return false, "", model, fmt.Errorf("judge gave no verdict: %.80s", line)
	}
	return yes, strings.TrimSpace(strings.TrimLeft(rest, ":.-* ")), model, nil
}

var newsFetchStrip = regexp.MustCompile(`(?is)<(script|style|nav|header|footer|aside)[^>]*>.*?</(script|style|nav|header|footer|aside)>`)
var newsFetchClient = &http.Client{Timeout: 25 * time.Second}

// newsArticleText returns an article's text: the GDELT full text already
// stored when there is one, otherwise a fetch, the way twoai_solo_news reads
// a page. "" when nothing readable came back.
func newsArticleText(db *sql.DB, u string) string {
	var t string
	db.QueryRow(`SELECT COALESCE(substr(fulltext,1,14000),'') FROM pipeline.documents WHERE url=$1 AND fulltext IS NOT NULL ORDER BY id DESC LIMIT 1`, u).Scan(&t)
	if len(t) >= 1200 {
		return t
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; theworldofai.org news reader; srj@srjconsultingservices.com)")
	resp, err := newsFetchClient.Do(req)
	if err != nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	text := enfPlain(newsFetchStrip.ReplaceAllString(string(body), " "))
	if len(text) < 1200 {
		return ""
	}
	if len(text) > 14000 {
		text = text[:14000]
	}
	return text
}

// newsGapArticle is one article behind a safety-check subject.
type newsGapArticle struct{ Title, URL, Domain string }

// newsGapResult is what acting on one subject produced.
type newsGapResult struct {
	Display string
	Outlets int
	Stories []string // "headline (uid, n outlets)"
	Report  string   // why no story, when none was built
}

const newsGapStorySystem = `You write one news story for The World of AI, a reference site about artificial intelligence read by business leaders, developers and policy readers, from several outlets' articles about the same event. Use only facts stated in the article texts given; do not add background, opinions or anything they do not say. Ignore navigation, menus, related headlines and author bios. Reply in exactly this form:
HEADLINE: one factual headline under 110 characters, naming who did what, with no outlet names, no quotation marks and no dashes
SUMMARY: two short paragraphs, 120 to 220 words in total, plain English, what happened and who said it, then the figures or specifics the articles give. No headings, no bullet points, no quotation longer than ten words.`

var newsGapHeadRe = regexp.MustCompile(`(?is)HEADLINE:\s*(.+?)\s*\n+\s*SUMMARY:\s*(.+)$`)

// newsGapAct turns one flagged subject into stories (row 415) when the judge
// says AI is its substance (row 416), or into a reported line with the
// judge's reason. companies maps a folded entity name to a company uid with
// a page, for the pins.
func newsGapAct(db *sql.DB, key, display string, ents []string, arts []newsGapArticle, outlets int, asOf time.Time, companies map[string]string) newsGapResult {
	res := newsGapResult{Display: display, Outlets: outlets}
	if len(arts) > 14 {
		arts = arts[:14]
	}
	// Read up to four of the articles for the judge.
	texts := map[string]string{}
	var titles []string
	var joined strings.Builder
	for _, a := range arts {
		titles = append(titles, a.Title)
		if len(texts) >= 4 {
			continue
		}
		if t := newsArticleText(db, a.URL); t != "" {
			texts[a.URL] = t
			if joined.Len() < 9000 {
				fmt.Fprintf(&joined, "[%s] %s\n\n", a.Domain, trunc(t, 3000))
			}
		}
	}
	if len(texts) == 0 {
		res.Report = "no article text could be read, so nothing was judged"
		twoaiAIVerdictLog(db, "news_gap", key, arts[0].URL, false, res.Report, "")
		return res
	}
	yes, reason, model, err := twoaiAIJudge("news_gap_judge", display, titles, joined.String())
	if err != nil {
		res.Report = "the AI judge did not answer, it will be tried on the next run"
		return res
	}
	twoaiAIVerdictLog(db, "news_gap", key, arts[0].URL, yes, reason, model)
	if !yes {
		res.Report = "judged not about AI: " + reason
		return res
	}

	// One subject can hold several events (LTIMindtree's Blueverse launches
	// and its NVIDIA tax platform). Articles join when their headlines agree,
	// and each group of two or more outlets is one story.
	type grp struct {
		arts []newsGapArticle
		tk   map[string]bool
	}
	var groups []*grp
	for _, a := range arts {
		tk := newsTitleTokens(newsStripOutlet(a.Title, a.Domain))
		var hit *grp
		for _, g := range groups {
			if newsTokSim(tk, g.tk) >= 0.4 {
				hit = g
				break
			}
		}
		if hit == nil {
			groups = append(groups, &grp{arts: []newsGapArticle{a}, tk: tk})
			continue
		}
		hit.arts = append(hit.arts, a)
	}
	singles := 0
	for _, g := range groups {
		doms := map[string]bool{}
		for _, a := range g.arts {
			doms[a.Domain] = true
		}
		if len(doms) < 2 {
			singles++
			continue
		}
		// Already a story? Then it was covered under other names.
		var have string
		for _, a := range g.arts {
			db.QueryRow(`SELECT uid FROM twoai_news_stories WHERE retired_at IS NULL
				AND story->'Articles' @> jsonb_build_array(jsonb_build_object('URL', $1::text)) LIMIT 1`, a.URL).Scan(&have)
			if have != "" {
				break
			}
		}
		if have != "" {
			res.Stories = append(res.Stories, fmt.Sprintf("already covered by story %s", have))
			continue
		}
		var body strings.Builder
		sumURL, sumDom := "", ""
		for _, a := range g.arts {
			t := texts[a.URL]
			if t == "" && body.Len() < 6000 {
				t = newsArticleText(db, a.URL)
			}
			if t == "" {
				continue
			}
			if sumURL == "" {
				sumURL, sumDom = a.URL, a.Domain
			}
			if body.Len() < 12000 {
				fmt.Fprintf(&body, "Article from %s, headline: %s\n%s\n\n", a.Domain, a.Title, trunc(t, 4000))
			}
		}
		if body.Len() == 0 {
			singles++
			continue
		}
		out, smodel, gerr := twoaiGenerate("news_gap_story", newsGapStorySystem, body.String())
		m := newsGapHeadRe.FindStringSubmatch(strings.TrimSpace(out))
		if gerr != nil || m == nil || isRefusal(out) {
			res.Stories = append(res.Stories, "a story was due but the writer gave no usable text, it will be tried on the next run")
			continue
		}
		headline := strings.Trim(strings.TrimSpace(m[1]), `"'`)
		for _, a := range g.arts {
			headline = newsStripOutlet(headline, a.Domain)
		}
		summary := strings.TrimSpace(m[2])
		slug := strings.Trim(acctSlugRe.ReplaceAllString(strings.ToLower(headline), "-"), "-")
		if len(slug) > 80 {
			slug = strings.Trim(slug[:80], "-")
		}
		if slug == "" {
			continue
		}
		uid := twoaiUID("story:" + slug)
		articles := []map[string]string{}
		domains := []string{}
		seenDom := map[string]bool{}
		for _, a := range g.arts {
			articles = append(articles, map[string]string{"URL": a.URL, "Title": a.Title, "Domain": a.Domain, "Date": asOf.Format(time.RFC3339)})
			if !seenDom[a.Domain] {
				seenDom[a.Domain] = true
				domains = append(domains, a.Domain)
			}
		}
		orgs := []string{}
		for _, e := range ents {
			orgs = append(orgs, newsEntityDisplay(strings.Join(titles, " | "), e))
		}
		st := map[string]any{
			"uid": uid, "Slug": slug, "Headline": headline, "Summary": summary,
			"Articles": articles, "Domains": domains, "DomainCount": len(domains), "ArticleCount": len(articles),
			"SummaryURL": sumURL, "SummaryDomain": sumDom, "Orgs": orgs, "Persons": []string{},
			"editor_note": fmt.Sprintf("Found by the daily news safety check: %d outlets in 72 hours and no story. Judged about AI: %s. Written by %s and %s from the article texts.", len(domains), reason, model, smodel),
		}
		raw, _ := json.Marshal(st)
		pub := asOf.Format("2006-01-02")
		r, err := db.Exec(`INSERT INTO twoai_news_stories (slug, headline, story, published_on, first_published, last_seen, uid)
			VALUES ($1,$2,$3::jsonb,$4::date,now(),now(),$5) ON CONFLICT DO NOTHING`, slug, headline, string(raw), pub, uid)
		if err != nil {
			res.Stories = append(res.Stories, "story insert failed: "+trunc(err.Error(), 80))
			continue
		}
		if n, _ := r.RowsAffected(); n == 0 {
			res.Stories = append(res.Stories, fmt.Sprintf("already a story under slug %s", slug))
			continue
		}
		pinned := []string{}
		for _, e := range ents {
			cu := companies[e]
			if cu == "" {
				continue
			}
			db.Exec(`INSERT INTO twoai_page_news (page_uid, page_path, story_uid, story_slug, headline, published_on, placed_by, reason, active, added_at)
				SELECT $1, $2, $3, $4, $5, $6::date, 'news safety check', $7, true, now()
				WHERE NOT EXISTS (SELECT 1 FROM twoai_page_news WHERE page_uid=$1 AND story_uid=$3)`,
				cu, "companies/"+cu+".json", uid, slug, headline, pub, "Found by the daily news safety check (bridge row 415), attached automatically.")
			pinned = append(pinned, cu)
		}
		line := fmt.Sprintf("%s (story %s, %d outlets", headline, uid, len(domains))
		if len(pinned) > 0 {
			line += ", pinned to company " + strings.Join(pinned, ", ")
		}
		res.Stories = append(res.Stories, line+")")
		fmt.Printf("news_gap: built story %s: %s\n", uid, trunc(headline, 90))
	}
	if len(res.Stories) == 0 {
		res.Report = fmt.Sprintf("judged about AI (%s), but its articles are %d separate items, none carried by two outlets", reason, singles)
	}
	return res
}

// newsCompanyMoved mirrors twoai-site scripts/company-moves.json: AMD's SEC
// record 6b518abb is folded into b54d3e84 at build (bridge row 409).
var newsCompanyMoved = map[string]string{"6b518abb": "b54d3e84"}

// newsCompanyPages maps folded company names and aliases to uids whose
// company page exists, for pinning safety-check stories.
func newsCompanyPages(db *sql.DB) map[string]string {
	out := map[string]string{}
	rows, err := db.Query(`SELECT e.uid, e.name, COALESCE(e.aliases,'[]'::jsonb)::text FROM twoai_entities e
		WHERE e.kind='company' AND EXISTS (SELECT 1 FROM twoai_pages p WHERE p.path = 'companies/' || e.uid || '.json')`)
	if err != nil {
		return out
	}
	defer rows.Close()
	type row struct{ uid, name, aliases string }
	var all []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.uid, &r.name, &r.aliases) == nil {
			all = append(all, r)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].uid < all[j].uid })
	for _, r := range all {
		names := []string{r.name}
		var al []string
		if json.Unmarshal([]byte(r.aliases), &al) == nil {
			names = append(names, al...)
		}
		for _, n := range names {
			if k := newsEntNorm(n); k != "" {
				if _, ok := out[k]; !ok {
					out[k] = r.uid
				}
			}
		}
	}
	// A record retired into another (twoai-site scripts/company-moves.json)
	// pins to the record that stays.
	for k, u := range out {
		if to, ok := newsCompanyMoved[u]; ok {
			out[k] = to
		}
	}
	return out
}

var newsDupOfRe = regexp.MustCompile(`^duplicate of ([0-9a-f]{8})`)

// newsBundleChainStrip follows each one-outlet bundle along the stories it
// was merged into (the "duplicate of" chain on retired rows, which survives
// even when the merge log was overwritten) and takes the bundle's stray
// links, the articles that do not match its own headline, out of each of
// them. 2026-10-04: the Manila Times bundle 9a54c265 went into b8678de1,
// which was then merged into 2928478e, carrying six unrelated Manila Times
// pieces into a story about the "super intelligence" rebrand. Links are only
// taken from stories on the bundle's own chain, never from a story that
// carries the same article on its own merits.
func newsBundleChainStrip(db *sql.DB) {
	rows, err := db.Query(`SELECT uid, headline, story::text, COALESCE(retired_reason,'') FROM twoai_news_stories
		WHERE published_on > current_date - 21`)
	if err != nil {
		return
	}
	type st struct {
		head, reason string
		doc          map[string]any
	}
	all := map[string]*st{}
	for rows.Next() {
		var uid, head, raw, reason string
		if rows.Scan(&uid, &head, &raw, &reason) != nil {
			continue
		}
		var d map[string]any
		if json.Unmarshal([]byte(raw), &d) == nil {
			all[uid] = &st{head, reason, d}
		}
	}
	rows.Close()
	changed := map[string]bool{}
	for uid, b := range all {
		d := newsDedupeFrom(uid, "", b.head, b.doc)
		if !newsBundle(d.titles, d.domains) {
			continue
		}
		var dom string
		for x := range d.domains {
			dom = x
		}
		lead := newsTitleTokens(newsStripOutlet(b.head, dom))
		stray := map[string]bool{}
		arts, _ := b.doc["Articles"].([]any)
		for _, a := range arts {
			m, _ := a.(map[string]any)
			t, _ := m["Title"].(string)
			u, _ := m["URL"].(string)
			if u != "" && newsTokSim(lead, newsTitleTokens(newsStripOutlet(t, dom))) < 0.6 {
				stray[u] = true
			}
		}
		if len(stray) == 0 {
			continue
		}
		cur := uid
		for hop := 0; hop < 6; hop++ {
			m := newsDupOfRe.FindStringSubmatch(all[cur].reason)
			if m == nil || all[m[1]] == nil {
				break
			}
			next := all[m[1]]
			sarts, _ := next.doc["Articles"].([]any)
			keep := []any{}
			for _, a := range sarts {
				mm, _ := a.(map[string]any)
				if u, _ := mm["URL"].(string); stray[u] {
					continue
				}
				keep = append(keep, a)
			}
			if len(keep) < len(sarts) && len(keep) > 0 {
				next.doc["Articles"] = keep
				next.doc["ArticleCount"] = len(keep)
				changed[m[1]] = true
				fmt.Printf("news_bundle: %s loses %d stray links from bundle %s\n", m[1], len(sarts)-len(keep), uid)
			}
			cur = m[1]
		}
	}
	for uid := range changed {
		if b, err := json.Marshal(all[uid].doc); err == nil {
			db.Exec(`UPDATE twoai_news_stories SET story=$2::jsonb WHERE uid=$1`, uid, string(b))
		}
	}
}
