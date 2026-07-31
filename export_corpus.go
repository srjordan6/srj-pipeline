package main

// export_corpus: daily LLM training-corpus snapshot.
//
// Ported July 31 2026 from scripts/export_corpus.py (kept as the reference
// implementation until this stage is verified end to end, then retired).
// Runs as the last stage of `pipeline all`, so it snapshots the same day's
// sync and publish output.
//
// Pulls every published data family into one JSONL corpus and writes it, via
// the site Worker's bearer-gated PUT /api/archive, into the PRIVATE
// srj-uploads bucket:
//
//   corpus/training/YYYY-MM-DD/corpus.jsonl   immutable daily snapshot
//   corpus/training/YYYY-MM-DD/manifest.json  counts, sha256, bytes
//   corpus/training/latest/corpus.jsonl       rolling pointer
//   corpus/training/latest/manifest.json
//
// Each record carries provenance: "owned" and "public-record" are trainable;
// "third-party" (vendor/news headlines) is retrieval-only and excluded from
// training sets by default. Every run logs to srj_corpus_log, ok or not.
//
// Environment: DATABASE_URL, ARCHIVE_ENDPOINT, ARCHIVE_TOKEN (all already on
// the srj-pipeline cron).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const corpusTarball = "https://codeload.github.com/srjordan6/srj-content/tar.gz/refs/heads/main"

var corpusDBTables = []struct{ table, rtype, prov string }{
	{"synced_glossary_terms", "glossary-term", "owned"},
	{"synced_laws", "law-summary", "owned"},
	{"synced_tools", "tool-entry", "owned"},
	{"ai_lawsuits", "lawsuit-tracked", "public-record"},
	{"ai_lawsuit_candidates", "lawsuit-candidate", "public-record"},
	{"ai_intel_candidates", "intel-candidate", "third-party"},
}

var corpusContentDirs = map[string][2]string{
	"governance":  {"governance-page", "owned"},
	"news":        {"news", "owned"},
	"people":      {"person-profile", "owned"},
	"resources":   {"resource", "owned"},
	"books":       {"book", "owned"},
	"lawsuits":    {"lawsuit-dossier", "owned"},
	"legislation": {"legislation", "owned"},
	"migrated":    {"site-page", "owned"},
	"leaderboard": {"leaderboard", "owned"},
	"intel":       {"vendor-intel", "third-party"},
}

// corpusText assembles the record text: substantive string fields, longest
// first, falling back to the whole record as JSON.
func corpusText(d map[string]any) string {
	var ss []string
	for _, v := range d {
		if s, ok := v.(string); ok && len(s) > 40 {
			ss = append(ss, s)
		}
	}
	if len(ss) == 0 {
		b, _ := json.Marshal(d)
		return string(b)
	}
	sort.Slice(ss, func(i, j int) bool { return len(ss[i]) > len(ss[j]) })
	return strings.Join(ss, "\n\n")
}

func corpusTitle(d map[string]any) any {
	for _, k := range []string{"title", "name", "term", "case_name", "law_name", "tool_name", "headline", "page"} {
		if v, ok := d[k]; ok && v != nil && v != "" {
			return fmt.Sprint(v)
		}
	}
	return nil
}

func corpusRecord(id, rtype, prov, source string, d map[string]any) map[string]any {
	text := corpusText(d)
	h := sha256.Sum256([]byte(text))
	var created any
	for _, k := range []string{"created_at", "discovered_at", "date_created", "updated", "date", "generated"} {
		if v, ok := d[k]; ok && v != nil {
			created = v
			break
		}
	}
	url := d["url"]
	if url == nil {
		url = d["canonical_url"]
	}
	return map[string]any{
		"id": id, "type": rtype, "title": corpusTitle(d), "text": text,
		"url": url, "source": source, "provenance": prov, "created": created,
		"content_hash": hex.EncodeToString(h[:])[:16], "raw": d,
	}
}

func corpusDBRecords(db *sql.DB) ([]map[string]any, error) {
	var out []map[string]any
	for _, t := range corpusDBTables {
		rows, err := db.Query("SELECT * FROM " + t.table)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.table, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if rows.Scan(ptrs...) != nil {
				continue
			}
			d := map[string]any{}
			for i, c := range cols {
				v := vals[i]
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				if ts, ok := v.(time.Time); ok {
					v = ts.UTC().Format(time.RFC3339)
				}
				if v != nil && v != "" {
					d[c] = v
				}
			}
			rid := fmt.Sprint(d["id"])
			if d["id"] == nil {
				rid = fmt.Sprint(d["slug"])
			}
			out = append(out, corpusRecord("db:"+t.table+":"+rid, t.rtype, t.prov, "postgres:"+t.table, d))
		}
		rows.Close()
	}
	return out, nil
}

// corpusItems: a content file may be one object, a list, or a wrapper with a
// list under a known key.
func corpusItems(v any) []map[string]any {
	switch x := v.(type) {
	case []any:
		var out []map[string]any
		for _, e := range x {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		for _, k := range []string{"items", "cases", "entries", "terms", "tools", "laws", "pages", "people"} {
			if l, ok := x[k].([]any); ok {
				var out []map[string]any
				for _, e := range l {
					if m, ok := e.(map[string]any); ok {
						out = append(out, m)
					}
				}
				return out
			}
		}
		return []map[string]any{x}
	}
	return nil
}

func corpusContentRecords() ([]map[string]any, error) {
	resp, err := http.Get(corpusTarball)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("content tarball: %d", resp.StatusCode)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	var out []map[string]any
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		parts := strings.SplitN(hdr.Name, "/", 3)
		if len(parts) < 3 || hdr.Typeflag != tar.TypeReg || !strings.HasSuffix(hdr.Name, ".json") {
			continue
		}
		meta, ok := corpusContentDirs[parts[1]]
		if !ok {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, 64*1024*1024))
		if err != nil {
			continue
		}
		var data any
		if json.Unmarshal(raw, &data) != nil {
			continue
		}
		for i, d := range corpusItems(data) {
			clean := map[string]any{}
			for k, v := range d {
				if v != nil && v != "" {
					clean[k] = v
				}
			}
			rid := fmt.Sprint(clean["slug"])
			if clean["slug"] == nil {
				if clean["id"] != nil {
					rid = fmt.Sprint(clean["id"])
				} else {
					rid = fmt.Sprintf("%s:%d", parts[2], i)
				}
			}
			if clean["url"] == nil && parts[1] == "governance" && clean["slug"] != nil {
				clean["url"] = "https://srjconsultingservices.com/ai-governance/" + fmt.Sprint(clean["slug"]) + "/"
			}
			out = append(out, corpusRecord("content:"+parts[1]+":"+rid, meta[0], meta[1],
				"srj-content:"+parts[1]+"/"+parts[2], clean))
		}
	}
	return out, nil
}

func corpusLog(db *sql.DB, ok bool, counts map[string]int, size int, digest, key, detail string) {
	_, err := db.Exec(`INSERT INTO srj_corpus_log
		(ok, records, owned_records, public_records, thirdparty_records, bytes, sha256, r2_key, detail)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''),NULLIF($9,''))`,
		ok, counts["total"], counts["owned"], counts["public-record"], counts["third-party"],
		size, digest, key, detail)
	if err != nil {
		fmt.Fprintln(os.Stderr, "corpus-log write failed:", err)
	}
}

func exportCorpus(db *sql.DB) error {
	endpoint, token := os.Getenv("ARCHIVE_ENDPOINT"), os.Getenv("ARCHIVE_TOKEN")
	if endpoint == "" || token == "" {
		return fmt.Errorf("ARCHIVE_ENDPOINT and ARCHIVE_TOKEN must be set")
	}
	fail := func(err error) error {
		corpusLog(db, false, map[string]int{}, 0, "", "", err.Error())
		return err
	}
	dbRecs, err := corpusDBRecords(db)
	if err != nil {
		return fail(err)
	}
	contentRecs, err := corpusContentRecords()
	if err != nil {
		return fail(err)
	}
	records := append(dbRecs, contentRecs...)
	counts := map[string]int{"total": len(records)}
	var buf bytes.Buffer
	for _, r := range records {
		counts[r["provenance"].(string)]++
		line, merr := json.Marshal(r)
		if merr != nil {
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	body := buf.Bytes()
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	day := time.Now().UTC().Format("2006-01-02")
	manifest, _ := json.MarshalIndent(map[string]any{
		"generated": time.Now().UTC().Format(time.RFC3339),
		"records":   counts, "bytes": len(body), "sha256": digest,
		"training_note": "Train on provenance in {owned, public-record} only; third-party records are retrieval-only.",
	}, "", "  ")

	for _, prefix := range []string{"corpus/training/" + day, "corpus/training/latest"} {
		if err := archivePut(endpoint, token, prefix+"/corpus.jsonl", "application/x-ndjson", body); err != nil {
			return fail(err)
		}
		if err := archivePut(endpoint, token, prefix+"/manifest.json", "application/json", manifest); err != nil {
			return fail(err)
		}
	}
	corpusLog(db, true, counts, len(body), digest, "corpus/training/"+day+"/corpus.jsonl", "")
	fmt.Printf("export_corpus: ok total=%d owned=%d public=%d thirdparty=%d bytes=%d -> corpus/training/%s/\n",
		counts["total"], counts["owned"], counts["public-record"], counts["third-party"], len(body), day)
	return nil
}
