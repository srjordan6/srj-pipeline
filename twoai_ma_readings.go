package main

// twoai_ma_reading: what an 8-K actually says, in three paragraphs, instead of
// a link nobody can open.
//
// Stephen, 2026-09-20, looking at the M&A list: this takes us to a link that
// nobody can read. He was right, and it was worse than it looked. The link on
// each row was the EDGAR ARCHIVE DIRECTORY, not the filing: a bare listing of
// twelve files with names like crwv-20260807.htm, ex101creditagreement.htm and
// 0001769628-26-000357-xbrl.zip. A reader who clicks it has to guess which one
// is the document, and nothing on the page tells them.
//
// The row said even less than the link. "2026-08-10 CoreWeave, items
// 1.01,2.03,7.01,9.01" tells a reader who already knows the item codes that
// CoreWeave signed something and took on debt. It does not say what, how much,
// from whom or why. That row was for a $2.6 billion delayed draw term loan
// from JPMorgan and MUFG to buy GPU servers, maturing September 2031. None of
// that was on the site.
//
// WHAT THIS DOES. For each filing: find the primary document through EDGAR's
// index.json rather than guessing a filename, extract the Item sections, and
// have Ollama write three paragraphs. What was agreed and with whom. What it
// costs and on what terms. Why it matters for AI infrastructure. The link goes
// at the bottom, and it points at the document rather than the directory.
//
// AN 8-K IS THE RIGHT DOCUMENT FOR A MODEL TO READ and the numbers are the
// reason. The Item sections are short, factual, and written to a legal
// template: amount, counterparty, rate, maturity, security. There is no
// narrative to misread. The prompt therefore forbids inference entirely: if
// the filing does not state a figure, the summary does not carry one.
//
// ONE FILING IS READ ONCE, EVER. An 8-K describes an event on a date; it does
// not change afterwards. The reading is stored against the accession number
// and never regenerated, which is the opposite of the defect removed from
// twoai_enacted_laws on 2026-09-19, where ten published pages were rewritten
// daily with different words.
//
// EDGAR'S FAIR ACCESS RULES are not optional: a descriptive User-Agent with a
// contact address, and no more than ten requests a second. This stage is far
// under that, at two requests per filing with a pause between.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const edgarUA = "theworldofai.org research (info@srjconsultingservices.com)"

// The item codes that carry substance. A filing of only 7.01 or 9.01 is a
// press release or an exhibit list and is not worth a model call.
var maSubstantiveItems = []string{"1.01", "1.02", "2.01", "2.03", "3.02", "5.02", "8.01"}

func twoaiMAReadingEnsure(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_ma_readings (
		accession text PRIMARY KEY,
		cik text NOT NULL,
		company text NOT NULL,
		filed date,
		items text,
		doc_url text,            -- the PRIMARY DOCUMENT, not the directory
		reading text,            -- three paragraphs
		model text,
		chars int,               -- how much filing text the model was given
		generated_on date,
		attempts int NOT NULL DEFAULT 0,
		last_note text,
		updated_at timestamptz NOT NULL DEFAULT now())`)
	return err
}

func edgarGet(client *http.Client, url string) ([]byte, int, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", edgarUA)
	// NO Accept-Encoding HEADER. Setting it by hand turns off Go's transparent
	// gzip handling: the transport only decompresses a response it asked for
	// itself. Found by the first live test against EDGAR, where every body came
	// back starting 0x1f 0x8b and index.json failed to parse. Leaving the
	// header off means the transport sets gzip, asks for it, and unwraps it.
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return b, resp.StatusCode, nil
}

// twoaiMAPrimaryDoc asks EDGAR which file is the filing. Guessing the name
// from the accession number works until it does not: the naming varies by
// filer agent, and the exhibits are often far larger than the document, so
// picking the biggest file would return a credit agreement rather than the
// 8-K that describes it.
func twoaiMAPrimaryDoc(client *http.Client, dirURL string) (string, error) {
	base := strings.TrimSuffix(dirURL, "/") + "/"
	body, code, err := edgarGet(client, base+"index.json")
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("index.json returned %d", code)
	}
	var d struct {
		Directory struct {
			Item []struct {
				Name string `json:"name"`
				Size string `json:"size"`
			} `json:"item"`
		} `json:"directory"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return "", err
	}
	// The primary document is an .htm that is not an exhibit, not an index and
	// not an XBRL sidecar. Exhibits are named ex\d+ by convention across
	// filer agents; the index files carry the accession number.
	for _, it := range d.Directory.Item {
		n := strings.ToLower(it.Name)
		if !strings.HasSuffix(n, ".htm") && !strings.HasSuffix(n, ".html") {
			continue
		}
		if strings.HasPrefix(n, "ex") || strings.Contains(n, "-index") ||
			strings.Contains(n, "_htm.xml") || strings.HasPrefix(n, "r") && strings.Contains(n, ".htm") && len(n) < 8 {
			continue
		}
		return base + it.Name, nil
	}
	return "", fmt.Errorf("no primary document among %d files", len(d.Directory.Item))
}

var maTagRe = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>`)
var maBlockRe = regexp.MustCompile(`(?i)</(p|div|tr|td|h[1-6]|li)>`)
var maAnyTagRe = regexp.MustCompile(`<[^>]+>`)
var maItemRe = regexp.MustCompile(`(?i)\bItem\s+\d\.\d\d\b`)

// twoaiMAItemText reduces the filing to the part that says something. The
// cover page is a form: address, phone number, ticker, a column of empty
// checkboxes. Everything before the first Item heading is dropped, which on
// the CoreWeave filing of 2026-08-07 removed 2,400 characters of boilerplate
// from a 9,000 character document.
func twoaiMAItemText(raw []byte) string {
	t := maTagRe.ReplaceAllString(string(raw), " ")
	t = maBlockRe.ReplaceAllString(t, "\n")
	t = maAnyTagRe.ReplaceAllString(t, " ")
	t = html.UnescapeString(t)
	t = strings.ReplaceAll(t, "\u00a0", " ")
	t = regexp.MustCompile(`[ \t]+`).ReplaceAllString(t, " ")
	t = regexp.MustCompile(`\n\s*\n+`).ReplaceAllString(t, "\n")
	t = strings.TrimSpace(t)
	if loc := maItemRe.FindStringIndex(t); loc != nil {
		t = t[loc[0]:]
	}
	// The signature block adds nothing and is the same on every filing.
	for _, cut := range []string{"SIGNATURES", "SIGNATURE\n", "Pursuant to the requirements of the Securities Exchange Act"} {
		if i := strings.Index(t, cut); i > 400 {
			t = t[:i]
		}
	}
	if len(t) > 14000 {
		t = t[:14000]
	}
	return strings.ToValidUTF8(t, "")
}

const maReadingSystem = `You read one SEC Form 8-K and explain it to a reader who follows AI infrastructure but is not a lawyer or a banker. Your output is published on a reference site, so it must be exact.

ABSOLUTE RULES:
- Use ONLY the filing text supplied. Never add background, history, market context or anything you know about the company from elsewhere.
- Every figure, date, rate, counterparty and party name must appear in the filing. If the filing does not state an amount, do not give one and do not estimate.
- Do not speculate about motive, strategy or what happens next beyond what the filing itself states.
- Plain English. Expand the jargon the filing uses: say what a delayed draw term loan is, what SOFR plus a margin means in practice, what it means that obligations are secured by substantially all assets.
- No hyphens used as dashes. Use commas or full stops.

Write EXACTLY three paragraphs, separated by a blank line, no headings and no bullet points:

1. WHAT WAS AGREED. The transaction, the parties by name, and the date. Who is borrowing or buying, from whom, and through which entity if a subsidiary is involved.
2. THE TERMS. The amounts, the interest rate, the fees, the maturity, what is pledged as security, and any guarantee. Give the numbers the filing gives.
3. WHY IT MATTERS. What the money or the agreement is for, as the filing states it, and what that tells a reader about AI infrastructure spending. Stay inside the filing.

If the filing text carries no substantive Item section, output exactly: NOTHING`

func twoaiMAReadingParse(reply string) (string, string) {
	s := strings.TrimSpace(reply)
	if s == "" {
		return "", "empty reply"
	}
	if strings.HasPrefix(strings.ToUpper(s), "NOTHING") {
		return "", "nothing"
	}
	if isRefusal(s) {
		return "", "model refused"
	}
	// A local model often opens with "Here are three paragraphs:".
	var keep []string
	for _, p := range regexp.MustCompile(`\n\s*\n`).Split(s, -1) {
		p = strings.TrimSpace(p)
		if p == "" || len(strings.Fields(p)) < 12 {
			continue
		}
		if strings.HasPrefix(p, "#") || strings.HasSuffix(p, ":") {
			continue
		}
		keep = append(keep, p)
	}
	if len(keep) < 3 {
		return "", fmt.Sprintf("wanted three paragraphs, got %d", len(keep))
	}
	if len(keep) > 3 {
		keep = keep[:3]
	}
	out := strings.Join(keep, "\n\n")
	if isRefusal(out) {
		return "", "model refused inside the text"
	}
	return out, ""
}

func twoaiMAReadings(db *sql.DB) error {
	if err := twoaiMAReadingEnsure(db); err != nil {
		return err
	}
	if twoaiLLMFor("ma_readings") != "ollama" {
		fmt.Println("twoai_ma_readings: stage is not routed to ollama, skipping")
		return nil
	}
	limit := 12
	if v := strings.TrimSpace(os.Getenv("TWOAI_MA_LIMIT")); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	// Newest first, substantive items only, three tries then left alone.
	any := "%" + strings.Join(maSubstantiveItems, "%|%") + "%"
	_ = any
	rows, err := db.Query(`
		SELECT f.accession, f.cik, f.company, f.filed, COALESCE(f.items,''), f.doc_url
		FROM twoai_ma_filings f
		LEFT JOIN twoai_ma_readings r ON r.accession = f.accession
		WHERE (r.accession IS NULL OR (r.reading IS NULL AND r.attempts < 3))
		  AND (f.items ~ '1\.01|1\.02|2\.01|2\.03|3\.02|5\.02|8\.01')
		ORDER BY f.filed DESC
		LIMIT $1`, limit)
	if err != nil {
		return err
	}
	type job struct {
		// filed is a STRING, not a time.Time. twoai_ma_filings.filed is a text
		// column holding 'YYYY-MM-DD'. Declaring it as time.Time made every
		// Scan fail, and because a row is only appended when Scan returns nil,
		// all 351 filings were discarded without a word: the first two runs
		// reported written=0 nothing=0 failed=0 and read like an empty queue
		// rather than a broken one. A silent skip on scan error is what made a
		// one-line type bug look like a working stage with nothing to do.
		acc, cik, company, items, dir, filed string
	}
	var jobs []job
	scanFailed := 0
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.acc, &j.cik, &j.company, &j.filed, &j.items, &j.dir); err != nil {
			// Counted and reported. A row this stage cannot read is a gap, and a
			// gap that says nothing is the defect above.
			scanFailed++
			if scanFailed == 1 {
				fmt.Fprintf(os.Stderr, "twoai_ma_readings: cannot read filing rows: %v\n", err)
			}
			continue
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if scanFailed > 0 {
		fmt.Fprintf(os.Stderr, "twoai_ma_readings: %d filing row(s) could not be read at all\n", scanFailed)
	}

	client := &http.Client{Timeout: 45 * time.Second}
	written, nothing, failed := 0, 0, 0
	fail := func(j job, note string) {
		db.Exec(`INSERT INTO twoai_ma_readings (accession, cik, company, filed, items, attempts, last_note)
			VALUES ($1,$2,$3,$4,$5,1,$6)
			ON CONFLICT (accession) DO UPDATE SET attempts = twoai_ma_readings.attempts + 1,
				last_note = $6, updated_at = now()`,
			j.acc, j.cik, j.company, j.filed, j.items, note)
	}
	for _, j := range jobs {
		doc, err := twoaiMAPrimaryDoc(client, j.dir)
		if err != nil {
			failed++
			fail(j, "primary doc: "+err.Error())
			continue
		}
		time.Sleep(400 * time.Millisecond)
		raw, code, err := edgarGet(client, doc)
		if err != nil || code != 200 {
			failed++
			fail(j, fmt.Sprintf("fetch %s returned %d", doc, code))
			continue
		}
		text := twoaiMAItemText(raw)
		if len(text) < 400 {
			nothing++
			fail(j, "no substantive item text")
			continue
		}
		user := "Company: " + j.company + "\nFiled: " + j.filed +
			"\nItems reported: " + j.items + "\n\nFILING TEXT:\n" + text
		reply, model, gerr := twoaiGenerate("ma_readings", maReadingSystem, user)
		if gerr != nil {
			fmt.Fprintf(os.Stderr, "twoai_ma_readings: %s: %v\n", j.acc, gerr)
			if strings.Contains(gerr.Error(), "marked down") || strings.Contains(gerr.Error(), "unreachable") {
				fmt.Fprintf(os.Stderr, "twoai_ma_readings: ollama is down, %d filing(s) left for the next run\n",
					len(jobs)-written-nothing-failed)
				break
			}
			failed++
			fail(j, "generate: "+gerr.Error())
			continue
		}
		reading, why := twoaiMAReadingParse(reply)
		if reading == "" {
			if why == "nothing" {
				nothing++
			} else {
				failed++
			}
			fail(j, why)
			continue
		}
		if _, err := db.Exec(`INSERT INTO twoai_ma_readings
			(accession, cik, company, filed, items, doc_url, reading, model, chars, generated_on, attempts, last_note)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,current_date,0,NULL)
			ON CONFLICT (accession) DO UPDATE SET doc_url=$6, reading=$7, model=$8, chars=$9,
				generated_on=current_date, attempts=0, last_note=NULL, updated_at=now()`,
			j.acc, j.cik, j.company, j.filed, j.items, doc, reading, model, len(text)); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_ma_readings store:", err)
			failed++
			continue
		}
		written++
		fmt.Printf("twoai_ma_readings: %s %s <- %s (%d chars of filing)\n",
			j.filed, j.company, model, len(text))
		time.Sleep(700 * time.Millisecond)
	}
	var have, total int
	db.QueryRow(`SELECT count(*) FROM twoai_ma_readings WHERE reading IS NOT NULL`).Scan(&have)
	db.QueryRow(`SELECT count(*) FROM twoai_ma_filings WHERE items ~ '1\.01|1\.02|2\.01|2\.03|3\.02|5\.02|8\.01'`).Scan(&total)
	fmt.Printf("twoai_ma_readings: written=%d nothing=%d failed=%d | %d of %d substantive filings read\n",
		written, nothing, failed, have, total)
	return nil
}
