package main

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// SITING NEWS THAT REACHES THE SITING PAGE.
//
// The policy-news intake shipped 2026-09-04 already searches for "data center
// moratorium" and by the next day had captured Orange County weighing a
// moratorium on AI data centre applications, Kalamazoo adding one to its
// zoning code, Hochul's moratorium in New York, a South Dakota utility
// regulator race turning on a data centre pause, and an Eastport council
// meeting on an underwater data centre. All of it landed in the daily
// briefing and stopped there.
//
// That is the same dependency on luck the link checker removed: a section on
// siting and power law would go stale unless somebody happened to read the
// briefing and act. This stage closes it in two directions at once.
//
// A, immediately: matching stories go to twoai_siting_watch and render on the
// siting page as recent activity, so the page is current the day a moratorium
// is reported rather than whenever it is next edited.
//
// B, as work: an unworked row is a queue item. Cowork reads the ordinance,
// adds it to the regime sections, updates any facility row in that
// jurisdiction, and stamps worked_on. The watch list is what the page shows;
// the ordinance in the page body is what makes it a reference rather than a
// feed.
//
// The match is deliberately narrow. "AI moratorium" in a school district is
// not siting law, and America's Two Largest School Districts Impose AI
// Moratoriums was in the first day's catch: a story must name data centre
// infrastructure AND a siting, zoning, utility or permitting action.

var twoaiSitingSubject = regexp.MustCompile(`(?i)\bdata ?cent(er|re)s?\b|\bserver farm\b|\bhyperscale\b`)

var twoaiSitingAction = map[string]*regexp.Regexp{
	"moratorium":  regexp.MustCompile(`(?i)\bmoratori(um|a)\b|\bpause\b|\bhalt\b|\bfreeze\b`),
	"zoning":      regexp.MustCompile(`(?i)\bzoning\b|\brezon|\bordinance\b|\bland use\b|\bcomprehensive plan\b|\bconditional use\b|\bvariance\b`),
	"siting":      regexp.MustCompile(`(?i)\bsiting board\b|\bpower siting\b|\bcertificate of (public )?need\b|\bpermit(ting|ted)?\b`),
	"utility":     regexp.MustCompile(`(?i)\butility commission\b|\bpublic service commission\b|\bpuc\b|\bratepayer|\brate case\b|\binterconnection\b|\btariff\b`),
	"tax":         regexp.MustCompile(`(?i)\babatement\b|\btax exemption\b|\bincentive agreement\b|\bpilot agreement\b`),
	"prohibition": regexp.MustCompile(`(?i)\bban(s|ned|ning)?\b|\bprohibit`),
}

func twoaiSitingWatch(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_siting_watch (
		url text PRIMARY KEY, title text NOT NULL, publisher text, published_on date,
		jurisdiction text, kind text, first_seen timestamptz NOT NULL DEFAULT now(),
		worked_on date, worked_note text)`); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT url, title, COALESCE(published_at::date::text,'')
		FROM pipeline.documents
		WHERE raw::text LIKE '%"intake": "policy-news"%'
		  AND fetched_at > now() - interval '30 days'
		  AND NOT EXISTS (SELECT 1 FROM twoai_siting_watch w WHERE w.url = documents.url)`)
	if err != nil {
		return err
	}
	type cand struct{ url, title, pub string }
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.url, &c.title, &c.pub) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()

	added := 0
	for _, c := range cands {
		if !twoaiSitingSubject.MatchString(c.title) {
			continue
		}
		kind := ""
		for k, re := range twoaiSitingAction {
			if re.MatchString(c.title) {
				if kind == "" || k == "moratorium" || k == "prohibition" {
					kind = k
				}
			}
		}
		if kind == "" {
			continue
		}
		var pub any
		if c.pub != "" {
			pub = c.pub
		}
		if _, err := db.Exec(`INSERT INTO twoai_siting_watch (url, title, publisher, published_on, jurisdiction, kind)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (url) DO NOTHING`,
			c.url, strings.TrimSpace(c.title), publisherFromURL(c.url), pub,
			twoaiSitingJurisdiction(c.title), kind); err == nil {
			added++
		}
	}

	var open, total int
	db.QueryRow(`SELECT count(*) FILTER (WHERE worked_on IS NULL), count(*) FROM twoai_siting_watch`).Scan(&open, &total)
	fmt.Printf("twoai_siting_watch: new=%d unworked=%d total=%d\n", added, open, total)
	if open > 0 {
		fmt.Printf("twoai_siting_watch: %d siting stor%s await an ordinance read for /ai-compliance/datacenter-siting-and-power/\n",
			open, map[bool]string{true: "y", false: "ies"}[open == 1])
	}
	return nil
}

// twoaiSitingJurisdiction pulls a place out of the headline when the wire
// convention makes it safe: "Kalamazoo works to add..." or "Orange County to
// weigh...". Where it cannot, it returns empty rather than guessing, and the
// row still counts - the jurisdiction is for grouping on the page, not for
// any claim.
func twoaiSitingJurisdiction(title string) string {
	if m := regexp.MustCompile(`([A-Z][a-zA-Z.\-]+(?: [A-Z][a-zA-Z.\-]+){0,3} County)`).FindStringSubmatch(title); m != nil {
		return m[1]
	}
	if m := regexp.MustCompile(`^([A-Z][a-zA-Z.\-]+(?: [A-Z][a-zA-Z.\-]+){0,2}),? (?:to |works |weighs|considers|approves|rejects|adds|bans|pauses)`).FindStringSubmatch(title); m != nil {
		return m[1]
	}
	return ""
}
