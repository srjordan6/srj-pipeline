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
	"fmt"
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
		// The four-part treatment Stephen asked for: define, examples, how to
		// find it, how to defend. Curated in twoai_security_topics, rendered
		// above the six-domain reads so a reader gets the subject in depth
		// before the domain-by-domain lens.
		type topicBlock struct {
			Block string `json:"block"`
			Body  string `json:"body"`
		}
		var topic []topicBlock
		trows, terr := db.Query(`SELECT block, body FROM twoai_security_topics
			WHERE section_slug=$1 ORDER BY CASE block
				WHEN 'def' THEN 1 WHEN 'examples' THEN 2 WHEN 'detect' THEN 3 WHEN 'defend' THEN 4 END`, slug)
		if terr != nil {
			return count, terr
		}
		for trows.Next() {
			var tb topicBlock
			if trows.Scan(&tb.Block, &tb.Body) == nil {
				topic = append(topic, tb)
			}
		}
		trows.Close()
		// Deep-dive blocks: per-topic structured treatments beyond the
		// four-part standard, grouped (layers of the chain, attack areas, an
		// end-to-end scenario, a control framework, board questions, the
		// trust chain). Present only where a topic has earned that depth.
		type deepBlock struct {
			Label string `json:"label"`
			Body  string `json:"body"`
		}
		type deepGroup struct {
			Grp     string      `json:"grp"`
			Heading string      `json:"heading"`
			Intro   string      `json:"intro"`
			Layout  string      `json:"layout"`
			Blocks  []deepBlock `json:"blocks"`
		}
		var deep []deepGroup
		ddrows, derr := db.Query(`SELECT grp, heading, intro, layout FROM twoai_security_deepdive_groups
			WHERE section_slug=$1 ORDER BY sort`, slug)
		if derr != nil {
			return count, derr
		}
		for ddrows.Next() {
			var dg deepGroup
			if ddrows.Scan(&dg.Grp, &dg.Heading, &dg.Intro, &dg.Layout) == nil {
				deep = append(deep, dg)
			}
		}
		ddrows.Close()
		for gi := range deep {
			brows, berr := db.Query(`SELECT label, body FROM twoai_security_deepdive
				WHERE section_slug=$1 AND grp=$2 ORDER BY sort`, slug, deep[gi].Grp)
			if berr != nil {
				return count, berr
			}
			for brows.Next() {
				var dbk deepBlock
				if brows.Scan(&dbk.Label, &dbk.Body) == nil {
					deep[gi].Blocks = append(deep[gi].Blocks, dbk)
				}
			}
			brows.Close()
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
			"name": name, "blurb": blurb, "items": items, "domains": domains, "topic": topic, "deep": deep,
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

	// ---- The AI Security and Risk hub: six domains first, topics beneath.
	// The category page now lists the six security domains for this domain
	// (via the ecosystem override), and each domain path points at an anchor
	// on this hub, where every topic appears under every domain it has
	// curated prose for. Sixteen topics flat told a reader nothing about how
	// the coverage is organized; the six-domain grouping is the organizing
	// idea of the whole security build.
	{
		type topicRef struct {
			Name  string `json:"name"`
			Path  string `json:"path"`
			Blurb string `json:"blurb"`
		}
		type domGroup struct {
			Slug   string     `json:"slug"`
			Label  string     `json:"label"`
			Blurb  string     `json:"blurb"`
			Topics []topicRef `json:"topics"`
		}
		var groups []domGroup
		grows, gerr := db.Query(`SELECT slug, label, blurb FROM twoai_security_domain_defs ORDER BY sort`)
		if gerr != nil {
			return count, gerr
		}
		for grows.Next() {
			var g domGroup
			if grows.Scan(&g.Slug, &g.Label, &g.Blurb) == nil {
				groups = append(groups, g)
			}
		}
		grows.Close()
		for gi := range groups {
			trows, terr := db.Query(`SELECT t.name, COALESCE(t.live_path,''), COALESCE(t.blurb,'')
				FROM twoai_security_domains sd JOIN twoai_taxonomy t ON t.slug = sd.section_slug
				WHERE sd.domain_slug = $1 ORDER BY t.sort`, groups[gi].Slug)
			if terr != nil {
				return count, terr
			}
			for trows.Next() {
				var tr topicRef
				if trows.Scan(&tr.Name, &tr.Path, &tr.Blurb) == nil && tr.Path != "" {
					groups[gi].Topics = append(groups[gi].Topics, tr)
				}
			}
			trows.Close()
		}
		var hn, hb string
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug='ai-security-risk'`).Scan(&hn, &hb)
		var topicCount int
		db.QueryRow(`SELECT count(DISTINCT section_slug) FROM twoai_security_domains`).Scan(&topicCount)
		// MITRE ATLAS rides on the hub: poll the manifest, ingest a release
		// only when it is new, and render the current state either way.
		twoaiAtlasWatch(db)
		twoaiAvidWatch(db)
		twoaiOwaspWatch(db)
		hub := map[string]any{
			"uid": twoaiUID("section:ai-security-risk"), "tax": "ai-security-risk",
			"shape": "security-hub", "name": hn, "blurb": hb, "generated": today,
			"domains": groups, "topic_count": topicCount,
		}
		if atlas := twoaiAtlasDoc(db); atlas != nil {
			hub["atlas"] = atlas
		}
		if avid := twoaiAvidDoc(db); avid != nil {
			hub["avid"] = avid
		}
		if owasp := twoaiOwaspDoc(db); owasp != nil {
			hub["owasp"] = owasp
		}
		hj, _ := json.Marshal(hub)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ('security/index.json','security-hub',$1::jsonb,'ai-security-risk',1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`, string(hj)); err != nil {
			return count, err
		}
		count++
		fmt.Printf("twoai_build: security hub domains=%d topics=%d\n", len(groups), topicCount)
	}

	return count, nil
}
