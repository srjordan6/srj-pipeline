package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Remote binary assets: book covers and executive briefing PDFs. Volume V
// launched after the WordPress cutover, so its assets were never in the R2
// archive the earlier volumes load from. Rather
// than embedding image bytes in source, each cover is fetched from a staging
// URL, verified against a pinned SHA-256, and then carried by the favicons
// stage through putToRepo into srj-site public/covers/ like any other
// embedded asset. The blob-SHA check inside putToRepo keeps daily re-runs
// write-free once the file is in the repo, and a fetch failure only logs a
// warning: after the first successful run the repo copy is authoritative
// and the staging URL no longer matters.
var coverSources = []struct {
	repoPath string // path under public/ in srj-site
	url      string
	sha256   string
}{
	{
		repoPath: "covers/the-ai-it-security-audit-cover.jpg",
		url:      "https://x0.at/MI7j.jpg",
		sha256:   "97a5008b1cf98faf815aabf374884d236a88c3bd17ef28c0a6b78abf8d70fe63",
	},
	{
		repoPath: "briefings/AI_IT_Security_Audit_Executive_Briefing.pdf",
		url:      "https://x0.at/rc5K.pdf",
		sha256:   "6b25e9553eccdc69aadf167c6ee2826a7e0b62c8d970f63f11b100c93bf14a16",
	},
	{
		repoPath: "images/insights/ai-governance-update-aug-2026.png",
		url:      "https://x0.at/uNzz.png",
		sha256:   "8e3021564978593ff2d350df86c833f5caee22c1f6c69bc6447feefdbea7065b",
	},
}

// loadRemoteCovers downloads each cover, verifies its hash, and merges it
// into faviconFiles so runFavicons pushes it with the rest. Never fails the
// stage: a dead link or bad byte just means this run skips that cover.
//
// An asset already in the repo is skipped without fetching. putToRepo
// already made the push write-free once the blob matched, but the download
// still ran every time, and runFavicons is called by the daily `all` pass
// AND by hourlyCatchUp, so this was pulling several megabytes an hour from
// the staging host to produce nothing. Existence is a sufficient test
// because the only way a file reaches the repo is through this function,
// which verifies the pinned SHA-256 first.
//
// The practical effect is that each cover retires its own staging URL after
// the first successful run. That matters because the staging host is
// ephemeral: when a link eventually expires, a retired asset says nothing
// rather than warning every hour about a file that is already published.
func loadRemoteCovers() {
	client := &http.Client{Timeout: 60 * time.Second}
	for _, c := range coverSources {
		if repoHasFile("srjordan6/srj-site", "public/"+c.repoPath) {
			continue
		}
		resp, err := client.Get(c.url)
		if err != nil {
			fmt.Println("covers: fetch failed, skipping", c.repoPath, err)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			fmt.Println("covers: read failed, skipping", c.repoPath, resp.StatusCode)
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != c.sha256 {
			fmt.Println("covers: SHA-256 mismatch, skipping", c.repoPath)
			continue
		}
		faviconFiles[c.repoPath] = base64.StdEncoding.EncodeToString(data)
		fmt.Println("covers: verified", c.repoPath, len(data), "bytes")
	}
}

// repoHasFile reports whether a path exists in the repo. Any error, including
// a missing token, answers false: the caller then falls back to fetching and
// pushing, which is the behaviour this function is an optimisation over, so a
// failed check costs bandwidth rather than correctness.
func repoHasFile(repo, path string) bool {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return false
	}
	req, err := http.NewRequest("GET",
		"https://api.github.com/repos/"+repo+"/contents/"+path, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "srj-pipeline/1.0")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
