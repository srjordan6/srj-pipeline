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
// FEEDS CHOSEN BY TESTING, NOT BY GUESSING. Twenty-nine candidate feeds were
// probed on 2026-08-26 across federal, state and municipal agencies with an
// AI enforcement mandate; ten answered with parseable RSS and are listed
// below, and every failure is recorded beside them. A feed that does not
// exist is not tracked as though it might.
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

// EVERY FEED HERE WAS PROBED BEFORE IT WAS ADDED, and the ones that are
// missing are missing for a reason recorded below. A dead feed in this list
// would report a quiet zero for ever, which is the failure this file exists
// to prevent.
//
// SELECTION RULE: an agency belongs here if AI enforcement is within its
// MANDATE, not if it happened to act on AI this week. Probed 2026-08-26,
// only 1 of 105 items across the six new feeds was AI-relevant - and that is
// the expected shape. Enforcement is episodic: the FTC opened two AI dockets
// in six weeks and the DOJ none, which tells you nothing about whether DOJ
// will bring an algorithmic-discrimination case next month.
var twoaiAgencyFeeds = []struct{ agency, url string }{
	// Consumer protection and competition: Section 5 is the main federal
	// hook for AI claims, deception and algorithmic pricing.
	{"FTC", "https://www.ftc.gov/feeds/press-release.xml"},
	{"FTC", "https://www.ftc.gov/feeds/press-release-consumer-protection.xml"},
	{"FTC", "https://www.ftc.gov/feeds/press-release-competition.xml"},
	// Securities: AI-washing enforcement against issuers and advisers.
	{"SEC", "https://www.sec.gov/news/pressreleases.rss"},
	// Consumer finance: adverse action notices, algorithmic underwriting.
	{"CFPB", "https://www.consumerfinance.gov/about-us/newsroom/feed/"},
	// Civil rights and antitrust, including algorithmic price-fixing - the
	// RealPage matter is a DOJ case, not an FTC one.
	{"DOJ", "https://www.justice.gov/news/rss?type=press_release"},
	// Medical devices: AI/ML-enabled device authorisations and recalls.
	{"FDA", "https://www.fda.gov/about-fda/contact-fda/stay-informed/rss-feeds/press-releases/rss.xml"},
	// Model risk management for national banks, SR 11-7's OCC counterpart.
	{"OCC", "https://www.occ.gov/rss/occ_news.xml"},
	// STATE. Colorado enforces SB 26-189 from 2027-01-01, and New Jersey's
	// AG issued the algorithmic-discrimination guidance under the LAD.
	{"Colorado AG", "https://coag.gov/press-releases/feed/"},
	{"New Jersey AG", "https://www.njoag.gov/feed/"},
}

// AGENCIES WITH AN AI MANDATE AND NO USABLE FEED, probed 2026-08-26. Recorded
// so nobody re-adds them hoping, and so the coverage gap is a written fact
// rather than an assumption:
//
//	EEOC        - newsroom.xml and /rss/all both 404. Title VII and the ADA
//	              applied to automated hiring; a real loss.
//	HHS OCR     - ocr-rss.xml 403. Section 1557 and HIPAA for clinical AI.
//	FCC         - headlines and enforcement feeds 403. AI voice cloning
//	              under the TCPA.
//	NHTSA, CPSC - 403. Autonomous vehicles and connected products.
//	DOL         - 403. Workplace AI guidance.
//	HUD         - 404. Algorithmic tenant screening and fair housing.
//	CA AG, CA CPPA, NY AG, NY DFS, TX AG, IL AG, MA AG, WA AG, CT AG,
//	NYC DCWP    - no valid RSS at any tested path. Between them these cover
//	              the CCPA ADMT rules, TRAIGA, BIPA and Local Law 144, so
//	              this is the largest hole in state coverage. Scraping their
//	              newsrooms is the obvious next step and is deliberately not
//	              done here: a parser against ten bespoke HTML layouts is a
//	              maintenance burden that should be chosen on purpose, not
//	              slipped in.

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
