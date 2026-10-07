package main

// EXECUTIVE ACTIONS ON THE STATE LAW PAGES, theworldofai row 566 (2026-10-07).
//
// Stephen read "Utah Gov. Cox signs executive order on AI integration in
// state government" on the news page and asked why the Utah law page did not
// have it. It could not: the state pages are built from LegiScan, and an
// executive order, an attorney general's guidance or a task force never
// passes through a legislature. twoai_state_actions holds those, one row per
// action with its date, title, one sentence of what it does and the primary
// source; the state page renders them under "Executive actions"; and a feed
// watch over the governors' newsrooms (twoai_gov_feeds, below) fills the
// table with candidates so the section does not depend on anyone reading the
// news first.
//
// THE FEEDS ARE PROBED BY THE STAGE, NOT ASSUMED. Every row of twoai_gov_feeds
// starts from the newsroom's home page; the first run looks for a declared
// feed in the page head (twoaiDiscoverFeedInHTML, as the company harvest
// does), tries /feed/ when none is declared, and records what it found with
// the HTTP status, so a newsroom with no usable feed is a line in the daily
// summary and in the morning bridge row, never a quiet zero. The sandbox the
// code is written in cannot reach the .gov hosts, so no feed URL here was
// checked by hand; the probe is the check.

import (
	"database/sql"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// twoaiGovAIRe is what makes a release AI-related.
var twoaiGovAIRe = regexp.MustCompile(`(?i)\b(artificial intelligence|AI|A\.I\.|machine learning|generative|chatbot|deepfake|algorithm\w*|automated decision|large language model|LLM)\b`)

// twoaiGovActionRe marks a release that is itself an executive action, which
// goes live on the page; anything else AI-related is a candidate for review.
var twoaiGovActionRe = regexp.MustCompile(`(?i)\b(executive order|signs|signed|task ?force|guidance|directive|proclamation|attorney general|advisory|commission)\b`)

func twoaiStateActionsEnsure(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_state_actions (
		id serial PRIMARY KEY,
		state_code text NOT NULL,
		kind text NOT NULL DEFAULT 'executive order',
		acted_on date,
		title text NOT NULL,
		what text,
		source_url text NOT NULL UNIQUE,
		source_title text,
		story_uid text,
		status text NOT NULL DEFAULT 'live',
		added_by text, added_on date NOT NULL DEFAULT current_date, note text)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_gov_feeds (
		code text PRIMARY KEY,
		name text NOT NULL,
		kind text NOT NULL DEFAULT 'governor',
		home_url text NOT NULL,
		feed_url text,
		enabled boolean NOT NULL DEFAULT true,
		probed_at timestamptz, probe_status int, probe_note text,
		last_items int, last_ai int, last_ok_at timestamptz,
		note text)`)
	// The seed: states with AI laws first, then the federal and EU sources.
	// home_url is the newsroom; feed_url is a candidate where the site is a
	// WordPress install (CA, FL, UT) and is otherwise found by the probe.
	for _, f := range [][4]string{
		{"CA", "California, Office of the Governor", "https://www.gov.ca.gov/", "https://www.gov.ca.gov/feed/"},
		{"CO", "Colorado, Office of the Governor", "https://www.colorado.gov/governor", ""},
		{"CT", "Connecticut, Office of the Governor", "https://portal.ct.gov/governor", ""},
		{"FL", "Florida, Office of the Governor", "https://www.flgov.com/", "https://www.flgov.com/feed/"},
		{"GA", "Georgia, Office of the Governor", "https://gov.georgia.gov/", ""},
		{"IL", "Illinois, Office of the Governor", "https://gov.illinois.gov/", ""},
		{"MD", "Maryland, Office of the Governor", "https://governor.maryland.gov/", ""},
		{"MA", "Massachusetts, Office of the Governor", "https://www.mass.gov/orgs/office-of-the-governor", ""},
		{"MT", "Montana, Office of the Governor", "https://governor.mt.gov/", ""},
		{"NH", "New Hampshire, Office of the Governor", "https://www.governor.nh.gov/", ""},
		{"NJ", "New Jersey, Office of the Governor", "https://www.nj.gov/governor/", ""},
		{"NY", "New York, Office of the Governor", "https://www.governor.ny.gov/", ""},
		{"OR", "Oregon, Office of the Governor", "https://www.oregon.gov/gov/", ""},
		{"TN", "Tennessee, Office of the Governor", "https://www.tn.gov/governor.html", ""},
		{"TX", "Texas, Office of the Governor", "https://gov.texas.gov/", ""},
		{"UT", "Utah, Office of the Governor", "https://governor.utah.gov/", "https://governor.utah.gov/feed/"},
		{"VA", "Virginia, Office of the Governor", "https://www.governor.virginia.gov/", ""},
		{"WA", "Washington, Office of the Governor", "https://governor.wa.gov/", ""},
		{"US-WH-ACTIONS", "White House, presidential actions", "https://www.whitehouse.gov/presidential-actions/", "https://www.whitehouse.gov/presidential-actions/feed/"},
		{"US-WH-BRIEF", "White House, briefings and statements", "https://www.whitehouse.gov/briefings-statements/", "https://www.whitehouse.gov/briefings-statements/feed/"},
		{"EU-DIGITAL", "European Commission, digital strategy news", "https://digital-strategy.ec.europa.eu/en/news", ""},
	} {
		kind := "governor"
		if strings.HasPrefix(f[0], "US-") || strings.HasPrefix(f[0], "EU-") {
			kind = "federal"
		}
		db.Exec(`INSERT INTO twoai_gov_feeds (code, name, kind, home_url, feed_url) VALUES ($1,$2,$3,$4,NULLIF($5,''))
			ON CONFLICT (code) DO NOTHING`, f[0], f[1], kind, f[2], f[3])
	}
	// Utah, the action that opened the row.
	db.Exec(`INSERT INTO twoai_state_actions (state_code, kind, acted_on, title, what, source_url, source_title, story_uid, status, added_by, note)
		VALUES ('UT', 'executive order', '2026-10-06',
		'Executive order establishing a pro-human approach to AI in state government',
		'Governor Spencer Cox ordered a structured, pro-human framework for AI across state government, with AI training for state employees.',
		'https://governor.utah.gov/', 'Utah Governor news release: Gov. Cox Signs Executive Order Establishing Utah''s Pro-Human Approach to AI in State Government',
		'8d1f4660', 'live', 'srj', 'theworldofai row 566, 2026-10-07. The release URL on governor.utah.gov could not be opened from the srj sandbox; Forth.News mirror https://www.forth.news/lists/utgov/CfmBpAmPTThYoSZYU2vb5. Replace source_url with the release page when read.')
		ON CONFLICT (source_url) DO NOTHING`)
}

// twoaiStateActions returns a state's live actions, newest first, for the
// state page document.
func twoaiStateActions(db *sql.DB, code string) []map[string]any {
	rows, err := db.Query(`SELECT kind, COALESCE(acted_on::text,''), title, COALESCE(what,''), source_url, COALESCE(source_title,''), COALESCE(story_uid,'')
		FROM twoai_state_actions WHERE state_code=$1 AND status='live' ORDER BY acted_on DESC NULLS LAST, id DESC`, code)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var kind, on, title, what, src, srcTitle, story string
		if rows.Scan(&kind, &on, &title, &what, &src, &srcTitle, &story) != nil {
			continue
		}
		m := map[string]any{"kind": kind, "date": on, "title": title, "what": what, "source_url": src, "source_title": srcTitle}
		if story != "" {
			m["story_url"] = "/ai-news/" + story + "/"
		}
		out = append(out, m)
	}
	return out
}

// twoaiFetchFeed reads one RSS or Atom feed.
func twoaiFetchFeed(client *http.Client, u string) (int, []twoaiFeedItem, []byte, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; theworldofai.org government watch; info@srjconsultingservices.com)")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml, text/html;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
	if resp.StatusCode != 200 {
		return resp.StatusCode, nil, body, fmt.Errorf("http %d", resp.StatusCode)
	}
	var doc twoaiFeedDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return 200, nil, body, fmt.Errorf("not a feed: %v", err)
	}
	items := doc.Channel.Items
	if len(items) == 0 {
		items = doc.Entries
	}
	return 200, items, body, nil
}

// twoaiGovWatch reads every enabled newsroom feed, probing for the feed where
// none is known, and writes AI-related releases as actions (live when the
// title says it is an action, candidate otherwise) and as documents for the
// news intake, so an official release is clustered with the outlets' coverage.
func twoaiGovWatch(db *sql.DB) error {
	twoaiStateActionsEnsure(db)
	rows, err := db.Query(`SELECT code, name, kind, home_url, COALESCE(feed_url,'') FROM twoai_gov_feeds WHERE enabled ORDER BY code`)
	if err != nil {
		return err
	}
	type feed struct{ code, name, kind, home, url string }
	var feeds []feed
	for rows.Next() {
		var f feed
		if rows.Scan(&f.code, &f.name, &f.kind, &f.home, &f.url) == nil {
			feeds = append(feeds, f)
		}
	}
	rows.Close()
	var sourceID int
	db.QueryRow(`SELECT id FROM pipeline.sources WHERE key='gdelt'`).Scan(&sourceID)
	client := &http.Client{Timeout: 30 * time.Second}
	today := time.Now().UTC().Format("2006-01-02")
	alive, dead, live, cands, docs := 0, 0, 0, 0, 0
	var deadNotes []string
	for _, f := range feeds {
		status, items, body, ferr := 0, []twoaiFeedItem(nil), []byte(nil), error(nil)
		url := f.url
		if url == "" {
			// Probe: the declared feed in the home page's head, else /feed/.
			req, _ := http.NewRequest("GET", f.home, nil)
			req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; theworldofai.org government watch; info@srjconsultingservices.com)")
			if resp, err := client.Do(req); err == nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
				resp.Body.Close()
				status = resp.StatusCode
				if resp.StatusCode == 200 {
					url = twoaiDiscoverFeedInHTML(f.home, b)
				}
			} else {
				ferr = err
			}
			if url == "" && ferr == nil {
				url = strings.TrimRight(f.home, "/") + "/feed/"
			}
		}
		if url != "" {
			status, items, body, ferr = twoaiFetchFeed(client, url)
		}
		_ = body
		if ferr != nil || len(items) == 0 {
			dead++
			note := "no feed found"
			if ferr != nil {
				note = trunc(ferr.Error(), 160)
			} else if len(items) == 0 {
				note = "feed answered with no items"
			}
			deadNotes = append(deadNotes, fmt.Sprintf("%s (%s: %s)", f.code, url, note))
			db.Exec(`UPDATE twoai_gov_feeds SET probed_at=now(), probe_status=$2, probe_note=$3, last_items=0 WHERE code=$1`, f.code, status, note)
			continue
		}
		alive++
		if f.url == "" {
			db.Exec(`UPDATE twoai_gov_feeds SET feed_url=$2 WHERE code=$1`, f.code, url)
		}
		ai := 0
		for _, it := range items {
			title := strings.TrimSpace(html.UnescapeString(twoaiTagStrip.ReplaceAllString(it.Title, "")))
			link := strings.TrimSpace(it.URL())
			desc := strings.TrimSpace(html.UnescapeString(twoaiTagStrip.ReplaceAllString(it.Description+" "+it.Summary, " ")))
			if title == "" || link == "" || !twoaiGovAIRe.MatchString(title+" "+desc) {
				continue
			}
			ai++
			date := twoaiFeedDate(it.PubDate, it.Published, it.Updated, it.Date)
			if date == "" {
				date = today
			}
			stateCode := f.code
			if f.kind == "federal" {
				stateCode = "US"
			}
			st, kind := "candidate", "release"
			if m := twoaiGovActionRe.FindString(title); m != "" {
				st = "live"
				switch strings.ToLower(m) {
				case "executive order", "signs", "signed", "proclamation", "directive":
					kind = "executive order"
				case "task force", "taskforce", "commission":
					kind = "task force"
				case "guidance", "advisory", "attorney general":
					kind = "guidance"
				}
			}
			res, err := db.Exec(`INSERT INTO twoai_state_actions (state_code, kind, acted_on, title, what, source_url, source_title, status, added_by, note)
				VALUES ($1,$2,$3::date,$4,$5,$6,$7,$8,'twoai_gov_watch', 'from the newsroom feed ' || $9 || ' on ' || $10) ON CONFLICT (source_url) DO NOTHING`,
				stateCode, kind, date, trunc(title, 300), trunc(desc, 400), link, f.name, st, url, today)
			if err != nil {
				fmt.Fprintf(os.Stderr, "twoai_gov_watch: %s: %v\n", f.code, err)
				continue
			}
			if k, _ := res.RowsAffected(); k == 0 {
				continue
			}
			if st == "live" {
				live++
			} else {
				cands++
			}
			// The release is also a document for the news intake, so the
			// morning briefing clusters it with the outlets that report it.
			if sourceID > 0 {
				if r2, err := db.Exec(`INSERT INTO pipeline.documents (source_id, external_id, change_hash, url, title, published_at, fetched_at, raw)
					SELECT $1, md5($2), md5($2), $2, $3, $4::date, now(),
					       jsonb_build_object('url', $2, 'date', $4 || 'T12:00:00Z', 'title', $3, 'domain', $5, 'intake', 'gov_watch',
					                          'query', $6, 'hand', 'twoai_gov_watch ' || $7 || ', official release')
					WHERE NOT EXISTS (SELECT 1 FROM pipeline.documents WHERE url=$2)`,
					sourceID, link, title, date, publisherFromURL(link), f.name, f.code); err == nil {
					if k, _ := r2.RowsAffected(); k > 0 {
						docs++
					}
				}
			}
		}
		db.Exec(`UPDATE twoai_gov_feeds SET probed_at=now(), probe_status=200, probe_note='ok', last_items=$2, last_ai=$3, last_ok_at=now() WHERE code=$1`,
			f.code, len(items), ai)
	}
	sort.Strings(deadNotes)
	fmt.Printf("twoai_gov_watch: feeds=%d alive=%d dead=%d actions_live=%d candidates=%d documents=%d\n",
		len(feeds), alive, dead, live, cands, docs)
	if len(deadNotes) > 0 {
		fmt.Printf("twoai_gov_watch: no usable feed: %s\n", strings.Join(deadNotes, "; "))
	}
	return nil
}
