package main

// twoai_enacted_laws: a compliance page for every AI law that passed.
//
// Stephen, 2026-09-17: on /ai-laws/ all we get are links to bills. We do not
// get the actual law and a summary of how it affects AI once it becomes law.
// Every passed bill needs a page under /ai-compliance/. Assign the task to
// Ollama.
//
// WHAT IT DOES. For every passed bill this site judged relevant, it fetches
// the enrolled text from LegiScan, hands the whole statute to the model with
// a fixed brief, and writes a compliance page: what the law does, who it
// applies to, when it takes effect, the obligations and prohibitions, the
// penalties and who enforces them, how it changes AI deployment in practice,
// and the exemptions - each with the section it comes from. The full text of
// the law sits at the foot of the page, because it is public record and a
// reader should be able to check every claim against it without leaving.
//
// WHAT MAKES IT SAFE TO PUBLISH UNREVIEWED. Three things. The model is given
// the statute and only the statute, and told that anything it cannot point to
// a section for is not to be stated. Every section cited is checked against
// the text before the page is written; a citation to a section that does not
// exist fails the bill. And the page states, in its own header, that the
// reading is a model's and the law is the law: the text is there to be
// checked, and the reading is labelled as a reading.
//
// WHEN IT RUNS. Daily. A bill is done once; it is redone only when LegiScan
// reports a new change_hash, which means the text or status changed. The
// page keeps the uid it was minted with, so a law amended next session keeps
// its URL.
//
// COST. About 30,000 input tokens per bill on Pro plus thinking, a few cents
// each; the 192 relevant passed bills come to under ten dollars, once.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type lawReading struct {
	WhatItDoes      string   `json:"what_it_does"`
	AppliesTo       []string `json:"who_it_applies_to"`
	EffectiveDate   string   `json:"effective_date"`
	KeyDefinitions  []string `json:"key_definitions"`
	Obligations     []string `json:"obligations"`
	Prohibitions    []string `json:"prohibitions"`
	Penalties       string   `json:"penalties_and_enforcement"`
	AffectsAI       []string `json:"how_it_affects_ai_deployment"`
	Exemptions      []string `json:"notable_exemptions"`
	ComplianceSteps []string `json:"compliance_steps"`
	SectionsCited   []string `json:"sections_cited"`
}

var sectionRe = regexp.MustCompile(`(?i)\b(?:sec(?:tion)?\.?|§)\s*([0-9][0-9A-Za-z.\-]*)`)

func twoaiEnactedLaws(db *sql.DB) error {
	key := os.Getenv("LEGISCAN_API_KEY")
	if key == "" {
		return fmt.Errorf("LEGISCAN_API_KEY not set")
	}
	limit := 25
	if v := os.Getenv("TWOAI_ENACTED_LAWS_LIMIT"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	// A statute needs a wide window. 131,072 drew a 500 from Ollama Cloud on
	// the first bill; 65,536 is what a long bill actually needs and is known
	// to be served. Set for this stage only unless the caller chose one.
	if os.Getenv("OLLAMA_NUM_CTX") == "" {
		os.Setenv("OLLAMA_NUM_CTX", "65536")
	}

	// Passed, relevant, and either never paged or changed since.
	rows, err := db.Query(`
		SELECT DISTINCT ON (d.raw->'bill'->>'state', d.raw->'bill'->>'bill_number')
		       d.external_id, d.change_hash, d.url,
		       d.raw->'bill'->>'state', d.raw->'bill'->>'bill_number', d.raw->'bill'->>'title',
		       COALESCE(d.raw->'bill'->>'status_date',''),
		       COALESCE(d.raw->'bill'->>'description',''),
		       d.raw->'bill'->'texts'
		FROM pipeline.documents d
		JOIN pipeline.sources s ON s.id = d.source_id AND s.key = 'legiscan'
		JOIN twoai_bill_events e ON e.state = d.raw->'bill'->>'state'
		                        AND e.bill_number = d.raw->'bill'->>'bill_number'
		                        AND e.relevant AND e.status = 4
		WHERE (d.raw->'bill'->>'status')::int = 4
		  AND jsonb_array_length(COALESCE(d.raw->'bill'->'texts','[]'::jsonb)) > 0
		  AND NOT EXISTS (
		      SELECT 1 FROM twoai_pages p
		      WHERE p.path = 'compliance/law-' || lower(d.raw->'bill'->>'state') || '-' ||
		                     lower(regexp_replace(d.raw->'bill'->>'bill_number','[^A-Za-z0-9]','','g')) || '.json'
		        AND p.data->>'change_hash' = d.change_hash)
		ORDER BY d.raw->'bill'->>'state', d.raw->'bill'->>'bill_number', d.raw->'bill'->>'status_date' DESC, d.change_hash DESC
		LIMIT $1`, limit)
	if err != nil {
		return err
	}
	type bill struct {
		id, hash, url, state, number, title, statusDate, desc string
		texts                                                 json.RawMessage
	}
	var bills []bill
	for rows.Next() {
		var b bill
		if rows.Scan(&b.id, &b.hash, &b.url, &b.state, &b.number, &b.title, &b.statusDate, &b.desc, &b.texts) == nil {
			bills = append(bills, b)
		}
	}
	rows.Close()
	if len(bills) == 0 {
		fmt.Println("twoai_enacted_laws: every relevant passed bill already has a current page")
		return nil
	}

	client := &http.Client{Timeout: 60 * time.Second}
	system := `You are writing a compliance reference page for theworldofai.org from the full text of an enacted law, which follows. Read the whole statute. Answer ONLY from it.

Return ONLY a JSON object, no prose, no markdown fences, with exactly these keys:
{
 "what_it_does": "two or three sentences, plain English",
 "who_it_applies_to": ["..."],
 "effective_date": "the date or condition in the text, or 'not stated in the text'",
 "key_definitions": ["term: definition as the statute gives it, with section"],
 "obligations": ["what a covered party must do, with section"],
 "prohibitions": ["what is forbidden, with section"],
 "penalties_and_enforcement": "who enforces, what the penalties are, private right of action or not, with sections",
 "how_it_affects_ai_deployment": ["concrete consequences for an organisation deploying AI, with section"],
 "notable_exemptions": ["...with section"],
 "compliance_steps": ["what a covered organisation would need to do, in order"],
 "sections_cited": ["every section number you relied on, e.g. 'Sec. 3', '§ 1798.100']
}

Rules. Every item in obligations, prohibitions, definitions, effects and exemptions ends with the section it comes from in parentheses. If the text does not say something, say 'not stated in the text' rather than inferring. Do not add background about other laws. Plain English, no hyphens in prose, use commas or periods.`

	made, failed := 0, 0
	for _, b := range bills {
		text, mime, docURL, err := legiscanLatestText(client, key, b.texts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "twoai_enacted_laws: %s %s: text: %v\n", b.state, b.number, err)
			failed++
			continue
		}
		if len(text) < 400 {
			fmt.Fprintf(os.Stderr, "twoai_enacted_laws: %s %s: text too short (%d chars, %s), skipping\n", b.state, b.number, len(text), mime)
			failed++
			continue
		}
		// Cap the prompt at roughly 40,000 tokens to stay inside the window
		// with room for the answer. A bill longer than that is NOT cut at the
		// cap: an omnibus puts its AI section wherever it lands, and CT HB
		// 05222, 308,000 characters ending "...And Artificial Intelligence",
		// got a reading about baby food and used cars because the AI provisions
		// were past the cut. So a long bill is read as its opening, which
		// carries the title and definitions, plus a window around every place
		// the AI vocabulary appears, up to the cap. The model is told what it
		// is looking at.
		const capChars = 160000
		prompt := text
		if len(prompt) > capChars {
			prompt = twoaiFocusStatute(text, capChars)
		}
		user := fmt.Sprintf("STATE: %s\nBILL: %s\nTITLE: %s\nSTATUS DATE: %s\n\nFULL TEXT OF THE ENACTED LAW:\n\n%s", b.state, b.number, b.title, b.statusDate, prompt)
		raw, model, err := twoaiGenerate("ENACTED_LAWS", system, user)
		if err != nil {
			fmt.Fprintf(os.Stderr, "twoai_enacted_laws: %s %s: %v\n", b.state, b.number, err)
			failed++
			continue
		}
		raw = strings.TrimSpace(raw)
		if i := strings.Index(raw, "{"); i >= 0 {
			if k := strings.LastIndex(raw, "}"); k > i {
				raw = raw[i : k+1]
			}
		}
		var r lawReading
		if err := json.Unmarshal([]byte(raw), &r); err != nil || strings.TrimSpace(r.WhatItDoes) == "" {
			fmt.Fprintf(os.Stderr, "twoai_enacted_laws: %s %s: unparseable: %s\n", b.state, b.number, truncate(raw, 160))
			failed++
			continue
		}
		// EVERY CITED SECTION MUST EXIST IN THE TEXT. This is the check that
		// makes an unreviewed reading publishable: a model that invents a
		// section number fails the bill rather than publishing the invention.
		lowText := strings.ToLower(text)
		bad := 0
		for _, s := range r.SectionsCited {
			n := sectionRe.FindStringSubmatch(s)
			if n == nil {
				continue
			}
			if !strings.Contains(lowText, strings.ToLower(n[1])) {
				bad++
			}
		}
		if bad > 0 && bad*3 > len(r.SectionsCited) {
			fmt.Fprintf(os.Stderr, "twoai_enacted_laws: %s %s: %d of %d cited sections not found in the text; rejected\n", b.state, b.number, bad, len(r.SectionsCited))
			failed++
			continue
		}

		slug := "law-" + strings.ToLower(b.state) + "-" + strings.ToLower(regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(b.number, ""))
		h8 := sha256.Sum256([]byte("compliance:" + slug))
		uid := hex.EncodeToString(h8[:4])
		body := renderLawPage(b.state, b.number, b.title, b.statusDate, b.url, docURL, model, r, text)
		rb, _ := json.Marshal(r)
		if _, err := db.Exec(`
			INSERT INTO twoai_pages (path, kind, taxonomy_slug, data, updated_at)
			VALUES ($1, 'compliance', 'enacted-ai-laws', jsonb_build_object(
				'uid', $2::text, 'page_uid', $2::text, 'slug', $3::text,
				'title', $4::text, 'state', $5::text, 'bill_number', $6::text,
				'status_date', $7::text, 'legiscan_url', $8::text, 'text_url', $9::text,
				'change_hash', $10::text, 'generated_by', $11::text,
				'generated', $12::text, 'verified', $12::text,
				'summary', $13::text, 'reading', $14::jsonb, 'body_html', $15::text,
				'law_text_chars', $16::int,
				'citations', jsonb_build_array(jsonb_build_object('author', $5::text || ' Legislature', 'year', left($7::text,4), 'journal', $5::text || ' ' || $6::text || ', enrolled text via LegiScan', 'quote', ''))
			), now())
			ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data, taxonomy_slug = EXCLUDED.taxonomy_slug, updated_at = now()`,
			"compliance/"+slug+".json", uid, slug,
			b.state+" "+b.number+": "+b.title, b.state, b.number, b.statusDate, b.url, docURL,
			b.hash, model, time.Now().UTC().Format("2006-01-02"),
			r.WhatItDoes, string(rb), body, len(text)); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_enacted_laws: store %s %s: %v\n", b.state, b.number, err)
			failed++
			continue
		}
		made++
		fmt.Printf("twoai_enacted_laws: %s %s <- %s (%d chars of statute, %d sections cited)\n", b.state, b.number, model, len(text), len(r.SectionsCited))
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("twoai_enacted_laws: pages=%d failed=%d of %d bills this run\n", made, failed, len(bills))
	return nil
}

// legiscanLatestText fetches the most recent text document for a bill and
// returns plain text. LegiScan returns the document base64 encoded; HTML is
// stripped, PDF goes through pdftotext.
func legiscanLatestText(client *http.Client, key string, texts json.RawMessage) (string, string, string, error) {
	var docs []struct {
		DocID int    `json:"doc_id"`
		Date  string `json:"date"`
		Type  string `json:"type"`
		Mime  string `json:"mime"`
		URL   string `json:"state_link"`
	}
	if err := json.Unmarshal(texts, &docs); err != nil || len(docs) == 0 {
		return "", "", "", fmt.Errorf("no text documents listed")
	}
	// Prefer the enrolled/chaptered text; else the latest by date.
	best := docs[0]
	for _, d := range docs {
		t := strings.ToLower(d.Type)
		if strings.Contains(t, "enrolled") || strings.Contains(t, "chaptered") || strings.Contains(t, "act") {
			best = d
			break
		}
		if d.Date > best.Date {
			best = d
		}
	}
	u := fmt.Sprintf("https://api.legiscan.com/?key=%s&op=getBillText&id=%d", key, best.DocID)
	resp, err := client.Get(u)
	if err != nil {
		return "", "", "", err
	}
	bb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var tp struct {
		Status string `json:"status"`
		Text   struct {
			Mime string `json:"mime"`
			Doc  string `json:"doc"`
		} `json:"text"`
	}
	if json.Unmarshal(bb, &tp) != nil || tp.Status != "OK" || tp.Text.Doc == "" {
		return "", "", "", fmt.Errorf("getBillText %d: %.160s", best.DocID, bb)
	}
	rawDoc, err := base64.StdEncoding.DecodeString(tp.Text.Doc)
	if err != nil {
		return "", "", "", err
	}
	mime := strings.ToLower(tp.Text.Mime)
	var text string
	switch {
	case strings.Contains(mime, "pdf"):
		cmd := exec.Command("pdftotext", "-layout", "-", "-")
		cmd.Stdin = strings.NewReader(string(rawDoc))
		out, err := cmd.Output()
		if err != nil {
			return "", mime, best.URL, fmt.Errorf("pdftotext: %w", err)
		}
		text = string(out)
	default:
		text = html.UnescapeString(regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>`).ReplaceAllString(string(rawDoc), " "))
		text = regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(text, " ")
	}
	text = strings.ToValidUTF8(strings.ReplaceAll(text, "\x00", ""), "")
	text = regexp.MustCompile(`[ \t]+`).ReplaceAllString(text, " ")
	text = regexp.MustCompile(`\n{3,}`).ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text), mime, best.URL, nil
}

func renderLawPage(state, number, title, statusDate, legiscanURL, textURL, model string, r lawReading, text string) string {
	esc := html.EscapeString
	var sb strings.Builder
	list := func(h string, items []string) {
		if len(items) == 0 {
			return
		}
		sb.WriteString("<h2>" + esc(h) + "</h2><ul>")
		for _, it := range items {
			sb.WriteString("<li>" + esc(it) + "</li>")
		}
		sb.WriteString("</ul>")
	}
	sb.WriteString("<p class=\"meta-line\">Passed " + esc(statusDate) + ". Reading generated by " + esc(model) + " from the enrolled text; the full text is at the foot of this page so every statement can be checked against it. <a href=\"" + esc(legiscanURL) + "\" rel=\"noopener\">LegiScan record</a>")
	if textURL != "" {
		sb.WriteString(" &middot; <a href=\"" + esc(textURL) + "\" rel=\"noopener\">official text</a>")
	}
	sb.WriteString("</p>")
	sb.WriteString("<h2>What it does</h2><p>" + esc(r.WhatItDoes) + "</p>")
	list("Who it applies to", r.AppliesTo)
	if strings.TrimSpace(r.EffectiveDate) != "" {
		sb.WriteString("<h2>Effective date</h2><p>" + esc(r.EffectiveDate) + "</p>")
	}
	list("Key definitions", r.KeyDefinitions)
	list("Obligations", r.Obligations)
	list("Prohibitions", r.Prohibitions)
	if strings.TrimSpace(r.Penalties) != "" {
		sb.WriteString("<h2>Penalties and enforcement</h2><p>" + esc(r.Penalties) + "</p>")
	}
	list("How it affects AI deployment", r.AffectsAI)
	list("Notable exemptions", r.Exemptions)
	list("Compliance steps", r.ComplianceSteps)
	sb.WriteString("<h2>Full text of the law</h2><p class=\"meta-line\">" + esc(state+" "+number) + ", " + fmt.Sprintf("%d", len(text)) + " characters, as enrolled. Public record.</p>")
	sb.WriteString("<details><summary>Show the full text</summary><pre style=\"white-space:pre-wrap;font-size:.85em\">" + esc(text) + "</pre></details>")
	return sb.String()
}

// twoaiFocusStatute reduces a long bill to its opening plus windows around
// every AI-relevant passage, within budget, so an omnibus is read where it
// matters rather than where it starts.
var statuteAIRe = regexp.MustCompile(`(?i)artificial intelligence|automated decision|algorithmic|machine learning|generative|chatbot|deepfake|synthetic media|large language model|automated system|frontier model|foundation model`)

func twoaiFocusStatute(text string, budget int) string {
	head := 40000
	if head > len(text) {
		head = len(text)
	}
	out := []string{text[:head]}
	used := head
	last := head
	const win = 6000
	for _, m := range statuteAIRe.FindAllStringIndex(text, -1) {
		start := m[0] - win/2
		if start < last {
			start = last
		}
		end := m[1] + win/2
		if end > len(text) {
			end = len(text)
		}
		if start >= end {
			continue
		}
		if used+(end-start) > budget {
			break
		}
		out = append(out, "\n\n[...]\n\n"+text[start:end])
		used += end - start
		last = end
	}
	return strings.Join(out, "") + fmt.Sprintf("\n\n[THIS BILL IS %d CHARACTERS. YOU HAVE BEEN GIVEN ITS OPENING AND EVERY PASSAGE THAT MENTIONS AI, AUTOMATED DECISIONS OR RELATED TERMS. READ FOR THE AI PROVISIONS; DESCRIBE THE REST ONLY AS CONTEXT.]", len(text))
}
