package main

// Visible corrections on the page, theworldofai bridge row 437 (Stephen,
// 2026-10-04, ahead of a NewsGuard review). Corrections are recorded in the
// existing twoai_corrections table (corrected_on, page_url, section,
// what_was_wrong, what_is_right, how_found, public), which already feeds the
// public log on /data-quality/ (twoai_quality.go). This stage publishes the
// public rows that name a page to meta/corrections.json, so the site can show
// a dated note on the corrected page itself: "Corrected <date>: earlier
// version said <what_was_wrong>."

import (
	"database/sql"
	"fmt"
)

func twoaiCorrections(db *sql.DB, upsert func(path, kind string, v any) error) (int, error) {
	rows, err := db.Query(`SELECT COALESCE(page_url,''), COALESCE(section,''), corrected_on::text, COALESCE(what_was_wrong,'')
		FROM twoai_corrections WHERE public AND COALESCE(page_url,'') <> '' AND COALESCE(what_was_wrong,'') <> ''
		ORDER BY corrected_on DESC, id DESC`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	items := []map[string]string{}
	for rows.Next() {
		var p, t, d, e string
		if rows.Scan(&p, &t, &d, &e) == nil {
			items = append(items, map[string]string{"path": p, "title": t, "corrected_on": d, "earlier": e})
		}
	}
	if err := upsert("meta/corrections.json", "corrections", map[string]any{
		"uid": twoaiUID("meta:corrections"), "items": items, "count": len(items),
	}); err != nil {
		return 0, err
	}
	fmt.Printf("twoai_corrections: %d page notes published ok=true\n", len(items))
	return 1, nil
}
