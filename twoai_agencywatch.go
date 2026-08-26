package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// WHAT THE AGENCIES DO, NOT ONLY WHAT THEY PUBLISH IN THE FEDERAL REGISTER.
//
// A governance weekly on 2026-08-26 named the FTC's request for comment on an
// enforcement policy statement for personalized pricing, opened 19 August, as
// a high-relevance item. It was not in this corpus, and checking why produced
// a better answer than the one expected: the Federal Register does not have
// it either. Searching the FR API for "personalized pricing" returns nothing
// from August 2026. The item is an FTC press release and a comment docket.
//
// So the gap was never the Federal Register filter, which works. The gap was
// that ENFORCEMENT ITSELF WAS NOT A SOURCE. Consent orders, policy
// statements, comment dockets, settlements and complaints are how agencies
// actually shape what Section 5, Title VII or the FCRA mean for an AI system,
// and they appear in agency press feeds days or weeks before any Federal
// Register notice, if one ever comes.
//
// FEEDS CHOSEN BY TESTING, NOT BY GUESSING. Checked 2026-08-26: the FTC
// press-release feed and its consumer-protection feed both answer 200 and
// carry the personalized-pricing item; SEC returns 25 items; CFPB answers but
// carries one. EEOC's newsroom feed 404s and HHS OCR's 403s, so neither is
// listed - a feed that does not exist is not tracked as though it might.
// Those two agencies stay a manual gap, recorded here rather than pretended
// away.
//
// THE SAME RELEVANCE RULE AS THE FEDERAL REGISTER STAGE. An agency publishes
// constantly and most of it has nothing to do with AI, so a release earns a
// row only when an AI term appears in its title or summary. mentionsAI is
// shared with the Federal Register stage on purpose: one definition of what
// counts, not two that drift.

// Named practices that are algorithmic in substance whether or not the agency
// says so. Kept short on purpose; every addition should be justified by an
// action it would have caught.
var twoaiAgencyAdjacent = regexp.MustCompile(`(?i)\b(personalized pricing|surveillance pricing|dynamic pricing|automated system|dark pattern|facial recognition|biometric|predictive analytic)`)

var twoaiAgencyFeeds = []struct{ agency, url string }{
	{"FTC", "https://www.ftc.gov/feeds/press-release.xml"},
	{"FTC", "https://www.ftc.gov/feeds/press-release-consumer-protection.xml"},
	{"SEC", "https://www.sec.gov/news/pressreleases.rss"},
	{"CFPB", "https://www.consumerfinance.gov/about-us/newsroom/feed/"},
}

func twoaiAgencyWatch(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_agency_actions (
		uid text PRIMARY KEY,
		agency text NOT NULL,
		title text NOT NULL,
		url text NOT NULL,
		summary text,
		published_on date,
		matched_terms text,
		first_seen timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("agency_watch create: %w", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS twoai_agency_actions_pub ON twoai_agency_actions (published_on DESC)`)

	client := &http.Client{Timeout: 45 * time.Second}
	seen, kept := 0, 0
	byAgency := map[string]int{}
	var failed []string

	for _, f := range twoaiAgencyFeeds {
		req, _ := http.NewRequest("GET", f.url, nil)
		req.Header.Set("User-Agent", "srj-pipeline/1.0 (srjconsultingservices.com)")
		resp, err := client.Do(req)
		if err != nil {
			failed = append(failed, f.agency+" "+err.Error())
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		code := resp.StatusCode
		resp.Body.Close()
		if code != 200 {
			failed = append(failed, fmt.Sprintf("%s http %d", f.agency, code))
			continue
		}

		var feed struct {
			Items []struct {
				Title       string `xml:"title"`
				Link        string `xml:"link"`
				Description string `xml:"description"`
				PubDate     string `xml:"pubDate"`
			} `xml:"channel>item"`
		}
		dec := xml.NewDecoder(strings.NewReader(string(body)))
		dec.Strict = false
		dec.Entity = xml.HTMLEntity
		if err := dec.Decode(&feed); err != nil {
			failed = append(failed, f.agency+" parse: "+err.Error())
			continue
		}

		for _, it := range feed.Items {
			seen++
			title := strings.Join(strings.Fields(it.Title), " ")
			summary := strings.Join(strings.Fields(stripTags(it.Description)), " ")
			if title == "" || it.Link == "" {
				continue
			}
			// Title or summary only. An agency mentioning AI in the body of a
			// merger release is not an AI enforcement action.
			//
			// TWO TESTS, AND THE SECOND ONE EARNED ITS PLACE. The FTC opened
			// two comment dockets that a governance weekly flagged as high
			// relevance. mentionsAI caught the one titled "Policy Statement
			// Addressing AI Accuracy" and missed "Enforcement Policy
			// Statement Regarding Personalized Pricing" - because the FTC
			// never uses the word AI in that release, though the practice is
			// algorithmic by definition. A governance corpus that only
			// notices enforcement when the agency says "AI" will miss the
			// half that matters most.
			//
			// The adjacency list is deliberately SHORT and concrete: named
			// practices that are algorithmic in substance. Measured against
			// the live feeds it added exactly one item to 86 scanned, so it
			// is a scalpel and not a floodgate. Which test fired is stored,
			// so a reviewer can see whether an action was self-described as
			// AI or caught by inference.
			matched := ""
			switch {
			case mentionsAI(title) || mentionsAI(summary):
				matched = "ai_term"
			case twoaiAgencyAdjacent.MatchString(title) || twoaiAgencyAdjacent.MatchString(summary):
				matched = "ai_adjacent_practice"
			default:
				continue
			}
			// The uid is the link, hashed: agencies reuse titles across years
			// and the link is the thing that identifies the action.
			h := sha256.Sum256([]byte(it.Link))
			uid := hex.EncodeToString(h[:])[:16]
			var pub any
			if t, err := parseFeedDate(it.PubDate); err == nil {
				pub = t.Format("2006-01-02")
			}
			if len(summary) > 2000 {
				summary = summary[:2000]
			}
			res, err := db.Exec(`INSERT INTO twoai_agency_actions
				(uid, agency, title, url, summary, published_on, matched_terms)
				VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,'')::date,$7)
				ON CONFLICT (uid) DO NOTHING`,
				uid, f.agency, title, it.Link, summary, pub, matched)
			if err != nil {
				fmt.Fprintln(os.Stderr, "agency_watch insert:", err)
				continue
			}
			if n, _ := res.RowsAffected(); n > 0 {
				kept++
				byAgency[f.agency]++
			}
		}
		time.Sleep(1200 * time.Millisecond) // courtesy between agencies
	}

	var total int
	db.QueryRow(`SELECT count(*) FROM twoai_agency_actions`).Scan(&total)
	parts := []string{}
	for a, n := range byAgency {
		parts = append(parts, fmt.Sprintf("%s=%d", a, n))
	}
	fmt.Printf("agency_watch: scanned=%d new=%d total=%d %s\n",
		seen, kept, total, strings.Join(parts, " "))
	// Say which feed broke rather than reporting a quiet zero: EEOC and HHS
	// OCR are already known-missing, and a fifth silent failure would be
	// indistinguishable from a quiet week.
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "agency_watch: %d feed(s) unavailable: %s\n",
			len(failed), strings.Join(failed, "; "))
	}
	return nil
}

// parseFeedDate accepts the handful of shapes these feeds actually use.
func parseFeedDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822,
		"2006-01-02T15:04:05Z07:00", "2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparsed date %q", s)
}

// stripTags removes markup from a feed description. Agency feeds put escaped
// HTML in the description, and the AI test should read the words, not the
// tags around them.
var twoaiTagRe = regexp.MustCompile(`<[^>]*>`)

func stripTags(s string) string { return twoaiTagRe.ReplaceAllString(s, " ") }
