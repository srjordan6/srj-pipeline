package main

// The IntuitionLabs library on the Healthcare page. Stephen, 2026-10-04: "most
// of https://intuitionlabs.ai/ should be in" the Healthcare industry page
// (092b8864).
//
// IntuitionLabs, an AI consultancy for pharma and life sciences, publishes
// about 1,250 articles, roughly 530 of them on AI. Their text is theirs, so
// nothing is copied: the page carries each relevant article's title, date and
// a link to it on their site, grouped by topic, newest first.
//
//  1. Harvest. /articles and /articles/page/N carry the titles as schema.org
//     ListItems, newest first. The first run walks every page; later runs stop
//     at the first page with nothing new. Dates come from the articles
//     sitemap's lastmod.
//  2. Classify. Titles that pass the AI term test go to the model in batches,
//     which puts each in one life sciences topic or "none" (general AI, GPU
//     prices, model comparisons with no life sciences angle).
//  3. Publish. The grouped list goes into twoai_page_extras under the
//     Healthcare page, merged into the page at publish time.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const extLibSource = "intuitionlabs"
const extLibPage = "industries/industry-healthcare.json"

var extLibTopics = []string{
	"Drug discovery and research",
	"Clinical development and trials",
	"Regulatory, quality and compliance",
	"Safety and pharmacovigilance",
	"Medical affairs and medical writing",
	"Commercial and market access",
	"Manufacturing and supply chain",
	"Health care delivery and medical devices",
	"AI adoption, governance and tools in life sciences",
}

var (
	extLibLD   = regexp.MustCompile(`(?s)<script[^>]*application/ld\+json[^>]*>(.*?)</script>`)
	extLibLoc  = regexp.MustCompile(`(?s)<url>\s*<loc>https://intuitionlabs\.ai/articles/([a-z0-9-]+)</loc>\s*(?:<lastmod>([^<]+)</lastmod>)?`)
	extLibAIRe = regexp.MustCompile(`(?i)\b(ai|genai|generative|llms?|gpt[-\w.]*|chatgpt|openai|claude|anthropic|gemini|copilot|machine learning|ml|deep learning|neural|agents?|agentic|chatbots?|rag|nlp|language models?|foundation models?|alphafold|computer vision|predictive|automation|mcp|deepseek|llama|mistral|prompt\w*|embeddings?)\b`)
)

func extLibGet(u string) (string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 SRJ-Consulting-intel-sync/1.0 (theworldofai.org)")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s: status %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return string(b), err
}

// extLibListItems pulls (slug, title) pairs from a listing page.
func extLibListItems(page string) [][2]string {
	var out [][2]string
	for _, m := range extLibLD.FindAllStringSubmatch(page, -1) {
		var v any
		if json.Unmarshal([]byte(m[1]), &v) != nil {
			continue
		}
		stack := []any{v}
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			switch t := x.(type) {
			case []any:
				stack = append(stack, t...)
			case map[string]any:
				u, _ := t["url"].(string)
				name, _ := t["name"].(string)
				if t["@type"] == "ListItem" && strings.Contains(u, "/articles/") && name != "" {
					slug := u[strings.LastIndex(u, "/")+1:]
					if slug != "" && slug != "articles" {
						out = append(out, [2]string{slug, html.UnescapeString(name)})
					}
				}
				for _, vv := range t {
					stack = append(stack, vv)
				}
			}
		}
	}
	return out
}

func twoaiExtLibrary(db *sql.DB) error {
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_ext_library (source text NOT NULL, slug text NOT NULL, url text NOT NULL,
		title text NOT NULL, lastmod date, first_seen date NOT NULL DEFAULT current_date, is_ai boolean,
		topic text, classified_on date, PRIMARY KEY (source, slug))`)
	db.Exec(`CREATE TABLE IF NOT EXISTS twoai_page_extras (page_path text NOT NULL, key text NOT NULL, value jsonb NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (page_path, key))`)

	// 1. Harvest.
	var known int
	db.QueryRow(`SELECT count(*) FROM twoai_ext_library WHERE source = $1`, extLibSource).Scan(&known)
	added := 0
	for page := 1; page <= 120; page++ {
		u := "https://intuitionlabs.ai/articles"
		if page > 1 {
			u = fmt.Sprintf("%s/page/%d", u, page)
		}
		body, err := extLibGet(u)
		if err != nil {
			if page == 1 {
				return err
			}
			break // past the last page
		}
		items := extLibListItems(body)
		if len(items) == 0 {
			break
		}
		fresh := 0
		for _, it := range items {
			res, err := db.Exec(`INSERT INTO twoai_ext_library (source, slug, url, title, is_ai)
				VALUES ($1,$2,$3,$4,$5) ON CONFLICT (source, slug) DO UPDATE SET title = EXCLUDED.title
				WHERE twoai_ext_library.title <> EXCLUDED.title`,
				extLibSource, it[0], "https://intuitionlabs.ai/articles/"+it[0], it[1], extLibAIRe.MatchString(it[1]))
			if err == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					fresh++
				}
			}
		}
		added += fresh
		// Newest first: once a page brings nothing new, the rest is known.
		if fresh == 0 && known > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if sm, err := extLibGet("https://intuitionlabs.ai/articles-sitemap.xml"); err == nil {
		for _, m := range extLibLoc.FindAllStringSubmatch(sm, -1) {
			if len(m[2]) >= 10 {
				db.Exec(`UPDATE twoai_ext_library SET lastmod = $3::date WHERE source = $1 AND slug = $2 AND lastmod IS DISTINCT FROM $3::date`,
					extLibSource, m[1], m[2][:10])
			}
		}
	}

	// 2. Classify, 50 titles a call, at most 6 calls a run.
	topicList := strings.Join(extLibTopics, "\n")
	system := "You sort article titles from IntuitionLabs, a consultancy that writes about AI in pharma, biotech and life sciences, " +
		"into topics for the Healthcare page of a reference site about AI. For each title choose exactly one topic from this list, " +
		"copied exactly:\n" + topicList + "\nor none, when the article is not about using AI in life sciences or health care " +
		"(for example general GPU prices, model comparisons or AI pricing with no life sciences angle). " +
		"Reply with ONLY a JSON array, one element per title, in the order given: {\"slug\": \"...\", \"topic\": \"...\"}."
	valid := map[string]bool{"none": true}
	for _, t := range extLibTopics {
		valid[t] = true
	}
	classified := 0
	for call := 0; call < 6; call++ {
		rows, err := db.Query(`SELECT slug, title FROM twoai_ext_library WHERE source = $1 AND is_ai AND topic IS NULL
			ORDER BY lastmod DESC NULLS LAST, slug LIMIT 50`, extLibSource)
		if err != nil {
			break
		}
		var b strings.Builder
		n := 0
		for rows.Next() {
			var slug, title string
			if rows.Scan(&slug, &title) == nil {
				fmt.Fprintf(&b, "%s | %s\n", slug, title)
				n++
			}
		}
		rows.Close()
		if n == 0 {
			break
		}
		out, _, err := twoaiGenerate("library_topics", system, "slug | title\n"+b.String())
		if err != nil {
			fmt.Println("twoai_ext_library: classify:", err)
			break
		}
		if i, j := strings.Index(out, "["), strings.LastIndex(out, "]"); i >= 0 && j > i {
			out = out[i : j+1]
		}
		var got []struct {
			Slug  string `json:"slug"`
			Topic string `json:"topic"`
		}
		if json.Unmarshal([]byte(out), &got) != nil {
			fmt.Println("twoai_ext_library: classify: the reply was not the JSON asked for")
			break
		}
		for _, g := range got {
			if !valid[g.Topic] {
				continue
			}
			if res, err := db.Exec(`UPDATE twoai_ext_library SET topic = $3, classified_on = current_date
				WHERE source = $1 AND slug = $2 AND topic IS NULL`, extLibSource, g.Slug, g.Topic); err == nil {
				if k, _ := res.RowsAffected(); k > 0 {
					classified++
				}
			}
		}
	}

	// SUPERSEDED, theworldofai row 457 (Stephen, 2026-10-05): the site carries
	// no reference to IntuitionLabs, no per-article pages and no list of their
	// titles. This stage now only registers and classifies the articles; the
	// material reaches the site through the Life Sciences synthesis pages and
	// the routing of rows 457 and 458, each fact cited to its primary source.
	// The Healthcare library row in twoai_page_extras is kept, not rendered.
	fmt.Printf("twoai_ext_library: %d new articles, %d classified ok=true\n", added, classified)
	return nil
}
