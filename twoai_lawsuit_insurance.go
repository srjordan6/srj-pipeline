package main

// HOW INSURANCE WOULD RESPOND, per lawsuit. Stephen, 2026-09-21: does the
// tracker show insurers paying the defense or backing plaintiffs? It does not,
// and it cannot: a defendant's policies go to the other side in discovery,
// not onto the public docket, and litigation funding is disclosed only in the
// few courts that require it. So each case carries instead how coverage
// typically works for its kind of claim.
//
// One text per category in twoai_lawsuit_insurance, drafted once and reviewed
// by Stephen, who works in insurance. Only rows he sets to 'published' are
// attached to a case; a draft is never shown. A case with no category (still
// unclassified) gets no section rather than a guess.

import (
	"database/sql"
	"encoding/json"
)

func twoaiLawsuitInsurance(db *sql.DB) map[string]map[string]any {
	out := map[string]map[string]any{}
	rows, err := db.Query(`SELECT category, heading, policies, body::text, COALESCE(hub_name,''), COALESCE(hub_href,''),
			COALESCE(to_char(reviewed_on,'YYYY-MM-DD'),'')
		FROM twoai_lawsuit_insurance WHERE status = 'published'`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var cat, heading, policies, body, hubName, hubHref, reviewed string
		if rows.Scan(&cat, &heading, &policies, &body, &hubName, &hubHref, &reviewed) != nil {
			continue
		}
		var paras []string
		if json.Unmarshal([]byte(body), &paras) != nil || len(paras) == 0 {
			continue
		}
		out[cat] = map[string]any{"heading": heading, "policies": policies, "body": paras,
			"hub_name": hubName, "hub_href": hubHref, "reviewed_on": reviewed}
	}
	return out
}
