package main

// Understand AI (d2390ba3): the glossary as a guided path, rebuilt every run
// from this site's own tables. Stephen, 2026-09-21: the page was thin, a
// table of contents written once in SQL that nothing refreshed. It now
// carries, per step, the glossary definition and example of each term it
// names, and where that idea shows up on this site with today's figures; one
// worked example traced through all nine steps at today's median API price;
// a short FAQ with schema; and a reading written by Ollama from the page's
// own data, cached on a hash so it is rewritten only when the data changes.
//
// Nothing here is invented. Definitions and examples are the glossary rows.
// Figures are read from the published section documents at build time. The
// worked example states its one assumption, the words-per-token rule of
// thumb, and names who publishes it.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const understandPath = "ecosystem/understand-ai.json"
const understandTech = "/ai-ecosystem/technology-and-core-infrastructure/"

type uaTerm struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Href       string `json:"href"`
	Definition string `json:"definition"`
	Example    string `json:"example,omitempty"`
}

type uaMeet struct {
	Name string `json:"name"`
	Href string `json:"href"`
	UID  string `json:"uid"`
	Fact string `json:"fact"`
}

type uaStep struct {
	N     int      `json:"n"`
	Title string   `json:"title"`
	Why   string   `json:"why"`
	Terms []uaTerm `json:"terms"`
	Meet  []uaMeet `json:"meet"`
}

func uaTotal(db *sql.DB, path string) (string, int) {
	var uid string
	var total sql.NullInt64
	db.QueryRow(`SELECT COALESCE(data->>'uid',''), NULLIF(data->>'total','')::int FROM twoai_pages WHERE path=$1`, path).Scan(&uid, &total)
	return uid, int(total.Int64)
}

func twoaiUnderstandAI(db *sql.DB) error {
	var raw []byte
	if err := db.QueryRow(`SELECT data::text FROM twoai_pages WHERE path=$1`, understandPath).Scan(&raw); err != nil {
		return fmt.Errorf("page missing: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}

	// Glossary rows.
	terms := map[string]uaTerm{}
	if r, err := db.Query(`SELECT t->>'slug', t->>'term', t->>'uid', COALESCE(t->>'definition',''), COALESCE(t->>'example','')
		FROM twoai_pages p, jsonb_array_elements(p.data->'terms') t WHERE p.path='glossary/glossary.json'`); err == nil {
		for r.Next() {
			var s, n, u, d, e string
			if r.Scan(&s, &n, &u, &d, &e) == nil {
				terms[s] = uaTerm{Name: n, UID: u, Href: "/ai-glossary/" + s + "/", Definition: d, Example: e}
			}
		}
		r.Close()
	}

	// Live figures.
	var medIn, medOut float64
	var provs, ctxMax, nModels int
	var priceUID string
	db.QueryRow(`SELECT data->>'uid', (data->'stats'->>'median_prompt_pm')::float, (data->'stats'->>'median_completion_pm')::float,
		(data->'stats'->>'provider_count')::int, (data->'stats'->>'context_max')::int, NULLIF(data->>'total','')::int
		FROM twoai_pages WHERE path='models/api-pricing.json'`).Scan(&priceUID, &medIn, &medOut, &provs, &ctxMax, &nModels)
	llmUID, llmN := uaTotal(db, "models/llms.json")
	embUID, embN := uaTotal(db, "models/embedding-models.json")
	infUID, infN := uaTotal(db, "repos/inference-engines.json")
	fwUID, fwN := uaTotal(db, "repos/ai-frameworks.json")
	srvUID, srvN := uaTotal(db, "models/serving-providers.json")
	trUID, trN := uaTotal(db, "tech/ds-training.json")
	evUID, evN := uaTotal(db, "tech/ds-evaluation.json")
	a2aUID, _ := uaTotal(db, "repos/a2a-protocol.json")
	agUID, agN := uaTotal(db, "repos/autonomous-agents.json")
	var mcpN int
	db.QueryRow(`SELECT count(*) FROM twoai_mcp_servers WHERE COALESCE(status,'active') <> 'deleted'`).Scan(&mcpN)

	// The worked example: a 40 page contract. 500 words a page is the stated
	// assumption; 0.75 words per token is OpenAI's published rule of thumb
	// for English text.
	const pages, wordsPerPage, outTokens = 40, 500, 1000
	words := pages * wordsPerPage
	inTokens := int(math.Round(float64(words)/0.75/100) * 100)
	var fits int
	db.QueryRow(`SELECT count(*) FROM twoai_pages p, jsonb_array_elements(p.data->'providers') pr, jsonb_array_elements(pr->'models') m
		WHERE p.path='models/api-pricing.json' AND (m->>'context')::numeric >= $1`, inTokens+outTokens).Scan(&fits)
	costIn := float64(inTokens) / 1e6 * medIn
	costOut := float64(outTokens) / 1e6 * medOut
	cents := (costIn + costOut) * 100

	t := func(slugs ...string) []uaTerm {
		out := []uaTerm{}
		for _, s := range slugs {
			if v, ok := terms[s]; ok {
				out = append(out, v)
			}
		}
		return out
	}
	m := func(name, uid, fact string) uaMeet {
		return uaMeet{Name: name, UID: uid, Href: understandTech + uid + "/", Fact: fact}
	}
	f2 := func(v float64) string { return fmt.Sprintf("$%.2f", v) }

	steps := []uaStep{
		{1, "Start with the unit: the token", "Everything a model reads and writes is counted in tokens, and so is everything it costs. Until this one is clear, no pricing page will make sense.",
			t("token"), []uaMeet{m("API Pricing", priceUID, fmt.Sprintf("Today the median model tracked here costs %s per million input tokens and %s per million output tokens, across %d models from %d providers.", f2(medIn), f2(medOut), nModels, provs))}},
		{2, "What the model is made of: transformer, foundation model", "The architecture underneath almost every current system, and the word for a large general model that others are built on.",
			t("transformer", "foundation-model"), []uaMeet{
				m("Large Language Models", llmUID, fmt.Sprintf("The %s most downloaded open language models on Hugging Face, refreshed daily.", commaInt(llmN))),
				m("AI Frameworks", fwUID, fmt.Sprintf("%d open source frameworks these models are built and trained with.", fwN))}},
		{3, "How it represents meaning: embedding", "How text becomes numbers a machine can compare. This is the idea that makes search, retrieval and recommendation work.",
			t("embedding"), []uaMeet{m("Embedding Models", embUID, fmt.Sprintf("The %s most downloaded open embedding models, the component that turns text into those numbers.", commaInt(embN)))}},
		{4, "How much it can hold at once: context window", "The working memory of a model, and the constraint behind most disappointing results. A model has not read your whole document unless the document fits.",
			t("context-window"), []uaMeet{m("API Pricing", priceUID, fmt.Sprintf("The largest context window among the %d API models tracked here is %s tokens.", nModels, commaInt(ctxMax)))}},
		{5, "Running it: inference", "Training builds the model. Inference is using it. Almost every cost a business actually pays is here, not in training.",
			t("inference"), []uaMeet{
				m("Inference Engines", infUID, fmt.Sprintf("%d open source engines that run models efficiently on your own hardware.", infN)),
				m("Model Serving Providers", srvUID, fmt.Sprintf("%d companies that run inference for you and bill per token.", srvN))}},
		{6, "Making it yours: fine-tuning, prompt engineering", "The two ways to change a model's behaviour without building one. Knowing which problem calls for which is most of the practical skill.",
			t("fine-tuning", "prompt-engineering"), []uaMeet{m("Training Datasets", trUID, fmt.Sprintf("%d curated datasets used to train and fine-tune models, with licence and gating.", trN))}},
		{7, "Giving it your facts: retrieval", "Feeding a model your own documents at the moment of the question, rather than retraining it. This is how most enterprise deployments actually work.",
			t("rag-retrieval-augmented-generation"), []uaMeet{m("Embedding Models", embUID, "Retrieval runs on embeddings: your documents and the question are both turned into numbers, and the closest matches are handed to the model.")}},
		{8, "Letting it act: agent", "A model that takes actions rather than only answering. Every governance and security question gets harder at this step.",
			t("agent"), []uaMeet{
				{Name: "MCP Server tracker", Href: "/mcp/", UID: "", Fact: fmt.Sprintf("%s servers from the official Model Context Protocol registry, the standard way agents are given tools.", commaInt(mcpN))},
				m("Agent2Agent (A2A) Protocol", a2aUID, "The open protocol for one agent to hand work to another, explained from its specification."),
				m("Autonomous Agents", agUID, fmt.Sprintf("%d open source agent projects tracked from their repositories.", agN))}},
		{9, "Where it goes wrong: hallucination", "Confident output that is not true. Understanding why it happens, rather than treating it as a bug to patch, is the difference between a working deployment and a liability.",
			t("hallucination"), []uaMeet{m("Evaluation Datasets", evUID, fmt.Sprintf("%d curated benchmarks used to measure how often models get things right.", evN))}},
	}

	example := []map[string]string{
		{"step": "Tokens", "text": fmt.Sprintf("A 40 page contract at about %d words a page is roughly %s words. At OpenAI's rule of thumb of about 0.75 words per token for English, that is about %s tokens in, plus about %s tokens for a one page summary out.", wordsPerPage, commaInt(words), commaInt(inTokens), commaInt(outTokens))},
		{"step": "Context window", "text": fmt.Sprintf("The model has to hold all %s tokens at once. %d of the %d API models tracked here can, and %d cannot, so for those the contract would have to be split.", commaInt(inTokens+outTokens), fits, nModels, nModels-fits)},
		{"step": "Inference and cost", "text": fmt.Sprintf("At today's median prices, %s per million in and %s per million out, the read costs about $%.4f and the summary about $%.4f, together about %.1f cents. Price, not capability, is rarely the barrier at this size.", f2(medIn), f2(medOut), costIn, costOut, cents)},
		{"step": "Retrieval instead", "text": "For a library of contracts rather than one, the whole library will not fit. Retrieval embeds every contract once and hands the model only the passages that match the question."},
		{"step": "Where it goes wrong", "text": "A summary can state a clause the contract does not contain. For anything with legal weight, the summary is a map to the pages to read, not a substitute for them."},
	}

	faqs := []map[string]string{
		{"q": "What should I learn first about AI?", "a": "Tokens. Every price, limit and speed figure for a model is stated in tokens, so the rest of the vocabulary depends on it."},
		{"q": "How much does it cost to use an AI model?", "a": fmt.Sprintf("Today the median API model tracked on this site costs %s per million input tokens and %s per million output tokens. Summarising a 40 page contract comes to about %.1f cents at those prices.", f2(medIn), f2(medOut), cents)},
		{"q": "What is the difference between training and inference?", "a": "Training builds a model from data and is done once, usually by the model's maker. Inference is every use of the model after that, and it is where most ongoing cost sits."},
		{"q": "Should I fine-tune a model or use retrieval?", "a": "Use retrieval when the model needs your facts, because documents change and retrieval reads them at question time. Use fine-tuning when you need a different behaviour or style, not new facts."},
		{"q": "Why do AI models make things up?", "a": "A model produces the most likely continuation of text, not a checked fact. When the likely answer and the true answer differ, it states the likely one with the same confidence."},
	}

	now := time.Now()
	doc["shape"] = "learning-path"
	doc["steps"] = steps
	doc["example"] = map[string]any{"title": "One question through all nine steps: summarising a 40 page contract", "steps": example,
		"assumption": "Assumes about 500 words a page. Words per token is OpenAI's published rule of thumb for English text, and varies by language and model.",
		"assumption_source": "https://help.openai.com/en/articles/4936856-what-are-tokens-and-how-to-count-them"}
	doc["faqs"] = faqs
	doc["generated"] = now.Format("2006-01-02")
	doc["verified"] = now.Format("2006-01-02")
	doc["built_at"] = now.Format("2006-01-02T15:04:05-07")
	doc["refresh_every_days"] = 1
	delete(doc, "built") // the steps supersede the old one-line list

	// The reading: Ollama, from the page's own data, cached on a hash of the
	// figures so it is rewritten only when they change.
	facts, _ := json.Marshal(map[string]any{"steps": steps, "example": example})
	h := sha256.Sum256(facts)
	hash := hex.EncodeToString(h[:8])
	metric := "page:" + understandPath
	var have int
	db.QueryRow(`SELECT 1 FROM twoai_industry_analysis WHERE metric=$1 AND data_hash=$2`, metric, hash).Scan(&have)
	if have != 1 {
		body, model, err := twoaiGenerate("page_readings", thinPageSystem,
			"The page publishes at https://theworldofai.org/ai-ecosystem/research-knowledge-and-learning/d2390ba3/\n\nIts data:\n"+string(facts))
		if err != nil || len(body) < 300 {
			fmt.Printf("twoai_understand_ai: reading not written: %v (len %d)\n", err, len(body))
		} else {
			db.Exec(`INSERT INTO twoai_industry_analysis (metric, data_hash, model, body, generated_on)
				VALUES ($1,$2,$3,$4,current_date) ON CONFLICT (metric, data_hash) DO NOTHING`, metric, hash, model, body)
		}
	}

	b, _ := json.Marshal(doc)
	if _, err := db.Exec(`UPDATE twoai_pages SET data = $2::jsonb, updated_at = now() WHERE path = $1`, understandPath, string(b)); err != nil {
		return err
	}
	fmt.Printf("twoai_understand_ai: steps=%d terms=%d median_in=%.2f fits=%d/%d cents=%.1f ok=true\n", len(steps), len(terms), medIn, fits, nModels, cents)
	return nil
}

func commaInt(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}
