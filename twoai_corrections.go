package main

// Visible corrections, theworldofai bridge row 437 (Stephen, 2026-10-04,
// ahead of a NewsGuard review). A corrected published fact is recorded in
// twoai_corrections: the page it appeared on, the date, what the earlier
// version said, and an optional note. This stage writes them all to
// meta/corrections.json, which the site matches by page path to show a
// dated note on the page, and lists in full, newest first, on /data-quality/.
// The rows are content: theworldofai adds them when it corrects a record.

import (
	"database/sql"
	"fmt"
)

func twoaiCorrections(db *sql.DB, upsert func(path, kind string, v any) error) (int, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_corrections (
		id bigserial PRIMARY KEY,
		page_path text NOT NULL,
		title text NOT NULL DEFAULT '',
		corrected_on date NOT NULL DEFAULT current_date,
		earlier text NOT NULL,
		note text NOT NULL DEFAULT '',
		withdrawn boolean NOT NULL DEFAULT false,
		created_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return 0, err
	}
	rows, err := db.Query(`SELECT page_path, title, corrected_on::text, earlier, note FROM twoai_corrections
		WHERE NOT withdrawn ORDER BY corrected_on DESC, id DESC`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	items := []map[string]string{}
	for rows.Next() {
		var p, t, d, e, n string
		if rows.Scan(&p, &t, &d, &e, &n) == nil {
			items = append(items, map[string]string{"path": p, "title": t, "corrected_on": d, "earlier": e, "note": n})
		}
	}
	if err := upsert("meta/corrections.json", "corrections", map[string]any{
		"uid": twoaiUID("meta:corrections"), "items": items, "count": len(items),
	}); err != nil {
		return 0, err
	}
	fmt.Printf("twoai_corrections: %d corrections published ok=true\n", len(items))
	return 1, nil
}
