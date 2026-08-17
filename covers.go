package main

import (
	"archive/zip"
	"bytes"
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
		repoPath: "covers/the-ai-lawyer-cover.jpg",
		url:      "https://x0.at/HEuD.jpg",
		sha256:   "b0d3aaf519e726c116e4e36260e9654a08637304cd7b060b4afae6f1b67729bc",
	},
	{
		repoPath: "briefings/AI_IT_Security_Implementation_Strategy_Executive_Briefing.pdf",
		url:      "https://x0.at/1H35.pdf",
		sha256:   "fb18956b874943e9ece9ecb3fa5d1e57adf661099b3bcfceb2b4dc72b7f9d4d9",
	},
	{
		repoPath: "images/insights/ai-governance-update-aug-2026.png",
		url:      "https://x0.at/uNzz.png",
		sha256:   "8e3021564978593ff2d350df86c833f5caee22c1f6c69bc6447feefdbea7065b",
	},
}

// Zip-delivered asset sets. Same trust model as coverSources, one URL and
// one SHA-256 pin per archive, but the payload is a zip whose entries all
// land under repoPrefix.
//
// EMPTY ON PURPOSE, Aug 13 2026. The first use was the 134 Book 06 figure
// previews, and it proved this delivery path does not exist: the pipeline's
// GitHub token has no Contents write on srjordan6/srj-site, so the very
// first PUT returned 403 and nothing shipped. The failure had been latent
// for weeks because putToRepo compares blob SHAs and never writes a file
// already in the repo, so the token was never exercised.
//
// The previews went to the srj-assets R2 bucket instead, which is where
// Volume V keeps its own previews and where Book 06's full-size originals
// already were, and they serve at the same URL through the Worker's asset
// proxy. Repo delivery would also have meant one commit per file, which is
// one Cloudflare build per file.
//
// Leave this empty unless the token gains that scope. If it ever does, the
// right shape is still a single tree commit, not 134 contents-API calls.
var zipSources = []struct {
	repoPrefix string // repo path prefix each entry lands under, no leading slash
	url        string
	sha256     string
}{}

func loadZipAssets() {
	for _, z := range zipSources {
		resp, err := http.Get(z.url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "zip asset", z.url, ":", err)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, "zip asset", z.url, "read:", err)
			continue
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != z.sha256 {
			fmt.Fprintln(os.Stderr, "zip asset", z.url, ": sha mismatch, refusing")
			continue
		}
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			fmt.Fprintln(os.Stderr, "zip asset", z.url, ":", err)
			continue
		}
		n := 0
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				continue
			}
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				continue
			}
			faviconFiles[z.repoPrefix+f.Name] = base64.StdEncoding.EncodeToString(b)
			n++
		}
		fmt.Println("zip assets: merged", n, "files under", z.repoPrefix)
	}
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
