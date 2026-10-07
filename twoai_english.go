package main

// twoai_english: everything visitors see on theworldofai.org is in English.
//
// Stephen's standing rule, theworldofai bridge row 508, 2026-10-06, after
// /ai-news/incident/1722/ led with a Hindi headline from
// navbharattimes.indiatimes.com. The pipeline had a translator for incident
// headlines since 2026-09-11, but it was gated on ANTHROPIC_API_KEY, and that
// key was removed on 2026-09-17, so from that day every non-English headline
// went out untranslated: twoai_translations has no row newer than 20:12 that
// evening. The gate failed silently because the fallback was "show the
// original", which is exactly what the rule now forbids.
//
// This file is the one place that answers two questions:
//
//	twoaiEnglishLang   is this text English? A cheap, offline check, unit
//	                   tested: script first (Devanagari, kana, Hangul,
//	                   Cyrillic and so on), then for Latin script a function
//	                   word count against English and fifteen other
//	                   languages. No model call is spent on English.
//	twoaiEnglish       the English for a non-English text, through
//	                   twoaiGenerate with a stage name, cached for ever in
//	                   twoai_translations on the text's hash, so a headline
//	                   costs one call however many pages and runs show it.
//
// Where translation happens:
//
//   - at harvest, for the AI Incident Database feed (twoai_incidents.go) and
//     the daily briefing's headlines and outlet titles (publish_news), so a
//     new headline is English the day it arrives
//   - in twoai_english_sweep, a bulk stage that runs off-peak over every
//     table a visitor reads from, translates what the harvest missed, keeps
//     the original beside the English, logs each change in
//     twoai_translation_log and tells theworldofai by bridge.
//
// THE ORIGINAL IS ALWAYS KEPT. A column <field> gets <field>_original and
// <field>_lang beside it; a jsonb document gets <key>_original and <key>_lang
// beside the key (news stories, whose templates read them) or an
// english_originals map keyed on the path (content records). Nothing is lost,
// and a page can say "Translated from Hindi" beside a translated headline.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Detection
// ---------------------------------------------------------------------------

// English function words. Words that are also common function words in the
// languages below are kept here only when English uses them far more ("in",
// "on"); "a", "i", "e", "o" are left out because they are prepositions or
// conjunctions in half the languages listed.
var englishStop = englishWordSet(`the of and to in for on with is are was were as at by from that this it its be
has have had after over they their them says said will how why what who about into more than not new can could
would should his her he she we you our your an or but up out amid against which when while been being just only
also before under amid between first year years after there these those does did doing`)

// Function words per language. A word that is also an everyday English word
// is left out ("do", "no", "as", "war", "hat", "will", "die" stays for German
// only because the German texts are full of it and English rarely says it in
// a headline twice). The language code chosen here is a hint for the model;
// the model returns the code that is stored.
var englishForeignStop = map[string]map[string]bool{
	"es": englishWordSet(`el la los las del de y que en un una por con para se su sus al es lo como más pero sobre entre cuando fue ha han sin desde este esta según también muy hasta ya le les cómo qué cuál está están ser son tras ante durante`),
	"fr": englishWordSet(`le la les des du de et un une est pour dans avec sur au aux qui que qu ne pas ce cette ces par plus ses sa il elle ont été sont l d leur comme mais nous vous selon après être fait`),
	"de": englishWordSet(`der die das und ist mit für auf ein eine einen einem eines den dem des zu zum zur im nicht sich von bei wird werden auch nach aus wie über gegen durch hatte sind oder noch nur um künstliche intelligenz`),
	"nl": englishWordSet(`de het een en van voor met op niet dat die zijn wordt worden naar bij ook om aan uit als maar nog wel te je ze zich hun heeft hebben na werd geen deze dit er door tegen onder niets daarna wat`),
	"pt": englishWordSet(`o os de da das dos em na um uma que para com por não mais foi ao aos à é são pelo pela sobre entre como seu sua ser está inteligência`),
	"it": englishWordSet(`il lo la gli le di del della dei delle che è un una per con non si sono nel nella al alla da dal ha anche come più ma questo questa sul sulla intelligenza`),
	"sv": englishWordSet(`och att det som en är på för med av till den inte har om ett var jag från men eller kan ska efter också`),
	"da": englishWordSet(`og det er til på med af ikke som en den har de et fra kan vil skal efter også om`),
	"pl": englishWordSet(`w z na się nie że jest od po przez dla jak o co ale za oraz przy są który która które sztuczna`),
	"tr": englishWordSet(`ve bir bu için ile da de olarak olan çok daha gibi en ne mi ama sonra kadar yapay zeka`),
	"id": englishWordSet(`yang dan di ini itu dengan untuk dari dalam tidak akan pada ke juga oleh ada adalah atau karena kecerdasan buatan`),
	"ro": englishWordSet(`și în la cu de pe din un o care pentru este nu să mai sau ca prin fost`),
	"cs": englishWordSet(`je v na se s z že o pro jako ale jsou které který při od umělá`),
	"fi": englishWordSet(`ja ei että se tai kun myös joka mutta ovat oli tekoäly`),
	"hu": englishWordSet(`az és hogy nem egy van meg ez de csak mint már szerint mesterséges`),
	"vi": englishWordSet(`của và là các những trong được cho với không một người này đã có`),
}

// englishForeignOrder fixes the order languages are compared in, so a tie
// is broken the same way on every run.
var englishForeignOrder = []string{"es", "fr", "de", "nl", "pt", "it", "sv", "da", "pl", "tr", "id", "ro", "cs", "fi", "hu", "vi"}

func englishWordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

// The two long dashes, written as code points so no source line carries one.
var (
	emDash = string(rune(0x2014))
	enDash = string(rune(0x2013))
)

// englishStripOutlet drops a short trailing " - Outlet" or " | Outlet" from a
// headline before it is judged. Google News titles end in the outlet's name,
// often in its own script ("... at home using lifelog data - 동아사이언스"), and
// an outlet's name is a proper name that may stay as it is.
func englishStripOutlet(s string) string {
	for _, sep := range []string{" - ", " | ", " " + enDash + " ", " " + emDash + " "} {
		if i := strings.LastIndex(s, sep); i > 0 {
			tail := s[i+len(sep):]
			if utf8.RuneCountInString(tail) <= 40 && utf8.RuneCountInString(s[:i]) >= 20 {
				return s[:i]
			}
		}
	}
	return s
}

// twoaiEnglishLang returns "" when the text reads as English (or carries no
// signal at all, like a bare name), and otherwise a best-guess ISO 639-1
// code, "und" when the script is foreign but not one listed.
func twoaiEnglishLang(s string) string {
	s = strings.TrimSpace(englishStripOutlet(strings.TrimSpace(s)))
	if s == "" {
		return ""
	}
	// Script first. Greek letters in a scientific abstract (alpha, beta) and a
	// foreign outlet name after an English headline are a small share of the
	// letters; a text in another script is most of them.
	scripts := map[string]int{}
	latin, total := 0, 0
	ukr, persian := false, false
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		total++
		switch {
		case unicode.Is(unicode.Latin, r):
			latin++
		case unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r):
			scripts["ja"]++
		case unicode.Is(unicode.Han, r):
			scripts["zh"]++
		case unicode.Is(unicode.Hangul, r):
			scripts["ko"]++
		case unicode.Is(unicode.Cyrillic, r):
			scripts["ru"]++
			if strings.ContainsRune("іїєґІЇЄҐ", r) {
				ukr = true
			}
		case unicode.Is(unicode.Arabic, r):
			scripts["ar"]++
			if strings.ContainsRune("پچژگ", r) {
				persian = true
			}
		case unicode.Is(unicode.Devanagari, r):
			scripts["hi"]++
		case unicode.Is(unicode.Bengali, r):
			scripts["bn"]++
		case unicode.Is(unicode.Tamil, r):
			scripts["ta"]++
		case unicode.Is(unicode.Telugu, r):
			scripts["te"]++
		case unicode.Is(unicode.Gujarati, r):
			scripts["gu"]++
		case unicode.Is(unicode.Gurmukhi, r):
			scripts["pa"]++
		case unicode.Is(unicode.Kannada, r):
			scripts["kn"]++
		case unicode.Is(unicode.Malayalam, r):
			scripts["ml"]++
		case unicode.Is(unicode.Thai, r):
			scripts["th"]++
		case unicode.Is(unicode.Hebrew, r):
			scripts["he"]++
		case unicode.Is(unicode.Greek, r):
			scripts["el"]++
		case unicode.Is(unicode.Georgian, r):
			scripts["ka"]++
		case unicode.Is(unicode.Armenian, r):
			scripts["hy"]++
		default:
			scripts["und"]++
		}
	}
	if total == 0 {
		return ""
	}
	if non := total - latin; non >= 4 && non*10 >= total*3 {
		// Japanese is written with kanji and kana together; any kana at all
		// makes it Japanese rather than Chinese.
		if scripts["ja"] > 0 {
			return "ja"
		}
		best, n := "und", 0
		for _, k := range []string{"zh", "ko", "ru", "ar", "hi", "bn", "ta", "te", "gu", "pa", "kn", "ml", "th", "he", "el", "ka", "hy", "und"} {
			if scripts[k] > n {
				best, n = k, scripts[k]
			}
		}
		if best == "ru" && ukr {
			return "uk"
		}
		if best == "ar" && persian {
			return "fa"
		}
		return best
	}

	// Latin script: count function words.
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) })
	if len(words) < 3 {
		return ""
	}
	en := 0
	score := map[string]int{}
	marks := 0
	for _, w := range words {
		if englishStop[w] {
			en++
		}
		for _, l := range englishForeignOrder {
			if englishForeignStop[l][w] {
				score[l]++
			}
		}
		// Dutch headlines can be nearly free of function words ("Bekende
		// Nederlanders doen massaal aangifte in zaak deepfakepornovideo's"),
		// but Dutch spelling doubles vowels and writes ij where English
		// rarely does. An English false alarm ("Beijing SaaS startup") costs
		// one model call, which answers "en" and is cached.
		if len(w) >= 4 && (strings.Contains(w, "aa") || strings.Contains(w, "ij") || strings.Contains(w, "uu")) {
			score["nl"]++
		}
		for _, r := range w {
			if r > 0x7f && unicode.Is(unicode.Latin, r) {
				marks++
			}
		}
	}
	best, n := "", 0
	for _, l := range englishForeignOrder {
		if score[l] > n {
			best, n = l, score[l]
		}
	}
	if n >= 2 && n > 2*en {
		return best
	}
	// Accented letters back up a weaker word count: "Täter «kaufte
	// Rindfleisch, um zu üben»" has two German function words and two
	// umlauts. One accented name in an English headline ("Española") is not
	// enough on its own.
	if n >= 1 && n > en && marks >= 3 && marks*6 >= len(words) {
		return best
	}
	// A short title with no English function word at all, one foreign one
	// and an accent: "Políticas de escalamiento responsable (RSP)", "Mistral
	// AI - KI für Deutschland". A false alarm costs one cached model call.
	if n >= 1 && en == 0 && marks >= 1 {
		return best
	}
	return ""
}

// twoaiEnglishLangNames gives the language name for a code, for logs and the
// bridge. The site has its own copy for the visitor label (src/lib/langs.ts).
var twoaiEnglishLangNames = map[string]string{
	"nl": "Dutch", "de": "German", "fr": "French", "es": "Spanish", "it": "Italian", "pt": "Portuguese",
	"ja": "Japanese", "zh": "Chinese", "ko": "Korean", "ru": "Russian", "uk": "Ukrainian", "ar": "Arabic",
	"fa": "Persian", "hi": "Hindi", "bn": "Bengali", "ta": "Tamil", "te": "Telugu", "gu": "Gujarati",
	"pa": "Punjabi", "kn": "Kannada", "ml": "Malayalam", "mr": "Marathi", "ur": "Urdu", "sv": "Swedish",
	"da": "Danish", "no": "Norwegian", "nb": "Norwegian", "fi": "Finnish", "pl": "Polish", "tr": "Turkish",
	"he": "Hebrew", "id": "Indonesian", "ms": "Malay", "cs": "Czech", "sk": "Slovak", "el": "Greek",
	"hu": "Hungarian", "ro": "Romanian", "vi": "Vietnamese", "th": "Thai", "ca": "Catalan", "bg": "Bulgarian",
	"sr": "Serbian", "hr": "Croatian", "sl": "Slovenian", "lt": "Lithuanian", "lv": "Latvian", "et": "Estonian",
	"ka": "Georgian", "hy": "Armenian", "tl": "Filipino",
}

func twoaiEnglishLangName(code string) string {
	if n, ok := twoaiEnglishLangNames[strings.ToLower(code)]; ok {
		return n
	}
	return code
}

// ---------------------------------------------------------------------------
// Translation
// ---------------------------------------------------------------------------

const englishRules = `Translate literally and completely: do not summarise, shorten, editorialise or add facts.
Names of people, organisations, places and products stay as written when they are in the Latin alphabet; otherwise give their usual English spelling or a standard romanisation.
The quoted title of a book, film, law, bill or programme may stay in its own language, followed by an English gloss in square brackets.
Do not use em dashes or en dashes; use commas. No preamble, no markdown.`

const englishHeadlineSystem = `You identify the language of a news headline or title and render it in English for an English-language reference site.
Return ONLY a JSON object: {"lang":"<ISO 639-1 code>","english":"<the headline in English>"}.
If the text is already English, return lang "en" and the text unchanged.
` + englishRules

const englishPassageSystem = `You identify the language of a passage and render it in English for an English-language reference site.
Return ONLY a JSON object: {"lang":"<ISO 639-1 code>","english":"<the passage in English>"}.
If the passage is already English, return lang "en" and the passage unchanged.
Keep paragraph breaks as \n\n inside the JSON string.
` + englishRules

// Model calls per run, per stage. The sweep's cap is its per-run budget; the
// live stages (incident harvest, daily briefing) get their own so a feed full
// of foreign headlines cannot run away with a run.
var (
	twoaiEnglishMu    sync.Mutex
	twoaiEnglishSpent = map[string]int{}
)

func twoaiEnglishCap(stage string) int {
	env, def := "TWOAI_TRANSLATE_PER_RUN", 40
	if stage == "twoai_english_sweep" {
		env, def = "TWOAI_ENGLISH_PER_RUN", 60
	}
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(env))); err == nil && v >= 0 {
		return v
	}
	return def
}

func twoaiEnglishTake(stage string) bool {
	twoaiEnglishMu.Lock()
	defer twoaiEnglishMu.Unlock()
	if twoaiEnglishSpent[stage] >= twoaiEnglishCap(stage) {
		return false
	}
	twoaiEnglishSpent[stage]++
	return true
}

var (
	twoaiTranslationsTableOnce sync.Once
	twoaiEnglishSaid           sync.Map
)

func twoaiTranslationsTable(db *sql.DB) {
	twoaiTranslationsTableOnce.Do(func() {
		db.Exec(`CREATE TABLE IF NOT EXISTS twoai_translations (
			key text PRIMARY KEY, source text NOT NULL, lang text NOT NULL, english text NOT NULL,
			model text, created_at timestamptz NOT NULL DEFAULT now())`)
	})
}

// twoaiEnglishKey is the cache key. Headlines keep the "title:" prefix the
// incident translator has used since 2026-09-11, so its 20 cached rows still
// answer; longer passages get their own prefix.
func twoaiEnglishKey(text string) string {
	prefix := "title:"
	if utf8.RuneCountInString(text) > 300 {
		prefix = "text:"
	}
	h := sha256.Sum256([]byte(prefix + text))
	return hex.EncodeToString(h[:8])
}

// twoaiEnglishCached answers from the cache only. found is false when the text
// has never been through the model.
func twoaiEnglishCached(db *sql.DB, text string) (english, lang string, found bool) {
	twoaiTranslationsTable(db)
	if db.QueryRow(`SELECT english, lang FROM twoai_translations WHERE key=$1`, twoaiEnglishKey(text)).Scan(&english, &lang) == nil {
		return english, lang, true
	}
	return "", "", false
}

// twoaiEnglish returns the English for text and the language it was in.
// English text comes back unchanged with lang "en" and costs nothing. A
// non-English text is answered from twoai_translations or, within the stage's
// per-run cap, by one twoaiGenerate call whose answer is cached for ever.
// err is set when no English could be had; the caller then keeps what it had.
func twoaiEnglish(db *sql.DB, stage, text string) (string, string, error) {
	text = strings.TrimSpace(text)
	if text == "" || twoaiEnglishLang(text) == "" {
		return text, "en", nil
	}
	if en, lang, ok := twoaiEnglishCached(db, text); ok {
		if lang == "en" {
			return text, "en", nil
		}
		return en, lang, nil
	}
	if utf8.RuneCountInString(text) > 8000 {
		return text, "", fmt.Errorf("too long to translate (%d characters)", utf8.RuneCountInString(text))
	}
	if !twoaiEnglishTake(stage) {
		return text, "", fmt.Errorf("%s: translation cap of %d a run reached", stage, twoaiEnglishCap(stage))
	}
	system := englishHeadlineSystem
	if utf8.RuneCountInString(text) > 300 {
		system = englishPassageSystem
	}
	out, model, err := twoaiGenerate(stage, system, text)
	if err != nil {
		return text, "", err
	}
	lang, english, err := twoaiEnglishParse(text, out)
	if err != nil {
		return text, "", err
	}
	db.Exec(`INSERT INTO twoai_translations (key, source, lang, english, model) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (key) DO NOTHING`, twoaiEnglishKey(text), text, lang, english, model)
	if lang == "en" {
		return text, "en", nil
	}
	return english, lang, nil
}

// twoaiEnglishParse reads the model's JSON answer and refuses one that is not
// a translation: empty, the source handed back under a foreign code, or text
// still mostly in another script.
func twoaiEnglishParse(source, out string) (lang, english string, err error) {
	out = strings.TrimSpace(out)
	if i, j := strings.Index(out, "{"), strings.LastIndex(out, "}"); i >= 0 && j > i {
		out = out[i : j+1]
	}
	var r struct {
		Lang    string `json:"lang"`
		English string `json:"english"`
	}
	if json.Unmarshal([]byte(out), &r) != nil {
		return "", "", fmt.Errorf("translation answer was not JSON")
	}
	lang = strings.ToLower(strings.TrimSpace(r.Lang))
	if len(lang) < 2 {
		return "", "", fmt.Errorf("translation answer had no language")
	}
	if len(lang) > 3 {
		lang = lang[:2]
	}
	if i := strings.IndexAny(lang, "-_"); i > 0 {
		lang = lang[:i]
	}
	if lang == "en" {
		return "en", source, nil
	}
	// House style: commas, not dashes. A dash between words becomes a comma;
	// an en dash in a range of numbers becomes a hyphen.
	english = strings.TrimSpace(strings.NewReplacer(
		" "+emDash+" ", ", ", emDash, ", ", " "+enDash+" ", ", ", enDash, "-").Replace(r.English))
	if english == "" || english == source {
		return "", "", fmt.Errorf("no English in the translation answer")
	}
	if l := twoaiEnglishLang(english); l != "" && !isLatinCode(l) {
		return "", "", fmt.Errorf("translation still reads as %s", l)
	}
	if utf8.RuneCountInString(english) > 4*utf8.RuneCountInString(source)+200 {
		return "", "", fmt.Errorf("translation far longer than its source")
	}
	return lang, english, nil
}

// isLatinCode is true for the codes twoaiEnglishLang can return for Latin
// script. A Latin-script verdict on a translation is often a foreign title
// kept with its English gloss, which the rules allow.
func isLatinCode(l string) bool {
	_, ok := englishForeignStop[l]
	return ok
}

// twoaiEnglishLive is the harvest-time translator: the English and the
// language when text is not English and a translation was had, otherwise two
// empty strings and the caller keeps the text as it is.
func twoaiEnglishLive(db *sql.DB, text string) (string, string) {
	if twoaiEnglishLang(text) == "" {
		return "", ""
	}
	en, lang, err := twoaiEnglish(db, "translate_title", text)
	if err != nil {
		if _, said := twoaiEnglishSaid.LoadOrStore(err.Error(), true); !said {
			fmt.Fprintf(os.Stderr, "translate_title: %v (the text stays as it is until the sweep translates it)\n", err)
		}
		return "", ""
	}
	if lang == "" || lang == "en" || en == text {
		return "", ""
	}
	return en, lang
}

// ---------------------------------------------------------------------------
// Schema and the log
// ---------------------------------------------------------------------------

var (
	twoaiEnglishSchemaOnce sync.Once
	twoaiEnglishColsMu     sync.Mutex
	twoaiEnglishColsSeen   = map[string]bool{}
)

// twoaiEnglishSchema makes the columns the upserts rely on before they run:
// the incident harvest keeps title_original, and the vendor feed upserts
// refuse to put a translated title or summary back into the source language.
// Checked against information_schema first, so a run that finds them in
// place takes no lock.
func twoaiEnglishSchema(db *sql.DB) {
	twoaiEnglishSchemaOnce.Do(func() {
		twoaiTranslationsTable(db)
		db.Exec(`CREATE TABLE IF NOT EXISTS twoai_translation_log (
			id bigserial PRIMARY KEY, tbl text NOT NULL, row_key text NOT NULL, field text NOT NULL,
			lang text NOT NULL, old_text text NOT NULL, new_text text NOT NULL, model_stage text,
			owner text NOT NULL DEFAULT 'srj', bridged boolean NOT NULL DEFAULT false,
			at timestamptz NOT NULL DEFAULT now())`)
		for _, c := range [][2]string{
			{"twoai_incidents", "title"},
			{"twoai_vendor_posts", "title"},
			{"twoai_vendor_posts", "summary"},
		} {
			twoaiEnglishEnsureCol(db, c[0], c[1]+"_original", "text")
			twoaiEnglishEnsureCol(db, c[0], c[1]+"_lang", "text")
		}
	})
}

// twoaiVendorEnglishGuard is the SET clause both vendor post upserts share
// for title and summary (main.go twoaiVendorNews, twoai_vendor_feeds.go).
// While the feed still carries the text the sweep translated, the English
// stays; a feed that changes its text replaces both, and the sweep looks at
// the new text on its next run. An empty feed summary keeps the stored one,
// as it always has.
const twoaiVendorEnglishGuard = `title = CASE WHEN twoai_vendor_posts.title_original IS NOT NULL
				AND EXCLUDED.title = twoai_vendor_posts.title_original
				THEN twoai_vendor_posts.title ELSE EXCLUDED.title END,
			title_original = CASE WHEN EXCLUDED.title = twoai_vendor_posts.title_original
				THEN twoai_vendor_posts.title_original END,
			title_lang = CASE WHEN EXCLUDED.title = twoai_vendor_posts.title_original
				THEN twoai_vendor_posts.title_lang END,
			summary = CASE WHEN COALESCE(EXCLUDED.summary,'') = '' THEN twoai_vendor_posts.summary
				WHEN EXCLUDED.summary = twoai_vendor_posts.summary_original THEN twoai_vendor_posts.summary
				ELSE EXCLUDED.summary END,
			summary_original = CASE WHEN COALESCE(EXCLUDED.summary,'') = ''
				OR EXCLUDED.summary = twoai_vendor_posts.summary_original
				THEN twoai_vendor_posts.summary_original END,
			summary_lang = CASE WHEN COALESCE(EXCLUDED.summary,'') = ''
				OR EXCLUDED.summary = twoai_vendor_posts.summary_original
				THEN twoai_vendor_posts.summary_lang END`

// twoaiVendorEnglishSet returns the guard once its columns are known to
// exist, and otherwise the clause the upserts used before 2026-10-06, so a
// failed ALTER can never stop vendor news from being written.
func twoaiVendorEnglishSet(db *sql.DB) string {
	twoaiEnglishSchema(db)
	twoaiEnglishColsMu.Lock()
	ok := twoaiEnglishColsSeen["twoai_vendor_posts.title_original"] && twoaiEnglishColsSeen["twoai_vendor_posts.title_lang"] &&
		twoaiEnglishColsSeen["twoai_vendor_posts.summary_original"] && twoaiEnglishColsSeen["twoai_vendor_posts.summary_lang"]
	twoaiEnglishColsMu.Unlock()
	if ok {
		return twoaiVendorEnglishGuard
	}
	return `title=EXCLUDED.title,
			summary=CASE WHEN EXCLUDED.summary <> '' THEN EXCLUDED.summary ELSE twoai_vendor_posts.summary END`
}

// twoaiEnglishEnsureCol adds a column if it is missing. Table and column
// names come only from the constant lists in this file.
func twoaiEnglishEnsureCol(db *sql.DB, table, col, typ string) {
	k := table + "." + col
	twoaiEnglishColsMu.Lock()
	seen := twoaiEnglishColsSeen[k]
	twoaiEnglishColsMu.Unlock()
	if seen {
		return
	}
	var exists bool
	db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema='public' AND table_name=$1 AND column_name=$2)`, table, col).Scan(&exists)
	if !exists {
		if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s`, table, col, typ)); err != nil {
			fmt.Fprintf(os.Stderr, "twoai_english: add %s: %v\n", k, err)
			return
		}
		fmt.Printf("twoai_english: added column %s\n", k)
	}
	twoaiEnglishColsMu.Lock()
	twoaiEnglishColsSeen[k] = true
	twoaiEnglishColsMu.Unlock()
}

// twoaiEnglishLogged records one translation, once: the same field of the
// same row translated from the same text is not logged twice.
func twoaiEnglishLogged(db *sql.DB, table, key, field, lang, oldText, newText, stage, owner string) {
	twoaiEnglishSchema(db)
	db.Exec(`INSERT INTO twoai_translation_log (tbl, row_key, field, lang, old_text, new_text, model_stage, owner)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8
		WHERE NOT EXISTS (SELECT 1 FROM twoai_translation_log
			WHERE tbl=$1 AND row_key=$2 AND field=$3 AND old_text=$5)`,
		table, key, field, lang, oldText, newText, stage, owner)
}

// ---------------------------------------------------------------------------
// jsonb documents
// ---------------------------------------------------------------------------

// Keys whose values are identifiers, links, dates or names lists, never
// prose a visitor reads as a sentence.
var englishSkipKeys = map[string]bool{
	"url": true, "URL": true, "href": true, "slug": true, "Slug": true, "uid": true, "id": true,
	"domain": true, "Domain": true, "Domains": true, "date": true, "Date": true, "source_url": true,
	"SummaryURL": true, "SummaryDomain": true, "image": true, "email": true, "wikidata_qid": true,
	"Persons": true, "Orgs": true, "merged_from": true, "english_originals": true, "kind": true,
	"state": true, "bill": true, "published_on": true, "ArchivedDate": true, "ArchivedGenerated": true,
}

func englishSkipKey(k string) bool {
	return englishSkipKeys[k] || strings.HasSuffix(k, "_original") || strings.HasSuffix(k, "_lang") ||
		strings.HasSuffix(k, "_url") || strings.HasSuffix(k, "_uid")
}

// englishChange is one string in a document that was put into English.
type englishChange struct {
	Path, Lang, Old, New string
}

// twoaiEnglishDoc walks a decoded jsonb document and replaces every
// non-English string of a dozen characters or more with its English.
//
// siblings: a string under key K in an object gets K_original and K_lang
// beside it, which is what the news templates read. Otherwise, and for a
// string inside an array, the original goes into a top-level
// english_originals map keyed on the path, when the document is an object.
//
// translate is twoaiEnglish bound to a stage, or a cache-only lookup.
func twoaiEnglishDoc(root any, siblings bool, translate func(string) (string, string, error)) (any, []englishChange) {
	var changes []englishChange
	var walk func(v any, path string) any
	try := func(s, path string) (string, bool) {
		if utf8.RuneCountInString(strings.TrimSpace(s)) < 12 || strings.HasPrefix(s, "http") || twoaiEnglishLang(s) == "" {
			return s, false
		}
		en, lang, err := translate(s)
		if err != nil || lang == "" || lang == "en" || en == s {
			return s, false
		}
		changes = append(changes, englishChange{Path: path, Lang: lang, Old: s, New: en})
		return en, true
	}
	originals := map[string]any{}
	walk = func(v any, path string) any {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if englishSkipKey(k) {
					continue
				}
				p := k
				if path != "" {
					p = path + "." + k
				}
				if s, ok := t[k].(string); ok {
					if en, changed := try(s, p); changed {
						t[k] = en
						if siblings {
							t[k+"_original"] = s
							t[k+"_lang"] = changes[len(changes)-1].Lang
						} else {
							originals[p] = map[string]any{"text": s, "lang": changes[len(changes)-1].Lang}
						}
					}
					continue
				}
				t[k] = walk(t[k], p)
			}
			return t
		case []any:
			for i := range t {
				p := fmt.Sprintf("%s[%d]", path, i)
				if s, ok := t[i].(string); ok {
					if en, changed := try(s, p); changed {
						t[i] = en
						originals[p] = map[string]any{"text": s, "lang": changes[len(changes)-1].Lang}
					}
					continue
				}
				t[i] = walk(t[i], p)
			}
			return t
		}
		return v
	}
	root = walk(root, "")
	if len(originals) > 0 {
		if m, ok := root.(map[string]any); ok {
			prev, _ := m["english_originals"].(map[string]any)
			if prev == nil {
				prev = map[string]any{}
			}
			for k, v := range originals {
				if _, had := prev[k]; !had {
					prev[k] = v
				}
			}
			m["english_originals"] = prev
		}
	}
	return root, changes
}

// twoaiEnglishCacheOnly is a translate function for twoaiEnglishDoc that
// never calls a model: the news archive uses it so that a refresh from
// news.json cannot put back a headline the sweep has already translated.
func twoaiEnglishCacheOnly(db *sql.DB) func(string) (string, string, error) {
	return func(s string) (string, string, error) {
		en, lang, ok := twoaiEnglishCached(db, strings.TrimSpace(s))
		if !ok || lang == "en" {
			return s, "", fmt.Errorf("not cached")
		}
		return en, lang, nil
	}
}

// ---------------------------------------------------------------------------
// The sweep
// ---------------------------------------------------------------------------

// englishCol is one text column a visitor reads. Owner "theworldofai" marks
// the content project's tables: Stephen's rule lets srj fix certainly-wrong
// content there, and every such change is reported to them.
type englishCol struct {
	Table, Key, Col, Where, Owner string
}

// englishJSONCol is one jsonb column a visitor reads.
type englishJSONCol struct {
	Table, Key, Col, Where, Owner string
	Siblings                      bool
}

// Found with SQL on 2026-10-06 (information_schema, then a count of values
// with letters outside the Latin script or foreign function words). The
// techgig and solo news tables hold only URLs and outcomes apart from
// solo_news.subject; the abstracts of papers are not swept, titles are.
var twoaiEnglishCols = []englishCol{
	{"twoai_incidents", "guid", "title", "", "srj"},
	{"twoai_news_stories", "slug", "headline", "retired_at IS NULL", "srj"},
	{"twoai_page_news", "page_uid||'|'||story_uid", "headline", "active", "srj"},
	{"twoai_news_links", "story_uid||'|'||target_kind||'|'||target_uid", "headline", "retired_reason IS NULL", "srj"},
	{"twoai_solo_news", "url", "subject", "", "srj"},
	{"twoai_state_case_news", "url", "title", "", "srj"},
	{"twoai_state_news_pins", "state_slug||'|'||story_uid", "note", "", "srj"},
	{"ai_intel_candidates", "id::text", "name", "", "srj"},
	{"ai_intel_candidates", "id::text", "summary", "", "srj"},
	{"twoai_vendor_posts", "slug", "title", "retired_at IS NULL", "srj"},
	{"twoai_vendor_posts", "slug", "summary", "retired_at IS NULL", "srj"},
	{"twoai_vendor_posts", "slug", "reader_note", "retired_at IS NULL", "srj"},
	{"twoai_research_papers", "uid", "title", "", "srj"},
	{"twoai_health_papers", "doi", "title", "", "srj"},
	{"twoai_company_profiles", "uid", "last_round", "", "srj"},
	{"twoai_sourced_facts", "id::text", "claim", "", "theworldofai"},
	{"twoai_sourced_facts", "id::text", "source_title", "", "theworldofai"},
	{"twoai_section_pages", "slug", "name", "", "theworldofai"},
	{"twoai_section_pages", "slug", "blurb", "", "theworldofai"},
	{"twoai_section_pages", "slug", "answer", "", "theworldofai"},
	{"twoai_section_pages", "slug", "meaning_heading", "", "theworldofai"},
	{"twoai_section_pages", "slug", "meaning", "", "theworldofai"},
	{"twoai_section_pages", "slug", "explainer", "", "theworldofai"},
}

var twoaiEnglishJSONCols = []englishJSONCol{
	{"twoai_news_stories", "slug", "story", "retired_at IS NULL", "srj", true},
	{"twoai_company_profiles", "uid", "leadership", "", "srj", false},
	{"twoai_section_pages", "slug", "developments", "", "theworldofai", false},
	{"twoai_section_pages", "slug", "key_points", "", "theworldofai", false},
	{"twoai_section_pages", "slug", "faq", "", "theworldofai", false},
	{"site_people", "slug", "data", "", "theworldofai", false},
	{"site_content", "path", "data", "", "theworldofai", false},
}

// twoaiEnglishSweep is the twoai_english_sweep stage. It returns how many
// values it put into English, so the backlog loop can tell when it is idle.
func twoaiEnglishSweep(db *sql.DB) int {
	const stage = "twoai_english_sweep"
	if twoaiDeferForPeak(stage) {
		return 0
	}
	twoaiEnglishSchema(db)
	translate := func(s string) (string, string, error) { return twoaiEnglish(db, stage, s) }
	done, failed, capped := 0, 0, false

	for _, c := range twoaiEnglishCols {
		var hasOrig bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema='public' AND table_name=$1 AND column_name=$2)`, c.Table, c.Col+"_original").Scan(&hasOrig)
		where := fmt.Sprintf("%s IS NOT NULL AND %s <> ''", c.Col, c.Col)
		if c.Where != "" {
			where += " AND (" + c.Where + ")"
		}
		if hasOrig {
			// A value already translated is not looked at again, except a
			// harvest row still waiting for its translation (title equal to
			// its original), which the incident harvest leaves that way.
			where += fmt.Sprintf(" AND (%s_original IS NULL OR %s = %s_original)", c.Col, c.Col, c.Col)
		}
		rows, err := db.Query(fmt.Sprintf(`SELECT (%s)::text, %s FROM %s WHERE %s`, c.Key, c.Col, c.Table, where))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %s.%s: %v\n", stage, c.Table, c.Col, err)
			continue
		}
		type kv struct{ k, v string }
		var todo []kv
		for rows.Next() {
			var x kv
			if rows.Scan(&x.k, &x.v) == nil && twoaiEnglishLang(x.v) != "" {
				todo = append(todo, x)
			}
		}
		rows.Close()
		for _, x := range todo {
			en, lang, err := translate(x.v)
			if err != nil {
				if strings.Contains(err.Error(), "cap of") {
					capped = true
				} else {
					failed++
					// NAME THE VALUE. Row 563 (2026-10-07): failed=2 on every run
					// for a week and nothing said which two, so nothing could be
					// fixed or marked. The first few are printed with the reason.
					if failed <= 5 {
						fmt.Fprintf(os.Stderr, "%s: translate failed %s.%s key=%s: %v: %q\n", stage, c.Table, c.Col, x.k, err, trunc(x.v, 100))
					}
				}
				continue
			}
			if lang == "en" || en == x.v {
				continue
			}
			twoaiEnglishEnsureCol(db, c.Table, c.Col+"_original", "text")
			twoaiEnglishEnsureCol(db, c.Table, c.Col+"_lang", "text")
			res, err := db.Exec(fmt.Sprintf(`UPDATE %s SET %s=$1, %s_original=$2, %s_lang=$3 WHERE (%s)::text=$4 AND %s=$2`,
				c.Table, c.Col, c.Col, c.Col, c.Key, c.Col), en, x.v, lang, x.k)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: update %s %s: %v\n", stage, c.Table, x.k, err)
				continue
			}
			if n, _ := res.RowsAffected(); n > 0 {
				done++
				twoaiEnglishLogged(db, c.Table, x.k, c.Col, lang, x.v, en, stage, c.Owner)
			}
		}
	}

	for _, c := range twoaiEnglishJSONCols {
		where := c.Col + " IS NOT NULL"
		if c.Where != "" {
			where += " AND (" + c.Where + ")"
		}
		rows, err := db.Query(fmt.Sprintf(`SELECT (%s)::text, %s::text FROM %s WHERE %s`, c.Key, c.Col, c.Table, where))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %s.%s: %v\n", stage, c.Table, c.Col, err)
			continue
		}
		type kv struct{ k, v string }
		var todo []kv
		for rows.Next() {
			var x kv
			if rows.Scan(&x.k, &x.v) == nil && englishDocMayNeed(x.v) {
				todo = append(todo, x)
			}
		}
		rows.Close()
		for _, x := range todo {
			var doc any
			if json.Unmarshal([]byte(x.v), &doc) != nil {
				continue
			}
			doc, changes := twoaiEnglishDoc(doc, c.Siblings, func(s string) (string, string, error) {
				en, lang, err := translate(s)
				if err != nil {
					if strings.Contains(err.Error(), "cap of") {
						capped = true
					} else {
						failed++
						if failed <= 5 {
							fmt.Fprintf(os.Stderr, "%s: translate failed %s.%s key=%s: %v: %q\n", stage, c.Table, c.Col, x.k, err, trunc(s, 100))
						}
					}
				}
				return en, lang, err
			})
			if len(changes) == 0 {
				continue
			}
			// A root that is an array has nowhere to keep originals inside
			// it, so they go in a <col>_originals column beside it.
			var extra string
			if _, isObj := doc.(map[string]any); !isObj && !c.Siblings {
				m := map[string]any{}
				for _, ch := range changes {
					m[ch.Path] = map[string]any{"text": ch.Old, "lang": ch.Lang}
				}
				b, _ := json.Marshal(m)
				extra = string(b)
				twoaiEnglishEnsureCol(db, c.Table, c.Col+"_originals", "jsonb")
			}
			nb, err := json.Marshal(doc)
			if err != nil {
				continue
			}
			var res sql.Result
			if extra != "" {
				res, err = db.Exec(fmt.Sprintf(`UPDATE %s SET %s=$1::jsonb, %s_originals=COALESCE(%s_originals,'{}'::jsonb) || $4::jsonb
					WHERE (%s)::text=$2 AND %s::text=$3`, c.Table, c.Col, c.Col, c.Col, c.Key, c.Col), string(nb), x.k, x.v, extra)
			} else {
				res, err = db.Exec(fmt.Sprintf(`UPDATE %s SET %s=$1::jsonb WHERE (%s)::text=$2 AND %s::text=$3`,
					c.Table, c.Col, c.Key, c.Col), string(nb), x.k, x.v)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: update %s %s: %v\n", stage, c.Table, x.k, err)
				continue
			}
			if n, _ := res.RowsAffected(); n > 0 {
				for _, ch := range changes {
					done++
					twoaiEnglishLogged(db, c.Table, x.k, c.Col+"."+ch.Path, ch.Lang, ch.Old, ch.New, stage, c.Owner)
				}
			}
		}
	}

	// Archived incident pages carry their headline and report titles inside
	// twoai_pages; publish_news refreshes them from twoai_incidents, which the
	// column pass above has just put into English.
	fmt.Printf("%s: translated=%d failed=%d cap_reached=%v (cap %d model calls a run)\n",
		stage, done, failed, capped, twoaiEnglishCap(stage))
	twoaiEnglishBridge(db)
	return done
}

// englishDocMayNeed is a cheap pre-check on a document's text: anything with
// a non-ASCII letter, or with enough words to carry foreign function words.
func englishDocMayNeed(raw string) bool {
	for _, r := range raw {
		if r > 0x7f && unicode.IsLetter(r) {
			return true
		}
	}
	// Pure ASCII: only a Latin-script language without accents (Dutch,
	// Indonesian, much Spanish) can hide here, so the walk decides.
	return len(raw) > 40
}

// twoaiEnglishBridge sends theworldofai one bridge row listing what was
// translated since the last one, content-project rows first, capped near
// sixty lines; the rest is in twoai_translation_log.
func twoaiEnglishBridge(db *sql.DB) {
	rows, err := db.Query(`SELECT id, tbl, row_key, field, lang, old_text, new_text, owner
		FROM twoai_translation_log WHERE NOT bridged ORDER BY (owner='srj'), id`)
	if err != nil {
		return
	}
	type line struct {
		id                                      int64
		tbl, key, field, lang, old, new_, owner string
	}
	var all []line
	for rows.Next() {
		var l line
		if rows.Scan(&l.id, &l.tbl, &l.key, &l.field, &l.lang, &l.old, &l.new_, &l.owner) == nil {
			all = append(all, l)
		}
	}
	rows.Close()
	if len(all) == 0 {
		return
	}
	clip := func(s string, n int) string {
		s = strings.Join(strings.Fields(s), " ")
		if utf8.RuneCountInString(s) > n {
			return string([]rune(s)[:n]) + "..."
		}
		return s
	}
	var b strings.Builder
	content := 0
	for _, l := range all {
		if l.owner != "srj" {
			content++
		}
	}
	fmt.Fprintf(&b, "Stephen's rule (row 508): everything visitors see on theworldofai.org is in English. "+
		"The pipeline translated %d values that were not, %d of them in your tables. "+
		"Each original is kept beside the English (a <field>_original and <field>_lang column, "+
		"<key>_original keys in news story jsonb, or an english_originals map in a content record), "+
		"and every change is in twoai_translation_log. Proper names stay as written. "+
		"If a translation is wrong, correct the English in place; the original stays where it is.\n\n", len(all), content)
	shown := 0
	for _, l := range all {
		if shown == 60 {
			break
		}
		who := ""
		if l.owner != "srj" {
			who = " (your table)"
		}
		fmt.Fprintf(&b, "- %s %s %s%s, from %s: %q -> %q\n", l.tbl, l.key, l.field, who,
			twoaiEnglishLangName(l.lang), clip(l.old, 120), clip(l.new_, 120))
		shown++
	}
	if len(all) > shown {
		fmt.Fprintf(&b, "\n%d more are in twoai_translation_log (bridged = false until this row was sent, now true).\n", len(all)-shown)
	}
	topic := fmt.Sprintf("English sweep: %d values translated, %d in your tables", len(all), content)
	if _, err := db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body) VALUES ('srj','theworldofai',$1,$2)`,
		topic, b.String()); err != nil {
		fmt.Fprintln(os.Stderr, "twoai_english_sweep: bridge:", err)
		return
	}
	ids := make([]string, 0, len(all))
	for _, l := range all {
		ids = append(ids, strconv.FormatInt(l.id, 10))
	}
	db.Exec(`UPDATE twoai_translation_log SET bridged = true WHERE id = ANY(string_to_array($1, ',')::bigint[])`, strings.Join(ids, ","))
	fmt.Printf("twoai_english_sweep: bridge row sent to theworldofai, %d changes\n", len(all))
}
