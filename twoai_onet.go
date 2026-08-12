package main

// twoai_onet: the AI Skills Graph, built from O*NET Web Services.
//
// O*NET is the U.S. Department of Labor's occupational database. It gives us
// the one thing a skills page needs and nobody publishes for free otherwise:
// which skills and knowledge areas an occupation actually requires, weighted
// by importance, with links between occupations that share them.
//
// The graph is the join, not the download. O*NET says an Information Security
// Analyst needs Critical Thinking at importance 75; twoai_jobs says who is
// hiring one this week and at what salary. Neither half is rare on its own.
// Together they answer "what do I need to learn, and who will pay me for it",
// which is the question the section exists for.
//
// ATTRIBUTION. USDOL/ETA requires the O*NET-in-it mark and a specific
// attribution sentence on every page that shows this data, and O*NET is their
// trademark. The site renders that from src/components/OnetAttribution.astro.
// Redistribution as a dataset is not permitted, so the skills graph is never
// added to /api/ as a bulk download the way the site's own data is.
//
// Environment: ONET_API_KEY. Absent, the stage skips with a note and leaves
// the existing graph alone; it never fails a run.
//
// AUTH, THE HARD WAY. The first attempt used HTTP Basic against
// services.onetcenter.org/ws and every request came back 401. That is the
// version 1.9 shape. Version 2.0 moved to api-v2.onetcenter.org with an
// X-API-Key header, which is what O*NET's own published client does. Reading
// the reference manual was not enough, because its example URLs still show the
// old host; the client library is the thing that tells the truth.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// twoaiOnetSeed is the occupation set the graph starts from: the AI and
// computer-security occupations O*NET codes separately. Related occupations
// discovered from these are recorded as edges but not fetched, so the graph
// stays about this field instead of walking outward into all 900+ occupations.
var twoaiOnetSeed = []string{
	"15-2051.00", // Data Scientists
	"15-1221.00", // Computer and Information Research Scientists
	"15-1212.00", // Information Security Analysts
	"15-1299.04", // Penetration Testers
	"15-1299.05", // Information Security Engineers
	"15-1299.06", // Digital Forensics Analysts
	"15-1299.08", // Computer Systems Engineers/Architects
	"15-1299.09", // Information Technology Project Managers
	"15-1252.00", // Software Developers
	"15-1253.00", // Software Quality Assurance Analysts and Testers
	"15-1211.00", // Computer Systems Analysts
	"15-1241.00", // Computer Network Architects
	"15-1244.00", // Network and Computer Systems Administrators
	"15-1243.00", // Database Architects
	"15-1242.00", // Database Administrators
	"15-1251.00", // Computer Programmers
	"15-2041.00", // Statisticians
	"11-3021.00", // Computer and Information Systems Managers
}

const twoaiOnetBase = "https://api-v2.onetcenter.org"

// SHAPES, THE SAME HARD WAY. The second attempt authenticated fine and still
// got 15 of 18 wrong, because I guessed the field names. bright_outlook is an
// ARRAY of categories, not an object; skills carry `importance` and not a
// `score` object; tasks use `title` and not `name`; job zone is its own
// endpoint; wages live under `salary` as annual_median and friends. The three
// that did come through were simply the occupations with no bright_outlook
// field to trip over, and each stored exactly ten of everything, which was the
// tell: `display=long` is not a parameter here. Paging is `start` and `end`.
//
// Every struct below now comes from the published OpenAPI description at
// services.onetcenter.org/reference/openapi.json, which states each field
// outright. Guessing twice was one time too many.

// twoaiOnetPageEnd is the page ceiling for descriptor reports. The API
// defaults to 10 and caps at 2000; 200 covers every descriptor set O*NET
// publishes for an occupation with room to spare.
const twoaiOnetPageEnd = 200

// twoaiOnetStaleDays is when the graph is re-fetched in full. O*NET publishes
// on a roughly annual cycle, so a daily refetch would be 18 requests a day of
// pure noise against somebody else's free service. reviewed_on drives it.
const twoaiOnetStaleDays = 30

// onetElement is a descriptor row from a details report. `importance` is the
// standardised 0-100 rating and is absent when O*NET has no numeric rating,
// so it is a json.Number rather than an int.
type onetElement struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Importance  json.Number `json:"importance"`
}

func (e onetElement) Score() float64 {
	if e.Importance == "" {
		return 0
	}
	f, err := e.Importance.Float64()
	if err != nil {
		return 0
	}
	return f
}

func twoaiOnetGet(path string, key string, out any) error {
	req, err := http.NewRequest("GET", twoaiOnetBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "theworldofai.org (stephen@srjconsultingservices.com)")
	resp, err := twoaiJobsClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %d", path, resp.StatusCode)
	}
	return json.Unmarshal(b, out)
}

func twoaiOnet(db *sql.DB) error {
	auth := os.Getenv("ONET_API_KEY")
	if auth == "" {
		fmt.Fprintln(os.Stderr, "twoai_onet: ONET_API_KEY unset, skipping (existing graph kept)")
		return nil
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_onet_occupations (
		soc_code text PRIMARY KEY,
		title text NOT NULL,
		description text NOT NULL DEFAULT '',
		bright_outlook boolean NOT NULL DEFAULT false,
		job_zone int,
		ai_relevance text NOT NULL DEFAULT '',
		skills jsonb NOT NULL DEFAULT '[]'::jsonb,
		knowledge jsonb NOT NULL DEFAULT '[]'::jsonb,
		tasks jsonb NOT NULL DEFAULT '[]'::jsonb,
		related jsonb NOT NULL DEFAULT '[]'::jsonb,
		wage_median numeric, wage_p10 numeric, wage_p90 numeric,
		fetched_at timestamptz NOT NULL DEFAULT now(),
		reviewed_on date NOT NULL DEFAULT current_date)`); err != nil {
		return err
	}

	fetched, skipped, failed := 0, 0, 0
	for _, soc := range twoaiOnetSeed {
		var reviewed time.Time
		err := db.QueryRow(`SELECT reviewed_on FROM twoai_onet_occupations WHERE soc_code=$1`, soc).Scan(&reviewed)
		if err == nil && time.Since(reviewed) < twoaiOnetStaleDays*24*time.Hour {
			skipped++
			continue
		}

		var summary struct {
			Code          string `json:"code"`
			Title         string `json:"title"`
			Description   string `json:"description"`
			BrightOutlook []struct {
				Code  string `json:"code"`
				Title string `json:"title"`
			} `json:"bright_outlook"`
		}
		if err := twoaiOnetGet("/online/occupations/"+soc+"/", auth, &summary); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_onet: %s summary: %v\n", soc, err)
			failed++
			continue
		}
		if summary.Title == "" {
			fmt.Fprintf(os.Stderr, "twoai_onet: %s returned no title, skipping\n", soc)
			failed++
			continue
		}

		// Descriptor reports page at 10 by default. `end` raises that; there is
		// no display parameter, which is why every occupation previously stored
		// exactly ten skills.
		get := func(kind string) []onetElement {
			var doc struct {
				Total   int           `json:"total"`
				Element []onetElement `json:"element"`
			}
			if err := twoaiOnetGet(fmt.Sprintf("/online/occupations/%s/details/%s?start=1&end=%d",
				soc, kind, twoaiOnetPageEnd), auth, &doc); err != nil {
				fmt.Fprintf(os.Stderr, "twoai_onet: %s %s: %v\n", soc, kind, err)
				return nil
			}
			if doc.Total > len(doc.Element) {
				fmt.Fprintf(os.Stderr, "twoai_onet: %s %s truncated at %d of %d\n",
					soc, kind, len(doc.Element), doc.Total)
			}
			return doc.Element
		}
		skills := get("skills")
		knowledge := get("knowledge")

		// Tasks label their text `title`, not `name`.
		var taskDoc struct {
			Task []struct {
				ID         string      `json:"id"`
				Title      string      `json:"title"`
				Importance json.Number `json:"importance"`
				Category   string      `json:"category"`
			} `json:"task"`
		}
		if err := twoaiOnetGet(fmt.Sprintf("/online/occupations/%s/details/tasks?start=1&end=%d",
			soc, twoaiOnetPageEnd), auth, &taskDoc); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_onet: %s tasks: %v\n", soc, err)
		}
		// The page renders tasks under a `name`, so normalise here rather than
		// teaching the template about O*NET's field names.
		tasksOut := make([]map[string]any, 0, len(taskDoc.Task))
		for _, t := range taskDoc.Task {
			if t.Title == "" {
				continue
			}
			m := map[string]any{"id": t.ID, "name": t.Title}
			if t.Category != "" {
				m["category"] = t.Category
			}
			tasksOut = append(tasksOut, m)
		}

		var relDoc struct {
			Occupation []struct {
				Code  string `json:"code"`
				Title string `json:"title"`
			} `json:"occupation"`
		}
		if err := twoaiOnetGet(fmt.Sprintf("/online/occupations/%s/details/related_occupations?start=1&end=%d",
			soc, twoaiOnetPageEnd), auth, &relDoc); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_onet: %s related: %v\n", soc, err)
		}

		// Job zone is its own endpoint, not a field on the summary.
		var zoneDoc struct {
			Code      json.Number `json:"code"`
			Title     string      `json:"title"`
			Education string      `json:"education"`
		}
		if err := twoaiOnetGet("/online/occupations/"+soc+"/details/job_zone", auth, &zoneDoc); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_onet: %s job_zone: %v\n", soc, err)
		}

		// Wages: the My Next Move career report, under `salary`. A missing
		// figure stays null rather than being filled from anywhere else.
		var outlook struct {
			Salary struct {
				Annual10 json.Number `json:"annual_10th_percentile"`
				AnnualMe json.Number `json:"annual_median"`
				Annual90 json.Number `json:"annual_90th_percentile"`
			} `json:"salary"`
			Outlook struct {
				Category    string `json:"category"`
				Description string `json:"description"`
			} `json:"outlook"`
		}
		if err := twoaiOnetGet("/mnm/careers/"+soc+"/job_outlook", auth, &outlook); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_onet: %s outlook: %v\n", soc, err)
		}
		numOrNil := func(n json.Number) any {
			if n == "" {
				return nil
			}
			f, err := n.Float64()
			if err != nil || f <= 0 {
				return nil
			}
			return f
		}

		skillsJSON, _ := json.Marshal(skills)
		knowJSON, _ := json.Marshal(knowledge)
		taskJSON, _ := json.Marshal(tasksOut)
		relJSON, _ := json.Marshal(relDoc.Occupation)
		jz, _ := zoneDoc.Code.Int64()
		var jobZone any
		if jz > 0 {
			jobZone = jz
		}

		if _, err := db.Exec(`INSERT INTO twoai_onet_occupations
			(soc_code, title, description, bright_outlook, job_zone, skills, knowledge,
			 tasks, related, wage_median, wage_p10, wage_p90, fetched_at, reviewed_on)
			VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8::jsonb,$9::jsonb,$10,$11,$12,now(),current_date)
			ON CONFLICT (soc_code) DO UPDATE SET
				title=EXCLUDED.title, description=EXCLUDED.description,
				bright_outlook=EXCLUDED.bright_outlook, job_zone=EXCLUDED.job_zone,
				skills=EXCLUDED.skills, knowledge=EXCLUDED.knowledge, tasks=EXCLUDED.tasks,
				related=EXCLUDED.related, wage_median=EXCLUDED.wage_median,
				wage_p10=EXCLUDED.wage_p10, wage_p90=EXCLUDED.wage_p90,
				fetched_at=now(), reviewed_on=current_date`,
			soc, summary.Title, summary.Description, len(summary.BrightOutlook) > 0,
			jobZone, string(skillsJSON), string(knowJSON), string(taskJSON), string(relJSON),
			numOrNil(outlook.Salary.AnnualMe), numOrNil(outlook.Salary.Annual10),
			numOrNil(outlook.Salary.Annual90)); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_onet: %s upsert: %v\n", soc, err)
			failed++
			continue
		}
		fetched++
		// O*NET is a free public service. One request per 300ms is polite and
		// still finishes the whole seed set inside a minute.
		time.Sleep(300 * time.Millisecond)
	}

	var total int
	db.QueryRow(`SELECT count(*) FROM twoai_onet_occupations`).Scan(&total)
	fmt.Printf("twoai_onet: fetched=%d fresh_skipped=%d failed=%d occupations=%d ok=true\n",
		fetched, skipped, failed, total)
	return nil
}

// twoaiSkills builds skills/index.json and one file per occupation.
//
// The skill nodes are the graph's spine: every skill seen across the seeded
// occupations, each carrying the occupations that need it and how importantly.
// That inversion is what makes it a graph rather than eighteen unrelated
// profile pages, and it is the view a reader actually wants — "who hires for
// Critical Thinking" is a more useful question than "what does a Data
// Scientist do".
func twoaiSkills(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	rows, err := db.Query(`SELECT soc_code, title, description, bright_outlook,
			COALESCE(job_zone,0), skills::text, knowledge::text, tasks::text, related::text,
			COALESCE(wage_median,0), COALESCE(wage_p10,0), COALESCE(wage_p90,0),
			to_char(reviewed_on,'YYYY-MM-DD')
		FROM twoai_onet_occupations ORDER BY title`)
	if err != nil {
		return 0, nil
	}
	type elem struct {
		ID    string  `json:"id"`
		Name  string  `json:"name"`
		Desc  string  `json:"description,omitempty"`
		Score float64 `json:"score,omitempty"`
	}
	type occ struct {
		SOC        string  `json:"soc"`
		Title      string  `json:"title"`
		Desc       string  `json:"description,omitempty"`
		Bright     bool    `json:"bright_outlook,omitempty"`
		JobZone    int     `json:"job_zone,omitempty"`
		WageMedian float64 `json:"wage_median,omitempty"`
		WageP10    float64 `json:"wage_p10,omitempty"`
		WageP90    float64 `json:"wage_p90,omitempty"`
		Skills     []elem  `json:"skills,omitempty"`
		Knowledge  []elem  `json:"knowledge,omitempty"`
		Tasks      []struct {
			Name string `json:"name"`
		} `json:"tasks,omitempty"`
		Related []struct {
			Code  string `json:"code"`
			Title string `json:"title"`
		} `json:"related,omitempty"`
		Reviewed string `json:"reviewed_on,omitempty"`
		Openings int    `json:"openings,omitempty"`
	}
	var occs []occ
	for rows.Next() {
		var o occ
		var sk, kn, tk, rl string
		if rows.Scan(&o.SOC, &o.Title, &o.Desc, &o.Bright, &o.JobZone, &sk, &kn, &tk, &rl,
			&o.WageMedian, &o.WageP10, &o.WageP90, &o.Reviewed) != nil {
			continue
		}
		var rawSk, rawKn []onetElement
		json.Unmarshal([]byte(sk), &rawSk)
		json.Unmarshal([]byte(kn), &rawKn)
		conv := func(in []onetElement) []elem {
			out := []elem{}
			for _, e := range in {
				out = append(out, elem{ID: e.ID, Name: e.Name, Desc: e.Description, Score: e.Score()})
			}
			return out
		}
		o.Skills, o.Knowledge = conv(rawSk), conv(rawKn)
		json.Unmarshal([]byte(tk), &o.Tasks)
		json.Unmarshal([]byte(rl), &o.Related)
		occs = append(occs, o)
	}
	rows.Close()
	if len(occs) == 0 {
		return 0, nil
	}

	// Live openings per occupation, matched from our own job listings on the
	// occupation title's distinctive words. This is the join the section
	// exists for, and it is deliberately conservative: a title match we are
	// not confident in shows no number rather than a wrong one.
	for i := range occs {
		key := strings.ToLower(occs[i].Title)
		for _, cut := range []string{"s and ", " and ", ", "} {
			if idx := strings.Index(key, cut); idx > 0 {
				key = key[:idx]
			}
		}
		key = strings.TrimSuffix(strings.TrimSpace(key), "s")
		if len(key) < 6 {
			continue
		}
		db.QueryRow(`SELECT count(*) FROM twoai_jobs
			WHERE last_seen > now() - interval '3 days' AND lower(title) LIKE '%'||$1||'%'`,
			key).Scan(&occs[i].Openings)
	}

	// Invert to skill nodes.
	type skillNode struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Desc   string `json:"description,omitempty"`
		Occs   []occRef `json:"occupations"`
		Median float64  `json:"median_score,omitempty"`
	}
	type byID struct {
		node *skillNode
		sum  float64
	}
	index := map[string]*byID{}
	for _, o := range occs {
		for _, s := range o.Skills {
			if s.ID == "" {
				continue
			}
			b, ok := index[s.ID]
			if !ok {
				b = &byID{node: &skillNode{ID: s.ID, Name: s.Name, Desc: s.Desc}}
				index[s.ID] = b
			}
			b.node.Occs = append(b.node.Occs, occRef{SOC: o.SOC, Title: o.Title, Score: s.Score,
				WageMedian: o.WageMedian, Openings: o.Openings})
			b.sum += s.Score
		}
	}
	skillList := []skillNode{}
	for _, b := range index {
		if len(b.node.Occs) > 0 {
			b.node.Median = b.sum / float64(len(b.node.Occs))
		}
		skillList = append(skillList, *b.node)
	}
	// Most-shared skills first: a skill four occupations need is more useful
	// as an entry point than one only a single occupation lists.
	sort.Slice(skillList, func(i, j int) bool {
		if len(skillList[i].Occs) != len(skillList[j].Occs) {
			return len(skillList[i].Occs) > len(skillList[j].Occs)
		}
		if skillList[i].Median != skillList[j].Median {
			return skillList[i].Median > skillList[j].Median
		}
		return skillList[i].Name < skillList[j].Name
	})

	pages := 0
	for _, o := range occs {
		if err := upsert("skills/occupation-"+o.SOC+".json", "onet-occupation", map[string]any{
			"generated": today, "occupation": o,
		}); err != nil {
			return pages, err
		}
		pages++
	}
	if err := upsert("skills/index.json", "skills-hub", map[string]any{
		"uid":         twoaiUID("section:ai-skills-graph"),
		"generated":   today,
		"occupations": occs,
		"skills":      skillList,
		"attribution": "This site incorporates information from O*NET Web Services by the U.S. Department of Labor, " +
			"Employment and Training Administration (USDOL/ETA). O*NET is a trademark of USDOL/ETA.",
	}); err != nil {
		return pages, err
	}
	pages++
	fmt.Printf("twoai_build: skills graph occupations=%d skills=%d ok=true\n", len(occs), len(skillList))
	return pages, nil
}

type occRef struct {
	SOC        string  `json:"soc"`
	Title      string  `json:"title"`
	Score      float64 `json:"score,omitempty"`
	WageMedian float64 `json:"wage_median,omitempty"`
	Openings   int     `json:"openings,omitempty"`
}
