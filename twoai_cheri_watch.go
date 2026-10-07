package main

// twoai_cheri_watch: new items on CHERI, CHERIoT and Morello, every run, for
// the Architecture and Engineering security domain page (c42f2aba).
//
// Requested by theworldofai for Stephen on 2026-10-06 (bridge row 501): talk
// about CHERI, https://www.cl.cam.ac.uk/research/security/ctsrd/cheri/, and
// track its development on that page.
//
// A SIBLING OF THE HEALTH WATCH, NOT A TOPIC INSIDE IT. twoai_health_watch's
// coverage resolution reads every coverage row in its table whatever the
// topic, and spends its page fetch budget on medical primaries, so CHERI rows
// in that table would cost the diabetes and Lp(a) hubs fetches and end up
// at 'needs primary'. This stage keeps the same row shape in its own table,
// twoai_cheri_watch, its own one-row-a-day bridge guard, and reuses the
// health watch's feed parser, Google News helpers and the health research
// stage's OpenAlex client.
//
// WHAT COUNTS AS NEW. Every item is keyed by its URL and inserted with ON
// CONFLICT DO NOTHING, so a row is new exactly once. The bridge row counts
// rows still at status 'new' that arrived after the previous bridge row.
//
// PRIMARY SOURCES FIRST, COVERAGE MARKED. The CHERI Alliance's own feed, the
// GitHub release feeds of the CHERI and CHERIoT repositories, RISC-V
// International's feed, Codasip's newsroom and OpenAlex are primary. Google
// News stands in for sources with no working feed (Arm's Morello news, the
// UK Digital Security by Design programme), and those rows carry kind
// 'coverage' and a source ending "(coverage)".
//
// FALSE POSITIVES. "Cheri" is a first name and Morello a surname, so a general
// source's item is kept only when cheriMatch finds a CHERI term in it, and the
// words that fired go into detail->>'matched' (AGENTS.md rule 4). The CHERI
// terms are matched case sensitively, and Morello counts only beside Arm,
// CHERI or hardware words.
//
// ON THE PAGE. Each run attaches 'cheri_watch' to security/domain-arch.json
// through twoai_page_extras: the CHERI facts the content project wrote for
// that page (twoai_sourced_facts) and the five newest tracker rows. The page
// renders them under "CHERI: memory safety in hardware" and leaves those facts
// out of its general facts block.
//
// NOTHING HERE CALLS A MODEL.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	twoaiCheriStage = "twoai_cheri_watch"
	// The page the tracker renders on, as twoai_pages and the site know it.
	twoaiCheriPagePath   = "security/domain-arch.json"
	twoaiCheriTargetPath = "/ai-ecosystem/enterprise-applications-governance-and-tools/c42f2aba/"
	twoaiCheriExtrasKey  = "cheri_watch"
	// A fact the content project files under this topic always belongs to the
	// CHERI section; one with no topic belongs when it names a CHERI term.
	twoaiCheriFactsTopic = "CHERI: memory safety in hardware"
	twoaiCheriShow       = 5
	// Google News links resolved to the publisher per run, two requests each.
	twoaiCheriResolveBudget = 15
	// OpenAlex calls a day: one per search, and the searches run once a day.
	twoaiCheriOABudget = 2
	// How far back the OpenAlex searches look.
	twoaiCheriOADays  = 730
	twoaiCheriReserve = 45 * time.Second
)

// ---------------------------------------------------------------------
// The CHERI gate.
// ---------------------------------------------------------------------

var (
	// Case sensitive on purpose: the project writes CHERI and CHERIoT, and a
	// person called Cheri is written Cheri.
	cheriStrongRe  = regexp.MustCompile(`\bCHERI(?:oT)?\b|Capability Hardware Enhanced RISC|[Cc]apability[- ][Hh]ardware|Digital Security by Design|\bDSbD\b`)
	cheriMorelloRe = regexp.MustCompile(`\bMorello\b`)
	// Morello is also a surname, so it needs a hardware word beside it.
	cheriMorelloCtxRe = regexp.MustCompile(`(?i)\barm\b|\bcheri|capabilit\w*|prototype|processor|\bboards?\b|\bchips?\b|silicon|memory[- ]safe\w*|\bsoc\b|\bcpu\b`)
)

// cheriMatch returns the words that make text a CHERI item, or "" when it
// is not one.
func cheriMatch(text string) string {
	if m := cheriStrongRe.FindString(text); m != "" {
		return m
	}
	if m := cheriMorelloRe.FindString(text); m != "" {
		if c := cheriMorelloCtxRe.FindString(text); c != "" {
			return m + " + " + c
		}
	}
	return ""
}

// ---------------------------------------------------------------------
// Sources. Each feed below answered curl on 2026-10-05 with the item count
// noted. A repository with no release yet still answers with an empty
// feed, and is listed so its first release is caught.
// ---------------------------------------------------------------------

// twoaiCheriFeed is one RSS or Atom feed. With filter set an item is kept
// only when cheriMatch finds a CHERI term in its title or summary. prefix
// names a repository in front of a release title, which is often just a
// version number.
type twoaiCheriFeed struct {
	source, url, kind, prefix string
	filter                    bool
}

var twoaiCheriFeeds = []twoaiCheriFeed{
	// The CHERI Alliance's own news, 10 items. Every post is about CHERI.
	{"CHERI Alliance", "https://cheri-alliance.org/feed/", "alliance", "", false},
	// RISC-V International's news, 10 items, filtered: the CHERI extension
	// is one of many.
	{"RISC-V International", "https://riscv.org/feed/", "standards", "", true},
	// Codasip ships CHERI cores and writes about much else, 9 items, filtered.
	{"Codasip", "https://codasip.com/feed/", "vendor", "", true},
	// GitHub release feeds: riscv-cheri 10 entries, cheriot-ibex 1,
	// sonata-system 10, and three repositories with no release yet.
	{"GitHub riscv/riscv-cheri", "https://github.com/riscv/riscv-cheri/releases.atom", "release", "RISC-V CHERI specification", false},
	{"GitHub microsoft/cheriot-ibex", "https://github.com/microsoft/cheriot-ibex/releases.atom", "release", "CHERIoT Ibex core", false},
	{"GitHub lowRISC/sonata-system", "https://github.com/lowRISC/sonata-system/releases.atom", "release", "Sonata CHERIoT board", false},
	{"GitHub lowRISC/sonata-software", "https://github.com/lowRISC/sonata-software/releases.atom", "release", "Sonata CHERIoT software", false},
	{"GitHub CHERIoT-Platform/cheriot-rtos", "https://github.com/CHERIoT-Platform/cheriot-rtos/releases.atom", "release", "CHERIoT RTOS", false},
	{"GitHub CHERIoT-Platform/llvm-project", "https://github.com/CHERIoT-Platform/llvm-project/releases.atom", "release", "CHERIoT LLVM toolchain", false},
}

// SOURCES WITH NO WORKING FEED on 2026-10-05:
//
//	Cambridge CTSRD   the CHERI page and ctsrd/news.html answer, as HTML
//	                  with no feed; cheri/cheri-news.html is a 404.
//	CHERI Research    cheri-research-centre.org did not answer at all.
//	Centre
//	Morello           morello-project.org answers, /news/ is a 404; Arm's
//	                  Morello news is covered through Google News below.
//	dsbd.tech         /feed/ answers with an HTML page, not a feed; covered
//	                  through Google News below.
//	CHERIoT-Platform  cheriot-ibex has no repository under that org (404);
//	                  the core is microsoft/cheriot-ibex above.

// twoaiCheriCoverage are Google News queries, each story kept only when its
// headline passes cheriMatch.
var twoaiCheriCoverage = []struct{ label, query, window string }{
	{"CHERI", `CHERI ("memory safety" OR capability OR CHERIoT OR Morello)`, "14d"},
	{"Morello", `Morello (Arm OR CHERI) (board OR processor OR prototype OR "memory safety")`, "30d"},
	{"Digital Security by Design", `"Digital Security by Design" OR DSbD CHERI`, "30d"},
	{"CHERI silicon", `CHERI (Codasip OR "SCI Semiconductor" OR lowRISC OR Microsoft OR Google OR Arm) (chip OR core OR silicon OR processor)`, "30d"},
}

// twoaiCheriOASearches are the OpenAlex title and abstract searches.
var twoaiCheriOASearches = []string{"CHERI capability", "CHERIoT"}

// ---------------------------------------------------------------------
// The run.
// ---------------------------------------------------------------------

type cheriRun struct {
	*hwRun
	added    map[string]int
	resolved int
}

func (c *cheriRun) fetch(u string) ([]byte, error) {
	// The browser-compatible client that still names itself, the shape that
	// was verified with curl.
	return twoaiJobsGet(u, map[string]string{"User-Agent": twoaiHealthPageUA})
}

// save writes one item and reports whether it was new.
func (c *cheriRun) save(it hwItem) bool {
	if it.url == "" || it.title == "" {
		return false
	}
	if it.detail == nil {
		it.detail = map[string]any{}
	}
	if len(it.title) > 1000 {
		it.title = it.title[:1000]
	}
	dj, _ := json.Marshal(it.detail)
	res, err := c.db.Exec(`INSERT INTO twoai_cheri_watch (url, topic, source, title, item_date, kind, sub_hub, status, detail)
		VALUES ($1,'cheri',$2,$3,NULLIF($4,'')::date,$5,'cheri','new',$6::jsonb)
		ON CONFLICT (url) DO NOTHING`,
		it.url, it.source, it.title, it.date, it.kind, string(dj))
	if err != nil {
		c.notice("insert %s: %v", it.url, err)
		return false
	}
	if n, _ := res.RowsAffected(); n > 0 {
		c.added[it.kind]++
		return true
	}
	return false
}

func twoaiCheriEnsure(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_cheri_watch (
		url text PRIMARY KEY,
		topic text NOT NULL DEFAULT 'cheri',
		source text NOT NULL,
		title text NOT NULL,
		item_date date,
		kind text NOT NULL,
		sub_hub text,
		status text NOT NULL DEFAULT 'new',
		found_on date NOT NULL DEFAULT current_date,
		found_at timestamptz NOT NULL DEFAULT now(),
		detail jsonb NOT NULL DEFAULT '{}'::jsonb)`); err != nil {
		return fmt.Errorf("create twoai_cheri_watch: %w", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS twoai_cheri_watch_status_found ON twoai_cheri_watch (status, found_at DESC)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS twoai_cheri_watch_gnews ON twoai_cheri_watch ((detail->>'gnews'))`)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_cheri_watch_bridge (
		sent_on date PRIMARY KEY DEFAULT current_date,
		sent_at timestamptz NOT NULL DEFAULT now(),
		items int NOT NULL,
		bridge_topic text)`); err != nil {
		return fmt.Errorf("create twoai_cheri_watch_bridge: %w", err)
	}
	return nil
}

func twoaiCheriWatch(db *sql.DB) error {
	start := time.Now()
	if err := twoaiCheriEnsure(db); err != nil {
		return err
	}
	limit := twoaiStageDeadlineDefault
	if d, ok := twoaiStageDeadline[twoaiCheriStage]; ok {
		limit = d
	}
	c := &cheriRun{
		hwRun: &hwRun{db: db, stop: start.Add(limit - twoaiCheriReserve),
			seen: map[string]int{}, added: map[string]int{}, skipped: map[string]int{}, byTopic: map[string]int{}},
		added: map[string]int{},
	}

	c.feeds()
	c.openAlex()
	c.coverage()
	placed, facts := twoaiCheriPlace(db)
	c.bridge()

	for _, n := range c.notices {
		// Stdout: a source being down is a notice, not a failure.
		fmt.Printf("twoai_cheri_watch: notice: %s\n", n)
	}
	total := 0
	for _, n := range c.added {
		total += n
	}
	fmt.Printf("twoai_cheri_watch: alliance=%d standards=%d vendor=%d releases=%d papers=%d coverage=%d new=%d on_page=%d facts=%d notices=%d elapsed=%s ok=true\n",
		c.added["alliance"], c.added["standards"], c.added["vendor"], c.added["release"], c.added["paper"], c.added["coverage"],
		total, placed, facts, len(c.notices), time.Since(start).Round(time.Second))
	return nil
}

// cheriReleaseTitle puts the repository in front of a release title, unless
// the title already names it.
func cheriReleaseTitle(prefix, title string) string {
	if prefix == "" || strings.Contains(strings.ToLower(title), strings.ToLower(prefix)) {
		return title
	}
	return prefix + ": " + title
}

func (c *cheriRun) feeds() {
	for _, f := range twoaiCheriFeeds {
		if c.late(f.source + " feed") {
			return
		}
		body, err := c.fetch(f.url)
		if err != nil {
			c.notice("%s feed: %v", f.source, err)
			continue
		}
		items, err := hwParseFeed(body)
		if err != nil {
			c.notice("%s feed parse: %v", f.source, err)
			continue
		}
		for _, it := range items {
			link := it.URL()
			title := hwClean(it.Title)
			if link == "" || title == "" {
				continue
			}
			summary := twoaiFeedSummary(it.Description, it.Summary, it.Content)
			d := map[string]any{"feed": f.url}
			if f.filter {
				m := cheriMatch(title + " " + summary)
				if m == "" {
					continue
				}
				d["matched"] = m
			}
			if summary != "" {
				d["summary"] = summary
			}
			c.save(hwItem{url: link, source: f.source, title: cheriReleaseTitle(f.prefix, title),
				date: twoaiFeedDate(it.Published, it.Updated, it.PubDate, it.Date), kind: f.kind, detail: d})
		}
	}
}

// openAlex runs the paper searches once a calendar day, newest first,
// through the health research stage's OpenAlex client, which adds the key
// and the mailto and handles a spent budget or a rate limit.
func (c *cheriRun) openAlex() {
	const gate = "twoai_cheri_watch_openalex"
	var last string
	c.db.QueryRow(`SELECT last_run_date::text FROM pipeline_stage_runs WHERE stage=$1`, gate).Scan(&last)
	today := time.Now().UTC().Format("2006-01-02")
	if last == today {
		return
	}
	oa := &hrRun{db: c.db, client: &http.Client{Timeout: 60 * time.Second}, stop: c.stop}
	since := time.Now().AddDate(0, 0, -twoaiCheriOADays).Format("2006-01-02")
	ran := 0
	for _, s := range twoaiCheriOASearches {
		if c.late("OpenAlex " + s) {
			break
		}
		filter := fmt.Sprintf("title_and_abstract.search:%s,from_publication_date:%s", s, since)
		u := fmt.Sprintf("https://api.openalex.org/works?filter=%s&sort=publication_date:desc&per-page=25&select=%s",
			url.QueryEscape(filter), url.QueryEscape("id,doi,title,display_name,publication_date,type,primary_location,abstract_inverted_index"))
		body, code, err := oa.oaGet(u, twoaiCheriOABudget)
		if err != nil || code != 200 {
			c.notice("OpenAlex %q: http %d %v", s, code, err)
			continue
		}
		ran++
		var out struct {
			Results []twoaiHRWork `json:"results"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			c.notice("OpenAlex %q parse: %v", s, err)
			continue
		}
		for _, w := range out.Results {
			title := hwClean(w.Title)
			if title == "" {
				title = hwClean(w.Display)
			}
			m := cheriMatch(title + " " + twoaiOAAbstract(w.AbstractII))
			if m == "" {
				continue
			}
			link := strings.TrimSpace(w.ID)
			if doi := twoaiHRDOI(w.DOI); doi != "" {
				link = "https://doi.org/" + doi
			}
			src := w.venue()
			if src == "" {
				src = "OpenAlex"
			}
			c.save(hwItem{url: link, source: src, title: title, date: w.PubDate, kind: "paper",
				detail: map[string]any{"openalex": w.ID, "search": s, "type": w.Type, "matched": m}})
		}
	}
	c.notices = append(c.notices, oa.notices...)
	if ran > 0 {
		c.db.Exec(`INSERT INTO pipeline_stage_runs (stage, last_run_date, last_run_at) VALUES ($1, current_date, now())
			ON CONFLICT (stage) DO UPDATE SET last_run_date=current_date, last_run_at=now()`, gate)
	}
}

func (c *cheriRun) coverage() {
	for _, q := range twoaiCheriCoverage {
		if c.late(q.label + " coverage") {
			return
		}
		for _, it := range c.gnewsItems(q.label, q.query, q.window, 15) {
			title := hwGNewsTitle(it.Title, it.Source)
			m := cheriMatch(title)
			glink := it.URL()
			if m == "" || glink == "" || title == "" {
				continue
			}
			var n int
			c.db.QueryRow(`SELECT count(*) FROM twoai_cheri_watch WHERE url=$1 OR detail->>'gnews'=$1`, glink).Scan(&n)
			if n > 0 {
				continue
			}
			u := glink
			if c.resolved < twoaiCheriResolveBudget && !c.late("coverage resolution") {
				c.resolved++
				u = resolveGoogleNews(glink)
			}
			pub := strings.TrimSpace(it.Source)
			if pub == "" {
				pub = "News"
			}
			c.save(hwItem{url: u, source: pub + " (coverage)", title: title, date: twoaiFeedDate(it.PubDate), kind: "coverage",
				detail: map[string]any{"query": q.query, "gnews": glink, "publisher": pub, "matched": m}})
		}
	}
}

// ---------------------------------------------------------------------
// On the page.
// ---------------------------------------------------------------------

// twoaiCheriPlace attaches the CHERI facts and the newest tracker rows to
// the Architecture and Engineering page. It returns the tracker rows and
// facts placed.
func twoaiCheriPlace(db *sql.DB) (int, int) {
	var facts []map[string]string
	if rows, err := db.Query(`SELECT claim, source_url, COALESCE(source_title,''), COALESCE(source_date::text,''), COALESCE(topic,'')
		FROM twoai_sourced_facts WHERE status='live' AND target_path=$1 ORDER BY sort, id`, twoaiCheriTargetPath); err == nil {
		for rows.Next() {
			var claim, u, t, d, topic string
			if rows.Scan(&claim, &u, &t, &d, &topic) != nil {
				continue
			}
			if why := cheriFactMatch(topic, claim); why != "" {
				facts = append(facts, map[string]string{"claim": claim, "source_url": u, "source_title": t, "date": d, "matched": why})
			}
		}
		rows.Close()
	}
	// A person the site has a page for is linked where a fact names them
	// (theworldofai row 503: Peter G. Neumann, CHERI's principal
	// investigator). The claim is split around the name so the page can link
	// it in place; the longest name wins.
	type cheriPerson struct{ name, path string }
	var people []cheriPerson
	if rows, err := db.Query(`SELECT data->>'name', data->>'uid' FROM twoai_pages
		WHERE path ~ '^people/[0-9a-f]{8}\.json$' AND length(COALESCE(data->>'name','')) >= 8 AND COALESCE(data->>'uid','') <> ''
		ORDER BY length(data->>'name') DESC`); err == nil {
		for rows.Next() {
			var n, u string
			if rows.Scan(&n, &u) == nil {
				people = append(people, cheriPerson{n, "/ai-ecosystem/ecosystem-entities-market-and-operations/" + u + "/"})
			}
		}
		rows.Close()
	}
	for _, f := range facts {
		for _, p := range people {
			if i := strings.Index(f["claim"], p.name); i >= 0 {
				f["pre"], f["person"], f["post"], f["person_path"] = f["claim"][:i], p.name, f["claim"][i+len(p.name):], p.path
				break
			}
		}
	}
	// EVERY NAME WITH A PAGE, 2026-10-07 (theworldofai rows 530 and 532): the
	// facts also name SRI International, which now has a company page, and
	// may name glossary terms. Each fact carries parts, text runs with an
	// href where a run is a person, company or glossary term this site has a
	// page for, each target linked at its first mention in the section only.
	// pre/person/post stay for a site build that predates parts.
	var targets []cheriLinkTarget
	for _, p := range people {
		targets = append(targets, cheriLinkTarget{p.name, p.path})
	}
	if rows, err := db.Query(`SELECT name, uid FROM twoai_company_profiles WHERE length(COALESCE(name,'')) >= 4`); err == nil {
		for rows.Next() {
			var n, u string
			if rows.Scan(&n, &u) == nil {
				targets = append(targets, cheriLinkTarget{n, "/companies/" + u + "/"})
			}
		}
		rows.Close()
	}
	for _, t := range twoaiGlossaryTerms(db) {
		for _, n := range t.names {
			targets = append(targets, cheriLinkTarget{n, "/ai-glossary/" + t.slug + "/"})
		}
	}
	linked := map[string]bool{}
	var factsOut []map[string]any
	for _, f := range facts {
		o := map[string]any{}
		for k, v := range f {
			o[k] = v
		}
		if parts := cheriLinkParts(f["claim"], targets, linked); len(parts) > 1 {
			o["parts"] = parts
		}
		factsOut = append(factsOut, o)
	}
	var items []map[string]any
	var tracked int
	db.QueryRow(`SELECT count(*) FROM twoai_cheri_watch WHERE status NOT IN ('skipped','hidden')`).Scan(&tracked)
	// One line per title: Zenodo gives a paper a concept DOI and a version
	// DOI, and both arrive from OpenAlex (2026-10-06, the same paper twice).
	if rows, err := db.Query(`SELECT url, title, source, d, kind FROM (
			SELECT DISTINCT ON (lower(title)) url, title, source, COALESCE(item_date::text,'') d, kind, item_date, found_at
			FROM twoai_cheri_watch WHERE status NOT IN ('skipped','hidden')
			ORDER BY lower(title), item_date DESC NULLS LAST, found_at DESC) x
		ORDER BY item_date DESC NULLS LAST, found_at DESC LIMIT $1`, twoaiCheriShow); err == nil {
		for rows.Next() {
			var u, t, s, d, k string
			if rows.Scan(&u, &t, &s, &d, &k) == nil {
				// The page says "news coverage" itself, so the publisher is
				// shown without the stored suffix.
				items = append(items, map[string]any{"url": u, "title": t, "source": strings.TrimSuffix(s, " (coverage)"),
					"date": d, "kind": k, "coverage": k == "coverage"})
			}
		}
		rows.Close()
	}
	if len(facts) == 0 && len(items) == 0 {
		db.Exec(`DELETE FROM twoai_page_extras WHERE page_path=$1 AND key=$2`, twoaiCheriPagePath, twoaiCheriExtrasKey)
		return 0, 0
	}
	v, _ := json.Marshal(map[string]any{"facts": factsOut, "items": items, "tracked": tracked,
		"as_of": time.Now().UTC().Format("2006-01-02")})
	if _, err := db.Exec(`INSERT INTO twoai_page_extras (page_path, key, value) VALUES ($1, $2, $3::jsonb)
		ON CONFLICT (page_path, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
		WHERE twoai_page_extras.value IS DISTINCT FROM EXCLUDED.value`, twoaiCheriPagePath, twoaiCheriExtrasKey, string(v)); err != nil {
		fmt.Printf("twoai_cheri_watch: notice: page extras: %v\n", err)
	}
	return len(items), len(facts)
}

// cheriFactMatch says why a fact on the page belongs in the CHERI section:
// its topic is the section's heading, or it has no topic and names CHERI.
func cheriFactMatch(topic, claim string) string {
	topic = strings.TrimSpace(topic)
	if strings.EqualFold(topic, twoaiCheriFactsTopic) {
		return "topic"
	}
	if topic != "" {
		return ""
	}
	return cheriMatch(claim)
}

// ---------------------------------------------------------------------
// The daily bridge row.
// ---------------------------------------------------------------------

// cheriBridgeBody writes the bridge message, near forty lines at most.
func cheriBridgeBody(today, since string, n int, sources []hwCount, rows []hwRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CHERI watch, %s: %d new items since %s.\n", today, n, since)
	b.WriteString("Primary sources: the CHERI Alliance feed, GitHub releases of the CHERI and CHERIoT repositories, RISC-V International and Codasip (both filtered to CHERI terms) and OpenAlex papers. Items marked coverage are news reports found through Google News (Morello, Digital Security by Design, CHERI silicon) and need a primary source before anything is published.\n")
	parts := []string{}
	for _, s := range sources {
		parts = append(parts, fmt.Sprintf("%s %d", s.name, s.n))
	}
	fmt.Fprintf(&b, "By source: %s.\n", strings.Join(parts, ", "))
	if len(rows) > 0 {
		b.WriteString("Newest:\n")
	}
	for i, r := range rows {
		if i == 30 {
			fmt.Fprintf(&b, "... and %d more.\n", n-30)
			break
		}
		t := r.title
		if len(t) > 160 {
			t = t[:157] + "..."
		}
		date := r.date
		if date == "" {
			date = "undated"
		}
		tag := ""
		if r.kind == "coverage" {
			tag = "[coverage] "
		}
		fmt.Fprintf(&b, "- %s%s (%s, %s) %s\n", tag, t, r.source, date, r.url)
	}
	fmt.Fprintf(&b, "The newest %d render on Architecture and Engineering (%s) under CHERI development tracker, with the CHERI facts above them. All rows are in twoai_cheri_watch, status new until you change it; status hidden keeps a row off the page. srj owns the stage, send code needs by bridge.", twoaiCheriShow, twoaiCheriTargetPath)
	return b.String()
}

func (c *cheriRun) bridge() {
	var sentToday bool
	c.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM twoai_cheri_watch_bridge WHERE sent_on=current_date)`).Scan(&sentToday)
	if sentToday {
		return
	}
	var last sql.NullTime
	c.db.QueryRow(`SELECT max(sent_at) FROM twoai_cheri_watch_bridge`).Scan(&last)
	since := "the first run"
	cutoff := time.Time{}
	if last.Valid {
		cutoff = last.Time
		since = "the last bridge row, " + last.Time.UTC().Format("2006-01-02 15:04 UTC")
	}
	const scope = `FROM twoai_cheri_watch WHERE status='new' AND found_at > $1`
	var n int
	if err := c.db.QueryRow(`SELECT count(*) `+scope, cutoff).Scan(&n); err != nil || n == 0 {
		return
	}
	sources := c.counts(`SELECT source, count(*) `+scope+` GROUP BY 1 ORDER BY 2 DESC, 1`, cutoff)
	var list []hwRow
	if rows, err := c.db.Query(`SELECT kind, title, source, COALESCE(item_date::text,''), url `+scope+`
		ORDER BY item_date DESC NULLS LAST, found_at DESC LIMIT 31`, cutoff); err == nil {
		for rows.Next() {
			var x hwRow
			if rows.Scan(&x.kind, &x.title, &x.source, &x.date, &x.url) == nil {
				list = append(list, x)
			}
		}
		rows.Close()
	}
	subject := fmt.Sprintf("CHERI watch: %d new items", n)
	body := cheriBridgeBody(time.Now().UTC().Format("2006-01-02"), since, n, sources, list)
	if _, err := c.db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body) VALUES ('srj','theworldofai',$1,$2)`, subject, body); err != nil {
		c.notice("bridge row: %v", err)
		return
	}
	c.db.Exec(`INSERT INTO twoai_cheri_watch_bridge (items, bridge_topic) VALUES ($1,$2) ON CONFLICT DO NOTHING`, n, subject)
	fmt.Printf("twoai_cheri_watch: bridge row sent, %s\n", subject)
}

type cheriLinkTarget struct{ name, href string }

// cheriLinkParts splits text into runs, linking each target's first
// whole-word mention, longest name first, skipping a target whose href is
// already in linked (an earlier fact took it).
func cheriLinkParts(text string, targets []cheriLinkTarget, linked map[string]bool) []map[string]string {
	sorted := append([]cheriLinkTarget(nil), targets...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i].name) > len(sorted[j].name) })
	type span struct {
		i, j int
		href string
	}
	var spans []span
	used := make([]bool, len(text))
	for _, t := range sorted {
		if linked[t.href] || t.name == "" {
			continue
		}
		at := cheriWordIndex(text, t.name)
		if at < 0 {
			continue
		}
		end := at + len(t.name)
		clash := false
		for k := at; k < end; k++ {
			if used[k] {
				clash = true
				break
			}
		}
		if clash {
			continue
		}
		for k := at; k < end; k++ {
			used[k] = true
		}
		linked[t.href] = true
		spans = append(spans, span{at, end, t.href})
	}
	sort.Slice(spans, func(a, b int) bool { return spans[a].i < spans[b].i })
	var parts []map[string]string
	pos := 0
	for _, sp := range spans {
		if sp.i > pos {
			parts = append(parts, map[string]string{"text": text[pos:sp.i]})
		}
		parts = append(parts, map[string]string{"text": text[sp.i:sp.j], "href": sp.href})
		pos = sp.j
	}
	if pos < len(text) {
		parts = append(parts, map[string]string{"text": text[pos:]})
	}
	return parts
}

// cheriWordIndex finds name in text as whole words, case-sensitive, since
// the targets are proper names and terms as written.
func cheriWordIndex(text, name string) int {
	from := 0
	for {
		i := strings.Index(text[from:], name)
		if i < 0 {
			return -1
		}
		i += from
		end := i + len(name)
		before := i == 0 || !cheriWordRune(text[i-1])
		after := end >= len(text) || !cheriWordRune(text[end])
		if before && after {
			return i
		}
		from = i + 1
	}
}

func cheriWordRune(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}
