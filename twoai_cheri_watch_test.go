package main

import (
	"strings"
	"testing"
)

func TestCheriMatch(t *testing.T) {
	keep := []string{
		"CHERI Alliance welcomes new members",
		"CHERIoT RTOS 1.0 released",
		"RISC-V ratifies the CHERI extension",
		"Codasip ships a CHERI-enabled RISC-V core",
		"Digital Security by Design: the next phase of funding",
		"Arm Morello board evaluation results",
		"Morello prototype shows memory safety at scale",
		"A study of capability hardware for C and C++",
	}
	for _, s := range keep {
		if cheriMatch(s) == "" {
			t.Errorf("should keep %q", s)
		}
	}
	drop := []string{
		"Cheri Smith named chief executive",
		"Tom Morello announces world tour",
		"Morello wins the Italian cup",
		"RISC-V summit agenda announced",
		"Cherish every release",
		"Model capability evaluation for frontier systems",
	}
	for _, s := range drop {
		if m := cheriMatch(s); m != "" {
			t.Errorf("should drop %q, matched %q", s, m)
		}
	}
}

func TestCheriFactMatch(t *testing.T) {
	if cheriFactMatch("CHERI: memory safety in hardware", "Anything at all") != "topic" {
		t.Error("a fact filed under the section heading belongs")
	}
	if cheriFactMatch("", "CHERI extensions exist for 64-bit MIPS") == "" {
		t.Error("an untopiced fact naming CHERI belongs")
	}
	if cheriFactMatch("Agent identity", "CHERI is mentioned here") != "" {
		t.Error("a fact under another topic stays in the general block")
	}
	if cheriFactMatch("", "Least privilege limits the blast radius") != "" {
		t.Error("an untopiced fact without CHERI stays in the general block")
	}
}

func TestCheriReleaseTitle(t *testing.T) {
	if got := cheriReleaseTitle("CHERIoT RTOS", "v1.2"); got != "CHERIoT RTOS: v1.2" {
		t.Errorf("got %q", got)
	}
	if got := cheriReleaseTitle("CHERIoT RTOS", "CHERIoT RTOS v1.2"); got != "CHERIoT RTOS v1.2" {
		t.Errorf("got %q", got)
	}
	if got := cheriReleaseTitle("", "Alliance news"); got != "Alliance news" {
		t.Errorf("got %q", got)
	}
}

func TestCheriParseGitHubAtom(t *testing.T) {
	atom := `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>tag:github.com,2008:Repository/1/v0.9.5</id>
    <updated>2025-06-03T10:00:00Z</updated>
    <link rel="alternate" type="text/html" href="https://github.com/riscv/riscv-cheri/releases/tag/v0.9.5"/>
    <title>v0.9.5</title>
    <content type="html">&lt;p&gt;Release notes&lt;/p&gt;</content>
  </entry>
</feed>`
	items, err := hwParseFeed([]byte(atom))
	if err != nil || len(items) != 1 {
		t.Fatalf("parse: %v, %d items", err, len(items))
	}
	it := items[0]
	if it.URL() != "https://github.com/riscv/riscv-cheri/releases/tag/v0.9.5" {
		t.Errorf("url %q", it.URL())
	}
	if d := twoaiFeedDate(it.Published, it.Updated, it.PubDate, it.Date); d != "2025-06-03" {
		t.Errorf("date %q", d)
	}
}

func TestCheriBridgeBody(t *testing.T) {
	body := cheriBridgeBody("2026-10-06", "the first run", 2,
		[]hwCount{{"CHERI Alliance", 1}, {"The Register (coverage)", 1}},
		[]hwRow{
			{kind: "alliance", title: "New members join", source: "CHERI Alliance", date: "2026-10-05", url: "https://cheri-alliance.org/x/"},
			{kind: "coverage", title: "Morello results", source: "The Register (coverage)", url: "https://example.com/y"},
		})
	for _, want := range []string{"CHERI watch, 2026-10-06: 2 new items", "[coverage] Morello results", "undated", "twoai_cheri_watch"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "\u2014") {
		t.Error("no em-dashes in a bridge body")
	}
}
