package main

import "testing"

func TestFamBenchMatch(t *testing.T) {
	cases := []struct {
		line, dev, system, detail string
		want                      bool
	}{
		{"Claude", "Anthropic", "claude-opus-4-6-high", "rank 2, anthropic, 77636 votes", true},
		{"GPT", "OpenAI", "gpt-5-5", "", true},
		{"Gemini", "Google", "gemini-4-argon-high", "", true},
		{"Command", "Cohere", "Claude Code + GBOX MCP", "", false},
		{"Command", "Cohere", "command-a-03-2025", "rank 40, cohere", true},
		{"Command", "Cohere", "Command line agent", "", false},
		{"Seed", "ByteDance Seed", "seed-2.0-pro", "bytedance", false},
		{"Seed", "ByteDance Seed", "seed-2.0-pro", "ByteDance Seed", true},
		{"DeepSeek", "DeepSeek", "Deepseek v3.2", "", true},
		{"Grok", "SpaceXAI", "grokking agent", "", false},
		{"Claude", "Anthropic", "claude_opus_4_6_inspect", "", true},
	}
	for _, c := range cases {
		if got := famBenchMatch(c.line, c.dev, c.system, c.detail); got != c.want {
			t.Errorf("famBenchMatch(%q, %q, %q, %q) = %v, want %v", c.line, c.dev, c.system, c.detail, got, c.want)
		}
	}
}

func TestExcelDate(t *testing.T) {
	if got := excelDate("46235"); got != "2026-08-01" {
		t.Errorf("excelDate(46235) = %q", got)
	}
	if got := excelDate("Model"); got != "" {
		t.Errorf("a text cell gave %q", got)
	}
}
