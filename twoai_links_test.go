package main

import "testing"

func TestCheriLinkParts(t *testing.T) {
	targets := []cheriLinkTarget{
		{"SRI International", "/companies/d7018517/"},
		{"Peter G. Neumann", "/people/50e8f6d9/"},
		{"Arm", "/companies/arm/"},
	}
	linked := map[string]bool{}
	parts := cheriLinkParts("Dr Peter G. Neumann, SRI International principal investigator, Armv8-A.", targets, linked)
	var got []string
	for _, p := range parts {
		if p["href"] != "" {
			got = append(got, p["text"]+"="+p["href"])
		}
	}
	if len(got) != 2 || got[0] != "Peter G. Neumann=/people/50e8f6d9/" || got[1] != "SRI International=/companies/d7018517/" {
		t.Fatalf("links: %v (Armv8-A must not link Arm)", got)
	}
	whole := ""
	for _, p := range parts {
		whole += p["text"]
	}
	if whole != "Dr Peter G. Neumann, SRI International principal investigator, Armv8-A." {
		t.Fatalf("parts do not rebuild the text: %q", whole)
	}
	// A target is linked at its first mention in the section only.
	again := cheriLinkParts("SRI International again.", targets, linked)
	if len(again) != 1 {
		t.Fatalf("second mention linked: %v", again)
	}
}

func TestGlossaryNamed(t *testing.T) {
	glossTermsOnce.Do(func() {})
	prev := glossTermsCached
	glossTermsCached = []glossTerm{
		{slug: "risks-digest", term: "RISKS Digest", names: []string{"RISKS Digest", "ACM RISKS Forum", "RISKS Forum"}},
		{slug: "multics", term: "Multics", names: []string{"Multics"}},
		{slug: "forum", term: "Forum", names: []string{"Forums"}},
	}
	defer func() { glossTermsCached = prev }()
	got := twoaiGlossaryNamed(nil, "He designed the file system of Multics and founded the ACM RISKS Forum; multicsx is not it.", 8)
	if len(got) != 2 || got[0].Name != "Multics" || got[1].Name != "RISKS Digest" || got[1].Path != "/ai-glossary/risks-digest/" {
		t.Fatalf("got %+v", got)
	}
}
