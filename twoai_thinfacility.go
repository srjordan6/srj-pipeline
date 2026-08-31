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
	mw      float64
	sqft    int64
	certs   []string
	postal  string
	address []string
}

func twoaiThinFillFacilities(db *sql.DB) {
	due := thinDue(db, "dc-facility", thinFacilityCap)
	if len(due) == 0 {
		return
	}
	client := &http.Client{Timeout: 30 * time.Second}
	filled, empty := 0, 0
	for _, c := range due {
		// A URL with no path beyond the domain, or one path segment, is an
		// operator's index, not a facility page. Nothing to read; say so once
		// rather than fetching sixty hub pages a run.
		if !tfFacPath.MatchString(c.source) {
			thinAttempt(db, c.path, "website is an operator index, not a facility page")
			continue
		}
		time.Sleep(1500 * time.Millisecond)
		body, err := thinGet(client, c.source)
		if err != nil {
			thinAttempt(db, c.path, err.Error())
			continue
		}
		r, ok := tfParse(body)
		if !ok {
			empty++
			thinAttempt(db, c.path, "page publishes no capacity or floor area we can read")
			continue
		}
		var operator string
		db.QueryRow(`SELECT COALESCE(operator,'') FROM twoai_dc_facilities WHERE id=$1`, c.ref).Scan(&operator)
		profile := map[string]any{
			"operator": operator,
			"source": map[string]any{
				"publisher": operator, "page": c.source,
				"retrieved": time.Now().UTC().Format("2006-01-02"),
				"basis":     "Operator facility page; structured facts only, no operator prose reproduced.",
			},
		}
		if r.mw > 0 {
			profile["it_capacity_mw"] = r.mw
		}
		if r.sqft > 0 {
			profile["technical_space_sqft"] = r.sqft
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
		if _, err := db.Exec(`UPDATE twoai_dc_facilities
			SET profile=$2::jsonb, status='enriched',
			    critical_it_mw = CASE WHEN $3 > 0 THEN $3 ELSE critical_it_mw END,
			    last_seen=current_date
			WHERE id=$1`, c.ref, string(pj), r.mw); err != nil {
			thinAttempt(db, c.path, "db: "+err.Error())
			continue
		}
		db.Exec(`DELETE FROM twoai_thin_queue WHERE path=$1`, c.path)
		filled++
	}
	fmt.Printf("thinpages: facility pages read: %d enriched, %d published nothing readable, of %d due\n",
		filled, empty, len(due))
}

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
				r.mw = tfNum(m[1])
			} else if m := tfMWAnyRe.FindStringSubmatch(l); m != nil && tfPwrLblRe.MatchString(l) {
				r.mw = tfNum(m[1])
			}
		}
		// Floor area, value and label in either order.
		if r.sqft == 0 {
			if m := tfSqInRe.FindStringSubmatch(l); m != nil {
				r.sqft = int64(tfNum(m[1]))
			} else if m := tfNumRe.FindStringSubmatch(l); m != nil {
				if i+1 < len(L) && tfSqLblRe.MatchString(L[i+1]) {
					r.sqft = int64(tfNum(m[1]))
				} else if i > 0 && tfSqLblRe.MatchString(L[i-1]) {
					r.sqft = int64(tfNum(m[1]))
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
	// Sanity: a campus under a tenth of a megawatt or over a gigawatt, or a
	// footprint under a thousand feet, is a misread, not a small building.
	if r.mw > 0 && (r.mw < 0.1 || r.mw > 1000) {
		r.mw = 0
	}
	if r.sqft > 0 && r.sqft < 1000 {
		r.sqft = 0
	}
	return r, r.mw > 0 || r.sqft > 0
}

func tfNum(s string) float64 {
	v, _ := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSuffix(s, "+"), ",", ""), 64)
	return v
}
