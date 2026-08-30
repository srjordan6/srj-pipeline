package main

// MITRE ATLAS watch: the adversarial threat landscape for AI systems.
//
// ATLAS is the ATT&CK-style knowledge base of how AI systems are actually
// attacked: tactics, techniques, mitigations, and documented case studies.
// It is the reference the AI Security and Risk section should track, and it
// moves on a monthly release cadence rather than continuously.
//
// Monitoring design. atlas.mitre.org is a client-rendered application with
// no data endpoint, so the machine-readable source is MITRE's own data repo,
// mitre-atlas/atlas-data. Two files matter:
//
//	dist/manifest.yaml   small, lists every release and its file path. This
//	                     is the cheap daily poll: if the newest release is one
//	                     we have already ingested, the run stops there.
//	dist/v6/ATLAS-*.yaml the release itself, roughly 700KB, fetched only when
//	                     the manifest shows something new.
//
// dist/ATLAS.yaml is deprecated upstream and deliberately not used; the
// deprecation notice is the first line of that file.
//
// Licence: the ATLAS data is Apache 2.0, MITRE. That is permissive with
// attribution, unlike the share-alike sources this site also carries, so the
// object names and structure can be republished with credit.
//
// What "new information" means here is not a diff we have to compute: every
// ATLAS object carries its own created-date and modified-date, so the newest
// techniques and case studies fall out of the data itself.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	atlasManifestURL = "https://raw.githubusercontent.com/mitre-atlas/atlas-data/main/dist/manifest.yaml"
	atlasFileBase    = "https://raw.githubusercontent.com/mitre-atlas/atlas-data/main/dist/"
	atlasSiteURL     = "https://atlas.mitre.org/"
)

type atlasManifestEntry struct {
	Release     string `yaml:"release"`
	ReleaseDate string `yaml:"release-date"`
	Versions    []struct {
		FormatVersion string `yaml:"format-version"`
		Path          string `yaml:"path"`
	} `yaml:"versions"`
}

type atlasObject struct {
	Name         string   `yaml:"name"`
	ID           string   `yaml:"id"`
	CreatedDate  string   `yaml:"created-date"`
	ModifiedDate string   `yaml:"modified-date"`
	ObjectType   string   `yaml:"object-type"`
	Actor        string   `yaml:"actor"`
	Target       string   `yaml:"target"`
	Date         string   `yaml:"date"`
	Type         string   `yaml:"type"`
	Platforms    []string `yaml:"platforms"`
	Maturity     string   `yaml:"maturity"`
}

type atlasRelease struct {
	FormatVersion string                 `yaml:"format-version"`
	Tactics       map[string]atlasObject `yaml:"tactics"`
	Techniques    map[string]atlasObject `yaml:"techniques"`
	Mitigations   map[string]atlasObject `yaml:"mitigations"`
	CaseStudies   map[string]atlasObject `yaml:"case-studies"`
}

// twoaiAtlasWatch polls the manifest and ingests any release we have not
// seen. Returns the release string in play, or empty when nothing changed.
func twoaiAtlasWatch(db *sql.DB) string {
	b, err := twoaiGridGet(atlasManifestURL)
	if err != nil {
		fmt.Println("twoai_atlas: manifest fetch failed:", err, "(keeping prior release)")
		return ""
	}
	var manifest []atlasManifestEntry
	if err := yaml.Unmarshal(b, &manifest); err != nil || len(manifest) == 0 {
		fmt.Printf("twoai_atlas: manifest unparsed: %v entries=%d\n", err, len(manifest))
		return ""
	}
	// Newest release first, by release string, which is YYYY.MM and sorts.
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Release > manifest[j].Release })
	newest := manifest[0]
	if len(newest.Versions) == 0 {
		fmt.Println("twoai_atlas: newest release lists no files")
		return ""
	}
	// Prefer the highest format-version offered for that release.
	v := newest.Versions[0]
	for _, cand := range newest.Versions {
		if cand.FormatVersion > v.FormatVersion {
			v = cand
		}
	}

	var known string
	db.QueryRow(`SELECT release FROM twoai_atlas_releases WHERE release=$1`, newest.Release).Scan(&known)
	if known != "" {
		fmt.Printf("twoai_atlas: release %s already ingested, nothing new\n", newest.Release)
		return newest.Release
	}

	rb, err := twoaiGridGet(atlasFileBase + v.Path)
	if err != nil {
		fmt.Println("twoai_atlas: release fetch failed:", err)
		return ""
	}
	var rel atlasRelease
	if err := yaml.Unmarshal(rb, &rel); err != nil {
		fmt.Println("twoai_atlas: release unparsed:", err)
		return ""
	}
	if len(rel.Techniques) == 0 {
		fmt.Println("twoai_atlas: release has no techniques, refusing to ingest")
		return ""
	}

	sets := []struct {
		kind string
		m    map[string]atlasObject
	}{
		{"tactic", rel.Tactics}, {"technique", rel.Techniques},
		{"mitigation", rel.Mitigations}, {"case-study", rel.CaseStudies},
	}
	stored := 0
	for _, s := range sets {
		for id, o := range s.m {
			if o.ID != "" {
				id = o.ID
			}
			extra := map[string]any{}
			if o.Actor != "" {
				extra["actor"] = o.Actor
			}
			if o.Target != "" {
				extra["target"] = o.Target
			}
			if o.Date != "" {
				extra["date"] = o.Date
			}
			if o.Type != "" {
				extra["type"] = o.Type
			}
			if len(o.Platforms) > 0 {
				extra["platforms"] = o.Platforms
			}
			if o.Maturity != "" {
				extra["maturity"] = o.Maturity
			}
			ej, _ := json.Marshal(extra)
			// NOTE: descriptions are intentionally not stored. The counts,
			// names and dates are what this site publishes; the prose stays
			// at MITRE, where the reader is sent.
			if _, err := db.Exec(`INSERT INTO twoai_atlas_objects
				(id, kind, name, created_date, modified_date, extra, release)
				VALUES ($1,$2,$3,NULLIF($4,'')::date,NULLIF($5,'')::date,$6::jsonb,$7)
				ON CONFLICT (id) DO UPDATE SET kind=EXCLUDED.kind, name=EXCLUDED.name,
					modified_date=EXCLUDED.modified_date, extra=EXCLUDED.extra,
					last_seen=now()`,
				id, s.kind, o.Name, o.CreatedDate, o.ModifiedDate, string(ej), newest.Release); err == nil {
				stored++
			}
		}
	}
	db.Exec(`INSERT INTO twoai_atlas_releases
		(release, release_date, path, format_version, tactics, techniques, mitigations, case_studies)
		VALUES ($1,NULLIF($2,'')::date,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (release) DO UPDATE SET release_date=EXCLUDED.release_date,
			path=EXCLUDED.path, format_version=EXCLUDED.format_version,
			tactics=EXCLUDED.tactics, techniques=EXCLUDED.techniques,
			mitigations=EXCLUDED.mitigations, case_studies=EXCLUDED.case_studies`,
		newest.Release, newest.ReleaseDate, v.Path, v.FormatVersion,
		len(rel.Tactics), len(rel.Techniques), len(rel.Mitigations), len(rel.CaseStudies))
	fmt.Printf("twoai_atlas: NEW release %s (%s) ingested objects=%d tactics=%d techniques=%d mitigations=%d case_studies=%d\n",
		newest.Release, newest.ReleaseDate, stored,
		len(rel.Tactics), len(rel.Techniques), len(rel.Mitigations), len(rel.CaseStudies))
	return newest.Release
}

type atlasOut struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Created string `json:"created,omitempty"`
	Target  string `json:"target,omitempty"`
	Actor   string `json:"actor,omitempty"`
}

// twoaiAtlasDoc assembles what the security hub renders. Everything here is
// counts, names and dates: the substance stays at MITRE and the reader is
// pointed there.
func twoaiAtlasDoc(db *sql.DB) map[string]any {
	var release, relDate string
	var tac, tech, mit, cs int
	err := db.QueryRow(`SELECT release, COALESCE(release_date::text,''), tactics, techniques, mitigations, case_studies
		FROM twoai_atlas_releases ORDER BY release DESC LIMIT 1`).Scan(&release, &relDate, &tac, &tech, &mit, &cs)
	if err != nil {
		return nil
	}
	recent := func(kind string, n int) []atlasOut {
		out := []atlasOut{}
		rows, err := db.Query(`SELECT id, name, kind, COALESCE(created_date::text,''),
				COALESCE(extra->>'target',''), COALESCE(extra->>'actor','')
			FROM twoai_atlas_objects WHERE kind=$1 AND created_date IS NOT NULL
			ORDER BY created_date DESC, id DESC LIMIT $2`, kind, n)
		if err != nil {
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var o atlasOut
			if rows.Scan(&o.ID, &o.Name, &o.Kind, &o.Created, &o.Target, &o.Actor) == nil {
				out = append(out, o)
			}
		}
		return out
	}
	var newestCreated string
	db.QueryRow(`SELECT COALESCE(max(created_date)::text,'') FROM twoai_atlas_objects`).Scan(&newestCreated)
	return map[string]any{
		"release": release, "release_date": relDate,
		"tactics": tac, "techniques": tech, "mitigations": mit, "case_studies": cs,
		"newest_techniques":   recent("technique", 6),
		"newest_case_studies": recent("case-study", 6),
		"newest_object_date":  newestCreated,
		"site_url":            atlasSiteURL,
		"data_url":            "https://github.com/mitre-atlas/atlas-data",
		"checked":             time.Now().UTC().Format("2006-01-02"),
	}
}
