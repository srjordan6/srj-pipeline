package main

// THE FACILITY REGISTRY WITHOUT A PUBLIC SERVER IN THE LOOP.
//
// twoaiDcHarvestCountry asks Overpass, a donated shared query service, one
// question per run. On 2026-09-02 every Overpass mirror answered 503 or 504
// for hours, the pipeline correctly kept its prior registry, and China - first
// on the country wheel - went unharvested again. That is the failure shape of
// depending on someone else's free endpoint for the foundation of a section.
//
// Geofabrik publishes daily extracts of the whole map as .osm.pbf files, per
// country and per US state, free, under the same ODbL the registry already
// attributes. This stage downloads the extract for the country whose turn it
// is, streams it through a tag filter for telecom=data_center and
// building=data_center, and upserts through exactly the same statement the
// Overpass path uses, with the same osm:<type>/<id> identities, so nothing
// downstream can tell which path a row came from. Overpass remains the
// fallback only if the download itself fails.
//
// Cadence: the rotation already gives each country one day in thirteen, and
// the map of data centres changes slowly, so no further gate is needed. The
// extracts are large - Texas about 400 MB, China about 900 MB - but the
// stream is filtered as it arrives and never written to disk, so the cron's
// ephemeral filesystem is untouched.
//
// Ways carry no coordinates of their own in a PBF; their centre has to be
// computed from member nodes. Data-centre-tagged ways are rare and their
// nodes are not, so pass 1 collects the matching ways and the node ids they
// need, and pass 2 over the same download picks up only those nodes. Two
// passes over one stream is cheaper than any alternative that keeps the whole
// node table in memory. Relations are left to the Overpass path on its day.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/paulmach/osm"
	"github.com/paulmach/osm/osmpbf"
)

// Geofabrik paths for the countries on the wheel. The US stays on Overpass:
// its single extract is about 10 GB, and the US harvest has been answering.
var twoaiGeofabrik = map[string]string{
	"CN": "asia/china-latest.osm.pbf",
	"DE": "europe/germany-latest.osm.pbf",
	"GB": "europe/great-britain-latest.osm.pbf",
	"FR": "europe/france-latest.osm.pbf",
	"NL": "europe/netherlands-latest.osm.pbf",
	"IE": "europe/ireland-and-northern-ireland-latest.osm.pbf",
	"SE": "europe/sweden-latest.osm.pbf",
	"NO": "europe/norway-latest.osm.pbf",
	"ES": "europe/spain-latest.osm.pbf",
	"IT": "europe/italy-latest.osm.pbf",
	"PL": "europe/poland-latest.osm.pbf",
	"FI": "europe/finland-latest.osm.pbf",
	"DK": "europe/denmark-latest.osm.pbf",
}

func twoaiDcIsDataCenter(tags osm.Tags) bool {
	m := tags.Map()
	return m["telecom"] == "data_center" || m["building"] == "data_center" || m["man_made"] == "data_center"
}

// twoaiDcHarvestExtract streams one country's Geofabrik extract and upserts
// every named data-centre element. Returns elements seen and rows upserted,
// or an error if the download could not be opened, in which case the caller
// falls back to Overpass.
func twoaiDcHarvestExtract(db *sql.DB, iso string) (int, int, error) {
	path, ok := twoaiGeofabrik[iso]
	if !ok {
		return 0, 0, fmt.Errorf("no Geofabrik path for %s", iso)
	}
	url := "https://download.geofabrik.de/" + path
	open := func() (io.ReadCloser, error) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("User-Agent", "theworldofai.org facility registry (contact: stephen@srjconsultingservices.com)")
		resp, err := (&http.Client{Timeout: 45 * time.Minute}).Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return resp.Body, nil
	}

	// PASS 1: every matching node and way, with the node ids the ways need.
	type hit struct {
		typ   string
		id    int64
		tags  osm.Tags
		lat   float64
		lon   float64
		nodes []osm.NodeID
	}
	var hits []hit
	need := map[osm.NodeID]bool{}

	body, err := open()
	if err != nil {
		return 0, 0, err
	}
	sc := osmpbf.New(context.Background(), body, 3)
	for sc.Scan() {
		switch o := sc.Object().(type) {
		case *osm.Node:
			if twoaiDcIsDataCenter(o.Tags) {
				hits = append(hits, hit{"node", int64(o.ID), o.Tags, o.Lat, o.Lon, nil})
			}
		case *osm.Way:
			if twoaiDcIsDataCenter(o.Tags) {
				ids := make([]osm.NodeID, 0, len(o.Nodes))
				for _, n := range o.Nodes {
					ids = append(ids, n.ID)
					need[n.ID] = true
				}
				hits = append(hits, hit{"way", int64(o.ID), o.Tags, 0, 0, ids})
			}
		}
	}
	scanErr := sc.Err()
	sc.Close()
	body.Close()
	if scanErr != nil {
		return 0, 0, scanErr
	}
	if len(hits) == 0 {
		return 0, 0, nil
	}

	// PASS 2: coordinates for the nodes that matching ways reference.
	coords := map[osm.NodeID][2]float64{}
	if len(need) > 0 {
		body, err = open()
		if err != nil {
			return len(hits), 0, err
		}
		sc = osmpbf.New(context.Background(), body, 3)
		for sc.Scan() {
			if n, ok := sc.Object().(*osm.Node); ok && need[n.ID] {
				coords[n.ID] = [2]float64{n.Lat, n.Lon}
			}
		}
		sc.Close()
		body.Close()
	}

	n := 0
	for _, h := range hits {
		m := h.tags.Map()
		name := strings.TrimSpace(m["name"])
		if name == "" {
			continue // an unnamed footprint is a shape, not a directory entry
		}
		lat, lon := h.lat, h.lon
		if h.typ == "way" {
			var sx, sy float64
			var k int
			for _, id := range h.nodes {
				if c, ok := coords[id]; ok {
					sx += c[0]
					sy += c[1]
					k++
				}
			}
			if k == 0 {
				continue
			}
			lat, lon = sx/float64(k), sy/float64(k)
		}
		id := fmt.Sprintf("osm:%s/%d", h.typ, h.id)
		site := m["website"]
		if site == "" {
			site = m["contact:website"]
		}
		tj, _ := json.Marshal(m)
		if _, err := db.Exec(`INSERT INTO twoai_dc_facilities
			(id, src, name, operator, city, state, lat, lon, website, osm_tags, country)
			VALUES ($1,'osm',$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10)
			ON CONFLICT (id) DO UPDATE SET
				name=CASE WHEN twoai_dc_facilities.profile='{}'::jsonb THEN EXCLUDED.name ELSE twoai_dc_facilities.name END,
				operator=CASE WHEN twoai_dc_facilities.profile='{}'::jsonb THEN EXCLUDED.operator ELSE twoai_dc_facilities.operator END,
				city=CASE WHEN twoai_dc_facilities.profile='{}'::jsonb AND EXCLUDED.city<>'' THEN EXCLUDED.city ELSE twoai_dc_facilities.city END,
				state=CASE WHEN twoai_dc_facilities.profile='{}'::jsonb AND EXCLUDED.state<>'' THEN EXCLUDED.state ELSE twoai_dc_facilities.state END,
				lat=CASE WHEN twoai_dc_facilities.profile='{}'::jsonb THEN EXCLUDED.lat ELSE twoai_dc_facilities.lat END,
				lon=CASE WHEN twoai_dc_facilities.profile='{}'::jsonb THEN EXCLUDED.lon ELSE twoai_dc_facilities.lon END,
				website=CASE WHEN twoai_dc_facilities.profile='{}'::jsonb AND EXCLUDED.website<>'' THEN EXCLUDED.website ELSE twoai_dc_facilities.website END,
				osm_tags=EXCLUDED.osm_tags, last_seen=current_date`,
			id, name, strings.TrimSpace(m["operator"]),
			strings.TrimSpace(m["addr:city"]), strings.TrimSpace(m["addr:state"]),
			lat, lon, strings.TrimSpace(site), string(tj), iso); err == nil {
			n++
		}
	}
	return len(hits), n, nil
}
