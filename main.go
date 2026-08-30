package main

import (
	"context"
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// STAGE DEADLINES: NO SINGLE STAGE MAY HANG THE RUN.
//
// Every stage runs as its own subprocess, so a deadline here covers all of
// them rather than patching one. On 2026-08-26 the intel sweep hit a
// CourtListener rate limit and never returned; the run sat for over half an
// hour writing nothing, and the twelve stages behind it - including the claim
// layer, which had a fixed prompt waiting to be proved - never ran at all. A
// third-party throttle should not be able to decide what the rest of the
// pipeline does tonight.
//
// A killed stage is not a failure to hide. It prints what it was and how long
// it got, so a stage that starts timing out regularly is visible in the log
// rather than showing up as work quietly not happening. Every stage here is
// either idempotent or cursor-persisted, so being killed mid-flight costs at
// most the batch in progress.
var twoaiStageDeadline = map[string]time.Duration{
	"twoai_build":     45 * time.Minute, // renders every page document
	"twoai_embed":     45 * time.Minute, // embeds every changed chunk
	"twoai_vectorize": 30 * time.Minute,
	"openalex_pull":   25 * time.Minute, // 150 pages plus backoff on 504s
	"twoai_claims":    25 * time.Minute,
	"twoai_jobs":      25 * time.Minute,
	"intel":           10 * time.Minute, // the stage that proved the need
	"twoai_recap":     8 * time.Minute,  // RECAP filing harvest, 12 dockets a run
	"export_corpus":   20 * time.Minute,
	// twoai_publish PUTs one file per changed page through the contents API,
	// sequentially, because GitHub serializes mutations to a single repo anyway.
	// A normal day changes 70 to 150 files and takes a minute or two. A day that
	// touches every page - a template change, a new field, an embed model swap -
	// changes about 2,800 and lands right on the old 20 minute default: it
	// finished with seconds to spare on 2026-08-24 and was killed on 08-26 and
	// 08-27. R2 carries the whole set in one request and is the primary path, so
	// a kill here only degrades the GitHub fallback, quietly, which is exactly
	// the failure a fallback must not have. If it times out again the real fix is
	// the git tree API: one commit for the whole set instead of one per file.
	"twoai_publish": 45 * time.Minute,
}

const twoaiStageDeadlineDefault = 20 * time.Minute

func runSequence(stages []string) {
	for _, s := range stages {
		limit := twoaiStageDeadlineDefault
		if d, ok := twoaiStageDeadline[s]; ok {
			limit = d
		}
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		cmd := exec.CommandContext(ctx, os.Args[0], s)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Run() // a failing source must not block the others
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "%s: KILLED after %s deadline, continuing the run\n", s, limit)
		}
		cancel()
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pipeline <source_key>")
		os.Exit(2)
	}
	src := os.Args[1]
	// Fast path for theworldofai.org. `pipeline all` takes twenty to thirty
	// minutes because it re-ingests every source, so a change to the taxonomy or
	// a page template used to mean waiting for a full run to see it. This runs
	// only the stages that turn existing SQL into a published site, which takes
	// about a minute. Nothing here fetches from an external source.
	if src == "twoai" {
		for _, s := range []string{"twoai_build", "twoai_publish_r2", "twoai_publish", "deploy_site"} {
			cmd := exec.Command(os.Args[0], s)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			cmd.Run()
		}
		return
	}
	if src == "all" {
		// email_route is RETIRED from the daily run, Stephen's decision 2026-08-18.
		//
		// It ran from 2026-08-01 to 2026-08-12 and wrote 58 bridge rows across
		// eight projects. Thirty-seven were never acked, including all sixteen to
		// books_kdp and all fourteen to srj. Its last dozen rows were six
		// Draft2Digital "Published:" auto-notices, three Render failure alerts,
		// one piece of account-deletion marketing, and two genuine replies that
		// were sitting in the inbox anyway. Five in six of its output was
		// automated notification mail that a filter rule routes just as well.
		//
		// What made it redundant: the Inkbox gateway now pushes mail into
		// project_bridge within seconds of arrival, which was this stage's main
		// job. inkbox_pull rode along as the daily reconciler until 2026-08-26,
		// when the srj-inkbox-tick cron took over: it runs inkboxPull plus
		// inkboxOutbox every fifteen minutes from Ohio, which is ninety-six
		// reconciliations a day against this sequence's one. Stephen flagged
		// the duplicate on 2026-08-29 and it is retired from `all`; the tick
		// cron is the reconciler now, and `pipeline inkbox_pull` still runs
		// on demand.
		//
		// What is genuinely lost, stated rather than glossed: nothing now triages
		// the general srj@srjconsultingservices.com inbox, so deadline, financial
		// and legal language in mail nobody addressed to a project mailbox is
		// spotted by reading the inbox, not by a machine. The stage caught a Manus
		// account-deletion deadline and repeated focms-api failures that way. That
		// is the cost of this decision and it is accepted, not overlooked.
		//
		// The stage itself is untouched and still runs on demand: `pipeline
		// email_route`. It needs GOOGLE_SA_EMAIL and GOOGLE_SA_KEY on whatever
		// host runs it, plus gmail.modify in the domain-wide delegation grant.
		//
		// favicons is also OUT of the daily run, Stephen's decision 2026-08-18.
		// It is a one-shot idempotent push of binary assets into srj-site:
		// four favicons embedded as base64, plus book covers, executive-briefing
		// PDFs and insight images merged in by loadRemoteCovers and the zip
		// asset sets. Every file it handles is already in the repo, so a daily
		// run is a few dozen GETs that write nothing and print an "ensured" line
		// per file - the log noise Stephen asked about, with no product.
		//
		// Assets change when a book ships or a cover is replaced, which is an
		// event, not a daily rhythm. Run `pipeline favicons` at that point. The
		// stage is unchanged and still idempotent, so running it costs nothing
		// but the GETs.
		seq := []string{"federal_register", "agency_watch", "legiscan", "gdelt", "govinfo", "mcp_registry", "twoai_recap", "intel", "archive_news", "publish_news", "publish_legislation", "publish_leaderboard", "publish_lawsuits", "publish_intel", "sync_people", "sync_content", "bench_results", "twoai_jobs", "twoai_vendor_feeds", "twoai_case_studies", "vendor_notes", "twoai_onet", "twoai_ga_top", "talent_pull", "ask_pull", "twoai_openlibrary", "docwatch", "doi_queue", "twoai_build", "twoai_embed", "twoai_vectorize", "twoai_publish", "twoai_publish_r2", "arxiv_watch", "url_registry", "twoai_indexnow", "audit_sync", "export_corpus", "deploy_site"}
		// The corpus stages ride along with the daily build UNTIL a dedicated
		// corpus cron exists, at which point setting CORPUS_CRON=1 here stops
		// the duplication. Leaving them in by default matters: removing them
		// first and trusting that the other cron gets created is exactly the
		// silent-degradation failure this pipeline is built to avoid - the
		// corpus would simply stop growing and nothing would say so.
		if os.Getenv("CORPUS_CRON") == "" {
			seq = append(seq[:1], append([]string{"openalex_pull"}, seq[1:]...)...)
			for i, s := range seq {
				if s == "twoai_build" {
					seq = append(seq[:i], append([]string{"twoai_claims"}, seq[i:]...)...)
					break
				}
			}
		}
		runSequence(seq)
		return
	}

	// `pipeline corpus` is the works spine and the claim layer on their own
	// schedule. The corpus is a different-shaped job from rendering a site:
	// it grows by tens of thousands of rows a night, it depends on APIs that
	// time out, and it must not sit behind thirty stages of page building to
	// get its turn. A CourtListener rate limit hanging the intel sweep on
	// 2026-08-26 starved the claim layer of a run entirely, which is what
	// prompted the split.
	if src == "corpus" {
		runSequence([]string{"openalex_pull", "doi_queue", "twoai_claims"})
		return
	}

	// publish_leaderboard needs no database: it is a pure HTTP fetch of a
	// public leaderboard mirror. Handled before the db open so it still runs
	// on a host with no DATABASE_URL set.
	if src == "publish_leaderboard" {
		if err := publishLeaderboard(); err != nil {
			fmt.Fprintln(os.Stderr, "publish_leaderboard:", err)
			os.Exit(1)
		}
		fmt.Println("publish_leaderboard: ok")
		return
	}

	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "db open:", err)
		os.Exit(1)
	}
	defer db.Close()

	if src == "publish_news" {
		if err := publishNews(db); err != nil {
			fmt.Fprintln(os.Stderr, "publish_news:", err)
			os.Exit(1)
		}
		fmt.Println("publish_news: ok")
		return
	}
	if src == "publish_legislation" {
		if err := publishLegislation(db); err != nil {
			fmt.Fprintln(os.Stderr, "publish_legislation:", err)
			os.Exit(1)
		}
		fmt.Println("publish_legislation: ok")
		return
	}
	if src == "publish_lawsuits" {
		if err := publishLawsuits(db); err != nil {
			fmt.Fprintln(os.Stderr, "publish_lawsuits:", err)
			os.Exit(1)
		}
		fmt.Println("publish_lawsuits: ok")
		return
	}
	if src == "publish_intel" {
		if err := publishIntel(db); err != nil {
			fmt.Fprintln(os.Stderr, "publish_intel:", err)
			os.Exit(1)
		}
		fmt.Println("publish_intel: ok")
		return
	}
	if src == "intel" {
		if err := intelSync(db); err != nil {
			fmt.Fprintln(os.Stderr, "intel:", err)
			os.Exit(1)
		}
		fmt.Println("intel: ok")
		return
	}
	if src == "twoai_publish_r2" {
		if err := twoaiPublishR2(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_publish_r2:", err)
			os.Exit(1)
		}
		return
	}
	if src == "mcp_registry" {
		if err := mcpRegistry(db); err != nil {
			fmt.Fprintln(os.Stderr, "mcp_registry:", err)
			os.Exit(1)
		}
		return
	}
	if src == "arxiv_watch" {
		if err := arxivWatch(db); err != nil {
			fmt.Fprintln(os.Stderr, "arxiv_watch:", err)
			os.Exit(1)
		}
		return
	}

	if src == "url_registry" {
		if err := urlRegistry(db); err != nil {
			fmt.Fprintln(os.Stderr, "url_registry:", err)
			os.Exit(1)
		}
		return
	}

	// twoai_embed builds the retrieval index the site assistant answers from.
	// Runs after twoai_build, which is what writes the pages it reads.
	// twoai_ask: retrieval probe. Prints the chunks the index would hand an
	// answer model for a question, and generates nothing.
	// serve: the site assistant endpoint. Deployed as a Render Web Service from
	// this repo with `./pipeline serve`, in the database's own environment,
	// because the retrieval index sits behind a one-IP allow list a Worker
	// cannot cross.
	if src == "serve" {
		if err := twoaiServe(db); err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
		return
	}

	if src == "twoai_ask" {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, `usage: pipeline twoai_ask "your question"`)
			os.Exit(1)
		}
		if err := twoaiAsk(db, strings.Join(os.Args[2:], " ")); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_ask:", err)
			os.Exit(1)
		}
		return
	}

	// twoai_vectorize pushes the index to Cloudflare so the edge Worker can
	// query it. Runs after twoai_embed, which is what writes the vectors.
	if src == "twoai_vectorize" {
		if err := twoaiVectorizeRun(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_vectorize:", err)
			os.Exit(1)
		}
		return
	}

	if src == "twoai_embed" {
		if err := twoaiEmbedRun(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_embed:", err)
			os.Exit(1)
		}
		return
	}

	// twoai_indexnow pushes newly published URLs to Bing, Yandex, Naver and
	// Seznam. Runs after url_registry, which is what decides "new".
	// `pipeline twoai_indexnow verify` checks the key file is readable without
	// submitting anything.
	if src == "twoai_indexnow" {
		if len(os.Args) > 2 && os.Args[2] == "verify" {
			if err := verifyIndexNowKey(); err != nil {
				fmt.Fprintln(os.Stderr, "twoai_indexnow:", err)
				os.Exit(1)
			}
			return
		}
		if err := twoaiIndexNow(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_indexnow:", err)
			os.Exit(1)
		}
		return
	}

	if src == "bench_results" {
		if err := benchResults(db); err != nil {
			fmt.Fprintln(os.Stderr, "bench_results:", err)
			os.Exit(1)
		}
		return
	}

	if src == "sync_people" {
		if err := syncPeople(db); err != nil {
			fmt.Fprintln(os.Stderr, "sync_people:", err)
			os.Exit(1)
		}
		return
	}

	if src == "sync_content" {
		if err := syncContent(db); err != nil {
			fmt.Fprintln(os.Stderr, "sync_content:", err)
			os.Exit(1)
		}
		return
	}

	if src == "twoai_recap" {
		if err := twoaiRecapCitations(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_recap:", err)
			os.Exit(1)
		}
		return
	}

	if src == "twoai_build" {
		if err := twoaiBuild(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_build:", err)
			os.Exit(1)
		}
		return
	}

	if src == "twoai_jobs" {
		if err := twoaiJobsFetch(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_jobs:", err)
			os.Exit(1)
		}
		return
	}

	if src == "twoai_vendor_feeds" {
		if err := twoaiVendorFeeds(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_vendor_feeds:", err)
			os.Exit(1)
		}
		return
	}

	if src == "twoai_case_studies" {
		if err := twoaiCaseStudyHarvest(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_case_studies:", err)
			os.Exit(1)
		}
		return
	}

	if src == "twoai_onet" {
		if err := twoaiOnet(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_onet:", err)
			os.Exit(1)
		}
		return
	}

	if src == "audit_sync" {
		if err := auditSync(db); err != nil {
			fmt.Fprintln(os.Stderr, "audit_sync:", err)
			os.Exit(1)
		}
		return
	}
	if src == "twoai_ga_top" {
		if err := twoaiGATop(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_ga_top:", err)
			os.Exit(1)
		}
		return
	}
	if src == "talent_pull" {
		// Non-fatal by design: a D1 hiccup must not sink the nightly build;
		// the site simply rebuilds from the last successful pull.
		if err := talentPull(db); err != nil {
			fmt.Fprintln(os.Stderr, "talent_pull:", err)
		}
		return
	}

	if src == "openalex_pull" {
		// Non-fatal: the spine grows by the daily rhythm; a rate-limited or
		// failed run resumes from its persisted cursor tomorrow.
		if err := twoaiOpenAlex(db); err != nil {
			fmt.Fprintln(os.Stderr, "openalex_pull:", err)
		}
		return
	}

	if src == "twoai_claims" {
		// Non-fatal: the claim layer is an enrichment of a corpus that keeps
		// growing; a bad API day costs a batch, not a build.
		if err := twoaiClaims(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_claims:", err)
		}
		return
	}

	if src == "agency_watch" {
		// Non-fatal: an agency feed being down is not a build failure.
		if err := twoaiAgencyWatch(db); err != nil {
			fmt.Fprintln(os.Stderr, "agency_watch:", err)
		}
		return
	}

	if src == "doi_queue" {
		// Non-fatal: a DOI that will not resolve today stays pending.
		if err := twoaiDOIQueue(db); err != nil {
			fmt.Fprintln(os.Stderr, "doi_queue:", err)
		}
		return
	}

	if src == "docwatch" {
		// Non-fatal: a framework site being down is not a build failure.
		if err := twoaiDocWatch(db); err != nil {
			fmt.Fprintln(os.Stderr, "docwatch:", err)
		}
		return
	}

	if src == "twoai_openlibrary" {
		// Non-fatal: a public API being slow is not a reason to fail a build.
		if err := twoaiOpenLibrary(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_openlibrary:", err)
		}
		return
	}

	if src == "ask_pull" {
		// Non-fatal for the same reason talent_pull is: the log mirror is
		// diagnostics, and a D1 hiccup must not sink the nightly build.
		if err := askLogPull(db); err != nil {
			fmt.Fprintln(os.Stderr, "ask_pull:", err)
		}
		return
	}

	if src == "twoai_publish" {
		if err := twoaiPublish(db); err != nil {
			fmt.Fprintln(os.Stderr, "twoai_publish:", err)
			os.Exit(1)
		}
		return
	}

	if src == "deploy_site" {
		if err := deploySite(); err != nil {
			fmt.Fprintln(os.Stderr, "deploy_site:", err)
			os.Exit(1)
		}
		fmt.Println("deploy_site: ok")
		return
	}

	// Backfill for rows stored before the resolver landed: 1,213 of them hold
	// an opaque Google News redirect. Own subcommand, never part of `all`, so
	// a Google-side change can stall this and nothing else.
	if src == "favicons" {
		// Remote-sourced assets merge into faviconFiles before the push so
		// the daily run ensures them alongside the embedded set. Wired here,
		// not in hourlyCatchUp, so the zip fetch happens once a day, not 24
		// times. If a staging URL has expired the loader logs and skips, and
		// files already in the repo stay put.
		// loadRemoteCovers is called inside runFavicons; calling it here too
		// fetched and verified every cover twice per run (visible as a
		// duplicated log block). The zip assets still load here.
		loadZipAssets()
		if err := runFavicons(); err != nil {
			fmt.Fprintln(os.Stderr, "favicons:", err)
			os.Exit(1)
		}
		return
	}

	if src == "resolve_gnews" {
		rows, err := db.Query(`SELECT id, url, vendor FROM ai_intel_candidates
			WHERE url LIKE '%news.google.com/rss/articles%' ORDER BY id DESC`)
		if err != nil {
			fmt.Fprintln(os.Stderr, "resolve_gnews:", err)
			os.Exit(1)
		}
		type row struct {
			id          int
			url, vendor string
		}
		var todo []row
		for rows.Next() {
			var r row
			var v sql.NullString
			if rows.Scan(&r.id, &r.url, &v) == nil {
				r.vendor = v.String
				todo = append(todo, r)
			}
		}
		rows.Close()
		fixed, missed := 0, 0
		for i, r := range todo {
			real := resolveGoogleNews(r.url)
			if real == r.url {
				missed++
			} else {
				vendor := r.vendor
				if strings.Contains(vendor, "(coverage)") {
					if pub := publisherFromURL(real); pub != "" {
						vendor = pub
					}
				}
				db.Exec(`UPDATE ai_intel_candidates SET url=$1, vendor=$2 WHERE id=$3`, real, vendor, r.id)
				fixed++
			}
			time.Sleep(700 * time.Millisecond)
			if (i+1)%50 == 0 {
				fmt.Printf("resolve_gnews: %d/%d fixed=%d missed=%d\n", i+1, len(todo), fixed, missed)
			}
		}
		fmt.Printf("resolve_gnews: done fixed=%d missed=%d of %d\n", fixed, missed, len(todo))
		return
	}

	if src == "vendor_notes" {
		if err := vendorNotes(db); err != nil {
			fmt.Fprintln(os.Stderr, "vendor_notes:", err)
			os.Exit(1)
		}
		return
	}

	if src == "inkbox_pull" {
		if err := inkboxPull(db); err != nil {
			fmt.Fprintln(os.Stderr, "inkbox_pull:", err)
			os.Exit(1)
		}
		return
	}

	if src == "inkbox_outbox" {
		if err := inkboxOutbox(db); err != nil {
			fmt.Fprintln(os.Stderr, "inkbox_outbox:", err)
			os.Exit(1)
		}
		return
	}

	if src == "inkbox_tick" {
		// The fifteen-minute cron. Receive first, then send, so a reply queued
		// in response to something that arrived this same tick still goes out
		// without waiting another quarter hour.
		if err := inkboxPull(db); err != nil {
			fmt.Fprintln(os.Stderr, "inkbox_tick: pull:", err)
		}
		if err := inkboxOutbox(db); err != nil {
			fmt.Fprintln(os.Stderr, "inkbox_tick: outbox:", err)
			os.Exit(1)
		}
		return
	}

	if src == "email_route" {
		if err := emailRoute(db); err != nil {
			fmt.Fprintln(os.Stderr, "email_route:", err)
			os.Exit(1)
		}
		return
	}

	if src == "export_corpus" {
		if err := exportCorpus(db); err != nil {
			fmt.Fprintln(os.Stderr, "export_corpus:", err)
			os.Exit(1)
		}
		return
	}

	if src == "archive_news" {
		if err := archiveNews(db); err != nil {
			fmt.Fprintln(os.Stderr, "archive_news:", err)
			os.Exit(1)
		}
		fmt.Println("archive_news: ok")
		return
	}
	var sourceID int
	if err := db.QueryRow(`SELECT id FROM pipeline.sources WHERE key=$1`, src).Scan(&sourceID); err != nil {
		fmt.Fprintln(os.Stderr, "unknown source:", src, err)
		os.Exit(1)
	}
	var runID int64
	if err := db.QueryRow(`INSERT INTO pipeline.runs (source_id) VALUES ($1) RETURNING id`, sourceID).Scan(&runID); err != nil {
		fmt.Fprintln(os.Stderr, "run insert:", err)
		os.Exit(1)
	}

	var fetched, added int
	var runErr error
	switch src {
	case "federal_register":
		fetched, added, runErr = federalRegister(db, sourceID)
	case "legiscan":
		fetched, added, runErr = legiscan(db, sourceID)
	case "gdelt":
		fetched, added, runErr = gdelt(db, sourceID)
	case "govinfo":
		fetched, added, runErr = govinfo(db, sourceID)
	default:
		runErr = fmt.Errorf("no adapter for source %q", src)
	}

	status, errText := "ok", sql.NullString{}
	if runErr != nil {
		status = "error"
		errText = sql.NullString{String: runErr.Error(), Valid: true}
		fmt.Fprintln(os.Stderr, "run error:", runErr)
	}
	db.Exec(`UPDATE pipeline.runs SET finished_at=now(), status=$1, docs_fetched=$2, docs_new=$3, error=$4 WHERE id=$5`,
		status, fetched, added, errText, runID)
	fmt.Printf("run %d: %s fetched=%d new=%d status=%s\n", runID, src, fetched, added, status)
	if runErr != nil {
		os.Exit(1)
	}
}

func federalRegister(db *sql.DB, sourceID int) (fetched, added int, err error) {
	url := "https://www.federalregister.gov/api/v1/documents.json" +
		"?conditions%5Bterm%5D=%22artificial+intelligence%22" +
		"&order=newest&per_page=100" +
		"&fields%5B%5D=document_number&fields%5B%5D=title&fields%5B%5D=type" +
		"&fields%5B%5D=abstract&fields%5B%5D=publication_date&fields%5B%5D=agencies" +
		"&fields%5B%5D=html_url&fields%5B%5D=pdf_url&fields%5B%5D=raw_text_url"

	client := &http.Client{Timeout: 60 * time.Second}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "srj-pipeline/1.0 (srjconsultingservices.com)")
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, 0, fmt.Errorf("FR API status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, err
	}

	var payload struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, 0, err
	}

	for _, doc := range payload.Results {
		fetched++
		title, _ := doc["title"].(string)
		abstract, _ := doc["abstract"].(string)

		// The API query already carries conditions[term]="artificial
		// intelligence", but conditions[term] searches the FULL TEXT of a
		// document. A submarine cable licensing rule that mentions AI once in
		// its body matches, and so does a fishery council meeting notice. On
		// 2026-07-28 only 15 of 101 stored documents mentioned AI anywhere, and
		// the other 86 were noise occupying the corpus.
		//
		// Keep the broad query, since recall at the API is free and cheap to
		// filter, then narrow here: a document earns a row only if AI appears in
		// its TITLE or ABSTRACT, which is the difference between a rule about AI
		// and a rule that happens to mention it.
		if !mentionsAI(title) && !mentionsAI(abstract) {
			continue
		}

		raw, _ := json.Marshal(doc)
		h := sha256.Sum256(raw)
		hash := hex.EncodeToString(h[:])
		extID, _ := doc["document_number"].(string)
		if extID == "" {
			continue
		}
		htmlURL, _ := doc["html_url"].(string)
		var pub any
		if p, ok := doc["publication_date"].(string); ok && p != "" {
			pub = p
		}
		res, e := db.Exec(`INSERT INTO pipeline.documents (source_id, external_id, change_hash, url, title, published_at, raw)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (source_id, external_id, change_hash) DO NOTHING`,
			sourceID, extID, hash, htmlURL, title, pub, raw)
		if e != nil {
			return fetched, added, e
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return fetched, added, nil
}

// govinfo pulls federal court opinions that mention AI from the U.S.
// Government Publishing Office's official USCOURTS collection.
//
// WHY THIS EXISTS ALONGSIDE COURTLISTENER. CourtListener gives us dockets:
// who sued whom, and what was filed when. It does not give us the court's own
// words. govinfo publishes the opinion PDFs and their metadata as an official
// government record, free, with a documented API. Rulings are where AI law
// actually gets made, so a tracker that only watches filings watches the least
// interesting half.
//
// The search is a FULL TEXT phrase match, which means most hits mention AI in
// passing rather than being about it: a live check on 2026-08-02 returned 1,168
// opinions, including insurance and social-security appeals whose text happens
// to contain the phrase. Everything is kept in the corpus regardless, because
// the corpus is a research asset and filtering it destroys recall we cannot get
// back. Only opinions whose CAPTION names a known AI party are promoted into
// the lawsuit candidate queue, where the existing scoring applies. That single
// live check surfaced DOE v. X.AI Corp., which the docket sweep had not.
//
// Key from GOVINFO_API_KEY, free from api.data.gov. Without it the stage says
// so and returns cleanly rather than failing the run.
func govinfo(db *sql.DB, sourceID int) (fetched, added int, err error) {
	key := os.Getenv("GOVINFO_API_KEY")
	if key == "" {
		return 0, 0, fmt.Errorf("GOVINFO_API_KEY not set")
	}
	since := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	body, _ := json.Marshal(map[string]any{
		"query":      `collection:USCOURTS "artificial intelligence" publishdate:range(` + since + `,)`,
		"pageSize":   100,
		"offsetMark": "*",
		"sorts":      []map[string]string{{"field": "publishdate", "sortOrder": "DESC"}},
	})
	req, err := http.NewRequest("POST", "https://api.govinfo.gov/search", bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("X-Api-Key", key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "srj-pipeline/1.0 (srjconsultingservices.com)")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return 0, 0, fmt.Errorf("govinfo search %d: %s", resp.StatusCode, b)
	}
	var payload struct {
		Count   int `json:"count"`
		Results []struct {
			Title            string         `json:"title"`
			PackageID        string         `json:"packageId"`
			GranuleID        string         `json:"granuleId"`
			DateIssued       string         `json:"dateIssued"`
			GovernmentAuthor []string       `json:"governmentAuthor"`
			Download         map[string]any `json:"download"`
			ResultLink       string         `json:"resultLink"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, 0, err
	}

	// A caption naming an AI company is the signal that an opinion is about AI
	// rather than merely mentioning it.
	aiParty := regexp.MustCompile(`(?i)openai|anthropic|midjourney|stability ai|uncharted labs|udio|suno|perplexity|x\.?ai|character\.?ai|character technologies|clearview|workday|hirevue|scale ai|cohere|mistral|eleven ?labs|minimax|hugging face|deepseek`)
	queued := 0
	for _, r := range payload.Results {
		if r.PackageID == "" || r.Title == "" {
			continue
		}
		fetched++
		court := ""
		if len(r.GovernmentAuthor) > 0 {
			court = r.GovernmentAuthor[len(r.GovernmentAuthor)-1]
		}
		link := r.ResultLink
		if link == "" {
			link = "https://www.govinfo.gov/app/details/" + r.PackageID
		}
		meta := map[string]any{
			"title": r.Title, "packageId": r.PackageID, "granuleId": r.GranuleID,
			"dateIssued": r.DateIssued, "court": court, "download": r.Download,
			"collection": "USCOURTS",
		}
		raw, _ := json.Marshal(meta)
		h := sha256.Sum256(raw)
		extID := r.PackageID
		if r.GranuleID != "" {
			extID = r.PackageID + "/" + r.GranuleID
		}
		var pub any
		if r.DateIssued != "" {
			pub = r.DateIssued
		}
		ok, e := insertDoc(db, sourceID, extID, hex.EncodeToString(h[:]), link, r.Title, pub, raw)
		if e != nil {
			return fetched, added, e
		}
		if ok {
			added++
		}
		// Promote to the lawsuit candidate queue only when the caption names
		// an AI party. Score 4 keeps these below the auto-publish threshold of
		// 5, since an opinion tells us a case exists but not that it belongs on
		// a tracker of AI litigation.
		if !aiParty.MatchString(r.Title) {
			continue
		}
		res, e := db.Exec(`INSERT INTO ai_lawsuit_candidates
			(source, source_id, case_name, court, docket, filed_date, url, snippet, score)
			VALUES ('govinfo', $1, $2, $3, '', NULLIF($4,'')::date, $5, $6, 4)
			ON CONFLICT (source_id) DO NOTHING`,
			"govinfo-"+extID, r.Title, court, r.DateIssued, link,
			"Federal court opinion published by GPO in the USCOURTS collection.")
		if e == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				queued++
			}
		}
	}
	fmt.Printf("govinfo: matched=%d stored=%d queued_candidates=%d\n", payload.Count, added, queued)
	return fetched, added, nil
}

// aiTerm matches AI as a subject, not AI as a passing mention. The \b on the
// bare "AI" is what stops it matching inside "said", "chair", or "maintain".
var aiTerm = regexp.MustCompile(`(?i)\bartificial intelligence\b|\bmachine learning\b|\bA\.?I\.?\b|\bgenerative ai\b|\balgorithmic\b|\bautomated decision\b`)

func mentionsAI(s string) bool { return s != "" && aiTerm.MatchString(s) }

// insertDoc appends one document to the corpus with change_hash dedupe.
func insertDoc(db *sql.DB, sourceID int, extID, hash, url, title string, pub any, raw []byte) (bool, error) {
	res, err := db.Exec(`INSERT INTO pipeline.documents (source_id, external_id, change_hash, url, title, published_at, raw)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (source_id, external_id, change_hash) DO NOTHING`,
		sourceID, extID, hash, url, title, pub, raw)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// legiscan pulls state AI bills, completely. getSearchRaw returns the FULL
// result set for the query (up to 2,000 ids per page with LegiScan's own
// change_hash), where the old getSearch capped at the top ~100 by relevance
// and a brand-new bill could in theory sit below the fold for days. Only ids
// that are new or changed get hydrated through getBill, so a quiet day costs
// one search call; the hydration budget caps a heavy first pass and carries
// the remainder to the next run. Relevance below 50 is a passing mention,
// not an AI bill. Key from LEGISCAN_API_KEY.
func legiscan(db *sql.DB, sourceID int) (fetched, added int, err error) {
	key := os.Getenv("LEGISCAN_API_KEY")
	if key == "" {
		return 0, 0, fmt.Errorf("LEGISCAN_API_KEY not set")
	}
	client := &http.Client{Timeout: 60 * time.Second}
	const hydrateBudget = 300
	hydrated := 0
	for page := 1; page <= 3; page++ {
		url := fmt.Sprintf("https://api.legiscan.com/?key=%s&op=getSearchRaw&state=ALL&query=%s&page=%d",
			key, "%22artificial+intelligence%22", page)
		resp, e := client.Get(url)
		if e != nil {
			return fetched, added, e
		}
		body, e := io.ReadAll(resp.Body)
		resp.Body.Close()
		if e != nil {
			return fetched, added, e
		}
		var payload struct {
			Status       string `json:"status"`
			SearchResult struct {
				Summary struct {
					PageTotal int `json:"page_total"`
				} `json:"summary"`
				Results []struct {
					Relevance  int    `json:"relevance"`
					BillID     int    `json:"bill_id"`
					ChangeHash string `json:"change_hash"`
				} `json:"results"`
			} `json:"searchresult"`
		}
		if e := json.Unmarshal(body, &payload); e != nil {
			return fetched, added, e
		}
		if payload.Status != "OK" {
			return fetched, added, fmt.Errorf("legiscan status %s", payload.Status)
		}
		for _, r := range payload.SearchResult.Results {
			if r.BillID == 0 || r.Relevance < 50 {
				continue
			}
			fetched++
			extID := fmt.Sprintf("%d", r.BillID)
			var exists bool
			if e := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pipeline.documents
				WHERE source_id=$1 AND external_id=$2 AND change_hash=$3)`,
				sourceID, extID, r.ChangeHash).Scan(&exists); e != nil {
				return fetched, added, e
			}
			if exists {
				continue
			}
			if hydrated >= hydrateBudget {
				continue // picked up on the next run
			}
			bu := fmt.Sprintf("https://api.legiscan.com/?key=%s&op=getBill&id=%d", key, r.BillID)
			br, e := client.Get(bu)
			if e != nil {
				fmt.Fprintln(os.Stderr, "legiscan getBill", r.BillID, ":", e)
				continue
			}
			bb, e := io.ReadAll(br.Body)
			br.Body.Close()
			if e != nil {
				continue
			}
			var bp struct {
				Status string `json:"status"`
				Bill   struct {
					BillID     int    `json:"bill_id"`
					State      string `json:"state"`
					BillNumber string `json:"bill_number"`
					Title      string `json:"title"`
					URL        string `json:"url"`
					StatusDate string `json:"status_date"`
				} `json:"bill"`
			}
			if json.Unmarshal(bb, &bp) != nil || bp.Status != "OK" || bp.Bill.BillID == 0 {
				fmt.Fprintln(os.Stderr, "legiscan getBill", r.BillID, ": bad payload")
				continue
			}
			hydrated++
			var pub any
			if bp.Bill.StatusDate != "" {
				pub = bp.Bill.StatusDate
			}
			ok, e := insertDoc(db, sourceID, extID, r.ChangeHash, bp.Bill.URL,
				bp.Bill.State+" "+bp.Bill.BillNumber+": "+bp.Bill.Title, pub, bb)
			if e != nil {
				return fetched, added, e
			}
			if ok {
				added++
			}
			time.Sleep(500 * time.Millisecond)
		}
		if page >= payload.SearchResult.Summary.PageTotal {
			break
		}
		time.Sleep(2 * time.Second)
	}
	return fetched, added, nil
}

// gdelt pulls the last 24h of global news via GDELT's raw 15-minute GKG
// files (data.gdeltproject.org, no throttle), filtered to AI relevance.
// DISCOVERY layer only: news is never fact evidence. Each matched line
// carries persons/orgs/themes, the seed data for AI-people and AI-orgs.
func gdelt(db *sql.DB, sourceID int) (fetched, added int, err error) {
	client := &http.Client{Timeout: 90 * time.Second}
	now := time.Now().UTC().Truncate(15 * time.Minute)
	for i := 96; i >= 1; i-- { // last 24h of 15-min slices
		ts := now.Add(-time.Duration(i) * 15 * time.Minute).Format("20060102150405")
		url := "http://data.gdeltproject.org/gdeltv2/" + ts + ".gkg.csv.zip"
		resp, e := client.Get(url)
		if e != nil {
			continue // transient slice failure; the day's other 95 carry it
		}
		body, e := io.ReadAll(resp.Body)
		resp.Body.Close()
		if e != nil || resp.StatusCode != 200 {
			continue
		}
		zr, e := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if e != nil || len(zr.File) == 0 {
			continue
		}
		f, e := zr.File[0].Open()
		if e != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			low := strings.ToLower(line)
			if !strings.Contains(low, "artificial intelligence") && !strings.Contains(low, "artificialintelligence") {
				continue
			}
			c := strings.Split(line, "\t")
			if len(c) < 15 {
				continue
			}
			docURL := c[4]
			if !strings.HasPrefix(docURL, "http") {
				continue
			}
			fetched++
			title := ""
			if j := strings.Index(line, "<PAGE_TITLE>"); j >= 0 {
				if k := strings.Index(line[j:], "</PAGE_TITLE>"); k > 12 {
					title = line[j+12 : j+k]
				}
			}
			// THE STORY ITSELF MUST BE ABOUT AI, NOT MERELY TAGGED AI.
			//
			// The cheap line-level test above is a prefilter over an 8MB CSV
			// row, and it matches the THEMES column as readily as the article.
			// GDELT tags a tourism communique that mentions technology with an
			// AI theme, the line contains "artificial intelligence", and a
			// story about the Jaipur Declaration entered this site's AI news.
			// On 2026-08-23 the daily briefing led with BRICS tourism
			// ministers and Amber Fort; two of its ten stories were about AI.
			//
			// So relevance is now decided on the TITLE, which is the article's
			// own claim about itself, not on metadata a third party attached.
			// A record with no title cannot make that claim and is dropped:
			// this page would rather carry less than carry noise.
			if !twoaiTitleIsAI(title) {
				continue
			}
			// Full raw line retained per retention policy: everything the
			// pipeline downloads is kept for future LLM development.
			meta := map[string]string{"url": docURL, "domain": c[3], "date": c[1],
				"persons": trunc(c[11], 800), "orgs": trunc(c[13], 800), "themes": trunc(c[7], 800), "title": title,
				"line": line}
			raw, _ := json.Marshal(meta)
			uh := sha256.Sum256([]byte(docURL))
			id := hex.EncodeToString(uh[:])[:32]
			var pub any
			// GDELT GKG stamps are YYYYMMDDHHMMSS. The date alone was being
			// stored, which flattened every story's coverage into a single
			// undifferentiated day and made an hour-level timeline on the site
			// impossible without inventing times. Keep the full stamp; the
			// column is timestamptz and always could have held it.
			if len(c[1]) >= 14 {
				pub = c[1][:4] + "-" + c[1][4:6] + "-" + c[1][6:8] + "T" +
					c[1][8:10] + ":" + c[1][10:12] + ":" + c[1][12:14] + "Z"
			} else if len(c[1]) >= 8 {
				pub = c[1][:4] + "-" + c[1][4:6] + "-" + c[1][6:8]
			}
			ok, e := insertDoc(db, sourceID, id, id, docURL, title, pub, raw)
			if e != nil {
				f.Close()
				return fetched, added, e
			}
			if ok {
				added++
			}
		}
		f.Close()
		time.Sleep(300 * time.Millisecond)
	}
	return fetched, added, nil
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// twoaiAIWordRe holds the surface forms an AI story actually uses in its
// headline. Deliberately concrete: "ai" as a standalone token, the spelled-out
// phrase, and the named technologies. Broad tech words like "algorithm",
// "data" or "digital" are excluded because they are what let the tourism and
// green-computing stories in.
var twoaiAIWordRe = regexp.MustCompile(`(?i)\b(a\.?i\.?|artificial intelligence|machine learning|deep learning|neural network|large language model|llm|llms|generative ai|genai|chatbot|chatgpt|openai|anthropic|deepmind|copilot|gemini|claude|llama|transformer model|foundation model|agentic|robotaxi|humanoid robot|self-driving|autonomous vehicle|computer vision|speech recognition|deepfake|algorithmic bias)\b`)

// twoaiTitleIsAI reports whether a headline is about AI on its own terms.
func twoaiTitleIsAI(title string) bool {
	t := strings.TrimSpace(title)
	if t == "" {
		return false
	}
	return twoaiAIWordRe.MatchString(t)
}

// twoaiWireTitle normalises a headline so the same wire item recognises
// itself across every outlet that republished it.
//
// WHY RANKING BY OUTLET COUNT WAS BACKWARDS. Stories rank by how many outlets
// carried them, which is a fair proxy for importance only when the outlets are
// independent newsrooms. On 2026-08-23 the top story was carried by 59
// "separate" domains - britainnews.net, irishsun.com, zimbabwestar.com,
// middleeaststar.com and 55 more - which is one syndication network wearing 59
// names. A tourism communique beat every real AI story, and the more
// mechanically an item was mirrored the higher it ranked.
//
// THE TEST IS BEHAVIOURAL, NOT NAME-BASED. The obvious fix, matching domains
// that look like a farm, is a trap: the pattern that catches zimbabwestar.com
// also catches nytimes.com and washingtonpost.com, which would merge the New
// York Times and the LA Times into one "outlet" and undercount exactly the
// coverage worth trusting. Verified before shipping, and discarded.
//
// What actually separates a mirror from a newsroom is the headline. Wire
// mirrors republish character-identical titles; independent newsrooms write
// their own. So the unit of coverage is the DISTINCT HEADLINE, and a hundred
// verbatim copies count once no matter who published them or what they are
// called. The trailing " - Outlet Name" that syndication platforms append is
// stripped first, so one paper group's house style does not fake variety.
var twoaiWirePunctRe = regexp.MustCompile(`[^a-z0-9]+`)
var twoaiWireTailRe = regexp.MustCompile(`(?i)\s*(&#x2013;|&#8211;|[-\x{2013}\x{2014}|])\s*[^-|\x{2013}\x{2014}]{1,40}$`)

func twoaiWireTitle(title string) string {
	t := strings.ToLower(strings.TrimSpace(title))
	// Strip one trailing " - Publication" attribution.
	t = twoaiWireTailRe.ReplaceAllString(t, "")
	t = strings.Trim(twoaiWirePunctRe.ReplaceAllString(t, " "), " ")
	return t
}

// publishNews clusters the day's gdelt coverage into top stories and
// publishes news/news.json to srj-content. Stories rank by breadth of
// coverage (unique outlets). Same GitHub-commit flow as before.
func publishNews(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	rows, err := db.Query(`SELECT d.title, d.url, to_char(d.published_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), d.raw->>'domain', coalesce(d.raw->>'persons',''), coalesce(d.raw->>'orgs','')
		FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
		WHERE s.key='gdelt' AND d.title <> '' AND d.fetched_at > now() - interval '36 hours'
		ORDER BY d.id DESC LIMIT 600`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type art struct{ Title, URL, Date, Domain, persons, orgs string }
	var arts []art
	// Per-URL summary and lead text, for the story summary at the top of each
	// news page. Own-words summaries only; the site never republishes bodies.
	type docText struct{ summary, text string }
	docs := map[string]docText{}
	{
		drows, derr := db.Query(`SELECT d.url, COALESCE(d.summary,''), COALESCE(substr(d.fulltext,1,12000),'')
			FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
			WHERE s.key='gdelt' AND d.fetched_at > now() - interval '36 hours'
			AND (d.summary IS NOT NULL OR d.fulltext IS NOT NULL)`)
		if derr == nil {
			for drows.Next() {
				var u, sm, tx string
				if drows.Scan(&u, &sm, &tx) == nil {
					docs[u] = docText{summary: sm, text: tx}
				}
			}
			drows.Close()
		}
	}
	// THE FILTER RUNS HERE TOO, NOT ONLY AT INGEST.
	//
	// Deciding relevance at ingest stops new junk entering, but it cannot
	// clean the corpus already sitting inside the 36-hour window: 171 of the
	// 268 records in it on 2026-08-23 arrived under the old rule, so the
	// briefing kept leading with BRICS tourism ministers after the ingest fix
	// shipped. Purging those rows was the obvious alternative and was
	// rejected - archive pages are built from them, and deleting the rows
	// would delete published URLs, which is the exact failure this project
	// spent the morning repairing.
	//
	// Filtering at publish leaves the corpus intact, keeps every archived page
	// answering, and means the briefing corrects itself on the next run rather
	// than waiting a day and a half for bad records to age out.
	skipped := 0
	for rows.Next() {
		var a art
		var d sql.NullString
		if rows.Scan(&a.Title, &a.URL, &d, &a.Domain, &a.persons, &a.orgs) == nil {
			if !twoaiTitleIsAI(a.Title) {
				skipped++
				continue
			}
			a.Date = d.String
			arts = append(arts, a)
		}
	}
	if skipped > 0 {
		fmt.Printf("publishNews: %d non-AI headlines skipped, %d kept\n", skipped, len(arts))
	}

	stop := map[string]bool{"the": true, "a": true, "an": true, "of": true, "to": true, "in": true, "on": true, "for": true, "and": true, "with": true, "as": true, "at": true, "by": true, "is": true, "its": true, "ai": true, "artificial": true, "intelligence": true, "new": true, "how": true, "what": true, "why": true}
	toks := func(s string) map[string]bool {
		m := map[string]bool{}
		w := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !('a' <= r && r <= 'z' || '0' <= r && r <= '9') })
		for _, x := range w {
			if len(x) > 2 && !stop[x] {
				m[x] = true
			}
		}
		return m
	}
	sim := func(a, b map[string]bool) float64 {
		n := 0
		for k := range a {
			if b[k] {
				n++
			}
		}
		d := len(a)
		if len(b) < d {
			d = len(b)
		}
		if d == 0 {
			return 0
		}
		return float64(n) / float64(d)
	}

	type cluster struct {
		arts []art
		tk   map[string]bool
	}
	var cls []*cluster
	for _, a := range arts {
		tk := toks(a.Title)
		if len(tk) < 3 {
			continue
		}
		placed := false
		for _, c := range cls {
			if sim(tk, c.tk) >= 0.6 {
				c.arts = append(c.arts, a)
				for k := range tk {
					c.tk[k] = true
				}
				placed = true
				break
			}
		}
		if !placed {
			cls = append(cls, &cluster{arts: []art{a}, tk: tk})
		}
	}
	// Independent coverage, measured in distinct headlines. A wire item
	// mirrored across fifty domains contributes one, so breadth reflects how
	// many newsrooms wrote about a story rather than how efficiently a feed
	// was republished. See twoaiWireTitle.
	newsrooms := func(c *cluster) int {
		m := map[string]bool{}
		for _, a := range c.arts {
			m[twoaiWireTitle(a.Title)] = true
		}
		return len(m)
	}
	domains := func(c *cluster) int {
		m := map[string]bool{}
		for _, a := range c.arts {
			m[a.Domain] = true
		}
		return len(m)
	}
	sort.Slice(cls, func(i, j int) bool {
		if newsrooms(cls[i]) != newsrooms(cls[j]) {
			return newsrooms(cls[i]) > newsrooms(cls[j])
		}
		if domains(cls[i]) != domains(cls[j]) {
			return domains(cls[i]) > domains(cls[j])
		}
		return len(cls[i].arts) > len(cls[j].arts)
	})
	if len(cls) > 10 {
		cls = cls[:10]
	}

	slugify := func(s string) string {
		s = strings.ToLower(s)
		var b strings.Builder
		dash := false
		for _, r := range s {
			if 'a' <= r && r <= 'z' || '0' <= r && r <= '9' {
				b.WriteRune(r)
				dash = false
			} else if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
			if b.Len() >= 60 {
				break
			}
		}
		return strings.Trim(b.String(), "-")
	}
	top := func(field func(art) string, n int) func(*cluster) []string {
		return func(c *cluster) []string {
			cnt := map[string]int{}
			for _, a := range c.arts {
				for _, p := range strings.Split(field(a), ";") {
					p = strings.TrimSpace(p)
					if p != "" {
						cnt[p]++
					}
				}
			}
			// Initialised, not declared nil. A nil slice marshals to JSON null,
			// not [], and the site prerenders one page per story: on 2026-07-28 a
			// story with no organizations shipped "Orgs": null, the Astro template
			// called .slice on it, and that single record failed the entire site
			// build. Empty must mean empty, everywhere this package emits JSON.
			ks := []string{}
			for k := range cnt {
				ks = append(ks, k)
			}
			sort.Slice(ks, func(i, j int) bool { return cnt[ks[i]] > cnt[ks[j]] })
			if len(ks) > n {
				ks = ks[:n]
			}
			return ks
		}
	}
	topPersons := top(func(a art) string { return a.persons }, 6)
	topOrgs := top(func(a art) string { return a.orgs }, 6)

	type story struct {
		Slug, Headline            string
		Summary                   string
		SummaryURL, SummaryDomain string
		ArticleCount, DomainCount int
		Domains                   []string
		Persons, Orgs             []string
		Articles                  []map[string]string
	}
	var stories []story
	big := []string{}
	seen := map[string]bool{}
	for _, c := range cls {
		h := c.arts[0].Title
		sl := slugify(h)
		if sl == "" || seen[sl] {
			continue
		}
		seen[sl] = true
		dm := map[string]bool{}
		dl := []string{}
		as := []map[string]string{}
		for _, a := range c.arts {
			if !dm[a.Domain] {
				dm[a.Domain] = true
				dl = append(dl, a.Domain)
			}
			if len(as) < 15 {
				as = append(as, map[string]string{"Title": a.Title, "URL": a.URL, "Domain": a.Domain, "Date": a.Date})
			}
		}
		if len(dl) > 12 {
			dl = dl[:12]
		}
		summary, sumURL, sumDomain := "", "", ""
		for _, a := range c.arts {
			dt, okd := docs[a.URL]
			if !okd || (dt.summary == "" && dt.text == "") {
				continue
			}
			if dt.summary == "" {
				s2, serr := anthropicSummarize(h, dt.text)
				if serr != nil {
					fmt.Fprintln(os.Stderr, "publish_news summarize:", serr)
					continue
				}
				dt.summary = s2
				db.Exec(`UPDATE pipeline.documents SET summary=$1 WHERE url=$2`, s2, a.URL)
			}
			summary, sumURL, sumDomain = dt.summary, a.URL, a.Domain
			break
		}
		stories = append(stories, story{Slug: sl, Headline: h, Summary: summary, SummaryURL: sumURL, SummaryDomain: sumDomain, ArticleCount: len(c.arts), DomainCount: len(dm), Domains: dl, Persons: topPersons(c), Orgs: topOrgs(c), Articles: as})
		if len(big) < 4 {
			big = append(big, h)
		}
	}

	payload, _ := json.MarshalIndent(map[string]any{
		"generated":   time.Now().UTC().Format(time.RFC3339),
		"date":        time.Now().UTC().Format("2006-01-02"),
		"big_picture": big,
		"stories":     stories,
	}, "", " ")

	// THE GATE.
	//
	// This publish step commits straight to the default branch of srj-content,
	// and a push to srj-content is itself the site deploy trigger. There is no
	// review, no CI, and no staging step in between: whatever this function
	// writes is on the public site within minutes.
	//
	// That is fine while the data is well formed and unacceptable when it is
	// not, which is not hypothetical. On 2026-07-28 a single malformed story
	// failed the entire site build and blocked every deploy, including unrelated
	// ones, until the templates were patched by hand.
	//
	// So nothing is published unless it passes. A refusal to publish leaves
	// yesterday's briefing live, which is a good outcome: slightly stale beats
	// broken, and the failure is loud in the cron logs rather than silent.
	if err := validateNews(payload); err != nil {
		return fmt.Errorf("refusing to publish malformed news.json: %w", err)
	}

	api := "https://api.github.com/repos/srjordan6/srj-content/contents/news/news.json"
	client := &http.Client{Timeout: 60 * time.Second}
	gh := func(method, url string, body []byte) (*http.Response, error) {
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		return client.Do(req)
	}
	sha := ""
	if resp, e := gh("GET", api, nil); e == nil {
		var cur struct {
			SHA string `json:"sha"`
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && json.Unmarshal(b, &cur) == nil {
			sha = cur.SHA
		}
	}
	put := map[string]any{"message": "pipeline: daily news refresh",
		"content": base64.StdEncoding.EncodeToString(payload)}
	if sha != "" {
		put["sha"] = sha
	}
	pb, _ := json.Marshal(put)
	resp, e := gh("PUT", api, pb)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github PUT %d: %.200s", resp.StatusCode, b)
	}
	return nil
}

// validateNews is the publish gate. It re-reads the marshalled payload the way
// the site will read it, and rejects anything the Astro build cannot survive.
//
// It parses the bytes rather than inspecting the in-memory structs on purpose.
// The failure this exists to prevent was a marshalling artefact, a nil slice
// becoming null, which is invisible from the Go side and only appears once the
// value has been through encoding/json. Checking the structs would have missed
// it. Check what actually ships.
//
// Every rule here maps to something that breaks a real page:
//
//	Slug         the story's URL. Empty means a page at a bad path.
//	Headline     the h1 and the <title>. Empty means an untitled page.
//	Articles     the entire body of the story page. Empty means a blank page.
//	null arrays  the 2026-07-28 build failure, exactly.
//	duplicates   two stories claiming one URL; the later one silently wins.
func validateNews(payload []byte) error {
	var doc struct {
		Generated  string             `json:"generated"`
		Date       string             `json:"date"`
		BigPicture *[]string          `json:"big_picture"`
		Stories    *[]json.RawMessage `json:"stories"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return fmt.Errorf("payload is not valid JSON: %w", err)
	}
	if doc.Generated == "" || doc.Date == "" {
		return fmt.Errorf("missing generated or date")
	}
	if doc.BigPicture == nil {
		return fmt.Errorf("big_picture is null, must be an array")
	}
	if doc.Stories == nil {
		return fmt.Errorf("stories is null, must be an array")
	}
	if len(*doc.Stories) == 0 {
		return fmt.Errorf("no stories: publishing an empty briefing would blank the page")
	}

	// Pointer fields distinguish "absent or null" from "present but empty",
	// which is the whole point of this check.
	seen := map[string]bool{}
	for i, raw := range *doc.Stories {
		var s struct {
			Slug         string               `json:"Slug"`
			Headline     string               `json:"Headline"`
			ArticleCount int                  `json:"ArticleCount"`
			DomainCount  int                  `json:"DomainCount"`
			Domains      *[]string            `json:"Domains"`
			Persons      *[]string            `json:"Persons"`
			Orgs         *[]string            `json:"Orgs"`
			Articles     *[]map[string]string `json:"Articles"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("story %d does not parse: %w", i, err)
		}
		where := fmt.Sprintf("story %d (%q)", i, s.Slug)
		if s.Slug == "" {
			return fmt.Errorf("%s: empty Slug, would render at a broken URL", where)
		}
		if seen[s.Slug] {
			return fmt.Errorf("%s: duplicate Slug, two stories cannot share one URL", where)
		}
		seen[s.Slug] = true
		if strings.TrimSpace(s.Headline) == "" {
			return fmt.Errorf("%s: empty Headline, would render an untitled page", where)
		}
		for name, arr := range map[string]*[]string{
			"Domains": s.Domains, "Persons": s.Persons, "Orgs": s.Orgs,
		} {
			if arr == nil {
				return fmt.Errorf("%s: %s is null, must be [] (this is the 2026-07-28 build break)", where, name)
			}
		}
		if s.Articles == nil {
			return fmt.Errorf("%s: Articles is null, must be []", where)
		}
		if len(*s.Articles) == 0 {
			return fmt.Errorf("%s: no articles, the story page would have no body", where)
		}
		for j, a := range *s.Articles {
			if strings.TrimSpace(a["URL"]) == "" || strings.TrimSpace(a["Title"]) == "" {
				return fmt.Errorf("%s: article %d missing URL or Title", where, j)
			}
		}
		if s.DomainCount < 1 || s.ArticleCount < 1 {
			return fmt.Errorf("%s: DomainCount and ArticleCount must both be at least 1", where)
		}
	}
	return nil
}

// publishLegislation writes the AI legislation tracker to srj-content.
//
// Source is LegiScan, which is the one regulatory adapter whose output is
// genuinely on topic: on 2026-07-28, 91 of its 100 stored documents were AI
// bills, against 15 of 101 for the Federal Register. It also carries exactly
// what a tracker needs and news.json does not: a jurisdiction, a bill number, a
// plain-language legislative stage, and the date that stage was reached.
//
// The AI filter is applied here as well as at fetch time, because the corpus is
// append-only and already holds rows fetched before the filter existed.
//
// Stage is LegiScan's own last_action string, verbatim. It is deliberately not
// mapped onto a tidy enum like "Committee" or "Passed": the mapping would be a
// guess dressed as a status, and "Signed by Governor" already says more than
// any bucket would.
func publishLegislation(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}

	rows, err := db.Query(`
		SELECT DISTINCT ON (d.raw->>'state', d.raw->>'bill_number')
		       coalesce(d.raw->>'state',''), coalesce(d.raw->>'bill_number',''),
		       d.title, d.url, coalesce(d.raw->>'last_action',''),
		       coalesce(d.raw->>'last_action_date',''), coalesce(d.raw->>'text_url','')
		FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
		WHERE s.key='legiscan' AND d.title <> ''
		ORDER BY d.raw->>'state', d.raw->>'bill_number', d.raw->>'last_action_date' DESC NULLS LAST, d.id DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type bill struct {
		State, Number, Title, URL, LastAction, LastActionDate, TextURL string
	}
	bills := []bill{}
	for rows.Next() {
		var b bill
		if rows.Scan(&b.State, &b.Number, &b.Title, &b.URL, &b.LastAction, &b.LastActionDate, &b.TextURL) != nil {
			continue
		}
		if b.State == "" || b.Number == "" || !mentionsAI(b.Title) {
			continue
		}
		bills = append(bills, b)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Most recently acted first: a tracker is only useful if movement is at
	// the top. Bills with no recorded action sort last rather than being
	// dropped, since "introduced, nothing since" is itself a status.
	sort.SliceStable(bills, func(i, j int) bool {
		if bills[i].LastActionDate != bills[j].LastActionDate {
			return bills[i].LastActionDate > bills[j].LastActionDate
		}
		if bills[i].State != bills[j].State {
			return bills[i].State < bills[j].State
		}
		return bills[i].Number < bills[j].Number
	})

	states := map[string]bool{}
	for _, b := range bills {
		states[b.State] = true
	}

	payload, _ := json.MarshalIndent(map[string]any{
		"generated":     time.Now().UTC().Format(time.RFC3339),
		"date":          time.Now().UTC().Format("2006-01-02"),
		"source":        "LegiScan",
		"jurisdictions": len(states),
		"count":         len(bills),
		"bills":         bills,
	}, "", " ")

	if err := validateLegislation(payload); err != nil {
		return fmt.Errorf("refusing to publish malformed legislation.json: %w", err)
	}
	return putToContent(tok, "legislation/legislation.json",
		"pipeline: AI legislation tracker refresh", payload)
}

// validateLegislation is the same gate discipline as validateNews: check the
// bytes that will ship, refuse rather than publish, and let yesterday's file
// stand. Every rule maps to something that breaks a rendered row.
func validateLegislation(payload []byte) error {
	var doc struct {
		Generated string             `json:"generated"`
		Date      string             `json:"date"`
		Bills     *[]json.RawMessage `json:"bills"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return fmt.Errorf("payload is not valid JSON: %w", err)
	}
	if doc.Generated == "" || doc.Date == "" {
		return fmt.Errorf("missing generated or date")
	}
	if doc.Bills == nil {
		return fmt.Errorf("bills is null, must be an array")
	}
	if len(*doc.Bills) == 0 {
		return fmt.Errorf("no bills: publishing an empty tracker would blank the page")
	}
	seen := map[string]bool{}
	for i, raw := range *doc.Bills {
		var b struct {
			State, Number, Title, URL string
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Errorf("bill %d does not parse: %w", i, err)
		}
		key := b.State + " " + b.Number
		if strings.TrimSpace(b.State) == "" || strings.TrimSpace(b.Number) == "" {
			return fmt.Errorf("bill %d: missing state or bill number, the row's identity", i)
		}
		if seen[key] {
			return fmt.Errorf("bill %d: duplicate %s, one bill cannot occupy two rows", i, key)
		}
		seen[key] = true
		if strings.TrimSpace(b.Title) == "" {
			return fmt.Errorf("bill %d (%s): empty title", i, key)
		}
		if !strings.HasPrefix(b.URL, "http") {
			return fmt.Errorf("bill %d (%s): URL is not a link, the row would cite nothing", i, key)
		}
	}
	return nil
}

// publishLeaderboard writes the model leaderboard to srj-content.
//
// WHY THIS ADAPTER EXISTS, and it is worth stating plainly. The request that
// prompted it arrived with hand-pasted benchmark figures: "Claude Opus 4.8
// leads at ~1,580 Elo, followed by GPT-5.5 Pro and Gemini 3.1 Pro." Checked
// against arena.ai the same day, every clause was wrong. The actual #1 was
// claude-fable-5 at 1508. Opus 4.8 Thinking sat at #14 on 1484. GPT-5.5 Pro was
// not on the text board at all. The 1580 figure appears to be the CODE board's
// range misread as the text board's.
//
// The numbers traced back to an SEO content farm, not to arena.ai. That is the
// whole argument for fetching rather than typing: a leaderboard is the most
// perishable content on the site, it moves weekly, and a hand-pasted table is
// wrong within days and then stays wrong. On a site whose competitive position
// is correction rather than currency, publishing a stale table copied from an
// aggregator would undo the thing the governance library is for.
//
// SOURCE. arena.ai (formerly LMSYS Chatbot Arena) publishes no API; its own
// mirror repo says so. This reads the daily GitHub snapshot, which carries the
// upstream fetched_at and source_url in every file, so the provenance chain
// stays legible: arena.ai is the source, the mirror is the access route, and
// both are named on the rendered page.
//
// TWO UPSTREAM FACTS LEARNED BY RUNNING IT, both of which broke the first
// version and neither of which is documented in the mirror's schema:
//
//  1. Scores arrive as JSON floats (1508.0), not integers, despite the schema
//     table saying int. Decoding into *int fails the whole board silently.
//  2. The agent board is RANK-ONLY. All ten entries carry a null score, ci, and
//     votes. That is a real property of the upstream board, not corruption, so
//     the gate must allow it while still catching a scored board that has lost
//     its numbers.
//
// The second is handled by requiring internal consistency rather than presence:
// a board must be all-scored or all-unscored. Half a board losing its scores is
// a malformation; a board that never had them is a format.
//
// WHAT IS DELIBERATELY NOT COLLECTED. MMLU-Pro, SWE-bench Verified, GPQA
// Diamond, MATH, tokens-per-second, and context-window figures were all in the
// original paste. None are in this feed, none could be verified against a
// primary source in the same pass, and the ones that could be checked were
// wrong. They are omitted rather than carried across on trust. If a verified
// machine-readable source for any of them is found later, it gets its own
// adapter and its own gate. An unsourced number on this site is worse than a
// missing one.
func publishLeaderboard() error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	const mirror = "https://raw.githubusercontent.com/oolong-tea-2026/arena-ai-leaderboards/main/data"
	client := &http.Client{Timeout: 60 * time.Second}

	get := func(url string) ([]byte, error) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("GET %s: %d", url, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}

	// latest.json is the pointer to the newest dated snapshot directory.
	// Following it rather than constructing today's date means a day the
	// upstream fetch failed yields yesterday's board, correctly stamped,
	// instead of a 404 that would blank the page.
	var ptr struct {
		Date string `json:"date"`
		Path string `json:"path"`
	}
	b, err := get(mirror + "/latest.json")
	if err != nil {
		return fmt.Errorf("latest pointer: %w", err)
	}
	if err := json.Unmarshal(b, &ptr); err != nil {
		return fmt.Errorf("latest pointer parse: %w", err)
	}
	if ptr.Path == "" {
		return fmt.Errorf("latest pointer carries no path")
	}

	// All numerics are float64 because the upstream emits floats regardless
	// of what its schema table claims. Rendering formats them.
	type model struct {
		Rank    int      `json:"rank"`
		Model   string   `json:"model"`
		Vendor  *string  `json:"vendor"`
		License *string  `json:"license"`
		Score   *float64 `json:"score"`
		CI      *float64 `json:"ci"`
		Votes   *float64 `json:"votes"`
	}
	type board struct {
		Key       string  `json:"key"`
		Label     string  `json:"label"`
		Note      string  `json:"note"`
		Scored    bool    `json:"scored"`
		SourceURL string  `json:"source_url"`
		FetchedAt string  `json:"fetched_at"`
		Count     int     `json:"count"`
		Models    []model `json:"models"`
	}

	// Only the boards a Chat & General LLM reader is actually choosing
	// between. The image, video, and edit boards belong to different
	// catalog categories and would be noise here.
	wanted := []struct{ key, label, note string }{
		{"text", "Overall text and chat",
			"Head-to-head human preference on general conversation. The closest thing the field has to a general-purpose ranking."},
		{"code", "Code generation",
			"The same vote mechanic restricted to coding prompts. Ranks differ sharply from the text board, which is the reason to read both."},
		{"agent", "Agentic use",
			"Multi-step tool-using tasks rather than single answers. The board that matters if the model will act rather than reply. Published as an order only, with no ratings."},
		{"vision", "Vision and multimodal",
			"Image understanding and mixed text-image prompts."},
		{"search", "Search-grounded answers",
			"Models answering with live retrieval, where citation quality matters as much as fluency."},
	}

	boards := []board{}
	for _, w := range wanted {
		raw, err := get(fmt.Sprintf("%s/%s/%s.json", mirror, ptr.Path, w.key))
		if err != nil {
			// A single missing board is not fatal. The upstream index
			// varies by day and a partial page beats no page.
			fmt.Fprintf(os.Stderr, "leaderboard: skipping %s: %v\n", w.key, err)
			continue
		}
		var f struct {
			Meta struct {
				Leaderboard string `json:"leaderboard"`
				SourceURL   string `json:"source_url"`
				FetchedAt   string `json:"fetched_at"`
				ModelCount  int    `json:"model_count"`
			} `json:"meta"`
			Models []model `json:"models"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			fmt.Fprintf(os.Stderr, "leaderboard: %s does not parse: %v\n", w.key, err)
			continue
		}
		// Top 12 per board. The full boards run past 100 models; a
		// reference page that reprints all of them is a worse read than
		// the source, and the source is linked on every board.
		top := f.Models
		if len(top) > 12 {
			top = top[:12]
		}
		scored := len(top) > 0
		for _, m := range top {
			if m.Score == nil {
				scored = false
				break
			}
		}
		boards = append(boards, board{
			Key: w.key, Label: w.label, Note: w.note, Scored: scored,
			SourceURL: f.Meta.SourceURL, FetchedAt: f.Meta.FetchedAt,
			Count: f.Meta.ModelCount, Models: top,
		})
	}

	payload, _ := json.MarshalIndent(map[string]any{
		"generated":   time.Now().UTC().Format(time.RFC3339),
		"date":        ptr.Date,
		"source":      "arena.ai (formerly LMSYS Chatbot Arena)",
		"source_url":  "https://arena.ai/leaderboard/",
		"access_note": "arena.ai publishes no API. Read via the daily public snapshot mirror at github.com/oolong-tea-2026/arena-ai-leaderboards, which preserves upstream fetched_at and source_url per board.",
		"method":      "Crowdsourced blind pairwise voting, scored with a Bradley-Terry model and reported as an Elo-style rating with a 95 percent confidence interval. Gaps under roughly 10 points sit inside the noise floor and should not be read as a ranking.",
		"boards":      boards,
	}, "", " ")

	if err := validateLeaderboard(payload); err != nil {
		return fmt.Errorf("refusing to publish malformed leaderboard.json: %w", err)
	}
	return putToContent(tok, "leaderboard/leaderboard.json",
		"pipeline: model leaderboard refresh", payload)
}

// validateLeaderboard applies the same gate discipline as validateNews and
// validateLegislation: parse the bytes that will actually ship, refuse rather
// than publish, and leave yesterday's file standing. Stale beats broken.
//
// Every rule maps to something visibly wrong on the rendered page. A board with
// no models renders an empty table. A missing fetched_at strips the page of the
// one thing that makes a perishable table trustworthy, which is the date it was
// true. Ranks that skip or repeat render a table that silently misorders.
//
// The scored rule is consistency, not presence, because the agent board is
// legitimately rank-only upstream. All-scored and all-unscored are both valid;
// a mix means a scored board lost its numbers mid-fetch.
func validateLeaderboard(payload []byte) error {
	var doc struct {
		Generated string `json:"generated"`
		Date      string `json:"date"`
		Source    string `json:"source"`
		Boards    *[]struct {
			Key       string `json:"key"`
			Label     string `json:"label"`
			Scored    bool   `json:"scored"`
			FetchedAt string `json:"fetched_at"`
			SourceURL string `json:"source_url"`
			Models    *[]struct {
				Rank  int      `json:"rank"`
				Model string   `json:"model"`
				Score *float64 `json:"score"`
			} `json:"models"`
		} `json:"boards"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return fmt.Errorf("payload is not valid JSON: %w", err)
	}
	if doc.Generated == "" || doc.Date == "" || doc.Source == "" {
		return fmt.Errorf("missing generated, date, or source")
	}
	if doc.Boards == nil {
		return fmt.Errorf("boards is null, must be an array")
	}
	if len(*doc.Boards) == 0 {
		return fmt.Errorf("no boards: publishing this would blank the leaderboard section")
	}
	seen := map[string]bool{}
	for i, b := range *doc.Boards {
		if strings.TrimSpace(b.Key) == "" || strings.TrimSpace(b.Label) == "" {
			return fmt.Errorf("board %d: missing key or label", i)
		}
		if seen[b.Key] {
			return fmt.Errorf("board %d: duplicate key %q", i, b.Key)
		}
		seen[b.Key] = true
		if strings.TrimSpace(b.FetchedAt) == "" {
			return fmt.Errorf("board %q: no fetched_at, an undated leaderboard is not a fact", b.Key)
		}
		if !strings.HasPrefix(b.SourceURL, "http") {
			return fmt.Errorf("board %q: source_url is not a link, the table would cite nothing", b.Key)
		}
		if b.Models == nil {
			return fmt.Errorf("board %q: models is null, must be []", b.Key)
		}
		if len(*b.Models) == 0 {
			return fmt.Errorf("board %q: no models, the table would render empty", b.Key)
		}
		for j, m := range *b.Models {
			if strings.TrimSpace(m.Model) == "" {
				return fmt.Errorf("board %q model %d: empty model name", b.Key, j)
			}
			if m.Rank != j+1 {
				return fmt.Errorf("board %q model %d (%s): rank is %d, ranks must run 1..n without gaps or repeats",
					b.Key, j, m.Model, m.Rank)
			}
			if b.Scored && m.Score == nil {
				return fmt.Errorf("board %q model %d (%s): board is scored but this row has none, so a scored board has lost its numbers",
					b.Key, j, m.Model)
			}
			if !b.Scored && m.Score != nil {
				return fmt.Errorf("board %q model %d (%s): board is marked unscored but carries a score",
					b.Key, j, m.Model)
			}
		}
	}
	return nil
}

// putToContent writes one file to srj-content via the GitHub contents API,
// reading the current blob SHA first so the write is an update rather than a
// rejected create. Shared by every publish step.
func putToContent(tok, path, message string, payload []byte) error {
	return putToRepoTok(tok, "srjordan6/srj-content", path, message, payload)
}

// putToRepo is putToContent for any srjordan6 repo, reading GITHUB_TOKEN
// itself. Added for the favicons stage, which writes binary assets into
// srj-site public/ because no other connector in the toolchain can carry
// binary content to that repo.
func putToRepo(repo, path, message string, payload []byte) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	return putToRepoTok(tok, repo, path, message, payload)
}

func putToRepoTok(tok, repo, path, message string, payload []byte) error {
	api := "https://api.github.com/repos/" + repo + "/contents/" + path
	client := &http.Client{Timeout: 60 * time.Second}
	gh := func(method, url string, body []byte) (*http.Response, error) {
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		return client.Do(req)
	}
	sha := ""
	if resp, e := gh("GET", api, nil); e == nil {
		var cur struct {
			SHA string `json:"sha"`
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && json.Unmarshal(b, &cur) == nil {
			sha = cur.SHA
			// Skip the write when the stored blob already matches: git blob
			// SHA1 is sha1("blob " + len + NUL + content). Keeps daily
			// re-runs of asset stages write-free.
			h := sha1.New()
			fmt.Fprintf(h, "blob %d", len(payload))
			h.Write([]byte{0})
			h.Write(payload)
			if fmt.Sprintf("%x", h.Sum(nil)) == sha {
				return nil
			}
		}
	}
	put := map[string]any{"message": message,
		"content": base64.StdEncoding.EncodeToString(payload)}
	if sha != "" {
		put["sha"] = sha
	}
	pb, _ := json.Marshal(put)
	resp, e := gh("PUT", api, pb)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github PUT %s %d: %.200s", path, resp.StatusCode, b)
	}
	return nil
}

// ---- intel: AI Lawsuit Database + AI intel sync ----------------------------
//
// Keeps the ai_lawsuits table current from CourtListener, fills docket numbers
// still marked pending, queues newly filed AI lawsuits into
// ai_lawsuit_candidates, and watches Hugging Face plus vendor feeds for new
// models and terminology into ai_intel_candidates. Results log to
// srj_intel_log. COURTLISTENER_TOKEN is required for docket-detail reads
// (search works anonymously); without it the refresh job logs and moves on.

func clGet(path string, params map[string]string, out any) error {
	req, err := http.NewRequest("GET", "https://www.courtlistener.com/api/rest/v4"+path, nil)
	if err != nil {
		return err
	}
	q := req.URL.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	req.URL.RawQuery = q.Encode()
	req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (srjconsultingservices.com)")
	if tok := os.Getenv("COURTLISTENER_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Token "+tok)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == 429 {
			// CourtListener sends Retry-After telling us exactly how long
			// its quota window has left. The old fixed 15/30/45s ladder
			// ignored it and burned all three attempts inside a window
			// that had longer to run, which is why the Aug 8 run lost four
			// docket refreshes while two succeeded. Honor the header when
			// present, capped so a long window cannot stall the whole run.
			wait := time.Duration(15*attempt) * time.Second
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, perr := strconv.Atoi(strings.TrimSpace(ra)); perr == nil && secs > 0 {
					wait = time.Duration(secs) * time.Second
					if wait > 90*time.Second {
						wait = 90 * time.Second
					}
				}
			}
			resp.Body.Close()
			time.Sleep(wait)
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("courtlistener %s: %s", path, resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return fmt.Errorf("courtlistener rate limited: %s", path)
}

type clSearch struct {
	Results []struct {
		CaseName     string `json:"caseName"`
		DocketNumber string `json:"docketNumber"`
		DocketID     int64  `json:"docket_id"`
		Court        string `json:"court"`
		DateFiled    string `json:"dateFiled"`
		Snippet      string `json:"snippet"`
	} `json:"results"`
}

var docketIDRe = regexp.MustCompile(`/docket/(\d+)/`)

// intelRefresh checks tracked cases for docket movement.
//
// SWEEP BUDGET. This used to walk every active case on every run, which was
// fine at 13 cases and broke at 39: CourtListener began rate limiting, each
// blocked fetch cost about 90 seconds of backoff, and the run stopped reaching
// twoai_build at all. Auto-promotion tripled the case count, so the sweep cost
// tripled with it, and the stages downstream paid for it.
//
// So the sweep is now budgeted and prioritised rather than exhaustive. Cases
// are ordered by how long it has been since we last saw them move, and only
// the first N are checked per run. A case that filed something yesterday is
// checked today; one dormant since 2025 waits its turn. Over a few days every
// case still gets checked, which is what daily docket monitoring actually
// requires, without any single run trying to do all of it.
func intelRefresh(db *sql.DB) (checked, updated int, err error) {
	// Budget per run. Raise it only if CourtListener stops rate limiting.
	const sweepLimit = 12
	rows, err := db.Query(`SELECT id, slug, courtlistener_url, COALESCE(latest_development_date::text,''), COALESCE(timeline::text,'[]')
		FROM ai_lawsuits WHERE is_active AND courtlistener_url IS NOT NULL
		ORDER BY docket_checked_at ASC NULLS FIRST, latest_development_date DESC NULLS LAST
		LIMIT $1`, sweepLimit)
	if err != nil {
		return 0, 0, err
	}
	type caseRow struct {
		id                 int64
		slug, clURL, since string
		timeline           string
	}
	var cases []caseRow
	for rows.Next() {
		var c caseRow
		if err := rows.Scan(&c.id, &c.slug, &c.clURL, &c.since, &c.timeline); err != nil {
			rows.Close()
			return checked, updated, err
		}
		cases = append(cases, c)
	}
	rows.Close()
	rateLimited := 0
	for _, c := range cases {
		m := docketIDRe.FindStringSubmatch(c.clURL)
		if m == nil {
			continue
		}
		did := m[1]
		checked++
		// Stamp the attempt before the fetch, so a case that rate limits does
		// not monopolise the front of the queue on the next run.
		db.Exec(`UPDATE ai_lawsuits SET docket_checked_at = now() WHERE id = $1`, c.id)
		var docket struct {
			DateLastFiling string `json:"date_last_filing"`
		}
		if err := clGet("/dockets/"+did+"/", nil, &docket); err != nil {
			fmt.Fprintln(os.Stderr, "intel refresh", c.slug, "docket fetch:", err)
			// Give up the sweep once CourtListener is clearly throttling us.
			// Each blocked fetch costs about ninety seconds of backoff, so
			// pushing on turns one throttled API into an hour-long run and
			// starves every stage downstream. The remaining cases keep their
			// place in the queue and are checked on the next run.
			if strings.Contains(err.Error(), "rate limited") {
				rateLimited++
				if rateLimited >= 2 {
					fmt.Fprintln(os.Stderr, "intel refresh: rate limited, ending sweep early")
					break
				}
			}
			time.Sleep(2 * time.Second)
			continue
		}
		if docket.DateLastFiling == "" || (c.since != "" && docket.DateLastFiling <= c.since) {
			time.Sleep(2 * time.Second)
			continue
		}
		var entries struct {
			Results []struct {
				DateFiled   string          `json:"date_filed"`
				EntryNumber json.RawMessage `json:"entry_number"`
				Description string          `json:"description"`
			} `json:"results"`
		}
		if err := clGet("/docket-entries/", map[string]string{
			"docket": did, "order_by": "-date_filed", "page_size": "5",
		}, &entries); err != nil {
			fmt.Fprintln(os.Stderr, "intel refresh", c.slug, "entries fetch:", err)
			time.Sleep(2 * time.Second)
			continue
		}
		var existing []map[string]any
		json.Unmarshal([]byte(c.timeline), &existing)
		seen := map[string]bool{}
		for _, e := range existing {
			d, _ := e["date"].(string)
			n, _ := e["doc_no"].(string)
			seen[d+"|"+n] = true
		}
		var fresh []map[string]any
		for _, en := range entries.Results {
			desc := strings.TrimSpace(en.Description)
			docNo := strings.Trim(string(en.EntryNumber), `"null`)
			if en.DateFiled == "" || desc == "" || seen[en.DateFiled+"|"+docNo] {
				continue
			}
			fresh = append(fresh, map[string]any{
				"date":   en.DateFiled,
				"title":  trunc(desc, 300),
				"doc_no": docNo,
				"url":    "https://www.courtlistener.com/docket/" + did + "/",
			})
		}
		if len(fresh) > 0 {
			merged := append(fresh, existing...)
			sort.Slice(merged, func(i, j int) bool {
				di, _ := merged[i]["date"].(string)
				dj, _ := merged[j]["date"].(string)
				return di > dj
			})
			payload, _ := json.Marshal(merged)
			newest := fresh[0]
			if _, err := db.Exec(`UPDATE ai_lawsuits SET timeline=$1, latest_development=$2,
				latest_development_date=$3, updated_at=now() WHERE id=$4`,
				payload, newest["title"], newest["date"], c.id); err != nil {
				fmt.Fprintln(os.Stderr, "intel refresh", c.slug, "update:", err)
				continue
			}
			updated++
			fmt.Printf("intel refresh %s: %d new docket entries through %v\n", c.slug, len(fresh), newest["date"])
		}
		time.Sleep(2 * time.Second)
	}
	return checked, updated, nil
}

// intelResolve fills docket numbers still marked pending verification straight
// from CourtListener search.
func intelResolve(db *sql.DB) (resolved int, err error) {
	rows, err := db.Query(`SELECT id, slug, case_name, COALESCE(defendants,'')
		FROM ai_lawsuits WHERE docket ILIKE '%pending%' AND is_active`)
	if err != nil {
		return 0, err
	}
	type pend struct {
		id                         int64
		slug, caseName, defendants string
	}
	var pending []pend
	for rows.Next() {
		var p pend
		if err := rows.Scan(&p.id, &p.slug, &p.caseName, &p.defendants); err != nil {
			rows.Close()
			return resolved, err
		}
		pending = append(pending, p)
	}
	rows.Close()
	paren := regexp.MustCompile(`\(.*?\)`)
	for _, p := range pending {
		q := strings.TrimSpace(paren.ReplaceAllString(p.caseName, ""))
		var res clSearch
		if err := clGet("/search/", map[string]string{
			"type": "r", "q": `"` + q + `"`, "order_by": "score desc",
		}, &res); err != nil {
			fmt.Fprintln(os.Stderr, "intel resolve", p.slug, "search:", err)
			continue
		}
		surname := strings.ToLower(strings.TrimSpace(strings.Split(strings.Split(p.defendants, ";")[0], ",")[0]))
		if i := strings.Index(surname, " "); i > 0 {
			surname = surname[:i]
		}
		for i, h := range res.Results {
			if i >= 5 {
				break
			}
			if h.DocketNumber == "" || h.DocketID == 0 || surname == "" ||
				!strings.Contains(strings.ToLower(h.CaseName), surname) {
				continue
			}
			clURL := fmt.Sprintf("https://www.courtlistener.com/docket/%d/", h.DocketID)
			if _, err := db.Exec(`UPDATE ai_lawsuits SET docket=$1, courtlistener_url=$2, updated_at=now() WHERE id=$3`,
				h.DocketNumber, clURL, p.id); err == nil {
				resolved++
				fmt.Printf("intel resolve %s: docket %s\n", p.slug, h.DocketNumber)
			}
			break
		}
		time.Sleep(2 * time.Second)
	}
	return resolved, nil
}

// intelDiscover queues newly filed AI lawsuits as candidates for review, then
// auto-promotes the unambiguous ones into ai_lawsuits so the tracker stays
// current without a human in the loop.
//
// Two searches run, because they fail in opposite directions. The SUBJECT
// search finds AI litigation against defendants nobody has heard of yet, but a
// broad topical query returns thousands of rows where "machine learning" or
// "discrimination" appear in unrelated boilerplate: a live check on 2026-08-02
// returned 2,925 hits, most of them pharmaceutical product-liability suits. The
// DEFENDANT search is the opposite, precise and shallow: caseName against the
// known AI defendants returns almost pure signal (Anthropic 39 dockets, Workday
// 19, Clearview 9, Character Technologies 8, Stability AI 6).
//
// The claim vocabulary also widened past copyright. Copyright and training data
// were the first wave, but the categories now growing fastest are chatbot
// wrongful death and product liability, AI hiring discrimination, right of
// publicity and deepfakes, biometric privacy, and AI-washing securities fraud.
// A tracker that only watched copyright would have shown a shrinking field while
// the actual field expanded.
func intelDiscover(db *sql.DB) (added int, err error) {
	since := time.Now().AddDate(0, 0, -45).Format("2006-01-02")

	// Defendants worth watching by name. Precision comes from the party, so
	// these need no topical qualifier at all.
	defendants := []string{
		"OpenAI", "Anthropic", "Midjourney", "Stability AI", "Uncharted Labs",
		"Suno", "Perplexity", "Character Technologies", "Clearview AI",
		"Workday", "HireVue", "Minimax", "Runway AI", "ElevenLabs",
		"Nvidia", "Hugging Face", "Scale AI", "Cohere", "Mistral AI",
	}

	// Subject queries, one per claim family rather than one giant OR, so a
	// noisy family cannot swamp the others in a single ranked result set.
	subjects := []string{
		`("artificial intelligence" OR "generative AI" OR "large language model") AND (copyright OR "training data" OR infringement)`,
		`(chatbot OR "companion AI" OR "AI companion") AND ("wrongful death" OR suicide OR "product liability" OR "failure to warn")`,
		`("artificial intelligence" OR algorithm OR "automated decision") AND ("employment discrimination" OR "disparate impact" OR "hiring discrimination" OR ADEA OR "Title VII")`,
		`(deepfake OR "digital replica" OR "voice clone" OR "AI-generated likeness") AND ("right of publicity" OR defamation OR Lanham)`,
		`("facial recognition" OR biometric OR "face template") AND (BIPA OR "biometric privacy" OR "Illinois Biometric")`,
		`("artificial intelligence" OR "AI-powered") AND ("securities fraud" OR "materially false" OR "misled investors" OR "AI washing")`,
	}

	// Relevance scoring. "Artificial intelligence" appears in patent and
	// trademark boilerplate constantly; the first live run queued NASCAR
	// trademark chaff alongside a real new Anthropic suit. Score on the
	// signals that separate AI-subject litigation from passing mentions,
	// and queue only what clears the bar. The score is stored so review
	// can sort by it.
	aiParty := regexp.MustCompile(`(?i)openai|anthropic|meta platforms|midjourney|stability ai|suno|uncharted labs|udio|perplexity|x\.?ai|google|alphabet|microsoft|nvidia|hugging face|character\.?ai|character technologies|deepseek|mistral|runway|eleven ?labs|minimax|clearview|workday|hirevue|scale ai|cohere`)
	aiSubject := regexp.MustCompile(`(?i)training data|generative|large language|chatbot|machine learning|neural|copyright|infring|scrap(e|ing)|dataset|deepfake|right of publicity|biometric|wrongful death|product liability|disparate impact|securities fraud|ai washing|facial recognition|automated decision`)
	patentNoise := regexp.MustCompile(`(?i)patent|'\d{3} patent|licensing, llc|innovations ltd|ip pty|technology licensing`)

	queue := func(h struct {
		CaseName     string `json:"caseName"`
		DocketNumber string `json:"docketNumber"`
		DocketID     int64  `json:"docket_id"`
		Court        string `json:"court"`
		DateFiled    string `json:"dateFiled"`
		Snippet      string `json:"snippet"`
	}, base int) {
		if h.DocketID == 0 {
			return
		}
		score := base
		if aiParty.MatchString(h.CaseName) {
			score += 3
		}
		if aiSubject.MatchString(h.Snippet) || aiSubject.MatchString(h.CaseName) {
			score += 2
		}
		if patentNoise.MatchString(h.CaseName) {
			score -= 3
		}
		if score < 2 {
			return
		}
		var tracked bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM ai_lawsuits WHERE courtlistener_url LIKE $1)`,
			fmt.Sprintf("%%/docket/%d/%%", h.DocketID)).Scan(&tracked)
		if tracked {
			return
		}
		r, e := db.Exec(`INSERT INTO ai_lawsuit_candidates
			(source, source_id, case_name, court, docket, filed_date, url, snippet, score)
			VALUES ('courtlistener', $1, $2, $3, $4, NULLIF($5,'')::date, $6, $7, $8)
			ON CONFLICT (source_id) DO NOTHING`,
			fmt.Sprintf("cl-docket-%d", h.DocketID), h.CaseName, h.Court, h.DocketNumber,
			h.DateFiled, fmt.Sprintf("https://www.courtlistener.com/docket/%d/", h.DocketID),
			trunc(h.Snippet, 500), score)
		if e != nil {
			return
		}
		if n, _ := r.RowsAffected(); n > 0 {
			added++
			fmt.Printf("intel discover: queued (score %d) %s %s\n", score, h.CaseName, h.DocketNumber)
		}
	}

	// Discovery makes 25 API calls across the two passes. When CourtListener
	// throttles, each one costs about ninety seconds of backoff, which is over
	// half an hour of a run spent achieving nothing. Stop at the first sign of
	// it: discovery is a daily sweep, and missing one day costs a candidate
	// being queued tomorrow instead of today.
	throttled := false
	for _, q := range subjects {
		if throttled {
			break
		}
		var res clSearch
		if e := clGet("/search/", map[string]string{
			"type": "r", "q": q, "filed_after": since, "order_by": "dateFiled desc",
		}, &res); e != nil {
			fmt.Fprintln(os.Stderr, "intel discover subject:", e)
			if strings.Contains(e.Error(), "rate limited") {
				throttled = true
			}
			continue
		}
		for i, h := range res.Results {
			if i >= 25 {
				break
			}
			queue(h, 0)
		}
		time.Sleep(2 * time.Second)
	}

	// Defendant sweep runs on a longer window: a suit against a known AI
	// company is worth tracking whenever it was filed, not only in the last
	// 45 days. The base score of 3 reflects that the party alone is the
	// evidence.
	dsince := time.Now().AddDate(0, 0, -365).Format("2006-01-02")
	for _, d := range defendants {
		if throttled {
			break
		}
		var res clSearch
		if e := clGet("/search/", map[string]string{
			"type": "r", "q": fmt.Sprintf(`caseName:("%s")`, d),
			"filed_after": dsince, "order_by": "dateFiled desc",
		}, &res); e != nil {
			fmt.Fprintln(os.Stderr, "intel discover defendant", d, ":", e)
			if strings.Contains(e.Error(), "rate limited") {
				throttled = true
			}
			continue
		}
		for i, h := range res.Results {
			if i >= 20 {
				break
			}
			queue(h, 3)
		}
		time.Sleep(2 * time.Second)
	}

	if throttled {
		fmt.Println("intel discover: rate limited, discovery cut short this run")
	}
	promoted, perr := intelPromote(db)
	if perr != nil {
		return added, perr
	}
	if promoted > 0 {
		fmt.Printf("intel discover: promoted %d candidates to the tracker\n", promoted)
	}
	return added, nil
}

// intelPromote publishes high-confidence candidates into ai_lawsuits so a newly
// filed case appears on the tracker without waiting on a human.
//
// Only the verified fields carry over: case name, court, docket number, filing
// date, the parties as they appear in the caption, and the CourtListener link.
// Nothing interpretive is generated here. executive_summary, why_it_matters,
// and claims stay empty until a person writes them, because a machine-written
// characterisation of somebody's lawsuit is exactly the kind of confident
// invention this platform exists not to publish. The page renders what it has
// and says less where it has nothing.
//
// The timeline fills itself: intelRefresh reads the docket for every active
// case on the next run, so a promoted case gains its history automatically.
func intelPromote(db *sql.DB) (int, error) {
	// Auto-publishing taught a lesson on its first live run. The defendant
	// sweep scores on the party name, and party names collide: "Cohere" is an
	// AI lab and also Cohere Health, a prior-authorisation company, and Cohere
	// Beauty of Omaha. "Suno" is a music model and also a surname, which put a
	// federal criminal case on the tracker. Nvidia is sued constantly over
	// patents and securities, none of which is AI-subject litigation. Twenty
	// candidates auto-published and three of them did not belong.
	//
	// So promotion now needs two things the queue score alone cannot give: the
	// caption must name a defendant we actually track as an AI product or
	// service company, and it must not match the collision and boilerplate
	// patterns that produced the bad rows. Anything that fails either test
	// stays a candidate. A case sitting in the queue costs nothing; a wrong
	// case on a public tracker costs the credibility the whole site runs on.
	aiCore := regexp.MustCompile(`(?i)openai|anthropic|midjourney|stability ai|uncharted labs|udio|suno,? inc|suno inc|perplexity ai|character technologies|character\.ai|clearview ai|workday|hirevue|runway ai|eleven ?labs|minimax|hugging face|deepseek|mistral ai|scale ai|x\.ai|meta platforms`)
	noise := regexp.MustCompile(`(?i)cohere health|cohere beauty|cali-curl|villanueva|monolithic 3d|speednic|edgecomm|array cache|arlington technologies|mobility workx|health discovery|concurrent ventures|in re subpoena|department of war|nvidia|patent|licensing, ?llc|technologies llc`)

	rows, err := db.Query(`SELECT id, case_name, court, COALESCE(docket,''),
			COALESCE(filed_date::text,''), url, COALESCE(snippet,'')
		FROM ai_lawsuit_candidates
		WHERE status='new' AND score >= 5
		ORDER BY filed_date DESC NULLS LAST LIMIT 20`)
	if err != nil {
		return 0, err
	}
	type cand struct {
		id                                       int64
		name, court, docket, filed, url, snippet string
	}
	var cs []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.name, &c.court, &c.docket, &c.filed, &c.url, &c.snippet) == nil {
			cs = append(cs, c)
		}
	}
	rows.Close()

	nonSlug := regexp.MustCompile(`[^a-z0-9]+`)
	promoted := 0
	for _, c := range cs {
		if !aiCore.MatchString(c.name) || noise.MatchString(c.name) {
			continue
		}
		// Two guards the defendant-name test cannot give, found by srj's audit
		// of the jump to 92 cases. A bankruptcy docket names Workday because it
		// is a creditor or a vendor of the debtor, not because anyone sued it
		// over AI, and that is a different collision class from the Cohere and
		// Suno name clashes. A caption with no " v. " separator is a docket
		// entity record rather than a suit: one row reached the tracker whose
		// entire case name was the bare string WORKDAY INC.
		if strings.Contains(strings.ToLower(c.court), "bankr") {
			continue
		}
		if !strings.Contains(c.name, " v. ") {
			continue
		}
		parties := strings.SplitN(c.name, " v. ", 2)
		plaintiff := strings.TrimSpace(parties[0])
		defendant := ""
		if len(parties) == 2 {
			defendant = strings.TrimSpace(parties[1])
		}
		if defendant == "" {
			continue
		}
		short := func(s string) string {
			f := strings.Fields(nonSlug.ReplaceAllString(strings.ToLower(s), " "))
			if len(f) > 2 {
				f = f[:2]
			}
			return strings.Join(f, "-")
		}
		slug := strings.Trim(short(plaintiff)+"-v-"+short(defendant), "-")
		if slug == "" || slug == "-v-" {
			continue
		}
		// Slug collisions are real: two Concord actions against Anthropic
		// already share a caption. Suffix rather than overwrite.
		base := slug
		for n := 2; n < 10; n++ {
			var taken bool
			db.QueryRow(`SELECT EXISTS(SELECT 1 FROM ai_lawsuits WHERE slug=$1)`, slug).Scan(&taken)
			if !taken {
				break
			}
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		var filed any
		if c.filed != "" {
			filed = c.filed
		}
		var summary any
		if c.snippet != "" {
			summary = c.snippet
		}
		if _, err := db.Exec(`INSERT INTO ai_lawsuits
			(slug, case_name, court, docket, filed_date, plaintiffs, defendants, category,
			 status, status_badge, courtlistener_url, source_url, is_active, display_order,
			 verified_date, summary)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'copyright',
			 'Filed; docket monitoring active, no development recorded yet by this tracker',
			 'Active Litigation',$8,$8,true,
			 (SELECT COALESCE(MAX(display_order),0)+10 FROM ai_lawsuits),
			 current_date,$9)
			ON CONFLICT (slug) DO NOTHING`,
			slug, c.name, c.court, c.docket, filed, plaintiff, defendant, c.url, summary); err != nil {
			fmt.Fprintln(os.Stderr, "intel promote", slug, ":", err)
			continue
		}
		db.Exec(`UPDATE ai_lawsuit_candidates SET status='promoted' WHERE id=$1`, c.id)
		promoted++
	}
	return promoted, nil
}

// intelAIWatch queues new Hugging Face models and AI vendor news as intel
// candidates, reusing the pipeline's aiTerm subject filter for the feeds.
func intelAIWatch(db *sql.DB) (added int, err error) {
	req, _ := http.NewRequest("GET", "https://huggingface.co/api/models?sort=createdAt&direction=-1&limit=25", nil)
	req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (srjconsultingservices.com)")
	if resp, herr := http.DefaultClient.Do(req); herr == nil {
		var models []struct {
			ID          string `json:"id"`
			ModelID     string `json:"modelId"`
			Downloads   int    `json:"downloads"`
			PipelineTag string `json:"pipeline_tag"`
		}
		if derr := json.NewDecoder(resp.Body).Decode(&models); derr == nil {
			for _, m := range models {
				mid := m.ModelID
				if mid == "" {
					mid = m.ID
				}
				if mid == "" || m.Downloads < 50 {
					continue
				}
				name, vendor := mid, ""
				if i := strings.Index(mid, "/"); i > 0 {
					vendor, name = mid[:i], mid[i+1:]
				}
				r, ierr := db.Exec(`INSERT INTO ai_intel_candidates (kind, name, vendor, url, summary, source, source_id)
					VALUES ('model', $1, NULLIF($2,''), $3, $4, 'huggingface', $5)
					ON CONFLICT (source_id) DO NOTHING`,
					name, vendor, "https://huggingface.co/"+mid,
					fmt.Sprintf("pipeline: %s, downloads: %d", m.PipelineTag, m.Downloads),
					"hf-"+mid)
				if ierr != nil {
					continue
				}
				if n, _ := r.RowsAffected(); n > 0 {
					added++
				}
			}
		}
		resp.Body.Close()
	} else {
		fmt.Fprintln(os.Stderr, "intel ai_watch huggingface:", herr)
	}
	// Phase 1 of the watch-everything directive (Stephen, July 31 2026,
	// changelog seq 138): every free source with a working RSS/Atom feed.
	// Feedless sources (BAAI, CAC, IMDA, Naver, 36Kr, QbitAI, Shanghai AI
	// Lab) are phase 2, scraper-based. Paid APIs (Dealroom, Tracxn) are
	// excluded on Stephen's instruction. A dead feed logs and is skipped, so
	// one source rotting never blocks the rest.
	feeds := []struct{ vendor, url string }{
		{"OpenAI", "https://openai.com/news/rss.xml"},
		{"Google DeepMind", "https://deepmind.google/blog/rss.xml"},
		{"Hugging Face", "https://huggingface.co/blog/feed.xml"},
		// Research and labs. Probed July 31: Aleph Alpha, KAIST, OECD.AI, and
		// Canada ISED expose no working feed (HTML pages or connection resets)
		// and move to the phase-2 scraper list.
		{"Mistral AI", "https://mistral.ai/rss.xml"},
		{"Stability AI (coverage)", "https://news.google.com/rss/search?q=%22Stability+AI%22&hl=en-US&gl=US&ceid=US:en"},
		{"AI21 Labs (coverage)", "https://news.google.com/rss/search?q=%22AI21+Labs%22&hl=en-US&gl=US&ceid=US:en"},
		{"The Alan Turing Institute", "https://www.turing.ac.uk/rss.xml"},
		{"INRIA", "https://inria.fr/en/rss.xml"},
		{"RIKEN AIP", "https://www.riken.jp/en/feed/"},
		{"MBZUAI", "https://mbzuai.ac.ae/news/feed/"},
		{"AI Singapore", "https://aisingapore.org/feed/"},
		// Policy, regulation, standards
		{"European Commission AI", "https://digital-strategy.ec.europa.eu/en/rss.xml"},
		{"UK DSIT", "https://www.gov.uk/government/organisations/department-for-science-innovation-and-technology.atom"},
		{"UK AI Safety Institute (coverage)", "https://news.google.com/rss/search?q=%22AI+Safety+Institute%22+UK&hl=en-US&gl=US&ceid=US:en"},
		{"UNESCO (coverage)", "https://news.google.com/rss/search?q=UNESCO+%22artificial+intelligence%22&hl=en-US&gl=US&ceid=US:en"},
		{"CIFAR", "https://cifar.ca/feed/"},
		// Media and industry
		{"The Register", "https://www.theregister.com/software/ai_ml/headlines.atom"},
		{"Rest of World", "https://restofworld.org/feed/latest/"},
		{"Synced", "https://syncedreview.com/feed/"},
		{"Sifted", "https://sifted.eu/feed"},
		{"Tech in Asia", "https://www.techinasia.com/rss"},
		{"KrASIA", "https://kr-asia.com/feed"},
		{"The Yuan (coverage)", "https://news.google.com/rss/search?q=%22The+Yuan%22+AI+site:the-yuan.com+OR+%22the-yuan.com%22&hl=en-US&gl=US&ceid=US:en"},
		{"Computing UK", "https://www.computing.co.uk/feeds/rss"},
		{"Heise", "https://www.heise.de/rss/heise-atom.xml"},
		{"L'Usine Digitale", "https://www.usine-digitale.fr/rss"},
		// Phase 2 of the watch-everything directive: sources with no working
		// feed of their own, watched through Google News RSS coverage
		// queries. This trades first-party immediacy for zero scraper
		// maintenance and free English-language handling of the
		// Chinese-language set; a first-party scraper can replace any of
		// these later without schema changes.
		{"BAAI (coverage)", "https://news.google.com/rss/search?q=%22Beijing+Academy+of+Artificial+Intelligence%22&hl=en-US&gl=US&ceid=US:en"},
		{"Shanghai AI Lab (coverage)", "https://news.google.com/rss/search?q=%22Shanghai+AI+Laboratory%22&hl=en-US&gl=US&ceid=US:en"},
		{"China CAC (coverage)", "https://news.google.com/rss/search?q=%22Cyberspace+Administration+of+China%22+AI&hl=en-US&gl=US&ceid=US:en"},
		{"Singapore IMDA (coverage)", "https://news.google.com/rss/search?q=IMDA+Singapore+AI&hl=en-US&gl=US&ceid=US:en"},
		{"Naver Clova (coverage)", "https://news.google.com/rss/search?q=Naver+HyperCLOVA+OR+%22Naver+AI%22&hl=en-US&gl=US&ceid=US:en"},
		{"36Kr (coverage)", "https://news.google.com/rss/search?q=36Kr+AI&hl=en-US&gl=US&ceid=US:en"},
		{"QbitAI (coverage)", "https://news.google.com/rss/search?q=QbitAI+OR+%22%E9%87%8F%E5%AD%90%E4%BD%8D%22&hl=en-US&gl=US&ceid=US:en"},
		{"Aleph Alpha (coverage)", "https://news.google.com/rss/search?q=%22Aleph+Alpha%22&hl=en-US&gl=US&ceid=US:en"},
		{"KAIST AI (coverage)", "https://news.google.com/rss/search?q=KAIST+AI&hl=en-US&gl=US&ceid=US:en"},
		{"OECD AI (coverage)", "https://news.google.com/rss/search?q=%22OECD%22+AI+policy&hl=en-US&gl=US&ceid=US:en"},
		{"Canada AI policy (coverage)", "https://news.google.com/rss/search?q=Canada+ISED+OR+CIFAR+AI&hl=en-US&gl=US&ceid=US:en"},
	}
	for _, f := range feeds {
		req, _ := http.NewRequest("GET", f.url, nil)
		req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (srjconsultingservices.com)")
		resp, ferr := http.DefaultClient.Do(req)
		if ferr != nil {
			fmt.Fprintln(os.Stderr, "intel ai_watch feed", f.vendor, ":", ferr)
			continue
		}
		// Buffered rather than streamed so a parse failure can be retried
		// against the same bytes by the regex fallback below.
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if rerr != nil {
			fmt.Fprintln(os.Stderr, "intel ai_watch feed", f.vendor, "read:", rerr)
			continue
		}
		var feed struct {
			Items []struct {
				Title string `xml:"title"`
				Link  string `xml:"link"`
			} `xml:"channel>item"`
		}
		// RSS in the wild is full of HTML entities (&mdash;) and sloppy
		// markup that Go's strict XML parser rejects (July 31 run: 7 of the
		// new feeds failed on entities alone). Lenient mode with the HTML
		// entity table rescues those; feeds serving actual HTML pages still
		// fail and need their URLs corrected instead.
		dec := xml.NewDecoder(bytes.NewReader(body))
		dec.Strict = false
		dec.Entity = xml.HTMLEntity
		derr := dec.Decode(&feed)
		if derr != nil {
			// Lenient mode still rejects genuinely malformed markup, e.g.
			// Sifted's stray attribute on line 91 (Aug 8 run). Rather than
			// lose the whole feed to one bad element, fall back to pulling
			// item titles and links out of the raw bytes. Same data, no
			// parser. A feed serving an HTML page matches nothing here and
			// still reports as a failure, which is the signal we want.
			items := itemRe.FindAllSubmatch(body, -1)
			recovered := 0
			for _, m := range items {
				t := titleRe.FindSubmatch(m[1])
				l := linkRe.FindSubmatch(m[1])
				if t == nil || l == nil {
					continue
				}
				feed.Items = append(feed.Items, struct {
					Title string `xml:"title"`
					Link  string `xml:"link"`
				}{Title: stripCDATA(string(t[1])), Link: stripCDATA(string(l[1]))})
				recovered++
			}
			if recovered == 0 {
				fmt.Fprintln(os.Stderr, "intel ai_watch feed", f.vendor, "parse:", derr)
				continue
			}
			fmt.Fprintf(os.Stderr, "intel ai_watch feed %s parse: %v (recovered %d items without the parser)\n",
				f.vendor, derr, recovered)
		}
		for _, it := range feed.Items {
			title, link := strings.TrimSpace(it.Title), strings.TrimSpace(it.Link)
			if title == "" || link == "" || !mentionsAI(title) {
				continue
			}
			// Google News coverage-proxy feeds hand us an opaque redirect
			// rather than the publisher link, and label the item with the
			// SEARCH QUERY that found it, which is how a Cohere story ended
			// up filed under "Aleph Alpha (coverage)". Resolve the real URL
			// and, for proxy feeds only, name the publisher instead of the
			// query. First-party vendor feeds keep their own label.
			vendorLabel := f.vendor
			sourceID := "rss-" + link
			if isGoogleNewsURL(link) {
				if real := resolveGoogleNews(link); real != link {
					link = real
					if strings.Contains(f.vendor, "(coverage)") {
						if pub := publisherFromURL(real); pub != "" {
							vendorLabel = pub
						}
					}
				}
				time.Sleep(700 * time.Millisecond)
			}
			r, ierr := db.Exec(`INSERT INTO ai_intel_candidates (kind, name, vendor, url, source, source_id)
				VALUES ('vendor-news', $1, $2, $3, 'rss', $4)
				ON CONFLICT (source_id) DO NOTHING`,
				trunc(title, 300), vendorLabel, link, sourceID)
			if ierr != nil {
				continue
			}
			if n, _ := r.RowsAffected(); n > 0 {
				added++
			}
		}
	}
	return added, nil
}

// intelSync runs the four intel jobs, each independent, and logs the run.
func intelSync(db *sql.DB) error {
	ok := true
	var details []string
	checked, updated, err := intelRefresh(db)
	if err != nil {
		ok = false
		details = append(details, "refresh: "+err.Error())
	}
	resolved, err := intelResolve(db)
	if err != nil {
		ok = false
		details = append(details, "resolve: "+err.Error())
	}
	lawAdded, err := intelDiscover(db)
	if err != nil {
		ok = false
		details = append(details, "discover: "+err.Error())
	}
	intelAdded, err := intelAIWatch(db)
	if err != nil {
		ok = false
		details = append(details, "ai_watch: "+err.Error())
	}
	detail := sql.NullString{String: strings.Join(details, "; "), Valid: len(details) > 0}
	db.Exec(`INSERT INTO srj_intel_log (job, ok, dockets_checked, dockets_updated,
		lawsuit_candidates_added, intel_candidates_added, detail)
		VALUES ('daily-sync', $1, $2, $3, $4, $5, $6)`,
		ok, checked, updated, lawAdded, intelAdded, detail)
	fmt.Printf("intel: checked=%d updated=%d resolved=%d lawsuit_candidates=%d intel_candidates=%d ok=%v\n",
		checked, updated, resolved, lawAdded, intelAdded, ok)
	if !ok {
		return fmt.Errorf("intel jobs failed: %s", strings.Join(details, "; "))
	}
	return nil
}

// ---- archive_news: full-text corpus archival + summaries -------------------
//
// Everything the news discovery layer finds is downloaded once and kept:
// article HTML and extracted text go to the PRIVATE R2 bucket (srj-uploads,
// corpus/ prefix) through the site Worker's /api/archive endpoint, and the
// extracted text is also held in pipeline.documents.fulltext for summarization
// and future LLM work. The public site only ever shows own-words summaries.
//
// Environment: ARCHIVE_ENDPOINT (https://srjconsultingservices.com/api/archive),
// ARCHIVE_TOKEN (bearer), ANTHROPIC_API_KEY (summaries; publish_news degrades
// gracefully without it).

var (
	scriptRe = regexp.MustCompile(`(?is)<(script|style|noscript|svg|nav|header|footer|form)[^>]*>.*?</\s*(script|style|noscript|svg|nav|header|footer|form)\s*>`)
	tagRe    = regexp.MustCompile(`(?s)<[^>]*>`)
	wsRe     = regexp.MustCompile(`[ \t\r\f]+`)
	nlRe     = regexp.MustCompile(`\n{3,}`)
)

// htmlToText is a deliberately simple extractor: strip the chrome-bearing
// elements, drop tags, decode the common entities, collapse whitespace. Good
// enough for summarization and corpus search; the raw HTML is archived too,
// so a better extractor can always re-run later.
func htmlToText(h string) string {
	h = scriptRe.ReplaceAllString(h, " ")
	h = strings.ReplaceAll(h, "</p>", "\n\n")
	h = strings.ReplaceAll(h, "<br>", "\n")
	h = strings.ReplaceAll(h, "<br/>", "\n")
	h = tagRe.ReplaceAllString(h, " ")
	for k, v := range map[string]string{"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`, "&#39;": "'", "&rsquo;": "'", "&lsquo;": "'", "&ldquo;": `"`, "&rdquo;": `"`, "&nbsp;": " ", "&mdash;": ",", "&ndash;": "-"} {
		h = strings.ReplaceAll(h, k, v)
	}
	h = wsRe.ReplaceAllString(h, " ")
	lines := strings.Split(h, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	h = strings.Join(lines, "\n")
	return strings.TrimSpace(nlRe.ReplaceAllString(h, "\n\n"))
}

// archivePut writes one object through the Worker's bearer-gated endpoint.
func archivePut(endpoint, token, key, contentType string, body []byte) error {
	req, err := http.NewRequest("PUT", endpoint+"?key="+key, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("archive PUT %s: %d %s", key, resp.StatusCode, b)
	}
	return nil
}

// archiveNews downloads article bodies for recent gdelt documents that have
// not been archived yet, stores HTML + text in R2, and keeps the text in
// pipeline.documents.fulltext. Failures mark fetch_failed_at so a dead URL is
// tried once, not daily forever.
func archiveNews(db *sql.DB) error {
	endpoint, token := os.Getenv("ARCHIVE_ENDPOINT"), os.Getenv("ARCHIVE_TOKEN")
	if endpoint == "" || token == "" {
		return fmt.Errorf("ARCHIVE_ENDPOINT and ARCHIVE_TOKEN must be set")
	}
	rows, err := db.Query(`SELECT d.id, d.external_id, d.url
		FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
		WHERE s.key='gdelt' AND d.r2_key IS NULL AND d.fetch_failed_at IS NULL
		AND d.fetched_at > now() - interval '10 days'
		ORDER BY d.id DESC LIMIT 80`)
	if err != nil {
		return err
	}
	type doc struct {
		id      int64
		ext, ur string
	}
	var todo []doc
	for rows.Next() {
		var d doc
		if rows.Scan(&d.id, &d.ext, &d.ur) == nil {
			todo = append(todo, d)
		}
	}
	rows.Close()
	client := &http.Client{Timeout: 25 * time.Second}
	archived, failed := 0, 0
	for _, d := range todo {
		req, rerr := http.NewRequest("GET", d.ur, nil)
		if rerr != nil {
			db.Exec(`UPDATE pipeline.documents SET fetch_failed_at=now() WHERE id=$1`, d.id)
			failed++
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; SRJ-archive/1.0; +https://srjconsultingservices.com)")
		resp, gerr := client.Do(req)
		if gerr != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			db.Exec(`UPDATE pipeline.documents SET fetch_failed_at=now() WHERE id=$1`, d.id)
			failed++
			time.Sleep(500 * time.Millisecond)
			continue
		}
		htmlB, rderr := io.ReadAll(io.LimitReader(resp.Body, 3*1024*1024))
		resp.Body.Close()
		if rderr != nil || len(htmlB) == 0 {
			db.Exec(`UPDATE pipeline.documents SET fetch_failed_at=now() WHERE id=$1`, d.id)
			failed++
			continue
		}
		text := htmlToText(string(htmlB))
		if len(text) > 200*1024 {
			text = text[:200*1024]
		}
		keyBase := "corpus/news/" + d.ext
		if err := archivePut(endpoint, token, keyBase+".html", "text/html; charset=utf-8", htmlB); err != nil {
			// A 403 here is the edge WAF challenging the article body itself
			// (seen live July 31), which will fail identically every day; mark
			// the doc failed so it is tried once, not forever. Transient
			// errors leave the doc eligible for the next run.
			if strings.Contains(err.Error(), ": 403 ") {
				db.Exec(`UPDATE pipeline.documents SET fetch_failed_at=now() WHERE id=$1`, d.id)
			}
			fmt.Fprintln(os.Stderr, "archive_news:", err)
			failed++
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err := archivePut(endpoint, token, keyBase+".txt", "text/plain; charset=utf-8", []byte(text)); err != nil {
			fmt.Fprintln(os.Stderr, "archive_news:", err)
		}
		// Postgres rejects invalid UTF-8 (seen live July 31: "invalid byte
		// sequence 0xbb" from mis-declared article encodings), so sanitize
		// before the write; the raw bytes are already preserved in R2.
		if _, err := db.Exec(`UPDATE pipeline.documents SET r2_key=$1, fulltext=$2 WHERE id=$3`,
			keyBase+".html", strings.ToValidUTF8(text, "\uFFFD"), d.id); err != nil {
			fmt.Fprintln(os.Stderr, "archive_news db:", err)
			continue
		}
		archived++
		time.Sleep(400 * time.Millisecond)
	}
	fmt.Printf("archive_news: archived=%d failed=%d of %d\n", archived, failed, len(todo))
	return nil
}

// anthropicSummarize writes a two-paragraph, own-words news summary. House
// style: plain English, commas rather than dashes, no reproduced passages.
func anthropicSummarize(headline, text string) (string, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "", fmt.Errorf("ANTHROPIC_API_KEY not set")
	}
	if len(text) < 400 {
		return "", fmt.Errorf("article text too short to summarize")
	}
	prompt := "Summarize this news article in two short paragraphs, 120 to 180 words total, entirely in your own words. " +
		"State what happened, who is involved, the key numbers, and what happens next if the article says. " +
		"Plain English. Use commas rather than dashes. Do not quote more than a few words. Do not repeat the headline. " +
		"Do not add opinions or information that is not in the article. Output only the summary paragraphs.\n\n" +
		"Headline: " + headline + "\n\nArticle text:\n" + text
	body, _ := json.Marshal(map[string]any{
		"model":      "claude-haiku-4-5",
		"max_tokens": 400,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("anthropic %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, c := range out.Content {
		sb.WriteString(c.Text)
	}
	s := strings.TrimSpace(sb.String())
	if s == "" {
		return "", fmt.Errorf("empty summary")
	}
	return s, nil
}

// ---- twoai: theworldofai.org, SQL -> twoai-content ------------------------
//
// The consumer property renders the same database. twoai_pages is a render
// cache: path is the exact repo path inside twoai-content, data is everything
// the Astro template needs. Dropping the table and re-running the pipeline
// must always reproduce it. twoaiBuild fills it from existing tables (bills,
// glossary, lawsuits); twoaiPublish exports rows whose git blob sha differs,
// so a quiet day is one tree call and zero commits, same as sync_content.
var twoaiStates = map[string]string{
	"AL": "Alabama", "AK": "Alaska", "AZ": "Arizona", "AR": "Arkansas", "CA": "California",
	"CO": "Colorado", "CT": "Connecticut", "DE": "Delaware", "FL": "Florida", "GA": "Georgia",
	"HI": "Hawaii", "ID": "Idaho", "IL": "Illinois", "IN": "Indiana", "IA": "Iowa",
	"KS": "Kansas", "KY": "Kentucky", "LA": "Louisiana", "ME": "Maine", "MD": "Maryland",
	"MA": "Massachusetts", "MI": "Michigan", "MN": "Minnesota", "MS": "Mississippi", "MO": "Missouri",
	"MT": "Montana", "NE": "Nebraska", "NV": "Nevada", "NH": "New Hampshire", "NJ": "New Jersey",
	"NM": "New Mexico", "NY": "New York", "NC": "North Carolina", "ND": "North Dakota", "OH": "Ohio",
	"OK": "Oklahoma", "OR": "Oregon", "PA": "Pennsylvania", "RI": "Rhode Island", "SC": "South Carolina",
	"SD": "South Dakota", "TN": "Tennessee", "TX": "Texas", "UT": "Utah", "VT": "Vermont",
	"VA": "Virginia", "WA": "Washington", "WV": "West Virginia", "WI": "Wisconsin", "WY": "Wyoming",
	"DC": "District of Columbia", "PR": "Puerto Rico", "US": "United States Congress",
}

func twoaiSlug(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

func twoaiBuild(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_pages (
		path text PRIMARY KEY, kind text NOT NULL, data jsonb NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	today := time.Now().UTC().Format("2006-01-02")

	// ---- F1: AI laws by state, from the LegiScan corpus. One row per bill
	// (latest change wins), grouped by the "ST NUM: Title" prefix.
	rows, err := db.Query(`SELECT DISTINCT ON (external_id) external_id, title, url,
			COALESCE(to_char(published_at,'YYYY-MM-DD'),'') 
		FROM pipeline.documents WHERE source_id = 2
		ORDER BY external_id, id DESC`)
	if err != nil {
		return err
	}
	type bill struct {
		Number string `json:"number"`
		Title  string `json:"title"`
		URL    string `json:"url"`
		Date   string `json:"date"`
	}
	byState := map[string][]bill{}
	for rows.Next() {
		var ext, title, url, date string
		if err := rows.Scan(&ext, &title, &url, &date); err != nil {
			rows.Close()
			return err
		}
		parts := strings.SplitN(title, ":", 2)
		head := strings.Fields(parts[0])
		if len(head) < 2 {
			continue
		}
		code := strings.ToUpper(head[0])
		if _, ok := twoaiStates[code]; !ok {
			continue
		}
		b := bill{Number: strings.Join(head[1:], " "), URL: url, Date: date}
		if len(parts) == 2 {
			b.Title = strings.TrimSpace(parts[1])
		}
		byState[code] = append(byState[code], b)
	}
	rows.Close()

	type stateIdx struct {
		Code  string `json:"code"`
		Name  string `json:"name"`
		Slug  string `json:"slug"`
		Count int    `json:"count"`
	}
	var index []stateIdx
	total := 0
	upsert := func(path, kind string, v any) error {
		j, _ := json.Marshal(v)
		_, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug)
			VALUES ($1,$2,$3::jsonb,$4)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, updated_at=now()`,
			path, kind, string(j), twoaiTaxonomyFor(kind))
		return err
	}
	for code, name := range twoaiStates {
		bills := byState[code]
		if bills == nil {
			bills = []bill{}
		}
		sort.Slice(bills, func(i, j int) bool { return bills[i].Date > bills[j].Date })
		total += len(bills)
		slug := twoaiSlug(name)
		index = append(index, stateIdx{code, name, slug, len(bills)})
		if err := upsert("laws/"+slug+".json", "state-law", map[string]any{
			"code": code, "name": name, "slug": slug, "count": len(bills),
			"bills": bills, "generated": today,
		}); err != nil {
			return err
		}
	}
	sort.Slice(index, func(i, j int) bool { return index[i].Name < index[j].Name })
	if err := upsert("laws/index.json", "hub", map[string]any{
		"states": index, "total": total, "generated": today,
	}); err != nil {
		return err
	}

	// A bundle row renders many URLs from one file: the glossary is 523 pages
	// in a single JSON, the tracker is one page per case plus the hub. Counting
	// rows therefore undercounts the site badly, which is why AI Litigation
	// reported 1 while publishing 92 cases. url_count records what a row
	// actually becomes on the site so the coverage numbers mean something.
	setURLs := func(path string, n int) {
		db.Exec(`UPDATE twoai_pages SET url_count=$1 WHERE path=$2`, n, path)
	}

	// ---- F2: glossary, straight from the library already in site_content.
	//
	// TWO TABLES HOLD GLOSSARY TERMS AND ONLY ONE OF THEM RENDERS. This block
	// reads site_content['resources/glossary.json'], which is the richer record
	// (it carries slug and origin, which the term pages use). The SRJ side also
	// maintains synced_glossary_terms, which has no slug and no origin.
	//
	// On 2026-08-18 eight new terms were written to synced_glossary_terms and
	// nowhere else. They never appeared on the site and never would have: this
	// stage cannot see that table. The count sat at 522 through repeated runs
	// while the other table said 536, and the eight URLs 404ed, which was
	// misread as a publish lag rather than a wrong source. The terms were
	// merged into site_content on 2026-08-19 and the drift check below exists
	// so the next divergence announces itself instead of hiding.
	//
	// Deliberately a warning, not a merge. Auto-copying between two tables
	// whose shapes differ would invent slugs silently, and a slug is a URL that
	// can never move once published.
	var syncedActive, inLibrary int
	db.QueryRow(`SELECT count(*) FROM synced_glossary_terms WHERE is_active`).Scan(&syncedActive)
	db.QueryRow(`SELECT jsonb_array_length(data->'terms') FROM site_content
		WHERE path='resources/glossary.json'`).Scan(&inLibrary)
	if syncedActive > 0 && inLibrary > 0 && syncedActive != inLibrary {
		fmt.Fprintf(os.Stderr,
			"twoai_build: GLOSSARY DRIFT: synced_glossary_terms has %d active, site_content library has %d. "+
				"Only the library renders. Terms present in one and not the other will not publish.\n",
			syncedActive, inLibrary)
	}

	var glossary string
	if err := db.QueryRow(`SELECT data::text FROM site_content WHERE path='resources/glossary.json'`).Scan(&glossary); err == nil && glossary != "" {
		var g map[string]any
		if json.Unmarshal([]byte(glossary), &g) == nil {
			g["generated"] = today

			// count is a stored copy of a number the array already knows, so it
			// goes stale the moment a term is added by hand: it read 522 against
			// 551 real entries on 2026-08-27, wrong since at least the 08-18
			// additions, and nothing complained because nothing recomputes it.
			// Derive it here every run. A derived value that is written once and
			// trusted forever is not data, it is a comment that lies.
			if ts, ok := g["terms"].([]any); ok {
				if stored, had := g["count"].(float64); had && int(stored) != len(ts) {
					fmt.Fprintf(os.Stderr,
						"twoai_build: glossary count was %d, terms array has %d, using the array\n",
						int(stored), len(ts))
				}
				g["count"] = len(ts)
			}

			// AUDIENCE LENSES: 2,109 rows across 522 terms, written to explain
			// each term to a child, a developer, a regulator, a CISO and the
			// rest. They live in twoai_glossary_lenses, the term page has
			// rendered them since it was built, and NOTHING HAS EVER PUT THEM IN
			// THE PAGE DOCUMENT. The template guards on t.lenses, that key never
			// existed in site_content, so the guard was false for every term and
			// the whole section silently disappeared. Months of writing, live in
			// SQL, invisible on the site.
			//
			// Merged here rather than into site_content because site_content is
			// the shared library both sites read, and the lenses are ours.
			lensRows, lerr := db.Query(`SELECT term_slug, audience, body
				FROM twoai_glossary_lenses ORDER BY term_slug, audience`)
			if lerr == nil {
				byTerm := map[string][]map[string]string{}
				for lensRows.Next() {
					var slug, audience, body string
					if lensRows.Scan(&slug, &audience, &body) == nil && slug != "" {
						byTerm[slug] = append(byTerm[slug], map[string]string{
							"audience": audience, "body": body,
						})
					}
				}
				lensRows.Close()
				attached, missing := 0, 0
				if terms, ok := g["terms"].([]any); ok {
					for _, raw := range terms {
						t, ok := raw.(map[string]any)
						if !ok {
							continue
						}
						slug, _ := t["slug"].(string)
						if ls, found := byTerm[slug]; found {
							t["lenses"] = ls
							attached++
						} else {
							missing++
						}
					}
				}
				// Said out loud because a silent zero here is exactly how this
				// went unnoticed: a term set with no lenses attached looks
				// identical to a term set that never had any.
				fmt.Printf("twoai_build: glossary lenses attached to %d terms, %d without\n",
					attached, missing)
			}
			if err := upsert("glossary/glossary.json", "glossary", g); err != nil {
				return err
			}
			terms := 0
			if t, ok := g["terms"].([]any); ok {
				terms = len(t)
			}
			setURLs("glossary/glossary.json", terms+1)
		}
	}

	// ---- F3: living lawsuit tracker from ai_lawsuits.
	lr, err := db.Query(`SELECT COALESCE(slug,''), case_name, court, COALESCE(docket,''),
			COALESCE(to_char(filed_date,'YYYY-MM-DD'),''), plaintiffs, defendants, category,
			status, COALESCE(status_badge,''), COALESCE(latest_development,''),
			COALESCE(to_char(latest_development_date,'YYYY-MM-DD'),''),
			COALESCE(executive_summary,''), COALESCE(why_it_matters,''), COALESCE(summary,''),
			COALESCE(claims,'[]'::jsonb)::text, COALESCE(timeline,'[]'::jsonb)::text,
			COALESCE(courtlistener_url,''), COALESCE(source_url,''), COALESCE(judge,'')
		FROM ai_lawsuits WHERE is_active IS NOT FALSE AND slug IS NOT NULL
		ORDER BY display_order, case_name`)
	if err != nil {
		return err
	}
	var cases []map[string]any
	for lr.Next() {
		var slug, name, court, docket, filed, pl, de, cat, status, badge, dev, devDate,
			exec, why, sum, claims, timeline, clURL, srcURL, judge string
		if err := lr.Scan(&slug, &name, &court, &docket, &filed, &pl, &de, &cat, &status, &badge,
			&dev, &devDate, &exec, &why, &sum, &claims, &timeline, &clURL, &srcURL, &judge); err != nil {
			lr.Close()
			return err
		}
		var cj, tj any
		json.Unmarshal([]byte(claims), &cj)
		json.Unmarshal([]byte(timeline), &tj)
		cases = append(cases, map[string]any{
			"slug": slug, "case_name": name, "court": court, "docket": docket,
			"filed_date": filed, "plaintiffs": pl, "defendants": de, "category": cat,
			"status": status, "status_badge": badge, "latest_development": dev,
			"latest_development_date": devDate, "executive_summary": exec,
			"why_it_matters": why, "summary": sum, "claims": cj, "timeline": tj,
			"courtlistener_url": clURL, "source_url": srcURL, "judge": judge,
		})
	}
	lr.Close()
	if err := upsert("lawsuits/lawsuits.json", "lawsuits", map[string]any{
		"cases": cases, "count": len(cases), "generated": today,
	}); err != nil {
		return err
	}
	setURLs("lawsuits/lawsuits.json", len(cases)+1)

	// ---- Static pages (about, contact, privacy, terms, disclaimer, disclosure).
	// Copy lives in site_content under twoai/static/*.json so nothing is typed
	// into a template; this stage only renders it into the twoai-content repo.
	sr, err := db.Query(`SELECT path, data::text FROM site_content
		WHERE path LIKE 'twoai/static/%' ORDER BY path`)
	if err != nil {
		return err
	}
	statics := 0
	for sr.Next() {
		var sp, sd string
		if err := sr.Scan(&sp, &sd); err != nil {
			sr.Close()
			return err
		}
		var doc map[string]any
		if json.Unmarshal([]byte(sd), &doc) != nil {
			continue
		}
		slug, _ := doc["slug"].(string)
		if slug == "" {
			slug = strings.TrimSuffix(sp[strings.LastIndex(sp, "/")+1:], ".json")
		}
		doc["generated"] = today
		if err := upsert("static/"+slug+".json", "static", doc); err != nil {
			sr.Close()
			return err
		}
		statics++
	}
	sr.Close()

	// ---- Most-visited pages, from twoai_ga_top_pages (the twoai_ga_top stage,
	// GA4 Data API). Exported as meta/popular-pages.json for the footer's
	// "Most visited" list. Reads the LATEST stored day rather than requiring
	// today: if the GA stage skipped (env missing, API refusal), the footer
	// keeps showing the last real ranking instead of going blank, and the
	// staleness is visible in the file's own day field.
	pr, err := db.Query(`SELECT day::text, rank, path, views, coalesce(title,'')
		FROM twoai_ga_top_pages
		WHERE day = (SELECT max(day) FROM twoai_ga_top_pages)
		ORDER BY rank`)
	if err == nil {
		type popRow struct {
			Rank  int    `json:"rank"`
			Path  string `json:"path"`
			Views int    `json:"views"`
			Title string `json:"title"`
		}
		var popDay string
		var pops []popRow
		for pr.Next() {
			var p popRow
			if pr.Scan(&popDay, &p.Rank, &p.Path, &p.Views, &p.Title) == nil {
				pops = append(pops, p)
			}
		}
		pr.Close()
		if len(pops) > 0 {
			if err := upsert("meta/popular-pages.json", "meta", map[string]any{
				"slug": "popular-pages", "day": popDay, "generated": today,
				"pages": pops,
			}); err != nil {
				return err
			}
		}
	}

	// ---- F4 tools directory. Catalog and deep profiles both already live in
	// site_content; this renders a hub, one page per category, and one page per
	// profiled tool. Tools with only a catalog row get a listing, not a page:
	// a page with nothing on it but a name and a link is thin by definition.
	tools, cats, profiles := twoaiToolData(db)
	toolPages := 0
	if len(tools) > 0 {
		byCat := map[string][]map[string]any{}
		for _, t := range tools {
			cn, _ := t["category"].(string)
			byCat[cn] = append(byCat[cn], t)
		}
		catIdx := []map[string]any{}
		for _, c := range cats {
			name, _ := c["name"].(string)
			cslug, _ := c["slug"].(string)
			list := byCat[name]
			if len(list) == 0 {
				continue
			}
			sort.Slice(list, func(i, j int) bool {
				a, _ := list[i]["name"].(string)
				b, _ := list[j]["name"].(string)
				return strings.ToLower(a) < strings.ToLower(b)
			})
			if err := upsert("tools/cat-"+cslug+".json", "tool-category", map[string]any{
				"name": name, "slug": cslug, "generated": today, "tools": list,
			}); err != nil {
				return err
			}
			catIdx = append(catIdx, map[string]any{"name": name, "slug": cslug, "count": len(list)})
			toolPages++
		}
		// Deep profiles, joined to the catalog row for the vendor link.
		byName := map[string]map[string]any{}
		for _, t := range tools {
			n, _ := t["name"].(string)
			byName[strings.ToLower(n)] = t
		}
		profiled := []map[string]any{}
		for slug, p := range profiles {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			cn, _ := pm["catalog_name"].(string)
			if cn == "" {
				cn, _ = pm["name"].(string)
			}
			if row := byName[strings.ToLower(cn)]; row != nil {
				pm["url"] = row["url"]
				pm["category"] = row["category"]
			}
			pm["slug"] = slug
			pm["generated"] = today
			if err := upsert("tools/"+slug+".json", "tool", pm); err != nil {
				return err
			}
			nm, _ := pm["name"].(string)
			tl, _ := pm["tagline"].(string)
			profiled = append(profiled, map[string]any{"slug": slug, "name": nm, "tagline": tl, "category": pm["category"]})
			toolPages++
		}
		sort.Slice(profiled, func(i, j int) bool {
			a, _ := profiled[i]["name"].(string)
			b, _ := profiled[j]["name"].(string)
			return strings.ToLower(a) < strings.ToLower(b)
		})
		sort.Slice(catIdx, func(i, j int) bool {
			a, _ := catIdx[i]["name"].(string)
			b, _ := catIdx[j]["name"].(string)
			return a < b
		})
		if err := upsert("tools/index.json", "tool-hub", map[string]any{
			"generated": today, "total": len(tools), "categories": catIdx,
			"profiled": profiled, "tools": tools,
		}); err != nil {
			return err
		}
		toolPages++
	}

	// ---- Talent Network form options. The join form's dropdowns come from the
	// same SQL that drives everything else: talent_questions rows, with the
	// governance list resolved live from the compliance library so a new
	// framework page automatically becomes a choosable answer. Published as
	// talent/options.json and baked into /talent/join/ at build.
	tqRows, tqErr := db.Query(`SELECT question_key, label, source, static_options::text
		FROM talent_questions WHERE active ORDER BY sort_order`)
	if tqErr == nil {
		type tq struct {
			Key     string   `json:"key"`
			Label   string   `json:"label"`
			Options []string `json:"options"`
		}
		var qs []tq
		for tqRows.Next() {
			var key, label, source, raw string
			if tqRows.Scan(&key, &label, &source, &raw) != nil {
				continue
			}
			var opts []string
			if source == "model_catalog" {
				// Every foundation-model vendor currently on the serving market,
				// from the same catalog that renders the models section - not a
				// hand-kept shortlist that goes stale. OpenRouter names come as
				// "Vendor: Model"; the vendor half, deduplicated, is the list.
				mr, err := db.Query(`SELECT DISTINCT split_part(name,':',1)
					FROM twoai_model_catalog
					WHERE source='openrouter' AND delisted_at IS NULL AND name LIKE '%:%'
					ORDER BY 1`)
				if err == nil {
					for mr.Next() {
						var v string
						if mr.Scan(&v) == nil && v != "" {
							opts = append(opts, v)
						}
					}
					mr.Close()
				}
				// Families that matter to candidates but are not routed through
				// OpenRouter, then the catch-all; static_options is the fallback
				// if the catalog query ever returns nothing.
				if len(opts) > 0 {
					opts = append(opts,
						"Stability AI (Stable Diffusion)", "Black Forest Labs (FLUX)",
						"Midjourney", "Runway", "ElevenLabs",
						"Open-weight fine-tunes (any family)")
				} else {
					json.Unmarshal([]byte(raw), &opts)
				}
			} else if strings.HasPrefix(source, "model_section:") {
				// One dropdown per model classification, fed by the same
				// catalog that renders the category pages. Top 100 by
				// downloads keeps the control usable; Other catches the rest.
				sec := strings.TrimPrefix(source, "model_section:")
				mr, err := db.Query(`SELECT name FROM twoai_model_catalog
					WHERE section=$1 AND delisted_at IS NULL
					ORDER BY COALESCE((data->>'downloads')::bigint,0) DESC, name LIMIT 100`, sec)
				if err == nil {
					for mr.Next() {
						var v string
						if mr.Scan(&v) == nil && v != "" {
							opts = append(opts, v)
						}
					}
					mr.Close()
				}
			} else if source == "model_reasoning" {
				// Reasoning models come from the OpenRouter half of the
				// catalog, where the reasoning flag lives.
				mr, err := db.Query(`SELECT name FROM twoai_model_catalog
					WHERE section='api' AND delisted_at IS NULL AND data->>'reasoning'='true'
					ORDER BY name LIMIT 100`)
				if err == nil {
					for mr.Next() {
						var v string
						if mr.Scan(&v) == nil && v != "" {
							opts = append(opts, v)
						}
					}
					mr.Close()
				}
			} else if source == "compliance" {
				cr, err := db.Query(`SELECT data->>'title' FROM site_content
					WHERE path LIKE 'governance/%'
					  AND path NOT IN ('governance/_meta.json','governance/sources.json','governance/ai-tools.json')
					  AND COALESCE(data->>'title','') <> '' ORDER BY data->>'title'`)
				if err == nil {
					for cr.Next() {
						var t string
						if cr.Scan(&t) == nil {
							opts = append(opts, t)
						}
					}
					cr.Close()
				}
			} else {
				json.Unmarshal([]byte(raw), &opts)
			}
			if len(opts) > 0 {
				qs = append(qs, tq{Key: key, Label: label, Options: opts})
			}
		}
		tqRows.Close()
		if len(qs) > 0 {
			if err := upsert("talent/options.json", "talent-options", map[string]any{
				"generated": today, "questions": qs,
			}); err != nil {
				return err
			}
			fmt.Printf("twoai_build: talent options questions=%d\n", len(qs))
		}
	}

	if err := talentPublish(db, today, upsert); err != nil {
		return err
	}

	compliance, err := twoaiCompliance(db, today, upsert)
	if err != nil {
		return err
	}

	if err := twoaiPaperExplain(db); err != nil {
		fmt.Println("twoai_paper_explain:", err)
	}

	research, err := twoaiResearch(db, today, upsert)
	if err != nil {
		return err
	}

	sources, err := twoaiSources(db, today, upsert)
	if err != nil {
		return err
	}

	downloads, err := twoaiDownloads(db, today, upsert)
	if err != nil {
		return err
	}

	vibe, err := twoaiVibeCoding(db, today, upsert)
	if err != nil {
		return err
	}
	_ = vibe

	caseStudies, err := twoaiCaseStudies(db, today, upsert)
	if err != nil {
		return err
	}
	_ = caseStudies

	caselaw, err := twoaiCaselaw(db, today, upsert)
	if err != nil {
		return err
	}
	_ = caselaw

	bookCatalog, err := twoaiBookCatalog(db, today, upsert)
	if err != nil {
		return err
	}
	_ = bookCatalog

	companies, err := twoaiCompanies(db, today, upsert)
	if err != nil {
		return err
	}

	people, err := twoaiPeople(db, today, upsert)
	if err != nil {
		return err
	}

	mcp, err := twoaiMCP(db, today, upsert)
	if err != nil {
		return err
	}

	ecosystem, err := twoaiEcosystem(db, today, upsert)
	if err != nil {
		return err
	}

	weeks, err := twoaiWeeks(db, today, upsert)
	if err != nil {
		return err
	}

	vendorNews, err := twoaiVendorNews(db, upsert)
	if err != nil {
		return err
	}

	timeline, err := twoaiTimeline(db, today, upsert)
	if err != nil {
		return err
	}

	watchPapers, err := twoaiResearchWatch(db, upsert)
	if err != nil {
		return err
	}

	jobListings, err := twoaiJobs(db, today, upsert)
	if err != nil {
		return err
	}

	// Enactment trigger: detect bills that became law, then publish, write
	// news, and queue the social post and Stephen's alert from that one event.
	if _, err := twoaiBillEvents(db, today); err != nil {
		return err
	}
	if _, err := twoaiBillEventsPublish(db, today, upsert); err != nil {
		return err
	}
	if _, err := twoaiBillEventsNews(db, today); err != nil {
		return err
	}
	if _, err := twoaiBillEventsQueue(db, today); err != nil {
		return err
	}
	if _, err := twoaiBillAlertsSend(db); err != nil {
		return err
	}
	if _, err := twoaiBillSocialToMarky(db); err != nil {
		return err
	}

	newsArchive, err := twoaiNewsArchive(db, upsert)
	if err != nil {
		return err
	}

	skillPages, err := twoaiSkills(db, today, upsert)
	if err != nil {
		return err
	}

	modelPages, err := twoaiModels(db, today)
	if err != nil {
		return err
	}
	fmt.Printf("twoai_build: model sections=%d\n", modelPages)

	factPages, err := twoaiCompanyFacts(db, today)
	if err != nil {
		return err
	}
	fmt.Printf("twoai_build: company fact sections=%d\n", factPages)

	chPages, err := twoaiCompanyHarvest(db, today)
	if err != nil {
		return err
	}
	fmt.Printf("twoai_build: company profiles patched=%d\n", chPages)

	orgPages, err := twoaiOrgFacts(db, today)
	if err != nil {
		return err
	}
	fmt.Printf("twoai_build: org fact sections=%d\n", orgPages)

	repoPages, err := twoaiRepos(db, today)
	if err != nil {
		return err
	}
	fmt.Printf("twoai_build: repo sections=%d\n", repoPages)

	statusPages, err := twoaiAPIStatus(db, today)
	if err != nil {
		return err
	}
	fmt.Printf("twoai_build: status sections=%d\n", statusPages)

	hwPages, err := twoaiHardware(db, today)
	if err != nil {
		return err
	}
	fmt.Printf("twoai_build: hardware+dataset sections=%d\n", hwPages)

	ihPages, err := twoaiIndustryHub(db, today)
	if err != nil {
		return err
	}
	fmt.Printf("twoai_build: industry hub sections=%d\n", ihPages)

	obsPages, err := twoaiObservatory(db, today)
	if err != nil {
		return err
	}
	fmt.Printf("twoai_build: observatory sections=%d\n", obsPages)

	graphPages, err := twoaiGraph(db, today)
	if err != nil {
		return err
	}
	fmt.Printf("twoai_build: graph sections=%d\n", graphPages)

	cmPages, err := twoaiCapexMA(db, today)
	if err != nil {
		return err
	}
	fmt.Printf("twoai_build: capex+ma sections=%d\n", cmPages)

	// After the SEC fetch on purpose: the Data Centers page renders this
	// run's capex rows, not yesterday's. Its first run read the table before
	// the fetch and showed four builders instead of eight.
	twoaiGridHarvest(db)
	dcPages, err := twoaiDatacenters(db, today)
	if gerr := twoaiGridPage(db, today); gerr != nil {
		fmt.Println("twoai_grid: page:", gerr)
	}
	if err != nil {
		fmt.Println("twoai_build: datacenters:", err)
		dcPages = 0
	}
	_ = dcPages

	secPages, err := twoaiSecurity(db, today)
	if err != nil {
		return err
	}
	fmt.Printf("twoai_build: security sections=%d\n", secPages)

	// Staleness tripwire for benchmark results. The result snapshots in
	// twoai_benchmarks.results are hand-curated from named evaluators, not
	// scraped: the source leaderboards are JS-rendered and re-baseline
	// without notice, so an automated scrape would either break silently or
	// publish wrong numbers silently, and the second failure mode is worse.
	// The trade is that a human (or a session) must refresh them, and this
	// warning is what makes that debt visible: it fires in the cron log when
	// any result set has gone unreviewed past its review interval, keyed on
	// updated_at so no text-date parsing of as_of is involved.
	var staleBench int
	var oldestBench sql.NullString
	if err := db.QueryRow(`SELECT count(*),
			min(slug || ' (' || to_char(updated_at, 'YYYY-MM-DD') || ')')
		FROM twoai_benchmarks
		WHERE results IS NOT NULL
		  AND updated_at < now() - (coalesce(review_interval_days, 90) || ' days')::interval`,
	).Scan(&staleBench, &oldestBench); err != nil {
		return err
	}
	if staleBench > 0 {
		fmt.Fprintf(os.Stderr, "twoai_build: WARNING %d benchmark result set(s) past review interval, oldest %s - refresh twoai_benchmarks.results\n",
			staleBench, oldestBench.String)
	}

	// The same debt check for the timeline's curated entries, for the same
	// reason and with one difference: these are keyed on reviewed_on, the date
	// a person last checked the entry, not updated_at, which any incidental
	// write would reset and so would quietly launder a stale entry as fresh.
	//
	// Intervals vary by how fast the ground moves under an entry rather than
	// being one number for the page. A 1943 paper is not going to change and
	// gets three years; a governance entry describes a regime with phased
	// obligations and gets one, because that is the entry most likely to be
	// quietly wrong. Auto-synced cases are excluded: they are rewritten from
	// the docket every run, so a review date on them would measure nothing.
	var staleTimeline int
	var oldestTimeline sql.NullString
	if err := db.QueryRow(`SELECT count(*),
			min(id || ' (' || to_char(reviewed_on, 'YYYY-MM-DD') || ')')
		FROM twoai_timeline
		WHERE origin <> 'auto-lawsuit'
		  AND (reviewed_on IS NULL
		       OR reviewed_on < current_date - (coalesce(review_interval_days, 1095) || ' days')::interval)`,
	).Scan(&staleTimeline, &oldestTimeline); err != nil {
		return err
	}
	if staleTimeline > 0 {
		fmt.Fprintf(os.Stderr, "twoai_build: WARNING %d timeline entr(ies) past review interval, oldest %s - re-verify the source and bump twoai_timeline.reviewed_on\n",
			staleTimeline, oldestTimeline.String)
	}

	fmt.Printf("twoai_build: states=%d bills=%d glossary=%v cases=%d statics=%d tools=%d weeks=%d ecosystem=%d compliance=%d mcp=%d people=%d companies=%d research=%d sources=%d vendor_news=%d arxiv_watch=%d timeline=%d jobs=%d news_archive=%d skills=%d downloads=%d ok=true\n",
		len(index), total, glossary != "", len(cases), statics, toolPages, weeks, ecosystem, compliance, mcp, people, companies, research, sources, vendorNews, watchPapers, timeline, jobListings, newsArchive, skillPages, downloads)
	return nil
}

// twoaiPublishR2 writes the whole content set to R2 as one compressed bundle.
//
// WHY THIS EXISTS. twoai_publish PUTs every changed page to GitHub through the
// contents API, one HTTP call per file. That was fine at 200 files. The MCP
// registry made it 1,616 and the stage took about twenty minutes, which is a
// scaling wall rather than a slow day: another registry-sized factory would
// exceed the cron window outright.
//
// The design is srj's and it is better than overwriting one fixed key. Every
// run writes an immutable bundle under a content hash, then updates one small
// manifest that names it. Publishing is therefore atomic, because the manifest
// flips only after the bundle is fully uploaded, and rollback is editing one
// tiny object. A reader that fetches the manifest can never see a half-written
// bundle.
//
// Credentials are R2 S3-compatible and scoped to this bucket alone. They are
// checked by name and never printed: the stage reports which are missing so a
// misconfiguration is obvious from the log without leaking anything.
// twoaiPublishGuard is the poison-the-well backstop. The pipeline rebuilds
// twoai_pages from many upstream sources every run; a corrupted feed, a
// spoofed API response, or a bug in one build stage can collapse the page set,
// and without a check the collapse would publish over thousands of good pages
// on the next export with no human in the loop. A site whose whole value is
// being trustworthy enough to cite must not silently ship a fraction of
// itself.
//
// The rule is RELATIVE, not a fixed floor: compare this run's page count to
// the highest count ever published, stored in twoai_publish_hwm. If the count
// has fallen below a fraction of the high-water mark, refuse to publish and
// return an error, so yesterday's bundle keeps serving. A legitimate large
// removal (a section retired on purpose) clears the gate by lowering the
// threshold once: set the env override TWOAI_PUBLISH_MIN to the new floor for
// one run, or the operator re-baselines the mark by hand. The mark only ever
// rises automatically; it never falls on its own, which is the point.
//
// Returns (ok, count, err). ok=false means DO NOT PUBLISH.
func twoaiPublishGuard(db *sql.DB) (bool, int, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_publish_hwm (
		id int PRIMARY KEY DEFAULT 1,
		high_water int NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now(),
		CONSTRAINT twoai_publish_hwm_single CHECK (id = 1))`); err != nil {
		return false, 0, err
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM twoai_pages`).Scan(&count); err != nil {
		return false, 0, err
	}
	if count == 0 {
		return false, 0, fmt.Errorf("publish guard: twoai_pages is empty, refusing to publish")
	}
	var hwm int
	db.QueryRow(`SELECT high_water FROM twoai_publish_hwm WHERE id = 1`).Scan(&hwm)

	// First run, or the mark grew: this count is at least as high as any seen,
	// so it is trustworthy and becomes the new mark. Publish.
	if count >= hwm {
		db.Exec(`INSERT INTO twoai_publish_hwm (id, high_water, updated_at)
			VALUES (1, $1, now())
			ON CONFLICT (id) DO UPDATE SET high_water = EXCLUDED.high_water, updated_at = now()`, count)
		return true, count, nil
	}

	// Below the mark. A small dip is normal churn; a collapse is the attack.
	// Threshold is 85% of the mark by default, overridable for a deliberate
	// large removal.
	threshold := (hwm * 85) / 100
	if env := os.Getenv("TWOAI_PUBLISH_MIN"); env != "" {
		if v, err := strconv.Atoi(env); err == nil && v >= 0 {
			threshold = v
		}
	}
	if count < threshold {
		return false, count, fmt.Errorf(
			"publish guard: %d pages is below the safety threshold of %d (high-water %d); "+
				"refusing to publish so the last good bundle keeps serving. If this drop is "+
				"intentional, set TWOAI_PUBLISH_MIN=%d for one run to re-baseline",
			count, threshold, hwm, count)
	}
	// Within the acceptable band below the mark: publish, but do NOT lower the
	// mark. The mark tracks the best the site has ever been, so a run that is
	// merely a bit smaller cannot erode the bar the next collapse is measured
	// against.
	return true, count, nil
}

func twoaiPublishR2(db *sql.DB) error {
	if ok, n, err := twoaiPublishGuard(db); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("publish guard blocked R2 publish at %d pages", n)
	}
	keyID := os.Getenv("R2_ACCESS_KEY_ID")
	secret := os.Getenv("R2_SECRET_ACCESS_KEY")
	endpoint := strings.TrimRight(os.Getenv("R2_S3_ENDPOINT"), "/")
	bucket := os.Getenv("R2_BUCKET")
	if bucket == "" {
		bucket = "twoai-content"
	}
	var missing []string
	if keyID == "" {
		missing = append(missing, "R2_ACCESS_KEY_ID")
	}
	if secret == "" {
		missing = append(missing, "R2_SECRET_ACCESS_KEY")
	}
	if endpoint == "" {
		missing = append(missing, "R2_S3_ENDPOINT")
	}
	if len(missing) > 0 {
		return fmt.Errorf("not configured, missing: %s", strings.Join(missing, ", "))
	}

	rows, err := db.Query(`SELECT path, data::text FROM twoai_pages ORDER BY path`)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := 0
	for rows.Next() {
		var p, d string
		if rows.Scan(&p, &d) != nil {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: p, Mode: 0o644, Size: int64(len(d)), ModTime: time.Now(),
		}); err != nil {
			rows.Close()
			return err
		}
		if _, err := tw.Write([]byte(d)); err != nil {
			rows.Close()
			return err
		}
		files++
	}
	rows.Close()
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if files == 0 {
		return fmt.Errorf("no pages to publish")
	}

	body := buf.Bytes()
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	bundleKey := "bundles/" + hash[:16] + ".tar.gz"

	if err := r2Put(endpoint, bucket, keyID, secret, bundleKey, "application/gzip", body); err != nil {
		return err
	}
	manifest, _ := json.Marshal(map[string]any{
		"bundle": bundleKey, "sha256": hash, "files": files,
		"bytes": len(body), "generated": time.Now().UTC().Format(time.RFC3339),
	})
	if err := r2Put(endpoint, bucket, keyID, secret, "manifest.json", "application/json", manifest); err != nil {
		return err
	}
	fmt.Printf("twoai_publish_r2: files=%d bytes=%d bundle=%s ok=true\n", files, len(body), bundleKey)
	return nil
}

// r2Put signs and sends one object with AWS Signature Version 4, which is what
// R2's S3-compatible API expects. Written out rather than pulling in the AWS
// SDK: this is the only S3 call the pipeline makes, and a single dependency-free
// function is easier to audit than a vendored SDK.
func r2Put(endpoint, bucket, keyID, secret, key, contentType string, body []byte) error {
	u, err := url.Parse(endpoint + "/" + bucket + "/" + key)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])

	canonicalHeaders := fmt.Sprintf("content-type:%s\nhost:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		contentType, u.Host, payloadHash, amzDate)
	signedHeaders := "content-type;host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		"PUT", u.EscapedPath(), "", canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")
	crHash := sha256.Sum256([]byte(canonicalRequest))

	scope := dateStamp + "/auto/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(crHash[:]),
	}, "\n")

	mac := func(k, d []byte) []byte {
		h := hmac.New(sha256.New, k)
		h.Write(d)
		return h.Sum(nil)
	}
	kDate := mac([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := mac(kDate, []byte("auto"))
	kService := mac(kRegion, []byte("s3"))
	kSigning := mac(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(mac(kSigning, []byte(stringToSign)))

	req, err := http.NewRequest("PUT", u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		keyID, scope, signedHeaders, signature))

	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("r2 PUT %s: %d %s", key, resp.StatusCode, b)
	}
	return nil
}

// twoaiUID returns the short alphanumeric identifier a page is published under.
//
// Stephen's instruction 2026-08-03: everything already published keeps its URL,
// and every NEW section addresses its pages by identifier below the category
// level rather than by slug. The identifier is the first eight hex characters
// of a SHA-256 of a stable key, so it is deterministic, collision-resistant at
// this scale, and survives a person or company being renamed, which is the
// whole point of addressing by identifier rather than by name.
func twoaiUID(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])[:8]
}

// twoaiResearch renders the AI research library from twoai_research_papers,
// which is seeded through the Consensus academic index.
//
// WHAT IS PUBLISHED. Bibliographic metadata, our own one-line note, and — when
// captured — the abstract from Consensus with attribution and a subscription
// note, plus three plain-English explanations of each paper for a beginner, a
// practitioner and a business reader. The abstract's provenance is stamped on
// every page ("Abstract via Consensus, subscription required") and Consensus is
// the linked source. No abstract is reproduced without that credit.
//
// STALENESS IS STATED. Consensus is an MCP connector inside a Claude session,
// not an API this pipeline can call, so unlike LegiScan or CourtListener this
// table cannot refresh itself on the daily run; it is topped up by a scheduled
// Cowork task that fetches abstracts and writes the three explanations. A
// section that silently goes stale is worse than one that says when it was
// last added to, so the pages publish the most recent added_on date rather
// than implying a currency they have not earned.
//
// WHAT THIS EMITS. One JSON per topic at research/{uid}.json (shelf view),
// one JSON per paper at research/paper/{uid}.json (paper detail page), and one
// research/index.json (library hub). The paper-detail files include everything
// needed to render the fixed page layout Stephen specified: title, type, year,
// authors, journal, volume, DOI, then abstract (with attribution), then the
// three explanations.
func twoaiResearch(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	rows, err := db.Query(`SELECT uid, title, COALESCE(authors,''), COALESCE(year,0),
			COALESCE(journal,''), COALESCE(citations,0), url, topic, COALESCE(our_note,''),
			COALESCE(added_on::text,''),
			COALESCE(abstract,''), COALESCE(abstract_source,''), COALESCE(doi,''),
			COALESCE(volume,''), COALESCE(paper_type,''),
			COALESCE(explain_beginner,''), COALESCE(explain_practitioner,''),
			COALESCE(explain_business,'')
		FROM twoai_research_papers ORDER BY citations DESC NULLS LAST, year DESC`)
	if err != nil {
		return 0, nil
	}
	type paper struct {
		UID             string `json:"uid"`
		Title           string `json:"title"`
		Authors         string `json:"authors,omitempty"`
		Year            int    `json:"year,omitempty"`
		Journal         string `json:"journal,omitempty"`
		Citations       int    `json:"citations"`
		URL             string `json:"url"`
		Topic           string `json:"topic"`
		Note            string `json:"note,omitempty"`
		Added           string `json:"added,omitempty"`
		Abstract        string `json:"abstract,omitempty"`
		AbstractSource  string `json:"abstract_source,omitempty"`
		DOI             string `json:"doi,omitempty"`
		Volume          string `json:"volume,omitempty"`
		Type            string `json:"paper_type,omitempty"`
		ExpBeginner     string `json:"explain_beginner,omitempty"`
		ExpPractitioner string `json:"explain_practitioner,omitempty"`
		ExpBusiness     string `json:"explain_business,omitempty"`
		Slug            string `json:"slug,omitempty"`
		TopicName       string `json:"topic_name,omitempty"`
		PageURL         string `json:"page_url,omitempty"`
	}
	var all []paper
	latest := ""
	for rows.Next() {
		var p paper
		if rows.Scan(&p.UID, &p.Title, &p.Authors, &p.Year, &p.Journal, &p.Citations,
			&p.URL, &p.Topic, &p.Note, &p.Added,
			&p.Abstract, &p.AbstractSource, &p.DOI, &p.Volume, &p.Type,
			&p.ExpBeginner, &p.ExpPractitioner, &p.ExpBusiness) != nil {
			continue
		}
		if p.Added > latest {
			latest = p.Added
		}
		p.PageURL = "/research/paper/" + p.UID + "/"
		all = append(all, p)
	}
	rows.Close()
	if len(all) == 0 {
		return 0, nil
	}

	label := map[string]string{
		"capabilities-and-limits": "Capabilities and Limits",
		"reasoning":               "Reasoning",
		"architectures":           "Architectures",
		"surveys":                 "Surveys of the Field",
		"evaluation":              "Evaluation and Benchmarks",
		"security":                "Security and Privacy",
		"governance":              "Governance and Policy",
		"eu-ai-act":               "The EU AI Act",
		"bias-and-fairness":       "Bias and Fairness",
		"applications":            "Applications by Sector",
		"methodology":             "Research Methodology",
		"healthcare-regulation":   "Healthcare Regulation",
	}

	// Backfill topic label on each paper so the paper-detail page can render
	// the human name of the shelf it lives on without a second lookup.
	for i := range all {
		all[i].Slug = all[i].Topic
		all[i].TopicName = label[all[i].Topic]
		if all[i].TopicName == "" {
			all[i].TopicName = strings.ReplaceAll(all[i].Topic, "-", " ")
		}
	}

	byTopic := map[string][]paper{}
	order := []string{}
	for _, p := range all {
		if _, ok := byTopic[p.Topic]; !ok {
			order = append(order, p.Topic)
		}
		byTopic[p.Topic] = append(byTopic[p.Topic], p)
	}
	sort.Slice(order, func(i, j int) bool { return len(byTopic[order[i]]) > len(byTopic[order[j]]) })

	count := 0
	topics := []map[string]any{}
	for _, t := range order {
		name := label[t]
		if name == "" {
			name = strings.ReplaceAll(t, "-", " ")
		}
		// Emit at research/{slug}.json only. The earlier hash-uid path
		// (research/{uid}.json) created a duplicate that collided in the
		// site's [topic].astro route on the same slug, letting the
		// alphabetically-first hash-named file win and its papers, which
		// pre-dated the uid backfill, rendered /research/paper/undefined/
		// on every link. The uid is still included in the payload so the
		// hub template can key on it.
		uid := twoaiUID("research-topic:" + t)
		if err := upsert("research/"+t+".json", "research-topic", map[string]any{
			"uid": uid, "topic": t, "slug": t, "name": name, "papers": byTopic[t],
			"total": len(byTopic[t]), "generated": today, "last_added": latest,
		}); err != nil {
			return count, err
		}
		count++
		topics = append(topics, map[string]any{
			"uid": uid, "topic": t, "slug": t, "name": name, "count": len(byTopic[t]),
		})
	}

	// Per-paper detail files. One JSON per row so the /research/paper/{uid}/
	// route reads exactly the paper it needs at build time.
	for _, p := range all {
		if err := upsert("research/paper/"+p.UID+".json", "research-paper", map[string]any{
			"uid": p.UID, "title": p.Title, "authors": p.Authors,
			"year": p.Year, "journal": p.Journal, "volume": p.Volume,
			"doi": p.DOI, "paper_type": p.Type,
			"citations": p.Citations, "url": p.URL,
			"topic": p.Topic, "topic_slug": p.Topic, "topic_name": p.TopicName,
			"abstract": p.Abstract, "abstract_source": p.AbstractSource,
			"note":                 p.Note,
			"explain_beginner":     p.ExpBeginner,
			"explain_practitioner": p.ExpPractitioner,
			"explain_business":     p.ExpBusiness,
			"added":                p.Added, "generated": today,
		}); err != nil {
			return count, err
		}
		count++
	}

	top := all
	if len(top) > 12 {
		top = top[:12]
	}
	if err := upsert("research/index.json", "research-hub", map[string]any{
		"uid": twoaiUID("section:research-library"), "topics": topics,
		"most_cited": top, "papers": all, "total": len(all),
		"generated": today, "last_added": latest,
	}); err != nil {
		return count, err
	}
	return count + 1, nil
}

// twoaiSources renders the central sources index at sources/index.json.
//
// WHY IT EXISTS. Every fact on theworldofai.org that is not our own reasoning
// traces to a primary source: a statute, a regulator, a standards body, a
// peer-reviewed paper. The reader is entitled to find that source without
// hunting the site for the page that cites it. This file is the bibliography
// pattern from a book: one page, six sections, every source with a stable
// deep link. Migrating it from srjconsultingservices.com to theworldofai.org
// so the destination has its own copy of the same discipline.
//
// WHAT IT EMITS. One JSON with sections in a fixed order (EU and Council of
// Europe, US federal, US state and city, other jurisdictions, standards
// bodies, peer-reviewed research), each section carrying its label and its
// list of {uid, title, url, authors, year, journal} entries in the same
// sort_order kept in twoai_sources. Research entries carry author/year/journal
// so the page can show them in bibliography form; primary sources do not.
func twoaiSources(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	rows, err := db.Query(`SELECT uid, section, section_label, sort_order, title, url,
			COALESCE(authors,''), COALESCE(year,0), COALESCE(journal,'')
		FROM twoai_sources
		ORDER BY CASE section
			WHEN 'eu' THEN 1
			WHEN 'us-federal' THEN 2
			WHEN 'us-state' THEN 3
			WHEN 'intl' THEN 4
			WHEN 'standards' THEN 5
			WHEN 'research' THEN 6
			ELSE 99 END, sort_order, title`)
	if err != nil {
		return 0, nil
	}
	type item struct {
		UID     string `json:"uid"`
		Title   string `json:"title"`
		URL     string `json:"url"`
		Authors string `json:"authors,omitempty"`
		Year    int    `json:"year,omitempty"`
		Journal string `json:"journal,omitempty"`
	}
	type sect struct {
		Key   string `json:"key"`
		Label string `json:"label"`
		Items []item `json:"items"`
	}
	var sections []sect
	byKey := map[string]int{}
	total := 0
	for rows.Next() {
		var it item
		var key, lbl string
		var sortOrd int
		if rows.Scan(&it.UID, &key, &lbl, &sortOrd, &it.Title, &it.URL,
			&it.Authors, &it.Year, &it.Journal) != nil {
			continue
		}
		idx, ok := byKey[key]
		if !ok {
			sections = append(sections, sect{Key: key, Label: lbl})
			idx = len(sections) - 1
			byKey[key] = idx
		}
		sections[idx].Items = append(sections[idx].Items, it)
		total++
	}
	rows.Close()
	if total == 0 {
		return 0, nil
	}

	// The curated list above is the citation apparatus: the laws, standards,
	// and papers the written pages cite. It is not the whole answer to "what
	// are your sources", because it says nothing about the feeds and APIs the
	// site actually runs on. Those are enumerated from twoai_data_sources, in
	// SQL for the same reason the vendor feeds are: a source list hardcoded in
	// Go is a source list that stops matching reality.
	//
	// Each row can name a table to count, so the page carries a live figure
	// rather than a claim. count_where is a fragment written by us in this
	// table, never user input, and the query is built only from those two
	// fields.
	type feedSrc struct {
		Key       string `json:"key"`
		Name      string `json:"name"`
		URL       string `json:"url"`
		Category  string `json:"category"`
		Publisher string `json:"publisher,omitempty"`
		WhatWeUse string `json:"what_we_use,omitempty"`
		Terms     string `json:"terms,omitempty"`
		Records   int    `json:"records,omitempty"`
	}
	var feeds []feedSrc
	frows, ferr := db.Query(`SELECT key, name, url, category, publisher, what_we_use, terms_note,
			COALESCE(count_table,''), COALESCE(count_where,'')
		FROM twoai_data_sources WHERE active ORDER BY sort_order, name`)
	if ferr == nil {
		for frows.Next() {
			var f feedSrc
			var tbl, where string
			if frows.Scan(&f.Key, &f.Name, &f.URL, &f.Category, &f.Publisher,
				&f.WhatWeUse, &f.Terms, &tbl, &where) != nil {
				continue
			}
			if tbl != "" {
				q := "SELECT count(*) FROM " + tbl
				if where != "" {
					q += " WHERE " + where
				}
				// A source whose table has gone is worth knowing about, but not
				// worth failing the build over: the row still lists correctly
				// without a count.
				if err := db.QueryRow(q).Scan(&f.Records); err != nil {
					fmt.Fprintf(os.Stderr, "twoai_build: sources count for %s: %v\n", f.Key, err)
				}
			}
			feeds = append(feeds, f)
		}
		frows.Close()
	}

	// Vendor feeds are listed individually rather than as one line, because
	// "27 company blogs" is not a source list, it is a summary of one.
	type vfeed struct {
		Vendor    string `json:"vendor"`
		URL       string `json:"url"`
		EntityUID string `json:"entity_uid,omitempty"`
		Healthy   bool   `json:"healthy"`
	}
	var vfeeds []vfeed
	vrows, verr := db.Query(`SELECT vendor, feed_url, COALESCE(entity_uid,''), last_ok IS NOT NULL
		FROM twoai_vendor_feeds WHERE active ORDER BY vendor`)
	if verr == nil {
		for vrows.Next() {
			var v vfeed
			if vrows.Scan(&v.Vendor, &v.URL, &v.EntityUID, &v.Healthy) == nil {
				vfeeds = append(vfeeds, v)
			}
		}
		vrows.Close()
	}

	// Our own titles. These are the one place on this site where linking to
	// srjconsultingservices.com is right: a reader asking what we publish is
	// being pointed at a product, not being handed our own site as evidence
	// for a claim about AI.
	type book struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		Subtitle  string `json:"subtitle,omitempty"`
		Pillar    string `json:"pillar,omitempty"`
		Status    string `json:"status"`
		Published string `json:"published,omitempty"`
		AmazonURL string `json:"amazon_url,omitempty"`
		Pages     int    `json:"pages,omitempty"`
	}
	// Read press_books directly - the canonical table - rather than the
	// books/books.json copy in site_content. The copy went stale within
	// three weeks of being introduced (book 5 published 2026-08-03; the
	// copy still said forthcoming on 2026-08-14), which is the whole
	// argument for never rendering from a snapshot when the table that
	// produced it is one query away.
	var books []book
	if brows, err := db.Query(`SELECT book_number, title, COALESCE(subtitle,''), pillar,
		status, COALESCE(published_on::text,''), COALESCE(amazon_url,''), COALESCE(pages,0)
		FROM press_books ORDER BY book_number`); err == nil {
		for brows.Next() {
			var b book
			if brows.Scan(&b.Number, &b.Title, &b.Subtitle, &b.Pillar,
				&b.Status, &b.Published, &b.AmazonURL, &b.Pages) == nil {
				books = append(books, b)
			}
		}
		brows.Close()
	}

	// CATALOGUE DRIFT. A published volume should carry a Wikidata item and an
	// Open Library work; both are created by hand and both have been forgotten
	// before. Volume V went out on 2026-08-03 and its Wikidata item was created
	// five days later only because someone happened to ask.
	//
	// This warns, it does not act. Creating a public catalogue record is
	// irreversible in practice — an item created in error needs a community
	// deletion nomination — and the data feeding it has been wrong before: two
	// ISBNs in the books table turned out to belong to a registrant block SRJ
	// does not own, and Volume VI carried a null page count for a day after
	// publication. A cron that published on status change would have pushed each
	// of those into a public catalogue and reported ok=true.
	//
	// Reads book_catalogue_status, NOT the three tables by hand. book_identifiers
	// keys on books.book_id while press_books keys on book_number, and the two
	// diverge: The AI Lawyer is books row 7 but book_number 10, so a direct join
	// on book_number credits its Wikidata item to Volume VII instead. The view
	// exists to make that mistake unavailable.
	type gap struct {
		num     int
		title   string
		missing []string
	}
	var gaps []gap
	if grows, err := db.Query(`SELECT book_number, title,
			wikidata_qid IS NULL, openlibrary_work IS NULL, books_row IS NULL
		FROM book_catalogue_status
		WHERE status = 'available'
		ORDER BY book_number`); err == nil {
		for grows.Next() {
			var g gap
			var noWD, noOL, noRow bool
			if grows.Scan(&g.num, &g.title, &noWD, &noOL, &noRow) == nil {
				if noRow {
					// No books row means no identifier can be attached at all,
					// so this is the blocker to report rather than the symptom.
					g.missing = append(g.missing, "books row")
				} else {
					if noWD {
						g.missing = append(g.missing, "wikidata")
					}
					if noOL {
						g.missing = append(g.missing, "openlibrary_work")
					}
				}
				if len(g.missing) > 0 {
					gaps = append(gaps, g)
				}
			}
		}
		grows.Close()
	}
	for _, g := range gaps {
		fmt.Printf("twoai_build: CATALOGUE DRIFT: book %d %q is published with no %s. "+
			"Create the record by hand, then record the id in book_identifiers.\n",
			g.num, g.title, strings.Join(g.missing, " and no "))
	}

	if err := upsert("sources/index.json", "sources-hub", map[string]any{
		"uid":          twoaiUID("section:sources-index"),
		"sections":     sections,
		"total":        total,
		"data_sources": feeds,
		"vendor_feeds": vfeeds,
		"books":        books,
		"generated":    today,
	}); err != nil {
		return 0, err
	}
	fmt.Printf("twoai_build: sources citations=%d data_sources=%d vendor_feeds=%d books=%d ok=true\n",
		total, len(feeds), len(vfeeds), len(books))
	return 1, nil
}

// twoaiEntity registers a named thing in twoai_entities and returns the
// identifier every part of the site should use for it.
//
// THE PROBLEM IT SOLVES. The same company shows up in four places already: as a
// vendor in the tools catalog, as a defendant in the lawsuit tracker, as a
// publisher in the MCP registry, and soon as the maker of a model. Each source
// spells it differently, so "OpenAI", "OpenAI, Inc." and "OpenAI Global, LLC"
// are three strings for one organisation. Joining on the display name means
// the cross-references silently disagree.
//
// So the identifier is derived from a NORMALIZED name, lowercased with legal
// suffixes and punctuation stripped, and the mapping is stored. Any factory
// that calls this with any spelling gets the same uid back, which is what makes
// a company page able to say what a company ships, who is suing it, and what it
// has published to MCP, and be sure all three refer to the same entity. The
// original spelling is kept as an alias so the trail from source to identifier
// stays visible rather than being lost in a hash.
var entitySuffixRe = regexp.MustCompile(`(?i)[ ,]+(inc|incorporated|llc|l\.l\.c|ltd|limited|corp|corporation|co|company|plc|gmbh|sa|s\.a|ag|pbc|lp|llp|holdings|labs|technologies|technology)\.?$`)
var entityPunctRe = regexp.MustCompile(`[^a-z0-9]+`)

// nonAlnumLower reduces a name to letters and digits for exact-equality
// matching. Shared by every join that must not be a substring test.
func nonAlnumLower(s string) string {
	return entityPunctRe.ReplaceAllString(strings.ToLower(s), "")
}

func twoaiEntityID(db *sql.DB, kind, name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	for {
		stripped := entitySuffixRe.ReplaceAllString(n, "")
		if stripped == n {
			break
		}
		n = stripped
	}
	n = strings.Trim(entityPunctRe.ReplaceAllString(n, "-"), "-")
	if n == "" {
		n = strings.Trim(entityPunctRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	}
	uid := twoaiUID(kind + ":" + n)
	if db != nil {
		db.Exec(`INSERT INTO twoai_entities (uid, kind, name, normalized, aliases)
			VALUES ($1,$2,$3,$4, jsonb_build_array($3::text))
			ON CONFLICT (uid) DO UPDATE SET last_seen = now(),
				aliases = CASE WHEN twoai_entities.aliases ? $3 THEN twoai_entities.aliases
					ELSE twoai_entities.aliases || jsonb_build_array($3::text) END`,
			uid, kind, strings.TrimSpace(name), n)
	}
	return uid
}

// twoaiCompanies builds the AI company directory from the vendors already in
// the tools catalog, cross-referenced against the lawsuit tracker and the MCP
// registry.
//
// WHY MOST VENDORS DO NOT GET A PAGE. The catalog names 261 vendors, but 229
// of them publish exactly one tool we track. A company page for those would
// repeat the tool page with a different heading, which is duplicate content
// against our own site and adds nothing a reader could not already see. So a
// vendor earns a page when it has two or more tools, is a defendant in a case
// we track, or publishes an MCP server. Everyone else stays a row on the hub
// with a link to their tool.
//
// WHAT MAKES THE PAGE WORTH HAVING. Not the vendor name, which is a fact we
// already had, but the join: what this company ships, whether anyone is suing
// it, and whether it has published to the MCP registry. Three separate sources
// answering one question about one organisation is the thing a directory of
// tools cannot do.
func twoaiCompanies(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	var catalog string
	if err := db.QueryRow(`SELECT data::text FROM site_content WHERE path='resources/tools.json'`).Scan(&catalog); err != nil {
		return 0, nil
	}
	var cat struct {
		Tools []struct {
			Name     string `json:"name"`
			Vendor   string `json:"vendor"`
			Note     string `json:"note"`
			URL      string `json:"url"`
			Category string `json:"category"`
		} `json:"tools"`
	}
	if json.Unmarshal([]byte(catalog), &cat) != nil || len(cat.Tools) == 0 {
		return 0, nil
	}

	// Deep profiles tell us which tools have their own page to link to.
	profiled := map[string]string{}
	var prof string
	if err := db.QueryRow(`SELECT data::text FROM site_content WHERE path='resources/tool-profiles.json'`).Scan(&prof); err == nil {
		var p struct {
			Profiles map[string]map[string]any `json:"profiles"`
		}
		if json.Unmarshal([]byte(prof), &p) == nil {
			for slug, v := range p.Profiles {
				if n, _ := v["name"].(string); n != "" {
					profiled[strings.ToLower(n)] = slug
				}
			}
		}
	}

	type product struct {
		Name     string `json:"name"`
		Note     string `json:"note,omitempty"`
		URL      string `json:"url,omitempty"`
		Category string `json:"category,omitempty"`
		Profile  string `json:"profile,omitempty"`
	}
	type company struct {
		UID      string              `json:"uid"`
		Name     string              `json:"name"`
		Products []product           `json:"products"`
		Cases    []map[string]string `json:"cases,omitempty"`
		MCP      []map[string]string `json:"mcp,omitempty"`
		Pages    bool                `json:"has_page"`
	}

	order := []string{}
	by := map[string]*company{}
	for _, t := range cat.Tools {
		v := strings.TrimSpace(t.Vendor)
		if v == "" {
			continue
		}
		c := by[v]
		if c == nil {
			c = &company{UID: twoaiEntityID(db, "company", v), Name: v}
			by[v] = c
			order = append(order, v)
		}
		c.Products = append(c.Products, product{
			Name: t.Name, Note: t.Note, URL: t.URL, Category: t.Category,
			Profile: profiled[strings.ToLower(t.Name)],
		})
	}

	// Litigation and MCP presence, matched on the company name appearing in the
	// defendants field or the server publisher. Deliberately conservative: a
	// short vendor name could match loosely, so require a word boundary.
	for _, v := range order {
		c := by[v]
		if len(v) < 3 {
			continue
		}
		pattern := "(^|[^a-z])" + strings.ToLower(regexp.QuoteMeta(v)) + "([^a-z]|$)"
		lr, err := db.Query(`SELECT slug, case_name, COALESCE(court,'') FROM ai_lawsuits
			WHERE is_active AND lower(COALESCE(defendants,'')) ~ $1 ORDER BY filed_date DESC LIMIT 12`, pattern)
		if err == nil {
			for lr.Next() {
				var slug, name, court string
				if lr.Scan(&slug, &name, &court) == nil {
					c.Cases = append(c.Cases, map[string]string{"slug": slug, "name": name, "court": court})
				}
			}
			lr.Close()
		}
		mr, err := db.Query(`SELECT slug, name, COALESCE(description,'') FROM twoai_mcp_servers
			WHERE status='active' AND lower(name) LIKE $1 ORDER BY name LIMIT 12`,
			"%"+strings.ToLower(strings.ReplaceAll(v, " ", ""))+"%")
		if err == nil {
			for mr.Next() {
				var slug, name, desc string
				if mr.Scan(&slug, &name, &desc) == nil {
					c.MCP = append(c.MCP, map[string]string{"slug": slug, "name": name, "description": desc})
				}
			}
			mr.Close()
		}
	}

	count := 0
	index := []map[string]any{}
	sort.Strings(order)

	// Load company profiles once. Every field is optional: nothing is embedded
	// unless twoai_company_profiles has a row for that uid, so a directory
	// entry with no profile still renders exactly as before.
	profiles := map[string]map[string]any{}
	if pr, err := db.Query(`SELECT uid, COALESCE(org_type,''), for_profit, founded,
			COALESCE(headquarters,''), COALESCE(website,''), COALESCE(ticker,''),
			COALESCE(cik,''), last_revenue_usd, last_revenue_end::text,
			COALESCE(last_revenue_form,''), verified_on::text
		FROM twoai_company_profiles`); err == nil {
		for pr.Next() {
			var uid, orgType, hq, website, ticker, cik, revForm string
			var forProfit sql.NullBool
			var founded sql.NullInt32
			var revUsd sql.NullInt64
			var revEnd, verified sql.NullString
			if pr.Scan(&uid, &orgType, &forProfit, &founded, &hq, &website, &ticker,
				&cik, &revUsd, &revEnd, &revForm, &verified) != nil {
				continue
			}
			p := map[string]any{}
			if orgType != "" {
				p["org_type"] = orgType
			}
			if forProfit.Valid {
				p["for_profit"] = forProfit.Bool
			}
			if founded.Valid {
				p["founded"] = founded.Int32
			}
			if hq != "" {
				p["headquarters"] = hq
			}
			if website != "" {
				p["website"] = website
			}
			if ticker != "" {
				p["ticker"] = ticker
			}
			if cik != "" {
				p["cik"] = cik
			}
			if revUsd.Valid {
				p["last_revenue_usd"] = revUsd.Int64
			}
			if revEnd.Valid {
				p["last_revenue_end"] = revEnd.String
			}
			if revForm != "" {
				p["last_revenue_form"] = revForm
			}
			if verified.Valid {
				p["verified_on"] = verified.String
			}
			profiles[uid] = p
		}
		pr.Close()
	}

	for _, v := range order {
		c := by[v]
		// Every tracked company gets a page. The directory was linking entries
		// whose pages did not exist - a reader clicked a company and landed on
		// nothing. Thin companies say less on their page; they no longer say
		// nothing at a dead URL.
		c.Pages = true
		if c.Pages {
			payload := map[string]any{"company": c, "generated": today}
			if p, ok := profiles[c.UID]; ok && len(p) > 0 {
				// Embed the profile map alongside the aggregated tools/cases/mcp
				// so a single fetch of companies/{uid}.json carries every field
				// the template renders. Nothing is duplicated: the profile map
				// only holds fields that are not otherwise on `company`.
				enriched := map[string]any{}
				enriched["uid"] = c.UID
				enriched["name"] = c.Name
				enriched["products"] = c.Products
				enriched["cases"] = c.Cases
				enriched["mcp"] = c.MCP
				enriched["has_page"] = c.Pages
				enriched["profile"] = p
				payload["company"] = enriched
			}
			if err := upsert("companies/"+c.UID+".json", "company", payload); err != nil {
				return count, err
			}
			count++
		}
		entry := map[string]any{
			"uid": c.UID, "name": c.Name, "products": len(c.Products),
			"cases": len(c.Cases), "mcp": len(c.MCP), "has_page": c.Pages,
		}
		// A sentence built from what we actually hold, so the directory reads as
		// prose rather than as a row of counts. Naming the products is the honest
		// description of a company we know only through its tools: we have not
		// researched these organisations and will not summarise them as though we
		// had.
		names := []string{}
		cats := map[string]bool{}
		for _, p := range c.Products {
			if p.Name != "" {
				names = append(names, p.Name)
			}
			if p.Category != "" {
				cats[p.Category] = true
			}
			if entry["url"] == nil && p.URL != "" {
				entry["url"] = p.URL
			}
		}
		shown := names
		if len(shown) > 4 {
			shown = shown[:4]
		}
		var b strings.Builder
		switch {
		case len(names) == 1:
			b.WriteString("Publishes " + names[0])
			if n := strings.TrimSpace(c.Products[0].Note); n != "" {
				b.WriteString(", " + strings.ToLower(n[:1]) + n[1:])
			}
			b.WriteString(".")
		case len(names) > 1:
			b.WriteString(fmt.Sprintf("Publishes %d tools we track, including %s",
				len(names), strings.Join(shown, ", ")))
			if len(cats) > 1 {
				b.WriteString(fmt.Sprintf(", across %d categories", len(cats)))
			}
			b.WriteString(".")
		}
		if len(c.Cases) > 0 {
			b.WriteString(fmt.Sprintf(" Named as a defendant in %d lawsuit%s we track.",
				len(c.Cases), map[bool]string{true: "", false: "s"}[len(c.Cases) == 1]))
		}
		if len(c.MCP) > 0 {
			b.WriteString(fmt.Sprintf(" Publishes %d server%s to the MCP registry.",
				len(c.MCP), map[bool]string{true: "", false: "s"}[len(c.MCP) == 1]))
		}
		entry["summary"] = b.String()
		if !c.Pages && len(c.Products) == 1 {
			entry["product"] = c.Products[0].Name
			entry["note"] = c.Products[0].Note
		}
		index = append(index, entry)
	}
	if err := upsert("companies/index.json", "company-hub", map[string]any{
		"uid": twoaiUID("section:ai-companies-directory"), "companies": index,
		"total": len(index), "profiled": count, "generated": today,
	}); err != nil {
		return count, err
	}
	return count + 1, nil
}

// twoaiPeople renders the AI people directory from site_people, the same 47
// profiles behind srjconsultingservices.com/ai-resources/ai-people/.
//
// These carry real substance: a hook, a quote, fields, a timeline, achievements,
// and sources, three to four kilobytes each. That is why this is a page factory
// rather than a directory listing. Profiles without sources are still rendered,
// but the page says which claims are unsourced rather than presenting everything
// with equal confidence.
func twoaiPeople(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	rows, err := db.Query(`SELECT slug, data::text FROM site_people ORDER BY slug`)
	if err != nil {
		return 0, err
	}
	type idx struct {
		UID      string   `json:"uid,omitempty"`
		Name     string   `json:"name"`
		Moniker  string   `json:"moniker,omitempty"`
		Hook     string   `json:"hook,omitempty"`
		Fields   []any    `json:"fields,omitempty"`
		Org      string   `json:"org,omitempty"`
		Title    string   `json:"title,omitempty"`
		Profiled bool     `json:"profiled"`
		Cats     []string `json:"cats,omitempty"`
	}
	var list []idx
	var docs []map[string]any
	uidBySlug := map[string]string{}
	for rows.Next() {
		var slug, raw string
		if rows.Scan(&slug, &raw) != nil {
			continue
		}
		var d map[string]any
		if json.Unmarshal([]byte(raw), &d) != nil {
			continue
		}
		name, _ := d["name"].(string)
		if name == "" {
			continue
		}
		uid := twoaiEntityID(db, "person", name)
		d["uid"] = uid
		d["generated"] = today
		uidBySlug[slug] = uid
		docs = append(docs, d)
		mon, _ := d["moniker"].(string)
		hook, _ := d["hook"].(string)
		fields, _ := d["fields"].([]any)
		list = append(list, idx{UID: uid, Name: name, Moniker: mon, Hook: hook, Fields: fields, Profiled: true})
	}
	rows.Close()

	// The roster is the people we TRACK; site_people is the subset we have
	// written up. srjconsultingservices.com publishes 65 while only 47 carry a
	// full profile, and a directory that silently showed 47 would be a quieter
	// but worse answer than one that says which 18 are tracked and unwritten.
	// Roster-only entries get a row with their organisation and title and no
	// page, because a name and a job title is not a profile.
	seen := map[string]bool{}
	for _, e := range list {
		seen[strings.ToLower(strings.TrimSpace(e.Name))] = true
	}
	// Exact-name matching is not enough to tell whether the roster is naming
	// somebody we have already profiled. The roster carries "Ravi Ummadisetti"
	// and site_people carries "Ravi Chandu Ummadisetti" — one person, two
	// pages, in the section whose entire point is that an entity has exactly
	// one identifier.
	//
	// The test is deliberately tight rather than fuzzy: the shorter name's
	// words must all appear in the longer one, AND first and last must match.
	// A dropped middle name matches; two different people who happen to share
	// a first and last name do not, because their other words differ. Loose
	// matching here would merge strangers, which is a worse failure than a
	// duplicate.
	nameWords := func(s string) []string {
		return strings.Fields(strings.ToLower(strings.TrimSpace(s)))
	}
	sameHuman := func(a, b string) bool {
		aw, bw := nameWords(a), nameWords(b)
		if len(aw) < 2 || len(bw) < 2 {
			return false
		}
		if aw[0] != bw[0] || aw[len(aw)-1] != bw[len(bw)-1] {
			return false
		}
		short, long := aw, bw
		if len(short) > len(long) {
			short, long = long, short
		}
		inLong := map[string]bool{}
		for _, w := range long {
			inLong[w] = true
		}
		for _, w := range short {
			if !inLong[w] {
				return false
			}
		}
		return true
	}
	profiledNames := make([]string, 0, len(list))
	for _, e := range list {
		profiledNames = append(profiledNames, e.Name)
	}
	tracked := 0
	var roster string
	if err := db.QueryRow(`SELECT data::text FROM site_content WHERE path='people/roster.json'`).Scan(&roster); err == nil {
		var r struct {
			People []struct {
				Name  string `json:"name"`
				Org   string `json:"org"`
				Title string `json:"title"`
			} `json:"people"`
			Source map[string]string `json:"source"`
		}
		if json.Unmarshal([]byte(roster), &r) == nil {
			for _, p := range r.People {
				n := strings.TrimSpace(p.Name)
				if n == "" || seen[strings.ToLower(n)] {
					continue
				}
				dup := false
				for _, pn := range profiledNames {
					if sameHuman(n, pn) {
						// Silent by design. This match is permanent and correct:
						// the roster and site_people spell the same person two
						// ways, the merge resolves it, and saying so on every run
						// forever reports a success as though it were a warning.
						// A name that stops matching shows up as a new stub in the
						// directory, which is the visible signal that matters.
						dup = true
						break
					}
				}
				if dup {
					continue
				}
				seen[strings.ToLower(n)] = true
				// A name with an organisation and a title is enough to publish:
				// three verified facts about a real person, sourced. It is not
				// enough to pretend it is a profile, so the page says what it is
				// and what it is missing rather than padding to look finished.
				uid := twoaiEntityID(db, "person", n)
				list = append(list, idx{UID: uid, Name: n, Org: p.Org, Title: p.Title})
				docs = append(docs, map[string]any{
					"uid": uid, "name": n, "org": p.Org, "title": p.Title,
					"tracked_only": true, "generated": today,
				})
				tracked++
			}
		}
	}
	if len(docs) == 0 {
		return 0, nil
	}

	// Category membership, from twoai_person_category. Seven categories are
	// taxonomy sections (Stephen's 2026-08 request) and publish as hub pages
	// below the category identifier path; the rest render as plain labels on a
	// profile until a section exists for them. A person can hold several.
	catTax := map[string]string{
		"researchers":                   "people-researchers",
		"founders-and-executives":       "people-founders",
		"investors":                     "people-investors",
		"open-source-maintainers":       "people-maintainers",
		"professors-and-academics":      "people-academics",
		"government-and-policy-leaders": "people-government",
		"authors-and-communicators":     "people-authors",
	}
	type catRef struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		UID  string `json:"uid,omitempty"`
	}
	catName := map[string]string{}
	catsByUID := map[string][]catRef{}
	if crows, err := db.Query(`SELECT pc.slug, pc.category, c.name
		FROM twoai_person_category pc
		JOIN twoai_people_categories c ON c.key = pc.category
		ORDER BY pc.slug, c.name`); err == nil {
		for crows.Next() {
			var slug, key, name string
			if crows.Scan(&slug, &key, &name) != nil {
				continue
			}
			catName[key] = name
			uid := uidBySlug[slug]
			if uid == "" {
				continue
			}
			r := catRef{Key: key, Name: name}
			if tax := catTax[key]; tax != "" {
				r.UID = twoaiUID("section:" + tax)
			}
			catsByUID[uid] = append(catsByUID[uid], r)
		}
		crows.Close()
	}
	for _, d := range docs {
		if uid, _ := d["uid"].(string); uid != "" && len(catsByUID[uid]) > 0 {
			d["cats"] = catsByUID[uid]
		}
	}
	for i := range list {
		if len(catsByUID[list[i].UID]) > 0 {
			keys := make([]string, 0, len(catsByUID[list[i].UID]))
			for _, r := range catsByUID[list[i].UID] {
				keys = append(keys, r.Key)
			}
			list[i].Cats = keys
		}
	}

	count := 0
	keep := make([]string, 0, len(docs))
	for _, d := range docs {
		uid, _ := d["uid"].(string)
		if err := upsert("people/"+uid+".json", "person", d); err != nil {
			return count, err
		}
		keep = append(keep, "people/"+uid+".json")
		count++
	}

	// Reap person rows that this run did not write.
	//
	// The uid is derived from the name, so changing how a name is normalized
	// changes the uid, and upsert-by-path then writes the profile to a NEW row
	// while the old one sits there forever still rendering a page. That is not
	// hypothetical: Tim O'Reilly published under two uids for a week, because an
	// earlier rule dropped the apostrophe (tim-oreilly) where the entity
	// normalizer turns it into a hyphen (tim-o-reilly). Two URLs, identical
	// content, competing with each other, in the one section whose entire
	// purpose is that an entity has exactly one identifier.
	//
	// Deleting what this run did not write is the only version of this that
	// stays correct when the rule changes again. It is guarded by len(docs) > 0
	// above, so a run that read nothing deletes nothing.
	if _, err := db.Exec(`DELETE FROM twoai_pages
		WHERE kind = 'person' AND NOT (path = ANY($1))`, pq.Array(keep)); err != nil {
		return count, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	if err := upsert("people/index.json", "person-hub", map[string]any{
		"uid": twoaiUID("section:ai-people-directory"), "people": list,
		"total": len(list), "profiled": count - tracked, "tracked_only": tracked,
		"generated": today,
	}); err != nil {
		return count, err
	}
	count++

	// The seven category sections publish as their own hub pages, one JSON per
	// taxonomy slug, taxonomy_slug set to the section itself so the ecosystem
	// map counts them. Members come only from profiled people: roster-only
	// entries carry no category assignment, and a category page listing names
	// that link nowhere would be padding. Written directly rather than through
	// upsert because upsert derives taxonomy_slug from kind, and here it
	// varies per page.
	keepCats := make([]string, 0, len(catTax))
	for key, tax := range catTax {
		var name, blurb string
		if db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug=$1`,
			tax).Scan(&name, &blurb) != nil {
			continue
		}
		if name == "" {
			name = catName[key]
		}
		type member struct {
			UID     string `json:"uid"`
			Name    string `json:"name"`
			Moniker string `json:"moniker,omitempty"`
			Hook    string `json:"hook,omitempty"`
		}
		members := []member{}
		for _, e := range list {
			for _, k := range e.Cats {
				if k == key {
					members = append(members, member{UID: e.UID, Name: e.Name, Moniker: e.Moniker, Hook: e.Hook})
					break
				}
			}
		}
		doc := map[string]any{
			"uid": twoaiUID("section:" + tax), "key": key, "tax": tax,
			"name": name, "blurb": blurb, "members": members,
			"total": len(members), "generated": today,
		}
		j, _ := json.Marshal(doc)
		path := "people/cat-" + tax + ".json"
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'person-category',$2::jsonb,$3,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			path, string(j), tax); err != nil {
			return count, err
		}
		keepCats = append(keepCats, path)
		count++
	}
	if len(keepCats) > 0 {
		if _, err := db.Exec(`DELETE FROM twoai_pages
			WHERE kind = 'person-category' AND NOT (path = ANY($1))`, pq.Array(keepCats)); err != nil {
			return count, err
		}
	}
	return count, nil
}

// twoaiMCP renders the Model Context Protocol server tracker from the mirror
// of the official registry.
//
// Every field on these pages comes from what the server's own publisher put in
// the registry. We do not describe what a server does beyond its own
// description, do not rate them, and do not claim any of them work: nobody has
// installed 300 servers to check. The page says what was published, by whom,
// when, and how to reach it, which is the useful and defensible thing.
//
// A server needs a description to get its own page. An entry that is a bare
// name and a version number is a directory row, not a page worth indexing.
func twoaiMCP(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	rows, err := db.Query(`SELECT name, slug, COALESCE(title,''), COALESCE(description,''),
			COALESCE(version,''), COALESCE(repository::text,''), COALESCE(website_url,''),
			COALESCE(remotes::text,''), COALESCE(packages::text,''),
			COALESCE(to_char(published_at,'YYYY-MM-DD'),''),
			COALESCE(to_char(registry_updated_at,'YYYY-MM-DD'),'')
		FROM twoai_mcp_servers WHERE status='active' ORDER BY name`)
	if err != nil {
		return 0, err
	}
	type srv struct {
		Name      string `json:"name"`
		Slug      string `json:"slug"`
		Title     string `json:"title,omitempty"`
		Desc      string `json:"description,omitempty"`
		Version   string `json:"version,omitempty"`
		Website   string `json:"website,omitempty"`
		RepoURL   string `json:"repo_url,omitempty"`
		Transport string `json:"transport,omitempty"`
		Vendor    string `json:"vendor,omitempty"`
		Published string `json:"published,omitempty"`
		Updated   string `json:"updated,omitempty"`
		// Connection detail, all from the registry record.
		RemoteType  string `json:"remote_type,omitempty"`
		RemoteURL   string `json:"remote_url,omitempty"`
		PkgID       string `json:"package_id,omitempty"`
		PkgRegistry string `json:"package_registry,omitempty"`
		PkgVersion  string `json:"package_version,omitempty"`
		// Publisher context.
		Namespace   string   `json:"namespace,omitempty"`
		CompanyUID  string   `json:"company_uid,omitempty"`
		Siblings    []string `json:"siblings,omitempty"`
		SiblingSlug []string `json:"sibling_slugs,omitempty"`
		// Whether this page carries enough to stand on its own in an index.
		Indexable bool `json:"indexable"`
	}
	var all []srv
	for rows.Next() {
		var s srv
		var repoRaw, remotesRaw, packagesRaw string
		if rows.Scan(&s.Name, &s.Slug, &s.Title, &s.Desc, &s.Version, &repoRaw,
			&s.Website, &remotesRaw, &packagesRaw, &s.Published, &s.Updated) != nil {
			continue
		}
		if repoRaw != "" {
			var r struct {
				URL string `json:"url"`
			}
			if json.Unmarshal([]byte(repoRaw), &r) == nil {
				s.RepoURL = r.URL
			}
		}
		// How you actually reach it, and with WHAT. The registry carries the
		// transport type, the package registry and the identifier to install;
		// a page that omits them is a name and a sentence, which is why these
		// pages were noindexed in the first place. Everything below is read
		// from the registry record, never inferred.
		switch {
		case remotesRaw != "" && remotesRaw != "null":
			s.Transport = "remote"
			var rem []struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			}
			if json.Unmarshal([]byte(remotesRaw), &rem) == nil && len(rem) > 0 {
				s.RemoteType = rem[0].Type
				s.RemoteURL = rem[0].URL
			}
		case packagesRaw != "" && packagesRaw != "null":
			s.Transport = "local package"
			var pkg []struct {
				Identifier   string `json:"identifier"`
				RegistryType string `json:"registryType"`
				Version      string `json:"version"`
				Transport    struct {
					Type string `json:"type"`
				} `json:"transport"`
			}
			if json.Unmarshal([]byte(packagesRaw), &pkg) == nil && len(pkg) > 0 {
				s.PkgID = pkg[0].Identifier
				s.PkgRegistry = pkg[0].RegistryType
				s.PkgVersion = pkg[0].Version
				s.RemoteType = pkg[0].Transport.Type
			}
		}
		// The registry namespaces names by reverse domain, so the leading
		// segments identify the publisher.
		if i := strings.Index(s.Name, "/"); i > 0 {
			parts := strings.Split(s.Name[:i], ".")
			for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
				parts[l], parts[r] = parts[r], parts[l]
			}
			s.Vendor = strings.Join(parts, ".")
			s.Namespace = s.Name[:i]
		}
		all = append(all, s)
	}
	rows.Close()
	if len(all) == 0 {
		return 0, nil
	}

	// Servers published under the same reverse-DNS namespace are the same
	// publisher, which is a real relationship the registry states rather than
	// one we infer. Linking them turns 1,900 orphans into families a reader can
	// navigate, and gives each page somewhere to go.
	byNS := map[string][]int{}
	for i, s := range all {
		if s.Namespace != "" {
			byNS[s.Namespace] = append(byNS[s.Namespace], i)
		}
	}
	// Company profiles, matched on EXACT normalised equality of the namespace's
	// last label, the same rule as src/lib/mcpAttribution.ts. Never a substring:
	// that is how 101 false attributions reached production on 2026-08-18.
	companyByName := map[string]string{}
	if cr, err := db.Query(`SELECT uid, name FROM twoai_entities WHERE kind='company'`); err == nil {
		for cr.Next() {
			var uid, name string
			if cr.Scan(&uid, &name) == nil {
				companyByName[nonAlnumLower(name)] = uid
			}
		}
		cr.Close()
	}

	for i := range all {
		s := &all[i]
		if s.Namespace != "" {
			label := s.Namespace
			if d := strings.LastIndex(label, "."); d >= 0 {
				label = label[d+1:]
			}
			s.CompanyUID = companyByName[nonAlnumLower(label)]
		}
		for _, j := range byNS[s.Namespace] {
			if j == i || len(s.Siblings) >= 8 {
				continue
			}
			t := all[j].Title
			if t == "" {
				t = all[j].Name
			}
			s.Siblings = append(s.Siblings, t)
			s.SiblingSlug = append(s.SiblingSlug, all[j].Slug)
		}
		// INDEXABLE means the page stands on its own: a description, a way to
		// actually connect to it, and a source to verify against. Pages that
		// fail this stay noindex, because the honest fix for a thin page is to
		// keep it out of the index, not to pad it.
		s.Indexable = s.Desc != "" &&
			(s.RemoteURL != "" || s.PkgID != "") &&
			(s.RepoURL != "" || s.Website != "")
	}

	count := 0
	for _, s := range all {
		if s.Desc == "" {
			continue
		}
		if err := upsert("mcp/"+s.Slug+".json", "mcp-server", map[string]any{
			"server": s, "generated": today,
		}); err != nil {
			return count, err
		}
		count++
	}
	remote, local := 0, 0
	for _, s := range all {
		switch s.Transport {
		case "remote":
			remote++
		case "local package":
			local++
		}
	}
	if err := upsert("mcp/index.json", "mcp-hub", map[string]any{
		"servers": all, "total": len(all), "profiled": count,
		"remote": remote, "local": local, "generated": today,
	}); err != nil {
		return count, err
	}
	return count + 1, nil
}

// mcpRegistry mirrors the official Model Context Protocol registry.
//
// SOURCE CHOICE. The obvious approach, searching GitHub for topic:mcp-server,
// is wrong: a live check returned 2,458 repositories led by n8n, gemini-cli,
// and a web-scraping framework, none of which is an MCP server. They tag the
// topic because they support MCP. The official registry at
// registry.modelcontextprotocol.io is the authoritative list of servers that
// have actually published themselves, which is a fact about the world rather
// than a guess from a keyword.
//
// Only entries the registry marks isLatest AND active are stored. The API
// returns every historical version of every server, so without that filter a
// server appears once per release and the count is nonsense.
func mcpRegistry(db *sql.DB) error {
	slugRe := regexp.MustCompile(`[^a-z0-9]+`)
	client := &http.Client{Timeout: 45 * time.Second}
	cursor := ""
	stored, pages := 0, 0

	for pages < 40 {
		u := "https://registry.modelcontextprotocol.io/v0/servers?limit=100"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "srj-pipeline/1.0 (theworldofai.org)")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			resp.Body.Close()
			return fmt.Errorf("mcp registry %d: %s", resp.StatusCode, b)
		}
		var payload struct {
			Servers []struct {
				Server struct {
					Name        string          `json:"name"`
					Title       string          `json:"title"`
					Description string          `json:"description"`
					Version     string          `json:"version"`
					WebsiteURL  string          `json:"websiteUrl"`
					Repository  json.RawMessage `json:"repository"`
					Remotes     json.RawMessage `json:"remotes"`
					Packages    json.RawMessage `json:"packages"`
				} `json:"server"`
				Meta struct {
					Official struct {
						Status      string `json:"status"`
						IsLatest    bool   `json:"isLatest"`
						PublishedAt string `json:"publishedAt"`
						UpdatedAt   string `json:"updatedAt"`
					} `json:"io.modelcontextprotocol.registry/official"`
				} `json:"_meta"`
			} `json:"servers"`
			Metadata struct {
				NextCursor string `json:"nextCursor"`
			} `json:"metadata"`
		}
		dec := json.NewDecoder(resp.Body)
		if err := dec.Decode(&payload); err != nil {
			resp.Body.Close()
			return err
		}
		resp.Body.Close()
		pages++

		for _, row := range payload.Servers {
			o := row.Meta.Official
			s := row.Server
			if !o.IsLatest || o.Status != "active" || s.Name == "" {
				continue
			}
			slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s.Name), "-"), "-")
			if slug == "" {
				continue
			}
			js := func(r json.RawMessage) any {
				if len(r) == 0 {
					return nil
				}
				return string(r)
			}
			var pub, upd any
			if o.PublishedAt != "" {
				pub = o.PublishedAt
			}
			if o.UpdatedAt != "" {
				upd = o.UpdatedAt
			}
			if _, err := db.Exec(`INSERT INTO twoai_mcp_servers
				(name, slug, title, description, version, repository, website_url,
				 remotes, packages, status, published_at, registry_updated_at, fetched_at)
				VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8::jsonb,$9::jsonb,$10,$11,$12,now())
				ON CONFLICT (name) DO UPDATE SET slug=EXCLUDED.slug, title=EXCLUDED.title,
					description=EXCLUDED.description, version=EXCLUDED.version,
					repository=EXCLUDED.repository, website_url=EXCLUDED.website_url,
					remotes=EXCLUDED.remotes, packages=EXCLUDED.packages,
					status=EXCLUDED.status, published_at=EXCLUDED.published_at,
					registry_updated_at=EXCLUDED.registry_updated_at, fetched_at=now()`,
				s.Name, slug, s.Title, s.Description, s.Version, js(s.Repository),
				s.WebsiteURL, js(s.Remotes), js(s.Packages), o.Status, pub, upd); err != nil {
				return err
			}
			stored++
		}
		cursor = payload.Metadata.NextCursor
		if cursor == "" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	fmt.Printf("mcp_registry: pages=%d active_servers=%d ok=true\n", pages, stored)
	return nil
}

// twoaiTaxonomyFor places a page in the AI Ecosystem tree at write time.
//
// Assigning this by hand in SQL after each new factory shipped did not hold:
// 74 pages, including all 61 compliance pages, ended up outside the tree, so
// their category reported fewer pages than it had and the coverage numbers on
// the site understated it. A page now gets its node from the same call that
// writes it, which is the only version of this that cannot drift.
//
// A kind with no entry returns empty, which stores NULL: chrome like the
// ecosystem hub and the legal pages genuinely sit outside the tree rather than
// belonging to a category, and pretending otherwise would inflate the counts.
func twoaiTaxonomyFor(kind string) any {
	switch kind {
	case "state-law", "hub":
		return "state-ai-laws"
	case "compliance", "compliance-hub":
		return "governance-frameworks"
	case "lawsuits":
		return "ai-lawsuit-tracker"
	case "glossary":
		return "ai-glossary"
	case "tool", "tool-category", "tool-hub":
		return "ai-tools-catalog"
	case "mcp-server", "mcp-hub":
		return "mcp-servers"
	case "person", "person-hub":
		return "ai-people-directory"
	case "company", "company-hub":
		return "ai-companies-directory"
	case "research-topic", "research-hub", "research-paper":
		return "research-library"
	case "sources-hub":
		return "research-library"
	case "week", "week-hub":
		return "this-week-in-ai"
	case "vendor-news":
		return "vendor-news"
	case "research-watch":
		return "research-library"
	case "jobs-hub":
		return "jobs-listings"
	case "skills-hub", "onet-occupation":
		// skills-graph, checked against twoai_taxonomy rather than guessed.
		// The last time a slug here was written from memory the foreign key
		// took the build down mid-run.
		return "skills-graph"
	case "news-archive":
		// The node is headline-news, not daily-news. A duplicate daily-news
		// node was removed when AI News was restructured, and guessing the
		// slug here cost a build: twoai_pages has a foreign key on
		// taxonomy_slug, so an unknown slug aborts twoaiBuild partway
		// through rather than degrading quietly.
		return "headline-news"
	}
	return nil
}

// twoaiToolData reads the AI tool catalog and the deep tool profiles out of
// site_content. Both are maintained for the SRJ site and are read only here.
// Any absence is tolerated: a missing row means the tools factory emits
// nothing rather than half a directory.
func twoaiToolData(db *sql.DB) (tools []map[string]any, cats []map[string]any, profiles map[string]any) {
	profiles = map[string]any{}
	var catalog string
	if err := db.QueryRow(`SELECT data::text FROM site_content WHERE path='resources/tools.json'`).Scan(&catalog); err == nil {
		var c struct {
			Tools      []map[string]any `json:"tools"`
			Categories []map[string]any `json:"categories"`
		}
		if json.Unmarshal([]byte(catalog), &c) == nil {
			tools, cats = c.Tools, c.Categories
		}
	}
	var prof string
	if err := db.QueryRow(`SELECT data::text FROM site_content WHERE path='resources/tool-profiles.json'`).Scan(&prof); err == nil {
		var p struct {
			Profiles map[string]any `json:"profiles"`
			AsOf     string         `json:"as_of"`
		}
		if json.Unmarshal([]byte(prof), &p) == nil && p.Profiles != nil {
			profiles = p.Profiles
		}
	}
	return tools, cats, profiles
}

// twoaiThemes classifies a bill or docket title into the recurring subjects of
// AI legislation. The point is not taxonomy for its own sake: a reader looking
// at forty bill rows cannot see a pattern, and the pattern is the story. What
// the categories are, and the keywords that mark them, are the same ones the
// bills use about themselves, so a bill lands in a bucket because of its own
// words rather than an editorial judgement about it.
//
// A bill can match more than one theme, and does not have to match any. An
// unmatched bill still appears in the table; it just does not contribute to
// the thematic summary, which is better than forcing it into a bucket that
// would misdescribe it.
var twoaiThemeRules = []struct {
	Name, Blurb string
	Re          *regexp.Regexp
}{
	{"Deepfakes and likeness", "Synthetic images, voices, and video of real people, and who owns a likeness once a machine can copy it.",
		regexp.MustCompile(`(?i)deepfake|synthetic media|digital replica|likeness|voice clon|impersonat|nonconsensual|sexually explicit`)},
	{"Elections", "AI-generated political content, disclosure on campaign material, and interference with voting.",
		regexp.MustCompile(`(?i)election|campaign|ballot|candidate|political advertis`)},
	{"Children and minors", "Companion chatbots, age verification, school use, and protections for people under eighteen.",
		regexp.MustCompile(`(?i)\bminor|child|kids|student|school|age verification|companion chatbot`)},
	{"Health care", "Clinical decision support, utilization review, mental health chatbots, and AI in diagnosis or coverage decisions.",
		regexp.MustCompile(`(?i)health|medical|clinical|patient|mental health|insur(er|ance) (review|decision)|prior authorization|therapist`)},
	{"Employment and hiring", "Automated screening of applicants, workplace surveillance, and decisions about pay or promotion.",
		regexp.MustCompile(`(?i)employ|hiring|applicant|worker|workplace|labor|resume|personnel decision`)},
	{"Government use", "How agencies themselves buy, deploy, and account for AI, including inventories and procurement rules.",
		regexp.MustCompile(`(?i)state agenc|government use|procurement|public sector|inventory of|task force|advisory (council|committee)|study committee`)},
	{"Transparency and disclosure", "Labeling AI-generated output, telling people when they are talking to a machine, and impact assessments.",
		regexp.MustCompile(`(?i)disclos|transparen|label|watermark|notice to consumers|impact assessment|audit`)},
	{"Consumer protection and discrimination", "Algorithmic decisions that affect credit, housing, insurance pricing, or that produce unlawful bias.",
		regexp.MustCompile(`(?i)discriminat|algorithmic (pricing|bias)|consumer protection|credit|housing|unfair|deceptive`)},
	{"Privacy and data", "Biometrics, training data, and what may be collected or fed into a model.",
		regexp.MustCompile(`(?i)privacy|biometric|personal (data|information)|training data|facial recognition|surveillance`)},
	{"Criminal law", "New offenses, penalties, and evidence rules for conduct carried out with AI.",
		regexp.MustCompile(`(?i)criminal|penalt|offense|felony|misdemeanor|fraud|prosecut`)},
	{"Safety and frontier models", "Obligations aimed at the most capable systems, including testing, incident reporting, and catastrophic risk.",
		regexp.MustCompile(`(?i)frontier|catastrophic|safety (standard|protocol)|critical infrastructure|foundation model|general.purpose`)},
	{"Infrastructure and energy", "Data centers, the power they draw, and the local cost of hosting them.",
		regexp.MustCompile(`(?i)data cent(er|re)|energy|electric|grid|water use|utility`)},
	{"Workforce and education", "Training people to use AI, apprenticeships, curriculum, and public literacy programs.",
		regexp.MustCompile(`(?i)workforce|apprentice|curriculum|literacy|training program|community college|scholarship`)},
	{"Intellectual property", "Copyright, authorship, and the use of protected work to build models.",
		regexp.MustCompile(`(?i)copyright|intellectual property|authorship|royalt|licens(e|ing) of works`)},
}

func twoaiClassify(text string) []string {
	var out []string
	for _, r := range twoaiThemeRules {
		if r.Re.MatchString(text) {
			out = append(out, r.Name)
		}
	}
	return out
}

// twoaiVendorNews builds news/vendor.json: the AI Vendor News page at
// /ai-news/vendor/, which is what the organisations building and governing AI
// said themselves.
//
// WHY THE SOURCE LIST IS EXPLICIT. ai_intel_candidates mixes two very
// different things under one kind: posts from a vendor's own feed, where the
// vendor column is the organisation ('OpenAI', 'Google DeepMind'), and press
// articles discovered by RSS, where the vendor column is whatever domain
// published them ('miragenews.com'). Only the first belongs here. A domain
// test alone would let any aggregator that happens to lack a dot in its name
// through, so the allowed sources are named. Adding a vendor is a deliberate
// edit to this list, which is the right friction for a page whose whole claim
// is that these are primary sources.
//
// Press coverage of the field is the Daily News briefing's job. The two pages
// answer different questions and must not blur: what is being reported, versus
// what the builders announced.

// twoaiPostSlug builds the permanent slug for a vendor announcement page:
// the sanitized title plus the first 6 hex chars of the URL's md5, matching
// the SQL expression that emitted the launch snapshot so the same post never
// gets two different URLs. Slugs are permanent once published, per the
// standing URL rule, which is why the disambiguator hashes the URL (stable)
// rather than anything that could be re-edited.
func twoaiPostSlug(name, url string) string {
	s := strings.ToLower(name)
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = s[:60]
	}
	return s + "-" + fmt.Sprintf("%x", md5.Sum([]byte(url)))[:6]
}

// twoaiVendorNews builds news/vendor.json for /ai-news/vendor/. Items carry
// the date the intel watch surfaced them, not the date the vendor published
// it, because that is what the table stores. The page says so. A newly added
// feed backfills its archive on first sweep, so its items all carry that
// sweep's date and would otherwise look like a burst of same-day
// announcements that never happened. Each item also carries the vendor's own
// published description (summary, when the row has one) and a permanent
// slug, because every post gets its own on-site page telling the reader why
// the outbound link is worth clicking before they leave the site.
func twoaiVendorNews(db *sql.DB, upsert func(path, kind string, v any) error) (int, error) {
	// Display names for feeds whose vendor column holds a hostname. Anything
	// not listed renders as stored.
	display := map[string]string{
		"stability.ai": "Stability AI",
		"aisi.gov.uk":  "UK AI Security Institute",
	}
	allowed := []string{
		"OpenAI", "Google DeepMind", "Hugging Face", "Mistral AI",
		"European Commission AI", "CIFAR", "AI Singapore",
		"stability.ai", "aisi.gov.uk",
	}
	const windowDays = 30
	const perVendor = 25

	// Every post page that has ever been published stays published: the URL
	// permanence rule applies to a permalink whether it fell off the page
	// because its source row aged out or because a newer post pushed it past
	// perVendor. The second is what actually bit us. On 2026-08-11 the sitemap
	// carried 48 /ai-news/vendor/ permalinks that were gone the next day, not
	// because the candidates expired (all 395 were still inside the window)
	// but because busier vendors pushed them out of their own top 25, and the
	// route builds its paths from the same capped list the hub renders.
	//
	// So the cap now governs display only. Posts are written through to
	// twoai_vendor_posts on first sight and every row is emitted in `archive`,
	// which is what the route builds from. first_published is set once and
	// never rewritten, so a permalink's identity does not drift.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_vendor_posts (
		slug text PRIMARY KEY,
		vendor text NOT NULL,
		title text NOT NULL,
		url text NOT NULL,
		summary text NOT NULL DEFAULT '',
		posted_on date,
		first_published timestamptz NOT NULL DEFAULT now(),
		last_seen timestamptz NOT NULL DEFAULT now())`); err != nil {
		return 0, err
	}

	rows, err := db.Query(`SELECT DISTINCT ON (url) vendor, name, url,
			to_char(discovered_at at time zone 'UTC','YYYY-MM-DD'),
			CASE WHEN length(trim(coalesce(summary,''))) >= 40 THEN summary ELSE '' END
		FROM ai_intel_candidates
		WHERE url IS NOT NULL AND url <> '' AND url NOT LIKE '%news.google%'
		  AND discovered_at > now() - ($1 || ' days')::interval
		  AND vendor = ANY($2)
		ORDER BY url, discovered_at DESC`, windowDays, pq.Array(allowed))
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type item struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Date    string `json:"date"`
		Summary string `json:"summary,omitempty"`
		Slug    string `json:"slug"`
	}
	byVendor := map[string][]item{}
	for rows.Next() {
		var vendor, name, url, date, summary string
		if err := rows.Scan(&vendor, &name, &url, &date, &summary); err != nil {
			return 0, err
		}
		if d, ok := display[vendor]; ok {
			vendor = d
		}
		byVendor[vendor] = append(byVendor[vendor], item{
			Title: name, URL: url, Date: date, Summary: summary,
			Slug: twoaiPostSlug(name, url),
		})
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// Write through before rendering. A post seen once is kept even when the
	// candidate row behind it is later pruned, which is the other way a
	// permalink can go missing.
	for vendor, items := range byVendor {
		for _, it := range items {
			if it.Slug == "" {
				continue
			}
			if _, err := db.Exec(`INSERT INTO twoai_vendor_posts
				(slug, vendor, title, url, summary, posted_on)
				VALUES ($1,$2,$3,$4,$5,$6::date)
				ON CONFLICT (slug) DO UPDATE SET
					vendor=EXCLUDED.vendor, title=EXCLUDED.title, url=EXCLUDED.url,
					summary=CASE WHEN EXCLUDED.summary <> '' THEN EXCLUDED.summary
					             ELSE twoai_vendor_posts.summary END,
					last_seen=now()`,
				it.Slug, vendor, it.Title, it.URL, it.Summary, it.Date); err != nil {
				fmt.Fprintln(os.Stderr, "twoai_build: vendor post upsert:", err)
			}
		}
	}

	type vendorBlock struct {
		Vendor string `json:"vendor"`
		Total  int    `json:"total"`
		Items  []item `json:"items"`
	}
	// Initialised, not nil: a nil slice marshals to JSON null, and the Astro
	// template maps over these arrays at prerender time. Empty must mean empty.
	blocks := []vendorBlock{}
	for vendor, items := range byVendor {
		sort.Slice(items, func(i, j int) bool { return items[i].Date > items[j].Date })
		total := len(items)
		if len(items) > perVendor {
			items = items[:perVendor]
		}
		blocks = append(blocks, vendorBlock{Vendor: vendor, Total: total, Items: items})
	}
	sort.Slice(blocks, func(i, j int) bool {
		if blocks[i].Total != blocks[j].Total {
			return blocks[i].Total > blocks[j].Total
		}
		return blocks[i].Vendor < blocks[j].Vendor
	})

	// A run that finds nothing in the intel path leaves the vendor blocks
	// alone, but the page no longer depends on them: `latest` below is built
	// from twoai_vendor_posts, which the feed stage fills directly.
	if len(blocks) == 0 {
		fmt.Fprintln(os.Stderr, "twoai_build: vendor news intel sweep found no items, using the archive alone")
	}

	// The archive is every post page that exists, capped by nothing. It drives
	// the permalink route, and `latest` below is the date-ordered slice of it
	// the hub renders.
	//
	// entity_uid is the cross-reference: /companies/ and /ai-news/vendor/ were
	// two lists of the same organisations with no link between them, so a
	// reader landing on an OpenAI announcement had no way to reach the OpenAI
	// profile and vice versa. Posts carry the uid of the company or tool that
	// published them, and the template turns it into a link.
	// A permalink is only worth minting when there is something on the page.
	// Turning on the 27 vendor feeds took the archive from 397 posts to 4,681
	// overnight, and 2,546 of those carry no summary at all: a headline, a date,
	// and a link out. Four and a half thousand pages of that is the doorway-page
	// pattern, it is what the blueprint means by "none thin", and at that scale
	// it puts the whole domain's standing at risk to host announcements the
	// vendor already publishes better.
	//
	// So a post gets its own page when it has a real summary to put on it.
	// Everything else is listed on the hub linking straight to the vendor's
	// original, which is where the site's own source rule points anyway.
	//
	// `source='intel'` is carried regardless: those 64 were published with
	// permalinks before the feeds were switched on, and a published URL does
	// not get withdrawn because a later rule would not have minted it.
	const summaryFloor = 120
	arows, err := db.Query(`SELECT p.slug, p.vendor, p.title, p.url, p.summary,
			COALESCE(to_char(p.posted_on,'YYYY-MM-DD'),''),
			COALESCE(p.entity_uid, f.entity_uid, ''),
			COALESCE(p.entity_kind, f.entity_kind, ''),
			(length(p.summary) >= $1 OR p.source = 'intel') AS has_page,
			COALESCE(p.reader_note, '')
		FROM twoai_vendor_posts p
		LEFT JOIN twoai_vendor_feeds f ON lower(f.vendor) = lower(p.vendor)
		ORDER BY p.posted_on DESC NULLS LAST, p.slug`, summaryFloor)
	if err != nil {
		return 0, err
	}
	type archived struct {
		item
		Vendor     string `json:"vendor"`
		EntityUID  string `json:"entity_uid,omitempty"`
		EntityKind string `json:"entity_kind,omitempty"`
		HasPage    bool   `json:"has_page"`
		// This site's own reading of what a post means for its readers. Kept
		// apart from Summary because Summary is the vendor's own words and
		// this is not. Empty on most posts, and the template renders nothing
		// rather than inventing one.
		ReaderNote string `json:"reader_note,omitempty"`
	}
	archiveOut := []archived{}
	pageCount := 0
	for arows.Next() {
		var a archived
		if err := arows.Scan(&a.Slug, &a.Vendor, &a.Title, &a.URL, &a.Summary, &a.Date,
			&a.EntityUID, &a.EntityKind, &a.HasPage, &a.ReaderNote); err != nil {
			arows.Close()
			return 0, err
		}
		if a.HasPage {
			pageCount++
		}
		archiveOut = append(archiveOut, a)
	}
	arows.Close()

	// The hub used to group by vendor and cap each at 25, which meant it read
	// as a directory of sources rather than a news page: a post from this
	// morning sat below a month-old one because its vendor sorted higher.
	// `latest` is the same posts in the order a reader wants them.
	const latestCap = 250
	latest := archiveOut
	if len(latest) > latestCap {
		latest = latest[:latestCap]
	}

	// Coverage is stated rather than implied. 27 of the 62 companies publish a
	// discoverable first-party feed; the rest are not covered, and a reader
	// should be able to see that instead of assuming silence means no news.
	var feedCount, feedOK int
	db.QueryRow(`SELECT count(*), count(*) FILTER (WHERE last_ok IS NOT NULL)
		FROM twoai_vendor_feeds WHERE active`).Scan(&feedCount, &feedOK)
	var companyCount int
	db.QueryRow(`SELECT count(*) FROM twoai_pages WHERE kind='company'`).Scan(&companyCount)

	now := time.Now().UTC()
	if err := upsert("news/vendor.json", "vendor-news", map[string]any{
		"generated":   now.Format(time.RFC3339),
		"date":        now.Format("2006-01-02"),
		"window_days": windowDays,
		"vendors":     blocks,
		"latest":      latest,
		"archive":     archiveOut,
		"coverage": map[string]int{
			"feeds": feedCount, "feeds_healthy": feedOK, "companies": companyCount,
		},
	}); err != nil {
		return 0, err
	}
	fmt.Printf("twoai_build: vendor news vendors=%d latest=%d archive=%d pages=%d feeds=%d/%d ok=true\n",
		len(blocks), len(latest), len(archiveOut), pageCount, feedOK, feedCount)
	return len(blocks), nil
}

// twoaiResearchWatch builds research/watch.json: the arXiv Watch page at
// /research/watch/, rendering the preprints arxiv_watch has filed in
// ai_intel_candidates (source='arxiv').
//
// The template labels each paper's vendor value as "mentions {name}", never
// as authorship: arXiv's API exposes no affiliations, so arxiv_watch matches
// names in the title and abstract, and a paper matched on a model family is
// usually a paper studying that model. This stage only renders what the watch
// stored; the honesty constraint lives in the wording, which the template
// owns. Rows marked ignored in review are excluded, which is the operator's
// lever for pruning noise without deleting the record.
//
// Same defensive posture as twoaiVendorNews: an empty sweep leaves the last
// good page in place, since arXiv API outages are routine (Aug 1) and a page
// that empties itself on a transient failure is worse than one a day stale.
func twoaiResearchWatch(db *sql.DB, upsert func(path, kind string, v any) error) (int, error) {
	const windowDays = 30
	rows, err := db.Query(`SELECT name, vendor,
			replace(url, 'http://arxiv.org', 'https://arxiv.org'),
			to_char(discovered_at at time zone 'UTC','YYYY-MM-DD')
		FROM ai_intel_candidates
		WHERE source = 'arxiv' AND status <> 'ignored'
		  AND discovered_at > now() - ($1 || ' days')::interval
		ORDER BY discovered_at DESC, id DESC`, windowDays)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type paper struct {
		Title    string `json:"title"`
		Match    string `json:"match"`
		URL      string `json:"url"`
		Surfaced string `json:"surfaced"`
	}
	// Initialised, not nil: nil marshals to JSON null and the template maps
	// over this array at prerender time.
	papers := []paper{}
	for rows.Next() {
		var p paper
		if err := rows.Scan(&p.Title, &p.Match, &p.URL, &p.Surfaced); err != nil {
			return 0, err
		}
		papers = append(papers, p)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(papers) == 0 {
		fmt.Fprintln(os.Stderr, "twoai_build: arxiv watch found no papers, keeping the existing page")
		return 0, nil
	}

	now := time.Now().UTC()
	if err := upsert("research/watch.json", "research-watch", map[string]any{
		"generated":   now.Format(time.RFC3339),
		"date":        now.Format("2006-01-02"),
		"window_days": windowDays,
		"papers":      papers,
	}); err != nil {
		return 0, err
	}
	return len(papers), nil
}

// benchResults auto-refreshes the benchmark result snapshots in
// twoai_benchmarks.results from STRUCTURED sources the projects publish
// themselves, then mirrors any changed results into the twoai_pages rows the
// site renders. It never scrapes rendered HTML: every adapter reads a JSON or
// CSV artifact (or an authenticated API) with a stable shape, validates what
// it parsed, and on any failure keeps the last good snapshot and warns, so
// the failure mode is a visibly dated page rather than a silently wrong one.
// The staleness tripwire in twoai_build is the backstop: an adapter that
// fails for 90 days surfaces in the cron log.
//
// Adapter status:
//   swe-bench  LIVE.    Official leaderboards.json from the swe-bench site
//                       repo (the data behind swebench.com), Verified board.
//   hle, gpqa  GATED.   Artificial Analysis API; activates when AA_API_KEY
//                       is set on the cron service (free key from
//                       artificialanalysis.ai). Until then the curated
//                       snapshot stands. The parse is defensive because the
//                       response shape could not be verified without a key;
//                       first keyed run confirms via the log.
//   lmarena    GATED.   The public HF mirror (lmarena-ai/arena-leaderboard)
//                       froze at leaderboard_table_20250804.csv, so a
//                       30-day recency gate rejects it; if the mirror
//                       revives, the stage logs that fresh data exists and
//                       still leaves the curated snapshot for a manual look
//                       first, because the CSV schema of a revived mirror is
//                       unverified by definition.
// Everything else (saturation statements, configuration-shaped notes) is
// stable prose with nothing to fetch.

// urlRegistry keeps twoai_url_registry in step with what the site actually
// publishes, by reading the sitemap the live site serves.
//
// WHY THE SITEMAP AND NOT twoai_pages. The obvious source is twoai_pages,
// and it is wrong. A twoai_pages row is not a page: the glossary is one row
// that renders 522 pages, the lawsuit tracker one row for 99, vendor news one
// row for 117. Worse, the mapping from a row to its URL lives in the Astro
// routes, not here, so any registry derived in Go duplicates route knowledge
// that can drift silently the moment a template moves. That drift is exactly
// what produced a registry that undercounted by 185 URLs and a Search Console
// report full of paths nobody could account for.
//
// The sitemap is generated by the build from the routes that actually
// rendered, so it is the only description of the site that cannot disagree
// with the site. Reading it here means a new section registers itself with no
// pipeline change at all.
//
// This runs before deploy_site, so it describes the build currently serving
// traffic rather than the one about to replace it. That is the correct
// meaning for a registry of live URLs, and it makes the row for a page that
// was just removed survive one extra day, which is what gives the disappeared
// check below something to report.
func urlRegistry(db *sql.DB) error {
	urls, err := fetchSitemapURLs("https://theworldofai.org/sitemap-index.xml")
	if err != nil {
		return err
	}

	// THE SITEMAP IS NO LONGER THE WHOLE SITE. It was, until vendor news
	// permalinks were withdrawn from it on 2026-08-26 to stop 2,227 feed
	// summaries competing for crawl budget against the glossary. Those pages
	// are live, linked and reachable; they are simply not advertised. Reading
	// the sitemap alone, this stage promptly reported 6,943 URLs gone and
	// raised 2,218 for redirect-or-restore, none of which needed either -
	// which would have buried the handful of genuinely disappeared URLs the
	// check exists to surface.
	//
	// The build now publishes what it withheld, so the registry still learns
	// the site from the build and cannot disagree with what rendered. Missing
	// or unreadable, the run continues on the sitemap alone rather than
	// failing: an incomplete registry is recoverable, and this stage refusing
	// to run is not.
	unlisted, uerr := fetchUnlistedURLs("https://theworldofai.org/unlisted-urls.json")
	if uerr != nil {
		fmt.Fprintln(os.Stderr, "url_registry: unlisted manifest unavailable, using sitemap only:", uerr)
	} else {
		seen := make(map[string]bool, len(urls))
		for _, u := range urls {
			seen[u] = true
		}
		added := 0
		for _, u := range unlisted {
			if !seen[u] {
				urls = append(urls, u)
				added++
			}
		}
		fmt.Printf("url_registry: sitemap=%d unlisted=%d combined=%d\n", len(urls)-added, added, len(urls))
	}

	// Fail closed. A sitemap that came back short is a fetch problem or a
	// broken build, not the site losing three thousand pages, and marking them
	// all disappeared would turn a transient failure into a permanent-looking
	// alarm. The floor is deliberately far below the real count so it only
	// catches genuine breakage.
	const minExpected = 500
	if len(urls) < minExpected {
		return fmt.Errorf("sitemap returned %d urls, below the %d floor, keeping the existing registry", len(urls), minExpected)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS twoai_url_registry (
		url text PRIMARY KEY,
		kind text NOT NULL,
		source_path text,
		title text,
		first_seen_at timestamptz NOT NULL DEFAULT now(),
		last_seen_at timestamptz NOT NULL DEFAULT now(),
		gsc_submitted_on date,
		gsc_batch text,
		bing_submitted_on date,
		notes text)`); err != nil {
		return err
	}

	// first_seen_at is never overwritten: it is the page's publication date as
	// far as we can observe it, and it is what makes "submitted to Search
	// Console but never indexed" answerable later.
	stmt, err := tx.Prepare(`INSERT INTO twoai_url_registry (url, kind, last_seen_at)
		VALUES ($1, $2, now())
		ON CONFLICT (url) DO UPDATE SET kind = EXCLUDED.kind, last_seen_at = now()`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, u := range urls {
		if _, err := stmt.Exec(u, urlRegistryKind(u)); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`ALTER TABLE twoai_url_registry
		ADD COLUMN IF NOT EXISTS resolution text,
		ADD COLUMN IF NOT EXISTS resolution_note text,
		ADD COLUMN IF NOT EXISTS resolved_at timestamptz`); err != nil {
		return err
	}

	// Rows the sitemap no longer lists. These are not deleted, because a URL
	// that stopped being published is the single most useful thing this table
	// can tell us: it was probably indexed, it is probably now a 404, and it
	// probably needs a redirect. Deleting it would destroy the evidence.
	//
	// A count alone was not enough. It sat at 77 for a day, rose to 81, and
	// nothing happened, because a warning with no state behind it is a warning
	// nobody can be behind on. Each vanished URL now carries a resolution:
	// 'redirected' when a rule was added, 'restored' when the page came back at
	// its own URL, 'intentional' when the URL was never meant to be public and
	// leaving it 404 is correct. Anything still unresolved after 48 hours is
	// named individually, the same tiered-tripwire discipline the timeline
	// entries use.
	var gone, unresolved int
	if err := tx.QueryRow(`SELECT count(*) FROM twoai_url_registry
		WHERE last_seen_at < now() - interval '1 hour'`).Scan(&gone); err != nil {
		return err
	}
	var overdue []string
	orows, err := tx.Query(`SELECT url FROM twoai_url_registry
		WHERE last_seen_at < now() - interval '48 hours' AND resolution IS NULL
		ORDER BY last_seen_at, url`)
	if err != nil {
		return err
	}
	for orows.Next() {
		var u string
		if err := orows.Scan(&u); err != nil {
			orows.Close()
			return err
		}
		overdue = append(overdue, u)
	}
	orows.Close()
	if err := tx.QueryRow(`SELECT count(*) FROM twoai_url_registry
		WHERE last_seen_at < now() - interval '1 hour' AND resolution IS NULL`).Scan(&unresolved); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	fmt.Printf("url_registry: live=%d gone=%d unresolved=%d\n", len(urls), gone, unresolved)
	if len(overdue) > 0 {
		fmt.Fprintf(os.Stderr, "url_registry: %d url(s) have been out of the sitemap for over 48 hours "+
			"with no resolution recorded. Redirect or restore each, then set resolution "+
			"('redirected' | 'restored' | 'intentional') on the row:\n", len(overdue))
		for i, u := range overdue {
			if i == 25 {
				fmt.Fprintf(os.Stderr, "  ... and %d more\n", len(overdue)-25)
				break
			}
			fmt.Fprintln(os.Stderr, "  "+u)
		}
	}
	return nil
}

// fetchUnlistedURLs reads the manifest of live pages the build deliberately
// keeps out of the sitemap. See astro.config.mjs for why it exists.
func fetchUnlistedURLs(u string) ([]string, error) {
	resp, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var out struct {
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.URLs, nil
}

// fetchSitemapURLs reads a sitemap index and returns every page URL beneath
// it. Sitemap indexes and sitemaps share the <loc> element, so one parser
// handles both: the first pass yields child sitemaps, the second yields pages.
func fetchSitemapURLs(indexURL string) ([]string, error) {
	children, err := fetchSitemapLocs(indexURL)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range children {
		// A sitemap index lists sitemaps; anything else is already a page URL,
		// which is what a single-file sitemap with no index would give us.
		if !strings.Contains(c, "sitemap") {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
			continue
		}
		pages, err := fetchSitemapLocs(c)
		if err != nil {
			return nil, err
		}
		for _, p := range pages {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out, nil
}

func fetchSitemapLocs(u string) ([]string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "srj-pipeline/1.0 (+https://theworldofai.org/)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s returned %d", u, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Locs []string `xml:"url>loc"`
		Maps []string `xml:"sitemap>loc"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	if len(doc.Maps) > 0 {
		return doc.Maps, nil
	}
	return doc.Locs, nil
}

// urlRegistryKind labels a URL by the section it belongs to, so the registry
// can be queried by content type without joining anything. The labels follow
// the URL, which is the only thing we can read here; a page that moves gets
// relabelled on the next run, which is the intended behaviour.
func urlRegistryKind(u string) string {
	p := strings.Trim(strings.TrimPrefix(strings.TrimPrefix(u, "https://theworldofai.org"), "/"), "/")
	if p == "" {
		return "home"
	}
	seg := strings.Split(p, "/")
	if len(seg) == 1 {
		switch seg[0] {
		case "ai-laws", "ai-glossary", "ai-lawsuits", "ai-tools", "companies", "research",
			"ai-compliance", "mcp", "benchmarks", "this-week-in-ai", "ai-ecosystem",
			"ai-news", "ai-prompts", "calculators", "sources", "api":
			return seg[0] + "-hub"
		}
		return "static"
	}
	switch seg[0] {
	case "ai-laws":
		return "state-law"
	case "ai-glossary":
		return "glossary-term"
	case "ai-lawsuits":
		return "lawsuit"
	case "ai-compliance":
		return "compliance"
	case "mcp":
		return "mcp-server"
	case "benchmarks":
		return "benchmark"
	case "companies":
		return "company"
	case "this-week-in-ai":
		return "week"
	case "calculators":
		return "calculator"
	case "ai-prompts":
		return "prompt-page"
	case "ai-tools":
		if strings.HasPrefix(seg[1], "cat-") {
			return "tool-category"
		}
		return "tool"
	case "research":
		if seg[1] == "paper" {
			return "research-paper"
		}
		if seg[1] == "watch" {
			return "research-watch"
		}
		return "research-topic"
	case "ai-news":
		if seg[1] == "vendor" {
			if len(seg) > 2 {
				return "news-vendor-post"
			}
			return "news-vendor-hub"
		}
		if seg[1] == "daily" {
			return "news-daily"
		}
		return "news-story"
	case "ai-ecosystem":
		if len(seg) == 2 {
			return "ecosystem-category"
		}
		return "ecosystem-entity"
	}
	return "other"
}

func benchResults(db *sql.DB) error {
	updated := 0

	if res, err := benchFetchSweBenchVerified(); err != nil {
		fmt.Fprintf(os.Stderr, "bench_results: swe-bench: %v (keeping last snapshot)\n", err)
	} else if err := benchStore(db, "swe-bench", res); err != nil {
		return err
	} else {
		updated++
	}

	if key := os.Getenv("AA_API_KEY"); key == "" {
		fmt.Fprintln(os.Stderr, "bench_results: AA_API_KEY not set, hle and gpqa stay on the curated snapshot (free key: artificialanalysis.ai)")
	} else {
		for slug, cfg := range map[string]struct {
			evalKeys       []string
			metric, srcURL string
		}{
			"hle":  {[]string{"humanitys_last_exam", "hle"}, "Accuracy, Artificial Analysis protocol", "https://artificialanalysis.ai/evaluations/humanitys-last-exam"},
			"gpqa": {[]string{"gpqa_diamond", "gpqa"}, "Accuracy on the 198-question Diamond subset", "https://artificialanalysis.ai/evaluations/gpqa-diamond"},
		} {
			if res, err := benchFetchArtificialAnalysis(key, cfg.evalKeys, cfg.metric, cfg.srcURL); err != nil {
				fmt.Fprintf(os.Stderr, "bench_results: %s via Artificial Analysis: %v (keeping last snapshot)\n", slug, err)
			} else if err := benchStore(db, slug, res); err != nil {
				return err
			} else {
				updated++
			}
		}
	}

	benchCheckLMArenaMirror()

	// Mirror changed results into the page rows the site renders. Guarded on
	// IS DISTINCT FROM so untouched pages keep their updated_at.
	if _, err := db.Exec(`UPDATE twoai_pages p
		SET data = p.data || jsonb_build_object('results', b.results), updated_at = now()
		FROM twoai_benchmarks b
		WHERE p.path = 'benchmarks/' || b.slug || '.json'
		  AND b.results IS NOT NULL
		  AND p.data->'results' IS DISTINCT FROM b.results`); err != nil {
		return err
	}

	fmt.Printf("bench_results: updated=%d ok=true\n", updated)
	return nil
}

// benchStore writes one benchmark's refreshed results and bumps updated_at,
// which is what keeps the twoai_build staleness tripwire quiet for adapters
// that are succeeding and lets it fire for ones that have been failing.
func benchStore(db *sql.DB, slug string, results map[string]any) error {
	buf, err := json.Marshal(results)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE twoai_benchmarks SET results = $1::jsonb, updated_at = now() WHERE slug = $2`, string(buf), slug)
	return err
}

// benchFetchSweBenchVerified reads the official SWE-bench leaderboard data:
// data/leaderboards.json in the swe-bench.github.io repo (master branch),
// which is the artifact the JS site renders. Entries are agent+model
// combinations with a resolved percentage and a submission date, which is
// editorially better than model-only vendor numbers because the scaffold is
// explicit in the row name. Validation: the Verified board must exist, carry
// at least 20 entries (it had 180 on 2026-08-10, so fewer than 20 means the
// shape changed), and every kept score must sit in (0,100].
func benchFetchSweBenchVerified() (map[string]any, error) {
	client := &http.Client{Timeout: 90 * time.Second}
	req, _ := http.NewRequest("GET", "https://raw.githubusercontent.com/swe-bench/swe-bench.github.io/master/data/leaderboards.json", nil)
	req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (theworldofai.org)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var doc struct {
		Leaderboards []struct {
			Name    string `json:"name"`
			Results []struct {
				Name     string  `json:"name"`
				Resolved float64 `json:"resolved"`
				Date     string  `json:"date"`
			} `json:"results"`
		} `json:"leaderboards"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	for _, lb := range doc.Leaderboards {
		if lb.Name != "Verified" {
			continue
		}
		type row struct {
			name     string
			resolved float64
			date     string
		}
		var rows []row
		for _, r := range lb.Results {
			if r.Name != "" && r.Resolved > 0 && r.Resolved <= 100 {
				rows = append(rows, row{r.Name, r.Resolved, r.Date})
			}
		}
		if len(rows) < 20 {
			return nil, fmt.Errorf("verified board has %d valid entries, expected 20+, shape may have changed", len(rows))
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].resolved > rows[j].resolved })
		out := []map[string]any{}
		for _, r := range rows[:5] {
			detail := ""
			if r.date != "" {
				detail = "submitted " + r.date
			}
			out = append(out, map[string]any{
				"system": r.name,
				"score":  fmt.Sprintf("%.1f%%", r.resolved),
				"detail": detail,
			})
		}
		return map[string]any{
			"as_of":      time.Now().UTC().Format("2006-01-02"),
			"source":     "SWE-bench official leaderboard (auto-refreshed daily from the published data file)",
			"source_url": "https://www.swebench.com/",
			"metric":     "% of 500 SWE-bench Verified issues resolved, official submissions",
			"note":       "Rows are agent-plus-model combinations as officially submitted, so the scaffold is part of the result. Vendor-reported model-only numbers are often higher than official submissions because harnesses differ; the official board is the comparable set.",
			"rows":       out,
		}, nil
	}
	return nil, fmt.Errorf("no Verified board in leaderboards.json")
}

// benchFetchArtificialAnalysis pulls one evaluation column from the
// Artificial Analysis models API and returns the top three models on it.
// EXPERIMENTAL until the first keyed run: the response shape was documented
// but could not be verified without a key, so the parse is defensive (finds
// the model array under "data", reads evaluations as a loose map, requires
// at least three parseable scores in (0,100]) and any surprise fails closed.
func benchFetchArtificialAnalysis(key string, evalKeys []string, metric, srcURL string) (map[string]any, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	req, _ := http.NewRequest("GET", "https://artificialanalysis.ai/api/v2/data/llms/models", nil)
	req.Header.Set("x-api-key", key)
	req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (theworldofai.org)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var doc struct {
		Data []struct {
			Name        string             `json:"name"`
			Evaluations map[string]float64 `json:"evaluations"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	type row struct {
		name  string
		score float64
	}
	var rows []row
	for _, m := range doc.Data {
		for _, k := range evalKeys {
			if v, ok := m.Evaluations[k]; ok && m.Name != "" {
				if v > 0 && v <= 1 { // some APIs report fractions
					v *= 100
				}
				if v > 0 && v <= 100 {
					rows = append(rows, row{m.Name, v})
				}
				break
			}
		}
	}
	if len(rows) < 3 {
		return nil, fmt.Errorf("parsed %d scores for %v, expected 3+, response shape differs from the assumption", len(rows), evalKeys)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].score > rows[j].score })
	out := []map[string]any{}
	for _, r := range rows[:3] {
		out = append(out, map[string]any{"system": r.name, "score": fmt.Sprintf("%.1f%%", r.score)})
	}
	return map[string]any{
		"as_of":      time.Now().UTC().Format("2006-01-02"),
		"source":     "Artificial Analysis API (auto-refreshed daily)",
		"source_url": srcURL,
		"metric":     metric,
		"note":       "Independent evaluation under one consistent protocol. Numbers from other evaluators differ because protocols differ; compare within one evaluator, not across.",
		"rows":       out,
	}, nil
}

// benchCheckLMArenaMirror watches the public Hugging Face mirror of the
// Arena leaderboard, which froze at leaderboard_table_20250804.csv. It never
// writes results: if the mirror revives, the CSV schema is by definition
// unverified, so the stage only logs that fresh data exists and a manual
// look is warranted. Silence in the log means the mirror is still frozen.
func benchCheckLMArenaMirror() {
	client := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest("GET", "https://huggingface.co/api/spaces/lmarena-ai/arena-leaderboard", nil)
	req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (theworldofai.org)")
	resp, err := client.Do(req)
	if err != nil {
		return // a watch endpoint failing is not worth a warning
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	var doc struct {
		Siblings []struct {
			Rfilename string `json:"rfilename"`
		} `json:"siblings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return
	}
	latest := ""
	for _, s := range doc.Siblings {
		f := s.Rfilename
		if strings.HasPrefix(f, "leaderboard_table_") && strings.HasSuffix(f, ".csv") && f > latest {
			latest = f
		}
	}
	if latest == "" {
		return
	}
	d, err := time.Parse("20060102", strings.TrimSuffix(strings.TrimPrefix(latest, "leaderboard_table_"), ".csv"))
	if err != nil {
		return
	}
	if time.Since(d) < 30*24*time.Hour {
		fmt.Fprintf(os.Stderr, "bench_results: LMArena HF mirror has fresh data (%s), review it and refresh the lmarena snapshot\n", latest)
	}
}

// twoaiWeeks builds the weekly digest, one page per ISO week, from our own
// verified tables rather than from a news feed.
//
// This is deliberately NOT built on ai_intel_candidates for the press-coverage
// half of that table. Those rows are headline-only: summary is null, the URL
// is frequently an opaque news.google.com redirect rather than a publisher
// link, and the vendor field is whichever domain a coverage query surfaced. A
// page assembled from that would be a wall of other people's headlines, which
// is both worthless to a reader and the exact shape of content ad networks
// reject. The vendor-feed half of the same table IS usable, has real publisher
// URLs, and is what twoaiVendorNews above renders; the daily press briefing is
// published separately by publish_news from clustered, summarised GDELT.
//
// So the honest weekly here is legislative movement, federal action, and
// docket movement, all of which we verify ourselves.
//
// Each item carries the explanatory text we already hold and can stand behind:
// LegiScan's own bill description, the Federal Register's abstract, which is a
// government-written summary in the public domain, and the why_it_matters
// paragraph from ai_lawsuits. Nothing on the page is written about an item we
// have not read. On top of that the stage computes a thematic breakdown, so a
// reader sees what the week was ABOUT rather than thirty unrelated rows.
//
// Quiet weeks are published as quiet, not padded. A week with two bills and
// nothing else says so, because a tracker that always claims a busy week is a
// tracker nobody can use to tell busy from quiet.

// twoaiTimeline renders the AI Historical Timeline at /ai-news/timeline/.
//
// HOW IT STAYS CURRENT, AND WHY IT IS BUILT THIS WAY. A timeline is the page
// type most likely to rot: the 1943 entry is settled forever, while the last
// two years change under you. So the two halves are kept current by different
// means, because they fail in different ways.
//
// The historical spine is curated rows in twoai_timeline, each carrying its
// primary source and the date it was last reviewed. It does not refresh,
// because there is nothing to refresh: what changes about 1956 is our
// understanding, not the facts, and that is a job for a person. The review
// date is rendered so a stale entry is visible rather than merely old.
//
// The current era updates itself from records we already verify daily. The
// tempting version of this reads the news feed and promotes whatever looks
// big, which is fabrication with extra steps: a press release is not a
// milestone, and no rule over prose can tell the difference. Instead the
// significance judgment is made once, deliberately, by flagging a case as
// timeline_milestone, and the pipeline then carries that case's live docket
// status and development date onto the timeline every day. The editorial
// decision is a human's and is made once; the facts underneath it stay fresh
// on their own. Unflagging a case removes its entry on the next run.
//
// Rows carry origin so the two kinds never blur: curated entries say a person
// wrote them, auto entries link to the case page and the court record.
func twoaiTimeline(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	// Sync the flagged lawsuits into the timeline. Written every run so a
	// docket development reaches the timeline the same day it reaches the case
	// page, and deleted when a case is unflagged so the two cannot disagree.
	if _, err := db.Exec(`DELETE FROM twoai_timeline
		WHERE origin = 'auto-lawsuit'
		  AND origin_ref NOT IN (SELECT slug FROM ai_lawsuits WHERE timeline_milestone)`); err != nil {
		return 0, err
	}
	if _, err := db.Exec(`INSERT INTO twoai_timeline
			(id, year, date_text, title, detail, category, era, source_name, source_url, origin, origin_ref, reviewed_on, updated_at)
		SELECT 'case-' || slug,
			EXTRACT(YEAR FROM filed_date)::int,
			to_char(filed_date, 'FMMonth YYYY'),
			case_name,
			coalesce(why_it_matters, executive_summary, summary, '') ||
				case when status is not null and status <> ''
					then ' Status as of ' || to_char(coalesce(latest_development_date, current_date), 'FMMonth DD, YYYY') || ': ' || status || '.'
					else '' end,
			'litigation',
			CASE
				WHEN EXTRACT(YEAR FROM filed_date) < 1956 THEN 'Origins (1943-1955)'
				WHEN EXTRACT(YEAR FROM filed_date) < 1974 THEN 'Symbolic era (1956-1973)'
				WHEN EXTRACT(YEAR FROM filed_date) < 1993 THEN 'AI winters and expert systems (1974-1992)'
				WHEN EXTRACT(YEAR FROM filed_date) < 2012 THEN 'Statistical turn (1993-2011)'
				WHEN EXTRACT(YEAR FROM filed_date) < 2017 THEN 'Deep learning era (2012-2016)'
				WHEN EXTRACT(YEAR FROM filed_date) < 2022 THEN 'Transformer era (2017-2021)'
				ELSE 'Assistant era (2022-present)'
			END,
			coalesce(court, 'Court record'),
			coalesce(courtlistener_url, source_url),
			'auto-lawsuit', slug, current_date, now()
		FROM ai_lawsuits
		WHERE timeline_milestone AND filed_date IS NOT NULL
		ON CONFLICT (id) DO UPDATE SET
			title = EXCLUDED.title, detail = EXCLUDED.detail,
			source_url = EXCLUDED.source_url, source_name = EXCLUDED.source_name,
			date_text = EXCLUDED.date_text, year = EXCLUDED.year,
			reviewed_on = EXCLUDED.reviewed_on, updated_at = now()`); err != nil {
		return 0, err
	}

	type entry struct {
		ID         string `json:"id"`
		Year       int    `json:"year"`
		Date       string `json:"date,omitempty"`
		Title      string `json:"title"`
		Detail     string `json:"detail"`
		Category   string `json:"category"`
		Era        string `json:"era"`
		SourceName string `json:"source_name,omitempty"`
		SourceURL  string `json:"source_url,omitempty"`
		Origin     string `json:"origin"`
		CaseSlug   string `json:"case_slug,omitempty"`
		Reviewed   string `json:"reviewed_on,omitempty"`
	}
	rows, err := db.Query(`SELECT id, year, coalesce(date_text,''), title, detail, category, era,
			coalesce(source_name,''), coalesce(source_url,''), origin,
			coalesce(origin_ref,''), coalesce(to_char(reviewed_on,'YYYY-MM-DD'),'')
		FROM twoai_timeline ORDER BY year, coalesce(sort_key, date_text, title)`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var all []entry
	eraOrder := []string{}
	seenEra := map[string]bool{}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.Year, &e.Date, &e.Title, &e.Detail, &e.Category, &e.Era,
			&e.SourceName, &e.SourceURL, &e.Origin, &e.CaseSlug, &e.Reviewed); err != nil {
			return 0, err
		}
		if e.Origin != "auto-lawsuit" {
			e.CaseSlug = ""
		}
		all = append(all, e)
		if !seenEra[e.Era] {
			seenEra[e.Era] = true
			eraOrder = append(eraOrder, e.Era)
		}
	}
	if len(all) == 0 {
		return 0, nil
	}

	// Group by era in first-appearance order, which is chronological because
	// the query is ordered by year.
	type eraBlock struct {
		Era     string  `json:"era"`
		Entries []entry `json:"entries"`
	}
	blocks := make([]eraBlock, 0, len(eraOrder))
	for _, name := range eraOrder {
		b := eraBlock{Era: name}
		for _, e := range all {
			if e.Era == name {
				b.Entries = append(b.Entries, e)
			}
		}
		blocks = append(blocks, b)
	}

	auto := 0
	for _, e := range all {
		if e.Origin == "auto-lawsuit" {
			auto++
		}
	}

	if err := upsert("timeline/index.json", "timeline", map[string]any{
		"uid":  twoaiUID("section:historical-timeline"),
		"eras": blocks, "total": len(all), "auto": auto,
		"curated":    len(all) - auto,
		"first_year": all[0].Year, "last_year": all[len(all)-1].Year,
		"generated": today,
	}); err != nil {
		return 0, err
	}
	return len(all), nil
}

func twoaiWeeks(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	type weekItem struct {
		State  string   `json:"state,omitempty"`
		Number string   `json:"number,omitempty"`
		Title  string   `json:"title"`
		URL    string   `json:"url"`
		Date   string   `json:"date"`
		Note   string   `json:"note,omitempty"`
		Slug   string   `json:"slug,omitempty"`
		Detail string   `json:"detail,omitempty"`
		Agency string   `json:"agency,omitempty"`
		Themes []string `json:"themes,omitempty"`
	}

	// Monday of the current ISO week, in UTC, then walk back eight weeks.
	now := time.Now().UTC()
	offset := (int(now.Weekday()) + 6) % 7 // Monday = 0
	thisMonday := time.Date(now.Year(), now.Month(), now.Day()-offset, 0, 0, 0, 0, time.UTC)

	type wk struct {
		Slug, Label, Start, End string
		Bills, Federal, Courts  []weekItem
	}
	var built []wk

	for back := 0; back < 8; back++ {
		start := thisMonday.AddDate(0, 0, -7*back)
		end := start.AddDate(0, 0, 7)
		iso, isoWeek := start.ISOWeek()
		w := wk{
			Slug:  fmt.Sprintf("%d-w%02d", iso, isoWeek),
			Label: fmt.Sprintf("Week %d, %d", isoWeek, iso),
			Start: start.Format("2006-01-02"),
			End:   end.AddDate(0, 0, -1).Format("2006-01-02"),
		}
		w.Bills = []weekItem{}
		w.Federal = []weekItem{}
		w.Courts = []weekItem{}

		br, err := db.Query(`SELECT DISTINCT ON (d.external_id) d.title, d.url,
				COALESCE(to_char(d.published_at,'YYYY-MM-DD'),''),
				COALESCE(d.raw->'bill'->>'description','')
			FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
			WHERE s.key='legiscan' AND d.published_at >= $1 AND d.published_at < $2
			ORDER BY d.external_id, d.id DESC`, start, end)
		if err != nil {
			return 0, err
		}
		for br.Next() {
			var title, url, date, descr string
			if br.Scan(&title, &url, &date, &descr) != nil {
				continue
			}
			parts := strings.SplitN(title, ":", 2)
			head := strings.Fields(parts[0])
			if len(head) < 2 {
				continue
			}
			code := strings.ToUpper(head[0])
			name, ok := twoaiStates[code]
			if !ok {
				continue
			}
			item := weekItem{State: name, Number: strings.Join(head[1:], " "), URL: url, Date: date}
			if len(parts) == 2 {
				item.Title = strings.TrimSpace(parts[1])
			}
			// LegiScan's description repeats the title on most bills. Carry it
			// only when it actually adds something, so the page does not print
			// the same sentence twice under a heading that promises more.
			if d := strings.TrimSpace(descr); d != "" && !strings.EqualFold(d, item.Title) {
				item.Detail = d
			}
			item.Themes = twoaiClassify(item.Title + " " + item.Detail)
			item.Slug = twoaiSlug(name)
			w.Bills = append(w.Bills, item)
		}
		br.Close()

		// The Federal Register corpus predates the mentionsAI filter, so it
		// still holds pre-filter rows: a Caribbean fishery council meeting is
		// in there because its full text says "artificial intelligence" once.
		// Filter again on read, or the weekly reports fishery meetings as AI
		// policy. The abstract is a government-written summary and public
		// domain, so it can be shown in full.
		fr, err := db.Query(`SELECT d.title, d.url, COALESCE(to_char(d.published_at,'YYYY-MM-DD'),''),
				COALESCE(d.raw->>'type',''), COALESCE(d.raw->>'abstract',''),
				COALESCE(d.raw->'agencies'->0->>'name','')
			FROM pipeline.documents d JOIN pipeline.sources s ON s.id=d.source_id
			WHERE s.key='federal_register' AND d.published_at >= $1 AND d.published_at < $2
			ORDER BY d.published_at DESC`, start, end)
		if err != nil {
			return 0, err
		}
		for fr.Next() {
			var it weekItem
			if fr.Scan(&it.Title, &it.URL, &it.Date, &it.Note, &it.Detail, &it.Agency) != nil {
				continue
			}
			if it.Title == "" || (!mentionsAI(it.Title) && !mentionsAI(it.Detail)) {
				continue
			}
			it.Themes = twoaiClassify(it.Title + " " + it.Detail)
			w.Federal = append(w.Federal, it)
		}
		fr.Close()

		cr, err := db.Query(`SELECT COALESCE(slug,''), case_name, COALESCE(court,''),
				COALESCE(latest_development,''), to_char(latest_development_date,'YYYY-MM-DD'),
				COALESCE(NULLIF(why_it_matters,''), COALESCE(executive_summary,'')), COALESCE(category,'')
			FROM ai_lawsuits
			WHERE is_active IS NOT FALSE AND latest_development_date >= $1 AND latest_development_date < $2
			ORDER BY latest_development_date DESC`, start, end)
		if err != nil {
			return 0, err
		}
		for cr.Next() {
			var it weekItem
			var court, category string
			if cr.Scan(&it.Slug, &it.Title, &court, &it.Note, &it.Date, &it.Detail, &category) == nil && it.Title != "" {
				it.State = court
				it.Agency = category
				it.URL = "/ai-lawsuits/" + it.Slug + "/"
				w.Courts = append(w.Courts, it)
			}
		}
		cr.Close()

		// An empty archive week is a page with nothing on it. The current week
		// always publishes, because "nothing yet this week" is a real answer;
		// older empty weeks are skipped rather than shipped hollow.
		if back > 0 && len(w.Bills)+len(w.Federal)+len(w.Courts) == 0 {
			continue
		}
		built = append(built, w)
	}

	idx := []map[string]any{}
	themeTotals := map[string]int{}
	for _, w := range built {
		// Thematic and jurisdictional breakdown for this week. Counts come from
		// the items themselves, so the summary sentence on the page can never
		// drift from the table below it.
		themeCount := map[string]int{}
		themeEg := map[string]string{}
		jur := map[string]int{}
		agency := map[string]int{}
		unthemed := 0
		for _, b := range w.Bills {
			jur[b.State]++
			if len(b.Themes) == 0 {
				unthemed++
			}
			for _, t := range b.Themes {
				themeCount[t]++
				themeTotals[t]++
				if themeEg[t] == "" {
					themeEg[t] = b.State + " " + b.Number
				}
			}
		}
		for _, f := range w.Federal {
			if f.Agency != "" {
				agency[f.Agency]++
			}
			for _, t := range f.Themes {
				themeCount[t]++
				themeTotals[t]++
			}
		}
		rank := func(m map[string]int) []map[string]any {
			ks := []string{}
			for k := range m {
				ks = append(ks, k)
			}
			sort.Slice(ks, func(i, j int) bool {
				if m[ks[i]] != m[ks[j]] {
					return m[ks[i]] > m[ks[j]]
				}
				return ks[i] < ks[j]
			})
			out := []map[string]any{}
			for _, k := range ks {
				row := map[string]any{"name": k, "count": m[k]}
				if eg := themeEg[k]; eg != "" {
					row["example"] = eg
				}
				for _, tr := range twoaiThemeRules {
					if tr.Name == k {
						row["blurb"] = tr.Blurb
					}
				}
				out = append(out, row)
			}
			return out
		}
		themes := rank(themeCount)
		if len(themes) > 8 {
			themes = themes[:8]
		}
		jurs := rank(jur)
		if len(jurs) > 8 {
			jurs = jurs[:8]
		}
		analysis := map[string]any{
			"themes": themes, "jurisdictions": jurs, "agencies": rank(agency),
			"jurisdiction_count": len(jur), "unthemed_bills": unthemed,
		}

		data := map[string]any{
			"slug": w.Slug, "label": w.Label, "start": w.Start, "end": w.End,
			"bills": w.Bills, "federal": w.Federal, "courts": w.Courts,
			"analysis": analysis,
			"counts": map[string]int{
				"bills": len(w.Bills), "federal": len(w.Federal), "courts": len(w.Courts),
				"total": len(w.Bills) + len(w.Federal) + len(w.Courts),
			},
			"generated": today,
		}
		if recap, model, rerr := twoaiWeekRecap(db, w.Label, w.Start, w.End, analysis,
			len(w.Bills), len(w.Federal), len(w.Courts)); rerr != nil {
			fmt.Fprintln(os.Stderr, "twoai week recap", w.Slug, ":", rerr)
		} else if recap != "" {
			data["recap"] = recap
			data["recap_model"] = model
		}
		if err := upsert("week/"+w.Slug+".json", "week", data); err != nil {
			return 0, err
		}
		topTheme := ""
		if len(themes) > 0 {
			topTheme, _ = themes[0]["name"].(string)
		}
		idx = append(idx, map[string]any{
			"slug": w.Slug, "label": w.Label, "start": w.Start, "end": w.End,
			"bills": len(w.Bills), "federal": len(w.Federal), "courts": len(w.Courts),
			"total":         len(w.Bills) + len(w.Federal) + len(w.Courts),
			"jurisdictions": len(jur), "top_theme": topTheme,
		})
	}
	if len(idx) == 0 {
		return 0, nil
	}
	// Archive-wide totals, so the hub can say what the whole period was about
	// rather than only listing weeks.
	tks := []string{}
	for k := range themeTotals {
		tks = append(tks, k)
	}
	sort.Slice(tks, func(i, j int) bool {
		if themeTotals[tks[i]] != themeTotals[tks[j]] {
			return themeTotals[tks[i]] > themeTotals[tks[j]]
		}
		return tks[i] < tks[j]
	})
	overall := []map[string]any{}
	for _, k := range tks {
		row := map[string]any{"name": k, "count": themeTotals[k]}
		for _, tr := range twoaiThemeRules {
			if tr.Name == k {
				row["blurb"] = tr.Blurb
			}
		}
		overall = append(overall, row)
		if len(overall) >= 10 {
			break
		}
	}
	grand := 0
	for _, w := range idx {
		grand += w["total"].(int)
	}
	if err := upsert("week/index.json", "week-hub", map[string]any{
		"weeks": idx, "latest": idx[0]["slug"], "generated": today,
		"themes": overall, "total_items": grand,
	}); err != nil {
		return 0, err
	}
	return len(built), nil
}

// twoaiCompliance renders the AI governance framework library: ISO 42001, the
// NIST AI RMF, the EU AI Act, SOC 2, the sector rules, the agency enforcement
// records, and the rest, 61 explainers held in site_content.
//
// THE MIGRATION PROBLEM, AND HOW THIS HANDLES IT. These explainers are already
// published at srjconsultingservices.com/ai-governance/. Stephen's intent is
// for theworldofai.org to become the source of all data and for the consulting
// site to become marketing, so the library belongs here eventually. But two
// sites under one owner publishing identical text is duplicate content:
// a search engine picks one and suppresses the other, and today the consulting
// site has the authority. Publishing these self-canonical right now would risk
// demoting the very pages that currently rank.
//
// So each page carries a canonical URL looked up from twoai_canonicals, which
// is seeded pointing at the SRJ original. The pages exist here, are linked, and
// are readable, while the search engine is told which copy is the original.
// When srj adds the 301 from /ai-governance/ to this site, deleting the row
// makes the page self-canonical on the next run. No code change, no rebuild of
// the factory, one DELETE per page or one for the lot.
//
// Body HTML is passed through as authored. It is SRJ's own writing, not
// third-party text, so there is nothing here to paraphrase around.
func twoaiCompliance(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	rows, err := db.Query(`SELECT c.path, c.data::text, COALESCE(k.canonical_url,'')
		FROM site_content c
		LEFT JOIN twoai_canonicals k
		  ON k.path = 'compliance/' || (c.data->>'slug') || '.json'
		WHERE c.path LIKE 'governance/%'
		  AND c.path NOT IN ('governance/_meta.json','governance/sources.json','governance/ai-tools.json')
		ORDER BY c.path`)
	if err != nil {
		return 0, err
	}
	type entry struct {
		Slug     string `json:"slug"`
		Title    string `json:"title"`
		Subtitle string `json:"subtitle,omitempty"`
		Short    string `json:"short,omitempty"`
		Parent   string `json:"parent,omitempty"`
	}
	var all []map[string]any
	for rows.Next() {
		var p, raw, canon string
		if rows.Scan(&p, &raw, &canon) != nil {
			continue
		}
		var doc map[string]any
		if json.Unmarshal([]byte(raw), &doc) != nil {
			continue
		}
		if s, _ := doc["slug"].(string); s == "" {
			continue
		}
		doc["generated"] = today
		if canon != "" {
			doc["canonical"] = canon
		}
		all = append(all, doc)
	}
	rows.Close()
	if len(all) == 0 {
		return 0, nil
	}

	count := 0
	index := []entry{}
	for _, doc := range all {
		slug, _ := doc["slug"].(string)
		if err := upsert("compliance/"+slug+".json", "compliance", doc); err != nil {
			return count, err
		}
		count++
		title, _ := doc["title"].(string)
		sub, _ := doc["subtitle"].(string)
		short, _ := doc["short"].(string)
		parent, _ := doc["parent"].(string)
		index = append(index, entry{slug, title, sub, short, parent})
	}
	sort.Slice(index, func(i, j int) bool { return index[i].Title < index[j].Title })
	if err := upsert("compliance/index.json", "compliance-hub", map[string]any{
		"frameworks": index, "total": len(index), "generated": today,
	}); err != nil {
		return count, err
	}
	return count + 1, nil
}

// twoaiEcosystem renders the site's spine: the AI Ecosystem root and its four
// categories, each listing the domains beneath it.
//
// The taxonomy lives in twoai_taxonomy rather than in a template, so the map a
// reader sees is the same map the build works from. That matters most for the
// parts that do NOT exist yet: a domain marked planned renders as planned,
// with no link, instead of quietly vanishing from the page. A site that only
// shows what it has finished is a site whose coverage nobody can assess.
//
// Page counts are computed from twoai_pages.taxonomy_slug, so the number next
// to a domain is the number of pages actually published under it.
func twoaiEcosystem(db *sql.DB, today string, upsert func(path, kind string, v any) error) (int, error) {
	type domain struct {
		Slug     string   `json:"slug"`
		Name     string   `json:"name"`
		Blurb    string   `json:"blurb"`
		Status   string   `json:"status"`
		Path     string   `json:"path,omitempty"`
		Pages    int      `json:"pages"`
		Sections []domain `json:"sections,omitempty"`
	}
	type category struct {
		Slug    string    `json:"slug"`
		Name    string    `json:"name"`
		Blurb   string    `json:"blurb"`
		Domains []*domain `json:"domains"`
		Live    int       `json:"live"`
		Pages   int       `json:"pages"`
	}

	// Levels 1 to 3: category, domain, section. Sections are what a reader
	// actually visits, so a domain with sections reports their combined page
	// count and counts as live when any section is.
	//
	// The count is pages, except where a section IS one page holding many
	// things. AI Jobs and Market Dynamics reported "1" beside a page carrying
	// 1,721 listings, which reads as one job. Where a section has a single row
	// and that row declares a larger `total`, the total is the honest number.
	//
	// Deliberately narrow. `total` does not mean the same thing everywhere —
	// state-ai-laws reports 2,417 bills across 54 pages, ai-tools-catalog 320
	// tools across 87 — and using it wherever it is larger would silently
	// restate counts the site has shown for weeks. Only the one-page-many-items
	// case is wrong, so only that case changes.
	rows, err := db.Query(`SELECT t.slug, t.name, COALESCE(t.blurb,''), t.status,
			COALESCE(t.live_path,''), COALESCE(t.parent_slug,''), t.level,
			(SELECT CASE
				WHEN count(*) = 1 AND COALESCE(max(NULLIF(p.data->>'total','')::int),0) > COALESCE(sum(p.url_count),0)
					THEN max(NULLIF(p.data->>'total','')::int)
				ELSE COALESCE(sum(p.url_count),0) END
			 FROM twoai_pages p WHERE p.taxonomy_slug = t.slug)
		FROM twoai_taxonomy t WHERE t.level IN (1,2,3) ORDER BY t.level, t.sort`)
	if err != nil {
		return 0, err
	}
	var cats []*category
	byslug := map[string]*category{}
	doms := map[string]*domain{}
	type pending struct {
		parent string
		d      domain
		level  int
	}
	var later []pending
	for rows.Next() {
		var slug, name, blurb, status, path, parent string
		var level, pages int
		if rows.Scan(&slug, &name, &blurb, &status, &path, &parent, &level, &pages) != nil {
			continue
		}
		if level == 1 {
			c := &category{Slug: slug, Name: name, Blurb: blurb}
			cats = append(cats, c)
			byslug[slug] = c
			continue
		}
		later = append(later, pending{parent, domain{Slug: slug, Name: name, Blurb: blurb,
			Status: status, Path: path, Pages: pages}, level})
	}
	rows.Close()
	// Domains first, then sections, so a section always finds its parent.
	for _, p := range later {
		if p.level != 2 {
			continue
		}
		c := byslug[p.parent]
		if c == nil {
			continue
		}
		d := p.d
		// Store pointers, not values. Appending to a slice can reallocate its
		// backing array, which silently invalidates any pointer taken into it
		// earlier: that is exactly how every section was attached to a copy
		// that then vanished, leaving live domains reporting zero pages and
		// linking nowhere.
		c.Domains = append(c.Domains, &d)
		doms[d.Slug] = &d
	}
	for _, p := range later {
		if p.level != 3 {
			continue
		}
		if d := doms[p.parent]; d != nil {
			d.Sections = append(d.Sections, p.d)
			d.Pages += p.d.Pages
			if p.d.Status == "live" && d.Status != "live" {
				d.Status = "live"
			}
		}
	}
	// AI Security and Risk presents its SIX SECURITY DOMAINS at this level,
	// not sixteen topics flat - the topics live grouped under the domains on
	// the hub the domain path points to. Sixteen entries told a category-page
	// reader nothing about how the coverage is organized.
	if d := doms["ai-security-risk"]; d != nil && d.Path != "" {
		topicPages := d.Pages
		grows, gerr := db.Query(`SELECT slug, label, blurb FROM twoai_security_domain_defs ORDER BY sort`)
		if gerr == nil {
			var six []domain
			for grows.Next() {
				var slug, label, blurb string
				if grows.Scan(&slug, &label, &blurb) == nil {
					six = append(six, domain{Slug: "secdom-" + slug, Name: label, Blurb: blurb,
						Status: "live", Path: d.Path + "#" + slug})
				}
			}
			grows.Close()
			if len(six) == 6 {
				d.Sections = six
				d.Pages = topicPages
			}
		}
	}
	for _, c := range cats {
		for _, d := range c.Domains {
			c.Pages += d.Pages
			if d.Status == "live" {
				c.Live++
			}
		}
	}
	if len(cats) == 0 {
		return 0, nil
	}

	count := 0
	summary := []map[string]any{}
	for _, c := range cats {
		if err := upsert("ecosystem/"+c.Slug+".json", "ecosystem-category", map[string]any{
			"slug": c.Slug, "name": c.Name, "blurb": c.Blurb, "domains": c.Domains,
			"live": c.Live, "total": len(c.Domains), "pages": c.Pages, "generated": today,
		}); err != nil {
			return count, err
		}
		count++
		summary = append(summary, map[string]any{
			"slug": c.Slug, "name": c.Name, "blurb": c.Blurb,
			"live": c.Live, "total": len(c.Domains), "pages": c.Pages,
		})
	}
	if err := upsert("ecosystem/index.json", "ecosystem-hub", map[string]any{
		"categories": summary, "generated": today,
	}); err != nil {
		return count, err
	}
	return count + 1, nil
}

// twoaiWeekRecap writes the short prose recap that sits at the top of a weekly
// digest page. Claude Sonnet writes it, and three deliberate constraints keep
// it honest.
//
// FIRST, it is given ONLY the computed analysis: theme counts, jurisdiction
// counts, agency counts, and the three totals. It never sees the bill titles or
// the case captions. A model that cannot see the underlying items cannot invent
// a claim about one, and every number in the prose is a number the page already
// prints in a table directly below it, so the two can never disagree.
//
// SECOND, it is cached against a fingerprint of that analysis. A closed week
// never changes, so its recap is written once and then read from
// twoai_week_recaps forever. Only a week whose counts actually moved is
// rewritten. Without this the stage would spend tokens nightly producing a
// slightly different paragraph about identical facts, and the page would
// visibly churn for no reader benefit.
//
// THIRD, a failure is not fatal. No API key, a timeout, or a refusal returns an
// empty string, the page renders without the recap, and the tables carry it.
// The prose is a convenience on top of the record, never the record itself.
const twoaiRecapModel = "claude-sonnet-5"

func twoaiWeekRecap(db *sql.DB, label, start, end string, analysis map[string]any,
	bills, federal, courts int) (string, string, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_week_recaps (
		slug text PRIMARY KEY, fingerprint text NOT NULL, recap text NOT NULL,
		model text NOT NULL, created_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return "", "", err
	}
	facts, _ := json.Marshal(map[string]any{
		"week": label, "start": start, "end": end,
		"bill_movements": bills, "federal_documents": federal, "case_updates": courts,
		"analysis": analysis,
	})
	fp := fmt.Sprintf("%x", sha256.Sum256(facts))
	slug := start

	var cachedFP, cached, cachedModel string
	db.QueryRow(`SELECT fingerprint, recap, model FROM twoai_week_recaps WHERE slug=$1`, slug).
		Scan(&cachedFP, &cached, &cachedModel)
	if cachedFP == fp && cached != "" {
		return cached, cachedModel, nil
	}

	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "", "", nil // silent, not an error: the tables stand alone
	}
	prompt := "You are writing the opening recap for a weekly record of United States AI policy " +
		"and litigation. Below is the ONLY information you have: computed counts from a database. " +
		"You cannot see the individual bills or cases, and you must not invent any.\n\n" +
		"Write two short paragraphs, 90 to 140 words total. Say what the week was about, using the " +
		"theme and jurisdiction counts to describe the shape of the activity. Name a theme only if it " +
		"appears in the data. Use the exact numbers given. If the totals are small, say the week was " +
		"quiet; do not inflate it. Plain English, commas rather than dashes, no bullet points, no " +
		"headline, no opinions about whether any law is good or bad, and no predictions. Do not claim " +
		"a bill passed: a bill movement means its record changed status. Output only the paragraphs.\n\n" +
		"Data:\n" + string(facts)

	body, _ := json.Marshal(map[string]any{
		"model":      twoaiRecapModel,
		"max_tokens": 500,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", "", fmt.Errorf("anthropic %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	var sb strings.Builder
	for _, c := range out.Content {
		sb.WriteString(c.Text)
	}
	recap := strings.TrimSpace(sb.String())
	if recap == "" {
		return "", "", fmt.Errorf("empty recap")
	}
	if _, err := db.Exec(`INSERT INTO twoai_week_recaps (slug, fingerprint, recap, model)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (slug) DO UPDATE SET fingerprint=EXCLUDED.fingerprint,
			recap=EXCLUDED.recap, model=EXCLUDED.model, created_at=now()`,
		slug, fp, recap, twoaiRecapModel); err != nil {
		return recap, twoaiRecapModel, nil
	}
	return recap, twoaiRecapModel, nil
}

// twoaiPublish exports twoai_pages to the twoai-content repo, sha-compared
// against the tree so unchanged rows cost nothing. Export-only: SQL is the
// origin here, there is nothing to backfill.
func twoaiPublish(db *sql.DB) error {
	if ok, n, err := twoaiPublishGuard(db); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("publish guard blocked GitHub publish at %d pages", n)
	}
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	client := &http.Client{Timeout: 120 * time.Second}
	get := func(url string) ([]byte, int, error) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b, resp.StatusCode, nil
	}
	blobSha := func(b []byte) string {
		h := sha1.New()
		fmt.Fprintf(h, "blob %d", len(b))
		h.Write([]byte{0})
		h.Write(b)
		return hex.EncodeToString(h.Sum(nil))
	}
	repoSha := map[string]string{}
	if tb, code, err := get("https://api.github.com/repos/srjordan6/twoai-content/git/trees/main?recursive=1"); err == nil && code == 200 {
		var tree struct {
			Tree []struct {
				Path string `json:"path"`
				Type string `json:"type"`
				Sha  string `json:"sha"`
			} `json:"tree"`
		}
		if json.Unmarshal(tb, &tree) == nil {
			for _, e := range tree.Tree {
				if e.Type == "blob" {
					repoSha[e.Path] = e.Sha
				}
			}
		}
	}
	rows, err := db.Query(`SELECT path, jsonb_pretty(data) FROM twoai_pages ORDER BY path`)
	if err != nil {
		return err
	}
	defer rows.Close()
	exported, unchanged, failed := 0, 0, 0
	for rows.Next() {
		var path, pretty string
		if err := rows.Scan(&path, &pretty); err != nil {
			return err
		}
		payload := []byte(pretty + "\n")
		if repoSha[path] == blobSha(payload) {
			unchanged++
			continue
		}
		put := map[string]any{
			"message": fmt.Sprintf("twoai: %s from twoai_pages %s", path, time.Now().UTC().Format("2006-01-02")),
			"content": base64.StdEncoding.EncodeToString(payload),
		}
		if sha := repoSha[path]; sha != "" {
			put["sha"] = sha
		}
		pb, _ := json.Marshal(put)
		req, _ := http.NewRequest("PUT", "https://api.github.com/repos/srjordan6/twoai-content/contents/"+path, bytes.NewReader(pb))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		pr, err := client.Do(req)
		if err != nil {
			return err
		}
		prb, _ := io.ReadAll(pr.Body)
		pr.Body.Close()
		if pr.StatusCode != 200 && pr.StatusCode != 201 {
			// One transient GitHub 500 used to abort the whole export here,
			// leaving every path that sorts after the failing file unpublished
			// (2026-08-22: a 500 on an mcp/ file kept talent/ out of the repo
			// and 404'd the first live talent page). Retry once, then log and
			// move on; the daily diff republishes anything still missing.
			time.Sleep(2 * time.Second)
			r2, _ := http.NewRequest("PUT", "https://api.github.com/repos/srjordan6/twoai-content/contents/"+path, bytes.NewReader(pb))
			r2.Header.Set("Authorization", "Bearer "+tok)
			r2.Header.Set("Accept", "application/vnd.github+json")
			r2.Header.Set("User-Agent", "srj-pipeline/1.0")
			if pr2, err2 := client.Do(r2); err2 == nil {
				prb2, _ := io.ReadAll(pr2.Body)
				pr2.Body.Close()
				if pr2.StatusCode == 200 || pr2.StatusCode == 201 {
					exported++
					continue
				}
				prb = prb2
			}
			failed++
			fmt.Fprintf(os.Stderr, "twoai_publish: github PUT %s failed twice (skipped): %.200s\n", path, prb)
			continue
		}
		exported++
	}
	fmt.Printf("twoai_publish: exported=%d unchanged=%d failed=%d ok=%v\n", exported, unchanged, failed, failed == 0)
	if failed > 20 {
		return fmt.Errorf("twoai_publish: %d PUTs failed, likely systemic", failed)
	}
	return nil
}

// ---- sync_content: every remaining content file, SQL -> srj-content --------
//
// Completes the July 31 directive for the whole repo: governance, resources,
// migrated pages, books, and roster all live in site_content (path, data
// jsonb) in srj-audit-db, and the repo is a generated artifact. Out of scope:
// .github (CI code, not content), people/{slug}.json (owned by site_people),
// and the five files the publish stages regenerate daily from their own SQL
// tables (news, legislation, leaderboard, lawsuits, intel).
//
// The tree API supplies every path with its git blob sha, so the export
// compares shas instead of downloading content; a quiet day costs one tree
// call and zero commits. The backfill imports any in-scope repo file that has
// no SQL row yet (ON CONFLICT DO NOTHING), which absorbs the pre-directive
// library once and is a no-op forever after; SQL wins from then on.
func syncContent(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS site_content (
		path text PRIMARY KEY, data jsonb NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	// Book excerpts for the topbar, regenerated from press_book_excerpts on
	// every run so adding a verified excerpt in SQL is the whole workflow.
	// Only rows marked active ship, and each carries the book it came from so
	// the topbar line always links to its own source.
	if _, err := db.Exec(`INSERT INTO site_content (path, data)
		SELECT 'books/excerpts.json', jsonb_build_object(
			'note','Verbatim excerpts from the SRJ book series, rendered in the site topbar and linked to the book they came from.',
			'generated', current_date::text,
			'excerpts', COALESCE(jsonb_agg(jsonb_build_object(
				'book_number', e.book_number, 'title', b.title, 'url', b.url_path,
				'excerpt', e.excerpt, 'section', e.source_section) ORDER BY e.book_number, e.id), '[]'::jsonb))
		FROM press_book_excerpts e JOIN press_books b ON b.book_number = e.book_number
		WHERE e.active
		ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data`); err != nil {
		fmt.Printf("sync_content: book excerpts refresh skipped: %v\n", err)
	}

	// books/books.json, regenerated from press_books and press_book_isbns on
	// every run. The file's own note has always promised that a status, price,
	// ISBN or page count "can never drift from the database again", and told the
	// reader to regenerate it by re-running the query. Nothing ever ran it, so
	// it drifted three times: Book 04's status, Book 01's hardback and ebook
	// ISBNs transposed (the swap survived the 2026-08-01 barcode correction and
	// reached the Book/Offer schema Google and Amazon read), and Books 05 and 06
	// left marked forthcoming with no ISBNs at all, so both published volumes
	// rendered no specs block and emitted no schema for weeks.
	//
	// Every one of those was found by eye. This closes the class: the snapshot
	// is now derived, so the tables are the only place a book fact is edited.
	//
	// Ordering is explicit (book_number, then sort_order within isbns) because
	// the output is compared for equality before it is written, and a set
	// returned in a different order each run would rewrite the row daily and
	// churn the content repo for no reason.
	if _, err := db.Exec(`INSERT INTO site_content (path, data)
		SELECT 'books/books.json', jsonb_build_object(
			'note','Bibliographic facts only. The prose on each book page stays as migrated from WordPress; this file supplies the specs block and the Book/Offer schema, so a status, price, ISBN or page count can never drift from the database again. Regenerated from press_books and press_book_isbns on every pipeline run.',
			'generated', current_date::text,
			'books', COALESCE(jsonb_agg(b.entry ORDER BY b.book_number), '[]'::jsonb))
		FROM (
			SELECT p.book_number, jsonb_build_object(
				'book_number', p.book_number,
				'title', p.title,
				'subtitle', p.subtitle,
				'pillar', p.pillar,
				'status', p.status,
				'pages', p.pages,
				'published_on', p.published_on::text,
				'url_path', p.url_path,
				'amazon_url', p.amazon_url,
				'isbns', COALESCE((
					SELECT jsonb_agg(jsonb_build_object(
						'isbn', i.isbn, 'format', i.format, 'list_price', i.list_price)
						ORDER BY i.sort_order)
					FROM press_book_isbns i WHERE i.book_number = p.book_number), '[]'::jsonb)
			) AS entry
			FROM press_books p
		) b
		ON CONFLICT (path) DO UPDATE SET data = EXCLUDED.data
		WHERE site_content.data IS DISTINCT FROM EXCLUDED.data`); err != nil {
		fmt.Printf("sync_content: books.json refresh skipped: %v\n", err)
	}

	outOfScope := func(p string) bool {
		if !strings.HasSuffix(p, ".json") || strings.HasPrefix(p, ".github/") {
			return true
		}
		if strings.HasPrefix(p, "people/") && p != "people/roster.json" {
			return true
		}
		switch p {
		case "news/news.json", "legislation/legislation.json", "leaderboard/leaderboard.json",
			"lawsuits/lawsuits.json", "intel/intel.json":
			return true
		}
		return false
	}

	client := &http.Client{Timeout: 120 * time.Second}
	get := func(url string) ([]byte, int, error) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b, resp.StatusCode, nil
	}
	blobSha := func(b []byte) string {
		h := sha1.New()
		fmt.Fprintf(h, "blob %d", len(b))
		h.Write([]byte{0})
		h.Write(b)
		return hex.EncodeToString(h.Sum(nil))
	}

	tb, code, err := get("https://api.github.com/repos/srjordan6/srj-content/git/trees/main?recursive=1")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("trees API returned %d", code)
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Sha  string `json:"sha"`
			URL  string `json:"url"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(tb, &tree); err != nil {
		return err
	}
	repoSha := map[string]string{}

	imported := 0
	for _, e := range tree.Tree {
		if e.Type != "blob" || outOfScope(e.Path) {
			continue
		}
		repoSha[e.Path] = e.Sha
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM site_content WHERE path=$1)`, e.Path).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		// Blob API handles any size (the contents API caps at 1MB, and
		// migrated-pages.json is 1.7MB).
		bb, bc, err := get(e.URL)
		if err != nil || bc != 200 {
			fmt.Fprintf(os.Stderr, "sync_content: blob %s: status %d err %v\n", e.Path, bc, err)
			continue
		}
		var blob struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(bb, &blob) != nil {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
		if err != nil || !json.Valid(raw) {
			fmt.Fprintf(os.Stderr, "sync_content: skip %s: not valid JSON\n", e.Path)
			continue
		}
		if _, err := db.Exec(`INSERT INTO site_content (path, data) VALUES ($1, $2::jsonb)
			ON CONFLICT (path) DO NOTHING`, e.Path, string(raw)); err != nil {
			return err
		}
		imported++
	}

	rows, err := db.Query(`SELECT path, jsonb_pretty(data) FROM site_content ORDER BY path`)
	if err != nil {
		return err
	}
	defer rows.Close()
	exported, unchanged := 0, 0
	for rows.Next() {
		var path, pretty string
		if err := rows.Scan(&path, &pretty); err != nil {
			return err
		}
		payload := []byte(pretty + "\n")
		if repoSha[path] == blobSha(payload) {
			unchanged++
			continue
		}
		// PUT directly with the sha from the tree: the contents GET inside
		// putToContent 403s on files over 1MB (migrated-pages.json is 1.7MB),
		// which would strip the sha and turn the update into a 422.
		put := map[string]any{
			"message": fmt.Sprintf("content: %s from site_content %s", path, time.Now().UTC().Format("2006-01-02")),
			"content": base64.StdEncoding.EncodeToString(payload),
		}
		if sha := repoSha[path]; sha != "" {
			put["sha"] = sha
		}
		pb, _ := json.Marshal(put)
		req, _ := http.NewRequest("PUT", "https://api.github.com/repos/srjordan6/srj-content/contents/"+path, bytes.NewReader(pb))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		pr, err := client.Do(req)
		if err != nil {
			return err
		}
		prb, _ := io.ReadAll(pr.Body)
		pr.Body.Close()
		if pr.StatusCode != 200 && pr.StatusCode != 201 {
			return fmt.Errorf("github PUT %s %d: %.200s", path, pr.StatusCode, prb)
		}
		exported++
	}
	fmt.Printf("sync_content: imported=%d exported=%d unchanged=%d ok=true\n", imported, exported, unchanged)
	return nil
}

// ---- sync_people: AI Movers and Shakers, SQL -> srj-content ----------------
//
// site_people (slug, data jsonb) is the single source of truth for the people
// directory, per the July 31 directive that ALL content lives in SQL and no
// content files ever land on a local machine. This stage makes the repo a
// generated artifact of the table:
//
//  1. Ensures the table exists.
//  2. One-time backfill: any people/{slug}.json already in srj-content that
//     has no SQL row is imported (ON CONFLICT DO NOTHING, so SQL always
//     wins afterward). This absorbs the 37 pre-directive profiles without a
//     manual load and is a no-op on every later run.
//  3. Exports every SQL row to people/{slug}.json via the GitHub API,
//     skipping files whose content is already identical, so quiet days
//     produce zero commits.
//
// roster.json is not a person and is left alone in the repo.
func syncPeople(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS site_people (
		slug text PRIMARY KEY, data jsonb NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	client := &http.Client{Timeout: 60 * time.Second}
	gh := func(method, url string, body []byte) (*http.Response, error) {
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "srj-pipeline/1.0")
		return client.Do(req)
	}

	// 2. Backfill from the repo listing.
	resp, err := gh("GET", "https://api.github.com/repos/srjordan6/srj-content/contents/people", nil)
	if err != nil {
		return err
	}
	var listing []struct {
		Name        string `json:"name"`
		DownloadURL string `json:"download_url"`
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 {
		_ = json.Unmarshal(b, &listing)
	}
	imported := 0
	for _, f := range listing {
		if !strings.HasSuffix(f.Name, ".json") || f.Name == "roster.json" {
			continue
		}
		slug := strings.TrimSuffix(f.Name, ".json")
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM site_people WHERE slug=$1)`, slug).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		fr, err := gh("GET", f.DownloadURL, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sync_people: fetch %s: %v\n", f.Name, err)
			continue
		}
		fb, _ := io.ReadAll(fr.Body)
		fr.Body.Close()
		if fr.StatusCode != 200 || !json.Valid(fb) {
			fmt.Fprintf(os.Stderr, "sync_people: skip %s: status %d or invalid JSON\n", f.Name, fr.StatusCode)
			continue
		}
		if _, err := db.Exec(`INSERT INTO site_people (slug, data) VALUES ($1, $2::jsonb)
			ON CONFLICT (slug) DO NOTHING`, slug, string(fb)); err != nil {
			return err
		}
		imported++
	}

	// 3. Export SQL -> repo, skipping identical files.
	rows, err := db.Query(`SELECT slug, jsonb_pretty(data) FROM site_people ORDER BY slug`)
	if err != nil {
		return err
	}
	defer rows.Close()
	exported, unchanged := 0, 0
	for rows.Next() {
		var slug, pretty string
		if err := rows.Scan(&slug, &pretty); err != nil {
			return err
		}
		payload := []byte(pretty + "\n")
		path := "people/" + slug + ".json"
		cur, err := gh("GET", "https://api.github.com/repos/srjordan6/srj-content/contents/"+path, nil)
		if err == nil {
			var meta struct {
				Content string `json:"content"`
			}
			cb, _ := io.ReadAll(cur.Body)
			cur.Body.Close()
			if cur.StatusCode == 200 && json.Unmarshal(cb, &meta) == nil {
				if dec, e := base64.StdEncoding.DecodeString(strings.ReplaceAll(meta.Content, "\n", "")); e == nil && bytes.Equal(dec, payload) {
					unchanged++
					continue
				}
			}
		}
		if err := putToContent(tok, path,
			fmt.Sprintf("people: %s from site_people %s", slug, time.Now().UTC().Format("2006-01-02")), payload); err != nil {
			return err
		}
		exported++
	}
	fmt.Printf("sync_people: imported=%d exported=%d unchanged=%d ok=true\n", imported, exported, unchanged)
	return nil
}

// ---- deploy_site: rebuild the website so today's data actually ships -------
//
// The publish stages write JSON to srj-content, but the website only rebuilds
// when srj-site itself is pushed, so without this stage a day's lawsuits,
// vendor news, and roundup sit in the content repo and never reach a visitor.
// This fires srj-site's existing "Trigger Cloudflare build" workflow through
// the GitHub API, using the token the pipeline already carries. No new secret,
// and the workflow already supports workflow_dispatch.
//
// A failure here is reported but should never be read as "the data is wrong":
// the data is published either way, it is the rebuild that did not happen.
func deploySite() error {
	// Preferred path: POST the Cloudflare deploy hook directly. Set
	// CLOUDFLARE_DEPLOY_HOOK on the Render service (same URL srj-site keeps
	// as a GitHub secret); it needs no GitHub permissions at all.
	if hook := strings.TrimSpace(os.Getenv("CLOUDFLARE_DEPLOY_HOOK")); hook != "" {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Post(hook, "application/json", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return fmt.Errorf("deploy hook returned %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
		}
		// theworldofai.org rebuilds on the same trigger when its hook is set.
		if th := strings.TrimSpace(os.Getenv("TWOAI_DEPLOY_HOOK")); th != "" {
			tr, terr := client.Post(th, "application/json", nil)
			if terr != nil {
				fmt.Fprintln(os.Stderr, "twoai deploy hook:", terr)
			} else {
				trb, _ := io.ReadAll(tr.Body)
				tr.Body.Close()
				if tr.StatusCode < 200 || tr.StatusCode > 299 {
					fmt.Fprintf(os.Stderr, "twoai deploy hook returned %d: %s\n", tr.StatusCode, strings.TrimSpace(string(trb)))
				}
			}
		}
		return nil
	}
	// Fallback: dispatch srj-site's deploy workflow. Requires the PAT to
	// carry workflow scope; a 403 here means add that scope or set the
	// CLOUDFLARE_DEPLOY_HOOK env var above.
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set and CLOUDFLARE_DEPLOY_HOOK not set")
	}
	const api = "https://api.github.com/repos/srjordan6/srj-site/actions/workflows/deploy.yml/dispatches"
	body := []byte(`{"ref":"main"}`)
	req, err := http.NewRequest("POST", api, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "srj-pipeline/1.0")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	// 204 No Content is the documented success for a workflow dispatch.
	if resp.StatusCode != 204 && resp.StatusCode != 201 && resp.StatusCode != 200 {
		return fmt.Errorf("workflow dispatch returned %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	return nil
}

// ---- publish_lawsuits: AI Lawsuit Database -> srj-content ------------------
//
// Exports every active case in ai_lawsuits, in display order, as one JSON
// document the Astro build consumes for /ai-governance/ai-lawsuits/. Runs
// after the intel stage in `all`, so each night's docket refresh reaches the
// site on its next build. json.RawMessage passes the timeline/claims/tags
// JSONB and the array columns through verbatim instead of re-modeling them.
func publishLawsuits(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	rows, err := db.Query(`
		SELECT json_build_object(
		  'slug', slug, 'case_name', case_name, 'court', court, 'docket', docket,
		  'judge', judge, 'filed_date', filed_date, 'plaintiffs', plaintiffs,
		  'defendants', defendants, 'category', category, 'status', status,
		  'status_badge', status_badge, 'latest_development', latest_development,
		  'latest_development_date', latest_development_date,
		  'courtlistener_url', courtlistener_url,
		  'executive_summary', executive_summary, 'why_it_matters', why_it_matters,
		  'target_models', target_models, 'disputed_datasets', disputed_datasets,
		  'materials_at_issue', materials_at_issue,
		  'plaintiff_counsel', plaintiff_counsel, 'defendant_counsel', defendant_counsel,
		  'claims', claims, 'timeline', timeline, 'tags', tags,
		  'related_slugs', related_slugs, 'display_order', display_order,
		  'verified_date', verified_date)
		FROM ai_lawsuits WHERE is_active ORDER BY display_order`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var cases []json.RawMessage
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		cases = append(cases, json.RawMessage(raw))
	}
	if len(cases) == 0 {
		return fmt.Errorf("ai_lawsuits returned no active cases; refusing to publish an empty database")
	}
	out, _ := json.MarshalIndent(map[string]any{
		"generated": time.Now().UTC().Format(time.RFC3339),
		"cases":     cases,
	}, "", "  ")
	return putToContent(tok, "lawsuits/lawsuits.json",
		fmt.Sprintf("lawsuits: %d cases %s", len(cases), time.Now().UTC().Format("2006-01-02")), out)
}

// ---- publish_intel: AI watch feed -> srj-content ---------------------------
//
// Fallback extractors for feeds whose XML will not parse. Deliberately
// simple: find each item element, then the first title and link inside it.
var (
	itemRe  = regexp.MustCompile(`(?is)<item[^>]*>(.*?)</item>`)
	titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	linkRe  = regexp.MustCompile(`(?is)<link[^>]*>(.*?)</link>`)
)

// stripCDATA unwraps the CDATA sections RSS titles often arrive in.
func stripCDATA(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<![CDATA[")
	s = strings.TrimSuffix(s, "]]>")
	return strings.TrimSpace(s)
}

// Publishes the newest non-ignored rows from ai_intel_candidates (new models,
// tools, terminology, vendor announcements) for the Everything else AI page.
// Ignored rows never ship; everything else does, newest first, capped so the
// page and the JSON stay small.
func publishIntel(db *sql.DB) error {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return fmt.Errorf("GITHUB_TOKEN not set")
	}
	// Newest 25 per vendor rather than the newest 120 overall. The flat
	// limit made the published vendor set a rolling window: a vendor that
	// stopped producing news fell out of the newest 120, its
	// /ai-resources/ai-vendor-news/{vendor}/ page stopped generating, and
	// the URL began returning 404 to anyone (Google included) who had
	// already crawled it. Google Search Console surfaced 22 such 404s on
	// 2026-08-07. Partitioning by vendor keeps every vendor page alive
	// permanently, which is what a stable URL requires, while capping the
	// payload: 161 vendors and 745 rows at ~200KB, versus 1,963 rows if
	// the whole table were published.
	rows, err := db.Query(`
		SELECT json_build_object(
		  'kind', kind, 'name', name, 'vendor', vendor, 'url', url,
		  'summary', summary, 'source', source, 'discovered_at', discovered_at)
		FROM (
		  SELECT *, row_number() OVER (
		    PARTITION BY vendor ORDER BY discovered_at DESC) AS rn
		  FROM ai_intel_candidates
		  WHERE status <> 'ignored'
		) ranked
		WHERE rn <= 25
		ORDER BY discovered_at DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	items := []json.RawMessage{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		items = append(items, json.RawMessage(raw))
	}
	out, _ := json.MarshalIndent(map[string]any{
		"generated": time.Now().UTC().Format(time.RFC3339),
		"items":     items,
	}, "", "  ")
	return putToContent(tok, "intel/intel.json",
		fmt.Sprintf("intel: %d items %s", len(items), time.Now().UTC().Format("2006-01-02")), out)
}

// arxivWatch is phase 3 of the watch-everything directive: affiliation
// tracking on new arXiv preprints. Pulls the newest submissions in cs.AI,
// cs.CL, and cs.LG from the official arXiv Atom API and keeps only papers
// whose title or abstract names a tracked institution, so the volume that
// lands in ai_intel_candidates stays reviewable instead of flooding it with
// every preprint. arXiv terms permit this use; the API is free and keyless.
func arxivWatch(db *sql.DB) error {
	orgs := []string{
		"Tsinghua", "Peking University", "BAAI", "Beijing Academy",
		"Shanghai AI Lab", "Chinese Academy of Sciences", "RIKEN", "AIST",
		"KAIST", "Naver", "MBZUAI", "AI21", "Weizmann", "Hebrew University",
		"Mistral", "Aleph Alpha", "Stability AI", "Alan Turing Institute",
		"Max Planck", "INRIA", "ELLIS", "AI Singapore", "DeepMind", "OpenAI",
		"Anthropic", "Hugging Face", "Zhipu", "Moonshot", "DeepSeek", "Qwen",
		"Alibaba", "Tencent", "ByteDance", "Huawei", "Baidu",
	}
	type entry struct {
		Title   string `xml:"title"`
		Summary string `xml:"summary"`
		ID      string `xml:"id"`
	}
	var parsed struct {
		Entries []entry `xml:"entry"`
	}
	added, scanned := 0, 0
	// THE SAMPLE WAS TOO SMALL FOR THE VOLUME. This fetched the newest 40 per
	// category, which was reasonable when written and is not now: measured on
	// 2026-08-26, cs.AI published 155 papers in a single day and cs.LG about
	// 100, so 40 covered roughly a quarter of one day of one category, and
	// only whatever happened to be newest at the moment the stage ran.
	// Anything published between runs beyond that window was missed
	// permanently, because this stage has no cursor - it always asks for the
	// newest N.
	//
	// 200 per page, two pages, is 400 per category and comfortably covers a
	// day even on a heavy one. arXiv permits max_results up to 2000 and asks
	// for a courtesy delay between calls, which is already honoured below.
	//
	// WHY THIS MATTERS MORE THAN IT LOOKS: arXiv is the ONLY same-day source
	// in the platform. OpenAlex, which carries the works spine, had none of
	// the 15 newest cs.AI papers when checked on 2026-08-26 - preprints take
	// days to weeks to appear there. If this watch misses a paper, nothing
	// else catches it that week.
	for _, cat := range []string{"cs.AI", "cs.CL", "cs.LG"} {
		for _, start := range []int{0, 200} {
			u := fmt.Sprintf("https://export.arxiv.org/api/query?search_query=cat:%s"+
				"&sortBy=submittedDate&sortOrder=descending&start=%d&max_results=200", cat, start)
		// arXiv's API is occasionally slow to first byte (Aug 1: cs.AI timed
		// out at 30s and the whole category skipped for the day). One retry
		// with a longer timeout keeps a slow response from costing a category.
		fetch := func(timeout time.Duration) (*http.Response, error) {
			req, _ := http.NewRequest("GET", u, nil)
			req.Header.Set("User-Agent", "SRJ-Consulting-intel-sync/1.0 (srjconsultingservices.com)")
			client := &http.Client{Timeout: timeout}
			return client.Do(req)
		}
		resp, err := fetch(30 * time.Second)
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			time.Sleep(5 * time.Second)
			resp, err = fetch(90 * time.Second)
		}
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			fmt.Fprintln(os.Stderr, "arxiv_watch", cat, ":", err)
			continue
		}
		parsed.Entries = nil
		dec := xml.NewDecoder(resp.Body)
		dec.Strict = false
		dec.Entity = xml.HTMLEntity
		derr := dec.Decode(&parsed)
		resp.Body.Close()
		if derr != nil {
			fmt.Fprintln(os.Stderr, "arxiv_watch", cat, "parse:", derr)
			continue
		}
		for _, e := range parsed.Entries {
			scanned++
			text := e.Title + " " + e.Summary
			var hit string
			for _, o := range orgs {
				if strings.Contains(text, o) {
					hit = o
					break
				}
			}
			if hit == "" || e.ID == "" {
				continue
			}
			title := strings.Join(strings.Fields(e.Title), " ")
			r, ierr := db.Exec(`INSERT INTO ai_intel_candidates (kind, name, vendor, url, source, source_id)
				VALUES ('paper', $1, $2, $3, 'arxiv', $4)
				ON CONFLICT (source_id) DO NOTHING`,
				trunc(title, 300), hit, e.ID, "arxiv-"+e.ID)
			if ierr != nil {
				// Say what failed. The original silent continue hid a CHECK
				// constraint that rejected kind='paper' outright: every insert
				// failed identically from the day this stage shipped, the log
				// printed papers_added=0 ok=true, and nothing distinguished
				// "no papers matched" from "every insert bounced" until
				// someone asked where the papers were (2026-08-10). One line
				// per failed insert makes that class of failure loud on day
				// one; the constraint now includes 'paper'.
				fmt.Fprintln(os.Stderr, "arxiv_watch insert:", ierr)
				continue
			}
			if n, _ := r.RowsAffected(); n > 0 {
				added++
			}
		}
		time.Sleep(3 * time.Second) // arXiv API courtesy delay
		}
	}
	fmt.Printf("arxiv_watch: papers_added=%d scanned=%d ok=true\n", added, scanned)
	return nil
}
