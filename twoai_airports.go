package main

import (
	"encoding/csv"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// NEAREST AIRPORT, FROM A PUBLIC-DOMAIN DATASET, COMPUTED NOT COPIED.
//
// Stephen, 2026-09-03: every data-centre page gets a fact box - address,
// operator, space, power, density, distance to the nearest airport. The
// commercial directories show that last figure; this registry computes it.
// OurAirports publishes every airport in the world with coordinates, in the
// public domain, refreshed daily. This loads the large and medium airports
// (about 5,300 rows) once per run and answers the nearest one by great-circle
// distance for any coordinate. No API, no key, no licence to attribute beyond
// courtesy, and the number is arithmetic on two public coordinates rather
// than a claim read off someone else's listing.

type twoaiAirport struct {
	Name, IATA, City string
	Lat, Lon         float64
}

func twoaiLoadAirports() []twoaiAirport {
	req, _ := http.NewRequest("GET", "https://davidmegginson.github.io/ourairports-data/airports.csv", nil)
	req.Header.Set("User-Agent", "theworldofai.org facility registry")
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil || resp.StatusCode != 200 {
		return nil
	}
	defer resp.Body.Close()
	r := csv.NewReader(resp.Body)
	r.FieldsPerRecord = -1
	head, err := r.Read()
	if err != nil {
		return nil
	}
	col := map[string]int{}
	for i, h := range head {
		col[h] = i
	}
	get := func(rec []string, k string) string {
		if i, ok := col[k]; ok && i < len(rec) {
			return rec[i]
		}
		return ""
	}
	var out []twoaiAirport
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		t := get(rec, "type")
		if t != "large_airport" && t != "medium_airport" {
			continue
		}
		lat, e1 := strconv.ParseFloat(get(rec, "latitude_deg"), 64)
		lon, e2 := strconv.ParseFloat(get(rec, "longitude_deg"), 64)
		if e1 != nil || e2 != nil {
			continue
		}
		out = append(out, twoaiAirport{
			Name: strings.TrimSpace(get(rec, "name")), IATA: strings.TrimSpace(get(rec, "iata_code")),
			City: strings.TrimSpace(get(rec, "municipality")), Lat: lat, Lon: lon,
		})
	}
	return out
}

// twoaiNearestAirport returns the closest large or medium airport and the
// great-circle distance in statute miles, or nil when the list is empty or
// the coordinate is unset.
func twoaiNearestAirport(airports []twoaiAirport, lat, lon float64) map[string]any {
	if len(airports) == 0 || (lat == 0 && lon == 0) {
		return nil
	}
	const R = 3958.7613 // Earth radius, miles
	rad := math.Pi / 180
	best, bestD := -1, math.Inf(1)
	for i, a := range airports {
		dLat := (a.Lat - lat) * rad
		dLon := (a.Lon - lon) * rad
		h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat*rad)*math.Cos(a.Lat*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
		d := 2 * R * math.Asin(math.Sqrt(h))
		if d < bestD {
			best, bestD = i, d
		}
	}
	if best < 0 {
		return nil
	}
	a := airports[best]
	return map[string]any{
		"name": a.Name, "iata": a.IATA, "city": a.City,
		"miles": math.Round(bestD*10) / 10,
	}
}
