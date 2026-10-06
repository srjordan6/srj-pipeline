package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The pages are copies of seed.bytedance.com/en/models and /en/research as
// fetched on 2026-10-05. The model replies below are what the extraction
// prompt asks for, written by hand, with the mistakes a model makes added on
// purpose: a name the page does not carry, another developer's model from a
// comparison table, and dates the page does not give.

func famSeedPage(t *testing.T, file, u string) (string, string, []famSrcLink) {
	raw, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Skip("testdata missing:", err)
	}
	return famSrcText(raw, u)
}

func TestFamSrcTextSeed(t *testing.T) {
	title, text, links := famSeedPage(t, "family_seed_models.html", "https://seed.bytedance.com/en/models")
	if title != "Seed Models" {
		t.Errorf("title = %q", title)
	}
	for _, want := range []string{"Seed2.1", "Seedance 2.5", "Seedream 5.0 Pro", "SeedRealtime", "Seed Audio 1.0", "Seed GR-3", "Protenix"} {
		if !famSrcVerbatim(text, want) {
			t.Errorf("text lacks %q", want)
		}
	}
	if strings.Contains(text, "<") && strings.Contains(text, "</") {
		t.Errorf("markup left in the text")
	}
	found := false
	for _, l := range links {
		if l.U == "https://seed.bytedance.com/en/seed2_1" {
			found = true
		}
	}
	if !found {
		t.Errorf("link to /en/seed2_1 not found among %d links", len(links))
	}
}

func TestFamSrcVerifySeedLines(t *testing.T) {
	title, text, links := famSeedPage(t, "family_seed_models.html", "https://seed.bytedance.com/en/models")
	reply := `{"lines":[
		{"name":"Seed2.1","modality":"multimodal","date_text":""},
		{"name":"Seedance 2.5","modality":"video","date_text":"July 31, 2026"},
		{"name":"Seedream 5.0 Pro","modality":"image","date_text":""},
		{"name":"SeedRealtime","modality":"audio","date_text":""},
		{"name":"Seed Audio 1.0","modality":"audio","date_text":""},
		{"name":"Seed GR-RL","modality":"robotics","date_text":""},
		{"name":"Seed GR-3","modality":"robotics","date_text":""},
		{"name":"Protenix","modality":"science","date_text":""},
		{"name":"Seed3.0","modality":"text","date_text":""},
		{"name":"Claude Opus 4.7","modality":"text","date_text":""}],
		"access":[{"channel":"Get API","kind":"first-party API"},{"channel":"Amazon Bedrock","kind":"cloud marketplace"}],
		"announcements":[]}`
	var ex famSrcExtract
	if !famSrcParseJSON("Here you go:\n"+reply, &ex) {
		t.Fatal("reply did not parse")
	}
	own := map[string]bool{"seed": true, "bytedance": true}
	others := map[string]bool{"claude": true, "gpt": true, "gemini": true, "seed": true}
	facts := famSrcVerify(ex, text, title, "https://seed.bytedance.com/en/models", links, own, others)
	got := map[string]famSrcFact{}
	for _, f := range facts {
		got[f.Kind+":"+f.Name] = f
		t.Logf("%s %q modality=%q announced=%q url=%s", f.Kind, f.Name, f.Detail, f.Announced, f.URL)
	}
	for _, want := range []string{"Seed2.1", "Seedance 2.5", "Seedream 5.0 Pro", "SeedRealtime", "Seed Audio 1.0", "Seed GR-RL", "Seed GR-3", "Protenix"} {
		if _, ok := got["line:"+want]; !ok {
			t.Errorf("line %q dropped", want)
		}
	}
	if _, ok := got["line:Seed3.0"]; ok {
		t.Errorf("Seed3.0 is not on the page and was kept")
	}
	if _, ok := got["line:Claude Opus 4.7"]; ok {
		t.Errorf("another developer's model was kept")
	}
	if f := got["line:Seedance 2.5"]; f.Announced != "" {
		t.Errorf("a date the page does not give was kept: %q", f.Announced)
	}
	if f := got["line:Seed2.1"]; f.URL != "https://seed.bytedance.com/en/seed2_1" {
		t.Errorf("Seed2.1 url = %q", f.URL)
	}
	if _, ok := got["access:Get API"]; ok {
		t.Errorf("a button label was kept as an access channel")
	}
	if _, ok := got["access:Amazon Bedrock"]; ok {
		t.Errorf("a channel the page does not name was kept")
	}
}

func TestFamSrcVerifySeedAnnouncements(t *testing.T) {
	title, text, links := famSeedPage(t, "family_seed_research.html", "https://seed.bytedance.com/en/research")
	reply := `{"lines":[{"name":"Seedance 2.5","modality":"video","date_text":"Jul 31, 2026"}],"access":[],
		"announcements":[
		{"title":"SeedRealtime Audio-Visual Full-Duplex LLM Released: Toward Omni-Modal Natural Interaction","date_text":"Aug 5, 2026"},
		{"title":"One-take Creation, Flexible Referencing: Introducing Seedance 2.5","date_text":"Jul 31, 2026"},
		{"title":"Beyond Generation, It Understands Design | Introducing Seedream 5.0 Pro","date_text":"Jul 8, 2026"},
		{"title":"Seed3.0 Officially Released","date_text":"Sep 1, 2026"},
		{"title":"From Speech to Audio Creation | Introducing the Seed Audio 1.0 Audio Creation Model","date_text":"Jan 27, 2026"}]}`
	var ex famSrcExtract
	if !famSrcParseJSON(reply, &ex) {
		t.Fatal("reply did not parse")
	}
	facts := famSrcVerify(ex, text, title, "https://seed.bytedance.com/en/research", links, map[string]bool{"seed": true}, map[string]bool{})
	got := map[string]famSrcFact{}
	for _, f := range facts {
		got[f.Kind+":"+f.Name] = f
		t.Logf("%s %q announced=%q url=%s", f.Kind, f.Name, f.Announced, f.URL)
	}
	if f := got["announcement:One-take Creation, Flexible Referencing: Introducing Seedance 2.5"]; f.Announced != "2026-07-31" ||
		!strings.Contains(f.URL, "/en/blog/one-take-creation-flexible-referencing-introducing-seedance-2-5") {
		t.Errorf("Seedance 2.5 announcement = %+v", f)
	}
	if f := got["announcement:SeedRealtime Audio-Visual Full-Duplex LLM Released: Toward Omni-Modal Natural Interaction"]; f.Announced != "2026-08-05" {
		t.Errorf("SeedRealtime announcement = %+v", f)
	}
	if _, ok := got["announcement:Seed3.0 Officially Released"]; ok {
		t.Errorf("an announcement the page does not carry was kept")
	}
	if _, ok := got["announcement:From Speech to Audio Creation | Introducing the Seed Audio 1.0 Audio Creation Model"]; ok {
		t.Errorf("an announcement with a date from elsewhere on the page was kept")
	}
	if f := got["line:Seedance 2.5"]; f.Announced != "2026-07-31" {
		t.Errorf("Seedance 2.5 line date = %q", f.Announced)
	}
}

func TestFamSrcDate(t *testing.T) {
	cases := map[string]string{
		"2026-06-23": "2026-06-23", "Aug 5, 2026": "2026-08-05", "Jul 31, 2026": "2026-07-31", "July 31st, 2026": "2026-07-31",
		"31 July 2026": "2026-07-31", "Sept. 4, 2025": "2025-09-04", "June 2026": "2026-06", "2025": "2025",
		"2026年6月23日": "2026-06-23", "2099-01-01": "", "yesterday": "", "": "",
	}
	for in, want := range cases {
		if got := famSrcDate(in); got != want {
			t.Errorf("famSrcDate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFamSrcCheckExplainer(t *testing.T) {
	_, text, _ := famSeedPage(t, "family_seed_models.html", "https://seed.bytedance.com/en/models")
	good := []string{
		"Seed2.1 is the newest model family on the ByteDance Seed models page, designed for real-world productivity, with major improvements to both general agents and code engineering. It comes in two models in different sizes, Pro and Turbo, and the page offers a model card, an API and a playground for it.",
		"Alongside it the page lists models for other media: Seedance 2.5, a new-generation video creation model built for 30-second storytelling, Seedream 5.0 Pro, a multimodal image generation model, SeedRealtime for full-duplex interaction across sound and vision, and Seed Audio 1.0 for audio creation. It also lists research models in robotics, Seed GR-RL and Seed GR-3, and Protenix, a biomolecular foundation model.",
	}
	allowed := []string{"ByteDance Seed", "Seed", "ByteDance"}
	if err := famSrcCheckExplainer(good, text, allowed); err != nil {
		t.Errorf("a draft drawn from the page was rejected: %v", err)
	}
	badNum := append([]string{}, good...)
	badNum[1] = strings.Replace(badNum[1], "30-second", "45-second", 1)
	if err := famSrcCheckExplainer(badNum, text, allowed); err == nil {
		t.Errorf("a number not in the page passed")
	}
	badName := append([]string{}, good...)
	badName[0] += " It is also sold through Amazon Bedrock."
	if err := famSrcCheckExplainer(badName, text, allowed); err == nil {
		t.Errorf("a product not in the page passed")
	}
	dash := append([]string{}, good...)
	dash[0] = strings.Replace(dash[0], ", with", " "+string(rune(0x2014))+" with", 1)
	if err := famSrcCheckExplainer(dash, text, allowed); err == nil {
		t.Errorf("an em dash passed")
	}
}

func TestFamSrcScope(t *testing.T) {
	sc := famDevSites["bytedance-seed/seed"].Scopes
	if !famSrcWithin("https://seed.bytedance.com/en/blog/x", sc) || famSrcWithin("https://seed.bytedance.com/zh/blog/x", sc) || famSrcWithin("https://www.byteplus.com/en", sc) {
		t.Errorf("scope test failed")
	}
	if famSrcKey("https://seed.bytedance.com/en/models?view_from=homepage_tab") != famSrcKey("https://seed.bytedance.com/en/models/") {
		t.Errorf("tracking query and trailing slash should not make a new page")
	}
	for k, s := range famDevSites {
		for _, u := range s.Starts {
			if !famSrcWithin(u, s.Scopes) {
				t.Errorf("%s: start %s is outside its own scopes", k, u)
			}
		}
	}
}

func TestFamAnswerSameName(t *testing.T) {
	doc := map[string]any{"name": "ByteDance Seed", "developer": "ByteDance Seed", "member_count": 6, "first_release": "2025-12-23",
		"latest_release": "2026-08-12", "latest_model": "ByteDance Seed: Seed 2.1 Turbo", "licence": "API only",
		"developer_company": map[string]string{"uid": "f52a9cea", "name": "ByteDance", "href": "/companies/f52a9cea/"}}
	got := famAnswer(doc)
	if !strings.HasPrefix(got, "ByteDance Seed is a model family from ByteDance, with 6 versions") || strings.Contains(got, "Seed's") {
		t.Errorf("answer = %q", got)
	}
	delete(doc, "developer_company")
	if got := famAnswer(doc); !strings.HasPrefix(got, "ByteDance Seed is a model family, with") {
		t.Errorf("answer without a company = %q", got)
	}
	doc["name"], doc["developer"] = "Anthropic Claude", "Anthropic"
	if got := famAnswer(doc); !strings.HasPrefix(got, "Anthropic Claude is Anthropic's model line") {
		t.Errorf("answer = %q", got)
	}
	g := &famGroup{DevName: "ByteDance Seed", LineName: "Seed", Dev: "bytedance-seed"}
	if famDevWord(g) != "ByteDance" {
		t.Errorf("famDevWord = %q", famDevWord(g))
	}
	c, ok := famFindCompany(map[string]famCompany{"bytedance": {"f52a9cea", "ByteDance"}}, g)
	if !ok || c.UID != "f52a9cea" {
		t.Errorf("ByteDance Seed should find ByteDance")
	}
}

func TestFamFAQ(t *testing.T) {
	g := &famGroup{DevName: "ByteDance Seed", LineName: "Seed", Line: "seed", Dev: "bytedance-seed", Members: []famMember{
		{ID: "bytedance-seed/seed-2-1-turbo", Name: "ByteDance Seed: Seed 2.1 Turbo", PromptPM: 0.5, CompletionPM: 2.5, Context: 262144, In: []string{"text", "image", "video"}, Out: []string{"text"}, Reasoning: true, Created: 3},
		{ID: "bytedance-seed/seed-2-1-pro", Name: "ByteDance Seed: Seed 2.1 Pro", PromptPM: 1.2, CompletionPM: 6, Context: 262144, In: []string{"text", "image"}, Out: []string{"text"}, Reasoning: true, Created: 2},
	}}
	doc := map[string]any{"name": "ByteDance Seed", "line": "Seed", "developer": "ByteDance Seed", "latest_model": "ByteDance Seed: Seed 2.1 Turbo", "latest_release": "2026-08-12",
		"input_modalities": []string{"image", "text", "video"}, "output_modalities": []string{"text"},
		"members": []map[string]any{
			{"context": int64(262144), "reasoning": true, "open_weights": false, "delisted": false},
			{"context": int64(262144), "reasoning": true, "open_weights": false, "delisted": false}},
		"hf_repos":  []map[string]any{{"id": "ByteDance-Seed/Seed-OSS-36B-Instruct", "url": "https://huggingface.co/ByteDance-Seed/Seed-OSS-36B-Instruct", "licence": "apache-2.0"}},
		"dev_lines": []map[string]any{{"name": "SeedRealtime", "announced": "2026-08-05"}},
		"providers": []map[string]any{{"provider": "Seed"}}}
	doc["licences"] = famLicences(doc["hf_repos"].([]map[string]any))
	doc["price_limits"] = famPriceLimits(g, "ByteDance Seed")
	faq := famFAQ(doc)
	b, _ := json.MarshalIndent(faq, "", " ")
	t.Log(string(b))
	t.Log(doc["price_limits"].(map[string]any)["summary"])
	if len(faq) < 4 {
		t.Fatalf("only %d questions", len(faq))
	}
	for _, f := range faq {
		if strings.Contains(f["a"], string(rune(0x2014))) {
			t.Errorf("em dash in %q", f["a"])
		}
	}
	if !strings.Contains(faq[0]["a"], "No. All 2 versions") || !strings.Contains(faq[0]["a"], "Seed-OSS-36B-Instruct") || !strings.Contains(faq[0]["a"], "Apache 2.0 licence") {
		t.Errorf("open source answer = %q", faq[0]["a"])
	}
	if !strings.Contains(faq[1]["a"], "from $0.5 to $1.2 per million input tokens") || !strings.Contains(faq[1]["a"], "The cheapest is Seed 2.1 Turbo") {
		t.Errorf("cost answer = %q", faq[1]["a"])
	}
}
