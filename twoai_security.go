package main

// twoai_security.go - the AI Security and Risk domain: sixteen sections
// rendered from the curated twoai_security table, where every row carries
// a source URL that was verified before insert and is re-verified daily
// by the apistatus reverify sweep. The spine is standards bodies and
// government agencies - OWASP's GenAI project, MITRE ATLAS, NIST's
// adversarial ML taxonomy, CISA, NCSC, the FBI's IC3 - plus the labs'
// own published threat and red-team reporting. Where the site already
// tracks the adjacent ground (state deepfake bills, MCP servers, dataset
// licensing, the compliance library), sections cross-link it and compute
// live counts instead of restating claims.

import (
	"database/sql"
	"encoding/json"
)

var twoaiSecuritySections = []string{
	"sec-prompt-injection", "sec-jailbreaks", "sec-model-poisoning",
	"sec-supply-chain", "sec-data-leakage", "sec-model-theft",
	"sec-shadow-ai", "sec-ai-malware", "sec-deepfakes", "sec-ai-phishing",
	"sec-tooling", "sec-agent-identity", "sec-agent-security",
	"sec-ai-soc", "sec-ai-privacy", "sec-red-teaming",
}

func twoaiSecurity(db *sql.DB, today string) (int, error) {
	count := 0
	// Live context the domain can compute from tables the site already keeps.
	var deepfakeBills, mcpServers, complianceDocs, lawsuitPages int
	// Counted from the state-law pages the site itself renders - the same
	// verify-against-production rule the graph census learned at seq 308:
	// pipeline.documents has no source column and the first guess scanned 0.
	db.QueryRow(`SELECT count(*) FROM twoai_pages, jsonb_array_elements(data->'bills') b
		WHERE kind='state-law' AND jsonb_typeof(data->'bills')='array'
		AND (b->>'title' ILIKE '%deepfake%' OR b->>'title' ILIKE '%deep fake%'
			OR b->>'title' ILIKE '%synthetic%')`).Scan(&deepfakeBills)
	db.QueryRow(`SELECT count(*) FROM twoai_pages WHERE kind='mcp-server'`).Scan(&mcpServers)
	db.QueryRow(`SELECT count(*) FROM twoai_pages WHERE path LIKE 'compliance/%'`).Scan(&complianceDocs)
	db.QueryRow(`SELECT COALESCE(sum(url_count),0) FROM twoai_pages WHERE kind='lawsuits'`).Scan(&lawsuitPages)

	domainLabels := map[string]string{
		"gov":  "Security Governance and Risk Management",
		"ops":  "Security Operations",
		"arch": "Architecture and Engineering",
		"app":  "Application and Product Security",
		"tprm": "Third-Party and Supply Chain Risk",
		"data": "Data Protection and Privacy",
	}
	for _, slug := range twoaiSecuritySections {
		type item struct {
			Slug   string `json:"slug"`
			Name   string `json:"name"`
			Kind   string `json:"kind"`
			Note   string `json:"note"`
			Source string `json:"source_name"`
			URL    string `json:"source_url"`
		}
		var items []item
		rows, err := db.Query(`SELECT slug, name, kind, note, source_name, source_url
			FROM twoai_security WHERE section_slug=$1 ORDER BY sort`, slug)
		if err != nil {
			return count, err
		}
		for rows.Next() {
			var it item
			if rows.Scan(&it.Slug, &it.Name, &it.Kind, &it.Note, &it.Source, &it.URL) == nil {
				items = append(items, it)
			}
		}
		rows.Close()
		if len(items) == 0 {
			continue // a security section never ships empty
		}
		// The six-domain treatment: substantive curated prose per security
		// domain, sourced from the SRJ security volumes and the standards
		// spine, so a topic renders under every domain it genuinely touches.
		type domainBlock struct {
			Slug  string `json:"slug"`
			Label string `json:"label"`
			Body  string `json:"body"`
		}
		var domains []domainBlock
		drows, err := db.Query(`SELECT domain_slug, body FROM twoai_security_domains
			WHERE section_slug=$1 ORDER BY sort`, slug)
		if err != nil {
			return count, err
		}
		for drows.Next() {
			var d domainBlock
			if drows.Scan(&d.Slug, &d.Body) == nil {
				d.Label = domainLabels[d.Slug]
				domains = append(domains, d)
			}
		}
		drows.Close()
		var name, blurb string
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug=$1`, slug).Scan(&name, &blurb)
		doc := map[string]any{
			"uid": twoaiUID("section:" + slug), "tax": slug, "generated": today,
			"name": name, "blurb": blurb, "items": items, "domains": domains,
			"stats": map[string]int{
				"deepfake_bills": deepfakeBills, "mcp_servers": mcpServers,
				"compliance_docs": complianceDocs, "lawsuit_pages": lawsuitPages,
			},
		}
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'security-section',$2::jsonb,$3,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			"security/"+slug+".json", string(j), slug); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
