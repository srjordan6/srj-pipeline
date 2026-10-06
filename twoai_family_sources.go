package main

// twoai_family_sources: the developer's own website behind each model family
// page. theworldofai rows 493 and 494 (Stephen, 2026-10-05: "the model pages
// are still a little thin"), widened the same day ("it would be great if we
// harvested info from the developer itself", pointing at
// seed.bytedance.com).
//
// CRAWL. Each family has a short list of start pages on its developer's own
// site (famDevSites): the home page, the models page, the news or research
// page. From them the stage follows links on the same host and inside the
// same path scope, model, research, blog, news, docs and API pages first, at
// most famSrcPagesPerRun fetches a family a run, two seconds apart, never
// where robots.txt says not to. A page read in the last week is not fetched
// again (start pages: the last day), but its stored links still extend the
// crawl, so a site is covered over a few runs. Pages drawn by script are read
// through crawlFetchBrowser, inside the monthly allowance the site crawl
// keeps. Every page is kept in twoai_family_pages with its text hash.
//
// EXTRACT. A page whose text hash differs from the hash it was last read at
// is read by the model (stage name family_extract) for three things: the
// developer's model lines (name, modality, announcement date), where the
// developer says the models can be used (its API, cloud marketplaces), and
// dated announcements. Every name, title and date must appear verbatim in
// that page's text, a date must sit near the name it dates, and a name that
// is another developer's line (Claude in a Seed comparison table) is dropped.
// What survives goes to twoai_family_lines with the page it came from.
//
// EXPLAIN. "What it is", two or three paragraphs written by the model (stage
// name family_explainer) from the family's own pages only, rewritten only
// when the text of those pages changes. A number, or a capitalised name, that
// is not in the source text rejects the draft.
//
// The family builder (twoai_model_families, inside twoai_build) reads these
// tables and attaches the blocks to each family page (famSrcAttach).

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Both model-calling steps work through a backlog and lose nothing by
// waiting, so they wait for off-peak hours like every other bulk stage.
func init() {
	twoaiBulkStages["family_extract"] = true
	twoaiBulkStages["family_explainer"] = true
}

const (
	famSrcPagesPerRun   = 25  // fetches per family per run (the brief: about 25)
	famSrcMaxStored     = 120 // pages kept per family
	famSrcExtractPerRun = 60  // pages read by the model per run
	famSrcExplainPerRun = 10  // explainers written per run
	famSrcBrowserPerRun = 40  // browser reads per run, all families
	famSrcFreshDays     = 7   // a page is not fetched again inside this
	famSrcStartFresh    = 24 * time.Hour
	famSrcPace          = 2 * time.Second
	famSrcTextCap       = 40000
)

// famSite is where one family's developer publishes about it. Scopes are a
// host with an optional path prefix ("seed.bytedance.com/en"); a followed
// link must fall inside one of them. Hosts are written without www.
type famSite struct {
	Starts  []string
	Scopes  []string
	Browser bool // the whole site is drawn by script
}

// famDevSites maps a family key to its developer's own site. Every start
// page was fetched with the crawler's user agent on 2026-10-05 and answered
// 200, robots.txt permitting. A family with no entry gets no developer
// blocks; unbiased.ai, nex-agi.com and the rest were checked to name the
// family on the page before they were added.
var famDevSites = map[string]famSite{
	// Multimodal Models, the first ten.
	"bytedance-seed/seed": {Starts: []string{"https://seed.bytedance.com/en/", "https://seed.bytedance.com/en/models", "https://seed.bytedance.com/en/research"},
		Scopes: []string{"seed.bytedance.com/en"}},
	"google/gemma": {Starts: []string{"https://deepmind.google/models/gemma/", "https://ai.google.dev/gemma/docs"},
		Scopes: []string{"deepmind.google/models/gemma", "ai.google.dev/gemma"}},
	"google/lyria": {Starts: []string{"https://deepmind.google/models/lyria/", "https://ai.google.dev/gemini-api/docs/music-generation"},
		Scopes: []string{"deepmind.google/models/lyria", "ai.google.dev/gemini-api/docs/music-generation"}},
	"minimax/minimax": {Starts: []string{"https://www.minimax.io/", "https://www.minimax.io/news", "https://platform.minimax.io/docs/guides/models-intro"},
		Scopes: []string{"minimax.io", "platform.minimax.io/docs"}},
	"mistralai/mistral": {Starts: []string{"https://mistral.ai/", "https://mistral.ai/news", "https://docs.mistral.ai/getting-started/models/"},
		Scopes: []string{"mistral.ai", "docs.mistral.ai"}},
	"moonshotai/kimi": {Starts: []string{"https://www.moonshot.ai/", "https://platform.kimi.ai/docs/introduction"},
		Scopes: []string{"moonshot.ai", "platform.kimi.ai/docs"}},
	"nvidia/nemotron": {Starts: []string{"https://www.nvidia.com/en-us/ai-data-science/foundation-models/nemotron/", "https://developer.nvidia.com/topics/ai/nemotron"},
		Scopes: []string{"nvidia.com/en-us/ai-data-science/foundation-models", "developer.nvidia.com/topics/ai/nemotron", "developer.nvidia.com/nemotron"}},
	"openrouter/auto": {Starts: []string{"https://openrouter.ai/openrouter/auto", "https://openrouter.ai/docs/guides/routing/routers/auto-router"},
		Scopes: []string{"openrouter.ai/openrouter/auto", "openrouter.ai/docs/guides/routing"}},
	"stepfun/step": {Starts: []string{"https://platform.stepfun.ai/", "https://platform.stepfun.ai/docs/en/welcome"},
		Scopes: []string{"platform.stepfun.ai"}},
	"thinkingmachines/inkling": {Starts: []string{"https://thinkingmachines.ai/", "https://thinkingmachines.ai/blog/"},
		Scopes: []string{"thinkingmachines.ai"}},
	// Large Language Models.
	"aion-labs/aion": {Starts: []string{"https://www.aionlabs.ai/"}, Scopes: []string{"aionlabs.ai"}},
	"anthropic/claude": {Starts: []string{"https://www.anthropic.com/claude", "https://platform.claude.com/docs/en/models/overview", "https://www.anthropic.com/news"},
		Scopes: []string{"anthropic.com", "claude.com/product", "platform.claude.com/docs/en/models", "platform.claude.com/docs/en/about-claude"}},
	"cohere/command": {Starts: []string{"https://cohere.com/command", "https://docs.cohere.com/docs/models"},
		Scopes: []string{"cohere.com", "docs.cohere.com/docs"}},
	"inclusionai/ling":      {Starts: []string{"https://www.inclusion-ai.org/"}, Scopes: []string{"inclusion-ai.org"}},
	"openai/gpt":            {Starts: []string{"https://openai.com/api/", "https://developers.openai.com/api/docs/models", "https://openai.com/news/"}, Scopes: []string{"openai.com", "developers.openai.com/api/docs/models"}},
	"perceptron/perceptron": {Starts: []string{"https://www.perceptron.inc/"}, Scopes: []string{"perceptron.inc"}},
	"qwen/qwen":             {Starts: []string{"https://qwen.ai/", "https://qwen.ai/research"}, Scopes: []string{"qwen.ai"}, Browser: true},
	"unbiased/pareto":       {Starts: []string{"https://unbiased.ai/"}, Scopes: []string{"unbiased.ai"}},
	"upstage/solar":         {Starts: []string{"https://console.upstage.ai/docs/models", "https://www.upstage.ai/"}, Scopes: []string{"console.upstage.ai/docs", "upstage.ai"}},
	"z-ai/glm":              {Starts: []string{"https://docs.z.ai/guides/overview/overview"}, Scopes: []string{"docs.z.ai"}},
	// Reasoning Models.
	"deepseek/deepseek":   {Starts: []string{"https://www.deepseek.com/", "https://api-docs.deepseek.com/"}, Scopes: []string{"deepseek.com", "api-docs.deepseek.com"}},
	"google/gemini":       {Starts: []string{"https://deepmind.google/models/gemini/", "https://ai.google.dev/gemini-api/docs/models"}, Scopes: []string{"deepmind.google/models/gemini", "ai.google.dev/gemini-api/docs"}},
	"ibm-granite/granite": {Starts: []string{"https://www.ibm.com/granite", "https://www.ibm.com/granite/docs/models/granite/"}, Scopes: []string{"ibm.com/granite"}},
	"inception/mercury":   {Starts: []string{"https://www.inceptionlabs.ai/"}, Scopes: []string{"inceptionlabs.ai"}},
	"meta/muse":           {Starts: []string{"https://ai.meta.com/"}, Scopes: []string{"ai.meta.com"}, Browser: true},
	"nex-agi/nex":         {Starts: []string{"https://nex-agi.com/"}, Scopes: []string{"nex-agi.com"}},
	"sakana/fugu":         {Starts: []string{"https://sakana.ai/"}, Scopes: []string{"sakana.ai"}},
	"tencent/hy":          {Starts: []string{"https://hunyuan.tencent.com/"}, Scopes: []string{"hunyuan.tencent.com"}, Browser: true},
	"x-ai/grok":           {Starts: []string{"https://x.ai/grok", "https://docs.x.ai/developers/models", "https://x.ai/news"}, Scopes: []string{"x.ai", "docs.x.ai/developers"}},
	"xiaomi/mimo":         {Starts: []string{"https://mimo.xiaomi.com/"}, Scopes: []string{"mimo.xiaomi.com"}},
}

// famMakerAlias names the company record for developers whose catalog name
// is not the company's (the catalog's "ByteDance Seed" is ByteDance's Seed
// team, "MistralAI" is Mistral AI). Lower case, matched against
// twoai_entities names and aliases.
var famMakerAlias = map[string]string{
	"bytedance-seed":   "bytedance",
	"mistralai":        "mistral ai",
	"thinkingmachines": "thinking machines lab",
	"x-ai":             "xai",
	"moonshotai":       "moonshot ai",
}

func famSrcEnsure(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_family_pages (
		family_uid text NOT NULL, url text NOT NULL, req_url text, title text, text text,
		text_hash text, http_status int, via text, links jsonb,
		fetched_at timestamptz NOT NULL DEFAULT now(),
		extracted_hash text, extracted_at timestamptz, extract_attempts int NOT NULL DEFAULT 0,
		PRIMARY KEY (family_uid, url))`)
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_family_lines (
		family_uid text NOT NULL, kind text NOT NULL, name text NOT NULL, detail text,
		announced text, date_text text, url text, source_url text NOT NULL, source_hash text,
		extracted_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (family_uid, kind, name, source_url))`)
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_family_sources (
		family_uid text PRIMARY KEY, family_key text, crawled_at timestamptz, pages int,
		explainer text, explainer_model text, explainer_on date, explainer_hash text,
		explainer_sources jsonb, explainer_try_hash text, explainer_attempts int NOT NULL DEFAULT 0)`)
}

// ---------------------------------------------------------------- text

type famSrcLink struct {
	U string `json:"u"`
	T string `json:"t"`
}

var famSrcDropRes = func() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, t := range []string{"script", "style", "noscript", "svg", "iframe", "template"} {
		out = append(out, regexp.MustCompile(`(?is)<`+t+`\b[^>]*>.*?</`+t+`\s*>`))
	}
	return out
}()
var famSrcBlockRe = regexp.MustCompile(`(?i)</(p|div|li|h1|h2|h3|h4|h5|h6|td|th|tr|section|article|blockquote|a|button|span|dt|dd|figcaption|label)>|<br\s*/?>`)
var famSrcTagRe = regexp.MustCompile(`(?s)<[^>]*>`)
var famSrcTitleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// famSrcText turns a page into its title, its readable text one line per
// block (short lines kept: a model name is often a line of its own), and its
// links with their anchor text.
func famSrcText(raw []byte, base string) (string, string, []famSrcLink) {
	s := string(raw)
	title := ""
	if m := famSrcTitleRe.FindStringSubmatch(s); m != nil {
		title = famSrcPlain(m[1])
	}
	var links []famSrcLink
	bu, _ := url.Parse(base)
	seenL := map[string]bool{}
	for _, m := range crawlHrefRe.FindAllStringSubmatch(s, 800) {
		ref, err := url.Parse(strings.TrimSpace(html.UnescapeString(m[1])))
		if err != nil || bu == nil {
			continue
		}
		abs := bu.ResolveReference(ref)
		if abs.Scheme != "http" && abs.Scheme != "https" {
			continue
		}
		abs.Fragment = ""
		u := abs.String()
		t := famSrcPlain(m[2])
		if len(t) > 140 {
			t = trunc(t, 140)
		}
		k := u + "\x00" + t
		if seenL[k] {
			continue
		}
		seenL[k] = true
		links = append(links, famSrcLink{u, t})
	}
	for _, re := range famSrcDropRes {
		s = re.ReplaceAllString(s, " ")
	}
	s = famSrcBlockRe.ReplaceAllString(s, "\n")
	s = famSrcTagRe.ReplaceAllString(s, " ")
	var keep []string
	seen := map[string]bool{}
	total := 0
	for _, ln := range strings.Split(s, "\n") {
		ln = famSrcPlain(ln)
		if len(ln) < 2 || seen[ln] {
			continue
		}
		seen[ln] = true
		keep = append(keep, ln)
		total += len(ln) + 1
		if total > famSrcTextCap {
			break
		}
	}
	return title, strings.Join(keep, "\n"), links
}

func famSrcPlain(s string) string {
	s = famSrcTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

// famSrcNorm is the form verbatim checks compare in: typographic quotes and
// dashes made plain, case folded, whitespace collapsed.
func famSrcNorm(s string) string {
	r := strings.NewReplacer(string(rune(0x2019)), "'", string(rune(0x2018)), "'", string(rune(0x201c)), `"`, string(rune(0x201d)), `"`,
		string(rune(0x2013)), "-", string(rune(0x2014)), "-", string(rune(0x00a0)), " ", string(rune(0x2122)), "", string(rune(0x00ae)), "")
	return strings.Join(strings.Fields(strings.ToLower(r.Replace(s))), " ")
}

// famSrcVerbatim reports whether s appears in text, as normalised.
func famSrcVerbatim(text, s string) bool {
	n := famSrcNorm(s)
	return n != "" && strings.Contains(famSrcNorm(text), n)
}

// famSrcNear reports whether a and b both appear in text within window
// characters of each other.
func famSrcNear(text, a, b string, window int) bool {
	t, na, nb := famSrcNorm(text), famSrcNorm(a), famSrcNorm(b)
	if na == "" || nb == "" {
		return false
	}
	var pa, pb []int
	for i := 0; ; {
		j := strings.Index(t[i:], na)
		if j < 0 {
			break
		}
		pa = append(pa, i+j)
		i += j + 1
	}
	for i := 0; ; {
		j := strings.Index(t[i:], nb)
		if j < 0 {
			break
		}
		pb = append(pb, i+j)
		i += j + 1
	}
	for _, x := range pa {
		for _, y := range pb {
			d := x - y
			if d < 0 {
				d = -d
			}
			if d <= window {
				return true
			}
		}
	}
	return false
}

var famSrcOrdRe = regexp.MustCompile(`(?i)\b(\d{1,2})(st|nd|rd|th)\b`)
var famSrcCJKDate = regexp.MustCompile(`^(\d{4})\s*年\s*(\d{1,2})\s*月(?:\s*(\d{1,2})\s*日)?$`)

// famSrcDate reads a date as a page writes it into ISO form: a full date,
// a month (2026-06) or a year. Nothing in the future beyond a month.
func famSrcDate(s string) string {
	s = strings.TrimSpace(strings.Trim(s, ".,;"))
	if s == "" {
		return ""
	}
	s = famSrcOrdRe.ReplaceAllString(s, "$1")
	s = strings.NewReplacer("Sept.", "Sep", "Sept ", "Sep ", ".", " ", "  ", " ").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	ok := func(t time.Time) bool {
		return t.Year() >= 1990 && t.Before(time.Now().AddDate(0, 1, 0))
	}
	if m := famSrcCJKDate.FindStringSubmatch(s); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		if mo < 1 || mo > 12 {
			return ""
		}
		if m[3] != "" {
			d, _ := strconv.Atoi(m[3])
			t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
			if t.Day() == d && ok(t) {
				return t.Format("2006-01-02")
			}
			return ""
		}
		return fmt.Sprintf("%04d-%02d", y, mo)
	}
	for _, l := range []string{"2006-01-02", "2006/01/02", "2006 01 02", "2006/1/2", "2006-1-2", "Jan 2, 2006", "January 2, 2006", "Jan 2 2006", "January 2 2006",
		"2 Jan 2006", "2 January 2006", "Monday, January 2, 2006", "Mon, Jan 2, 2006"} {
		if t, err := time.Parse(l, s); err == nil && ok(t) {
			return t.Format("2006-01-02")
		}
	}
	for _, l := range []string{"January 2006", "Jan 2006", "2006-01", "2006/01"} {
		if t, err := time.Parse(l, s); err == nil && ok(t) {
			return t.Format("2006-01")
		}
	}
	if len(s) == 4 {
		if y, err := strconv.Atoi(s); err == nil && y >= 1990 && y <= time.Now().Year() {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------- crawl

type famSrcFamily struct {
	Key, UID, Category, Name, Line, Dev, Company string
	Site                                         famSite
	CrawledAt                                    time.Time
}

// famSrcMaker is the company the family's developer belongs to, for the
// prompts: the page's company record, else the alias, else the developer.
func famSrcMaker(f famSrcFamily) string {
	if f.Company != "" {
		return f.Company
	}
	return f.Dev
}

var famSrcLangRe = regexp.MustCompile(`(?i)^/(zh|zh-cn|zh-hans|zh-hant|zh-tw|cn|ja|jp|ko|kr|de|fr|es|pt|pt-br|it|ru|ar|tr|vi|id|th)(/|$)`)
var famSrcPriority = regexp.MustCompile(`(?i)(model|research|blog|news|press|announc|release|/docs?\b|/api\b|developer|platform|pricing|card|technical-report|paper|publication|changelog|update|introduc|launch)`)

func famSrcWithin(u string, scopes []string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	h := crawlHost(u)
	for _, sc := range scopes {
		host, prefix := crawlSplitScope(sc)
		if h == host && (prefix == "" || strings.HasPrefix(p.Path, prefix)) {
			return true
		}
	}
	return false
}

// famSrcKey is a page's crawl identity: no query, no fragment, no trailing
// slash. Developer sites tag the same page with tracking queries
// (?view_from=homepage_tab on seed.bytedance.com).
func famSrcKey(u string) string {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil {
		return ""
	}
	return strings.ToLower(p.Scheme) + "://" + strings.ToLower(strings.TrimPrefix(p.Host, "www.")) + strings.TrimSuffix(p.Path, "/")
}

func famSrcClean(u string) string {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil {
		return ""
	}
	p.RawQuery, p.Fragment = "", ""
	return p.String()
}

type famSrcStored struct {
	fetched time.Time
	links   []famSrcLink
}

type famSrcRun struct {
	stop                                    time.Time
	robots                                  map[string]robotsRules
	browserOK                               bool
	browserUsed, browserLeft                int
	fetched, extracted, facts, explained    int
	modelDeferred                           bool
	others                                  map[string]bool
	plain, browser                          *http.Client
	lineOwners                              map[string]map[string]bool
	extractBudget, explainBudget            int
	familiesCrawled, unchanged, failedFetch int
}

// famSrcCrawl walks one family's developer site for this run.
func famSrcCrawl(db *sql.DB, run *famSrcRun, f famSrcFamily) int {
	stored := map[string]famSrcStored{}
	if rows, err := db.Query(`SELECT url, coalesce(req_url,''), fetched_at, coalesce(links,'[]'::jsonb)::text FROM twoai_family_pages WHERE family_uid=$1`, f.UID); err == nil {
		for rows.Next() {
			var u, req, lr string
			var at time.Time
			if rows.Scan(&u, &req, &at, &lr) != nil {
				continue
			}
			var ls []famSrcLink
			json.Unmarshal([]byte(lr), &ls)
			st := famSrcStored{at, ls}
			stored[famSrcKey(u)] = st
			if req != "" {
				stored[famSrcKey(req)] = st
			}
		}
		rows.Close()
	}
	nStored := 0
	db.QueryRow(`SELECT count(*) FROM twoai_family_pages WHERE family_uid=$1`, f.UID).Scan(&nStored)
	isStart := map[string]bool{}
	for _, s := range f.Site.Starts {
		isStart[famSrcKey(s)] = true
	}
	lineLow := strings.ToLower(f.Line)
	type cand struct {
		u     string
		score int
	}
	var queue []cand
	queued := map[string]bool{}
	push := func(raw, anchor string, base int) {
		u := famSrcClean(raw)
		k := famSrcKey(u)
		if u == "" || k == "" || queued[k] || !famSrcWithin(u, f.Site.Scopes) {
			return
		}
		p, _ := url.Parse(u)
		if crawlSkipExt.MatchString(p.Path) || crawlSkipPath.MatchString(p.Path+" ") {
			return
		}
		if famSrcLangRe.MatchString(p.Path) && !isStart[k] {
			return
		}
		queued[k] = true
		sc := base - strings.Count(strings.Trim(p.Path, "/"), "/")
		if famSrcPriority.MatchString(p.Path) {
			sc += 20
		}
		if lineLow != "" && (strings.Contains(strings.ToLower(p.Path), lineLow) || strings.Contains(strings.ToLower(anchor), lineLow)) {
			sc += 30
		}
		queue = append(queue, cand{u, sc})
	}
	for i, s := range f.Site.Starts {
		push(s, "", 1000-i)
	}
	famStop := time.Now().Add(5 * time.Minute)
	if famStop.After(run.stop) {
		famStop = run.stop
	}
	fetches, visits := 0, 0
	for len(queue) > 0 && fetches < famSrcPagesPerRun && visits < 300 && time.Now().Before(famStop) {
		sort.SliceStable(queue, func(i, j int) bool { return queue[i].score > queue[j].score })
		c := queue[0]
		queue = queue[1:]
		visits++
		k := famSrcKey(c.u)
		if st, ok := stored[k]; ok {
			fresh := time.Duration(famSrcFreshDays) * 24 * time.Hour
			if isStart[k] {
				fresh = famSrcStartFresh
			}
			if time.Since(st.fetched) < fresh {
				for _, l := range st.links {
					push(l.U, l.T, 0)
				}
				continue
			}
		} else if nStored >= famSrcMaxStored {
			continue
		}
		p, _ := url.Parse(c.u)
		root := p.Scheme + "://" + p.Host
		rules, ok := run.robots[root]
		if !ok {
			rules = crawlRobots(run.plain, root)
			run.robots[root] = rules
		}
		if !rules.allowed(p.EscapedPath()) {
			continue
		}
		var st int
		var body []byte
		var final, via string
		var err error
		if f.Site.Browser && run.browserOK && run.browserLeft > 0 {
			st, body, final, err = crawlFetchBrowser(run.browser, c.u)
			via = "browser"
			run.browserLeft--
			run.browserUsed++
		} else {
			st, body, final, err = crawlFetch(run.plain, c.u)
			via = "plain"
		}
		fetches++
		time.Sleep(famSrcPace)
		if err == nil && via == "plain" && st == 200 && run.browserOK && run.browserLeft > 0 {
			// A script shell: a page whose readable text is a title and a menu.
			if _, t, _ := famSrcText(body, final); len(t) < 400 {
				if st2, b2, f2, err2 := crawlFetchBrowser(run.browser, c.u); err2 == nil && st2 == 200 {
					st, body, final, via = st2, b2, f2, "browser"
				} else if err2 != nil && strings.Contains(err2.Error(), "not set") {
					run.browserOK = false
				}
				run.browserLeft--
				run.browserUsed++
				time.Sleep(famSrcPace)
			}
		}
		if err != nil {
			if via == "browser" && strings.Contains(err.Error(), "not set") {
				run.browserOK = false
			}
			run.failedFetch++
			continue
		}
		if final == "" {
			final = c.u
		}
		final = famSrcClean(final)
		if !famSrcWithin(final, f.Site.Scopes) {
			continue
		}
		title, text, links := famSrcText(body, final)
		if sourceErrorPage.MatchString(trunc(text, 1500)) && len(text) < 3000 {
			st = 404
		}
		h := sha256.Sum256([]byte(text))
		hash := hex.EncodeToString(h[:8])
		lj, _ := json.Marshal(links)
		var old string
		db.QueryRow(`SELECT coalesce(text_hash,'') FROM twoai_family_pages WHERE family_uid=$1 AND url=$2`, f.UID, final).Scan(&old)
		if old == hash {
			run.unchanged++
		}
		if _, err := db.Exec(`INSERT INTO twoai_family_pages (family_uid, url, req_url, title, text, text_hash, http_status, via, links, fetched_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,now())
			ON CONFLICT (family_uid, url) DO UPDATE SET req_url=$3, title=$4, text=$5, text_hash=$6, http_status=$7, via=$8, links=$9::jsonb, fetched_at=now()`,
			f.UID, final, c.u, title, text, hash, st, via, string(lj)); err == nil {
			if _, had := stored[famSrcKey(final)]; !had {
				nStored++
			}
			stored[famSrcKey(final)] = famSrcStored{time.Now(), links}
			stored[k] = stored[famSrcKey(final)]
		}
		run.fetched++
		if st != 200 {
			continue
		}
		for _, l := range links {
			push(l.U, l.T, 0)
		}
	}
	db.Exec(`INSERT INTO twoai_family_sources (family_uid, family_key, crawled_at, pages) VALUES ($1,$2,now(),$3)
		ON CONFLICT (family_uid) DO UPDATE SET family_key=$2, crawled_at=now(), pages=$3`, f.UID, f.Key, nStored)
	return fetches
}

// ---------------------------------------------------------------- extract

type famSrcExtract struct {
	Lines []struct {
		Name     string `json:"name"`
		Modality string `json:"modality"`
		DateText string `json:"date_text"`
	} `json:"lines"`
	Access []struct {
		Channel string `json:"channel"`
		Kind    string `json:"kind"`
	} `json:"access"`
	Announcements []struct {
		Title    string `json:"title"`
		DateText string `json:"date_text"`
	} `json:"announcements"`
}

type famSrcFact struct {
	Kind, Name, Detail, Announced, DateText, URL string
}

var famSrcModalities = map[string]string{
	"text": "text", "language": "text", "llm": "text", "image": "image", "images": "image", "vision": "image",
	"video": "video", "audio": "audio", "sound": "audio", "speech": "speech", "voice": "speech", "music": "music",
	"3d": "3D", "multimodal": "multimodal", "omni": "multimodal", "code": "code", "coding": "code",
	"embedding": "embedding", "embeddings": "embedding", "robotics": "robotics", "science": "science",
}

var famSrcButtonRe = regexp.MustCompile(`(?i)^(get|try|learn|start|sign|view|read|see|explore|contact|join|click|book|request|chat)\b`)

var famSrcAccessKinds = map[string]bool{"first-party API": true, "cloud marketplace": true, "partner platform": true, "app": true, "open weights": true}

const famSrcExtractSystem = `You read one page from an AI developer's own website and list what the page itself states. You are told which model family the page is being read for and who the developer is. Return only JSON:
{"lines":[{"name":"","modality":"","date_text":""}],"access":[{"channel":"","kind":""}],"announcements":[{"title":"","date_text":""}]}
lines: every model or model line the page presents as the developer's own, such as a model family, a named model or a version, including research models and models not sold through any API. Leave out models from other companies that the page compares against, builds on or hosts. name: exactly as written on the page. modality: one of text, image, video, audio, speech, music, 3d, multimodal, code, embedding, robotics, science, or "" when the page does not make it clear. date_text: the date the page gives for that model's release or announcement, copied exactly as written, or "" when the page gives none.
access: the ways the page says the developer's models can be used: the developer's own API or platform, cloud marketplaces and partner platforms (for example Amazon Bedrock, Google Cloud Vertex AI, Microsoft Azure, BytePlus), apps, open-weight downloads. channel: exactly as written on the page. kind: one of first-party API, cloud marketplace, partner platform, app, open weights.
announcements: dated news, blog or research items from the developer listed on the page: title exactly as written and date_text exactly as written. Leave out items the page gives no date for.
Copy names, titles and dates character for character. Never add anything the page does not say. Empty lists are fine.`

// famSrcLinkFor finds the page's own link for a name: an anchor that is the
// name, or that carries it with little else. Falls back to the page itself.
func famSrcLinkFor(name string, links []famSrcLink, pageURL string) string {
	n := famSrcNorm(name)
	best := ""
	for _, l := range links {
		a := famSrcNorm(l.T)
		if a == n {
			return famSrcClean(l.U)
		}
		// A card link that wraps the title, its blurb and its date starts
		// with the title.
		if best == "" && len(n) >= 4 && (strings.HasPrefix(a, n) || strings.Contains(a, n) && len(a) <= len(n)+40) {
			best = famSrcClean(l.U)
		}
	}
	if best != "" {
		return best
	}
	return pageURL
}

// famSrcFirstWord is the leading run of letters of a name, lower case:
// "Claude Opus 4.7" gives claude, "GPT-5.5" gives gpt.
func famSrcFirstWord(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if !unicode.IsLetter(r) {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// famSrcVerify keeps what the page text supports. own is the set of first
// words that belong to this developer (its lines and its names); others are
// the lines of every other developer in the catalog.
func famSrcVerify(ex famSrcExtract, text, pageTitle, pageURL string, links []famSrcLink, own, others map[string]bool) []famSrcFact {
	var out []famSrcFact
	seen := map[string]bool{}
	add := func(f famSrcFact) {
		k := f.Kind + "\x00" + famSrcNorm(f.Name)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, f)
	}
	dated := func(name, dateText string) (string, string) {
		dateText = strings.TrimSpace(dateText)
		if dateText == "" || !famSrcVerbatim(text, dateText) || !famSrcNear(text, name, dateText, 600) {
			return "", ""
		}
		iso := famSrcDate(dateText)
		if iso == "" {
			return "", ""
		}
		return iso, dateText
	}
	for _, l := range ex.Lines {
		name := strings.TrimSpace(l.Name)
		if len(name) < 2 || len(name) > 60 || !famSrcVerbatim(text, name) {
			continue
		}
		if w := famSrcFirstWord(name); w != "" && others[w] && !own[w] {
			continue
		}
		iso, dt := dated(name, l.DateText)
		add(famSrcFact{Kind: "line", Name: name, Detail: famSrcModalities[strings.ToLower(strings.TrimSpace(l.Modality))],
			Announced: iso, DateText: dt, URL: famSrcLinkFor(name, links, pageURL)})
	}
	for _, a := range ex.Access {
		ch := strings.TrimSpace(a.Channel)
		kind := strings.TrimSpace(a.Kind)
		// A button is not a channel: "Get API", "Try now".
		if len(ch) < 3 || len(ch) > 80 || !famSrcVerbatim(text, ch) || !famSrcAccessKinds[kind] || famSrcButtonRe.MatchString(ch) {
			continue
		}
		add(famSrcFact{Kind: "access", Name: ch, Detail: kind, URL: famSrcLinkFor(ch, links, pageURL)})
	}
	for _, a := range ex.Announcements {
		title := strings.TrimSpace(a.Title)
		if len(title) < 12 || len(title) > 240 || !famSrcVerbatim(text, title) {
			continue
		}
		iso, dt := dated(title, a.DateText)
		if iso == "" {
			continue
		}
		// The item's own link when the page carries one, else the listing
		// page itself, which states the title and the date.
		u := famSrcLinkFor(title, links, pageURL)
		add(famSrcFact{Kind: "announcement", Name: title, Announced: iso, DateText: dt, URL: u})
	}
	return out
}

// famSrcParseJSON pulls the JSON object out of a model reply.
func famSrcParseJSON(out string, v any) bool {
	t := strings.TrimSpace(out)
	if i := strings.Index(t, "{"); i >= 0 {
		t = t[i:]
	}
	if k := strings.LastIndex(t, "}"); k >= 0 {
		t = t[:k+1]
	}
	return json.Unmarshal([]byte(t), v) == nil
}

// famSrcExtractFamily reads the family's changed pages through the model.
func famSrcExtractFamily(db *sql.DB, run *famSrcRun, f famSrcFamily) {
	type pg struct {
		url, title, text, hash, links string
	}
	var pages []pg
	rows, err := db.Query(`SELECT url, coalesce(title,''), coalesce(text,''), coalesce(text_hash,''), coalesce(links,'[]'::jsonb)::text
		FROM twoai_family_pages WHERE family_uid=$1 AND http_status=200 AND length(coalesce(text,'')) > 200
		  AND coalesce(extracted_hash,'') <> coalesce(text_hash,'') AND extract_attempts < 3
		ORDER BY (url ~* '(model|news|blog|research|release|docs)') DESC, fetched_at`, f.UID)
	if err != nil {
		return
	}
	for rows.Next() {
		var p pg
		if rows.Scan(&p.url, &p.title, &p.text, &p.hash, &p.links) == nil {
			pages = append(pages, p)
		}
	}
	rows.Close()
	own := run.lineOwners[strings.SplitN(f.Key, "/", 2)[0]]
	for _, p := range pages {
		if run.modelDeferred || run.extractBudget <= 0 || time.Now().After(run.stop) {
			return
		}
		run.extractBudget--
		user := fmt.Sprintf("Model family: %s (line %s), developer %s.\nPage: %s\nPage title: %s\n\nPage text:\n%s",
			f.Name, f.Line, famSrcMaker(f), p.url, p.title, trunc(p.text, 24000))
		out, _, err := twoaiGenerate("family_extract", famSrcExtractSystem, user)
		if err != nil {
			if strings.Contains(err.Error(), "deferred") {
				run.modelDeferred = true
				return
			}
			db.Exec(`UPDATE twoai_family_pages SET extract_attempts = extract_attempts + 1 WHERE family_uid=$1 AND url=$2`, f.UID, p.url)
			continue
		}
		var ex famSrcExtract
		if !famSrcParseJSON(out, &ex) {
			db.Exec(`UPDATE twoai_family_pages SET extract_attempts = extract_attempts + 1 WHERE family_uid=$1 AND url=$2`, f.UID, p.url)
			continue
		}
		var links []famSrcLink
		json.Unmarshal([]byte(p.links), &links)
		facts := famSrcVerify(ex, p.text, p.title, p.url, links, own, run.others)
		tx, err := db.Begin()
		if err != nil {
			continue
		}
		tx.Exec(`DELETE FROM twoai_family_lines WHERE family_uid=$1 AND source_url=$2`, f.UID, p.url)
		for _, x := range facts {
			tx.Exec(`INSERT INTO twoai_family_lines (family_uid, kind, name, detail, announced, date_text, url, source_url, source_hash)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, f.UID, x.Kind, x.Name, x.Detail, x.Announced, x.DateText, x.URL, p.url, p.hash)
		}
		tx.Exec(`UPDATE twoai_family_pages SET extracted_hash=$3, extracted_at=now(), extract_attempts=0 WHERE family_uid=$1 AND url=$2`, f.UID, p.url, p.hash)
		if tx.Commit() == nil {
			run.extracted++
			run.facts += len(facts)
		}
	}
}

// ---------------------------------------------------------------- explain

var famSrcTokRe = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9.+\-']*[A-Za-z0-9]|[A-Za-z0-9]`)

// famSrcHasToken reports whether tok appears in lower-cased src with no
// letter or digit directly either side.
func famSrcHasToken(src, tok string) bool {
	for i := 0; ; {
		j := strings.Index(src[i:], tok)
		if j < 0 {
			return false
		}
		a, b := i+j, i+j+len(tok)
		before := a == 0 || !sectionIsWordByte(src[a-1])
		after := b == len(src) || !sectionIsWordByte(src[b])
		if before && after {
			return true
		}
		i = a + 1
	}
}

// famSrcStarters are ordinary words a sentence may open with.
var famSrcStarters = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`the it its this these those that there they their alongside both each all as at by for from in on
		with while unlike like a an and but also beyond besides together other another today according among across after before
		since through under over when where which who what some many most one two three four several users developers businesses
		access available further finally first second third here such instead rather because although though if unless
		within without beside behind along aimed designed built released based its every either neither no not only`) {
		m[w] = true
	}
	return m
}()

// famSrcSentenceStart reports whether the word at i opens a sentence.
func famSrcSentenceStart(s string, i int) bool {
	j := i - 1
	for j >= 0 && (s[j] == ' ' || s[j] == '"' || s[j] == '(') {
		j--
	}
	return j < 0 || s[j] == '.' || s[j] == '!' || s[j] == '\n' || s[j] == ':'
}

// famSrcCheckExplainer rejects a draft that carries a number or a
// capitalised name the source text does not, or breaks the house form.
// allowed are names that come from our own records (the family, the
// developer, the company).
func famSrcCheckExplainer(paras []string, src string, allowed []string) error {
	if len(paras) < 2 || len(paras) > 3 {
		return fmt.Errorf("%d paragraphs", len(paras))
	}
	all := strings.Join(paras, "\n")
	if strings.ContainsAny(all, string(rune(0x2014))+"?") || strings.Contains(all, "**") || strings.Contains(all, "#") {
		return fmt.Errorf("form")
	}
	if n := len(strings.Fields(all)); n < 100 || n > 380 {
		return fmt.Errorf("%d words", n)
	}
	s := famSrcNorm(src)
	ok := map[string]bool{}
	for _, a := range allowed {
		for _, w := range famSrcTokRe.FindAllString(famSrcNorm(a), -1) {
			ok[w] = true
		}
	}
	// One pass over every word: a word with a digit in it (30, 2.1,
	// Seed2.1, 30-second) or a capital letter must be in the sources as a
	// whole word, so a version or a name cannot be stretched or invented.
	for _, ix := range famSrcTokRe.FindAllStringIndex(all, -1) {
		t := all[ix[0]:ix[1]]
		first := []rune(t)[0]
		hasDigit := strings.IndexFunc(t, unicode.IsDigit) >= 0
		if !unicode.IsUpper(first) && !hasDigit {
			continue
		}
		lt := famSrcNorm(t)
		lt = strings.TrimSuffix(lt, "'s")
		if ok[lt] || famSrcHasToken(s, lt) {
			continue
		}
		// An ordinary word that opens a sentence is capitalised by grammar,
		// not because it names something.
		if !hasDigit && famSrcSentenceStart(all, ix[0]) && famSrcStarters[lt] {
			continue
		}
		return fmt.Errorf("%q not in the sources", t)
	}
	return nil
}

func famSrcExplainSystem(name, maker string) string {
	return fmt.Sprintf(`You write for The World of AI, a reference site. You are given pages from the website of %s, the developer of the %s model family. Write the section "What it is": two or three short paragraphs that explain the %s family to a reader who has not met it: what the models are, what the developer says they are for, how the family is organised (sizes, versions, related lines) and how the developer offers them. Use only what the pages state. Every number, version, product and company name you use must appear in the pages exactly as written. Attribute claims of quality to the developer ("%s says"). No marketing adjectives the pages do not use, no comparisons with other companies, no predictions, no questions, no lists, no headings, no markdown. Plain English, commas rather than dashes. 120 to 320 words in all.
Return only JSON: {"paragraphs": ["...", "..."]}`, maker, name, name, maker)
}

func famSrcExplainFamily(db *sql.DB, run *famSrcRun, f famSrcFamily) {
	type pg struct {
		url, title, text, hash string
		score                  int
	}
	var pages []pg
	rows, err := db.Query(`SELECT url, coalesce(title,''), coalesce(text,''), coalesce(text_hash,'') FROM twoai_family_pages
		WHERE family_uid=$1 AND http_status=200 AND length(coalesce(text,'')) > 300`, f.UID)
	if err != nil {
		return
	}
	isStart := map[string]bool{}
	for _, s := range f.Site.Starts {
		isStart[famSrcKey(s)] = true
	}
	line := strings.ToLower(f.Line)
	for rows.Next() {
		var p pg
		if rows.Scan(&p.url, &p.title, &p.text, &p.hash) != nil {
			continue
		}
		lt := strings.ToLower(p.title + "\n" + p.text)
		p.score = strings.Count(lt, line)
		if isStart[famSrcKey(p.url)] {
			p.score += 5
		}
		if p.score > 0 {
			pages = append(pages, p)
		}
	}
	rows.Close()
	if len(pages) == 0 {
		return
	}
	sort.SliceStable(pages, func(i, j int) bool {
		if pages[i].score != pages[j].score {
			return pages[i].score > pages[j].score
		}
		return pages[i].url < pages[j].url
	})
	var sb strings.Builder
	var used []map[string]string
	hashes := []string{}
	total := 0
	for _, p := range pages {
		if len(used) >= 8 || total > 24000 {
			break
		}
		t := trunc(p.text, 6000)
		total += len(t)
		fmt.Fprintf(&sb, "\n\n=== Page: %s\nTitle: %s\n%s", p.url, p.title, t)
		title := p.title
		if title == "" {
			title = p.url
		}
		used = append(used, map[string]string{"url": p.url, "title": title})
		hashes = append(hashes, p.url+"="+p.hash)
	}
	sort.Strings(hashes)
	h := sha256.Sum256([]byte(strings.Join(hashes, "\n")))
	hash := hex.EncodeToString(h[:8])
	var oldHash, tryHash string
	var attempts int
	db.QueryRow(`SELECT coalesce(explainer_hash,''), coalesce(explainer_try_hash,''), explainer_attempts FROM twoai_family_sources WHERE family_uid=$1`, f.UID).Scan(&oldHash, &tryHash, &attempts)
	if oldHash == hash || (tryHash == hash && attempts >= 3) {
		return
	}
	if run.modelDeferred || run.explainBudget <= 0 || time.Now().After(run.stop) {
		return
	}
	run.explainBudget--
	maker := famSrcMaker(f)
	out, model, err := twoaiGenerate("family_explainer", famSrcExplainSystem(f.Name, maker), fmt.Sprintf("Family: %s, developer %s.%s", f.Name, maker, sb.String()))
	fail := func(why string) {
		fmt.Printf("twoai_family_sources: explainer for %s held (%s)\n", f.Name, why)
		if tryHash != hash {
			attempts = 0
		}
		db.Exec(`UPDATE twoai_family_sources SET explainer_try_hash=$2, explainer_attempts=$3 WHERE family_uid=$1`, f.UID, hash, attempts+1)
	}
	if err != nil {
		if strings.Contains(err.Error(), "deferred") {
			run.modelDeferred = true
			return
		}
		fail(err.Error())
		return
	}
	var got struct {
		Paragraphs []string `json:"paragraphs"`
	}
	if !famSrcParseJSON(out, &got) {
		fail("not JSON")
		return
	}
	var paras []string
	for _, p := range got.Paragraphs {
		if p = strings.TrimSpace(twoaiStripMarkdown(p)); p != "" {
			paras = append(paras, p)
		}
	}
	if err := famSrcCheckExplainer(paras, sb.String(), []string{f.Name, f.Line, f.Dev, maker}); err != nil {
		fail(err.Error())
		return
	}
	sj, _ := json.Marshal(used)
	db.Exec(`UPDATE twoai_family_sources SET explainer=$2, explainer_model=$3, explainer_on=current_date, explainer_hash=$4, explainer_sources=$5::jsonb,
		explainer_try_hash=$4, explainer_attempts=0 WHERE family_uid=$1`, f.UID, strings.Join(paras, "\n\n"), model, hash, string(sj))
	run.explained++
}

// ---------------------------------------------------------------- stage

// famSrcFamilies lists the built family pages with a mapped developer site,
// Multimodal Models first, then the rest, least recently crawled first.
func famSrcFamilies(db *sql.DB) []famSrcFamily {
	rows, err := db.Query(`SELECT f.family_key, f.uid, f.category, coalesce(p.data->>'name', f.name), coalesce(p.data->>'line',''), coalesce(p.data->>'developer',''),
			coalesce(p.data->'developer_company'->>'name',''), coalesce(s.crawled_at, 'epoch'::timestamptz)
		FROM twoai_model_families f JOIN twoai_pages p ON p.path = 'tech/family-' || f.uid || '.json'
		LEFT JOIN twoai_family_sources s ON s.family_uid = f.uid
		WHERE f.page_built_on IS NOT NULL`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_family_sources:", err)
		return nil
	}
	defer rows.Close()
	var out []famSrcFamily
	for rows.Next() {
		var f famSrcFamily
		if rows.Scan(&f.Key, &f.UID, &f.Category, &f.Name, &f.Line, &f.Dev, &f.Company, &f.CrawledAt) != nil {
			continue
		}
		site, ok := famDevSites[f.Key]
		if !ok {
			continue
		}
		f.Site = site
		out = append(out, f)
	}
	rank := func(c string) int {
		switch c {
		case "multimodal-models":
			return 0
		case "llms":
			return 1
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank(out[i].Category) != rank(out[j].Category) {
			return rank(out[i].Category) < rank(out[j].Category)
		}
		return out[i].CrawledAt.Before(out[j].CrawledAt)
	})
	return out
}

// twoaiFamilySources is the stage: crawl, extract, explain, family by family.
func twoaiFamilySources(db *sql.DB) {
	famSrcEnsure(db)
	minutes := 26
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TWOAI_FAMILY_SOURCES_MINUTES"))); err == nil && v > 0 {
		minutes = v
	}
	run := &famSrcRun{stop: time.Now().Add(time.Duration(minutes) * time.Minute), robots: map[string]robotsRules{},
		plain: &http.Client{Timeout: 20 * time.Second}, browser: &http.Client{Timeout: 60 * time.Second},
		extractBudget: famSrcExtractPerRun, explainBudget: famSrcExplainPerRun, others: map[string]bool{}, lineOwners: map[string]map[string]bool{}}
	run.browserOK = os.Getenv("CLOUDFLARE_ACCOUNT_ID") != "" && (os.Getenv("CLOUDFLARE_BROWSER_TOKEN") != "" || os.Getenv("CLOUDFLARE_API_TOKEN") != "")
	var monthUsed int
	db.QueryRow(`SELECT (SELECT count(*) FROM twoai_family_pages WHERE via='browser' AND fetched_at >= date_trunc('month', now()))
		+ (SELECT count(*) FROM twoai_site_crawl_pages WHERE fetched_via='browser' AND fetched_on >= date_trunc('month', now())::date)`).Scan(&monthUsed)
	run.browserLeft = min(famSrcBrowserPerRun, crawlBrowserMonthlyPages-monthUsed)
	// Whose line is whose: a name on Seed's page that starts with another
	// developer's line (Claude, GPT, Gemini) is that developer's model.
	for _, g := range famLoad(db) {
		lw := strings.ToLower(g.LineName)
		if lw == "" {
			continue
		}
		if run.lineOwners[g.Dev] == nil {
			run.lineOwners[g.Dev] = map[string]bool{}
		}
		run.lineOwners[g.Dev][lw] = true
		for _, w := range strings.Fields(strings.ToLower(g.DevName)) {
			run.lineOwners[g.Dev][famSrcFirstWord(w)] = true
		}
	}
	fams := famSrcFamilies(db)
	for _, f := range fams {
		if time.Now().After(run.stop) {
			break
		}
		// others is rebuilt per developer, so the family's own words are
		// never another's: Google's Gemma pages may name Gemini.
		dev := strings.SplitN(f.Key, "/", 2)[0]
		run.others = map[string]bool{}
		for d, ws := range run.lineOwners {
			if d == dev {
				continue
			}
			for w := range ws {
				if !run.lineOwners[dev][w] {
					run.others[w] = true
				}
			}
		}
		n := famSrcCrawl(db, run, f)
		run.familiesCrawled++
		if n > 0 {
			fmt.Printf("twoai_family_sources: %s fetched=%d\n", f.Name, n)
		}
		famSrcExtractFamily(db, run, f)
		famSrcExplainFamily(db, run, f)
	}
	fmt.Printf("twoai_family_sources: families=%d fetched=%d unchanged=%d failed=%d browser=%d extracted=%d facts=%d explainers=%d deferred=%v ok=true\n",
		run.familiesCrawled, run.fetched, run.unchanged, run.failedFetch, run.browserUsed, run.extracted, run.facts, run.explained, run.modelDeferred)
}

// ---------------------------------------------------------------- attach

var famSrcCardRe = regexp.MustCompile(`(?i)\b(model card|system card|safety card|safety report|safety framework|technical report|tech report|responsible ai|usage policy|acceptable use|licen[cs]e|terms of use|prohibited use)\b`)

// famSrcAttach puts the developer blocks on a family page document.
func famSrcAttach(db *sql.DB, uid string, doc map[string]any) {
	// Model lines, merged across pages: one row per name, the dated reading
	// and the developer's own link preferred.
	type line struct {
		Name, Modality, Announced, URL string
		Sources                        map[string]bool
	}
	lines := map[string]*line{}
	var order []string
	access := map[string]map[string]any{}
	var accessOrder []string
	news := map[string]map[string]any{}
	if rows, err := db.Query(`SELECT kind, name, coalesce(detail,''), coalesce(announced,''), coalesce(url,''), source_url
		FROM twoai_family_lines WHERE family_uid=$1 ORDER BY extracted_at, source_url`, uid); err == nil {
		for rows.Next() {
			var kind, name, detail, ann, u, src string
			if rows.Scan(&kind, &name, &detail, &ann, &u, &src) != nil {
				continue
			}
			k := famSrcNorm(name)
			switch kind {
			case "line":
				l := lines[k]
				if l == nil {
					l = &line{Name: name, Sources: map[string]bool{}}
					lines[k] = l
					order = append(order, k)
				}
				if l.Modality == "" {
					l.Modality = detail
				}
				if ann != "" && (l.Announced == "" || ann < l.Announced) {
					l.Announced = ann
				}
				if (l.URL == "" || l.URL == src) && u != "" {
					l.URL = u
				}
				l.Sources[src] = true
			case "access":
				if _, ok := access[k]; !ok {
					access[k] = map[string]any{"channel": name, "kind": detail, "url": u, "source_url": src}
					accessOrder = append(accessOrder, k)
				}
			case "announcement":
				if _, ok := news[k]; !ok {
					news[k] = map[string]any{"title": name, "date": ann, "url": u, "source_url": src}
				}
			}
		}
		rows.Close()
	}
	devLines := []map[string]any{}
	for _, k := range order {
		l := lines[k]
		srcs := []string{}
		for s := range l.Sources {
			srcs = append(srcs, s)
		}
		sort.Strings(srcs)
		devLines = append(devLines, map[string]any{"name": l.Name, "modality": l.Modality, "announced": l.Announced, "url": l.URL, "sources": srcs})
	}
	sort.SliceStable(devLines, func(i, j int) bool {
		a, b := devLines[i]["announced"].(string), devLines[j]["announced"].(string)
		if a != b {
			return a > b
		}
		return devLines[i]["name"].(string) < devLines[j]["name"].(string)
	})
	if len(devLines) > 40 {
		devLines = devLines[:40]
	}
	doc["dev_lines"] = devLines
	acc := []map[string]any{}
	for _, k := range accessOrder {
		acc = append(acc, access[k])
	}
	doc["access"] = acc
	dn := []map[string]any{}
	for _, v := range news {
		dn = append(dn, v)
	}
	sort.SliceStable(dn, func(i, j int) bool { return dn[i]["date"].(string) > dn[j]["date"].(string) })
	if len(dn) > 5 {
		dn = dn[:5]
	}
	doc["dev_news"] = dn

	// Card, licence and policy links, as the developer's own pages label them.
	cards := []map[string]string{}
	seenCard := map[string]bool{}
	pages := 0
	if rows, err := db.Query(`SELECT url, coalesce(title,''), coalesce(links,'[]'::jsonb)::text FROM twoai_family_pages
		WHERE family_uid=$1 AND http_status=200 ORDER BY url`, uid); err == nil {
		for rows.Next() {
			var u, title, lr string
			if rows.Scan(&u, &title, &lr) != nil {
				continue
			}
			pages++
			var ls []famSrcLink
			json.Unmarshal([]byte(lr), &ls)
			for _, l := range ls {
				if len(l.T) > 80 || !famSrcCardRe.MatchString(l.T) || seenCard[famSrcKey(l.U)] {
					continue
				}
				seenCard[famSrcKey(l.U)] = true
				cards = append(cards, map[string]string{"label": l.T, "url": l.U, "on_page": title, "source_url": u})
			}
		}
		rows.Close()
	}
	if len(cards) > 10 {
		cards = cards[:10]
	}
	doc["dev_cards"] = cards
	doc["dev_pages_read"] = pages

	var expl, model, on, srcRaw string
	if db.QueryRow(`SELECT coalesce(explainer,''), coalesce(explainer_model,''), coalesce(explainer_on::text,''), coalesce(explainer_sources,'[]'::jsonb)::text
		FROM twoai_family_sources WHERE family_uid=$1`, uid).Scan(&expl, &model, &on, &srcRaw) == nil && expl != "" {
		var srcs []map[string]string
		json.Unmarshal([]byte(srcRaw), &srcs)
		doc["explainer"] = map[string]any{"paragraphs": strings.Split(expl, "\n\n"), "model": model, "written_on": on, "sources": srcs}
	}
}
