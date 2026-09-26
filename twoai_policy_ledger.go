package main

// twoai_policy_ledger: every AI law, rule and executive action this site
// tracks, in one dated table. Stephen, 2026-09-25: the AI Policy Ledger had
// been a placeholder on the Law and Compliance page since the coverage
// roadmap; this builds it.
//
// Rows come from two places. Every enacted state law already has a compliance
// page whose reading, made by the model from the statute's full text, carries
// who it applies to, its effective date and its penalties with section cites
// (twoai_enacted_laws). Those readings are the state rows, trimmed to a
// sentence each and linked to the page that holds the whole reading. The EU,
// China and US federal instruments are a fixed list below, each cited to its
// text and linked to the site's page on it; they change rarely and are edited
// here when they do. The page is rebuilt every run, so a newly enacted law
// appears the run after its compliance page is written.
//
// Nothing is inferred: a state row whose reading says a field is not stated
// in the text shows that, and a fixed row states only what its instrument
// says.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"sort"
	"strings"
	"time"
)

type ledgerRow struct {
	Jurisdiction, Instrument, Kind, Applies, Effective, Penalty, Href, Date string
}

// The fixed rows. Dates and penalties as the texts state them.
var twoaiLedgerFixed = []ledgerRow{
	{"European Union", "AI Act, Regulation (EU) 2024/1689, as amended by the Digital Omnibus, Regulation (EU) 2026/1744", "Regulation", "Providers and deployers of AI systems and general purpose models used in the EU, wherever established (Art. 2)", "In force 1 August 2024. Prohibitions 2 February 2025; general purpose model duties 2 August 2025; Article 50 transparency 2 August 2026; stand-alone high-risk duties 2 December 2027; Annex I product high-risk duties 2 August 2028", "EUR 35 million or 7 percent of worldwide turnover for prohibited practices; EUR 15 million or 3 percent for other breaches; EUR 7.5 million or 1 percent for incorrect information (Art. 99)", "/ai-compliance/eu-ai-act/", "2024-08-01"},
	{"European Union", "Product Liability Directive (EU) 2024/2853", "Directive", "Manufacturers and, in defined cases, providers and importers of software and AI systems", "Member states must transpose by 9 December 2026", "Liability for damage caused by a defective product; no fixed ceiling, damages as proved", "/ai-compliance/eu-product-liability/", "2024-12-09"},
	{"China", "Interim Measures for the Management of Generative AI Services", "Departmental rule (CAC and six other bodies)", "Providers of generative AI services offered to the public in China; internal development without public offering excluded (Art. 2)", "15 August 2023", "Rectification orders, suspension and removal of the service; monetary penalties under the Cybersecurity Law, Data Security Law and Personal Information Protection Law, the last up to RMB 50 million or 5 percent of prior year turnover", "/ai-compliance/china-ai-regulation/", "2023-08-15"},
	{"China", "Provisions on the Administration of Deep Synthesis Internet Information Services", "Departmental rule", "Providers and technical supporters of deep synthesis services, and their users", "10 January 2023", "Warnings, rectification orders, suspension; penalties under the underlying laws", "/ai-compliance/china-ai-regulation/", "2023-01-10"},
	{"China", "Provisions on the Administration of Algorithmic Recommendation", "Departmental rule", "Providers using algorithmic recommendation technology in internet information services", "1 March 2022; filing within ten working days for services with public opinion attributes", "Warnings, rectification orders, fines of RMB 10,000 to 100,000 where no other law applies", "/ai-compliance/china-cac-filings/", "2022-03-01"},
	{"China", "Measures for Labeling AI-Generated Synthetic Content", "Departmental rule", "Providers of services that generate synthetic content, and platforms that distribute it", "1 September 2025", "As under the generative AI and deep synthesis rules", "/ai-compliance/china-ai-regulation/", "2025-09-01"},
	{"China", "Interim Measures for the Administration of AI Anthropomorphic Interactive Services", "Departmental rule", "Providers of companion-style AI services", "15 July 2026", "As under the generative AI rules, including suspension and removal", "/ai-compliance/china-ai-regulation/", "2026-07-15"},
	{"United States, federal", "Executive Order 14179, Removing Barriers to American Leadership in Artificial Intelligence", "Executive order", "Federal agencies; revokes Executive Order 14110", "23 January 2025", "None; directs policy, no penalty", "/ai-compliance/federal-ai-legislation/", "2025-01-23"},
	{"United States, federal", "OMB Memorandum M-25-21, Accelerating Federal Use of AI", "Agency directive", "Federal agencies and their AI use, including procurement", "3 April 2025", "None; agency compliance requirement", "/ai-compliance/omb-m-25-21/", "2025-04-03"},
	{"United States, federal", "Executive order on a national policy framework for AI (state law challenge task force)", "Executive order", "Federal agencies; directs challenges to state AI laws", "11 December 2025", "None; directs litigation and funding conditions", "/ai-compliance/federal-ai-legislation/", "2025-12-11"},
	{"United States, New York City", "Local Law 144, automated employment decision tools", "Municipal law", "Employers and employment agencies using automated tools for hiring or promotion decisions for NYC positions", "5 July 2023", "Civil penalties of USD 500 for a first violation and USD 500 to 1,500 for each subsequent violation, each day a separate violation", "/ai-compliance/nyc-ll-144/", "2023-07-05"},
}

func twoaiPolicyLedger(db *sql.DB, today string) error {
	rows := append([]ledgerRow{}, twoaiLedgerFixed...)
	stateRows := 0
	q, err := db.Query(`SELECT data->>'state', data->>'bill_number', data->>'title', data->>'slug',
			COALESCE(data->'reading'->>'effective_date',''), COALESCE(data->'reading'->>'penalties_and_enforcement',''),
			COALESCE(data->'reading'->'who_it_applies_to', '[]'::jsonb)::text, COALESCE(data->>'status_date','')
		FROM twoai_pages WHERE path LIKE 'compliance/law-%' AND data->'reading' IS NOT NULL
		ORDER BY data->>'state', data->>'bill_number'`)
	if err != nil {
		return err
	}
	first := func(s string) string {
		s = strings.TrimSpace(s)
		if i := strings.IndexAny(s, ".;"); i > 40 && i < len(s)-1 {
			return s[:i+1]
		}
		return trunc(s, 220)
	}
	for q.Next() {
		var st, bill, title, slug, eff, pen, whoRaw, sdate string
		if q.Scan(&st, &bill, &title, &slug, &eff, &pen, &whoRaw, &sdate) != nil {
			continue
		}
		var who []string
		json.Unmarshal([]byte(whoRaw), &who)
		applies := ""
		if len(who) > 0 {
			applies = strings.Join(who[:min(2, len(who))], "; ")
			if len(who) > 2 {
				applies += fmt.Sprintf("; and %d more on the page", len(who)-2)
			}
		}
		rows = append(rows, ledgerRow{
			Jurisdiction: twoaiStateName(st), Instrument: bill + ", " + trunc(title, 120), Kind: "State statute",
			Applies: applies, Effective: first(eff), Penalty: first(pen), Href: "/ai-compliance/" + slug + "/", Date: sdate,
		})
		stateRows++
	}
	q.Close()
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Jurisdiction != rows[j].Jurisdiction {
			return rows[i].Jurisdiction < rows[j].Jurisdiction
		}
		return rows[i].Date > rows[j].Date
	})

	var b strings.Builder
	b.WriteString(`<nav class="srjgov-toc" aria-label="On this page"><p class="srjgov-toc-label">On this page</p><ul><li><a href="#how-to-read">How to read this table</a></li><li><a href="#ledger">The ledger</a></li><li><a href="#sources">Sources</a></li></ul></nav>`)
	fmt.Fprintf(&b, `<p>Every AI law, rule and executive action this site tracks, in one table: %d instruments, %d of them state statutes read from their enacted text, the rest the EU, China and US federal instruments cited to their text. Each row links to the site's page on that instrument, where the full reading and the text sit. Rebuilt on every run; a newly enacted state law appears here the run after its page is written.</p>`, len(rows), stateRows)
	b.WriteString(`<h2 id="how-to-read">How to read this table</h2><p>Applies to, effective date and penalty are as the text states them. For a state statute they are the first sentence of the reading on its page, which cites the section; where the reading says a thing is not stated in the text, that is what appears here. Penalty is the ceiling the text sets, not what any enforcer has imposed. A row sorted under a jurisdiction is that jurisdiction's own instrument; a federal law that applies to AI without naming it, such as the FTC Act or ECOA, is on the <a href="/ai-compliance/sector-rules/">sector rules</a> pages instead.</p>`)
	b.WriteString(`<h2 id="ledger">The ledger</h2><table class="srjgov-table"><thead><tr><th>Jurisdiction</th><th>Instrument</th><th>Applies to</th><th>Effective</th><th>Penalty ceiling as stated</th></tr></thead><tbody>`)
	for _, r := range rows {
		fmt.Fprintf(&b, `<tr><td>%s</td><td><a href="%s">%s</a><br><small>%s</small></td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			html.EscapeString(r.Jurisdiction), html.EscapeString(r.Href), html.EscapeString(r.Instrument), html.EscapeString(r.Kind),
			html.EscapeString(r.Applies), html.EscapeString(r.Effective), html.EscapeString(r.Penalty))
	}
	b.WriteString(`</tbody></table>`)
	b.WriteString(`<h2 id="sources">Sources</h2><p>State rows: the enacted text of each bill from LegiScan, read in full on the linked page, with section citations. EU rows: Regulation (EU) 2024/1689 and Regulation (EU) 2026/1744; Directive (EU) 2024/2853. China rows: the Cyberspace Administration of China's published measures, on the linked pages. US federal rows: the Federal Register and the White House texts, on the linked pages.</p>`)

	doc := map[string]any{
		"slug": "policy-ledger", "title": "AI Policy Ledger", "subtitle": "Every AI law, rule and executive action tracked, in one dated table",
		"parent":    "global-ai-laws",
		"short":     fmt.Sprintf("%d AI laws, rules and executive actions across the states, the federal government, the EU and China, each with who it applies to, its effective date and its penalty ceiling as the text states it.", len(rows)),
		"seo_title": "AI Policy Ledger: Every AI Law, Rule and Executive Action, Dated", "meta_description": fmt.Sprintf("A dated table of %d AI laws, rules and executive actions across US states, the federal government, the EU and China: who each applies to, when it takes effect, and its penalty ceiling as the text states it.", len(rows)),
		"focus_keyword": "AI policy ledger", "body_html": b.String(), "generated": today, "built_at": time.Now().Format(time.RFC3339),
		"uid": twoaiUID("section:policy-ledger"), "page_uid": twoaiUID("section:policy-ledger"), "verified": today, "refresh_every_days": 1,
		"citations": []map[string]string{{"author": "European Union", "journal": "Regulation (EU) 2024/1689, Article 99", "year": "2024", "quote": "Administrative fines of up to EUR 35 000 000 or 7 percent of total worldwide annual turnover for non-compliance with the prohibited practices."}},
	}
	raw, _ := json.Marshal(doc)
	if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, url_count, updated_at)
		VALUES ('compliance/policy-ledger.json', 'compliance', 'policy-ledger', $1::jsonb, 1, now())
		ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, taxonomy_slug = EXCLUDED.taxonomy_slug, updated_at = now()
		WHERE twoai_pages.data::text IS DISTINCT FROM EXCLUDED.data::text`, string(raw)); err != nil {
		return err
	}
	db.Exec(`UPDATE twoai_taxonomy SET status = 'live', live_path = '/ai-compliance/policy-ledger/' WHERE slug = 'policy-ledger'`)
	fmt.Printf("twoai_policy_ledger: rows=%d state=%d fixed=%d ok=true\n", len(rows), stateRows, len(twoaiLedgerFixed))
	return nil
}

// twoaiStateName maps a two letter code to the state name for the ledger.
func twoaiStateName(code string) string {
	names := map[string]string{"AL": "Alabama", "AK": "Alaska", "AZ": "Arizona", "AR": "Arkansas", "CA": "California", "CO": "Colorado", "CT": "Connecticut", "DE": "Delaware", "DC": "District of Columbia", "FL": "Florida", "GA": "Georgia", "HI": "Hawaii", "ID": "Idaho", "IL": "Illinois", "IN": "Indiana", "IA": "Iowa", "KS": "Kansas", "KY": "Kentucky", "LA": "Louisiana", "ME": "Maine", "MD": "Maryland", "MA": "Massachusetts", "MI": "Michigan", "MN": "Minnesota", "MS": "Mississippi", "MO": "Missouri", "MT": "Montana", "NE": "Nebraska", "NV": "Nevada", "NH": "New Hampshire", "NJ": "New Jersey", "NM": "New Mexico", "NY": "New York", "NC": "North Carolina", "ND": "North Dakota", "OH": "Ohio", "OK": "Oklahoma", "OR": "Oregon", "PA": "Pennsylvania", "PR": "Puerto Rico", "RI": "Rhode Island", "SC": "South Carolina", "SD": "South Dakota", "TN": "Tennessee", "TX": "Texas", "UT": "Utah", "VT": "Vermont", "VA": "Virginia", "WA": "Washington", "WV": "West Virginia", "WI": "Wisconsin", "WY": "Wyoming", "US": "United States, federal"}
	if n, ok := names[strings.ToUpper(code)]; ok {
		return "United States, " + n
	}
	return code
}
