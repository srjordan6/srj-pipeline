package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

// DATA CENTERS: the physical layer of the AI boom, tracked in two registers
// and honest about which is which. The live register is money and filings,
// because that is what primary sources publish: quarterly capital expenditure
// read from each builder's own XBRL facts on data.sec.gov (the capex fetcher
// gained Equinix, Digital Realty, Oracle, and CoreWeave for exactly this
// page), and material 8-K events from the same registrants. The reference
// register is editorial and lives in SQL: the operations metrics a facility
// is actually measured by (PUE through five nines, twoai_dc_metrics) and the
// market variables everyone asks about (queues, PPAs, rack density,
// twoai_dc_metrics track=market), each definition in this site's own words
// with the standard-setter linked. Megawatt pipelines and colocation rates
// are published only by commercial research firms, so the page says who
// publishes them (twoai_dc_sources) and does not republish numbers it cannot
// verify. All three seed tables are editable in SQL and render on the next
// run; nothing on this page is written by hand into a file.
func twoaiDatacenters(db *sql.DB, today string) (int, error) {
	var name, blurb string
	if err := db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy
		WHERE slug='data-centers' AND status='live'`).Scan(&name, &blurb); err != nil {
		return 0, nil // section not defined, nothing to render
	}

	type metric struct {
		Track      string `json:"track"`
		Category   string `json:"category"`
		Metric     string `json:"metric"`
		Definition string `json:"definition"`
		Formula    string `json:"formula,omitempty"`
		Why        string `json:"why"`
		Target     string `json:"target,omitempty"`
		SourceName string `json:"source_name,omitempty"`
		SourceURL  string `json:"source_url,omitempty"`
	}
	var metrics []metric
	mrows, err := db.Query(`SELECT track, category, metric, definition,
			COALESCE(formula,''), why_it_matters, COALESCE(target_note,''),
			COALESCE(source_name,''), COALESCE(source_url,'')
		FROM twoai_dc_metrics ORDER BY sort`)
	if err != nil {
		return 0, err
	}
	for mrows.Next() {
		var m metric
		if mrows.Scan(&m.Track, &m.Category, &m.Metric, &m.Definition,
			&m.Formula, &m.Why, &m.Target, &m.SourceName, &m.SourceURL) == nil {
			metrics = append(metrics, m)
		}
	}
	mrows.Close()

	type src struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		URL       string `json:"url"`
		Publishes string `json:"publishes"`
		Access    string `json:"access"`
	}
	var srcs []src
	srows, err := db.Query(`SELECT kind, name, url, publishes, access
		FROM twoai_dc_sources ORDER BY sort`)
	if err != nil {
		return 0, err
	}
	for srows.Next() {
		var s src
		if srows.Scan(&s.Kind, &s.Name, &s.URL, &s.Publishes, &s.Access) == nil {
			srcs = append(srcs, s)
		}
	}
	srows.Close()

	// The builders whose capex IS the data center buildout: hyperscalers plus
	// the colocation giants plus the pure-play AI cloud. Series come from the
	// twoai_capex table the SEC fetcher maintains; a company added to the
	// fetcher shows up here on its next run with no further wiring.
	dcNames := []string{"Microsoft", "Amazon", "Alphabet", "Meta", "Oracle",
		"Equinix", "Digital Realty", "CoreWeave"}
	type cpx struct {
		Name   string           `json:"name"`
		Latest float64          `json:"latest"`
		End    string           `json:"end"`
		Series []map[string]any `json:"series"`
	}
	var capex []cpx
	dcCIKs := map[string]bool{}
	for _, n := range dcNames {
		var x cpx
		x.Name = n
		crows, err := db.Query(`SELECT cik, end_date, val FROM twoai_capex
			WHERE name=$1 ORDER BY end_date DESC LIMIT 8`, n)
		if err != nil {
			continue
		}
		for crows.Next() {
			var cik, end string
			var val float64
			if crows.Scan(&cik, &end, &val) == nil {
				dcCIKs[cik] = true
				if x.End == "" {
					x.End, x.Latest = end, val
				}
				x.Series = append(x.Series, map[string]any{"end": end, "val": val})
			}
		}
		crows.Close()
		if x.End != "" {
			capex = append(capex, x)
		}
	}

	type filing struct {
		Company string `json:"company"`
		Filed   string `json:"filed"`
		Items   string `json:"items"`
		DocURL  string `json:"doc_url"`
	}
	var filings []filing
	cikList := make([]string, 0, len(dcCIKs))
	for c := range dcCIKs {
		cikList = append(cikList, c)
	}
	if len(cikList) > 0 {
		frows, err := db.Query(`SELECT company, to_char(filed,'YYYY-MM-DD'), items, doc_url
			FROM twoai_ma_filings WHERE cik = ANY($1)
			ORDER BY filed DESC LIMIT 12`, pq.Array(cikList))
		if err == nil {
			for frows.Next() {
				var f filing
				if frows.Scan(&f.Company, &f.Filed, &f.Items, &f.DocURL) == nil {
					filings = append(filings, f)
				}
			}
			frows.Close()
		}
	}

	// Absorb the retired hardware page's curated GPU-cloud operator rows
	// (AWS through Nebius); apistatus keeps verifying their links because the
	// rows never left twoai_hardware.
	type operator struct {
		Name      string `json:"name"`
		Maker     string `json:"maker"`
		Note      string `json:"note"`
		SourceURL string `json:"source_url"`
		Verified  string `json:"verified"`
	}
	var operators []operator
	orows, err := db.Query(`SELECT name, maker, note, source_url, COALESCE(verified_on::text,'')
		FROM twoai_hardware WHERE section_slug='datacenters' ORDER BY slug`)
	if err == nil {
		for orows.Next() {
			var o operator
			if orows.Scan(&o.Name, &o.Maker, &o.Note, &o.SourceURL, &o.Verified) == nil {
				operators = append(operators, o)
			}
		}
		orows.Close()
	}

	// The national facility registry: refresh from OpenStreetMap, geocode a
	// batch, then render one directory page per state.
	twoaiDcHarvest(db)
	type fac struct {
		Name     string  `json:"name"`
		Operator string  `json:"operator,omitempty"`
		City     string  `json:"city,omitempty"`
		Website  string  `json:"website,omitempty"`
		Lat      float64 `json:"lat,omitempty"`
		Lon      float64 `json:"lon,omitempty"`
		UID      string  `json:"uid,omitempty"`
		MW       float64 `json:"mw,omitempty"`
		Sqft     int64   `json:"sqft,omitempty"`
	}
	// A row whose profile jsonb carries curated, per-field-sourced spec data
	// (status=enriched, written by the thin-page scraper or by hand from an
	// operator's own spec sheet) gets its own page below and a uid here so
	// the directory line can link to it. The profile passes through raw:
	// a field added in SQL renders without a Go change.
	type enrichedFac struct {
		ID      string
		UID     string
		Name    string
		Op      string
		City    string
		State   string
		Country string
		Website string
		MW      float64
		Profile json.RawMessage
	}
	var enriched []enrichedFac
	byState := map[string][]fac{}
	byCountry := map[string][]fac{}
	mwByState := map[string]float64{}
	var facTotal, facOps int
	var facMW float64
	frows2, err := db.Query(`SELECT id, name, operator, city, COALESCE(state,''), website,
			COALESCE(lat,0), COALESCE(lon,0), COALESCE(country,'US'),
			COALESCE(critical_it_mw,0),
			COALESCE((profile->>'technical_space_sqft')::bigint,0),
			CASE WHEN status='enriched' AND profile<>'{}'::jsonb THEN profile ELSE '{}'::jsonb END
		FROM twoai_dc_facilities ORDER BY country, state, operator, name`)
	if err == nil {
		for frows2.Next() {
			var f fac
			var id, st, cc string
			var prof []byte
			if frows2.Scan(&id, &f.Name, &f.Operator, &f.City, &st, &f.Website, &f.Lat, &f.Lon, &cc,
				&f.MW, &f.Sqft, &prof) != nil {
				continue
			}
			facTotal++
			if f.Operator != "" {
				facOps++
			}
			if len(prof) > 2 { // more than the empty object
				f.UID = twoaiUID("dc-fac:" + id)
				enriched = append(enriched, enrichedFac{id, f.UID, f.Name, f.Operator, f.City, st, cc, f.Website, f.MW, json.RawMessage(prof)})
			}
			facMW += f.MW
			if cc != "US" {
				byCountry[cc] = append(byCountry[cc], f)
				continue
			}
			if st == "" {
				st = "ZZ"
			}
			mwByState[st] += f.MW
			byState[st] = append(byState[st], f)
		}
		frows2.Close()
	}
	type statePage struct {
		Code  string `json:"code"`
		UID   string `json:"uid"`
		Count int    `json:"count"`
	}
	var statePages []statePage
	keepPaths := map[string]bool{}
	for st, facs := range byState {
		if st == "ZZ" {
			continue // not yet geocoded; counted in the total, paged when located
		}
		u := twoaiUID("dc-state:" + st)
		path := "tech/dc-state-" + strings.ToLower(st) + ".json"
		keepPaths[path] = true
		sdoc := map[string]any{
			"shape": "dc-state", "uid": u, "tax": "data-centers", "generated": today,
			"state": st, "count": len(facs), "facilities": facs,
			"total_mw": mwByState[st],
			"parent":   map[string]any{"uid": twoaiUID("section:data-centers"), "name": name},
		}
		sj, _ := json.Marshal(sdoc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'tech-dc-child',$2::jsonb,'data-centers',1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			path, string(sj)); err == nil {
			statePages = append(statePages, statePage{st, u, len(facs)})
		}
	}
	sort.Slice(statePages, func(i, j int) bool { return statePages[i].Code < statePages[j].Code })

	// Beyond America: one directory per harvested country, reusing the state
	// page shape with the country name where the state code would sit.
	var intlPages []statePage
	for cc, facs := range byCountry {
		cname := cc
		for _, c := range twoaiDcCountries {
			if c.ISO == cc {
				cname = c.Name
			}
		}
		u := twoaiUID("dc-country:" + cc)
		path := "tech/dc-country-" + strings.ToLower(cc) + ".json"
		keepPaths[path] = true
		sdoc := map[string]any{
			"shape": "dc-state", "uid": u, "tax": "data-centers", "generated": today,
			"state": cname, "count": len(facs), "facilities": facs,
			"parent": map[string]any{"uid": twoaiUID("section:data-centers"), "name": name},
		}
		sj, _ := json.Marshal(sdoc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'tech-dc-child',$2::jsonb,'data-centers',1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			path, string(sj)); err == nil {
			intlPages = append(intlPages, statePage{cname, u, len(facs)})
		}
	}
	sort.Slice(intlPages, func(i, j int) bool { return intlPages[i].Code < intlPages[j].Code })

	// One page per enriched facility: the per-facility profile this file's
	// registry comment has promised since the section shipped. Keyed on
	// curated profile data, never on a bare OSM footprint, because a page for
	// a name and a coordinate pair is exactly the thin content AdSense
	// flagged once. The 2026-08-30 CyrusOne ingest (25 campuses, 980 MW) is
	// the first data through this path.
	facPages := 0
	for _, ef := range enriched {
		fpath := "tech/dc-fac-" + ef.UID + ".json"
		keepPaths[fpath] = true
		stateUID := ""
		if ef.Country == "US" && ef.State != "" {
			stateUID = twoaiUID("dc-state:" + ef.State)
		} else if ef.Country != "US" {
			stateUID = twoaiUID("dc-country:" + ef.Country)
		}
		fdoc := map[string]any{
			"shape": "dc-facility", "uid": ef.UID, "tax": "data-centers", "generated": today,
			"name": ef.Name, "operator": ef.Op, "city": ef.City, "state": ef.State,
			"country": ef.Country, "website": ef.Website, "mw": ef.MW,
			"profile": ef.Profile, "facility_id": ef.ID,
			"state_page": map[string]any{"uid": stateUID, "code": ef.State},
			"parent":     map[string]any{"uid": twoaiUID("section:data-centers"), "name": name},
		}
		fj, _ := json.Marshal(fdoc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'tech-dc-child',$2::jsonb,'data-centers',1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			fpath, string(fj)); err == nil {
			facPages++
		}
	}

	// One page per metric and per builder: the section's own reference book,
	// every page dated and regenerated daily from the same rows as the hub.
	type childRef struct {
		Label    string `json:"label"`
		Category string `json:"category,omitempty"`
		UID      string `json:"uid"`
	}
	var metricPages []childRef
	// Every metric's uid up front, so each page can list the other metrics in
	// its category as links: a reader who lands on PUE from a search should
	// find WUE and the rest without going back to the hub.
	metricUID := map[string]string{}
	for _, m := range metrics {
		metricUID[m.Metric] = twoaiUID("dc-metric:" + strings.ToLower(strings.ReplaceAll(m.Metric, " ", "-")))
	}
	for _, m := range metrics {
		u := metricUID[m.Metric]
		path := "tech/dc-metric-" + u + ".json"
		keepPaths[path] = true
		var siblings []childRef
		for _, o := range metrics {
			if o.Category == m.Category && o.Metric != m.Metric {
				siblings = append(siblings, childRef{o.Metric, o.Category, metricUID[o.Metric]})
			}
		}
		mdoc := map[string]any{
			"shape": "dc-metric", "uid": u, "tax": "data-centers", "generated": today,
			"metric": m, "siblings": siblings,
			"parent": map[string]any{"uid": twoaiUID("section:data-centers"), "name": name},
		}
		mj, _ := json.Marshal(mdoc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'tech-dc-child',$2::jsonb,'data-centers',1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			path, string(mj)); err == nil {
			metricPages = append(metricPages, childRef{m.Metric, m.Category, u})
		}
	}
	var builderPages []childRef
	for _, c := range capex {
		u := twoaiUID("dc-builder:" + strings.ToLower(c.Name))
		path := "tech/dc-builder-" + u + ".json"
		keepPaths[path] = true
		var bFilings []filing
		for _, f := range filings {
			if f.Company == c.Name {
				bFilings = append(bFilings, f)
			}
		}
		// The other builders' latest quarters, so the page can rank this one
		// among its peers from the same filings rather than restating a number
		// in isolation.
		type peer struct {
			Name   string  `json:"name"`
			Latest float64 `json:"latest"`
			End    string  `json:"end"`
			UID    string  `json:"uid"`
		}
		var peers []peer
		for _, o := range capex {
			peers = append(peers, peer{o.Name, o.Latest, o.End, twoaiUID("dc-builder:" + strings.ToLower(o.Name))})
		}
		sort.Slice(peers, func(i, j int) bool { return peers[i].Latest > peers[j].Latest })
		bdoc := map[string]any{
			"shape": "dc-builder", "uid": u, "tax": "data-centers", "generated": today,
			"builder": c, "filings": bFilings, "peers": peers,
			"parent": map[string]any{"uid": twoaiUID("section:data-centers"), "name": name},
		}
		bj, _ := json.Marshal(bdoc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'tech-dc-child',$2::jsonb,'data-centers',1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			path, string(bj)); err == nil {
			builderPages = append(builderPages, childRef{c.Name, "", u})
		}
	}
	// Nuclear for data centers: the curated SMR project tracker. Every row
	// carries its own verified primary source and an executed/pending/stated
	// deal status, because a nonbinding letter of intent must never render
	// like a signed power purchase agreement.
	type smrProject struct {
		Project    string  `json:"project"`
		Vendor     string  `json:"vendor"`
		Model      string  `json:"model"`
		Class      string  `json:"class"`
		TotalMW    float64 `json:"total_mw,omitempty"`
		Customer   string  `json:"customer,omitempty"`
		Site       string  `json:"site,omitempty"`
		State      string  `json:"state,omitempty"`
		DealStatus string  `json:"deal_status"`
		NRCStatus  string  `json:"nrc_status,omitempty"`
		Fuel       string  `json:"fuel,omitempty"`
		TargetCOD  string  `json:"target_cod,omitempty"`
		SourceURL  string  `json:"source_url"`
		Note       string  `json:"note,omitempty"`
		Verified   string  `json:"verified"`
	}
	var smrProjects []smrProject
	xrows, err := db.Query(`SELECT project, vendor, reactor_model, reactor_class,
			COALESCE(total_mw,0), customer, site, state, deal_status, nrc_status,
			fuel, target_cod, source_url, note, verified_on::text
		FROM twoai_dc_smr_projects ORDER BY sort`)
	if err == nil {
		for xrows.Next() {
			var x smrProject
			if xrows.Scan(&x.Project, &x.Vendor, &x.Model, &x.Class, &x.TotalMW,
				&x.Customer, &x.Site, &x.State, &x.DealStatus, &x.NRCStatus,
				&x.Fuel, &x.TargetCOD, &x.SourceURL, &x.Note, &x.Verified) == nil {
				smrProjects = append(smrProjects, x)
			}
		}
		xrows.Close()
	}
	smrUID := twoaiUID("dc-smr")
	if len(smrProjects) > 0 {
		path := "tech/dc-smr.json"
		keepPaths[path] = true
		xdoc := map[string]any{
			"shape": "dc-smr", "uid": smrUID, "tax": "data-centers", "generated": today,
			"projects": smrProjects,
			"parent":   map[string]any{"uid": twoaiUID("section:data-centers"), "name": name},
		}
		xj, _ := json.Marshal(xdoc)
		db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'tech-dc-child',$2::jsonb,'data-centers',1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			path, string(xj))
	}

	// Kind-scoped prune: a child page whose row was deleted disappears.
	if len(keepPaths) > 0 {
		keep := make([]string, 0, len(keepPaths))
		for p := range keepPaths {
			keep = append(keep, p)
		}
		db.Exec(`DELETE FROM twoai_pages WHERE kind='tech-dc-child' AND NOT (path = ANY($1))`, pq.Array(keep))
	}

	doc := map[string]any{
		"shape": "datacenters", "uid": twoaiUID("section:data-centers"),
		"tax": "data-centers", "generated": today, "name": name, "blurb": blurb,
		"metrics": metrics, "sources": srcs, "capex": capex, "filings": filings,
		"operators":    operators,
		"metric_pages": metricPages, "builder_pages": builderPages,
		"state_pages": statePages, "intl_pages": intlPages, "fac_total": facTotal, "fac_ops": facOps,
		"fac_mw": facMW, "fac_profiled": len(enriched),
		"smr":  map[string]any{"uid": smrUID, "count": len(smrProjects)},
		"grid":    map[string]any{"uid": twoaiUID("dc-grid")},
		"indexes": twoaiDcIndexes(db),
		"index_uids": map[string]any{
			"dc-grid": twoaiUID("dc-grid"), "dc-smr": twoaiUID("dc-smr"),
		},
	}
	j, _ := json.Marshal(doc)
	if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
		VALUES ('tech/datacenters.json','tech-datacenters',$1::jsonb,'data-centers',1)
		ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
			taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
		string(j)); err != nil {
		return 0, err
	}
	fmt.Printf("twoai_build: datacenters metrics=%d sources=%d capex_cos=%d filings=%d operators=%d facilities=%d profiled=%d states=%d children=%d\n",
		len(metrics), len(srcs), len(capex), len(filings), len(operators), facTotal, facPages, len(statePages)+len(intlPages), len(keepPaths))
	return 1, nil
}

// THE FACILITY REGISTRY. Stephen asked for every data center in America with
// a full facility profile. The honest scope of "every": no free, licensed
// dataset carries per-facility megawatts, PUE, or tier ratings; those live in
// commercial databases and on operator spec sheets. What exists licensed is
// OpenStreetMap, whose contributors have mapped ~1,700 US facilities
// (telecom=data_center and building=data_center), about 1,100 with named
// operators, ODbL, attributed on every page it touches; plus a dozen
// Wikidata items (CC0). This registry ingests both, geocodes to state
// through the FCC's public census API a batch at a time, and renders a
// per-state directory. Per-facility profile pages exist only once a
// facility's profile jsonb carries curated, per-field-sourced spec data
// (Stephen's five-dimension template); a page for a bare name and a pair of
// coordinates is exactly the thin content AdSense already flagged once.
// The registry is excluded from the training-corpus export: ODbL is
// share-alike at the database level and the corpus is not.
// Beyond America: one additional country per run, rotating by day of year,
// so every pipeline run costs OpenStreetMap exactly two queries. China is on
// the wheel with eyes open: facility mapping there is sparse and state
// disclosure minimal, so its directory will be thin and says so.
var twoaiDcCountries = []struct{ ISO, Name string }{
	{"CN", "China"}, {"DE", "Germany"}, {"GB", "United Kingdom"},
	{"FR", "France"}, {"NL", "Netherlands"}, {"IE", "Ireland"},
	{"SE", "Sweden"}, {"NO", "Norway"}, {"ES", "Spain"}, {"IT", "Italy"},
	{"PL", "Poland"}, {"FI", "Finland"}, {"DK", "Denmark"},
}

func twoaiDcHarvest(db *sql.DB) {
	twoaiDcHarvestCountry(db, "US")
	c := twoaiDcCountries[time.Now().YearDay()%len(twoaiDcCountries)]
	twoaiDcHarvestCountry(db, c.ISO)

	twoaiDcGeocodeUS(db)
}

func twoaiDcHarvestCountry(db *sql.DB, iso string) {
	client := &http.Client{Timeout: 200 * time.Second}
	q := `[out:json][timeout:180];
area["ISO3166-1"="` + iso + `"][admin_level=2]->.a;
( nwr["telecom"="data_center"](area.a);
  nwr["building"="data_center"](area.a); );
out tags center;`
	req, _ := http.NewRequest("POST", "https://overpass-api.de/api/interpreter",
		strings.NewReader(url.Values{"data": {q}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "theworldofai.org facility registry (contact: stephen@srjconsultingservices.com)")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("twoai_build: dc harvest", iso, "overpass:", err, "(keeping prior registry)")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Println("twoai_build: dc harvest", iso, "overpass status", resp.StatusCode, "(keeping prior registry)")
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	var out struct {
		Elements []struct {
			Type   string                      `json:"type"`
			ID     int64                       `json:"id"`
			Lat    float64                     `json:"lat"`
			Lon    float64                     `json:"lon"`
			Center *struct{ Lat, Lon float64 } `json:"center"`
			Tags   map[string]string           `json:"tags"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Elements) == 0 {
		fmt.Println("twoai_build: dc harvest", iso, "parse failed or empty, keeping prior registry")
		return
	}
	n := 0
	for _, e := range out.Elements {
		name := strings.TrimSpace(e.Tags["name"])
		if name == "" {
			continue // an unnamed footprint is a shape, not a directory entry
		}
		lat, lon := e.Lat, e.Lon
		if e.Center != nil {
			lat, lon = e.Center.Lat, e.Center.Lon
		}
		id := fmt.Sprintf("osm:%s/%d", e.Type, e.ID)
		site := e.Tags["website"]
		if site == "" {
			site = e.Tags["contact:website"]
		}
		tj, _ := json.Marshal(e.Tags)
		// Source precedence is deterministic: curated profile data (operator
		// spec sheets, filings, permits) outranks OSM. Once profile jsonb is
		// non-empty the daily OSM refresh may update only osm_tags and
		// last_seen; identity fields are frozen against the crowd map.
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
			id, name, strings.TrimSpace(e.Tags["operator"]),
			strings.TrimSpace(e.Tags["addr:city"]), strings.TrimSpace(e.Tags["addr:state"]),
			lat, lon, strings.TrimSpace(site), string(tj), iso); err == nil {
			n++
		}
	}
	fmt.Printf("twoai_build: dc harvest %s osm elements=%d upserted=%d\n", iso, len(out.Elements), n)
}

// Geocode a US batch to state through the FCC census block API, public and
// keyless. Two hundred a run clears the backlog in nine runs, then only
// newly mapped facilities cost anything. International rows group by
// country and skip this entirely.
func twoaiDcGeocodeUS(db *sql.DB) {
	rows, err := db.Query(`SELECT id, lat, lon FROM twoai_dc_facilities
		WHERE country='US' AND state='' AND lat IS NOT NULL ORDER BY id LIMIT 200`)
	if err != nil {
		return
	}
	type todo struct {
		id       string
		lat, lon float64
	}
	var todos []todo
	for rows.Next() {
		var t todo
		if rows.Scan(&t.id, &t.lat, &t.lon) == nil {
			todos = append(todos, t)
		}
	}
	rows.Close()
	geo := 0
	gclient := &http.Client{Timeout: 15 * time.Second}
	for _, t := range todos {
		u := fmt.Sprintf("https://geo.fcc.gov/api/census/area?lat=%f&lon=%f&format=json", t.lat, t.lon)
		greq, _ := http.NewRequest("GET", u, nil)
		greq.Header.Set("User-Agent", "theworldofai.org facility registry (contact: stephen@srjconsultingservices.com)")
		gresp, err := gclient.Do(greq)
		if err != nil {
			break
		}
		var g struct {
			Results []struct {
				StateCode  string `json:"state_code"`
				CountyName string `json:"county_name"`
			} `json:"results"`
		}
		body, _ := io.ReadAll(io.LimitReader(gresp.Body, 1<<20))
		gresp.Body.Close()
		if gresp.StatusCode != 200 || json.Unmarshal(body, &g) != nil || len(g.Results) == 0 {
			continue
		}
		if _, err := db.Exec(`UPDATE twoai_dc_facilities SET state=$1,
			city=CASE WHEN city='' THEN $2 ELSE city END WHERE id=$3`,
			g.Results[0].StateCode, g.Results[0].CountyName, t.id); err == nil {
			geo++
		}
		time.Sleep(250 * time.Millisecond)
	}
	if len(todos) > 0 {
		fmt.Printf("twoai_build: dc geocode attempted=%d resolved=%d\n", len(todos), geo)
	}
}
