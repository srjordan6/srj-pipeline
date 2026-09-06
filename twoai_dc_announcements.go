package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// THE NEWS INTAKE FEEDS THE REGISTRIES.
//
// On 2026-09-06 Stephen read the morning briefing: TCS HyperVault signs an
// MoU with Telangana for a 1 GW, 250-acre, Rs 70,000 crore AI campus in
// Hyderabad. The intake had captured it from five outlets. The briefing
// summarised it well. And then it stopped: no facility row, no operator row,
// nothing on the siting page. 384 data-centre stories in the previous seven
// days and 19 announcements in thirty, each read once for a briefing and
// never again. His question was whether the site mines its own feed. It did
// not. This stage does.
//
// It reads the full text the intake already fetched - no search, no API
// spend - and recognises the announcement shape: an operator, a place, and
// at least one hard figure (capacity in MW or GW, acreage, square footage,
// or investment). A story with that shape becomes a press: facility row
// with status "announced", holding exactly what the text states and
// nothing inferred, sourced to the story, ready for the municipal method.
// The Marysville row made by hand on 2026-09-05 is what this produces
// automatically.
//
// WHAT IT WILL NOT DO. Infer capacity from investment. Guess coordinates -
// a row without them stays unmapped until a person geocodes the stated
// address. Merge with an existing facility by name - a new press row that
// duplicates a mapped one is caught by the duplicate audit and marked, not
// silently absorbed. Store the article text - the row holds figures and a
// citation; the prose belongs to the publisher.

type dcAnnouncement struct {
	Operator, Parent, Place, State, Country string
	CapacityMW                              float64
	Acres, Sqft                             float64
	InvestmentUSD                           float64
	InvestmentText                          string
	Jobs                                    int
	Action                                  string
	Buildings                               int
}

var (
	reMW      = regexp.MustCompile(`(?i)\b(\d[\d,.]*)\s*(gigawatt|GW|megawatt|MW)\b`)
	reAcres   = regexp.MustCompile(`(?i)\b(\d[\d,.]*)\s*-?\s*acre`)
	reSqft    = regexp.MustCompile(`(?i)\b(\d[\d,.]*)\s*(million\s+)?(square\s+feet|sq\.?\s*ft|sqft)`)
	reUSD     = regexp.MustCompile(`(?i)\$\s?(\d[\d,.]*)\s*(billion|bn|million|mn|m)\b`)
	reINR     = regexp.MustCompile(`(?i)(?:Rs\.?|₹|INR)\s?(\d[\d,.]*)\s*(crore|lakh\s+crore)`)
	reGBP     = regexp.MustCompile(`(?i)£\s?(\d[\d,.]*)\s*(billion|bn|million|m)\b`)
	reEUR     = regexp.MustCompile(`(?i)€\s?(\d[\d,.]*)\s*(billion|bn|million|m)\b`)
	reJobs    = regexp.MustCompile(`(?i)\b(\d[\d,]*)\s+(?:direct\s+(?:and\s+indirect\s+)?)?(?:jobs|employment\s+opportunities|positions)\b`)
	reBldgs   = regexp.MustCompile(`(?i)\b(\d+|two|three|four|five|six|seven|eight|nine|ten|twelve|thirteen)[\s-]*(?:data[\s-]+cent(?:er|re)[\s-]+)?buildings?\b`)
	reAction  = regexp.MustCompile(`(?i)\b(signs?\s+(?:an?\s+)?(?:MoU|memorandum)|breaks?\s+ground|announce[sd]?|approve[sd]?|gets?\s+(?:go-ahead|approval|green\s*light)|acquire[sd]?|to\s+build|will\s+build|plans?\s+to\s+build|unveil[sd]?|secure[sd]?\s+approval|greenlit|granted\s+permission|wins?\s+approval)\b`)
	reSubject = regexp.MustCompile(`(?i)\bdata\s+cent(?:er|re)s?\b|\bAI\s+campus\b|\bhyperscale\b`)
	// Operator: capitalised run immediately before an action verb, or
	// "X's Y" / "X subsidiary Y". Conservative on purpose.
	reOperator = regexp.MustCompile(`([A-Z][A-Za-z0-9&.\-]+(?:\s+[A-Z][A-Za-z0-9&.\-]+){0,3})(?:'s|\s+subsidiary|\s+unit)?\s+([A-Z][A-Za-z0-9&.\-]+(?:\s+[A-Z][A-Za-z0-9&.\-]+){0,2})?\s*(?:signs?|breaks?|announce|gets?|to build|will build|plans|unveil|acquire|wins?|secures?)`)
	rePlaceIn  = regexp.MustCompile(`\bin ([A-Z][a-zA-Z.\-]+(?: [A-Z][a-zA-Z.\-]+){0,2})(?:, ([A-Z][a-zA-Z.\-]+(?: [A-Z][a-zA-Z.\-]+){0,2}))?`)
	wordNum    = map[string]int{"two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "twelve": 12, "thirteen": 13}
)

func numOf(s string) float64 {
	v, _ := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	return v
}

// twoaiParseAnnouncement returns nil unless the text has the announcement
// shape: subject, action, and at least one hard figure.
func twoaiParseAnnouncement(title, text string) *dcAnnouncement {
	t := title + "\n" + text
	if !reSubject.MatchString(t) || !reAction.MatchString(title) {
		return nil
	}
	a := &dcAnnouncement{Action: strings.ToLower(reAction.FindString(title))}
	if m := reMW.FindStringSubmatch(t); m != nil {
		v := numOf(m[1])
		if strings.HasPrefix(strings.ToLower(m[2]), "g") {
			v *= 1000
		}
		if v > 0 && v < 20000 {
			a.CapacityMW = v
		}
	}
	if m := reAcres.FindStringSubmatch(t); m != nil {
		if v := numOf(m[1]); v > 0 && v < 100000 {
			a.Acres = v
		}
	}
	if m := reSqft.FindStringSubmatch(t); m != nil {
		v := numOf(m[1])
		if m[2] != "" {
			v *= 1_000_000
		}
		if v > 1000 && v < 50_000_000 {
			a.Sqft = v
		}
	}
	// Investment: USD directly; INR, GBP and EUR converted at a stated rate
	// and the original kept as text, because the conversion is ours and the
	// figure is theirs.
	if m := reUSD.FindStringSubmatch(t); m != nil {
		v := numOf(m[1])
		if strings.HasPrefix(strings.ToLower(m[2]), "b") {
			v *= 1e9
		} else {
			v *= 1e6
		}
		a.InvestmentUSD, a.InvestmentText = v, m[0]
	} else if m := reINR.FindStringSubmatch(t); m != nil {
		v := numOf(m[1]) * 1e7 // one crore = 10 million rupees
		if strings.Contains(strings.ToLower(m[2]), "lakh") {
			v *= 1e5
		}
		a.InvestmentUSD, a.InvestmentText = v/84.0, m[0]+" (converted at 84 INR/USD)"
	} else if m := reGBP.FindStringSubmatch(t); m != nil {
		v := numOf(m[1])
		if strings.HasPrefix(strings.ToLower(m[2]), "b") {
			v *= 1e9
		} else {
			v *= 1e6
		}
		a.InvestmentUSD, a.InvestmentText = v*1.27, m[0]+" (converted at 1.27 USD/GBP)"
	} else if m := reEUR.FindStringSubmatch(t); m != nil {
		v := numOf(m[1])
		if strings.HasPrefix(strings.ToLower(m[2]), "b") {
			v *= 1e9
		} else {
			v *= 1e6
		}
		a.InvestmentUSD, a.InvestmentText = v*1.08, m[0]+" (converted at 1.08 USD/EUR)"
	}
	if m := reJobs.FindStringSubmatch(t); m != nil {
		a.Jobs = int(numOf(m[1]))
	}
	if m := reBldgs.FindStringSubmatch(t); m != nil {
		if n, ok := wordNum[strings.ToLower(m[1])]; ok {
			a.Buildings = n
		} else {
			a.Buildings = int(numOf(m[1]))
		}
	}
	if a.CapacityMW == 0 && a.Acres == 0 && a.Sqft == 0 && a.InvestmentUSD == 0 {
		return nil // an announcement with no figure is a rumour
	}
	if m := reOperator.FindStringSubmatch(title); m != nil {
		a.Parent = strings.TrimSpace(m[1])
		if m[2] != "" {
			a.Operator = strings.TrimSpace(m[2])
		} else {
			a.Operator, a.Parent = a.Parent, ""
		}
	}
	if m := rePlaceIn.FindStringSubmatch(t); m != nil {
		a.Place = m[1]
		if m[2] != "" {
			a.State = m[2]
		}
	}
	if a.Operator == "" || a.Place == "" {
		return nil // a figure without an operator and a place is not a facility
	}
	return a
}

func twoaiDcAnnouncements(db *sql.DB) error {
	rows, err := db.Query(`SELECT d.id, d.url, d.title, COALESCE(d.fulltext,''), COALESCE(d.published_at::date::text,'')
		FROM pipeline.documents d
		WHERE d.fetched_at > now() - interval '30 days'
		  AND (d.title ILIKE '%data cent%' OR d.title ILIKE '%AI campus%')
		  AND COALESCE(d.fulltext,'') <> ''
		  AND NOT EXISTS (SELECT 1 FROM twoai_dc_announcements a WHERE a.document_id = d.id)
		ORDER BY d.published_at DESC NULLS LAST LIMIT 200`)
	if err != nil {
		return err
	}
	type doc struct {
		id                        int64
		url, title, text, pubDate string
	}
	var docs []doc
	for rows.Next() {
		var d doc
		if rows.Scan(&d.id, &d.url, &d.title, &d.text, &d.pubDate) == nil {
			docs = append(docs, d)
		}
	}
	rows.Close()

	created, seen, skipped := 0, 0, 0
	today := time.Now().UTC().Format("2006-01-02")
	for _, d := range docs {
		a := twoaiParseAnnouncement(d.title, d.text)
		if a == nil {
			// Record the look so it is not repeated, with the reason.
			db.Exec(`INSERT INTO twoai_dc_announcements (document_id, url, title, outcome) VALUES ($1,$2,$3,'no-shape') ON CONFLICT DO NOTHING`, d.id, d.url, d.title)
			skipped++
			continue
		}
		// One facility per operator+place, however many outlets carried it.
		fid := "press:" + twoaiSlug(a.Operator+" "+a.Place)
		var exists bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM twoai_dc_facilities WHERE id = $1)`, fid).Scan(&exists)
		if exists {
			db.Exec(`INSERT INTO twoai_dc_announcements (document_id, url, title, facility_id, outcome) VALUES ($1,$2,$3,$4,'already-known') ON CONFLICT DO NOTHING`, d.id, d.url, d.title, fid)
			// Add the outlet as a further source on the existing row.
			db.Exec(`UPDATE twoai_dc_facilities SET profile = profile || jsonb_build_object('sources',
				COALESCE(profile->'sources','[]'::jsonb) || $2::jsonb) WHERE id = $1
				AND NOT (profile->'sources')::text LIKE '%' || $3 || '%'`,
				fid, mustJSON([]map[string]string{{"title": d.title, "publisher": publisherFromURL(d.url), "date": d.pubDate, "url": d.url}}), d.url)
			seen++
			continue
		}
		name := a.Operator + " " + a.Place
		profile := map[string]any{
			"announced_on":       d.pubDate,
			"announcement_type":  a.Action,
			"ownership_verified_on": today,
			"sources": []map[string]string{{"title": d.title, "publisher": publisherFromURL(d.url), "date": d.pubDate, "url": d.url}},
			"research_note": fmt.Sprintf("[%s, pipeline] Created from the news intake by twoai_dc_announcements. Every figure here is as stated in the cited report; nothing is inferred. Coordinates are not set - geocode the stated address before mapping. Status is announced, not operating.", today),
			"capacity_note": "Figures are from an announcement, not from an operator specification or a filing. Announced capacity is a plan. Investment is not capacity and neither is floor area.",
		}
		if a.Parent != "" {
			profile["parent_company"] = a.Parent
		}
		if a.CapacityMW > 0 {
			profile["planned_it_capacity_mw"] = a.CapacityMW
		}
		if a.Acres > 0 {
			profile["acres"] = a.Acres
		}
		if a.Sqft > 0 {
			profile["building_sqft"] = a.Sqft
		}
		if a.InvestmentUSD > 0 {
			profile["investment_usd"] = a.InvestmentUSD
			profile["investment_as_stated"] = a.InvestmentText
		}
		if a.Jobs > 0 {
			profile["jobs_announced"] = a.Jobs
		}
		if a.Buildings > 0 {
			profile["buildings_planned"] = a.Buildings
		}
		pj, _ := json.Marshal(profile)
		country := "US"
		if strings.Contains(a.InvestmentText, "INR") {
			country = "IN"
		} else if strings.Contains(a.InvestmentText, "GBP") {
			country = "GB"
		} else if strings.Contains(a.InvestmentText, "EUR") {
			country = "EU"
		}
		if _, err := db.Exec(`INSERT INTO twoai_dc_facilities (id, src, name, operator, city, state, country, status, profile)
			VALUES ($1, 'press', $2, $3, $4, $5, $6, 'announced', $7::jsonb) ON CONFLICT (id) DO NOTHING`,
			fid, name, a.Operator, a.Place, a.State, country, string(pj)); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_dc_announcements: insert", fid, err)
			continue
		}
		// The operator, if unknown.
		db.Exec(`INSERT INTO twoai_dc_operators (uid, name, operator_type, profile)
			SELECT $1, $2, 'unclassified', jsonb_build_object('first_seen', 'news intake', 'first_seen_on', $3::text, 'parent_company', $4::text)
			WHERE NOT EXISTS (SELECT 1 FROM twoai_dc_operators WHERE lower(name) = lower($2))`,
			twoaiUID("dc-op:"+a.Operator), a.Operator, today, a.Parent)
		db.Exec(`INSERT INTO twoai_dc_announcements (document_id, url, title, facility_id, outcome) VALUES ($1,$2,$3,$4,'created') ON CONFLICT DO NOTHING`, d.id, d.url, d.title, fid)
		created++
		fmt.Printf("twoai_dc_announcements: created %s | %s | %.0f MW, %.0f acres, $%.0fM | %s\n",
			fid, a.Action, a.CapacityMW, a.Acres, a.InvestmentUSD/1e6, publisherFromURL(d.url))
	}
	fmt.Printf("twoai_dc_announcements: read=%d created=%d added_source_to_known=%d no_shape=%d\n", len(docs), created, seen, skipped)
	return nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
