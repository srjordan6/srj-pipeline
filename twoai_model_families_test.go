package main

import "testing"

func TestFamKey(t *testing.T) {
	cases := map[string]string{
		"openai/gpt-6.1-sol-pro":          "openai/gpt",
		"anthropic/claude-sonnet-5.5":     "anthropic/claude",
		"z-ai/glm-5.3-prime":              "z-ai/glm",
		"qwen/qwen3.8-max-prime":          "qwen/qwen",
		"xiaomi/mimo-v2.6-pro":            "xiaomi/mimo",
		"unbiased/pareto-26.10-preview":   "unbiased/pareto",
		"unbiased/pareto":                 "unbiased/pareto",
		"cohere/command-a-plus":           "cohere/command",
		"upstage/solar-mini4":             "upstage/solar",
		"apodex/apodex-1.1-mini:free":     "",
		"~anthropic/claude-sonnet-latest": "",
		"openai/gpt-chat-latest":          "",
	}
	for id, want := range cases {
		d, l := famKey(id)
		got := ""
		if d != "" {
			got = d + "/" + l
		}
		if got != want {
			t.Errorf("famKey(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestFamNames(t *testing.T) {
	if got := famLineName("OpenAI: GPT-6.1 Sol"); got != "GPT" {
		t.Errorf("line of GPT-6.1 Sol = %q", got)
	}
	if got := famLineName("Xiaomi: MiMo-V2.6-Pro"); got != "MiMo" {
		t.Errorf("line of MiMo-V2.6-Pro = %q", got)
	}
	if got := famLineName("Qwen: Qwen3.8 Max Prime"); got != "Qwen" {
		t.Errorf("line of Qwen3.8 = %q", got)
	}
	if got := famLineName("Pareto 26.10 Preview"); got != "Pareto" {
		t.Errorf("line of Pareto = %q", got)
	}
	if got := famDevName("Pareto 26.10 Preview", "unbiased"); got != "Unbiased" {
		t.Errorf("developer without a prefix = %q", got)
	}
	g := famGroup{DevName: "Qwen", LineName: "Qwen"}
	if g.displayName() != "Qwen" {
		t.Errorf("same developer and line should not repeat: %q", g.displayName())
	}
	g = famGroup{DevName: "Anthropic", LineName: "Claude"}
	if g.displayName() != "Anthropic Claude" {
		t.Errorf("display name = %q", g.displayName())
	}
}

func TestFamParams(t *testing.T) {
	cases := map[string]string{
		"qwen/qwen3.8-27b":          "27B",
		"qwen/qwen3.8-2.4t-a95b":    "2.4T total, 95B active",
		"qwen/qwen3.6-35b-a3b":      "35B total, 3B active",
		"anthropic/claude-opus-5.5": "",
		"z-ai/glm-4.5-air":          "",
	}
	for id, want := range cases {
		if got := famParams(id); got != want {
			t.Errorf("famParams(%q) = %q, want %q", id, got, want)
		}
	}
}
