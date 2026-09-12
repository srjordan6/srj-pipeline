package main

// twoai_llm_compare: put two models on the same task and store both answers
// next to each other, so a person can read them before either is trusted.
//
// Stephen took the Ollama Pro plan on 2026-09-12. What the plan buys is the
// larger cloud models, and what those are FOR is the judgment jobs - company
// profiles, industry analysis, page readings - that carry most of the
// Anthropic bill and that a 12GB card cannot run. Whether a 70B or 235B
// open model can be trusted with them is an empirical question, and this is
// the experiment.
//
// It deliberately does not decide anything. It picks N company profiles that
// Claude has already written, runs the same prompt and the same source text
// through the candidate model, and writes the pair to twoai_llm_compare.
// The judgment is read off a screen by a person: does the local answer name
// anything the source does not? A fabricated insight reads exactly like a
// real one, and no automated score catches that reliably.
//
//	pipeline twoai_llm_compare company_profiles qwen3:235b 20
//
// Then:
//
//	SELECT a_model, b_model, left(a_out,300), left(b_out,300)
//	FROM twoai_llm_compare WHERE job='company_profiles' ORDER BY random() LIMIT 5;

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func twoaiLLMCompare(db *sql.DB, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: pipeline twoai_llm_compare <job> <ollama-model> [n]\n  jobs: company_profiles, point_briefs")
	}
	job, model := args[0], args[1]
	n := 20
	if len(args) > 2 {
		if v, err := strconv.Atoi(args[2]); err == nil && v > 0 {
			n = v
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_llm_compare (
		id serial PRIMARY KEY,
		job text NOT NULL, key text NOT NULL,
		a_model text, a_out text,
		b_model text, b_out text, b_ms int,
		b_validation text,
		created_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	type task struct{ key, system, user, payload, aModel, aOut string }
	var tasks []task

	switch job {
	case "company_profiles":
		// Existing Claude-written profiles, rebuilt from the same harvest and
		// held facts through the same payload builder the harvester uses, so
		// the only variable is the model.
		rows, err := db.Query(`
			SELECT a.metric, a.model, a.body, p.uid, p.name
			FROM twoai_industry_analysis a
			JOIN twoai_company_profiles p ON a.metric = 'company-' || p.uid
			WHERE a.metric LIKE 'company-%' AND a.body <> ''
			ORDER BY random() LIMIT $1`, n*2)
		if err != nil {
			return err
		}
		for rows.Next() && len(tasks) < n {
			var key, am, ao, uid, name string
			if rows.Scan(&key, &am, &ao, &uid, &name) != nil {
				continue
			}
			payload, ok := twoaiCompanyProfilePayload(db, uid, name)
			if !ok {
				continue
			}
			tasks = append(tasks, task{key: key, aModel: am, aOut: ao, payload: payload,
				system: twoaiCompanyProfileSystem,
				user:   "Company: " + name + "\nThe data:\n" + payload + "\n\nWrite the profile now."})
		}
		rows.Close()
	case "point_briefs":
		rows, err := db.Query(`
			SELECT b.url, b.model, b.brief, h.extract, h.source_name
			FROM twoai_point_briefs b JOIN twoai_source_harvest h ON h.url=b.url
			WHERE b.brief IS NOT NULL AND b.brief <> '' AND h.extract <> ''
			ORDER BY random() LIMIT $1`, n)
		if err != nil {
			return err
		}
		for rows.Next() {
			var key, am, ao, extract, name string
			if rows.Scan(&key, &am, &ao, &extract, &name) == nil {
				tasks = append(tasks, task{key: key, aModel: am, aOut: ao,
					system: pointBriefSystem,
					user:   "Source: " + key + "\nPublisher: " + name + "\n\nText harvested from the page:\n\n" + extract})
			}
		}
		rows.Close()
	default:
		return fmt.Errorf("unknown job %q", job)
	}
	if len(tasks) == 0 {
		return fmt.Errorf("no existing %s outputs with source text to compare against", job)
	}

	done, failed, rejected := 0, 0, 0
	for _, t := range tasks {
		start := time.Now()
		out, err := twoaiOllamaCall(model, t.system, t.user)
		ms := int(time.Since(start).Milliseconds())
		if err != nil {
			fmt.Fprintln(os.Stderr, "twoai_llm_compare:", t.key, err)
			failed++
			continue
		}
		out = strings.TrimSpace(out)
		// The same gate the harvester applies to Claude: every percentage and
		// dollar figure in the output must appear verbatim in the input. This
		// is the one automated signal that catches an invented number, and a
		// candidate that fails it is out before anyone reads its prose.
		validation := "ok"
		if t.payload != "" {
			if verr := twoaiValidateAnalysis(t.payload, out); verr != nil {
				validation = "REJECTED: " + verr.Error()
				rejected++
			}
		}
		db.Exec(`INSERT INTO twoai_llm_compare (job, key, a_model, a_out, b_model, b_out, b_ms, b_validation)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, job, t.key, t.aModel, t.aOut, "ollama:"+model, out, ms, validation)
		done++
	}
	fmt.Printf("twoai_llm_compare: job=%s model=%s pairs=%d failed=%d fabricated_figures=%d | read them: SELECT b_validation, left(a_out,400) AS claude, left(b_out,400) AS candidate FROM twoai_llm_compare WHERE job='%s' AND b_model='ollama:%s' ORDER BY random() LIMIT 5\n",
		job, model, done, failed, rejected, job, model)
	return nil
}
