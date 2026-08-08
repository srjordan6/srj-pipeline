package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
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
		url:      "https://x0.at/SdIJ.png",
		sha256:   "52ba94710b254d167e84cf43e783859d287c002db7933b0ca04736bbcde5bc6b",
	},
}

// loadRemoteCovers downloads each cover, verifies its hash, and merges it
// into faviconFiles so runFavicons pushes it with the rest. Never fails the
// stage: a dead link or bad byte just means this run skips that cover.
func loadRemoteCovers() {
	client := &http.Client{Timeout: 60 * time.Second}
	for _, c := range coverSources {
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
