package main

// twoai_graph.go - the Entity Graph made visible. The graph already exists
// in this system as typed identifiers (entity uid, section uid, docket,
// arxiv id) and the cross-references between them; these two pages census
// it live so the claim "every company, model, paper, person, dataset,
// benchmark, and regulation is a node" is a counted fact, not a slogan.
// Every number is computed at render time from the same tables the site
// renders from, so the census can never drift from the site.

import (
	"database/sql"
	"encoding/json"
)

func twoaiGraph(db *sql.DB, today string) (int, error) {
	count := 0
	one := func(q string) int64 {
		var n sql.NullInt64
		db.QueryRow(q).Scan(&n)
		return n.Int64
	}
	write := func(slug, path string, doc map[string]any) error {
		doc["uid"] = twoaiUID("section:" + slug)
		doc["tax"] = slug
		doc["generated"] = today
		var name, blurb string
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug=$1`, slug).Scan(&name, &blurb)
		doc["name"] = name
		doc["blurb"] = blurb
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'tech-section',$2::jsonb,$3,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			path, string(j), slug); err != nil {
			return err
		}
		count++
		return nil
	}
	type nrow struct {
		Type   string `json:"type"`
		Count  int64  `json:"count"`
		Scheme string `json:"scheme"`
		Home   string `json:"home"`
	}
	nodes := []nrow{
		{"Companies", one(`SELECT jsonb_array_length(data->'companies') FROM twoai_pages WHERE path='companies/index.json'`),
			"8-char entity uid", "/ai-ecosystem/ecosystem-entities-market-and-operations/"},
		{"Open models", one(`SELECT count(DISTINCT data->>'id') FROM twoai_model_catalog WHERE source='huggingface' AND delisted_at IS NULL`),
			"Hugging Face repo id", "/ai-ecosystem/technology-and-core-infrastructure/87868942/"},
		{"API models", one(`SELECT count(*) FROM twoai_model_catalog WHERE source='openrouter' AND delisted_at IS NULL`),
			"provider/model id", "/ai-ecosystem/technology-and-core-infrastructure/56f96b2b/"},
		{"Research papers", one(`SELECT count(*) FROM twoai_research_papers`),
			"DOI or arXiv id, linked to the original publisher", "/research/"},
		{"Papers cited by tracked models", one(`SELECT count(DISTINCT t) FROM twoai_model_catalog, jsonb_array_elements_text(data->'tags') t WHERE source='huggingface' AND delisted_at IS NULL AND t LIKE 'arxiv:%'`),
			"arXiv id from the model's own tags", "/research/"},
		{"People", one(`SELECT count(*) FROM twoai_pages WHERE kind='person'`),
			"8-char person uid", "/ai-ecosystem/ecosystem-entities-market-and-operations/"},
		{"Datasets", one(`SELECT count(*) FROM twoai_dataset_catalog`),
			"Hugging Face dataset id", "/ai-ecosystem/technology-and-core-infrastructure/12245496/"},
		{"Benchmarks tracked", one(`SELECT count(*) FROM twoai_pages WHERE kind='benchmark'`),
			"benchmark page", "/ai-ecosystem/research-knowledge-and-learning/"},
		{"State AI bills", one(`SELECT COALESCE(sum(jsonb_array_length(data->'bills')),0) FROM twoai_pages WHERE kind='state-law' AND jsonb_typeof(data->'bills')='array'`),
			"state + bill number", "/ai-laws/"},
		{"Lawsuits", one(`SELECT COALESCE(sum(url_count),0) FROM twoai_pages WHERE kind='lawsuits'`),
			"court docket number via CourtListener", "/ai-lawsuits/"},
		{"Compliance documents", one(`SELECT count(*) FROM twoai_pages WHERE path LIKE 'compliance/%'`),
			"standard identifier (ISO, NIST, EU)", "/ai-compliance/"},
		{"MCP servers", one(`SELECT count(*) FROM twoai_pages WHERE kind='mcp-server'`),
			"registry id from the official MCP registry", "/mcp/"},
		{"Repositories", one(`SELECT count(DISTINCT repo) FROM twoai_repo_catalog`),
			"GitHub owner/name", "/ai-ecosystem/technology-and-core-infrastructure/2eb54277/"},
		{"Tools", one(`SELECT COALESCE(max(url_count),0) FROM twoai_pages WHERE path LIKE 'tools/index%'`),
			"tool page", "/ai-tools/"},
		{"Hardware entries", one(`SELECT count(*) FROM twoai_hardware`),
			"curated row with verified source", "/ai-ecosystem/technology-and-core-infrastructure/43b3b015/"},
		{"Industries", one(`SELECT count(*) FROM twoai_industries`),
			"sector page", "/ai-ecosystem/enterprise-applications-governance-and-tools/"},
	}
	var nodeTotal int64
	for _, n := range nodes {
		nodeTotal += n.Count
	}
	if err := write("entity-graph", "learn/graph-entities.json", map[string]any{
		"shape": "graph-nodes", "nodes": nodes, "node_total": nodeTotal,
	}); err != nil {
		return count, err
	}

	type erow struct {
		Type   string `json:"type"`
		Count  int64  `json:"count"`
		Method string `json:"method"`
		Home   string `json:"home"`
	}
	edges := []erow{
		{"Model cites paper", one(`SELECT count(*) FROM (SELECT DISTINCT data->>'id', t FROM twoai_model_catalog, jsonb_array_elements_text(data->'tags') t WHERE source='huggingface' AND delisted_at IS NULL AND t LIKE 'arxiv:%') x`),
			"arXiv ids the model authors put in their own Hub tags", "/ai-ecosystem/technology-and-core-infrastructure/87868942/"},
		{"Company named in lawsuit", one(`SELECT COALESCE(sum((c->>'cases')::int),0) FROM twoai_pages, jsonb_array_elements(data->'companies') c WHERE path='companies/index.json'`),
			"defendant matching against the CourtListener docket tracker", "/ai-lawsuits/"},
		{"Company publishes MCP server", one(`SELECT COALESCE(sum((c->>'mcp')::int),0) FROM twoai_pages, jsonb_array_elements(data->'companies') c WHERE path='companies/index.json'`),
			"publisher matching against the official MCP registry", "/mcp/"},
		{"Company ships product", one(`SELECT COALESCE(sum((c->>'products')::int),0) FROM twoai_pages, jsonb_array_elements(data->'companies') c WHERE path='companies/index.json'`),
			"curated tool directory attribution", "/ai-tools/"},
		{"Company files Form D", one(`SELECT count(*) FROM twoai_company_formd`),
			"exact-issuer match against SEC EDGAR full-text search, collisions excluded by verified CIK", "/ai-ecosystem/ecosystem-entities-market-and-operations/42bcd5be/"},
		{"Company holds granted patents", one(`SELECT count(*) FROM twoai_company_profiles WHERE patents_count>0`),
			"applicant-phrase match against the USPTO Open Data Portal, floors not totals", "/ai-ecosystem/ecosystem-entities-market-and-operations/2cb1cf97/"},
		{"Company is SEC registrant", one(`SELECT count(*) FROM twoai_company_profiles WHERE edgar IS NOT NULL`),
			"exact-name match against EDGAR company search, verified within 7 days", "/ai-ecosystem/ecosystem-entities-market-and-operations/a185389c/"},
		{"Person belongs to category", one(`SELECT count(*) FROM twoai_person_category`),
			"curated assignment in the people directory", "/ai-ecosystem/ecosystem-entities-market-and-operations/"},
		{"Model belongs to modality section", one(`SELECT count(*) FROM twoai_model_catalog WHERE source='huggingface' AND delisted_at IS NULL`),
			"the Hub's own pipeline tag or tag filter, provenance stated per section", "/ai-ecosystem/technology-and-core-infrastructure/87868942/"},
		{"API tracked by status feed", one(`SELECT count(DISTINCT provider) FROM twoai_status_snapshots`),
			"provider status feed polled every run", "/ai-ecosystem/technology-and-core-infrastructure/5b57e70c/"},
		{"Vendor feed maps to company", one(`SELECT count(*) FROM twoai_vendor_feeds WHERE entity_uid IS NOT NULL AND entity_uid<>''`),
			"curated entity uid on each of the vendor news feeds", "/ai-news/vendor/"},
	}
	var edgeTotal int64
	for _, e := range edges {
		edgeTotal += e.Count
	}
	if err := write("entity-relationships", "learn/graph-relationships.json", map[string]any{
		"shape": "graph-edges", "edges": edges, "edge_total": edgeTotal,
	}); err != nil {
		return count, err
	}
	return count, nil
}
