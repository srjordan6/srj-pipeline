package main

// The Lp(a) drug table, theworldofai row 484 (Stephen, 2026-10-05).
//
// One row per drug in twoai_lpa_drugs, written by theworldofai and shown on
// Lp(a) Clinical Trials (lpa-trials). The status label is one of five fixed
// words that never imply an effect: Approved, Phase 3, Phase 2, Early
// research, Outcome failed (a CHECK constraint holds the list). Only rows with
// status 'live' publish.

import (
	"database/sql"
	"fmt"
)

func lpaLoadDrugs(db *sql.DB) []map[string]string {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_lpa_drugs (slug text PRIMARY KEY, drug text NOT NULL, company text, technology text,
		route text, frequency text, phase text, lpa_reduction text, reduction_source_url text, outcome_trial text,
		outcome_trial_url text, outcome_status text, aortic_stenosis_status text, fda_status text,
		status_label text CHECK (status_label IN ('Approved','Phase 3','Phase 2','Early research','Outcome failed')),
		sort int DEFAULT 100, status text NOT NULL DEFAULT 'draft', last_reviewed date,
		created_at timestamptz DEFAULT now(), updated_at timestamptz DEFAULT now())`)
	rows, err := db.Query(`SELECT drug, COALESCE(company,''), COALESCE(technology,''), COALESCE(route,''), COALESCE(frequency,''),
			COALESCE(status_label,''), COALESCE(lpa_reduction,''), COALESCE(reduction_source_url,''), COALESCE(outcome_trial,''),
			COALESCE(outcome_trial_url,''), COALESCE(outcome_status,''), COALESCE(aortic_stenosis_status,''),
			COALESCE(fda_status,''), COALESCE(last_reviewed::text,'')
		FROM twoai_lpa_drugs WHERE status = 'live' ORDER BY sort, lower(drug)`)
	if err != nil {
		fmt.Println("twoai_lpa_drugs:", err)
		return nil
	}
	defer rows.Close()
	keys := []string{"drug", "company", "technology", "route", "frequency", "status_label", "reduction", "reduction_source",
		"outcome_trial", "outcome_trial_url", "outcome_status", "aortic", "fda_status", "last_reviewed"}
	var out []map[string]string
	for rows.Next() {
		v := make([]string, len(keys))
		ptrs := make([]any, len(keys))
		for i := range v {
			ptrs[i] = &v[i]
		}
		if rows.Scan(ptrs...) != nil {
			continue
		}
		m := map[string]string{}
		for i, k := range keys {
			if v[i] != "" {
				m[k] = v[i]
			}
		}
		out = append(out, m)
	}
	return out
}
