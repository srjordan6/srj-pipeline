package main

import (
	"fmt"
	"strings"
	"testing"
)

// The headlines below are real: incident titles and outlet titles that were
// in twoai_incidents, ai_intel_candidates, twoai_vendor_posts and
// twoai_health_papers on 2026-10-06.
func TestTwoaiEnglishLangEnglish(t *testing.T) {
	for _, s := range []string{
		"Canada resident NRI trapped with a fake ID and asked for 50,000 dollars: Ghaziabad police act against a sextortion gang",
		"Jamia Millia student among 4 held for using AI deepfakes to extort money from NRI",
		"4 arrested for ₹6.5 crore extortion racket in Ghaziabad",
		"Sisters sue Española over traffic stop they say was illegal",
		"Cooke campaign sends cease-and-desist to Congressman Van Orden over AI deepfakes",
		"KAIST develops AI to detect cerebrovascular disease risk at home using lifelog data - 동아사이언스",
		"China to Promote Innovative Development of AI Agents - 中国科技网",
		"Saudi Arabia to Host Fourth UNESCO Global Forum on the Ethics of AI in Riyadh - وكالة الأنباء السعود",
		"The trillion-dollar space war",
		"An AI-Faked Text Got Her Arrested. Now She's Fighting for Legal Reform.",
		"Why I left OpenAI",
		"Two die in crash as storm hits the coast",
		"Mistral AI launches Le Chat Pro for teams",
		"Data de la Cruz joins the board of Anthropic",
		"Lipoprotein(a) levels and the α-subunit of β-receptors were measured in 120 patients with the disease.",
		"Yann LeCun",
		"",
		// Doubled vowels, ij and accents in English headlines.
		"Beijing is forcing a mass breakup with AI lovers",
		"HIPAA BAAs are now available to Pro teams",
		"UIDAI unveils five new initiatives at Aadhaar Samvaad Kolkata to further advance trusted digital identity and good governance",
		"Introducing Aardvark: OpenAI’s agentic security researcher",
		"State of Emergency Declared in California in Anticipation of More El Niño-driven Storms",
		"David Vélez and Robin Vince join the boards of the OpenAI Foundation and OpenAI Group PBC",
		"Meet Gonçalo Gaiolas: DeepL's chief product officer on AI product vision",
		"Introducing Le Chat Enterprise",
		"Banque Internationale à Luxembourg S A : BIL welcomes the launch of Kyndryl's AI Innovation Lab in Luxembourg - marketscreener.com",
	} {
		if got := twoaiEnglishLang(s); got != "" {
			t.Errorf("twoaiEnglishLang(%q) = %q, want English", s, got)
		}
	}
}

func TestTwoaiEnglishLangForeign(t *testing.T) {
	cases := []struct{ s, want string }{
		{"कनाडा निवासी NRI को फेक आईडी से फंसाया, 50 हजार डॉलर की मांग, गाजियाबाद पुलिस का सेक्सटॉर्शन गैंग पर बड़ा एक्शन", "hi"},
		{"Nemotron-Personas-Japan: ソブリン AI のための合成データセット", "ja"},
		{"循環器学2022年の進歩", "ja"},
		{"独立研究人员如何在未对齐事件发生后调查 AI 的行为倾向", "zh"},
		{"РОЛЬ ЛИПОПРОТЕИНА(а) В СТРАТИФИКАЦИИ СЕРДЕЧНО-СОСУДИСТОГО РИСКА: СИСТЕМАТИЧЕСКИЙ ОБЗОР", "ru"},
		{"네이버 AI 검색이 광고 성장 견인", "ko"},
		{"الذكاء الاصطناعي في المملكة العربية السعودية", "ar"},
		{"L'homme qui a attaqué une garderie suisse a trouvé ses victimes sur ChatGPT", "fr"},
		{"Messerangriff auf Kinder: Täter «kaufte Rindfleisch, um zu üben»", "de"},
		{"Chinese hatte Tat zusammen mit ChatGPT geplant!", "de"},
		{"Zürich-Oerlikon: Angriff auf Kinder eines Kinderhortes mit Verletzungsfolgen", "de"},
		{"Gericht entscheidet pro Musikschaffende: GEMA setzt sich gegen SUNO durch und definiert Maßstäbe für internationales Urheberrecht", "de"},
		{"Planeó una masacre escolar con ChatGPT, intervino el FBI y no recibirá ninguna pena", "es"},
		{"Primera brecha de datos ejecutada por un agente de inteligencia artificial de forma autónoma", "es"},
		{"Cómo descubrió el FBI al chico de 15 años que organizaba con IA una masacre escolar en Quilmes y cuál era su plan", "es"},
		{"Motie van het lid Beckerman over een bevoegde instantie aanwijzen om ook deepfakeporno en andere onrechtmatige seksuele beelden offline te halen", "nl"},
		{"Wel of niet zeggen dat je in een neppornovideo zit? BN'ers worstelen ermee", "nl"},
		{"Kamerleden horen al maanden niets na deepfake pornovideo's: 'Week ophef, daarna werd het stil'", "nl"},
		{"Bekende Nederlanders doen massaal aangifte in zaak deepfakepornovideo's", "nl"},
		{"Vrouwelijke BN'ers overwegen aangifte vanwege deepfake pornovideo's", "nl"},
		{"Políticas de escalamiento responsable (RSP)", "es"},
		{"Mistral AI - KI für Deutschland", "de"},
		{"Mistral AI lève 3 milliards d’euros : derrière les modèles, la bataille pour devenir l’infrastructure souveraine de l’IA", "fr"},
		{"Por qué conviene que el razonamiento de la IA sea comprensible y fiel", "es"},
	}
	for _, c := range cases {
		if got := twoaiEnglishLang(c.s); got != c.want {
			t.Errorf("twoaiEnglishLang(%q) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestEnglishStripOutlet(t *testing.T) {
	if got := englishStripOutlet("Naver AI search spurs over half of 2025 ad growth - 디지털투데이"); got != "Naver AI search spurs over half of 2025 ad growth" {
		t.Errorf("outlet not stripped: %q", got)
	}
	// A short headline keeps its whole text: the dash is part of it.
	if got := englishStripOutlet("AI - a primer"); got != "AI - a primer" {
		t.Errorf("short headline cut: %q", got)
	}
}

func TestIncidentTitleMerge(t *testing.T) {
	hindi := "कनाडा निवासी NRI को फेक आईडी से फंसाया, 50 हजार डॉलर की मांग"
	hand := "Canada resident NRI trapped with a fake ID and asked for 50,000 dollars"
	machine := "Canada-based NRI trapped with a fake ID, 50,000 dollars demanded"
	cases := []struct {
		name               string
		exists             bool
		cur                incidentTitle
		feed, lang, en     string
		want               incidentTitle
		wantKeptBeforeCall bool
	}{
		{"new English", false, incidentTitle{}, "Woman jailed over AI texts", "", "",
			incidentTitle{Title: "Woman jailed over AI texts"}, false},
		{"new foreign, translated", false, incidentTitle{}, hindi, "hi", machine,
			incidentTitle{machine, hindi, "hi"}, false},
		{"new foreign, no translation yet", false, incidentTitle{}, hindi, "hi", "",
			incidentTitle{hindi, hindi, "hi"}, false},
		{"machine translation kept on re-harvest", true, incidentTitle{machine, hindi, "hi"}, hindi, "hi", "",
			incidentTitle{machine, hindi, "hi"}, true},
		// guid 82754709: the content project wrote the English into title by
		// hand before title_original existed. The next harvest must keep it.
		{"hand translation kept, original recorded", true, incidentTitle{Title: hand}, hindi, "hi", "",
			incidentTitle{hand, hindi, "hi"}, true},
		{"waiting row gets its translation", true, incidentTitle{hindi, hindi, "hi"}, hindi, "hi", machine,
			incidentTitle{machine, hindi, "hi"}, false},
		{"English headline edited by the publisher", true, incidentTitle{Title: "Old headline about AI"}, "New headline about AI", "", "",
			incidentTitle{Title: "New headline about AI"}, false},
		{"source changed, translation still kept", true, incidentTitle{machine, hindi, "hi"}, hindi + " अपडेट", "hi", "",
			incidentTitle{machine, hindi + " अपडेट", "hi"}, true},
	}
	for _, c := range cases {
		if got := incidentTitleKept(c.exists, c.cur, c.feed, c.lang != ""); got != c.wantKeptBeforeCall {
			t.Errorf("%s: incidentTitleKept = %v, want %v", c.name, got, c.wantKeptBeforeCall)
		}
		if got := incidentTitleMerge(c.exists, c.cur, c.feed, c.lang, c.en); got != c.want {
			t.Errorf("%s: incidentTitleMerge = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestTwoaiEnglishParse(t *testing.T) {
	src := "Primera brecha de datos ejecutada por un agente de inteligencia artificial"
	lang, en, err := twoaiEnglishParse(src, "```json\n{\"lang\":\"es\",\"english\":\"First data breach carried out by an AI agent "+emDash+" autonomously\"}\n```")
	if err != nil || lang != "es" || en != "First data breach carried out by an AI agent, autonomously" {
		t.Errorf("parse = %q %q %v", lang, en, err)
	}
	if lang, en, err := twoaiEnglishParse("Why I left OpenAI", `{"lang":"en","english":"Why I left OpenAI"}`); err != nil || lang != "en" || en != "Why I left OpenAI" {
		t.Errorf("English answer = %q %q %v", lang, en, err)
	}
	if _, _, err := twoaiEnglishParse(src, `{"lang":"es","english":"`+src+`"}`); err == nil {
		t.Error("the source handed back as its own translation was accepted")
	}
	hindi := "कनाडा निवासी NRI को फेक आईडी से फंसाया"
	if _, _, err := twoaiEnglishParse(hindi, `{"lang":"hi","english":"कनाडा निवासी NRI को फंसाया"}`); err == nil {
		t.Error("an answer still in Devanagari was accepted")
	}
	if _, _, err := twoaiEnglishParse(src, "Sorry, I cannot help with that."); err == nil {
		t.Error("a non-JSON answer was accepted")
	}
	if lang, _, err := twoaiEnglishParse(src, `{"lang":"es-AR","english":"First data breach"}`); err != nil || lang != "es" {
		t.Errorf("regional code = %q %v", lang, err)
	}
}

func fakeEnglish(s string) (string, string, error) {
	l := twoaiEnglishLang(s)
	if l == "" {
		return s, "en", nil
	}
	return "EN(" + strings.Fields(s)[0] + ")", l, nil
}

func TestTwoaiEnglishDocSiblings(t *testing.T) {
	story := map[string]any{
		"Slug":     "planeo-una-masacre-escolar-con-chatgpt",
		"Headline": "Planeó una masacre escolar con ChatGPT, intervino el FBI y no recibirá ninguna pena",
		"Summary":  "A teenager in Quilmes planned an attack with a chatbot and the FBI alerted local police.",
		"Articles": []any{
			map[string]any{"Title": "Un adolescente planificó con la Inteligencia Artificial matar a sus compañeros de colegio", "URL": "https://example.com/una-masacre-de-la-escuela"},
			map[string]any{"Title": "Teen planned school attack with ChatGPT, FBI says", "URL": "https://example.com/b"},
		},
		"Persons": []any{"Juan de la Cruz Pérez"},
	}
	out, ch := twoaiEnglishDoc(story, true, fakeEnglish)
	m := out.(map[string]any)
	if len(ch) != 2 {
		t.Fatalf("changes = %d, want 2: %+v", len(ch), ch)
	}
	if m["Headline"] != "EN(Planeó)" || m["Headline_lang"] != "es" || !strings.HasPrefix(m["Headline_original"].(string), "Planeó una") {
		t.Errorf("headline siblings wrong: %v %v %v", m["Headline"], m["Headline_lang"], m["Headline_original"])
	}
	a0 := m["Articles"].([]any)[0].(map[string]any)
	if a0["Title"] != "EN(Un)" || a0["Title_lang"] != "es" || a0["URL"] != "https://example.com/una-masacre-de-la-escuela" {
		t.Errorf("article siblings wrong: %v", a0)
	}
	if _, has := m["english_originals"]; has {
		t.Error("sibling mode wrote an english_originals map")
	}
	// Run again: nothing left to do, nothing logged twice.
	if _, ch2 := twoaiEnglishDoc(m, true, fakeEnglish); len(ch2) != 0 {
		t.Errorf("second pass changed %d strings", len(ch2))
	}
}

func TestTwoaiEnglishDocOriginalsMap(t *testing.T) {
	person := map[string]any{
		"name":         "Someone Example",
		"achievements": []any{"Fondatrice de la première école de données pour les femmes au Sénégal", "Built a national AI lab"},
		"quote":        map[string]any{"text": "Die Zukunft der Maschinen ist nicht ohne uns zu denken", "source_url": "https://example.com/zitat-von-der-konferenz"},
	}
	out, ch := twoaiEnglishDoc(person, false, fakeEnglish)
	m := out.(map[string]any)
	if len(ch) != 2 {
		t.Fatalf("changes = %d, want 2: %+v", len(ch), ch)
	}
	orig, ok := m["english_originals"].(map[string]any)
	if !ok || orig["achievements[0]"] == nil || orig["quote.text"] == nil {
		t.Fatalf("english_originals = %v", m["english_originals"])
	}
	if q := m["quote"].(map[string]any); q["text_original"] != nil {
		t.Error("content mode wrote a sibling key")
	}
	if got := fmt.Sprint(m["achievements"].([]any)[1]); got != "Built a national AI lab" {
		t.Errorf("English entry changed: %q", got)
	}
}
