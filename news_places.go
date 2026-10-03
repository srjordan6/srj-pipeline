package main

// news_places: the places named in a headline, so two stories that each name
// a different place are never taken for one event by headline words alone.
//
// The 23:38 run of 2026-10-02 merged "Kyndryl launches AI Innovation Lab in
// Singapore" into "BIL welcomes the launch of Kyndryl's AI Innovation Lab in
// Luxembourg": four of six headline words shared, and the two words that
// differ are the two places. A company opening the same kind of lab in two
// cities is two events. The list is countries, US states and large cities;
// a place missing from it simply does not trigger the check.

import (
	"regexp"
	"strings"
)

var newsPlaceList = []string{
	// Countries and common short forms.
	"afghanistan", "albania", "algeria", "argentina", "armenia", "australia", "austria", "azerbaijan", "bahrain", "bangladesh",
	"belarus", "belgium", "bolivia", "bosnia", "brazil", "bulgaria", "cambodia", "cameroon", "canada", "chile", "china", "colombia",
	"costa rica", "croatia", "cuba", "cyprus", "czech republic", "czechia", "denmark", "ecuador", "egypt", "estonia", "ethiopia",
	"finland", "france", "georgia", "germany", "ghana", "greece", "guatemala", "hong kong", "hungary", "iceland", "india",
	"indonesia", "iran", "iraq", "ireland", "israel", "italy", "jamaica", "japan", "jordan", "kazakhstan", "kenya", "kuwait",
	"latvia", "lebanon", "lithuania", "luxembourg", "malaysia", "malta", "mexico", "moldova", "mongolia", "morocco", "nepal",
	"netherlands", "new zealand", "nigeria", "north korea", "norway", "oman", "pakistan", "panama", "paraguay", "peru",
	"philippines", "poland", "portugal", "qatar", "romania", "russia", "rwanda", "saudi arabia", "senegal", "serbia",
	"singapore", "slovakia", "slovenia", "south africa", "south korea", "korea", "spain", "sri lanka", "sweden", "switzerland",
	"taiwan", "tanzania", "thailand", "tunisia", "turkey", "turkiye", "uganda", "ukraine", "united arab emirates", "uae",
	"united kingdom", "uk", "britain", "england", "scotland", "wales", "united states", "uruguay", "uzbekistan", "venezuela",
	"vietnam", "zambia", "zimbabwe", "europe", "africa", "asia", "latin america", "middle east",
	// US states.
	"alabama", "alaska", "arizona", "arkansas", "california", "colorado", "connecticut", "delaware", "florida", "hawaii",
	"idaho", "illinois", "indiana", "iowa", "kansas", "kentucky", "louisiana", "maine", "maryland", "massachusetts",
	"michigan", "minnesota", "mississippi", "missouri", "montana", "nebraska", "nevada", "new hampshire", "new jersey",
	"new mexico", "new york", "north carolina", "north dakota", "ohio", "oklahoma", "oregon", "pennsylvania", "rhode island",
	"south carolina", "south dakota", "tennessee", "texas", "utah", "vermont", "virginia", "washington", "west virginia",
	"wisconsin", "wyoming",
	// Large cities and metro areas.
	"atlanta", "austin", "boston", "chicago", "dallas", "denver", "detroit", "houston", "las vegas", "los angeles", "miami",
	"minneapolis", "nashville", "new york city", "philadelphia", "phoenix", "pittsburgh", "portland", "raleigh", "san antonio",
	"san diego", "san francisco", "san jose", "seattle", "silicon valley", "amsterdam", "athens", "bangalore", "bengaluru",
	"bangkok", "barcelona", "beijing", "berlin", "brussels", "buenos aires", "cairo", "chennai", "copenhagen", "delhi",
	"new delhi", "doha", "dubai", "dublin", "frankfurt", "geneva", "hamburg", "helsinki", "hyderabad", "istanbul", "jakarta",
	"johannesburg", "kuala lumpur", "lagos", "lisbon", "london", "madrid", "manila", "melbourne", "milan", "montreal",
	"moscow", "mumbai", "munich", "nairobi", "osaka", "oslo", "paris", "prague", "riyadh", "rome", "sao paulo", "seoul",
	"shanghai", "shenzhen", "stockholm", "sydney", "taipei", "tel aviv", "tokyo", "toronto", "vancouver", "vienna",
	"warsaw", "zurich",
}

var newsPlaceRe = func() *regexp.Regexp {
	parts := make([]string, 0, len(newsPlaceList))
	for _, p := range newsPlaceList {
		parts = append(parts, regexp.QuoteMeta(p))
	}
	// Longest first, so "new york city" wins over "new york".
	for i := 0; i < len(parts); i++ {
		for j := i + 1; j < len(parts); j++ {
			if len(parts[j]) > len(parts[i]) {
				parts[i], parts[j] = parts[j], parts[i]
			}
		}
	}
	return regexp.MustCompile(`\b(` + strings.Join(parts, "|") + `)\b`)
}()

// newsPlaces returns the places a headline names, lower-cased.
func newsPlaces(headline string) map[string]bool {
	out := map[string]bool{}
	for _, m := range newsPlaceRe.FindAllString(strings.ToLower(headline), -1) {
		out[m] = true
	}
	return out
}

// newsDifferentPlaces reports two headlines that each name a place and share
// none: two events, whatever else their words share.
func newsDifferentPlaces(a, b string) bool {
	pa, pb := newsPlaces(a), newsPlaces(b)
	if len(pa) == 0 || len(pb) == 0 {
		return false
	}
	for p := range pa {
		for q := range pb {
			// "new york city" and "new york" are one place.
			if p == q || strings.Contains(p, q) || strings.Contains(q, p) {
				return false
			}
		}
	}
	return true
}
