package main

import (
	"reflect"
	"testing"
)

func TestSectionNameWords(t *testing.T) {
	cases := map[string][]string{
		"Cloud Provider Deals":                        {"provider", "cloud", "deals"},
		"IQVIA's Agentic AI Platform":                 {"platform", "agentic", "iqvia"},
		"510(k), De Novo and PMA for AI Devices":      {"devices", "novo", "510", "pma"},
		"Life Sciences":                               nil,
		"When Trial AI Needs FDA Credibility Evidence": {"credibility", "evidence", "trial", "fda"},
	}
	for in, want := range cases {
		if got := sectionNameWords(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
}

func TestSectionQueries(t *testing.T) {
	if got := sectionQueries([]string{"provider", "cloud", "deals"}); !reflect.DeepEqual(got, []string{"provider & cloud & deals", "provider & cloud"}) {
		t.Errorf("got %v", got)
	}
	if got := sectionQueries(nil); got != nil {
		t.Errorf("empty name should give no query, got %v", got)
	}
}

func TestSectionHasWord(t *testing.T) {
	if !sectionHasWord("tools such as IQVIA, and others", "IQVIA") {
		t.Error("missed a whole word")
	}
	if sectionHasWord("the Boxer protocol", "Box") {
		t.Error("matched inside a word")
	}
	if !sectionHasWord("Boxer and Box", "Box") {
		t.Error("missed the second, whole occurrence")
	}
}

func TestSectionTermsIn(t *testing.T) {
	terms := []sectionGlossTerm{
		{"Retrieval-Augmented Generation (RAG)", "rag", "line"},
		{"Large Language Model (LLM)", "llm", "line"},
		{"Agent", "agent", "line"},
	}
	got := sectionTermsIn("Teams use RAG with a large language model.", terms, 10)
	if len(got) != 2 || got[0]["path"] != "/ai-glossary/rag/" || got[1]["path"] != "/ai-glossary/llm/" {
		t.Errorf("got %v", got)
	}
	// An acronym matches only in its own case: "rag" in lower case is a word.
	if got := sectionTermsIn("a rag and a bone", terms[:1], 10); len(got) != 0 {
		t.Errorf("lower-case acronym matched: %v", got)
	}
}
