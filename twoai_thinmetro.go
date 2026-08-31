package main

import (
	"math"
	"regexp"
	"strings"
)

// Reading an operator's METRO page to fill facilities its own facility pages
// leave empty.
//
// Stephen sent https://www.digitalrealty.com/data-centers/americas/dallas
// after I had retired 70 rows as "operator index, not a facility page" and 60
// more as publishing nothing readable. Both messages were too confident. That
// index carries per-building numbers for buildings whose own pages carry only
// campus prose: the page for DFW16 talks about a 100 MW substation and a
// 69-acre campus, while the metro page says the building at 1232 Alma Road has
// 60,687 square feet of raised floor and 6.75 MW of IT load.
//
// THE HARD PART IS NOT READING IT, IT IS ATTRIBUTION. A metro page describes
// several buildings in a row, so a figure lifted from the wrong block writes
// DFW16's capacity onto DFW18, which is worse than an empty field: a wrong
// number is quoted, and nobody can tell it is wrong by looking at it. So a
// block is only used when it identifies its building by something that cannot
// coincide - the coordinates the page publishes, or the street address - and
// only when exactly one facility matches. Two candidates means no answer.
type tfMetroBlock struct {
	addr     string
	lat, lon float64
	mw       float64
	sqft     int64
}

var (
	tfMetroSplit = regexp.MustCompile(`"field_intro_location":`)
	tfMetroMW    = regexp.MustCompile(`(?i)([0-9][0-9.,]*)\s*MW of IT load`)
	tfMetroSq    = regexp.MustCompile(`(?i)([0-9][0-9,]{3,})\s*square feet of raised floor`)
	tfMetroAddr  = regexp.MustCompile(`\d{2,6}\s+[A-Z][A-Za-z.' ]{2,28}(?:Lane|Road|Drive|Boulevard|Street|Way|Row|Avenue|Parkway|Blvd|Rd|Dr|St)\b`)
	tfMetroLat   = regexp.MustCompile(`"field_latitude":\[\{"value":"(-?[0-9.]+)"`)
	tfMetroLon   = regexp.MustCompile(`"field_longitude":\[\{"value":"(-?[0-9.]+)"`)
	tfMetroTag   = regexp.MustCompile(`<[^>]+>`)
)

// tfMetroURL turns a facility URL into its metro index, or empty when the URL
// has no parent worth reading. Only one level up: the continent page describes
// no buildings at all.
func tfMetroURL(facility string) string {
	u := strings.TrimSuffix(facility, "/")
	i := strings.LastIndex(u, "/")
	if i < 0 {
		return ""
	}
	parent := u[:i]
	// Guard against climbing to the section root, which lists metros not
	// buildings and would match every facility in the country.
	if strings.Count(strings.TrimPrefix(parent, "https://"), "/") < 3 {
		return ""
	}
	return parent
}

// tfParseMetro splits a metro page into per-building blocks and keeps only
// those carrying a capacity AND an identifier to attach it to.
func tfParseMetro(page string) []tfMetroBlock {
	page = strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\r\n`, " ").Replace(page)
	var out []tfMetroBlock
	for _, raw := range tfMetroSplit.Split(page, -1)[1:] {
		if len(raw) > 4000 {
			raw = raw[:4000]
		}
		txt := strings.Join(strings.Fields(tfMetroTag.ReplaceAllString(raw, " ")), " ")
		var b tfMetroBlock
		if m := tfMetroMW.FindStringSubmatch(txt); m != nil {
			b.mw = tfNum(m[1])
		}
		if m := tfMetroSq.FindStringSubmatch(txt); m != nil {
			b.sqft = int64(tfNum(m[1]))
		}
		if b.mw == 0 && b.sqft == 0 {
			continue
		}
		if a := tfMetroAddr.FindString(txt); a != "" {
			b.addr = strings.ToLower(strings.Join(strings.Fields(a), " "))
		}
		if m := tfMetroLat.FindStringSubmatch(raw); m != nil {
			b.lat = tfNum(m[1])
		}
		if m := tfMetroLon.FindStringSubmatch(raw); m != nil {
			b.lon = tfNum(m[1])
		}
		if b.addr == "" && (b.lat == 0 || b.lon == 0) {
			continue // a number with nothing to attach it to is not usable
		}
		out = append(out, b)
	}
	return out
}

// tfMetroMatch returns the one block describing this facility, or nil.
//
// 300 metres, because a data centre is a large building and OSM marks the
// footprint centroid while the operator publishes a front-door coordinate. Two
// candidates inside that radius means a campus of neighbours, and the honest
// answer there is none.
func tfMetroMatch(blocks []tfMetroBlock, lat, lon float64, house, street string) *tfMetroBlock {
	// ADDRESS FIRST, AND THE DALLAS PAGE IS WHY. Digital Realty's Richardson
	// campus puts 1232 Alma Road, 900 Quality Way and 907 Security Row within
	// 300 metres of each other, so proximity alone found three candidates for
	// DFW16 and refused - correctly, but uselessly, when the page had already
	// named the street. A street address identifies a building; a radius only
	// narrows the field. So an address match is taken on its own, and
	// coordinates are the fallback for blocks that carry no address.
	want := strings.ToLower(strings.TrimSpace(house + " " + street))
	if want != "" {
		var hit *tfMetroBlock
		n := 0
		for i := range blocks {
			if b := &blocks[i]; b.addr != "" && strings.HasPrefix(b.addr, want) {
				n++
				hit = b
			}
		}
		if n == 1 {
			return hit
		}
		if n > 1 {
			return nil // the same address twice is a page we do not understand
		}
	}
	if lat == 0 || lon == 0 {
		return nil
	}
	var hit *tfMetroBlock
	n := 0
	for i := range blocks {
		if b := &blocks[i]; b.lat != 0 && tfMetres(lat, lon, b.lat, b.lon) <= 300 {
			n++
			hit = b
		}
	}
	if n != 1 {
		return nil
	}
	return hit
}

func tfMetres(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := (lat2-lat1)*math.Pi/180, (lon2-lon1)*math.Pi/180
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return r * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
