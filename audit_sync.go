package main

// audit_sync: feed the SRJ AI Audit Platform's synced_* tables directly from
// site_content, replacing the retired WordPress scrape.
//
// HISTORY, so the next reader knows why this exists. The platform's tool
// inventory, glossary, and governance library were originally harvested by a
// WordPress plugin (srj-audit-sync.php) that scraped three rendered pages on
// srjconsultingservices.com and POSTed them to the platform's content-sync
// endpoint. On 2026-08-20 the ai-tools page began 301-redirecting to
// theworldofai.org; the harvester followed the redirect, parsed a page it was
// never written for, and pushed 86 page headings as tools, deactivating all
// 634 real rows. WordPress is retired, so the scrape is gone for good and
// this stage is now the OWNER of the three synced_ tables: same database,
// no HTTP, no HTML parsing, reading the same site_content rows the site
// itself renders from.
//
// Semantics preserved from the endpoint it replaces:
//   - present in source  -> upsert, is_active=TRUE
//   - absent from source -> is_active=FALSE (soft delete; nothing deleted)
//   - churn guard: if applying a dataset would deactivate more than 40% of
//     its currently-active rows, that dataset is SKIPPED with a logged
//     reason. A real edit moves a handful of rows; only a broken read
//     replaces the majority at once.
//
// Sources:
//   tools    <- site_content resources/tools.json        (data->'tools')
//   glossary <- site_content resources/glossary.json     (data->'terms')
//   laws     <- site_content governance/*                (compliance library),
//               published at https://theworldofai.org/ai-compliance/<slug>/
//
// The platform's HTTP endpoint (core/content_sync.py) stays in place, HMAC-
// protected and unused; its own guards were hardened the same day this stage
// was written.

import (
	"github.com/lib/pq"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
)

func auditSyncChurnOK(db *sql.DB, table, nameCol string, names []string) (bool, string) {
	var active, wouldDrop int
	err := db.QueryRow(
		`SELECT count(*) FILTER (WHERE is_active),
		        count(*) FILTER (WHERE is_active AND NOT (`+nameCol+` = ANY($1)))
		 FROM `+table, pq.Array(names)).Scan(&active, &wouldDrop)
	if err != nil {
		return false, fmt.Sprintf("churn check failed: %v", err)
	}
	if active >= 20 && wouldDrop*100 > active*40 {
		return false, fmt.Sprintf(
			"would deactivate %d of %d active rows (>40%% ceiling)", wouldDrop, active)
	}
	return true, ""
}

func auditSync(db *sql.DB) error {
	force := os.Getenv("AUDIT_SYNC_FORCE") == "1"

	// ---- tools ------------------------------------------------------------
	var toolsJSON string
	if err := db.QueryRow(`SELECT data::text FROM site_content
		WHERE path='resources/tools.json'`).Scan(&toolsJSON); err != nil {
		return fmt.Errorf("tools source: %w", err)
	}
	var tw struct {
		Tools []struct {
			Name, Vendor, Category, Note, Jurisdiction string
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(toolsJSON), &tw); err != nil {
		return fmt.Errorf("tools parse: %w", err)
	}
	toolNames := make([]string, 0, len(tw.Tools))
	for _, t := range tw.Tools {
		if t.Name != "" {
			toolNames = append(toolNames, t.Name)
		}
	}
	if len(toolNames) < 50 {
		fmt.Printf("audit_sync: tools skipped (%d < 50 floor)\n", len(toolNames))
	} else if ok, why := auditSyncChurnOK(db, "synced_tools", "tool_name", toolNames); !ok && !force {
		fmt.Printf("audit_sync: tools SKIPPED, %s (AUDIT_SYNC_FORCE=1 overrides)\n", why)
	} else {
		for i, t := range tw.Tools {
			if t.Name == "" {
				continue
			}
			if _, err := db.Exec(`INSERT INTO synced_tools
				(tool_name, category, vendor, vendor_hq, governance_notes,
				 sort_order, is_active, synced_at)
				VALUES ($1,$2,$3,$4,$5,$6,TRUE,NOW())
				ON CONFLICT (tool_name) DO UPDATE SET
				  category=EXCLUDED.category, vendor=EXCLUDED.vendor,
				  vendor_hq=EXCLUDED.vendor_hq,
				  governance_notes=EXCLUDED.governance_notes,
				  sort_order=EXCLUDED.sort_order,
				  is_active=TRUE, synced_at=NOW()`,
				t.Name, t.Category, t.Vendor, t.Jurisdiction, t.Note, i); err != nil {
				return fmt.Errorf("tools upsert %q: %w", t.Name, err)
			}
		}
		res, _ := db.Exec(`UPDATE synced_tools SET is_active=FALSE
			WHERE is_active AND NOT (tool_name = ANY($1))`, pq.Array(toolNames))
		n, _ := res.RowsAffected()
		fmt.Printf("audit_sync: tools upserted=%d deactivated=%d\n", len(toolNames), n)
	}

	// ---- glossary ----------------------------------------------------------
	var glossJSON string
	if err := db.QueryRow(`SELECT data::text FROM site_content
		WHERE path='resources/glossary.json'`).Scan(&glossJSON); err != nil {
		return fmt.Errorf("glossary source: %w", err)
	}
	var gw struct {
		Terms []struct {
			Term, Definition, Example, Category, Slug string
		} `json:"terms"`
	}
	if err := json.Unmarshal([]byte(glossJSON), &gw); err != nil {
		return fmt.Errorf("glossary parse: %w", err)
	}
	termNames := make([]string, 0, len(gw.Terms))
	for _, t := range gw.Terms {
		if t.Term != "" {
			termNames = append(termNames, t.Term)
		}
	}
	if len(termNames) < 100 {
		fmt.Printf("audit_sync: glossary skipped (%d < 100 floor)\n", len(termNames))
	} else if ok, why := auditSyncChurnOK(db, "synced_glossary_terms", "term", termNames); !ok && !force {
		fmt.Printf("audit_sync: glossary SKIPPED, %s (AUDIT_SYNC_FORCE=1 overrides)\n", why)
	} else {
		for _, t := range gw.Terms {
			if t.Term == "" {
				continue
			}
			// SLUG IS CARRIED, NOT DERIVED. The audit platform asked for this
			// after testing the alternative: it derived slugs from the 552 term
			// names and diffed them against the sitemap. 529 matched, 23 did
			// not, and about ten of those no transform can ever produce -
			// "Autonomy Levels (Agentic AI)" publishes as agentic-autonomy-levels,
			// "NEYR (Net Efficiency Yield Ratio)" as neyr, "Faithfulness /
			// Groundedness" as faithfulness. A derived slug would 404 on those
			// and would drift silently as terms are added or renamed, which is
			// the same failure the /ai-governance/ flattening already caused
			// once. The source row has carried a slug all along; this stage
			// simply was not reading it.
			if _, err := db.Exec(`INSERT INTO synced_glossary_terms
				(term, definition, example, category, slug, is_active, synced_at)
				VALUES ($1,$2,$3,$4,$5,TRUE,NOW())
				ON CONFLICT (term) DO UPDATE SET
				  definition=EXCLUDED.definition, example=EXCLUDED.example,
				  category=EXCLUDED.category, slug=EXCLUDED.slug,
				  is_active=TRUE, synced_at=NOW()`,
				t.Term, t.Definition, t.Example, t.Category, t.Slug); err != nil {
				return fmt.Errorf("glossary upsert %q: %w", t.Term, err)
			}
		}
		res, _ := db.Exec(`UPDATE synced_glossary_terms SET is_active=FALSE
			WHERE is_active AND NOT (term = ANY($1))`, pq.Array(termNames))
		n, _ := res.RowsAffected()
		fmt.Printf("audit_sync: glossary upserted=%d deactivated=%d\n", len(termNames), n)
	}

	// ---- laws (compliance library) -----------------------------------------
	rows, err := db.Query(`SELECT data->>'slug', data->>'title'
		FROM site_content
		WHERE path LIKE 'governance/%'
		  AND path NOT IN ('governance/_meta.json','governance/sources.json','governance/ai-tools.json')
		  AND COALESCE(data->>'slug','') <> '' AND COALESCE(data->>'title','') <> ''
		ORDER BY data->>'title'`)
	if err != nil {
		return fmt.Errorf("laws source: %w", err)
	}
	type law struct{ slug, title string }
	var laws []law
	for rows.Next() {
		var l law
		if rows.Scan(&l.slug, &l.title) == nil {
			laws = append(laws, l)
		}
	}
	rows.Close()
	lawNames := make([]string, len(laws))
	for i, l := range laws {
		lawNames[i] = l.title
	}
	if len(laws) < 20 {
		fmt.Printf("audit_sync: laws skipped (%d < 20 floor)\n", len(laws))
	} else if ok, why := auditSyncChurnOK(db, "synced_laws", "law_name", lawNames); !ok && !force {
		fmt.Printf("audit_sync: laws SKIPPED, %s (AUDIT_SYNC_FORCE=1 overrides)\n", why)
	} else {
		for i, l := range laws {
			url := "https://theworldofai.org/ai-compliance/" + l.slug + "/"
			if _, err := db.Exec(`INSERT INTO synced_laws
				(law_name, category, url, sort_order, is_active, synced_at)
				VALUES ($1,$2,$3,$4,TRUE,NOW())
				ON CONFLICT (law_name) DO UPDATE SET
				  category=EXCLUDED.category, url=EXCLUDED.url,
				  sort_order=EXCLUDED.sort_order, is_active=TRUE, synced_at=NOW()`,
				l.title, "", url, i); err != nil {
				return fmt.Errorf("laws upsert %q: %w", l.title, err)
			}
		}
		res, _ := db.Exec(`UPDATE synced_laws SET is_active=FALSE
			WHERE is_active AND NOT (law_name = ANY($1))`, pq.Array(lawNames))
		n, _ := res.RowsAffected()
		fmt.Printf("audit_sync: laws upserted=%d deactivated=%d\n", len(laws), n)
	}

	return nil
}
