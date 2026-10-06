package main

// twoai_news_archive: permanence for the daily briefing's story permalinks.
//
// news/news.json is not a twoai artifact. publish_news writes it to
// srj-content every morning and fetch-content.mjs pulls it straight from
// there at build time, so it always holds TODAY's clustering. The story
// pages at /ai-news/{slug}/ are built from it, which means every one of them
// stops existing the next morning: on 2026-08-11 the sitemap carried 8 that
// were gone by the 12th, and 2 more went the same way the day after. It is
// the same failure as the vendor permalinks, arriving through a different
// door, and it produces a fresh pair of dead URLs every single day.
//
// So this stage snapshots each story the first time it is seen and keeps it.
// The briefing keeps rendering today's clustering from news.json; the
// permalinks render from the archive, which only grows. A story's row is
// written once and then only refreshed in place, so a slug published on
// Monday still resolves in June.
//
// Read-only with respect to SRJ: this reads the artifact publish_news has
// already written earlier in the same run and never writes back to it.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// twoaiNewsArchiveMaxArticles bounds a stored story. The briefing shows a
// source list, not every article GDELT clustered, and an unbounded Articles
// array would grow the published archive without bound as the years pass.
const twoaiNewsArchiveMaxArticles = 25

const twoaiNewsJSONURL = "https://raw.githubusercontent.com/srjordan6/srj-content/main/news/news.json"

func twoaiNewsArchive(db *sql.DB, upsert func(path, kind string, v any) error) (int, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_news_stories (
		slug text PRIMARY KEY,
		headline text NOT NULL,
		story jsonb NOT NULL,
		published_on date,
		first_published timestamptz NOT NULL DEFAULT now(),
		last_seen timestamptz NOT NULL DEFAULT now())`); err != nil {
		return 0, err
	}
	// EVERY STORY IS AN ENTITY. No data without a uid: the uid is minted from
	// the slug on the same scheme as every other kind, STORED on the row, and
	// registered in twoai_entities as kind 'story'. Until 2026-09-06 it was
	// computed only at archive-render time below, so 389 published stories
	// had no row in the entity register and nothing else could key on them.
	// The column is added here rather than by hand because this stage's role
	// owns the table; the 389 existing rows are filled in the same statement
	// pattern the graph uses, sha256('story:'||slug) hex prefix.
	if _, err := db.Exec(`ALTER TABLE twoai_news_stories ADD COLUMN IF NOT EXISTS uid text`); err != nil {
		return 0, err
	}
	if _, err := db.Exec(`UPDATE twoai_news_stories
		SET uid = substr(encode(sha256(('story:'||slug)::bytea),'hex'),1,8) WHERE uid IS NULL`); err != nil {
		return 0, err
	}
	if _, err := db.Exec(`INSERT INTO twoai_entities (uid, kind, name, normalized, first_seen, last_seen)
		SELECT uid, 'story', headline, slug, first_published, last_seen FROM twoai_news_stories
		WHERE uid IS NOT NULL ON CONFLICT (uid) DO NOTHING`); err != nil {
		return 0, err
	}

	// ---- One-shot backfill of pre-archive orphans.
	//
	// This stage shipped 2026-08-11, but story permalinks had been going
	// live (and dying the next morning) since the daily briefing launched.
	// One of them was being advertised while returning 404. The full record
	// of every published slug survives in srj-content's git history of
	// news/news.json, so the backfill replays that history and inserts
	// whatever the archive is missing, with each story's own day as its
	// published date. DO NOTHING on conflict: a story the archive already
	// holds keeps its original capture untouched. Guarded by a sentinel
	// slug from the advertised orphan, so this walks the history exactly
	// once and never again.
	var sentinel int
	db.QueryRow(`SELECT count(*) FROM twoai_news_stories
		WHERE slug='a-box-of-letters-in-a-vancouver-storage-unit-reveals-a-famil'`).Scan(&sentinel)
	if sentinel == 0 {
		twoaiNewsArchiveBackfill(db)
	}

	// Fetch what publish_news wrote earlier this run. A failure here is not
	// fatal: the archive already holds everything published before today, and
	// losing one day's capture is far better than failing the build.
	// READ THE COMMIT, NOT THE CACHE, 2026-10-01. raw.githubusercontent.com
	// serves a cached copy for several minutes. On 2026-10-01 the 9:05 PM run
	// published two stories at 02:25 UTC, the archive read the cached earlier
	// news.json a minute later and missed them, the next publish dropped them
	// from news.json, and the site's URL guard then refused every deploy for
	// fifteen hours because two published story pages were gone. The archive
	// now reads news.json at the exact newest commit, which no cache serves
	// stale, falling back to the plain address only if the commit lookup fails.
	newsURL := twoaiNewsJSONURL
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		if cb, cerr := twoaiJobsGet("https://api.github.com/repos/srjordan6/srj-content/commits?path=news/news.json&per_page=1",
			map[string]string{"Authorization": "Bearer " + tok, "Accept": "application/vnd.github+json"}); cerr == nil {
			var cs []struct {
				SHA string `json:"sha"`
			}
			if json.Unmarshal(cb, &cs) == nil && len(cs) == 1 && cs[0].SHA != "" {
				newsURL = "https://raw.githubusercontent.com/srjordan6/srj-content/" + cs[0].SHA + "/news/news.json"
			}
		}
	}
	body, err := twoaiJobsGet(newsURL, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_build: news archive fetch:", err, "(keeping the existing archive)")
	} else {
		var doc struct {
			Date    string           `json:"date"`
			Stories []map[string]any `json:"stories"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_build: news archive parse:", err)
		} else {
			for _, s := range doc.Stories {
				slug, _ := s["Slug"].(string)
				headline, _ := s["Headline"].(string)
				// Six archived stories carry the key as lowercase slug/headline.
				// Both spellings are the same field; read either.
				if slug == "" {
					slug, _ = s["slug"].(string)
				}
				if headline == "" {
					headline, _ = s["headline"].(string)
				}
				if slug == "" || headline == "" {
					continue
				}
				// The refresh below rewrites headline from news.json on every
				// run while the story is current, so a cleaned headline would
				// come back raw unless the same rule runs here too.
				headline = newsStripOutlet(headline, newsArticleDomains(s)...)
				s["Headline"] = headline
				// A headline or outlet title the English sweep has translated
				// is translated again here from its cache, never by a model
				// call, so a refresh from news.json cannot put the original
				// language back (bridge row 508).
				if _, ch := twoaiEnglishDoc(s, true, twoaiEnglishCacheOnly(db)); len(ch) > 0 {
					if h, ok := s["Headline"].(string); ok && h != "" {
						headline = h
					}
				}
				s["uid"] = twoaiUID("story:" + slug)
				if arts, ok := s["Articles"].([]any); ok && len(arts) > twoaiNewsArchiveMaxArticles {
					s["Articles"] = arts[:twoaiNewsArchiveMaxArticles]
				}
				raw, err := json.Marshal(s)
				if err != nil {
					continue
				}
				var pub any
				if len(doc.Date) >= 10 {
					if _, err := time.Parse("2006-01-02", doc.Date[:10]); err == nil {
						pub = doc.Date[:10]
					}
				}
				// The story is refreshed in place while it is still today's
				// news (clustering picks up outlets through the day), but
				// first_published and published_on are written once. A
				// permalink's date must not drift.
				// The refresh replaces the story document, so anything the site
				// wrote onto it after the fact (the why_it_matters line, row 371)
				// is carried across rather than lost.
				if _, err := db.Exec(`INSERT INTO twoai_news_stories
					(slug, headline, story, published_on, uid)
					VALUES ($1,$2,$3::jsonb,$4::date,$5)
					ON CONFLICT (slug) DO UPDATE SET
						headline=EXCLUDED.headline,
						story=CASE WHEN twoai_news_stories.story ? 'why_it_matters'
							THEN EXCLUDED.story || jsonb_build_object('why_it_matters', twoai_news_stories.story->'why_it_matters')
							ELSE EXCLUDED.story END,
						last_seen=now(),
						uid=COALESCE(twoai_news_stories.uid, EXCLUDED.uid)`,
					slug, headline, string(raw), pub, twoaiUID("story:"+slug)); err != nil {
					fmt.Fprintln(os.Stderr, "twoai_build: news story upsert:", err)
				}
				if _, err := db.Exec(`INSERT INTO twoai_entities (uid, kind, name, normalized)
					VALUES ($1,'story',$2,$3)
					ON CONFLICT (uid) DO UPDATE SET name=EXCLUDED.name, last_seen=now()`,
					twoaiUID("story:"+slug), headline, slug); err != nil {
					fmt.Fprintln(os.Stderr, "twoai_build: news story entity:", err)
				}
			}
		}
	}

	// RETIRED STORIES, 2026-10-01. Stephen, on /ai-news/37badd05/ ("Data
	// Privacy and Targeting Archives", a Campaigns & Elections category
	// archive clustered with three unrelated outlets): this makes no sense,
	// why is it in the news. A story can be retired: the row and the
	// permalink stay, the page says the story was withdrawn and why, is
	// noindex, and leaves every list.
	db.Exec(`ALTER TABLE twoai_news_stories ADD COLUMN IF NOT EXISTS retired_at timestamptz`)
	db.Exec(`ALTER TABLE twoai_news_stories ADD COLUMN IF NOT EXISTS retired_reason text`)
	// The first retirement, by the editor; the MCP role cannot alter this
	// table, so the row is marked here once the columns exist.
	db.Exec(`UPDATE twoai_news_stories SET retired_at = now(), retired_reason = $2 WHERE uid = $1 AND retired_at IS NULL`,
		"37badd05", "the page it was built from is a category archive listing on campaignsandelections.com, not a news story, and the three outlets clustered with it reported nothing related (editor, 2026-10-01)")
	// One event is one story (row 368): same-event duplicates are merged
	// before the archive is written, so the survivor carries every outlet
	// and the duplicate's permalink points at it.
	newsStoryUIDRepair(db)
	// One outlet's unrelated articles are not a story (bridge row 414):
	// merges of such bundles are reversed and bundles trimmed to the
	// articles that match their headline, before the dedupe pass.
	newsBundleRepair(db)
	twoaiNewsDedupe(db)
	dupOf := newsDuplicateMap(db)
	survivorHead := map[string]string{}
	if len(dupOf) > 0 {
		if hr, err := db.Query(`SELECT uid, headline FROM twoai_news_stories WHERE retired_at IS NULL`); err == nil {
			for hr.Next() {
				var u, h string
				if hr.Scan(&u, &h) == nil {
					survivorHead[u] = h
				}
			}
			hr.Close()
		}
	}
	rows, err := db.Query(`SELECT story::text, COALESCE(to_char(published_on,'YYYY-MM-DD'),''),
			to_char(first_published at time zone 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			retired_at IS NOT NULL, COALESCE(retired_reason,'')
		FROM twoai_news_stories ORDER BY published_on DESC NULLS LAST, slug`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	stories := []map[string]any{}
	for rows.Next() {
		var raw, pub, first, why string
		var retired bool
		if err := rows.Scan(&raw, &pub, &first, &retired, &why); err != nil {
			return 0, err
		}
		var s map[string]any
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			continue
		}
		// The page needs to date itself from the day the story ran, not from
		// whenever this archive was last written.
		s["ArchivedDate"] = pub
		s["ArchivedGenerated"] = first
		// EVERY STORY IS AN ENTITY AND CARRIES ITS OWN UID. Stephen looked at
		// a story permalink on 2026-09-06 and found no identifier on it. Each
		// story is an element inside news/archive.json rather than a page row,
		// so the archive's own page_uid is the only one the template could
		// have shown - and labelling a Zuckerberg story with the identifier of
		// the whole archive is worse than showing nothing. The slug is the
		// story's stable natural key, so the uid is minted from it on the same
		// scheme as everything else, and the graph's mentioned_in edges can
		// key on this instead of the doc:<id> form they use today.
		slug, _ := s["Slug"].(string)
		if slug == "" {
			slug, _ = s["slug"].(string)
		}
		if slug != "" {
			s["uid"] = twoaiUID("story:" + slug)
		}
		if retired {
			s["retired"] = true
			s["retired_reason"] = why
			if uid, _ := s["uid"].(string); uid != "" {
				if surv, ok := dupOf[uid]; ok {
					s["duplicate_of"] = surv
					s["duplicate_of_headline"] = survivorHead[surv]
				}
			}
		}
		stories = append(stories, s)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(stories) == 0 {
		return 0, nil
	}

	now := time.Now().UTC()
	if err := upsert("news/archive.json", "news-archive", map[string]any{
		"generated": now.Format(time.RFC3339),
		"date":      now.Format("2006-01-02"),
		"total":     len(stories),
		"stories":   stories,
	}); err != nil {
		return 0, err
	}
	// THE FRONT PAGE PIN, theworldofai row 498 (Stephen, 2026-10-05: put it
	// at the top of /ai-news/ today). A story pinned to news/front.json in
	// twoai_page_news leads the AI News page; the publisher merges the pins
	// onto this small document like any page's, and a pin stands for 36 hours.
	db.Exec(`UPDATE twoai_page_news SET active = false, retired_reason = 'front page pin expired'
		WHERE page_path = 'news/front.json' AND active AND added_at < now() - interval '36 hours'`)
	upsert("news/front.json", "news-front", map[string]any{"shape": "news-front", "date": now.Format("2006-01-02")})
	fmt.Printf("twoai_build: news archive stories=%d ok=true\n", len(stories))
	return len(stories), nil
}

// twoaiNewsArchiveBackfill replays the git history of srj-content's
// news/news.json and inserts every story the archive does not already hold.
// Failures are logged and skipped: a partial backfill is strictly better
// than none, and the sentinel guard means an incomplete walk retries on the
// next run because the sentinel slug (published 2026-08-10) will land only
// when its day's version has been processed.
func twoaiNewsArchiveBackfill(db *sql.DB) {
	tok := os.Getenv("GITHUB_TOKEN")
	hdr := map[string]string{}
	if tok != "" {
		hdr["Authorization"] = "Bearer " + tok
	}
	raw, err := twoaiJobsGet(
		"https://api.github.com/repos/srjordan6/srj-content/commits?path=news/news.json&per_page=60", hdr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twoai_build: news backfill commit list:", err)
		return
	}
	var commits []struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(raw, &commits); err != nil {
		fmt.Fprintln(os.Stderr, "twoai_build: news backfill parse commits:", err)
		return
	}
	inserted, seen := 0, 0
	// Oldest first, so a slug's published_on comes from the first edition
	// that carried it and later editions leave it alone.
	for i := len(commits) - 1; i >= 0; i-- {
		body, err := twoaiJobsGet(
			"https://raw.githubusercontent.com/srjordan6/srj-content/"+commits[i].SHA+"/news/news.json", hdr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "twoai_build: news backfill version", commits[i].SHA[:10], err)
			continue
		}
		time.Sleep(150 * time.Millisecond)
		var doc struct {
			Date    string           `json:"date"`
			Stories []map[string]any `json:"stories"`
		}
		if json.Unmarshal(body, &doc) != nil || len(doc.Date) < 10 {
			continue
		}
		day := doc.Date[:10]
		if _, err := time.Parse("2006-01-02", day); err != nil {
			continue
		}
		for _, s := range doc.Stories {
			slug, _ := s["Slug"].(string)
			headline, _ := s["Headline"].(string)
			if slug == "" || headline == "" {
				continue
			}
			headline = newsStripOutlet(headline, newsArticleDomains(s)...)
			s["Headline"] = headline
			if arts, ok := s["Articles"].([]any); ok && len(arts) > twoaiNewsArchiveMaxArticles {
				s["Articles"] = arts[:twoaiNewsArchiveMaxArticles]
			}
			j, err := json.Marshal(s)
			if err != nil {
				continue
			}
			seen++
			res, err := db.Exec(`INSERT INTO twoai_news_stories
				(slug, headline, story, published_on, first_published)
				VALUES ($1,$2,$3::jsonb,$4::date,($4||'T12:00:00Z')::timestamptz)
				ON CONFLICT (slug) DO NOTHING`, slug, headline, string(j), day)
			if err != nil {
				fmt.Fprintln(os.Stderr, "twoai_build: news backfill insert:", err)
				continue
			}
			if n, _ := res.RowsAffected(); n > 0 {
				inserted++
			}
		}
	}
	fmt.Printf("twoai_build: news backfill versions=%d stories_seen=%d inserted=%d\n",
		len(commits), seen, inserted)
}
