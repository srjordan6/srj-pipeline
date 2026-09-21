package main

// Lobbying clients to company pages, both directions. Stephen, 2026-09-21:
// if AI lobbying filings name companies, they belong on /companies/, and the
// pages must cross reference one another. Lobbying firms get no pages;
// individual lobbyists are named on the filing entries and linked to a person
// page only when Stephen confirms the identity.
//
// MATCHING IS PROPOSED, THEN CONFIRMED. An LDA client record carries a name
// and an LDA id, nothing else: no domain, no CIK. The site's rule is never to
// trust a name match, so polProposeClientMatches only proposes: a client whose
// normalised name equals the normalised name or alias of exactly one company
// that has a page. "GOOGLE CLIENT SERVICES LLC" normalises to "google" and is
// proposed as Google; Stephen confirms in SQL. Two companies normalising to
// the same key are ambiguous and proposed to neither.
//
// THE PATCH. twoaiPoliticsCompanyPatch runs at the end of twoaiBuild, after
// the company documents are written, and adds a top-level `lobbying` key to
// each confirmed company's document: the filings as filed, newest first, with
// their lda.gov links, and a link back to the Lobbying on AI section. The
// company template renders it, and counts it as substance for the thin-page
// gate. Rebuilt every run from SQL.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/lib/pq"
)

var polSuffixRe = regexp.MustCompile(`\b(the|inc|incorporated|llc|l l c|corp|corporation|co|company|ltd|limited|plc|pbc|lp|llp|holdings|group|usa|north america|client services|services|technologies|technology)\b`)
var polNonAlnum = regexp.MustCompile(`[^a-z0-9 ]+`)
var polSpaces = regexp.MustCompile(`\s+`)
var polDBARe = regexp.MustCompile(`(?i)\b(?:d/?b/?a|doing business as|f/?k/?a|formerly)\b\.?\s*(.+)$`)

func polNormOrg(s string) string {
	s = strings.ToLower(ldaClean(s))
	// "MAPLEBEAR INC. D/B/A INSTACART" is Instacart.
	if m := polDBARe.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	s = strings.ReplaceAll(s, "&", " and ")
	s = polNonAlnum.ReplaceAllString(s, " ")
	for i := 0; i < 3; i++ {
		s = polSuffixRe.ReplaceAllString(s, " ")
	}
	return strings.TrimSpace(polSpaces.ReplaceAllString(s, " "))
}

func polProposeClientMatches(db *sql.DB) (int, error) {
	// Two kinds of target: a company with a /companies/ page, and a data center
	// operator with a registry page (tech/dc-op-<uid>.json), 2026-09-21. The
	// same client can be proposed for both, since CoreWeave is both.
	kinds := []struct{ kind, sql string }{
		{"company", `SELECT e.uid, e.name, COALESCE(e.aliases, '[]'::jsonb) FROM twoai_entities e
			WHERE e.kind = 'company' AND EXISTS (SELECT 1 FROM twoai_pages p WHERE p.path = 'companies/' || e.uid || '.json')`},
		{"dc_operator", `SELECT e.uid, e.name, COALESCE(e.aliases, '[]'::jsonb) FROM twoai_entities e
			WHERE e.kind = 'dc_operator' AND EXISTS (SELECT 1 FROM twoai_pages p WHERE p.path = 'tech/dc-op-' || e.uid || '.json')`},
	}
	total := 0
	for _, k := range kinds {
		n, err := polProposeKind(db, k.kind, k.sql)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func polProposeKind(db *sql.DB, kind, entitySQL string) (int, error) {
	rows, err := db.Query(entitySQL)
	if err != nil {
		return 0, err
	}
	byKey := map[string]string{}
	ambiguous := map[string]bool{}
	for rows.Next() {
		var uid, name string
		var aliasJSON []byte
		if rows.Scan(&uid, &name, &aliasJSON) != nil {
			continue
		}
		var aliases []string
		_ = json.Unmarshal(aliasJSON, &aliases)
		for _, n := range append([]string{name}, aliases...) {
			k := polNormOrg(n)
			if len(k) < 3 {
				continue
			}
			if prev, ok := byKey[k]; ok && prev != uid {
				ambiguous[k] = true
				continue
			}
			byKey[k] = uid
		}
	}
	rows.Close()

	cr, err := db.Query(`SELECT client_id, (array_agg(client_name ORDER BY dt_posted DESC))[1]
		FROM twoai_pol_lobbying l
		WHERE retired_reason IS NULL
		  AND NOT EXISTS (SELECT 1 FROM twoai_pol_client_matches m WHERE m.client_id = l.client_id AND m.target_kind = $1)
		GROUP BY client_id`, kind)
	if err != nil {
		return 0, err
	}
	type cand struct {
		id        int64
		name, uid string
	}
	var props []cand
	for cr.Next() {
		var id int64
		var name string
		if cr.Scan(&id, &name) != nil {
			continue
		}
		k := polNormOrg(name)
		if uid, ok := byKey[k]; ok && !ambiguous[k] {
			props = append(props, cand{id, name, uid})
		}
	}
	cr.Close()
	n := 0
	for _, p := range props {
		if _, err := db.Exec(`INSERT INTO twoai_pol_client_matches (client_id, client_name, company_uid, basis, status, target_kind)
			VALUES ($1,$2,$3,'normalised name equals name or alias','proposed',$4) ON CONFLICT (client_id, target_kind) DO NOTHING`,
			p.id, p.name, p.uid, kind); err == nil {
			n++
		}
	}
	return n, nil
}

type polLobbyist struct {
	Name string `json:"name"`
	Href string `json:"href,omitempty"`
}

type polCompanyFiling struct {
	UID            string        `json:"uid"`
	URL            string        `json:"url"`
	Period         string        `json:"period"`
	Posted         string        `json:"posted"`
	Registrant     string        `json:"registrant"`
	RegistrantHref string        `json:"registrant_href,omitempty"`
	Bills          []string      `json:"bills"`
	Lobbyists      []polLobbyist `json:"lobbyists"`
}

func polTitle(s string) string {
	w := strings.Fields(strings.ToLower(s))
	for i, x := range w {
		if len(x) > 0 {
			w[i] = strings.ToUpper(x[:1]) + x[1:]
		}
	}
	return strings.Join(w, " ")
}

func twoaiPoliticsCompanyPatch(db *sql.DB) error {
	// Lobbyist ids Stephen has confirmed as a person this site profiles.
	people := map[string]string{}
	if r, err := db.Query(`SELECT lobbyist_id::text, person_href FROM twoai_pol_lobbyist_people WHERE status = 'confirmed'`); err == nil {
		for r.Next() {
			var id, href string
			if r.Scan(&id, &href) == nil {
				people[id] = href
			}
		}
		r.Close()
	}

	rows, err := db.Query(`SELECT company_uid, target_kind, array_agg(client_id) FROM twoai_pol_client_matches
		WHERE status = 'confirmed' GROUP BY company_uid, target_kind`)
	if err != nil {
		return err
	}
	type co struct {
		uid, kind string
		clients   []int64
	}
	var cos []co
	for rows.Next() {
		var c co
		if rows.Scan(&c.uid, &c.kind, pq.Array(&c.clients)) == nil {
			cos = append(cos, c)
		}
	}
	rows.Close()

	patched := 0
	for _, c := range cos {
		fr, err := db.Query(`SELECT uid, document_url,
				CASE filing_period WHEN 'first_quarter' THEN 'Q1' WHEN 'second_quarter' THEN 'Q2'
					WHEN 'third_quarter' THEN 'Q3' WHEN 'fourth_quarter' THEN 'Q4'
					WHEN 'mid_year' THEN 'Mid-year' WHEN 'year_end' THEN 'Year-end'
					ELSE COALESCE(filing_type_display, 'LDA') END || ' ' || COALESCE(filing_year::text, ''),
				dt_posted, COALESCE(registrant_name, ''), bill_refs, raw, COALESCE(registrant_id, 0)
			FROM twoai_pol_lobbying l
			WHERE client_id = ANY($1) AND retired_reason IS NULL AND dt_posted IS NOT NULL
			ORDER BY dt_posted DESC`, pq.Array(c.clients))
		if err != nil {
			continue
		}
		var filings []polCompanyFiling
		total := 0
		latest := ""
		for fr.Next() {
			var f polCompanyFiling
			var posted sql.NullTime
			var bills []string
			var raw []byte
			var regID int64
			if fr.Scan(&f.UID, &f.URL, &f.Period, &posted, &f.Registrant, pq.Array(&bills), &raw, &regID) != nil {
				continue
			}
			if regID > 0 {
				// Every firm with an AI filing has a page, keyed on its LDA id.
				f.RegistrantHref = polBase + polUID("lda-firm", fmt.Sprint(regID)) + "/"
			}
			total++
			if latest == "" && posted.Valid {
				latest = posted.Time.UTC().Format("2006-01-02")
			}
			if len(filings) >= 25 {
				continue
			}
			f.Posted = posted.Time.UTC().Format("2006-01-02 15:04 UTC")
			f.Bills = bills
			if len(f.Bills) > 8 {
				f.Bills = f.Bills[:8]
			}
			f.Lobbyists = []polLobbyist{}
			seen := map[string]bool{}
			var rawAct struct {
				Activities []struct {
					Description string `json:"description"`
					Lobbyists   []struct {
						Lobbyist struct {
							ID        json.Number `json:"id"`
							FirstName string `json:"first_name"`
							LastName  string `json:"last_name"`
						} `json:"lobbyist"`
					} `json:"lobbyists"`
				} `json:"lobbying_activities"`
			}
			if json.Unmarshal(raw, &rawAct) == nil {
				for _, a := range rawAct.Activities {
					if !ldaKeep(a.Description) {
						continue
					}
					for _, l := range a.Lobbyists {
						id := fmt.Sprint(l.Lobbyist.ID)
						name := polTitle(strings.TrimSpace(l.Lobbyist.FirstName + " " + l.Lobbyist.LastName))
						if name == "" || seen[id] {
							continue
						}
						seen[id] = true
						// A confirmed AI People profile wins; otherwise the lobbyist's
						// own page, keyed on the LDA lobbyist id.
						href := people[id]
						if href == "" {
							href = polBase + polUID("lda-lobbyist", id) + "/"
						}
						f.Lobbyists = append(f.Lobbyists, polLobbyist{Name: name, Href: href})
					}
				}
			}
			filings = append(filings, f)
		}
		fr.Close()
		if len(filings) == 0 {
			continue
		}
		lob := map[string]any{
			"total": total, "latest": latest, "filings": filings, "client_ids": c.clients,
			"section_href": polBase + polLobbyUID + "/",
		}
		b, _ := json.Marshal(lob)
		path := "companies/" + c.uid + ".json"
		if c.kind == "dc_operator" {
			path = "tech/dc-op-" + c.uid + ".json"
		}
		res, err := db.Exec(`UPDATE twoai_pages SET data = data || jsonb_build_object('lobbying', $2::jsonb), updated_at = now()
			WHERE path = $1`, path, string(ldaRaw(b)))
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				patched++
			}
		}
	}
	var proposed int
	db.QueryRow(`SELECT count(*) FROM twoai_pol_client_matches WHERE status = 'proposed'`).Scan(&proposed)
	fmt.Printf("twoai_politics_companies: confirmed_companies=%d pages_patched=%d proposed_awaiting=%d\n", len(cos), patched, proposed)
	return nil
}
