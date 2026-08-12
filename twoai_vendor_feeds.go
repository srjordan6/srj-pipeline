package main

// twoai_vendor_feeds: first-party vendor announcements, straight from the
// companies' own feeds.
//
// The vendor page ran on nine sources because the feed list lived in Go, in
// the middle of the intel watch, and the vendor builder filtered it down to
// an allowlist. Meanwhile /companies/ tracked 62 organisations and only four
// of them had a single tracked post between them. The gap was configuration,
// not capability.
//
// Feeds now live in twoai_vendor_feeds, in SQL, so adding a vendor is an
// INSERT and not a deploy. Each row carries entity_uid, which is what lets a
// post on /ai-news/vendor/ link to that company's profile page: the two
// sections stop being separate lists of the same organisations.
//
// Twenty-seven of the 62 companies have a working first-party feed, verified
// by probing autodiscovery tags, common feed paths, and curated candidates.
// The other 35 publish no discoverable feed. They stay uncovered rather than
// being filled in from Google News query feeds, because those resolve to
// news.google.com redirects rather than publisher URLs, and every link this
// site publishes goes to the original source. The page says how many are
// covered so the gap is visible instead of implied.

import (
	"database/sql"
	"encoding/xml"
	"fmt"
	"html"
	"os"
	"regexp"
	"strings"
	"time"
)

// twoaiFeedMaxFailures is when a feed stops being tried. Feeds rot quietly —
// a blog moves, a path changes — and a dead feed that keeps being fetched
// every day forever is how a source list becomes fiction. Six consecutive
// failures is about a week of the daily cron.
const twoaiFeedMaxFailures = 6

type twoaiFeedItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	Published   string `xml:"published"`
	Updated     string `xml:"updated"`
	Date        string `xml:"date"`
	Summary     string `xml:"summary"`
	Content     string `xml:"encoded"`
	LinkAttr    []struct {
		Href string `xml:"href,attr"`
		Rel  string `xml:"rel,attr"`
	} `xml:"link"`
}

type twoaiFeedDoc struct {
	Channel struct {
		Items []twoaiFeedItem `xml:"item"`
	} `xml:"channel"`
	Entries []twoaiFeedItem `xml:"entry"`
}

var twoaiTagStrip = regexp.MustCompile(`<[^>]*>`)

// twoaiFeedDate parses the handful of date formats feeds actually use.
func twoaiFeedDate(vals ...string) string {
	layouts := []string{
		time.RFC1123Z, time.RFC1123, time.RFC3339, time.RFC822Z, time.RFC822,
		"2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05", "2006-01-02",
		"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
	}
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		for _, l := range layouts {
			if t, err := time.Parse(l, v); err == nil {
				return t.UTC().Format("2006-01-02")
			}
		}
	}
	return ""
}

// twoaiFeedSummary takes a short, plain-text abstract. Feed descriptions are
// the publisher's words, so this is capped hard: enough to tell a reader what
// the post is, never enough to stand in for reading it.
func twoaiFeedSummary(vals ...string) string {
	for _, v := range vals {
		s := strings.TrimSpace(html.UnescapeString(twoaiTagStrip.ReplaceAllString(v, " ")))
		s = strings.Join(strings.Fields(s), " ")
		if len(s) < 40 {
			continue
		}
		if len(s) > 300 {
			cut := strings.LastIndex(s[:300], ". ")
			if cut < 120 {
				cut = strings.LastIndex(s[:300], " ")
			}
			if cut > 0 {
				s = s[:cut+1]
			} else {
				s = s[:300]
			}
			s = strings.TrimSpace(s)
		}
		return s
	}
	return ""
}

func twoaiVendorFeeds(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_vendor_feeds (
		vendor text PRIMARY KEY,
		feed_url text NOT NULL,
		entity_uid text,
		entity_kind text NOT NULL DEFAULT 'company',
		active boolean NOT NULL DEFAULT true,
		last_ok timestamptz,
		last_error text,
		consecutive_failures int NOT NULL DEFAULT 0,
		created_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_vendor_posts (
		slug text PRIMARY KEY, vendor text NOT NULL, title text NOT NULL,
		url text NOT NULL, summary text NOT NULL DEFAULT '', posted_on date,
		first_published timestamptz NOT NULL DEFAULT now(),
		last_seen timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE twoai_vendor_posts
		ADD COLUMN IF NOT EXISTS entity_uid text,
		ADD COLUMN IF NOT EXISTS entity_kind text,
		ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT 'intel'`); err != nil {
		return err
	}

	rows, err := db.Query(`SELECT vendor, feed_url, COALESCE(entity_uid,''), entity_kind
		FROM twoai_vendor_feeds WHERE active AND consecutive_failures < $1 ORDER BY vendor`,
		twoaiFeedMaxFailures)
	if err != nil {
		return err
	}
	type feed struct{ vendor, url, uid, kind string }
	feeds := []feed{}
	for rows.Next() {
		var f feed
		if err := rows.Scan(&f.vendor, &f.url, &f.uid, &f.kind); err != nil {
			rows.Close()
			return err
		}
		feeds = append(feeds, f)
	}
	rows.Close()

	saved, failed := 0, 0
	for _, f := range feeds {
		body, err := twoaiJobsGet(f.url, nil)
		if err != nil {
			failed++
			db.Exec(`UPDATE twoai_vendor_feeds SET last_error=$2,
				consecutive_failures = consecutive_failures + 1 WHERE vendor=$1`,
				f.vendor, err.Error())
			fmt.Fprintf(os.Stderr, "twoai_vendor_feeds: %s: %v\n", f.vendor, err)
			continue
		}
		var doc twoaiFeedDoc
		if err := xml.Unmarshal(body, &doc); err != nil {
			failed++
			db.Exec(`UPDATE twoai_vendor_feeds SET last_error=$2,
				consecutive_failures = consecutive_failures + 1 WHERE vendor=$1`,
				f.vendor, "parse: "+err.Error())
			fmt.Fprintf(os.Stderr, "twoai_vendor_feeds: %s parse: %v\n", f.vendor, err)
			continue
		}
		items := doc.Channel.Items
		if len(items) == 0 {
			items = doc.Entries
		}
		n := 0
		for _, it := range items {
			link := strings.TrimSpace(it.Link)
			if link == "" {
				for _, la := range it.LinkAttr {
					if la.Rel == "" || la.Rel == "alternate" {
						link = strings.TrimSpace(la.Href)
						break
					}
				}
			}
			title := strings.TrimSpace(html.UnescapeString(twoaiTagStrip.ReplaceAllString(it.Title, "")))
			if title == "" || link == "" || strings.Contains(link, "news.google.") {
				continue
			}
			slug := twoaiPostSlug(title, link)
			date := twoaiFeedDate(it.PubDate, it.Published, it.Updated, it.Date)
			summary := twoaiFeedSummary(it.Description, it.Summary, it.Content)
			var posted any
			if date != "" {
				posted = date
			}
			// first_published and posted_on are written once. A permalink's
			// date must not move because a feed re-dated an old post.
			if _, err := db.Exec(`INSERT INTO twoai_vendor_posts
				(slug, vendor, title, url, summary, posted_on, entity_uid, entity_kind, source)
				VALUES ($1,$2,$3,$4,$5,$6::date,$7,$8,'feed')
				ON CONFLICT (slug) DO UPDATE SET
					vendor=EXCLUDED.vendor, title=EXCLUDED.title, url=EXCLUDED.url,
					summary=CASE WHEN EXCLUDED.summary <> '' THEN EXCLUDED.summary
					             ELSE twoai_vendor_posts.summary END,
					entity_uid=COALESCE(EXCLUDED.entity_uid, twoai_vendor_posts.entity_uid),
					entity_kind=COALESCE(EXCLUDED.entity_kind, twoai_vendor_posts.entity_kind),
					source='feed', last_seen=now()`,
				slug, f.vendor, title, link, summary, posted,
				nullIfEmpty(f.uid), nullIfEmpty(f.kind)); err != nil {
				fmt.Fprintln(os.Stderr, "twoai_vendor_feeds: upsert:", err)
				continue
			}
			n++
			saved++
		}
		db.Exec(`UPDATE twoai_vendor_feeds SET last_ok=now(), last_error=NULL,
			consecutive_failures=0 WHERE vendor=$1`, f.vendor)
		_ = n
	}

	// Feeds that have failed their way out of the rotation. Named, because a
	// silently shrinking source list is the failure this table exists to
	// prevent.
	var retired []string
	rrows, err := db.Query(`SELECT vendor FROM twoai_vendor_feeds
		WHERE active AND consecutive_failures >= $1 ORDER BY vendor`, twoaiFeedMaxFailures)
	if err == nil {
		for rrows.Next() {
			var v string
			if rrows.Scan(&v) == nil {
				retired = append(retired, v)
			}
		}
		rrows.Close()
	}
	if len(retired) > 0 {
		fmt.Fprintf(os.Stderr, "twoai_vendor_feeds: %d feed(s) have failed %d runs running and are no longer "+
			"being fetched, fix the URL or set active=false: %s\n",
			len(retired), twoaiFeedMaxFailures, strings.Join(retired, ", "))
	}

	var total int
	db.QueryRow(`SELECT count(*) FROM twoai_vendor_posts`).Scan(&total)
	fmt.Printf("twoai_vendor_feeds: feeds=%d ok=%d failed=%d posts_seen=%d archive=%d ok=true\n",
		len(feeds), len(feeds)-failed, failed, saved, total)
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
