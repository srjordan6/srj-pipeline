package main

// twoai_feed_discovery: a press feed for every company we track, found in the
// HTML we already have.
//
// Stephen, 2026-09-20: I want press reports from all the companies we track,
// as quick as possible. The instinct is to build something new, and the search
// results he was reading all say the same thing: a Zapier agent, an n8n
// workflow, a Python scraper.
//
// None of that is needed, because the system already exists and is running.
// twoai_vendor_feeds holds RSS feeds, twoai_vendor_news pulls them daily and
// renders 4,898 pages from a 6,060 item archive. It is proven. What it lacks
// is coverage: 32 feeds against 420 tracked companies, 27 of which are the
// same company by name. 378 companies have a website on file and no feed.
//
// AND THE FETCHING IS ALREADY DONE. twoai_company_harvest pulls 342 company
// home pages every single day. Their HTML arrives, gets reduced to a text
// extract, and the markup is thrown away. Nearly every site declares its feed
// in the head of that markup:
//
//	<link rel="alternate" type="application/rss+xml" href="/blog/rss.xml">
//
// So stage one costs NOTHING. No new request to anyone's server, no new user
// agent, no new politeness budget to manage. It reads bytes that were already
// on this machine and were being discarded. This is the same lesson as
// twoai_source_briefs.go, which was retired before first use when reading the
// existing code showed the fetching half already existed.
//
// STAGE TWO, the conventional paths, is the only part that costs requests, and
// it is bounded: a company with no declared feed is probed once across a short
// list of conventional paths, the result is recorded either way, and it is
// never probed again. A company that has no feed today and starts one next
// year is found by stage one, because its home page will then declare it.
//
// STAGE THREE, quarantine, exists because 378 new feeds will not all be press.
// Some will be a marketing blog, some a careers feed, some a podcast. A feed
// found by a machine does not go straight onto the site: it lands inactive
// with its first items sampled, and a person turns it on. That is the same
// contract as twoai_missing_entities, where the machine proposes and a person
// decides, and it is what kept "Inc." off the site.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

func twoaiFeedDiscoveryEnsure(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_feed_candidates (
		uid text PRIMARY KEY,
		name text NOT NULL,
		site text,
		feed_url text,
		found_via text,              -- 'declared' | 'probe:<path>'
		title text,                  -- the feed's own title, for the reviewer
		items int NOT NULL DEFAULT 0,
		newest date,                 -- most recent item, the freshness signal
		sample text,                 -- three recent headlines, so a person can judge
		status text NOT NULL DEFAULT 'proposed',   -- proposed | accepted | declined
		decided_on date,
		decided_note text,
		probed_on date,              -- set even when nothing is found, so we probe once
		checked_on date NOT NULL DEFAULT current_date)`)
	return err
}

// twoaiFeedLinkRe finds a declared feed in the document head. Deliberately
// permissive about attribute order, because half the web writes type before
// rel and half writes it after.
var twoaiFeedLinkRe = regexp.MustCompile(`(?is)<link[^>]+>`)
var twoaiFeedHrefRe = regexp.MustCompile(`(?is)href\s*=\s*["']([^"']+)["']`)
var twoaiFeedTypeRe = regexp.MustCompile(`(?is)type\s*=\s*["']application/(rss|atom)\+xml["']`)
var twoaiFeedRelRe = regexp.MustCompile(`(?is)rel\s*=\s*["'][^"']*alternate[^"']*["']`)

// Feeds we do not want even when a site declares them. A comments feed is not
// press, and a feed for one tag is narrower than the company.
var twoaiFeedRejectRe = regexp.MustCompile(`(?i)/comments/feed|comments-feed|/author/|/tag/|/category/|/podcast|itunes`)

// twoaiDiscoverFeedInHTML returns the absolute URL of the first usable feed a
// page declares, or empty. It is given the same bytes twoai_company_harvest
// already read.
func twoaiDiscoverFeedInHTML(pageURL string, body []byte) string {
	base, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	// Only the head matters and it keeps the scan cheap on a large page.
	s := string(body)
	if i := strings.Index(strings.ToLower(s), "</head>"); i > 0 {
		s = s[:i]
	}
	for _, tag := range twoaiFeedLinkRe.FindAllString(s, -1) {
		if !twoaiFeedTypeRe.MatchString(tag) || !twoaiFeedRelRe.MatchString(tag) {
			continue
		}
		m := twoaiFeedHrefRe.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		href := strings.TrimSpace(strings.ReplaceAll(m[1], "&amp;", "&"))
		if href == "" || twoaiFeedRejectRe.MatchString(href) {
			continue
		}
		u, err := base.Parse(href)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		return u.String()
	}
	return ""
}

// twoaiFeedNote records a declared feed found during the company harvest. It
// is called from inside that stage's worker, with its mutex already held, so
// it does no locking of its own and never returns an error worth stopping for.
func twoaiFeedNote(db *sql.DB, uid, name, site, feedURL, via string) {
	if feedURL == "" {
		return
	}
	// A company that already has a live feed is not a candidate.
	var live bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM twoai_vendor_feeds
		WHERE lower(vendor)=lower($1) OR feed_url=$2)`, name, feedURL).Scan(&live)
	if live {
		return
	}
	db.Exec(`INSERT INTO twoai_feed_candidates (uid, name, site, feed_url, found_via, checked_on)
		VALUES ($1,$2,$3,$4,$5,current_date)
		ON CONFLICT (uid) DO UPDATE SET name=$2, site=$3, checked_on=current_date,
			-- A declared feed replaces one found by probing, and a changed
			-- declaration replaces the old one, but only while nobody has
			-- ruled on it. A decision is not overwritten by a later crawl.
			feed_url = CASE WHEN twoai_feed_candidates.status='proposed' THEN $4 ELSE twoai_feed_candidates.feed_url END,
			found_via = CASE WHEN twoai_feed_candidates.status='proposed' THEN $5 ELSE twoai_feed_candidates.found_via END`,
		uid, name, site, feedURL, via)
}

// The conventional paths, in the order they are worth trying. Ordered by how
// often they are the right answer, so most companies cost one or two requests
// rather than eight.
var twoaiFeedProbePaths = []string{
	"/feed", "/rss", "/blog/feed", "/blog/rss.xml", "/feed.xml",
	"/news/feed", "/press/rss", "/newsroom/rss",
}

// twoaiFeedProbe is stage two: for companies whose site declares nothing, try
// the conventional paths ONCE. probed_on is written whether or not anything is
// found, so nobody pays for this twice.
func twoaiFeedProbe(db *sql.DB, limit int) error {
	rows, err := db.Query(`
		SELECT c.uid, c.name, h.url
		FROM twoai_company_profiles c
		JOIN twoai_company_harvest h ON h.uid = c.uid AND h.http_status = 200 AND COALESCE(h.url,'') <> ''
		LEFT JOIN twoai_feed_candidates f ON f.uid = c.uid
		WHERE (f.uid IS NULL OR (f.feed_url IS NULL AND f.probed_on IS NULL))
		  AND NOT EXISTS (SELECT 1 FROM twoai_vendor_feeds v WHERE lower(v.vendor) = lower(c.name))
		ORDER BY c.name
		LIMIT $1`, limit)
	if err != nil {
		return err
	}
	type job struct{ uid, name, site string }
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.uid, &j.name, &j.site) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()

	client := &http.Client{Timeout: 15 * time.Second}
	found, probed := 0, 0
	for _, j := range jobs {
		base, err := url.Parse(j.site)
		if err != nil {
			continue
		}
		probed++
		hit, via := "", ""
		for _, p := range twoaiFeedProbePaths {
			u, err := base.Parse(p)
			if err != nil {
				continue
			}
			// One request per path, stopping at the first that is really a
			// feed. A 200 is not enough: a site that serves its home page for
			// every unknown path would otherwise give eight false feeds.
			req, _ := http.NewRequest("GET", u.String(), nil)
			req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; theworldofai.org company directory; info@srjconsultingservices.com)")
			req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9")
			resp, ferr := client.Do(req)
			if ferr != nil {
				continue
			}
			ok := false
			if resp.StatusCode == 200 {
				head, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				ok = twoaiLooksLikeFeed(head)
			}
			resp.Body.Close()
			if ok {
				hit, via = u.String(), "probe:"+p
				break
			}
			time.Sleep(400 * time.Millisecond)
		}
		if hit != "" {
			found++
			twoaiFeedNote(db, j.uid, j.name, j.site, hit, via)
		}
		// Written either way. A company with no feed is asked once, ever.
		db.Exec(`INSERT INTO twoai_feed_candidates (uid, name, site, probed_on, checked_on)
			VALUES ($1,$2,$3,current_date,current_date)
			ON CONFLICT (uid) DO UPDATE SET probed_on=current_date, checked_on=current_date`,
			j.uid, j.name, j.site)
		time.Sleep(700 * time.Millisecond)
	}
	fmt.Printf("twoai_feed_probe: probed=%d found=%d\n", probed, found)
	return nil
}

// twoaiLooksLikeFeed tests the first bytes of a response rather than trusting
// the status code or the content type, because plenty of sites answer every
// unknown path with their home page and a cheerful 200.
func twoaiLooksLikeFeed(head []byte) bool {
	s := strings.ToLower(strings.TrimSpace(string(head)))
	if len(s) > 2048 {
		s = s[:2048]
	}
	return strings.Contains(s, "<rss") || strings.Contains(s, "<feed") ||
		strings.Contains(s, "<rdf:rdf")
}

// twoaiFeedSample is stage three: read each proposed feed ONCE, record what it
// actually publishes, and leave it for a person. Nothing here activates a feed.
func twoaiFeedSample(db *sql.DB, limit int) error {
	rows, err := db.Query(`SELECT uid, name, feed_url FROM twoai_feed_candidates
		WHERE status='proposed' AND COALESCE(feed_url,'') <> '' AND items = 0
		ORDER BY name LIMIT $1`, limit)
	if err != nil {
		return err
	}
	type job struct{ uid, name, feed string }
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.uid, &j.name, &j.feed) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()

	client := &http.Client{Timeout: 20 * time.Second}
	ok, bad := 0, 0
	for _, j := range jobs {
		req, _ := http.NewRequest("GET", j.feed, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; theworldofai.org company directory; info@srjconsultingservices.com)")
		resp, ferr := client.Do(req)
		if ferr != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			bad++
			db.Exec(`UPDATE twoai_feed_candidates SET status='declined', decided_on=current_date,
				decided_note='feed did not answer when sampled' WHERE uid=$1`, j.uid)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()

		var doc struct {
			Channel struct {
				Title string `xml:"title"`
				Items []struct {
					Title string `xml:"title"`
					Date  string `xml:"pubDate"`
				} `xml:"item"`
			} `xml:"channel"`
			Title   string `xml:"title"`
			Entries []struct {
				Title   string `xml:"title"`
				Updated string `xml:"updated"`
			} `xml:"entry"`
		}
		if xml.Unmarshal(body, &doc) != nil {
			bad++
			db.Exec(`UPDATE twoai_feed_candidates SET status='declined', decided_on=current_date,
				decided_note='not parseable as RSS or Atom' WHERE uid=$1`, j.uid)
			continue
		}
		title := strings.TrimSpace(doc.Channel.Title)
		if title == "" {
			title = strings.TrimSpace(doc.Title)
		}
		var heads []string
		var newest time.Time
		take := func(t, d string) {
			if t = strings.TrimSpace(t); t != "" && len(heads) < 3 {
				heads = append(heads, t)
			}
			for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339, "2006-01-02"} {
				if p, err := time.Parse(layout, strings.TrimSpace(d)); err == nil {
					if p.After(newest) {
						newest = p
					}
					break
				}
			}
		}
		n := 0
		for _, it := range doc.Channel.Items {
			take(it.Title, it.Date)
			n++
		}
		for _, e := range doc.Entries {
			take(e.Title, e.Updated)
			n++
		}
		if n == 0 {
			bad++
			db.Exec(`UPDATE twoai_feed_candidates SET status='declined', decided_on=current_date,
				decided_note='feed parsed but carries no items' WHERE uid=$1`, j.uid)
			continue
		}
		ok++
		var newestArg any
		if !newest.IsZero() {
			newestArg = newest.Format("2006-01-02")
		}
		db.Exec(`UPDATE twoai_feed_candidates SET title=$2, items=$3, newest=$4,
			sample=$5, checked_on=current_date WHERE uid=$1`,
			j.uid, title, n, newestArg, strings.Join(heads, " | "))
		time.Sleep(700 * time.Millisecond)
	}
	fmt.Printf("twoai_feed_sample: sampled=%d unusable=%d\n", ok, bad)
	return nil
}

// twoaiFeedPromote moves accepted candidates into twoai_vendor_feeds, which is
// the table the working pull already reads. A person sets status='accepted';
// this only carries the row across. THE MACHINE NEVER ACCEPTS ITS OWN FIND.
func twoaiFeedPromote(db *sql.DB) error {
	res, err := db.Exec(`
		INSERT INTO twoai_vendor_feeds (vendor, feed_url, entity_uid, entity_kind, active)
		SELECT f.name, f.feed_url, f.uid, 'company', true
		FROM twoai_feed_candidates f
		WHERE f.status='accepted' AND COALESCE(f.feed_url,'') <> ''
		  AND NOT EXISTS (SELECT 1 FROM twoai_vendor_feeds v WHERE lower(v.vendor)=lower(f.name))
		ON CONFLICT (vendor) DO NOTHING`)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		fmt.Printf("twoai_feed_promote: %d accepted feed(s) now live\n", n)
	}
	return nil
}

// twoaiFeedDiscoveryReport is the one line worth reading in the log.
func twoaiFeedDiscoveryReport(db *sql.DB) {
	var live, prop, declined, noFeed, tracked int
	db.QueryRow(`SELECT count(*) FROM twoai_vendor_feeds WHERE active`).Scan(&live)
	db.QueryRow(`SELECT count(*) FROM twoai_feed_candidates WHERE status='proposed' AND COALESCE(feed_url,'') <> ''`).Scan(&prop)
	db.QueryRow(`SELECT count(*) FROM twoai_feed_candidates WHERE status='declined'`).Scan(&declined)
	db.QueryRow(`SELECT count(*) FROM twoai_feed_candidates WHERE feed_url IS NULL AND probed_on IS NOT NULL`).Scan(&noFeed)
	db.QueryRow(`SELECT count(*) FROM twoai_company_profiles`).Scan(&tracked)
	fmt.Printf("twoai_feed_discovery: %d live feeds, %d awaiting review, %d declined, %d companies have no feed to find, of %d tracked\n",
		live, prop, declined, noFeed, tracked)
	if prop > 0 {
		fmt.Fprintf(os.Stderr, "twoai_feed_discovery: %d feed(s) need a yes or no. SELECT name, title, newest, sample, feed_url FROM twoai_feed_candidates WHERE status='proposed' AND feed_url IS NOT NULL ORDER BY newest DESC NULLS LAST;\n", prop)
	}
}

// Unused, kept so the hash import is honest if the sampler later stores one.
var _ = sha256.Sum256
var _ = hex.EncodeToString
