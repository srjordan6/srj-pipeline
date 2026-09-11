package main

// appsec_research: license gate and full-text retrieval for the AI
// application security research library (srj_appsec_research), the source
// base for Volume VIII.
//
// Stephen, 2026-09-11: "I need this working." The job that used to do this
// ran from a session, not from here, and a status report that day listed
// what it had not done: the gate half ran and the retrieval half never
// fetched a single paper; publisher rows sat in 'pending' and were re-examined
// every run forever; and 43 rows added after the job took its snapshot were
// never looked at, because it read its queue once and did not re-scan. This
// stage replaces it, in the daily pipeline, with the three fixes built in.
//
// RE-SCANS EVERY RUN. There is no snapshot. Every run selects whatever is
// currently license='unresolved' and whatever is permitted and still
// unfetched. A row added five minutes before the run is handled by that run.
//
// TERMINAL STATES. A licence that does not permit full text moves the row to
// full_text_status='metadata_only' and it is never examined again. The queue
// drains instead of cycling.
//
// LICENCE SOURCE. These are arXiv papers, and for a paper posted this month
// the only place its licence exists is arXiv's own record: OpenAlex indexes a
// new preprint days to weeks later and, checked 2026-09-11, had none of the
// 43 September papers. So the licence is read from the arXiv abstract page
// (the machine-readable OAI-PMH endpoint returned empty for the same ids the
// same day). This is the research library for a book, not the public site's
// research watch; the 2026-09-11 decision to run that watch on OpenAlex only
// stands and is not affected. arXiv's terms permit this access; requests are
// spaced at their requested three seconds.
//
// THE GATE. Permitted: CC0, CC BY, CC BY-SA, CC BY-NC, CC BY-NC-SA. NC-SA
// was the judgement call in the report - the old job rejected it as a
// "combined modifier", which cost five papers - and is ruled PERMITTED here:
// share-alike governs redistribution of derivatives, and storing a paper's
// text to inform a book is neither. ND is correctly rejected, as is arXiv's
// own non-exclusive licence (it grants arXiv distribution rights, not us) and
// anything marked publisher. Every licence is normalised to one short code
// in `license` with the URL in `license_url` and the original string kept in
// `license_raw`, so a GROUP BY works and nothing is lost.
//
// RETRIEVAL. A permitted, unfetched row gets its PDF from arxiv.org/pdf and
// its text via pdftotext (Poppler), which must be on PATH. If it is not, the
// row keeps its pending status with the reason in full_text_error and the
// log says so every run, by count, rather than a silent zero. Attribution is
// written on every fetched row: title, authors, year, arXiv id, licence.

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var appsecLicRe = regexp.MustCompile(`href="(https?://(?:creativecommons\.org|arxiv\.org/licenses)[^"]+)"`)

func appsecLicenseCode(url, raw string) string {
	s := strings.ToLower(url)
	if s == "" {
		s = strings.ToLower(raw)
	}
	switch {
	case strings.Contains(s, "publicdomain/zero") || strings.HasPrefix(strings.ToLower(raw), "cc0"):
		return "cc0"
	case strings.Contains(s, "/by-nc-nd/") || strings.HasPrefix(strings.ToLower(raw), "cc by-nc-nd"):
		return "cc-by-nc-nd"
	case strings.Contains(s, "/by-nc-sa/") || strings.HasPrefix(strings.ToLower(raw), "cc by-nc-sa"):
		return "cc-by-nc-sa"
	case strings.Contains(s, "/by-nd/") || strings.HasPrefix(strings.ToLower(raw), "cc by-nd"):
		return "cc-by-nd"
	case strings.Contains(s, "/by-nc/") || strings.HasPrefix(strings.ToLower(raw), "cc by-nc"):
		return "cc-by-nc"
	case strings.Contains(s, "/by-sa/") || strings.HasPrefix(strings.ToLower(raw), "cc by-sa"):
		return "cc-by-sa"
	case strings.Contains(s, "/licenses/by/") || strings.HasPrefix(strings.ToLower(raw), "cc by"):
		return "cc-by"
	case strings.Contains(s, "nonexclusive-distrib") || strings.HasPrefix(strings.ToLower(raw), "arxiv perpetual"):
		return "arxiv-nonexclusive"
	case strings.ToLower(raw) == "publisher":
		return "publisher"
	}
	return "unresolved"
}

func appsecPermitted(code string) bool {
	switch code {
	case "cc0", "cc-by", "cc-by-sa", "cc-by-nc", "cc-by-nc-sa":
		return true
	}
	return false
}

// twoaiFindPdftotext locates Poppler's pdftotext.
//
// PATH alone is not enough, and 2026-09-11 showed both reasons in one
// evening. winget installs Poppler USER-SCOPED, under the user's
// AppData\Local\Microsoft\WinGet\Packages, and puts its shims in a Links
// directory that a freshly opened shell still did not have on PATH. And the
// pipeline's scheduled runs execute as SYSTEM, which never sees a
// user-scoped PATH at all - so even a shell where pdftotext works proves
// nothing about the nightly run.
//
// So: an explicit override first, then PATH, then the places winget and the
// common installers actually put it. POPPLER_BIN in pipeline.env pins it if
// this ever needs to be certain.
func twoaiFindPdftotext() (string, error) {
	if p := strings.TrimSpace(os.Getenv("POPPLER_BIN")); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
		cand := filepath.Join(p, "pdftotext.exe")
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("pdftotext"); err == nil {
		return p, nil
	}
	var roots []string
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, `AppData\Local\Microsoft\WinGet\Packages`))
	}
	// The pipeline may run as SYSTEM; look in every user profile too, because
	// a user-scoped install is still the install we need to use.
	if users, err := os.ReadDir(`C:\Users`); err == nil {
		for _, u := range users {
			if u.IsDir() {
				roots = append(roots, filepath.Join(`C:\Users`, u.Name(), `AppData\Local\Microsoft\WinGet\Packages`))
			}
		}
	}
	roots = append(roots, `C:\Program Files\poppler`, `C:\Program Files (x86)\poppler`, `C:\poppler`)
	for _, root := range roots {
		var found string
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || found != "" {
				return nil
			}
			if !d.IsDir() && strings.EqualFold(d.Name(), "pdftotext.exe") {
				found = p
			}
			return nil
		})
		if found != "" {
			return found, nil
		}
	}
	return "", fmt.Errorf("pdftotext not found on PATH or in the usual install locations; set POPPLER_BIN in pipeline.env to its full path")
}

func appsecResearch(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE srj_appsec_research
		ADD COLUMN IF NOT EXISTS license_url text, ADD COLUMN IF NOT EXISTS license_raw text,
		ADD COLUMN IF NOT EXISTS license_resolved_at timestamptz, ADD COLUMN IF NOT EXISTS full_text_chars int,
		ADD COLUMN IF NOT EXISTS full_text_fetched_at timestamptz, ADD COLUMN IF NOT EXISTS full_text_error text`); err != nil {
		return err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	get := func(url string) ([]byte, int, error) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("User-Agent", "SRJ-Consulting-research/1.0 (srjconsultingservices.com)")
		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 40<<20))
		return b, resp.StatusCode, err
	}

	// 1. Resolve every unresolved licence - re-scanned every run.
	rows, err := db.Query(`SELECT id, arxiv_id, COALESCE(license_raw, license, '') FROM srj_appsec_research
		WHERE license='unresolved' AND arxiv_id IS NOT NULL ORDER BY id`)
	if err != nil {
		return err
	}
	type pend struct {
		id       int
		arxiv, raw string
	}
	var todo []pend
	for rows.Next() {
		var p pend
		if rows.Scan(&p.id, &p.arxiv, &p.raw) == nil {
			todo = append(todo, p)
		}
	}
	rows.Close()
	resolved, unresolved := 0, 0
	for _, p := range todo {
		page, code, err := get("https://arxiv.org/abs/" + p.arxiv)
		time.Sleep(3 * time.Second)
		if err != nil || code != 200 {
			unresolved++
			continue
		}
		m := appsecLicRe.FindSubmatch(page)
		if m == nil {
			unresolved++
			continue
		}
		url := string(m[1])
		lc := appsecLicenseCode(url, "")
		db.Exec(`UPDATE srj_appsec_research SET license=$1, license_url=$2, license_raw=COALESCE(license_raw,$2),
			license_resolved_at=now() WHERE id=$3`, lc, url, p.id)
		resolved++
	}

	// 2. Normalise anything still carrying a raw string, then apply the
	//    gate and make rejections terminal.
	db.Exec(`UPDATE srj_appsec_research SET license_raw = license WHERE license_raw IS NULL`)
	if _, err := db.Exec(`UPDATE srj_appsec_research SET
		full_text_permitted = (license IN ('cc0','cc-by','cc-by-sa','cc-by-nc','cc-by-nc-sa')),
		full_text_status = CASE
			WHEN license IN ('cc0','cc-by','cc-by-sa','cc-by-nc','cc-by-nc-sa') THEN CASE WHEN full_text IS NOT NULL THEN 'fetched' ELSE 'pending' END
			WHEN license = 'unresolved' THEN 'pending'
			ELSE 'metadata_only' END,
		attribution = COALESCE(attribution, CASE WHEN license IN ('cc0','cc-by','cc-by-sa','cc-by-nc','cc-by-nc-sa')
			THEN title || '. ' || COALESCE(authors,'') || CASE WHEN pub_year IS NOT NULL THEN ' (' || pub_year || ')' ELSE '' END ||
				'. arXiv:' || COALESCE(arxiv_id,'') || '. Licensed ' || upper(license) || ', ' || COALESCE(license_url,'') ELSE NULL END)`); err != nil {
		return err
	}

	// 3. Fetch full text for every permitted, unfetched row.
	pdftotext, ptErr := twoaiFindPdftotext()
	frows, err := db.Query(`SELECT id, arxiv_id FROM srj_appsec_research
		WHERE full_text_permitted AND full_text_status='pending' AND arxiv_id IS NOT NULL ORDER BY id`)
	if err != nil {
		return err
	}
	var fetch []pend
	for frows.Next() {
		var p pend
		if frows.Scan(&p.id, &p.arxiv) == nil {
			fetch = append(fetch, p)
		}
	}
	frows.Close()
	fetched, failed := 0, 0
	if len(fetch) > 0 && ptErr != nil {
		db.Exec(`UPDATE srj_appsec_research SET full_text_error=$1
			WHERE full_text_permitted AND full_text_status='pending'`, ptErr.Error())
		fmt.Printf("appsec_research: %d permitted rows await full text but pdftotext could not be located: %v\n", len(fetch), ptErr)
	} else {
		tmp := filepath.Join(os.TempDir(), "appsec")
		os.MkdirAll(tmp, 0o755)
		for _, p := range fetch {
			url := "https://arxiv.org/pdf/" + p.arxiv
			pdf, code, err := get(url)
			time.Sleep(3 * time.Second)
			if err != nil || code != 200 || len(pdf) < 1000 {
				db.Exec(`UPDATE srj_appsec_research SET full_text_error=$1, source_pdf_url=$2 WHERE id=$3`,
					fmt.Sprintf("pdf fetch: http %d %v", code, err), url, p.id)
				failed++
				continue
			}
			pf := filepath.Join(tmp, fmt.Sprintf("%d.pdf", p.id))
			tf := filepath.Join(tmp, fmt.Sprintf("%d.txt", p.id))
			os.WriteFile(pf, pdf, 0o644)
			cmd := exec.Command(pdftotext, "-layout", "-enc", "UTF-8", pf, tf)
			if out, err := cmd.CombinedOutput(); err != nil {
				db.Exec(`UPDATE srj_appsec_research SET full_text_error=$1, source_pdf_url=$2 WHERE id=$3`,
					"pdftotext: "+strings.TrimSpace(string(out))+" "+err.Error(), url, p.id)
				failed++
				continue
			}
			txtB, _ := os.ReadFile(tf)
			txt := strings.TrimSpace(string(txtB))
			os.Remove(pf)
			os.Remove(tf)
			if len(txt) < 2000 {
				db.Exec(`UPDATE srj_appsec_research SET full_text_error='extracted text under 2000 chars; likely a scanned or image PDF', source_pdf_url=$1 WHERE id=$2`, url, p.id)
				failed++
				continue
			}
			if _, err := db.Exec(`UPDATE srj_appsec_research SET full_text=$1, full_text_chars=$2, full_text_status='fetched',
				full_text_fetched_at=now(), source_pdf_url=$3, full_text_error=NULL,
				read_depth=CASE WHEN read_depth='abstract' THEN 'full' ELSE read_depth END WHERE id=$4`,
				txt, len(txt), url, p.id); err != nil {
				fmt.Fprintln(os.Stderr, "appsec_research: store", p.id, err)
				failed++
				continue
			}
			fetched++
		}
	}

	var total, permitted, withText, metaOnly, unres int
	db.QueryRow(`SELECT count(*), count(*) FILTER (WHERE full_text_permitted), count(*) FILTER (WHERE full_text IS NOT NULL),
		count(*) FILTER (WHERE full_text_status='metadata_only'), count(*) FILTER (WHERE license='unresolved') FROM srj_appsec_research`).
		Scan(&total, &permitted, &withText, &metaOnly, &unres)
	fmt.Printf("appsec_research: licences resolved=%d unresolved_after=%d | fetched=%d failed=%d | total=%d permitted=%d with_text=%d metadata_only=%d ok=%v\n",
		resolved, unres, fetched, failed, total, permitted, withText, metaOnly, unresolved == 0 && failed == 0 && ptErr == nil)
	// A run where every permitted row failed for one environmental reason is a
	// FAILED run, not a quiet one. exit=0 on a run that fetched nothing is how
	// 2026-09-11 looked fine in the log and had done nothing: the reason was
	// only visible by querying the table. Now the run says so itself.
	if ptErr != nil && len(fetch) > 0 {
		return fmt.Errorf("%d permitted rows could not be fetched: %w", len(fetch), ptErr)
	}
	return nil
}
