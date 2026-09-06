package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// THE PAGE THAT DOES NOT EXIST IS THE THINNEST PAGE OF ALL.
//
// On 2026-09-06 Stephen asked why the richest man in the world had no page on
// an AI site. Musk was absent, and so were Nadella, Pichai, Zuckerberg, Jassy,
// Murati, Liang Wenfeng and every regulator: 29 of 34 obvious names. The cause
// was structural. Both hundred-name lists the directory was built from were
// compiled by contribution to the TECHNOLOGY - who invented the transformer,
// who wrote backpropagation - so the people who decide what gets built, funded
// and regulated were never candidates in the first place.
//
// The thin-page scraper could not have caught it. It measures pages that
// EXIST and are underfilled. An entity with no page at all is invisible to
// every test it runs, which is why the gap survived two hundred additions and
// needed a human to notice.
//
// This stage closes that. It reads what the site's own content already names -
// news stories, company leadership rosters, lawsuit parties, facility
// operators - and reports the names that appear repeatedly with no page behind
// them. A name the site keeps mentioning and cannot link is a missing page,
// and it is now a queue rather than a discovery.
//
// WHAT IT DOES NOT DO. It does not create entities. Minting a uid for every
// capitalised string in a GDELT feed would fill twoai_entities with fragments,
// and the fabrication rule cuts against exactly that. It produces a ranked
// worklist with the evidence for each name - how many stories, which
// registries reference it - for a person to accept or decline. Declining is a
// real outcome and is recorded, so the same name is not proposed every day.

func twoaiThinMissingEntities(db *sql.DB) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_missing_entities (
		normalized   text PRIMARY KEY,
		name         text NOT NULL,
		kind         text NOT NULL,
		mentions     int  NOT NULL DEFAULT 0,
		sources      text NOT NULL,
		first_seen   timestamptz NOT NULL DEFAULT now(),
		last_seen    timestamptz NOT NULL DEFAULT now(),
		status       text NOT NULL DEFAULT 'proposed',
		decided_on   date,
		decided_note text)`); err != nil {
		fmt.Println("twoai_thin_missing:", err)
		return
	}

	// PEOPLE AND ORGANISATIONS THE NEWS KEEPS NAMING. GDELT's extractor tags
	// each story with the people and organisations it mentions. Those tags are
	// noisy - fragments, job titles, generic phrases - so the bar is
	// deliberately high: named in at least three separate stories, at least
	// two words, and not already an entity under any alias. A name that
	// clears that has been in the news three times and still has nowhere to
	// link, which is the definition of a page this site should have.
	if _, err := db.Exec(`
		INSERT INTO twoai_missing_entities (normalized, name, kind, mentions, sources, last_seen)
		SELECT norm, name, kind, n, 'news:' || n || ' stories', now()
		FROM (
			SELECT lower(btrim(x.v))                                   AS norm,
			       btrim(x.v)                                          AS name,
			       CASE WHEN k = 'Persons' THEN 'person' ELSE 'company' END AS kind,
			       count(DISTINCT s.value->>'Slug')                     AS n
			FROM twoai_pages p,
			     jsonb_array_elements(p.data->'stories') s,
			     LATERAL (VALUES ('Persons'), ('Orgs')) AS t(k),
			     jsonb_array_elements_text(COALESCE(s.value->t.k, '[]'::jsonb)) AS x(v)
			WHERE p.path = 'news/archive.json'
			  AND length(btrim(x.v)) BETWEEN 6 AND 60
			  AND btrim(x.v) ~ '^[A-Z][a-zA-Z.''-]+( [A-Z][a-zA-Z.''&-]+){1,3}$'
			GROUP BY 1,2,3
			HAVING count(DISTINCT s.value->>'Slug') >= 3
		) c
		WHERE NOT EXISTS (
			SELECT 1 FROM twoai_entities e
			WHERE e.normalized = regexp_replace(regexp_replace(c.norm,'[^a-z0-9]+','-','g'),'^-|-$','','g')
			   OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(e.aliases) a WHERE lower(a) = c.norm))
		  AND NOT EXISTS (SELECT 1 FROM twoai_company_profiles cp WHERE lower(cp.name) = c.norm)
		  AND NOT EXISTS (SELECT 1 FROM twoai_people_worklist w WHERE lower(w.name) = c.norm)
		ON CONFLICT (normalized) DO UPDATE
		  SET mentions = EXCLUDED.mentions, sources = EXCLUDED.sources, last_seen = now()
		  WHERE twoai_missing_entities.status = 'proposed'`); err != nil {
		fmt.Println("twoai_thin_missing news:", err)
	}

	// EXECUTIVES THE REGISTRY ALREADY RECORDS BUT CANNOT LINK. Every company
	// profile carries a leadership array with names in it. Where that person
	// has no entity, the company page prints a name the reader cannot follow,
	// and the graph cannot draw executive_of. This is the highest-confidence
	// source in the stage: the name is already in our own structured data,
	// with a role and usually a source URL beside it.
	if _, err := db.Exec(`
		INSERT INTO twoai_missing_entities (normalized, name, kind, mentions, sources, last_seen)
		SELECT norm, name, 'person', n, 'leadership of ' || firms, now()
		FROM (
			SELECT lower(btrim(l->>'name')) AS norm, btrim(l->>'name') AS name,
			       count(*) AS n, string_agg(DISTINCT c.name, ', ') AS firms
			FROM twoai_company_profiles c, jsonb_array_elements(c.leadership) l
			WHERE COALESCE(l->>'name','') <> ''
			GROUP BY 1,2
		) c
		WHERE NOT EXISTS (
			SELECT 1 FROM twoai_entities e
			WHERE e.kind = 'person'
			  AND e.normalized = regexp_replace(regexp_replace(c.norm,'[^a-z0-9]+','-','g'),'^-|-$','','g'))
		  AND NOT EXISTS (SELECT 1 FROM twoai_people_worklist w WHERE lower(w.name) = c.norm)
		ON CONFLICT (normalized) DO UPDATE
		  SET mentions = EXCLUDED.mentions, sources = EXCLUDED.sources, last_seen = now()
		  WHERE twoai_missing_entities.status = 'proposed'`); err != nil {
		fmt.Println("twoai_thin_missing leadership:", err)
	}

	// OPERATORS RUNNING FACILITIES WITH NO COMPANY BEHIND THEM. The facility
	// registry names an operator on every row. Where that operator has no
	// company profile, the site knows a company runs 40 buildings and can say
	// nothing about the company - which is how xAI and Tesla ended up in the
	// registry with no owner attached.
	if _, err := db.Exec(`
		INSERT INTO twoai_missing_entities (normalized, name, kind, mentions, sources, last_seen)
		SELECT lower(o.name), o.name, 'company', o.facility_count,
		       'operates ' || o.facility_count || ' facilities in the registry', now()
		FROM twoai_dc_operators o
		WHERE o.retired_at IS NULL AND o.company_uid IS NULL AND o.facility_count >= 3
		  AND NOT EXISTS (SELECT 1 FROM twoai_company_profiles c WHERE lower(c.name) = lower(o.name))
		ON CONFLICT (normalized) DO UPDATE
		  SET mentions = EXCLUDED.mentions, sources = EXCLUDED.sources, last_seen = now()
		  WHERE twoai_missing_entities.status = 'proposed'`); err != nil {
		fmt.Println("twoai_thin_missing operators:", err)
	}

	// COMPANIES BEING SUED, WITH NO PAGE. A defendant in an AI lawsuit that
	// the site tracks is, by the site's own editorial judgement, a company
	// worth knowing about.
	if _, err := db.Exec(`
		INSERT INTO twoai_missing_entities (normalized, name, kind, mentions, sources, last_seen)
		SELECT lower(btrim(d.party)), btrim(d.party), 'company', count(*),
		       'defendant in ' || count(*) || ' tracked lawsuits', now()
		FROM (
			SELECT unnest(string_to_array(defendants, ',')) AS party
			FROM ai_lawsuits WHERE is_active AND COALESCE(defendants,'') <> ''
		) d
		WHERE length(btrim(d.party)) BETWEEN 4 AND 60
		  AND btrim(d.party) ~ '^[A-Z]'
		GROUP BY 1,2
		HAVING count(*) >= 2
		   AND NOT EXISTS (SELECT 1 FROM twoai_company_profiles c WHERE lower(c.name) = lower(btrim(d.party)))
		   AND NOT EXISTS (SELECT 1 FROM twoai_entities e WHERE e.kind='company'
		        AND e.normalized = regexp_replace(regexp_replace(lower(btrim(d.party)),'[^a-z0-9]+','-','g'),'^-|-$','','g'))
		ON CONFLICT (normalized) DO UPDATE
		  SET mentions = EXCLUDED.mentions, sources = EXCLUDED.sources, last_seen = now()
		  WHERE twoai_missing_entities.status = 'proposed'`); err != nil {
		fmt.Println("twoai_thin_missing lawsuits:", err)
	}

	// A name proposed and then written stops being missing. This closes the
	// loop without a person having to mark anything: once the entity exists,
	// the row records that and drops out of the worklist.
	db.Exec(`UPDATE twoai_missing_entities m SET status = 'created', decided_on = current_date,
		decided_note = 'entity exists'
		WHERE m.status = 'proposed' AND EXISTS (
			SELECT 1 FROM twoai_entities e
			WHERE e.normalized = regexp_replace(regexp_replace(m.normalized,'[^a-z0-9]+','-','g'),'^-|-$','','g'))`)

	var proposed, created, declined int
	var top string
	db.QueryRow(`SELECT
		count(*) FILTER (WHERE status='proposed'),
		count(*) FILTER (WHERE status='created'),
		count(*) FILTER (WHERE status='declined'),
		COALESCE(string_agg(name || ' (' || mentions || ')', ', ' ORDER BY mentions DESC)
		         FILTER (WHERE status='proposed'), '')
		FROM twoai_missing_entities`).Scan(&proposed, &created, &declined, &top)
	if len(top) > 220 {
		top = top[:220] + "..."
	}
	fmt.Printf("twoai_thin_missing: proposed=%d created=%d declined=%d\n", proposed, created, declined)
	if proposed > 0 {
		fmt.Printf("twoai_thin_missing: most mentioned with no page: %s\n", strings.TrimSpace(top))
	}
}
