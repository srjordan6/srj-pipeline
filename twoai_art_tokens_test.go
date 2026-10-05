package main

import "testing"

func TestTwoaiArtTokenize(t *testing.T) {
	in := "The site tracks 850 AI tools and 688 glossary terms, 229 compliance and regulation pages and 2526 active MCP servers, 263 of them for SQL databases."
	want := "The site tracks {{art_tools}} AI tools and {{glossary_terms}} glossary terms, {{art_compliance_pages}} compliance and regulation pages and {{art_mcp_total}} active MCP servers, {{art_db_mcp}} of them for SQL databases."
	if got := twoaiArtTokenize(in); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if got := twoaiArtTokenize("In 2024 the FDA cleared 950 devices."); got != "In 2024 the FDA cleared 950 devices." {
		t.Errorf("an outside figure changed: %q", got)
	}
}
