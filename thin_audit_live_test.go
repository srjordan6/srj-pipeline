package main

import (
	"net/http"
	"os"
	"testing"
	"time"
)

func TestThinSitemapLive(t *testing.T) {
	if os.Getenv("THIN_LIVE") != "1" {
		t.Skip("set THIN_LIVE=1")
	}
	c := &http.Client{Timeout: 30 * time.Second}
	sms := thinSitemapLocs(c, "https://theworldofai.org/sitemap-index.xml")
	n := 0
	for _, sm := range sms {
		n += len(thinSitemapLocs(c, sm))
	}
	t.Logf("sitemaps=%d urls=%d", len(sms), n)
	if len(sms) == 0 || n < 100 {
		t.Errorf("sitemap walk failed")
	}
}
