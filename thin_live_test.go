package main

import (
	"net/http"
	"os"
	"testing"
	"time"
)

// Live registry and site reads; skipped unless THIN_LIVE=1 so CI and the
// cron's build never touch the network.
func TestThinFillersLive(t *testing.T) {
	if os.Getenv("THIN_LIVE") != "1" {
		t.Skip("set THIN_LIVE=1")
	}
	c := &http.Client{Timeout: 20 * time.Second}
	for _, id := range []string{"pretrip-mcp", "@modelcontextprotocol/sdk"} {
		l, lic, repo, pub, dl, err := thinNpmFacts(c, id)
		t.Logf("npm %s: latest=%s lic=%s repo=%s pub=%s dl=%v err=%v", id, l, lic, repo, pub, dl, err)
		if err != nil || l == "" {
			t.Errorf("npm %s failed", id)
		}
	}
	l, lic, repo, pub, err := thinPyPIFacts(c, "mcp")
	t.Logf("pypi mcp: latest=%s lic=%s repo=%s pub=%s err=%v", l, lic, repo, pub, err)
	if err != nil || l == "" {
		t.Errorf("pypi failed")
	}
	for _, site := range []string{"https://www.anthropic.com/", "https://huggingface.co/", "https://www.palantir.com/", "https://mistral.ai/"} {
		hq, fy, src, err := thinOrgFacts(c, site)
		t.Logf("org %s: hq=%q founded=%d src=%s err=%v", site, hq, fy, src, err)
	}
}
