package main

// intel_gnews: resolve Google News RSS redirect tokens to the real publisher
// URL. Added Aug 2 2026 after theworldofai reported that every row in
// ai_intel_candidates carries an opaque news.google.com/rss/articles/CBMi...
// link rather than a publisher link, which makes the table unusable for any
// reader-facing page.
//
// The token is NOT decodable on its own. It base64-decodes to a protobuf whose
// payload is another opaque id (AU_yqL...), and Google resolves that id only
// through a signed batchexecute call. The signature and timestamp are minted
// per request and served in the article page's HTML as data-n-a-sg and
// data-n-a-ts, so the flow is: GET the article page, scrape those two values,
// POST them back with the token.
//
// Verified working Aug 2 2026 against live rows:
//   CBMiU0FVX3lxTE5hOTZuRXV2d1It...  ->  https://eu.36kr.com/en/p/3922275681938817
//   CBMinAFBVV95cUxOWGRibGZTa3dm...  ->  https://bioengineer.org/kaist-ai-generates-...
//
// This is a scraped, unofficial interface. It will break when Google changes
// the page markup or the rpc id, so every failure path returns the original
// URL rather than an error: a Google link is worse than a publisher link but
// far better than a dropped row.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const gnewsUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

var (
	gnSigRe = regexp.MustCompile(`data-n-a-sg="([^"]+)"`)
	gnTsRe  = regexp.MustCompile(`data-n-a-ts="([^"]+)"`)
	gnResRe = regexp.MustCompile(`garturlres\\",\\"(.*?)\\"`)
	gnCli   = &http.Client{Timeout: 30 * time.Second}
)

// isGoogleNewsURL reports whether u is one of the opaque RSS redirect links.
func isGoogleNewsURL(u string) bool {
	return strings.Contains(u, "news.google.com/rss/articles/")
}

// resolveGoogleNews returns the publisher URL behind a Google News RSS link.
// On any failure it returns the input unchanged, so callers can assign the
// result unconditionally.
func resolveGoogleNews(gurl string) string {
	if !isGoogleNewsURL(gurl) {
		return gurl
	}
	parts := strings.SplitN(gurl, "/articles/", 2)
	if len(parts) != 2 {
		return gurl
	}
	token := strings.SplitN(parts[1], "?", 2)[0]

	page, err := gnFetch("GET", gurl, nil, "")
	if err != nil {
		return gurl
	}
	sg := gnSigRe.FindStringSubmatch(page)
	ts := gnTsRe.FindStringSubmatch(page)
	if sg == nil || ts == nil {
		return gurl
	}
	tsNum, err := strconv.ParseInt(ts[1], 10, 64)
	if err != nil {
		return gurl
	}

	// The inner request Google's own UI sends. The long literal is its
	// standard locale and feature block; it is opaque but stable, and the
	// call fails without it.
	inner, err := json.Marshal([]any{
		"garturlreq",
		[]any{
			[]any{"X", "X", []any{"X", "X"}, nil, nil, 1, 1, "US:en", nil, 1,
				nil, nil, nil, nil, nil, 0, 1},
			"X", "X", 1, []any{1, 1, 1}, 1, 1, nil, 0, 0, nil, 0,
		},
		token, tsNum, sg[1],
	})
	if err != nil {
		return gurl
	}
	envelope, err := json.Marshal([]any{[]any{[]any{"Fbv4je", string(inner), nil, "generic"}}})
	if err != nil {
		return gurl
	}
	form := url.Values{"f.req": {string(envelope)}}.Encode()

	body, err := gnFetch("POST",
		"https://news.google.com/_/DotsSplashUi/data/batchexecute",
		strings.NewReader(form),
		"application/x-www-form-urlencoded;charset=UTF-8")
	if err != nil {
		return gurl
	}
	m := gnResRe.FindStringSubmatch(body)
	if m == nil {
		return gurl
	}
	// The response is JSON-escaped twice; unquote turns \u003d and friends
	// back into real characters.
	out, err := strconv.Unquote(`"` + m[1] + `"`)
	if err != nil || !strings.HasPrefix(out, "http") || isGoogleNewsURL(out) {
		return gurl
	}
	return out
}

func gnFetch(method, u string, body io.Reader, ctype string) (string, error) {
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", gnewsUA)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := gnCli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("gnews %s %d", method, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	return string(b), err
}

// publisherFromURL gives a display source for an article once the real URL is
// known: the registrable-ish host with www and a leading m. stripped. Use it
// to replace the misleading "(coverage)" vendor labels, which name the SEARCH
// QUERY that found the story rather than the company the story is about, and
// which is why a Cohere story currently reads "Aleph Alpha (coverage)".
func publisherFromURL(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return ""
	}
	h := strings.ToLower(p.Host)
	h = strings.TrimPrefix(h, "www.")
	h = strings.TrimPrefix(h, "m.")
	return h
}
