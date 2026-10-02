package main

// twoai_cve_write: a headline that says what the flaw lets an attacker do,
// and a "How to defend against it" section, for every published AI CVE.
//
// theworldofai, bridge row 366 (Stephen, 2026-10-02): under Latest AI CVEs
// each CVE needs a headline that describes it and makes the reader want to
// learn more, and every CVE page needs the course of action to defend
// against the vulnerability.
//
// The writer sees the CVE record only: description, product and vendor,
// CVSS score and vector, CWE, the KEV flag and date, NVD's affected ranges
// (versionEndExcluding is the first fixed version when NVD knows one) and
// the reference URLs with their tags. Nothing it says may go beyond that,
// so after the model answers the code checks: a headline under ninety
// characters with no question mark, "critical" only when CVSS says so,
// "exploited" only when KEV says so, a fixed version only when the record
// holds that version string, an advisory link only when it is one of the
// references. What fails the check is not stored, and the row is retried
// up to three times. The other two parts of the section are general
// security practice for the weakness class and the attack path, and the
// page labels them so.
//
// A written row carries the hash of the record it was written from, so a
// record NVD changes is rewritten and an unchanged one never is. Newest
// published first, a dozen a run (TWOAI_CVE_WRITE_PER_RUN), proposed rows
// skipped, Ollama only through twoaiGenerate, deferred in peak hours like
// the other backlog stages. No exploit detail is asked for or kept.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type cveDefense struct {
	Fix struct {
		Status      string `json:"status"` // fixed | none_recorded
		Version     string `json:"version,omitempty"`
		AdvisoryURL string `json:"advisory_url,omitempty"`
		Text        string `json:"text"`
	} `json:"fix"`
	UntilPatched []string `json:"until_patched"`
	Check        []string `json:"check"`
	KevOpen      string   `json:"kev_open,omitempty"`
	WrittenBy    string   `json:"written_by"`
	WrittenOn    string   `json:"written_on"`
}

var cveVersionRe = regexp.MustCompile(`\b\d+(?:\.\d+){1,3}\b`)

// cveWriteHash is the SQL expression for the record hash, so the select and
// the update agree on it to the byte.
const cveWriteHash = `md5(concat_ws('|', description, cvss_score::text, cvss_vector, cwe, kev::text, kev_added::text, references_json::text, last_modified::text))`

func twoaiCVEWrite(db *sql.DB) {
	perRun := 12
	if v := strings.TrimSpace(os.Getenv("TWOAI_CVE_WRITE_PER_RUN")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			perRun = n
		}
	}
	if perRun == 0 {
		return
	}
	rows, err := db.Query(`SELECT cve_id, coalesce(product,''), coalesce(vendor,''), coalesce(published::text,''), coalesce(description,''),
			cvss_score, coalesce(cvss_severity,''), coalesce(cvss_vector,''), coalesce(cwe,''), kev, coalesce(kev_added::text,''),
			coalesce(affected_json::text,''), coalesce(references_tagged::text,''), coalesce(references_json,'[]'::jsonb)::text, `+cveWriteHash+`
		FROM twoai_cves
		WHERE status IN ('published','approved') AND written_hash IS DISTINCT FROM `+cveWriteHash+` AND write_attempts < 3
		ORDER BY published DESC NULLS LAST LIMIT $1`, perRun)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_cve_write select:", err)
		return
	}
	type row struct {
		id, product, vendor, published, desc, sev, vec, cwe, kevAdded, affRaw, taggedRaw, refsRaw, hash string
		score                                                                                           sql.NullFloat64
		kev                                                                                             bool
	}
	var todo []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.product, &r.vendor, &r.published, &r.desc, &r.score, &r.sev, &r.vec, &r.cwe, &r.kev, &r.kevAdded, &r.affRaw, &r.taggedRaw, &r.refsRaw, &r.hash) == nil {
			todo = append(todo, r)
		}
	}
	rows.Close()
	written, held, fetched := 0, 0, 0
	for _, r := range todo {
		// Rows stored before 2026-10-02 have no affected ranges or tagged
		// references. One NVD call fills them in, once.
		affected := []cveAffected{}
		tagged := []cveRef{}
		if r.affRaw == "" || r.taggedRaw == "" {
			params := url.Values{}
			params.Set("cveId", r.id)
			var p nvdPage
			if err := nvdGet(params, &p); err == nil && len(p.Vulnerabilities) == 1 {
				affected, tagged = cveRecordExtras(p.Vulnerabilities[0].CVE)
				aj, _ := json.Marshal(affected)
				tj, _ := json.Marshal(tagged)
				db.Exec(`UPDATE twoai_cves SET affected_json=$2::jsonb, references_tagged=$3::jsonb WHERE cve_id=$1`, r.id, string(aj), string(tj))
				fetched++
				time.Sleep(6500 * time.Millisecond)
			}
		} else {
			json.Unmarshal([]byte(r.affRaw), &affected)
			json.Unmarshal([]byte(r.taggedRaw), &tagged)
		}
		if len(tagged) == 0 {
			refs := []string{}
			json.Unmarshal([]byte(r.refsRaw), &refs)
			for _, u := range refs {
				tagged = append(tagged, cveRef{URL: u, Tags: []string{}})
			}
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "CVE: %s\nProduct: %s\nVendor: %s\nPublished: %s\n", r.id, fallback(r.product, "not named"), fallback(r.vendor, "not named"), r.published)
		if r.score.Valid {
			fmt.Fprintf(&sb, "CVSS: %.1f %s, vector %s\n", r.score.Float64, r.sev, fallback(r.vec, "not given"))
		} else {
			sb.WriteString("CVSS: not yet scored by NVD\n")
		}
		fmt.Fprintf(&sb, "Weakness class: %s\n", fallback(r.cwe, "not given"))
		if r.kev {
			fmt.Fprintf(&sb, "CISA KEV (exploited in the wild): yes, added %s\n", fallback(r.kevAdded, "date not given"))
		} else {
			sb.WriteString("CISA KEV (exploited in the wild): no\n")
		}
		sb.WriteString("\nDescription as filed:\n" + r.desc + "\n\nAffected ranges from NVD:\n")
		if len(affected) == 0 {
			sb.WriteString("(none recorded)\n")
		}
		for _, a := range affected {
			fmt.Fprintf(&sb, "- %s", a.Criteria)
			if a.StartIncluding != "" {
				fmt.Fprintf(&sb, " from %s", a.StartIncluding)
			}
			if a.StartExcluding != "" {
				fmt.Fprintf(&sb, " after %s", a.StartExcluding)
			}
			if a.EndIncluding != "" {
				fmt.Fprintf(&sb, " up to and including %s", a.EndIncluding)
			}
			if a.EndExcluding != "" {
				fmt.Fprintf(&sb, " before %s (so %s is the first version not affected)", a.EndExcluding, a.EndExcluding)
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\nReferences:\n")
		for _, t := range tagged {
			fmt.Fprintf(&sb, "- %s [%s]\n", t.URL, strings.Join(t.Tags, ", "))
		}

		system := `You write for The World of AI, a reference site read by business leaders, developers and security teams. You are given one CVE record and nothing else. Use only what the record says.
Return only JSON of this shape, no prose around it:
{"headline": "...", "fix": {"status": "fixed" or "none_recorded", "version": "", "advisory_url": "", "text": "..."}, "until_patched": ["...", "..."], "check": ["...", "..."]}
HEADLINE: under 90 characters, plain words, says what an attacker can do and to what, naming the product and version where the record gives them. Example of the shape: "Langflow 1.8.4 lets a web request write files anywhere on the server". It must be literally true to the record: do not write critical unless the CVSS severity is CRITICAL, do not write exploited or in the wild unless KEV says yes, do not write patch now or update now unless a fixed version is recorded, no question marks, no clickbait.
FIX: status fixed only when the record names a fixed version (an affected range ending before a version, or a Patch or Vendor Advisory reference that states one); then version is that exact string and advisory_url is one of the reference URLs above, preferably one tagged Patch or Vendor Advisory, and text says in one sentence to upgrade to it. Otherwise status none_recorded, version and advisory_url empty, and text says plainly that no fixed version is recorded as of the last check and names where a fix would appear (the project's releases page or security advisories), without inventing a version.
UNTIL_PATCHED: two to four short compensating controls specific to the weakness class and the attack path in the record, written as general security practice for this kind of flaw, not as vendor guidance. For example, for an unauthenticated file write through an HTTP API: keep the API off the public internet, require authentication on it, run the service under an account that can only write to its own data directory, watch for files created outside it.
CHECK: one to three short items a reader can act on to tell whether they are exposed: the version to look for, the setting or endpoint to inspect.
Never include exploit detail, proof of concept, payloads or steps an attacker could reuse. Defence only. Short sentences, plain English, commas rather than dashes.`
		out, model, gerr := twoaiGenerate("cve_writer", system, sb.String())
		if gerr != nil {
			if strings.Contains(gerr.Error(), "deferred") {
				return
			}
			continue // not counted as an attempt, retried next run
		}
		text := strings.TrimSpace(out)
		if i := strings.Index(text, "{"); i >= 0 {
			text = text[i:]
		}
		if k := strings.LastIndex(text, "}"); k >= 0 {
			text = text[:k+1]
		}
		var got struct {
			Headline string `json:"headline"`
			Fix      struct {
				Status      string `json:"status"`
				Version     string `json:"version"`
				AdvisoryURL string `json:"advisory_url"`
				Text        string `json:"text"`
			} `json:"fix"`
			UntilPatched []string `json:"until_patched"`
			Check        []string `json:"check"`
		}
		if err := json.Unmarshal([]byte(text), &got); err != nil {
			db.Exec(`UPDATE twoai_cves SET write_attempts = write_attempts + 1 WHERE cve_id=$1`, r.id)
			held++
			fmt.Printf("twoai_cve_write: %s: answer was not the expected JSON, held\n", r.id)
			continue
		}

		// THE CHECKS. Each one is a thing the request forbade in so many words.
		headline := strings.TrimSpace(strings.Trim(got.Headline, `"`))
		low := strings.ToLower(headline)
		why := ""
		switch {
		case headline == "":
			why = "no headline"
		case len([]rune(headline)) > 90:
			why = "headline over 90 characters"
		case strings.Contains(headline, "?"):
			why = "headline is a question"
		case strings.Contains(low, "critical") && !strings.EqualFold(r.sev, "CRITICAL"):
			why = "says critical, CVSS does not"
		case (strings.Contains(low, "exploited") || strings.Contains(low, "in the wild") || strings.Contains(low, "actively")) && !r.kev:
			why = "says exploited, KEV does not"
		}
		// Versions the record actually holds: the affected ranges and any
		// version-looking number in the description.
		allowed := map[string]bool{}
		for _, a := range affected {
			for _, v := range []string{a.StartIncluding, a.StartExcluding, a.EndIncluding, a.EndExcluding} {
				if v != "" {
					allowed[v] = true
				}
			}
		}
		for _, v := range cveVersionRe.FindAllString(r.desc, -1) {
			allowed[v] = true
		}
		refURLs := map[string]bool{}
		for _, t := range tagged {
			refURLs[t.URL] = true
		}
		var def cveDefense
		def.Fix.Status = "none_recorded"
		if got.Fix.Status == "fixed" && got.Fix.Version != "" && allowed[strings.TrimSpace(got.Fix.Version)] {
			def.Fix.Status = "fixed"
			def.Fix.Version = strings.TrimSpace(got.Fix.Version)
			if refURLs[strings.TrimSpace(got.Fix.AdvisoryURL)] {
				def.Fix.AdvisoryURL = strings.TrimSpace(got.Fix.AdvisoryURL)
			}
			def.Fix.Text = strings.TrimSpace(got.Fix.Text)
			if def.Fix.Text == "" {
				def.Fix.Text = "Upgrade to " + def.Fix.Version + " or later."
			}
		} else {
			where := "the project's releases page or security advisories"
			if r.vendor != "" {
				where = r.vendor + "'s security advisories or the project's releases page"
			}
			def.Fix.Text = "No fixed version is recorded in the CVE record or its references as of the last check. A fix, when there is one, will appear in " + where + "."
		}
		if why == "" && (strings.Contains(low, "patch now") || strings.Contains(low, "update now")) && def.Fix.Status != "fixed" {
			why = "says patch now, no fix recorded"
		}
		for _, s := range got.UntilPatched {
			if s = strings.TrimSpace(s); s != "" && len(def.UntilPatched) < 4 {
				def.UntilPatched = append(def.UntilPatched, s)
			}
		}
		for _, s := range got.Check {
			if s = strings.TrimSpace(s); s != "" && len(def.Check) < 3 {
				def.Check = append(def.Check, s)
			}
		}
		if len(def.UntilPatched) == 0 {
			why = fallback(why, "no compensating controls written")
		}
		if why != "" {
			db.Exec(`UPDATE twoai_cves SET write_attempts = write_attempts + 1 WHERE cve_id=$1`, r.id)
			held++
			fmt.Printf("twoai_cve_write: %s: held (%s)\n", r.id, why)
			continue
		}
		if r.kev {
			def.KevOpen = "CISA lists this CVE as exploited in the wild"
			if r.kevAdded != "" {
				def.KevOpen += ", added to the Known Exploited Vulnerabilities catalog on " + r.kevAdded
			}
			def.KevOpen += ". Treat the fix as urgent."
		}
		def.WrittenBy = model
		def.WrittenOn = time.Now().Format("2006-01-02")
		dj, _ := json.Marshal(def)
		if _, err := db.Exec(`UPDATE twoai_cves SET headline=$2, defense=$3::jsonb, written_on=current_date, written_hash=$4, write_attempts=0, updated_at=now() WHERE cve_id=$1`,
			r.id, headline, string(dj), r.hash); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_cve_write store:", err)
			continue
		}
		written++
		fmt.Printf("twoai_cve_write: %s: %s\n", r.id, headline)
	}
	fmt.Printf("twoai_cve_write: candidates=%d written=%d held=%d nvd_refetched=%d ok=true\n", len(todo), written, held, fetched)
}
