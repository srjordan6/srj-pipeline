package main

// One line per child on every hub, theworldofai row 450 (Stephen, 2026-10-04,
// on the AI Infrastructure and Hardware hub: "too much copy here, make each
// topic its own web page").
//
// Hubs listed each child with its twoai_taxonomy.blurb, and for 66 sections
// that blurb is the section's whole reading, up to 2,042 characters, so a hub
// read as one long essay. The blurb stays as it is, because the child's own
// page shows it in full. Each section also gets a line: the first sentence of
// a long blurb, kept in twoai_taxonomy.line. Hub child lists, the category
// pages and the older industries/ copies of the hubs show the line, and the
// long text appears on the child's page only. A line an editor writes is
// kept: line_auto is false for it, and only automatic lines are rewritten
// when the blurb changes.

import (
	"database/sql"
	"fmt"
	"strings"
	"unicode"
)

// twoaiLineMax is the longest blurb shown whole in a list.
const twoaiLineMax = 240

// twoaiOneLine returns the first sentence of a long text, or the text itself
// when it is short enough to list as it is.
func twoaiOneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n"); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) <= twoaiLineMax {
		return s
	}
	r := []rune(s)
	for i := 40; i < len(r)-2; i++ {
		if (r[i] != '.' && r[i] != '?' && r[i] != '!') || r[i+1] != ' ' {
			continue
		}
		next := r[i+2]
		if !unicode.IsUpper(next) && !unicode.IsDigit(next) && next != '"' && next != '“' {
			continue
		}
		// Not after an initial or a short abbreviation: "U.S. Senate", "e.g. The".
		j := i - 1
		for j >= 0 && r[j] != ' ' {
			j--
		}
		word := string(r[j+1 : i])
		if len([]rune(strings.ReplaceAll(word, ".", ""))) <= 2 {
			continue
		}
		if i+1 <= twoaiLineMax*2 {
			return string(r[:i+1])
		}
		break
	}
	// No usable sentence end: cut at a word boundary.
	cut := r
	if len(cut) > twoaiLineMax {
		cut = cut[:twoaiLineMax]
	}
	t := string(cut)
	if k := strings.LastIndex(t, " "); k > 80 {
		t = t[:k]
	}
	return strings.TrimRight(t, " ,;:") + "..."
}

// twoaiTaxonomyLines fills twoai_taxonomy.line for every long blurb.
func twoaiTaxonomyLines(db *sql.DB) {
	db.Exec(`ALTER TABLE twoai_taxonomy ADD COLUMN IF NOT EXISTS line text`)
	db.Exec(`ALTER TABLE twoai_taxonomy ADD COLUMN IF NOT EXISTS line_auto boolean NOT NULL DEFAULT true`)
	rows, err := db.Query(`SELECT slug, blurb, COALESCE(line,'') FROM twoai_taxonomy
		WHERE length(COALESCE(blurb,'')) > $1 AND (line IS NULL OR line = '' OR line_auto)`, twoaiLineMax)
	if err != nil {
		fmt.Println("twoai_taxonomy_lines:", err)
		return
	}
	type upd struct{ slug, line string }
	var ups []upd
	for rows.Next() {
		var slug, blurb, line string
		if rows.Scan(&slug, &blurb, &line) == nil {
			if l := twoaiOneLine(blurb); l != line {
				ups = append(ups, upd{slug, l})
			}
		}
	}
	rows.Close()
	for _, u := range ups {
		db.Exec(`UPDATE twoai_taxonomy SET line = $2, line_auto = true WHERE slug = $1`, u.slug, u.line)
	}
	if len(ups) > 0 {
		fmt.Printf("twoai_taxonomy_lines: %d one-line descriptions written\n", len(ups))
	}
}

// twoaiTaxLineSQL is the text a list shows for taxonomy row alias c.
const twoaiTaxLineSQL = `CASE WHEN length(COALESCE(c.blurb,'')) > 240 AND COALESCE(c.line,'') <> '' THEN c.line ELSE COALESCE(c.blurb,'') END`
