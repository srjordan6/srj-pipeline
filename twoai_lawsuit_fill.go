package main

// twoai_lawsuit_fill writes the two fields a tracked case is missing: what the
// case is, and why a reader should care.
//
// Stephen, 2026-09-18: please use ollama to fill in the details of all the
// cases we are behind on. 95 of 113 tracked cases had a caption, a court and a
// docket timeline and nothing else, because intelPromote carries over only
// verified fields and its own comment says why: a machine-written
// characterisation of somebody's lawsuit is exactly the kind of confident
// invention this platform exists not to publish.
//
// That concern is right, so this stage is built around it instead of past it.
//
// THE MODEL SEES ONLY THE RECORD. It is handed the caption, the court, the
// filing date, and the titles of the docket entries CourtListener returned,
// which are the clerk's words. It is told to describe what that record shows
// and nothing else: who sued whom, where, when, which claim family the entries
// indicate, and where the case stands procedurally.
//
// THE DRAFT IS CHECKED AGAINST THE RECORD. Any dollar figure, any number of
// three digits or more, and any capitalised name in the draft has to appear in
// the source text it was given. A draft that names something the record does
// not is rejected whole and nothing is written, the same rule
// twoai_enacted_laws applies to cited sections. A case with fewer than three
// docket entries is skipped: there is not enough record to read.
//
// WHY IT MATTERS IS NOT INVENTED PER CASE. It is one fixed sentence per claim
// family, written here by a person, because the significance of a copyright
// training-data suit is a property of the claim family, not something a model
// should improvise about a named defendant.
//
// Ollama only, through twoaiGenerate under stage LAWSUIT_FILL. There is no
// Anthropic route. A row is written once: a case that already has an executive
// summary, including the 18 written by hand, is never touched.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

var lawsuitWhy = map[string]string{
	// ONE INTELLECTUAL PROPERTY FAMILY, Stephen 2026-09-21: copyright, patent
	// and trademark are one category, because intellectual property insurance
	// answers all three. The kind of right is kept as a tag on each case.
	"intellectual property":              "Intellectual property suits over AI decide who owns what a model learned from and what it produces, whether copyrighted training data, patented methods or a protected name, which sets the licensing cost of every future model.",
	"hiring discrimination":              "Hiring discrimination suits test whether an employer, a vendor, or both answer for a screening tool that disadvantages a protected group, which decides who carries the liability in every automated hiring stack.",
	"product liability & wrongful death": "Product liability suits over chatbots ask a court to treat conversational output as a defective product, which would move AI harms out of speech law and into the law that governs unsafe goods.",
	"biometric privacy":                  "Biometric privacy suits apply statutes with fixed damages per violation to face and voice data collected at scale, which is why a single case can threaten a company's existence.",
	"trade secrets":                      "Trade secret suits between AI companies show how much of a model's value sits in people and unpublished methods, and how far a court will go to stop both from walking out the door.",
	"platform access & scraping":         "Scraping suits decide whether a site's terms and technical barriers can stop an AI company from collecting what is publicly visible, which governs how every model and agent gathers data.",
	"consumer protection":                "Consumer protection suits apply existing deception law to AI products, which lets a state attorney general act without waiting for an AI statute.",
	"right of publicity":                 "Right of publicity suits test whether a generated voice or likeness is a taking of the person it imitates, which decides what a generative tool may produce about real people.",
	"securities fraud":                   "Securities suits over AI claims hold executives to what they told investors a system could do, which is the enforcement route for AI washing.",
	"defamation":                         "Defamation suits over generated statements ask whether a company answers for false facts its model states about a real person or business.",
}

var lawsuitCatSignals = map[string]*regexp.Regexp{
	"intellectual property":              regexp.MustCompile(`(?i)copyright|ao 121|infring|dmca|patent|trademark|trade dress`),
	"hiring discrimination":              regexp.MustCompile(`(?i)title vii|adea|disparate impact|employment discrimination|eeoc`),
	"product liability & wrongful death": regexp.MustCompile(`(?i)wrongful death|product liability|failure to warn|strict liability`),
	"biometric privacy":                  regexp.MustCompile(`(?i)bipa|biometric`),
	"trade secrets":                      regexp.MustCompile(`(?i)trade secret|dtsa|misappropriat`),
	"platform access & scraping":         regexp.MustCompile(`(?i)scrap|cfaa|computer fraud and abuse`),
	"consumer protection":                regexp.MustCompile(`(?i)unfair (or|and) deceptive|consumer protection|udap`),
	"right of publicity":                 regexp.MustCompile(`(?i)right of publicity|likeness|lanham`),
	"securities fraud":                   regexp.MustCompile(`(?i)securities|10b-5|pslra|lead plaintiff`),
	"defamation":                         regexp.MustCompile(`(?i)defamation|libel|slander`),
}

// The order signals are tried in. A Go map iterates in random order, so a
// docket matching two families would have been labelled differently from one
// run to the next. Most specific first, copyright last, because "infring"
// also appears in patent and trademark dockets.
var lawsuitCatOrder = []string{
	"product liability & wrongful death", "biometric privacy", "hiring discrimination",
	"securities fraud", "trade secrets", "platform access & scraping", "defamation",
	"right of publicity", "consumer protection", "intellectual property",
}

var (
	fillNumRe  = regexp.MustCompile(`\$[0-9][0-9,.]*|\b[0-9]{3,}\b`)
	fillNameRe = regexp.MustCompile(`\b[A-Z][a-zA-Z]{3,}\b`)
	// Words a sentence may start with or that are ordinary legal vocabulary.
	// Anything capitalised and not here has to be in the record.
	fillCommon = regexp.MustCompile(`(?i)^(the|this|that|these|those|there|they|their|plaintiff|plaintiffs|defendant|defendants|court|district|circuit|appeals|appeal|judge|complaint|motion|order|summons|docket|case|filed|filing|entries|entry|record|united|states|northern|southern|eastern|western|central|middle|notice|answer|counsel|attorney|class|action|claim|claims|copyright|federal|civil|since|after|before|january|february|march|april|june|july|august|september|october|november|december|most|recent|activity|what|where|when|which|none|both|each|other|service|served|issued|assigned|magistrate|appearance|scheduling|conference|dismiss|amended|stipulation|extension|time|respond|corporate|disclosure|statement|cover|sheet|form|also|however|because|while|under|with|from|into|does|shows|show|names|named)$`)
)

type fillTL struct {
	Date  string `json:"date"`
	Title string `json:"title"`
}

func twoaiLawsuitFill(db *sql.DB) error {
	limit := 40
	if v := os.Getenv("TWOAI_LAWSUIT_FILL_LIMIT"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	for _, q := range []string{
		`ALTER TABLE ai_lawsuits ADD COLUMN IF NOT EXISTS details_model text`,
		`ALTER TABLE ai_lawsuits ADD COLUMN IF NOT EXISTS details_written_on date`,
		`ALTER TABLE ai_lawsuits ADD COLUMN IF NOT EXISTS details_last_try date`,
		`ALTER TABLE ai_lawsuits ADD COLUMN IF NOT EXISTS details_note text`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("lawsuit_fill schema: %w", err)
		}
	}

	rows, err := db.Query(`SELECT id, slug, case_name, COALESCE(court,''), COALESCE(filed_date::text,''),
			COALESCE(category,''), COALESCE(timeline::text,'[]'), COALESCE(summary,'')
		FROM ai_lawsuits
		WHERE is_active IS NOT FALSE
		  AND COALESCE(executive_summary,'') = ''
		  AND jsonb_array_length(COALESCE(timeline,'[]'::jsonb)) >= 3
		  AND (details_last_try IS NULL OR details_last_try < current_date - 7)
		ORDER BY filed_date DESC NULLS LAST
		LIMIT $1`, limit)
	if err != nil {
		return fmt.Errorf("lawsuit_fill query: %w", err)
	}
	type rec struct {
		id                                              int64
		slug, name, court, filed, category, tl, snippet string
	}
	var recs []rec
	for rows.Next() {
		var r rec
		if rows.Scan(&r.id, &r.slug, &r.name, &r.court, &r.filed, &r.category, &r.tl, &r.snippet) == nil {
			recs = append(recs, r)
		}
	}
	rows.Close()

	written, rejected, failed := 0, 0, 0
	for _, r := range recs {
		var tl []fillTL
		json.Unmarshal([]byte(r.tl), &tl)
		var sb strings.Builder
		for i, e := range tl {
			if i >= 40 {
				break
			}
			sb.WriteString(e.Date + "  " + trunc(e.Title, 320) + "\n")
		}
		record := fmt.Sprintf("CAPTION: %s\nCOURT: %s\nFILED: %s\nDOCKET ENTRIES (the clerk's words, newest first):\n%s",
			r.name, r.court, r.filed, sb.String())
		if r.snippet != "" {
			record += "\nSEARCH SNIPPET: " + trunc(r.snippet, 500) + "\n"
		}

		system := "You describe court records for a public lawsuit tracker. You are given a docket record and nothing else. " +
			"Write only what that record shows. Do not add background, allegations, amounts, outcomes, or anything you know about the parties from elsewhere. " +
			"If the record does not say what the claims are, say the record does not yet show them. Plain English, no markdown, no lists."
		user := "Write 3 to 4 sentences: who sued whom, in which court and when, what kind of claim the docket entries indicate, and where the case stands procedurally as of the newest entry. " +
			"Use only names, dates and numbers that appear in the record below. No more than 5 sentences.\n\n" + record

		db.Exec(`UPDATE ai_lawsuits SET details_last_try = current_date WHERE id = $1`, r.id)
		draft, model, gerr := twoaiGenerate("lawsuit_fill", system, user)
		if gerr != nil {
			failed++
			fmt.Fprintf(os.Stderr, "twoai_lawsuit_fill: %s: %v\n", r.slug, gerr)
			continue
		}
		draft = strings.TrimSpace(draft)
		if len(draft) < 120 {
			rejected++
			db.Exec(`UPDATE ai_lawsuits SET details_note = 'draft too short' WHERE id = $1`, r.id)
			continue
		}

		// The check. Everything specific in the draft must be in the record.
		src := strings.ToLower(record)
		bad := ""
		for _, n := range fillNumRe.FindAllString(draft, -1) {
			if !strings.Contains(src, strings.ToLower(strings.TrimRight(n, ".,"))) {
				bad = n
				break
			}
		}
		if bad == "" {
			for _, loc := range fillNameRe.FindAllStringIndex(draft, -1) {
				w := draft[loc[0]:loc[1]]
				// A capital at the start of a sentence is grammar, not a name.
				if loc[0] == 0 || (loc[0] >= 2 && (draft[loc[0]-2] == '.' || draft[loc[0]-1] == '\n')) {
					if !strings.Contains(src, strings.ToLower(w)) {
						continue
					}
				}
				if fillCommon.MatchString(w) {
					continue
				}
				if !strings.Contains(src, strings.ToLower(w)) {
					bad = w
					break
				}
			}
		}
		if bad != "" {
			rejected++
			note := "rejected: draft names " + bad + ", which is not in the docket record"
			db.Exec(`UPDATE ai_lawsuits SET details_note = $2 WHERE id = $1`, r.id, note)
			fmt.Fprintf(os.Stderr, "twoai_lawsuit_fill: %s: %s\n", r.slug, note)
			continue
		}

		// Claim family. A case with no family yet may be given one, and only one
		// the record itself shows. A case that already HAS a family is never
		// touched.
		//
		// The first version of this treated "copyright" as a meaningless default,
		// because intelPromote used to stamp it on every new row, and moved any
		// copyright case whose docket titles did not say the word to
		// unclassified. Docket titles are summonses and scheduling orders; they
		// rarely name the claim. On its first run, 2026-09-18, that demoted 45
		// correctly labelled cases, Getty Images v. Stability AI and Disney v.
		// MiniMax among them. They were restored the same hour from the labels
		// still on the live site. Absence of a word in a docket is not evidence
		// about the claim, so it no longer removes anything.
		cat := r.category
		if cat == "" || cat == "unclassified" {
			cat = "unclassified"
			for _, c := range lawsuitCatOrder {
				if lawsuitCatSignals[c].MatchString(record) {
					cat = c
					break
				}
			}
		}
		why := lawsuitWhy[cat]

		if _, err := db.Exec(`UPDATE ai_lawsuits
			SET executive_summary = $2, why_it_matters = NULLIF($3,''), category = $4,
			    details_model = $5, details_written_on = current_date, details_note = NULL, updated_at = now()
			WHERE id = $1 AND COALESCE(executive_summary,'') = ''`,
			r.id, draft, why, cat, model); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "twoai_lawsuit_fill: %s: write: %v\n", r.slug, err)
			continue
		}
		written++
		fmt.Printf("twoai_lawsuit_fill: %s <- %s (%d docket entries, %s)\n", r.slug, model, len(tl), cat)
		time.Sleep(400 * time.Millisecond)
	}
	fmt.Printf("twoai_lawsuit_fill: written=%d rejected=%d failed=%d of %d cases this run\n", written, rejected, failed, len(recs))
	return nil
}
