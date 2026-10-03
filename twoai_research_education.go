package main

// twoai_research_education: an Education shelf in the AI Research Library.
//
// Stephen, 2026-10-03: add education to the library's AI topics. The shelf
// was hand-curated from the Consensus index; OpenAlex now harvests Artificial
// Intelligence in Education (T14414, row 394), and twoai_works already held
// the field's landmark papers under its education topics. These 25 were
// chosen from the works whose topic is an education topic, whose title names
// both education and AI, which carry an abstract, and which are not clinical
// (medical education is under healthcare): the founding intelligent tutoring
// papers and their effectiveness studies, the field reviews, the ethics and
// policy frameworks, the generative AI and ChatGPT work, and learning
// analytics. Each gets a one-sentence note written from its own title and
// abstract only. Added once; a paper already on the shelf is left alone, and
// a note that fails is tried again on the next run.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

var twoaiEducationShelf = []string{
	// Intelligent tutoring, the field's foundation and its evidence.
	"W2136208491", // Intelligent Tutoring Systems (Science, 1985)
	"W2051339053", // The Relative Effectiveness of Human Tutoring, Intelligent Tutoring Systems...
	"W1985824294", // Effectiveness of Intelligent Tutoring Systems (meta-analysis)
	"W2112564774", // Intelligent Tutoring Goes To School in the Big City
	// Reviews of the field.
	"W2981863007", // Systematic review of AI applications in higher education
	"W2770717476", // Exploring the impact of AI on teaching and learning in higher education
	"W3156614709", // A Review of AI in Education from 2010 to 2020
	"W4366783381", // Artificial intelligence in higher education: the state of the field
	"W3157202587", // Artificial intelligence in education: The three paradigms
	"W4229056760", // The Promises and Challenges of AI for Teachers
	"W2746986659", // Intelligence Unleashed: An argument for AI in Education
	"W3046530224", // Historical threads, missing links, and future directions in AI in education
	// Ethics and policy.
	"W3199189488", // AI in education: Addressing ethical challenges in K-12 settings
	"W3155263273", // Ethics of AI in Education: Towards a Community-Wide Framework
	"W4304943299", // Ethical principles for artificial intelligence in education
	"W4383312437", // A comprehensive AI policy education framework for university teaching
	"W4293731327", // Towards Intelligent-TPACK: teachers' knowledge to integrate AI ethically
	// Generative AI and ChatGPT.
	"W4324046518", // Chatting and cheating: academic integrity in the era of ChatGPT
	"W4384464487", // Students' voices on generative AI in higher education
	"W4366420437", // What Is the Impact of ChatGPT on Education? A Rapid Review
	"W4317910584", // ChatGPT: the end of traditional assessments in higher education?
	"W4399790131", // Over-reliance on AI dialogue systems and students' cognitive abilities
	"W4385632485", // Practical and ethical challenges of LLMs in education: scoping review
	// Learning analytics.
	"W3000065748", // Educational data mining and learning analytics: An updated survey
	"W2078482049", // Learning analytics and educational data mining
}

const twoaiEducationTopic = "education"

func twoaiResearchEducation(db *sql.DB) int {
	added, held := 0, 0
	for _, oid := range twoaiEducationShelf {
		var exists bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM twoai_research_papers WHERE openalex_id=$1)`, oid).Scan(&exists)
		if exists {
			continue
		}
		var title, abstract, doi, oaURL, workType, authorsRaw string
		var year, cited sql.NullInt64
		if err := db.QueryRow(`SELECT title, COALESCE(abstract,''), COALESCE(doi,''), COALESCE(oa_url,''), COALESCE(work_type,''),
				COALESCE(authors::text,'[]'), pub_year, cited_by
			FROM twoai_works WHERE openalex_id=$1 AND excluded_reason IS NULL`, oid).
			Scan(&title, &abstract, &doi, &oaURL, &workType, &authorsRaw, &year, &cited); err != nil {
			fmt.Println("twoai_research_education: not in the index yet:", oid)
			continue
		}
		var auth []map[string]any
		json.Unmarshal([]byte(authorsRaw), &auth)
		names := []string{}
		for i, a := range auth {
			if i >= 6 {
				names = append(names, "et al.")
				break
			}
			if n, _ := a["name"].(string); n != "" {
				names = append(names, n)
			} else if n, _ := a["display_name"].(string); n != "" {
				names = append(names, n)
			}
		}
		note, ok := twoaiShelfNote(title, abstract)
		if !ok {
			held++
			continue
		}
		link := oaURL
		if doi != "" {
			link = "https://doi.org/" + doi
		}
		if link == "" {
			link = "https://openalex.org/" + oid
		}
		uid := "e" + twoaiUID("shelf:" + oid)[:7]
		if _, err := db.Exec(`INSERT INTO twoai_research_papers
				(uid, title, authors, year, journal, citations, url, topic, our_note, source, added_on, abstract, abstract_source, doi, paper_type, openalex_id)
			VALUES ($1,$2,$3,$4,NULL,$5,$6,$7,$8,'openalex',current_date,$9,'openalex',$10,$11,$12)
			ON CONFLICT (uid) DO NOTHING`,
			uid, title, strings.Join(names, ", "), nullInt(year), nullInt(cited), link, twoaiEducationTopic, note,
			abstract, nullStr(strings.ToLower(doi)), nullStr(workType), oid); err != nil {
			fmt.Println("twoai_research_education:", oid, err)
			continue
		}
		added++
		fmt.Printf("twoai_research_education: %s %s\n", uid, trunc(title, 80))
	}
	fmt.Printf("twoai_research_education: added=%d held=%d of %d ok=true\n", added, held, len(twoaiEducationShelf))
	return added
}

// twoaiShelfNote writes the shelf's one-line note from the title and
// abstract only: what the paper found or argues, and why it matters.
func twoaiShelfNote(title, abstract string) (string, bool) {
	system := `You write the one-line note for a paper on the shelf of an AI research library. Using only the title and abstract given, write one sentence of 18 to 40 words saying what the paper found or argues and why it matters to someone following AI in education. Plain English, no hype, commas rather than dashes, no quotation of the abstract, no markdown. Return only JSON: {"note": "..."}`
	out, _, err := twoaiGenerate("shelf_notes", system, "Title: "+title+"\n\nAbstract:\n"+trunc(abstract, 3500))
	if err != nil {
		return "", false
	}
	text := strings.TrimSpace(out)
	if i := strings.Index(text, "{"); i >= 0 {
		text = text[i:]
	}
	if k := strings.LastIndex(text, "}"); k >= 0 {
		text = text[:k+1]
	}
	var got struct {
		Note string `json:"note"`
	}
	if json.Unmarshal([]byte(text), &got) != nil {
		return "", false
	}
	n := strings.TrimSpace(twoaiStripMarkdown(got.Note))
	if l := len([]rune(n)); l < 60 || l > 320 || strings.Contains(n, "?") || strings.Contains(n, "—") {
		fmt.Fprintln(os.Stderr, "twoai_research_education: note held for", trunc(title, 60))
		return "", false
	}
	return n, true
}
