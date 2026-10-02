package main

import "testing"

// The generic names bridge row 357 listed must not be match keys; coined and
// vendor-bearing names must.
func TestCveDistinctive(t *testing.T) {
	no := []string{"remote", "server", "check", "platform", "directory", "intelligence", "plugin", "public", "knowledge", "Valid",
		"Schema", "Knowledge Base", "Offers", "Roster", "Linear", "mcp-server", "mcp-api", "mcp-proxy", "web-search", "ai-toolkit",
		"company-search", "control-plane", "knowledge-base", "servers", "registry", "continue", "swarm", "inspector", "Web Search Server", "AI Agent Tools"}
	yes := []string{"AnythingLLM", "LlamaIndex", "GPT4All", "Acme Knowledge Base", "acme-search", "Stripe Payments MCP", "Open WebUI", "Semantic Kernel"}
	for _, n := range no {
		if cveDistinctive(n) {
			t.Errorf("cveDistinctive(%q) = true, want false", n)
		}
	}
	for _, n := range yes {
		if !cveDistinctive(n) {
			t.Errorf("cveDistinctive(%q) = false, want true", n)
		}
	}
}
