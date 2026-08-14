package main

// twoai_observatory.go - AI Observatory: live telemetry on the field's
// health, computed entirely from data this pipeline already collects daily.
// Nothing here has its own collector; every number is a view over the model
// catalogue, repo catalogue, status snapshots, Form D ledger, and patent
// counts, so the observatory can never disagree with the sections it
// summarizes. A snapshots table starts accumulating history at first run;
// pages state when their series began rather than pretending depth.
//
// Cloud GPU Availability (obs-gpu-availability) is deliberately NOT built:
// no lawful, machine-readable free source exists for cross-provider GPU
// availability, and sections ship only when a traceable source backs them.

import (
	"database/sql"
	"encoding/json"
)

func twoaiObservatoryEnsure(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_observatory_snapshots (
		snap_date date NOT NULL,
		metric text NOT NULL,
		value double precision NOT NULL,
		PRIMARY KEY (snap_date, metric))`)
	return err
}

func twoaiObservatory(db *sql.DB, today string) (int, error) {
	if err := twoaiObservatoryEnsure(db); err != nil {
		return 0, err
	}
	count := 0

	write := func(slug string, doc map[string]any) error {
		doc["uid"] = twoaiUID("section:" + slug)
		doc["tax"] = slug
		doc["generated"] = today
		var name, blurb string
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug=$1`, slug).Scan(&name, &blurb)
		doc["name"] = name
		doc["blurb"] = blurb
		j, _ := json.Marshal(doc)
		_, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'obs-section',$2::jsonb,$3,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			"observatory/"+slug+".json", string(j), slug)
		if err == nil {
			count++
		}
		return err
	}
	snap := func(metric string, v float64) {
		db.Exec(`INSERT INTO twoai_observatory_snapshots (snap_date, metric, value)
			VALUES (current_date, $1, $2)
			ON CONFLICT (snap_date, metric) DO UPDATE SET value=EXCLUDED.value`, metric, v)
	}
	series := func(metric string) []map[string]any {
		var out []map[string]any
		rows, err := db.Query(`SELECT snap_date::text, value FROM twoai_observatory_snapshots
			WHERE metric=$1 ORDER BY snap_date DESC LIMIT 90`, metric)
		if err != nil {
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var d string
			var v float64
			if rows.Scan(&d, &v) == nil {
				out = append(out, map[string]any{"date": d, "value": v})
			}
		}
		return out
	}
	type row = map[string]any
	collect := func(q string, args ...any) []row {
		var out []row
		rows, err := db.Query(q, args...)
		if err != nil {
			return out
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if rows.Scan(ptrs...) == nil {
				r := row{}
				for i, c := range cols {
					if b, ok := vals[i].([]byte); ok {
						r[c] = string(b)
					} else {
						r[c] = vals[i]
					}
				}
				out = append(out, r)
			}
		}
		return out
	}

	// ---- Model Release Cadence: createdAt of every tracked open model.
	var modelsTracked int
	db.QueryRow(`SELECT count(*) FROM twoai_model_catalog WHERE source='huggingface'`).Scan(&modelsTracked)
	cadence := collect(`SELECT to_char(date_trunc('month',(data->>'createdAt')::timestamptz),'YYYY-MM') AS month, count(*) AS n
		FROM twoai_model_catalog WHERE source='huggingface' AND data->>'createdAt' IS NOT NULL
		GROUP BY 1 ORDER BY 1 DESC LIMIT 24`)
	newest := collect(`SELECT data->>'id' AS id, (data->>'createdAt')::date::text AS released, section
		FROM twoai_model_catalog WHERE source='huggingface' AND data->>'createdAt' IS NOT NULL
		ORDER BY (data->>'createdAt')::timestamptz DESC LIMIT 12`)
	snap("models_tracked", float64(modelsTracked))
	if err := write("obs-release-cadence", row{
		"models": modelsTracked, "by_month": cadence, "newest": newest,
		"series": series("models_tracked"),
	}); err != nil {
		return count, err
	}

	// ---- Repository Activity: recency and mass of the tracked repos.
	var repoN, pushed7, pushed30 int
	db.QueryRow(`SELECT count(DISTINCT repo),
		count(DISTINCT repo) FILTER (WHERE (data->>'pushed_at')::timestamptz > now()-interval '7 days'),
		count(DISTINCT repo) FILTER (WHERE (data->>'pushed_at')::timestamptz > now()-interval '30 days')
		FROM twoai_repo_catalog`).Scan(&repoN, &pushed7, &pushed30)
	topRepos := collect(`SELECT DISTINCT ON (repo) repo, (data->>'stars')::bigint AS stars,
		(data->>'pushed_at')::date::text AS pushed FROM twoai_repo_catalog
		ORDER BY repo, (data->>'stars')::bigint DESC`)
	// order by stars desc, cap 15
	if len(topRepos) > 1 {
		for i := 0; i < len(topRepos); i++ {
			for j := i + 1; j < len(topRepos); j++ {
				si, _ := topRepos[i]["stars"].(int64)
				sj, _ := topRepos[j]["stars"].(int64)
				if sj > si {
					topRepos[i], topRepos[j] = topRepos[j], topRepos[i]
				}
			}
		}
	}
	if len(topRepos) > 15 {
		topRepos = topRepos[:15]
	}
	var starSum float64
	db.QueryRow(`SELECT COALESCE(sum(s),0) FROM (SELECT DISTINCT ON (repo) (data->>'stars')::float AS s FROM twoai_repo_catalog ORDER BY repo) x`).Scan(&starSum)
	snap("repos_tracked", float64(repoN))
	snap("repos_pushed_7d", float64(pushed7))
	snap("repos_stars_total", starSum)
	if err := write("obs-github-activity", row{
		"repos": repoN, "pushed_7d": pushed7, "pushed_30d": pushed30,
		"stars_total": int64(starSum), "top": topRepos,
		"series": series("repos_stars_total"),
	}); err != nil {
		return count, err
	}

	// ---- Hugging Face Trends: leaders, licences, arrivals across the catalogue.
	var dlTotal float64
	db.QueryRow(`SELECT COALESCE(sum((data->>'downloads')::float),0) FROM twoai_model_catalog WHERE source='huggingface'`).Scan(&dlTotal)
	leaders := collect(`SELECT DISTINCT ON (data->>'id') data->>'id' AS id, (data->>'downloads')::bigint AS downloads
		FROM twoai_model_catalog WHERE source='huggingface' ORDER BY data->>'id', (data->>'downloads')::bigint DESC`)
	if len(leaders) > 1 {
		for i := 0; i < len(leaders); i++ {
			for j := i + 1; j < len(leaders); j++ {
				di, _ := leaders[i]["downloads"].(int64)
				dj, _ := leaders[j]["downloads"].(int64)
				if dj > di {
					leaders[i], leaders[j] = leaders[j], leaders[i]
				}
			}
		}
	}
	if len(leaders) > 12 {
		leaders = leaders[:12]
	}
	licences := collect(`SELECT COALESCE(lic,'undeclared') AS licence, count(*) AS n FROM (
		SELECT DISTINCT ON (data->>'id') (SELECT replace(t,'license:','') FROM jsonb_array_elements_text(data->'tags') t WHERE t LIKE 'license:%' LIMIT 1) AS lic
		FROM twoai_model_catalog WHERE source='huggingface' ORDER BY data->>'id') x
		GROUP BY 1 ORDER BY 2 DESC LIMIT 12`)
	snap("hf_downloads_total", dlTotal)
	if err := write("obs-huggingface", row{
		"models": modelsTracked, "downloads_total": int64(dlTotal),
		"leaders": leaders, "licences": licences,
		"series": series("hf_downloads_total"),
	}); err != nil {
		return count, err
	}

	// ---- API Uptime: share of healthy observations per provider.
	var provTotal, provHealthy int
	db.QueryRow(`SELECT count(DISTINCT provider),
		count(DISTINCT provider) FILTER (WHERE indicator='none') FROM (
		SELECT DISTINCT ON (provider) provider, indicator FROM twoai_status_snapshots
		ORDER BY provider, taken_at DESC) latest`).Scan(&provTotal, &provHealthy)
	uptime := collect(`SELECT provider, count(*) AS observations,
		round(100.0*count(*) FILTER (WHERE indicator='none')/count(*),1)::float AS healthy_pct,
		min(taken_at)::date::text AS since
		FROM twoai_status_snapshots GROUP BY provider ORDER BY provider`)
	snap("providers_healthy", float64(provHealthy))
	snap("providers_total", float64(provTotal))
	if err := write("obs-api-uptime", row{
		"providers": provTotal, "healthy_now": provHealthy, "table": uptime,
		"series": series("providers_healthy"),
	}); err != nil {
		return count, err
	}

	// ---- Funding Activity: Form D cadence from the ledger.
	var formdTotal, formdCompanies int
	db.QueryRow(`SELECT count(*), count(DISTINCT uid) FROM twoai_company_formd`).Scan(&formdTotal, &formdCompanies)
	byMonth := collect(`SELECT to_char(date_trunc('month', filed::date),'YYYY-MM') AS month, count(*) AS n
		FROM twoai_company_formd WHERE filed <> '' GROUP BY 1 ORDER BY 1 DESC LIMIT 24`)
	recentF := collect(`SELECT f.issuer, f.filed, f.total_sold, p.name FROM twoai_company_formd f
		LEFT JOIN twoai_company_profiles p ON p.uid=f.uid
		WHERE f.filed <> '' ORDER BY f.filed DESC LIMIT 12`)
	snap("formd_filings", float64(formdTotal))
	if err := write("obs-funding-activity", row{
		"filings": formdTotal, "companies": formdCompanies,
		"by_month": byMonth, "recent": recentF,
		"series": series("formd_filings"),
	}); err != nil {
		return count, err
	}

	// ---- Patent Filings: granted-patent floors from the USPTO sweep.
	var patCompanies int
	var patTotal float64
	db.QueryRow(`SELECT count(*), COALESCE(sum(patents_count),0) FROM twoai_company_profiles WHERE patents_count > 0`).Scan(&patCompanies, &patTotal)
	topPat := collect(`SELECT name, patents_count, uid FROM twoai_company_profiles
		WHERE patents_count > 0 ORDER BY patents_count DESC LIMIT 15`)
	snap("patented_companies", float64(patCompanies))
	snap("patents_total", patTotal)
	if err := write("obs-patent-filings", row{
		"companies": patCompanies, "patents_total": int64(patTotal), "top": topPat,
		"series": series("patents_total"),
	}); err != nil {
		return count, err
	}

	// ---- Security Incidents: open incidents at tracked API providers.
	// Scope is stated on the page: service incidents from provider status
	// feeds, not a breach or vulnerability database.
	var openNow float64
	db.QueryRow(`SELECT COALESCE(sum(open_incidents),0) FROM (
		SELECT DISTINCT ON (provider) provider, open_incidents FROM twoai_status_snapshots
		ORDER BY provider, taken_at DESC) latest`).Scan(&openNow)
	incByProv := collect(`SELECT provider, max(open_incidents) AS peak_open,
		count(*) FILTER (WHERE open_incidents > 0) AS observations_with_incidents,
		count(*) AS observations FROM twoai_status_snapshots GROUP BY provider
		HAVING max(open_incidents) > 0 ORDER BY 2 DESC`)
	snap("open_incidents", openNow)
	if err := write("obs-security-incidents", row{
		"open_now": int64(openNow), "providers": provTotal, "by_provider": incByProv,
		"series": series("open_incidents"),
	}); err != nil {
		return count, err
	}

	return count, nil
}
