package main

// THE GENERIC FACILITY PARSER: the 352 queued rows, not the one operator we
// wrote an adapter for.
//
// Stephen, 2026-08-30, looking at the Texas directory: are these going to be
// scraped? They were not. The queue counted 352 US facilities that carry an
// operator's own page URL and no specifications, and nothing worked it: the
// only filler was the CyrusOne adapter, which by design touches only its own
// twenty-five campuses.
//
// Writing sixty-two adapters is not the answer. Reading four operators'
// pages showed one shape underneath the branding: a short stat block of a
// number beside a unit label. What differs is the ORDER. Flexential and
// CyrusOne put the value first, "109,476" then "square-foot data center
// footprint"; CoreSite puts the label first, "SQUARE FOOTAGE" then
// "180,000+". So the parser reads pairs in both directions and takes the
// first that carries a unit it understands.
//
// What it refuses matters as much. A page with no number is recorded as an
// attempt with its reason, never as a facility with zeroes; a hub page
// listing forty sites is not a facility page and says so; and megawatts are
// only believed when a power word sits next to them, because "1,500 watts
// per square foot" and "40+ data centers" are also numbers on these pages.
// A row is written when EITHER capacity or floor area is found, because
// CoreSite publishes square footage and no megawatts, and half a
// specification from the operator is still the operator's specification.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The whole facility queue in one run: 352 pages at 1.5s is nine minutes,
// and this cron has nothing else to do. THIN_BUDGET_FACILITY overrides it.
var thinFacilityCap = thinBudget("FACILITY", 500)

var (
	tfMWRe     = regexp.MustCompile(`(?i)^([\d,]+(?:\.\d+)?)\s*(?:MW|megawatts?)\b`)
	tfMWAnyRe  = regexp.MustCompile(`(?i)\b([\d,]+(?:\.\d+)?)\s*(?:MW|megawatts?)\b`)
	tfNumRe    = regexp.MustCompile(`^([\d,]+(?:\.\d+)?)\+?$`)
	tfSqInRe   = regexp.MustCompile(`(?i)\b([\d,]{4,})\s*(?:\+\s*)?(?:square[- ]f(?:eet|oot)|sq\.?\s?ft\.?|sf)\b`)
	tfSqLblRe  = regexp.MustCompile(`(?i)square[- ]?f(?:eet|oot|ootage)|sq\.?\s?ft`)
	tfPwrLblRe = regexp.MustCompile(`(?i)critical (?:load|power|it)|it (?:load|capacity|power)|ups capacity|power capacity|total power|utility power`)
	tfZipRe    = regexp.MustCompile(`\b(\d{5})(?:-\d{4})?\b`)
	tfStreetRe = regexp.MustCompile(`\d+\s+\w`)
	tfFacPath  = regexp.MustCompile(`^https?://[^/]+/[^/]+/[^/?#]+`)
)

// The certification vocabulary, wider than the CyrusOne adapter's because
// these operators name more frameworks. Token scan only: attributes out, no
// operator prose in.
var tfCerts = []struct{ name, pat string }{
	{"SOC 1 Type 2", `SOC\s*1\s*type\s*(?:2|II)`}, {"SOC 2 Type 2", `SOC\s*2\s*type\s*(?:2|II)`},
	{"PCI DSS", `PCI[\s-]?DSS`}, {"HIPAA", `HIPAA`}, {"ISO 27001", `ISO\s*27001`},
	{"ISO 9001", `ISO\s*9001`}, {"FISMA", `FISMA`}, {"FedRAMP", `FedRAMP`},
	{"ITAR", `\bITAR\b`}, {"NIST 800-53", `NIST\s*800-53`}, {"HITRUST", `HITRUST`},
	{"SSAE 18", `SSAE\s*18`}, {"TIA-942", `TIA[- ]?942`},
	{"LEED", `\bLEED\b`}, {"Green Globes", `Green\s*Globe`},
}

type tfResult struct {
	mw   float64
	sqft int64
	// Figures the page publishes about the whole campus rather than this
	// building, kept apart so they are never attributed to one facility.
	campusMW   float64
	campusSqft int64
	mwLine     string
	sqftLine   string
	certs      []string
	postal  string
	address []string
}

func twoaiThinFillFacilities(db *sql.DB) {
	// One fetch per metro page per run, shared by every facility under it.
	metroCache := map[string][]tfMetroBlock{}
	metroFilled := 0
	due := thinDue(db, "dc-facility", thinFacilityCap)
	if len(due) == 0 {
		return
	}
	client := &http.Client{Timeout: 30 * time.Second}
	filled, empty, index := 0, 0, 0
	for _, c := range due {
		// A URL with no path beyond the domain, or one path segment, is an
		// operator's index, not a facility page. Nothing to read; say so once
		// rather than fetching sixty hub pages a run.
		if !tfFacPath.MatchString(c.source) {
			index++
			// Three attempts and it stops asking: an index URL will still be
			// an index URL tomorrow. These need a facility URL from the
			// operator, which is data work, not another fetch.
			db.Exec(`UPDATE twoai_thin_queue SET attempts=3, last_attempt=now(),
				last_error='the website on this row is an operator index, not a facility page; needs a facility URL'
				WHERE path=$1`, c.path)
			continue
		}
		time.Sleep(1500 * time.Millisecond)
		body, err := thinGet(client, c.source)
		if err != nil {
			// A 403 or 404 from the same URL on three separate runs is a
			// standing answer, not a bad day: the operator blocks non-browser
			// clients, or the page has moved and this row holds a dead link.
			// Retrying it forever spends requests to learn nothing.
			e := err.Error()
			switch {
			case strings.Contains(e, "HTTP 403"):
				thinPermanent(db, c.path, "the operator's site refuses automated requests, so its published figures cannot be read here")
			case strings.Contains(e, "HTTP 404"):
				thinPermanent(db, c.path, "the operator has moved or withdrawn this facility page; the link we hold is dead")
			case strings.Contains(e, "certificate"):
				thinPermanent(db, c.path, "the operator's site serves an invalid TLS certificate, so it cannot be read safely")
			default:
				thinAttempt(db, c.path, e)
			}
			continue
		}
		r, ok := tfParse(body)
		if !ok {
			empty++
			// "No capacity published" and "capacity published for the campus,
			// not this building" are different findings with different fixes,
			// and reporting them as one sent a whole afternoon looking for a
			// scraper bug that did not exist.
			thinAttempt(db, c.path, "page publishes no capacity or floor area we can read")
			continue
		}
		if r.mw == 0 && r.sqft == 0 {
			// The facility's own page gave only campus figures. Before
			// retiring it, try the operator's metro page, which on Digital
			// Realty carries per-building numbers the building pages omit.
			// Fetched once per metro per run, and only used when the block
			// identifies its building beyond doubt.
			if mu := tfMetroURL(c.source); mu != "" {
				blocks, seen := metroCache[mu]
				if !seen {
					time.Sleep(1500 * time.Millisecond)
					if mb, err := thinGet(client, mu); err == nil {
						blocks = tfParseMetro(mb)
					}
					metroCache[mu] = blocks
				}
				var lat, lon float64
				var house, street string
				db.QueryRow(`SELECT COALESCE(lat,0), COALESCE(lon,0),
						COALESCE(osm_tags->>'addr:housenumber',''),
						COALESCE(osm_tags->>'addr:street','')
					FROM twoai_dc_facilities WHERE id=$1`, c.ref).Scan(&lat, &lon, &house, &street)
				if hit := tfMetroMatch(blocks, lat, lon, house, street); hit != nil {
					r.mw, r.sqft = hit.mw, hit.sqft
					metroFilled++
				}
			}
		}
		if r.mw == 0 && r.sqft == 0 {
			// Campus figures only, and the metro page could not identify this
			// building either. It retires with a reason a reader can be shown.
			thinPermanent(db, c.path,
				"this page publishes capacity for the whole campus, not for this building")
		}
		// Most OSM rows carry no operator tag, and a facility page that says
		// "published by  on its own facility page" is worse than one that
		// names nobody. The host of the operator's own page is a fact we
		// have, so it becomes the publisher when the tag is missing: a page
		// on datafoundry.com is published by datafoundry.com, which is true
		// without claiming a corporate name the data never gave us.
		var operator string
		db.QueryRow(`SELECT COALESCE(operator,'') FROM twoai_dc_facilities WHERE id=$1`, c.ref).Scan(&operator)
		publisher := operator
		if publisher == "" {
			if m := regexp.MustCompile(`^https?://(?:www\.)?([^/]+)`).FindStringSubmatch(c.source); m != nil {
				publisher = m[1]
			}
		}
		profile := map[string]any{
			"source": map[string]any{
				"publisher": publisher, "page": c.source,
				"retrieved": time.Now().UTC().Format("2006-01-02"),
				"basis":     "Operator facility page; structured facts only, no operator prose reproduced.",
			},
		}
		if operator != "" {
			profile["operator"] = operator
		}
		if r.mw > 0 {
			profile["it_capacity_mw"] = r.mw
		}
		if r.sqft > 0 {
			profile["technical_space_sqft"] = r.sqft
		}
		// Campus figures are published under their own names so a template
		// can say "the operator's campus is 230 MW" and never imply the
		// building is.
		if r.campusMW > 0 {
			profile["campus_mw"] = r.campusMW
			profile["campus_mw_context"] = strings.TrimSpace(r.mwLine)
		}
		if r.campusSqft > 0 {
			profile["campus_sqft"] = r.campusSqft
			profile["campus_sqft_context"] = strings.TrimSpace(r.sqftLine)
		}
		if len(r.certs) > 0 {
			profile["certifications"] = r.certs
		}
		if r.postal != "" {
			profile["postal_code"] = r.postal
		}
		if len(r.address) > 0 {
			profile["address"] = r.address
		}
		pj, _ := json.Marshal(profile)
		// $3 IS A DECIMAL AND POSTGRES WAS INFERRING IT AS AN INTEGER. The
		// comparison "$3 > 0" against an untyped 0 made the driver resolve the
		// parameter to integer, so every facility that published a fractional
		// capacity - 1.2 MW, 3.75 MW, 238.7 MW - failed on insert with
		// "invalid input syntax for type integer" and was retired after three
		// runs as unfillable. It was not unfillable. Those 46 pages published
		// exactly what we asked for and we threw it away at the last step,
		// then logged the loss as a property of their websites.
		if _, err := db.Exec(`UPDATE twoai_dc_facilities
			SET profile=$2::jsonb, status='enriched',
			    critical_it_mw = CASE WHEN $3::numeric > 0 THEN $3::numeric ELSE critical_it_mw END,
			    last_seen=current_date
			WHERE id=$1`, c.ref, string(pj), r.mw); err != nil {
			thinAttempt(db, c.path, "db: "+err.Error())
			continue
		}
		db.Exec(`DELETE FROM twoai_thin_queue WHERE path=$1`, c.path)
		filled++
	}
	fmt.Printf("thinpages: facility pages read: %d enriched (%d of them from the operator's metro page), %d published nothing readable, %d were operator index URLs, of %d due\n",
		filled, metroFilled, empty, index, len(due))
}

// A figure in a sentence about a campus, a portfolio or a market belongs to
// the campus, not to the one building whose page it happens to sit on.
var tfCampusRe = regexp.MustCompile(`(?i)\b(campus|portfolio|market|metro|total capacity of the|across (our|the)|combined|region)\b`)

// Phrases that pin a figure to ONE building, which beat the campus words when
// both appear. Stephen pointed at the Dallas metro page, where all three
// scopes sit within a few sentences of each other: "the Dallas-Fort Worth
// data center market at 1,840 MW", "a substation with the ability to deliver
// up to 100 MW", and "features 69,867 square feet of raised floor space,
// 6.75 MW of IT load". Only the last is this building's number, and a campus
// rule reading nearby lines would have thrown it away with the other two.
//
// "IT load", "critical load" and "raised floor" are what an operator writes
// when describing the space a tenant actually rents. A substation rating is
// what the campus can draw. The distinction is the whole point of the field.
var tfFacilityRe = regexp.MustCompile(`(?i)\b(IT load|critical load|critical power|raised floor|white space|this facility|this building|the suite)\b`)

// tfParse reads the stat block whichever way round the operator wrote it.
func tfParse(page string) (tfResult, bool) {
	var r tfResult
	txt := thinTagRe.ReplaceAllString(page, " ")
	txt = html.UnescapeString(thinStrip.ReplaceAllString(txt, "\n"))
	var L []string
	for _, l := range strings.Split(txt, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			L = append(L, l)
		}
	}
	near := func(i int) string { // the lines on either side, for label context
		lo, hi := i-1, i+2
		if lo < 0 {
			lo = 0
		}
		if hi > len(L) {
			hi = len(L)
		}
		return strings.Join(L[lo:hi], " ")
	}
	for i, l := range L {
		// Megawatts, believed only beside a power word. "1,500 watts per
		// square foot" and "40+ data centers" are numbers on these pages too.
		if r.mw == 0 {
			if m := tfMWRe.FindStringSubmatch(l); m != nil && tfPwrLblRe.MatchString(near(i)) {
				r.mw, r.mwLine = tfNum(m[1]), near(i)
			} else if m := tfMWAnyRe.FindStringSubmatch(l); m != nil && tfPwrLblRe.MatchString(l) {
				r.mw, r.mwLine = tfNum(m[1]), l
			}
		}
		// Floor area, value and label in either order.
		if r.sqft == 0 {
			if m := tfSqInRe.FindStringSubmatch(l); m != nil {
				r.sqft, r.sqftLine = int64(tfNum(m[1])), near(i)
			} else if m := tfNumRe.FindStringSubmatch(l); m != nil {
				if i+1 < len(L) && tfSqLblRe.MatchString(L[i+1]) {
					r.sqft, r.sqftLine = int64(tfNum(m[1])), near(i)
				} else if i > 0 && tfSqLblRe.MatchString(L[i-1]) {
					r.sqft, r.sqftLine = int64(tfNum(m[1])), near(i)
				}
			}
		}
	}
	blob := strings.Join(L, " ")
	for _, c := range tfCerts {
		if regexp.MustCompile(`(?i)` + c.pat).MatchString(blob) {
			r.certs = append(r.certs, c.name)
		}
	}
	// A street line is one that carries a ZIP and is short enough to be an
	// address rather than a paragraph that happens to contain five digits.
	//
	// The ZIP is the LAST five-digit group on the line and never the first
	// token, because a street number is also five digits: Flexential's
	// Centennial page begins "12500 East Arapahoe Road" and the first
	// version of this read 12500 as the postcode. The same misread cost the
	// CyrusOne adapter a day earlier, on Houston's 11003.
	for _, l := range L {
		if len(l) > 90 || !tfStreetRe.MatchString(l) {
			continue
		}
		zips := tfZipRe.FindAllStringSubmatchIndex(l, -1)
		if len(zips) == 0 {
			continue
		}
		r.address = append(r.address, l)
		last := zips[len(zips)-1]
		if r.postal == "" && last[0] > 0 {
			r.postal = l[last[2]:last[3]]
		}
		if len(r.address) >= 3 {
			break
		}
	}
	// WHOSE NUMBER IS IT. Stephen checked digitalrealty.com and found the
	// pages perfectly readable, which they are: IAD12 says "the Ashburn
	// campus is powered by 80 MW" and "brings the total capacity of the
	// Digital Ashburn campus to 230MW", and CH1 gives 1,100,000 square feet.
	// Those are CAMPUS figures sitting in prose, and writing 230 MW onto one
	// building's page would be a fabricated fact of exactly the kind this
	// site exists not to produce.
	//
	// So a figure whose sentence talks about a campus, a portfolio or a
	// market is kept, labelled as campus scope, and NOT written to
	// critical_it_mw. The reader gets a true sentence about the campus
	// instead of a false one about the building.
	if r.mw > 0 && tfCampusRe.MatchString(r.mwLine) && !tfFacilityRe.MatchString(r.mwLine) {
		r.campusMW, r.mw = r.mw, 0
	}
	if r.sqft > 0 && tfCampusRe.MatchString(r.sqftLine) && !tfFacilityRe.MatchString(r.sqftLine) {
		r.campusSqft, r.sqft = r.sqft, 0
	}
	// Sanity: a campus under a tenth of a megawatt or over a gigawatt, or a
	// footprint under a thousand feet, is a misread, not a small building.
	if r.mw > 0 && (r.mw < 0.1 || r.mw > 1000) {
		r.mw = 0
	}
	if r.sqft > 0 && r.sqft < 1000 {
		r.sqft = 0
	}
	return r, r.mw > 0 || r.sqft > 0 || r.campusMW > 0 || r.campusSqft > 0
}

func tfNum(s string) float64 {
	v, _ := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSuffix(s, "+"), ",", ""), 64)
	return v
}
