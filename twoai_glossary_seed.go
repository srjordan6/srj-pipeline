package main

// twoai_glossary_seed: define the terms this site uses and never defined.
//
// Stephen, 2026-09-17, after an audit of the site against the glossary: 116
// of 118 candidate terms are undefined, and the top ones sit on hundreds of
// pages. "colocation" is on 1,034 pages. The glossary is a model-and-training
// vocabulary; the site has grown a data centre registry, a hundred insurance
// pages and a law tracker, and none of that language is in it. Use DeepSeek
// to seed and build the pages.
//
// HOW A DEFINITION IS WRITTEN. The model is given the term and up to four
// passages from THIS SITE'S OWN PAGES where the term is used, and asked to
// define it as the site uses it. That matters: "transformer" on a data centre
// page is a piece of electrical equipment, and a definition written from
// general knowledge would give the reader the neural architecture. The
// passages anchor the definition to the usage the reader arrived from.
//
// WHERE IT WRITES. Two places, on purpose. site_content's
// resources/glossary.json is the library that renders, shared with
// srjconsultingservices.com; a term goes into its terms array with slug,
// term, definition, example, origin and category, and the publisher attaches
// the uid. synced_glossary_terms is the SRJ-side record; the same term goes
// there with authored_here=true so the publisher's drift check, which compares
// the two counts, stays quiet. A term already present in the library by slug
// is never rewritten.
//
// Every seeded definition records the model in author_note. The glossary has
// no draft mechanism, so these publish on the next run; the definitions are
// short, anchored to site usage, and each names its category, which is the
// review surface.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

type glossCandidate struct{ term, category string }

var glossaryCandidates = []glossCandidate{
	// Data centres and infrastructure
	{"Colocation", "Data Centres & Infrastructure"}, {"Critical IT Load", "Data Centres & Infrastructure"},
	{"Hyperscaler", "Data Centres & Infrastructure"}, {"N+1 Redundancy", "Data Centres & Infrastructure"},
	{"Gigawatt Campus", "Data Centres & Infrastructure"}, {"Substation", "Data Centres & Infrastructure"},
	{"Transformer (Electrical)", "Data Centres & Infrastructure"}, {"Data Hall", "Data Centres & Infrastructure"},
	{"Tier III / Tier IV (Uptime Institute)", "Data Centres & Infrastructure"}, {"PUE (Power Usage Effectiveness)", "Data Centres & Infrastructure"},
	{"Chiller", "Data Centres & Infrastructure"}, {"Rack Density", "Data Centres & Infrastructure"},
	{"Subsea Cable", "Data Centres & Infrastructure"}, {"Liquid Cooling", "Data Centres & Infrastructure"},
	{"Direct-to-Chip Cooling", "Data Centres & Infrastructure"}, {"Immersion Cooling", "Data Centres & Infrastructure"},
	{"Coolant Distribution Unit (CDU)", "Data Centres & Infrastructure"}, {"Power Purchase Agreement (PPA)", "Data Centres & Infrastructure"},
	{"Interconnection Queue", "Data Centres & Infrastructure"}, {"Free Cooling", "Data Centres & Infrastructure"},
	{"Fuel Cell", "Data Centres & Infrastructure"}, {"Behind-the-Meter", "Data Centres & Infrastructure"},
	{"GPU Cloud", "Data Centres & Infrastructure"}, {"Neocloud", "Data Centres & Infrastructure"},
	{"Small Modular Reactor (SMR)", "Data Centres & Infrastructure"}, {"WUE (Water Usage Effectiveness)", "Data Centres & Infrastructure"},
	{"Curtailment", "Data Centres & Infrastructure"}, {"Dark Fibre", "Data Centres & Infrastructure"},
	{"PFAS", "Data Centres & Infrastructure"}, {"Thermal Runaway", "Data Centres & Infrastructure"},
	{"Load Shedding", "Data Centres & Infrastructure"}, {"Battery Energy Storage System (BESS)", "Data Centres & Infrastructure"},
	{"Demand Response", "Data Centres & Infrastructure"}, {"Dielectric Fluid", "Data Centres & Infrastructure"},
	{"UL 9540A", "Data Centres & Infrastructure"},
	// AI insurance
	{"Umbrella Policy", "AI Insurance"}, {"Business Interruption (BI)", "AI Insurance"},
	{"Waiting Period", "AI Insurance"}, {"Sublimit", "AI Insurance"}, {"Endorsement", "AI Insurance"},
	{"Cedent", "AI Insurance"}, {"Per Occurrence Limit", "AI Insurance"}, {"Aggregate Limit", "AI Insurance"},
	{"Contingent Business Interruption (CBI)", "AI Insurance"}, {"Equipment Breakdown", "AI Insurance"},
	{"Errors and Omissions (E&O)", "AI Insurance"}, {"Underwriting", "AI Insurance"},
	{"Replacement Cost", "AI Insurance"}, {"Actual Cash Value", "AI Insurance"},
	{"Retroactive Date", "AI Insurance"}, {"Captive Insurer", "AI Insurance"}, {"Extra Expense", "AI Insurance"},
	{"Aggregation (Insurance)", "AI Insurance"}, {"Parametric Insurance", "AI Insurance"},
	{"Sudden and Accidental", "AI Insurance"}, {"Reinsurance", "AI Insurance"}, {"Indemnity Period", "AI Insurance"},
	{"Total Insured Value (TIV)", "AI Insurance"}, {"Subrogation", "AI Insurance"},
	{"Directors and Officers (D&O)", "AI Insurance"}, {"Pollution Exclusion", "AI Insurance"},
	{"Claims-Made vs Occurrence", "AI Insurance"}, {"War Exclusion", "AI Insurance"}, {"Surplus Lines", "AI Insurance"},
	{"Environmental Impairment Liability (EIL)", "AI Insurance"}, {"Technology E&O", "AI Insurance"},
	{"Material Misrepresentation", "AI Insurance"}, {"Delay in Startup (DSU)", "AI Insurance"},
	{"Catastrophe Bond", "AI Insurance"}, {"Silent Cyber", "AI Insurance"}, {"Silent AI", "AI Insurance"},
	{"Layered Program", "AI Insurance"}, {"Hammer Clause", "AI Insurance"}, {"Probable Maximum Loss (PML)", "AI Insurance"},
	{"Ensuing Loss", "AI Insurance"}, {"Loss Run", "AI Insurance"}, {"Loss Ratio", "AI Insurance"},
	{"Notice Prejudice", "AI Insurance"}, {"Builders Risk", "AI Insurance"}, {"Duty to Defend", "AI Insurance"},
	{"NAIC Model Bulletin", "AI Insurance"}, {"Carve-Back", "AI Insurance"}, {"Self-Insured Retention (SIR)", "AI Insurance"},
	{"Manuscript Form", "AI Insurance"}, {"Algorithmic Underwriting", "AI Insurance"}, {"Risk Retention Group", "AI Insurance"},
	{"Lloyd's of London", "AI Insurance"}, {"Hard Market", "AI Insurance"},
	// AI security
	{"AVID (AI Vulnerability Database)", "AI Security & Assurance"}, {"Supply Chain Attack", "AI Security & Assurance"},
	{"Model Poisoning", "AI Security & Assurance"}, {"Training Data Extraction", "AI Security & Assurance"},
	{"Biometric Spoofing", "AI Security & Assurance"}, {"Compute Hijacking", "AI Security & Assurance"},
	{"Liveness Detection", "AI Security & Assurance"}, {"Synthetic Identity Fraud", "AI Security & Assurance"},
	{"OT Ransomware", "AI Security & Assurance"}, {"AI Incident Database (AIID)", "AI Security & Assurance"},
	// AI law
	{"Impact Assessment", "AI Law"}, {"Private Right of Action", "AI Law"}, {"Deployer", "AI Law"},
	{"Companion Chatbot", "AI Law"}, {"Whistleblower Protection", "AI Law"}, {"Frontier Developer", "AI Law"},
	{"Safe Harbor", "AI Law"}, {"Algorithmic Discrimination", "AI Law"}, {"UDAP", "AI Law"},
	{"Consequential Decision", "AI Law"}, {"Preemption", "AI Law"}, {"Critical Safety Incident", "AI Law"},
	{"AI Auditor", "AI Law"}, {"Duty of Care", "AI Law"},
}

type glossReading struct {
	Definition string `json:"definition"`
	Example    string `json:"example"`
	Origin     string `json:"origin"`
}

// twoaiGlossarySlug makes a term name safe as a URL path segment.
//
// twoaiSlug was not enough. "Tier III / Tier IV (Uptime Institute)" became
// "tier-iii-/-tier-iv-(uptime-institute)", the slash made Astro read it as a
// nested route with a missing parameter, and the site build failed twice on
// 2026-09-17 before the cause was visible. Parentheses, ampersands and plus
// signs in twenty other seeded terms were the same class of problem waiting.
// A slug is a URL, a URL that can never move once published, and it is worth
// being strict about at the point it is minted.
func twoaiGlossarySlug(term string) string {
	s := strings.ToLower(term)
	s = strings.ReplaceAll(s, "&", " and ")
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func twoaiGlossarySeed(db *sql.DB) error {
	limit := len(glossaryCandidates)
	if v := os.Getenv("TWOAI_GLOSSARY_SEED_LIMIT"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	var lib string
	if err := db.QueryRow(`SELECT data::text FROM site_content WHERE path='resources/glossary.json'`).Scan(&lib); err != nil {
		return fmt.Errorf("glossary library: %w", err)
	}
	var g map[string]any
	if err := json.Unmarshal([]byte(lib), &g); err != nil {
		return err
	}
	terms, _ := g["terms"].([]any)
	have := map[string]bool{}
	for _, raw := range terms {
		if t, ok := raw.(map[string]any); ok {
			if s, _ := t["slug"].(string); s != "" {
				have[s] = true
			}
			if n, _ := t["term"].(string); n != "" {
				have[strings.ToLower(n)] = true
			}
		}
	}

	system := `You are writing an entry for the AI glossary on theworldofai.org, a sourced public reference on AI, data centres, AI insurance and AI law. You are given a term and passages from the site's own pages where it is used. Define the term AS THE SITE USES IT, in the sense those passages show; if a term has a different meaning in another field, do not give that one.

Return ONLY a JSON object, no prose, no fences:
{"definition": "two or three sentences, plain English, a beginner can follow, no jargon inside the definition that itself needs defining",
 "example": "one sentence showing the term in use, drawn from the kind of situation the passages describe",
 "origin": "one sentence on where the term comes from or when it entered use, or 'Standard industry term' if unremarkable"}

Plain English. No hyphens in prose, use commas or periods.`

	made, failed, skipped := 0, 0, 0
	now := time.Now().UTC().Format("2006-01-02")
	for _, c := range glossaryCandidates {
		if made+failed >= limit {
			break
		}
		slug := twoaiGlossarySlug(c.term)
		if have[slug] || have[strings.ToLower(c.term)] {
			skipped++
			continue
		}
		// Passages from this site where the term appears, so the definition
		// matches the usage the reader arrived from.
		needle := strings.ToLower(regexp.MustCompile(`\s*\(.*\)$`).ReplaceAllString(c.term, ""))
		var passages []string
		prow, perr := db.Query(`SELECT substring(data::text FROM position($1 IN lower(data::text)) - 200 FOR 500)
			FROM twoai_pages WHERE position($1 IN lower(data::text)) > 200 LIMIT 4`, needle)
		if perr == nil {
			for prow.Next() {
				var p string
				if prow.Scan(&p) == nil {
					p = regexp.MustCompile(`["\\{}\[\]]|\\n`).ReplaceAllString(p, " ")
					passages = append(passages, strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(p, " ")))
				}
			}
			prow.Close()
		}
		user := fmt.Sprintf("TERM: %s\nCATEGORY: %s\n\nPASSAGES FROM THIS SITE WHERE IT IS USED:\n%s",
			c.term, c.category, nz(strings.Join(passages, "\n---\n"), "(no passages found; define in the sense of the category)"))
		raw, model, err := twoaiGenerate("GLOSSARY_SEED", system, user)
		if err != nil {
			fmt.Fprintf(os.Stderr, "twoai_glossary_seed: %s: %v\n", c.term, err)
			failed++
			continue
		}
		raw = strings.TrimSpace(raw)
		if i := strings.Index(raw, "{"); i >= 0 {
			if k := strings.LastIndex(raw, "}"); k > i {
				raw = raw[i : k+1]
			}
		}
		var r glossReading
		if err := json.Unmarshal([]byte(raw), &r); err != nil || len(strings.TrimSpace(r.Definition)) < 40 {
			fmt.Fprintf(os.Stderr, "twoai_glossary_seed: %s: unparseable: %s\n", c.term, truncate(raw, 120))
			failed++
			continue
		}
		entry := map[string]any{
			"slug": slug, "term": c.term, "category": c.category,
			"definition": r.Definition, "example": r.Example, "origin": r.Origin,
			"authored_here": true, "authored_on": now, "author_note": "Seeded by " + model + " from this site's own usage of the term; reviewed by Stephen on publication.",
		}
		terms = append(terms, entry)
		have[slug] = true
		// The SRJ-side record, so the two tables agree and the drift check
		// stays quiet.
		if _, err := db.Exec(`INSERT INTO synced_glossary_terms (term, definition, example, category, is_active, synced_at, authored_here, authored_on, author_note, slug)
			VALUES ($1,$2,$3,$4,true,now(),true,$5,$6,$7)
			ON CONFLICT DO NOTHING`, c.term, r.Definition, r.Example, c.category, now, entry["author_note"], slug); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_glossary_seed: synced row %s: %v\n", c.term, err)
		}
		made++
		fmt.Printf("twoai_glossary_seed: %s <- %s (%d passages)\n", c.term, model, len(passages))
		time.Sleep(300 * time.Millisecond)
	}
	if made > 0 {
		g["terms"] = terms
		g["count"] = len(terms)
		out, _ := json.Marshal(g)
		if _, err := db.Exec(`UPDATE site_content SET data=$1::jsonb, updated_at=now() WHERE path='resources/glossary.json'`, string(out)); err != nil {
			return fmt.Errorf("write library: %w", err)
		}
	}
	fmt.Printf("twoai_glossary_seed: added=%d failed=%d already_present=%d; library now %d terms\n", made, failed, skipped, len(terms))
	return nil
}
