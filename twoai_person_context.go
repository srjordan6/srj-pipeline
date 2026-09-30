package main

// twoai_person_context: what this site holds about a person, on the person's
// own page.
//
// Stephen, 2026-09-29, on Elon Musk's page: "very thin content on the richest
// man in the world". The profile was a hook, five facts, three achievements
// and a timeline, while the site held four of his companies, their filings,
// the lawsuits over his companies, and dozens of news stories that named
// him, none of which the page showed. Every person page now carries what the
// site knows: the companies they founded or lead, the lawsuits naming them or
// their companies, the news that named them, and a written reading of where
// they stand in AI now, written from those records and nothing else.

import (
	"time"

	"github.com/lib/pq"

	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const twoaiPersonReadingsPerRun = 12

var twoaiPersonReadingsWritten int

type personLink struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Detail string `json:"detail,omitempty"`
	Date   string `json:"date,omitempty"`
}

func twoaiPersonContext(db *sql.DB, uid, name string, d map[string]any) {
	if uid == "" || name == "" {
		return
	}
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_person_readings (uid text PRIMARY KEY, body jsonb NOT NULL,
		data_hash text, model text, generated_on date NOT NULL DEFAULT current_date)`)

	// Names the records are searched for: the full name, and the surname alone
	// when it is long enough and no other profiled person shares it.
	surname := name[strings.LastIndex(name, " ")+1:]
	var shared int
	db.QueryRow(`SELECT count(*) FROM site_people WHERE data->>'name' <> $1 AND lower(split_part(data->>'name',' ',-1)) = lower($2)`, name, surname).Scan(&shared)
	terms := []string{name}
	if len(surname) >= 4 && shared == 0 && !strings.EqualFold(surname, name) {
		terms = append(terms, surname)
	}

	var companies, lawsuits, stories []personLink
	// Companies, from the knowledge graph.
	if rows, err := db.Query(`SELECT c.uid, c.name, g.label FROM twoai_graph g JOIN twoai_company_profiles c ON c.uid = g.other_uid
			WHERE g.kind='person' AND g.uid=$1 AND g.other_kind='company' AND g.confidence <> 'candidate' ORDER BY g.confidence = 'primary' DESC, c.name`, uid); err == nil {
		for rows.Next() {
			var cu, cn, lb string
			if rows.Scan(&cu, &cn, &lb) == nil {
				companies = append(companies, personLink{Name: cn, Path: "/companies/" + cu + "/", Detail: lb})
			}
		}
		rows.Close()
	}
	companyNames := []string{}
	for _, c := range companies {
		companyNames = append(companyNames, c.Name)
	}
	// Lawsuits naming the person or one of their companies as a party.
	if rows, err := db.Query(`SELECT slug, case_name, status, COALESCE(latest_development_date::text,'')
			FROM ai_lawsuits WHERE is_active AND (
			  EXISTS (SELECT 1 FROM unnest($1::text[]) t WHERE plaintiffs ILIKE '%' || t || '%' OR defendants ILIKE '%' || t || '%' OR case_name ILIKE '%' || t || '%')
			) ORDER BY filed_date DESC NULLS LAST LIMIT 8`, pq.Array(append(append([]string{}, terms...), companyNames...))); err == nil {
		for rows.Next() {
			var s, cn, st, dt string
			if rows.Scan(&s, &cn, &st, &dt) == nil {
				lawsuits = append(lawsuits, personLink{Name: cn, Path: "/ai-lawsuits/" + s + "/", Detail: st, Date: dt})
			}
		}
		rows.Close()
	}
	// News stories that named the person, newest first.
	var storyTotal int
	if rows, err := db.Query(`SELECT slug, headline, published_on::text FROM twoai_news_stories
			WHERE EXISTS (SELECT 1 FROM unnest($1::text[]) t WHERE headline ILIKE '%' || t || '%')
			ORDER BY published_on DESC LIMIT 10`, pq.Array(terms)); err == nil {
		for rows.Next() {
			var s, h, dt string
			if rows.Scan(&s, &h, &dt) == nil {
				stories = append(stories, personLink{Name: h, Path: "/ai-news/" + s + "/", Date: dt})
			}
		}
		rows.Close()
	}
	db.QueryRow(`SELECT count(*) FROM twoai_news_stories WHERE EXISTS (SELECT 1 FROM unnest($1::text[]) t WHERE headline ILIKE '%' || t || '%')`, pq.Array(terms)).Scan(&storyTotal)

	if len(companies)+len(lawsuits)+len(stories) == 0 {
		return
	}
	d["on_site"] = map[string]any{
		"companies": companies, "lawsuits": lawsuits, "stories": stories, "story_total": storyTotal,
	}

	// The reading: where the person stands in AI now, from these records and
	// the profile, rewritten when the records change and at most 90 days old.
	var hb strings.Builder
	fmt.Fprintf(&hb, "%s|%v|%v", name, d["hook"], d["quick_facts"])
	for _, c := range companies {
		hb.WriteString("|c:" + c.Path + c.Detail)
	}
	for _, l := range lawsuits {
		hb.WriteString("|l:" + l.Path + l.Detail)
	}
	for _, s := range stories {
		hb.WriteString("|s:" + s.Path)
	}
	h := sha256.Sum256([]byte(hb.String()))
	want := hex.EncodeToString(h[:8])
	var have, body string
	var age int
	db.QueryRow(`SELECT data_hash, body::text, current_date - generated_on FROM twoai_person_readings WHERE uid=$1`, uid).Scan(&have, &body, &age)
	if have == want && age < 90 && body != "" {
		var m map[string]any
		if json.Unmarshal([]byte(body), &m) == nil {
			d["standing"] = m
		}
		return
	}
	if body != "" {
		var m map[string]any
		if json.Unmarshal([]byte(body), &m) == nil {
			d["standing"] = m
		}
	}
	if twoaiPersonReadingsWritten >= twoaiPersonReadingsPerRun {
		return
	}
	var rec strings.Builder
	fmt.Fprintf(&rec, "Profile: %s, %v. %v\nQuick facts: %v\n", name, d["moniker"], d["hook"], d["quick_facts"])
	if len(companies) > 0 {
		rec.WriteString("Companies on this site:\n")
		for _, c := range companies {
			var hq, org, funding, val, founded string
			db.QueryRow(`SELECT COALESCE(headquarters,''), COALESCE(org_type,''), COALESCE(total_funding_usd::text,''), COALESCE(valuation_usd::text,''), COALESCE(founded::text,'') FROM twoai_company_profiles WHERE uid=$1`, strings.Trim(strings.TrimPrefix(c.Path, "/companies/"), "/")).Scan(&hq, &org, &funding, &val, &founded)
			fmt.Fprintf(&rec, "- %s (%s): %s, %s, founded %s, funding %s, valuation %s\n", c.Name, c.Detail, org, hq, founded, funding, val)
		}
	}
	if len(lawsuits) > 0 {
		rec.WriteString("Lawsuits on this site:\n")
		for _, l := range lawsuits {
			fmt.Fprintf(&rec, "- %s: %s (%s)\n", l.Name, l.Detail, l.Date)
		}
	}
	if len(stories) > 0 {
		fmt.Fprintf(&rec, "News on this site naming this person (%d stories in all; the latest):\n", storyTotal)
		for _, s := range stories {
			fmt.Fprintf(&rec, "- %s (%s)\n", s.Name, s.Date[:min(10, len(s.Date))])
		}
	}
	sys := "You write for The World of AI, an atlas of artificial intelligence. You are given what this site holds about one person: their profile, their companies, the lawsuits naming them or their companies, and the news stories that named them. " +
		"Write three parts, each one paragraph of four to six sentences, in plain editorial prose (commas, not dashes; no em dashes; no marketing language): " +
		"standing: where this person stands in artificial intelligence now, what they control or influence, as the records show; " +
		"record: what the site's records add up to, the companies, the cases, the coverage, and what pattern runs through them; " +
		"watch: what is unresolved or coming next according to the records, such as a pending case, a planned listing or a dispute. " +
		"Use only the records supplied. Never add a fact, figure, date, company or case they do not contain. Where the records are thin, say less. " +
		`Answer with one JSON object and nothing else: {"standing": "", "record": "", "watch": ""}`
	out, model, err := twoaiGenerate("page_readings", sys, rec.String())
	if err != nil {
		return
	}
	got, perr := twoaiArtJSON(out)
	if perr != nil || len(got["standing"]) < 150 || len(got["record"]) < 120 {
		return
	}
	m := map[string]any{"standing": got["standing"], "record": got["record"], "watch": got["watch"], "generated_on": time.Now().Format("2006-01-02"), "model": model}
	b, _ := json.Marshal(m)
	db.Exec(`INSERT INTO twoai_person_readings (uid, body, data_hash, model, generated_on) VALUES ($1,$2::jsonb,$3,$4,current_date)
		ON CONFLICT (uid) DO UPDATE SET body=EXCLUDED.body, data_hash=EXCLUDED.data_hash, model=EXCLUDED.model, generated_on=current_date`, uid, string(b), want, model)
	d["standing"] = m
	twoaiPersonReadingsWritten++
}
