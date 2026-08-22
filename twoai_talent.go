package main

// The AI Talent Network pipeline stages.
//
// talent_pull copies candidate profiles out of the Cloudflare D1 database the
// Worker writes into (talent_state) and into Postgres, over the D1 REST API
// with a read-only token. Only publishable columns cross: PII, password
// hashes, tokens, and resume keys never leave D1 - the pull selects around
// them by construction, so Postgres cannot leak what it never held.
//
// Publication is then part of twoai_build: profiles.json is emitted from the
// Postgres copy for rows whose status is live. The status flip itself is the
// human review step and happens in D1 (the Worker's own gate for the public
// PDF, DOCX, and photo reads), so one UPDATE in the D1 console both opens the
// Worker's endpoints and, via the next pull, publishes the page.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	d1AccountID = "2db97ad8218dd8a17d22368d32e41161"
	d1DatabaseID = "db78dab4-42ea-4704-9cd0-3adeacebefe4"
)

func talentPull(db *sql.DB) error {
	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	if token == "" {
		return fmt.Errorf("talent_pull: CLOUDFLARE_API_TOKEN not set")
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_talent_profiles (
		tai_id text PRIMARY KEY,
		status text NOT NULL,
		profile jsonb NOT NULL DEFAULT '{}'::jsonb,
		answers jsonb NOT NULL DEFAULT '{}'::jsonb,
		share_pdf boolean NOT NULL DEFAULT false,
		has_photo boolean NOT NULL DEFAULT false,
		d1_updated text,
		pulled_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("talent_pull create table: %w", err)
	}

	// One query, no parameters, columns named so nothing sensitive can ride
	// along: no email, no pii, no pw_salt/pw_hash, no confirm_token, no
	// resume_key.
	body, _ := json.Marshal(map[string]any{
		"sql": `SELECT tai_id, status, profile, answers, share_pdf,
			CASE WHEN photo_type IS NOT NULL AND photo_type != '' THEN 1 ELSE 0 END AS has_photo,
			updated_at FROM talent_state`,
	})
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/d1/database/%s/query", d1AccountID, d1DatabaseID)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("talent_pull d1 query: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("talent_pull d1 http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Result []struct {
			Results []map[string]any `json:"results"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("talent_pull d1 parse: %w", err)
	}
	if !out.Success {
		msg := "unknown"
		if len(out.Errors) > 0 {
			msg = out.Errors[0].Message
		}
		return fmt.Errorf("talent_pull d1 error: %s", msg)
	}
	rows := []map[string]any{}
	if len(out.Result) > 0 {
		rows = out.Result[0].Results
	}

	str := func(v any) string {
		s, _ := v.(string)
		return s
	}
	n := 0
	for _, r := range rows {
		taiID := str(r["tai_id"])
		if taiID == "" {
			continue
		}
		profile := str(r["profile"])
		if profile == "" || !json.Valid([]byte(profile)) {
			profile = "{}"
		}
		answers := str(r["answers"])
		if answers == "" || !json.Valid([]byte(answers)) {
			answers = "{}"
		}
		sharePdf := fmt.Sprintf("%v", r["share_pdf"]) == "1"
		hasPhoto := fmt.Sprintf("%v", r["has_photo"]) == "1"
		if _, err := db.Exec(`INSERT INTO twoai_talent_profiles
			(tai_id, status, profile, answers, share_pdf, has_photo, d1_updated, pulled_at)
			VALUES ($1,$2,$3::jsonb,$4::jsonb,$5,$6,$7,now())
			ON CONFLICT (tai_id) DO UPDATE SET status=EXCLUDED.status,
				profile=EXCLUDED.profile, answers=EXCLUDED.answers,
				share_pdf=EXCLUDED.share_pdf, has_photo=EXCLUDED.has_photo,
				d1_updated=EXCLUDED.d1_updated, pulled_at=now()`,
			taiID, str(r["status"]), profile, answers, sharePdf, hasPhoto, str(r["updated_at"])); err != nil {
			return fmt.Errorf("talent_pull upsert %s: %w", taiID, err)
		}
		n++
	}
	// A profile deleted from D1 (expiry purge) disappears from Postgres too,
	// so an expired page cannot be rebuilt from a stale copy.
	if _, err := db.Exec(`DELETE FROM twoai_talent_profiles WHERE pulled_at < now() - interval '10 minutes'`); err != nil {
		return fmt.Errorf("talent_pull prune: %w", err)
	}
	fmt.Printf("talent_pull: upserted %d profiles from D1\n", n)
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// talentPublish emits content/talent/profiles.json for twoai_build from LIVE
// rows only. Question labels come from talent_questions so the skills blocks
// on the public page carry the same wording as the form that collected them.
func talentPublish(db *sql.DB, today string, upsert func(path, kind string, data any) error) error {
	labels := map[string]string{}
	order := []string{}
	// short_label is the resume-facing category name ("Foundation Models"),
	// per Stephen 2026-08-22; the full question text stays on the form only.
	if qr, err := db.Query(`SELECT question_key, COALESCE(NULLIF(short_label,''), label) FROM talent_questions WHERE active ORDER BY sort_order`); err == nil {
		for qr.Next() {
			var k, l string
			if qr.Scan(&k, &l) == nil {
				labels[k] = l
				order = append(order, k)
			}
		}
		qr.Close()
	}

	// Fresh listings for the match engine: same 3-day render window the jobs
	// page uses, loaded once and scored against every live profile.
	type jobRow struct {
		Title    string `json:"title"`
		Company  string `json:"company"`
		Location string `json:"location"`
		URL      string `json:"url"`
		Posted   string `json:"posted_on,omitempty"`
		Remote   bool   `json:"remote"`
		hay      string
	}
	var jobPool []jobRow
	if jr, err := db.Query(`SELECT title, company, COALESCE(location,''), remote, url,
			COALESCE(posted_on::text,''), COALESCE(skills::text,'[]')
		FROM twoai_jobs WHERE last_seen > now() - interval '3 days'`); err == nil {
		for jr.Next() {
			var j jobRow
			var skillsRaw string
			if jr.Scan(&j.Title, &j.Company, &j.Location, &j.Remote, &j.URL, &j.Posted, &skillsRaw) != nil {
				continue
			}
			var sk []string
			json.Unmarshal([]byte(skillsRaw), &sk)
			j.hay = strings.ToLower(j.Title + " " + j.Company + " " + strings.Join(sk, " "))
			jobPool = append(jobPool, j)
		}
		jr.Close()
	}
	paren := regexp.MustCompile(`\s*\([^)]*\)`)
	stop := map[string]bool{"ai": true, "other": true, "none": true, "yes": true, "no": true}

	rows, err := db.Query(`SELECT tai_id, profile, answers, share_pdf, has_photo, d1_updated
		FROM twoai_talent_profiles WHERE status='live' ORDER BY tai_id`)
	if err != nil {
		// Table absent (talent_pull never ran) is not fatal to the build.
		fmt.Println("twoai_build: talent profiles skipped:", err)
		return nil
	}
	defer rows.Close()

	type skill struct {
		Label string   `json:"label"`
		Items []string `json:"items"`
	}
	var profiles []map[string]any
	for rows.Next() {
		var taiID, profileRaw, answersRaw, updated string
		var sharePdf, hasPhoto bool
		if rows.Scan(&taiID, &profileRaw, &answersRaw, &sharePdf, &hasPhoto, &updated) != nil {
			continue
		}
		var p map[string]any
		var a map[string]struct {
			Selections []string `json:"selections"`
			NA         bool     `json:"na"`
			Other      string   `json:"other"`
		}
		json.Unmarshal([]byte(profileRaw), &p)
		json.Unmarshal([]byte(answersRaw), &a)

		years := ""
		var skills []skill
		for _, k := range order {
			ans, ok := a[k]
			if !ok {
				continue
			}
			items := append([]string{}, ans.Selections...)
			if ans.Other != "" {
				items = append(items, ans.Other)
			}
			if k == "experience-years" {
				if len(items) > 0 {
					years = items[0]
				}
				continue
			}
			if len(items) > 0 {
				skills = append(skills, skill{Label: labels[k], Items: items})
			}
		}

		// Match engine: a member term hits when it appears in the listing's
		// title, company, or extracted skills. Terms are the member's own
		// verified selections, lowercased, parentheticals stripped; short and
		// generic tokens are dropped so "AI" cannot match everything.
		terms := map[string]bool{}
		for _, ans := range a {
			for _, sel := range append(append([]string{}, ans.Selections...), ans.Other) {
				t := strings.ToLower(strings.TrimSpace(paren.ReplaceAllString(sel, "")))
				if len(t) >= 3 && !stop[t] {
					terms[t] = true
				}
			}
		}
		type scored struct {
			jobRow
			score int
		}
		var hits []scored
		for _, j := range jobPool {
			sc := 0
			for t := range terms {
				if strings.Contains(j.hay, t) {
					sc++
				}
			}
			if sc > 0 {
				hits = append(hits, scored{j, sc})
			}
		}
		sort.Slice(hits, func(i, k int) bool {
			if hits[i].score != hits[k].score {
				return hits[i].score > hits[k].score
			}
			return hits[i].Posted > hits[k].Posted
		})
		if len(hits) > 12 {
			hits = hits[:12]
		}
		var matches []jobRow
		for _, h := range hits {
			matches = append(matches, h.jobRow)
		}

		// D1's updated_at is epoch seconds; the page prints this verbatim,
		// so format it as a date here rather than shipping a raw integer.
		if secs, err2 := strconv.ParseInt(updated, 10, 64); err2 == nil && secs > 1000000000 {
			updated = time.Unix(secs, 0).UTC().Format("2006-01-02")
		}
		entry := map[string]any{
			"tai_id": taiID, "share_pdf": sharePdf, "has_photo": hasPhoto,
			"years": years, "skills": skills, "updated": updated,
			"matches": matches,
		}
		for _, k := range []string{"first_name", "headline", "location", "availability", "rate",
			"summary", "certifications", "publications", "awards", "work_experience", "education"} {
			if v, ok := p[k]; ok {
				entry[k] = v
			}
		}
		for _, k := range []string{"jobs", "projects_items", "education_items", "certifications_items",
			"publications_items", "patents_items", "awards_items"} {
			if v, ok := p[k]; ok {
				entry[k] = v
			}
		}
		profiles = append(profiles, entry)
	}
	if err := upsert("talent/profiles.json", "talent-profiles", map[string]any{
		"generated": today, "profiles": profiles,
	}); err != nil {
		return err
	}
	// Digest feed for the Worker's weekly mailer: public-safe by construction
	// (derived from public profiles and public listings), top 8 per member.
	digest := map[string]any{}
	for _, e := range profiles {
		m, _ := e["matches"].([]jobRow)
		if len(m) > 8 {
			m = m[:8]
		}
		digest[e["tai_id"].(string)] = map[string]any{
			"first_name": e["first_name"], "matches": m,
		}
	}
	if err := upsert("talent/matches.json", "talent-matches", map[string]any{
		"generated": today, "members": digest,
	}); err != nil {
		return err
	}
	fmt.Printf("twoai_build: talent profiles published=%d\n", len(profiles))
	return nil
}
