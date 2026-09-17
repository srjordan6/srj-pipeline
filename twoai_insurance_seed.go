package main

// twoai_insurance_seed: draft the two template sections on each AI Insurance
// coverage item, for Stephen to review and correct.
//
// Stephen, 2026-09-16: have our new AI model from Ollama do the seeding for
// all the pages. Each of the 100 items carries a fixed template - What the
// Underwriter Wants to Know, and What the Insured Needs Secured in five parts -
// and filling 100 of those by hand is weeks of work. A model can produce a
// competent first draft of each in seconds, and Stephen, who co-owns an
// insurance group, corrects it from knowledge the model does not have.
//
// WHAT THIS IS AND IS NOT. The output is a draft reading, not a finding. It
// goes onto the page with seeded_by and seeded_on set, the page stays draft,
// and nothing here reaches the index until Stephen removes the draft flag
// after reviewing it. The stage never overwrites a section that already has
// content, so a reviewed item is never re-seeded over. Re-seeding is a
// deliberate act: clear the field first.
//
// The model is told what the site is, what the item is, and what evidence
// already sits on the page, and asked to answer as a placement broker would.
// It is told not to invent carrier names, limit figures or policy form
// numbers; where a figure is genuinely market-standard it may state it, and
// where it is not it says so. Stephen's review is the check on that.
//
// Routed through twoaiGenerate under stage INSURANCE_SEED, so the provider and
// model follow pipeline.env like every other judgment job.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type insSeed struct {
	UnderwriterWantsToKnow []string `json:"underwriter_wants_to_know"`
	Part1                  []string `json:"part_1_core_third_party_liability_exposures"`
	Part2                  struct {
		PaperBasis           string `json:"paper_basis"`
		CGLPrimary           string `json:"cgl_primary"`
		ExcessUmbrellaTowers string `json:"excess_umbrella_towers"`
	} `json:"part_2_paper_type_and_limit_structure"`
	Part3 []string `json:"part_3_high_value_red_flags"`
	Part4 struct {
		PerOccurrence        string `json:"per_occurrence_primary_excess_layered_property"`
		PropertyEBEquipment  string `json:"property_eb_equipment"`
		BIWaitingPeriod      string `json:"bi_waiting_period"`
		BusinessInterruption string `json:"business_interruption"`
	} `json:"part_4_program_structure_and_limits"`
	Manuscript []string `json:"manuscript_wording_and_carve_backs"`
}

func twoaiInsuranceSeed(db *sql.DB) error {
	limit := 100
	if v := os.Getenv("TWOAI_INSURANCE_SEED_LIMIT"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	// Items whose underwriter section is still empty. That is the signal the
	// item has not been seeded or reviewed; a reviewed item has content and
	// is left alone.
	rows, err := db.Query(`
		SELECT path, data->>'name', data->>'parent_name', COALESCE(data->>'summary',''),
		       COALESCE((SELECT string_agg(p->>'name'||': '||COALESCE(p->>'desc',''), E'\n')
		                 FROM jsonb_array_elements(data->'points') p), '')
		FROM twoai_pages
		WHERE data->>'shape' = 'coverage-item'
		  AND jsonb_array_length(COALESCE(data->'underwriter_wants_to_know','[]'::jsonb)) = 0
		ORDER BY (data->>'section_number')::int, (data->>'item_number')::int
		LIMIT $1`, limit)
	if err != nil {
		return err
	}
	type job struct{ path, name, parent, summary, evidence string }
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.path, &j.name, &j.parent, &j.summary, &j.evidence) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	if len(jobs) == 0 {
		fmt.Println("twoai_insurance_seed: nothing to seed; every coverage item already has an underwriter section")
		return nil
	}

	system := `You are drafting reference material for theworldofai.org, a sourced public reference on AI law, risk and infrastructure, for a section on how the insurance market prices AI and data centre risk.

You are given one specific exposure. Answer as an experienced placement broker would, filling a fixed template. Return ONLY a JSON object with exactly these keys and shapes, no prose, no markdown fences:

{
 "underwriter_wants_to_know": ["..."],
 "part_1_core_third_party_liability_exposures": ["..."],
 "part_2_paper_type_and_limit_structure": {"paper_basis":"...","cgl_primary":"...","excess_umbrella_towers":"..."},
 "part_3_high_value_red_flags": ["..."],
 "part_4_program_structure_and_limits": {"per_occurrence_primary_excess_layered_property":"...","property_eb_equipment":"...","bi_waiting_period":"...","business_interruption":"..."},
 "manuscript_wording_and_carve_backs": ["..."]
}

Rules. Each list holds 4 to 7 items, each one sentence, specific to THIS exposure and not generic. paper_basis names occurrence-based versus claims-made and says which applies here and why. Do not invent carrier names, ISO form numbers, or limit figures presented as fact; where a figure is genuinely market-standard you may state it as typical, and where it is not, say what determines it instead. Red flags are policy wordings, exclusions or sublimits that would defeat cover for this exposure. Manuscript items are the specific carve-backs or endorsements a broker would negotiate. Plain English, no hyphens in prose, use commas or periods instead.`

	seeded, failed := 0, 0
	for _, j := range jobs {
		user := fmt.Sprintf("SECTION: %s\nITEM: %s\nWHAT IT COVERS: %s\n\nEVIDENCE ALREADY ON THE PAGE:\n%s",
			j.parent, j.name, j.summary, nz(j.evidence, "(none yet)"))
		raw, model, err := twoaiGenerate("INSURANCE_SEED", system, user)
		if err != nil {
			fmt.Fprintf(os.Stderr, "twoai_insurance_seed: %s: %v\n", j.name, err)
			failed++
			continue
		}
		raw = strings.TrimSpace(raw)
		// The model is told no fences; strip them anyway, and take the first
		// object if there is prose around it.
		if i := strings.Index(raw, "{"); i >= 0 {
			if k := strings.LastIndex(raw, "}"); k > i {
				raw = raw[i : k+1]
			}
		}
		var s insSeed
		if err := json.Unmarshal([]byte(raw), &s); err != nil || len(s.UnderwriterWantsToKnow) == 0 {
			fmt.Fprintf(os.Stderr, "twoai_insurance_seed: %s: unparseable or empty: %s\n", j.name, truncate(raw, 160))
			failed++
			continue
		}
		needs := map[string]any{
			"part_1_core_third_party_liability_exposures": s.Part1,
			"part_2_paper_type_and_limit_structure":       s.Part2,
			"part_3_high_value_red_flags":                 s.Part3,
			"part_4_program_structure_and_limits":         s.Part4,
			"manuscript_wording_and_carve_backs":          s.Manuscript,
		}
		nb, _ := json.Marshal(needs)
		ub, _ := json.Marshal(s.UnderwriterWantsToKnow)
		if _, err := db.Exec(`UPDATE twoai_pages
			SET data = data || jsonb_build_object(
			      'underwriter_wants_to_know', $2::jsonb,
			      'insured_needs_secured', $3::jsonb,
			      'seeded_by', $4::text,
			      'seeded_on', $5::text,
			      'draft', true),
			    updated_at = now()
			WHERE path = $1`, j.path, string(ub), string(nb), model, time.Now().UTC().Format("2006-01-02")); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_insurance_seed: store %s: %v\n", j.name, err)
			failed++
			continue
		}
		seeded++
		time.Sleep(400 * time.Millisecond)
	}
	fmt.Printf("twoai_insurance_seed: seeded=%d failed=%d of %d items; every seeded item stays draft until reviewed\n", seeded, failed, len(jobs))
	return nil
}

func nz(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
