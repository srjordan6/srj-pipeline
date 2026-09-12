package main

// twoai_llm: one place that decides which model answers, and what happens
// when it cannot.
//
// Stephen, 2026-09-12: Anthropic is getting too expensive, look at Ollama or
// Hugging Face. The measured bill is about $2 a day steady state and roughly
// $107 for everything written to date, so this is worth doing and is not an
// emergency - which is the right posture, because the failure mode of a weak
// model on this site is not a worse sentence, it is an invented fact on a
// page whose entire value is that it does not invent them.
//
// WHAT MOVES AND WHAT DOES NOT. The jobs split by what they ask of a model,
// not by how much they cost:
//
//   EXTRACTIVE - the text is in the prompt and the job is compression.
//   Vendor summaries, news summaries, point briefs, paper explanations.
//   A small local model is good at this, and if it strays the supplied text
//   is right there to check it against. These move.
//
//   JUDGMENT - read data and say what it means. Industry analysis, company
//   profiles, page readings. These are where a weak model fabricates, and
//   where a fabrication reads exactly like an insight. They stay on Claude
//   until a side-by-side says otherwise.
//
// The split is per stage rather than global, so moving one job is a config
// change and never an all-or-nothing switch.
//
// FALLING BACK IS THE POINT. If Ollama is not running, the answer is not
// "write nothing" - it is Claude, plus a line in the log saying the local
// model was unreachable. A stopped service must degrade to working-and-paid,
// never to silently-producing-nothing, which is the failure this pipeline has
// been bitten by more than once.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// twoaiLLMFor answers which provider a given stage should use.
//
//	TWOAI_LLM              default for every stage: "ollama" or "anthropic"
//	TWOAI_LLM_<STAGE>      override for one stage, same values
//
// Unset means anthropic, so nothing changes until somebody turns it on.
func twoaiLLMFor(stage string) string {
	if v := strings.TrimSpace(os.Getenv("TWOAI_LLM_" + strings.ToUpper(stage))); v != "" {
		return strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("TWOAI_LLM")); v != "" {
		return strings.ToLower(v)
	}
	return "anthropic"
}

var (
	ollamaWarnOnce sync.Once
	ollamaDown     bool
	ollamaMu       sync.Mutex
)

// twoaiOllamaHost is where the local server listens. Overridable because the
// PC may serve it to other machines later.
func twoaiOllamaHost() string {
	if h := strings.TrimSpace(os.Getenv("OLLAMA_HOST")); h != "" {
		if !strings.HasPrefix(h, "http") {
			h = "http://" + h
		}
		return strings.TrimRight(h, "/")
	}
	return "http://127.0.0.1:11434"
}

// twoaiOllamaModel is the local model, per stage then global.
func twoaiOllamaModel(stage string) string {
	if m := strings.TrimSpace(os.Getenv("OLLAMA_MODEL_" + strings.ToUpper(stage))); m != "" {
		return m
	}
	if m := strings.TrimSpace(os.Getenv("OLLAMA_MODEL")); m != "" {
		return m
	}
	// The default depends on where the request is going. Ollama Cloud's
	// catalogue (read 2026-09-12 with Stephen's key) has no mistral-small;
	// it carries gpt-oss:20b and :120b, gemma4:31b, the deepseek-v4 family,
	// qwen3.5:397b, kimi and glm. gpt-oss:20b is the cheap fast one and
	// suits the extractive jobs; the large ones are what the comparison
	// exists to test. Local keeps Mistral Small, which fits a 12GB card and
	// scored 80-81% on summarisation correctness in independent testing.
	if strings.Contains(twoaiOllamaHost(), "ollama.com") {
		return "gpt-oss:20b"
	}
	return "mistral-small"
}

var twoaiOllamaClient = &http.Client{Timeout: 180 * time.Second}

// twoaiOllamaCall runs one completion against the local server. Same shape as
// twoaiClaudeCall so a stage can swap between them without knowing which it
// got.
func twoaiOllamaCall(model, system, user string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":  model,
		"system": system,
		"prompt": user,
		"stream": false,
		"options": map[string]any{
			// Low temperature on purpose. Every job routed here is extractive,
			// and creative variation in a summary of somebody else's text is
			// indistinguishable from invention.
			"temperature": 0.2,
			"num_ctx":     8192,
		},
	})
	req, err := http.NewRequest("POST", twoaiOllamaHost()+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	// Ollama Cloud (ollama.com) uses the same API as a local server plus a
	// bearer key. Stephen took the Pro plan on 2026-09-12, which is what
	// makes the larger models reachable for the judgment jobs a 12GB card
	// cannot run. Local needs no key and ignores the header.
	if key := strings.TrimSpace(os.Getenv("OLLAMA_API_KEY")); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := twoaiOllamaClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("ollama %d: %.200s", resp.StatusCode, b)
	}
	var out struct {
		Response string `json:"response"`
		Error    string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Error != "" {
		return "", fmt.Errorf("ollama: %s", out.Error)
	}
	if strings.TrimSpace(out.Response) == "" {
		return "", fmt.Errorf("ollama returned an empty response")
	}
	return out.Response, nil
}

// twoaiGenerate is what every stage should call. It routes by stage, falls
// back to Claude when the local server cannot answer, and says so once per
// run rather than once per call - a hundred identical lines would bury the
// rest of the log, and one line is enough to know.
//
// The fallback is deliberately not silent and deliberately not fatal. A local
// model that is down should cost money, not content.
func twoaiGenerate(stage, system, user string) (string, string, error) {
	want := twoaiLLMFor(stage)
	if want == "ollama" {
		ollamaMu.Lock()
		down := ollamaDown
		ollamaMu.Unlock()
		if !down {
			model := twoaiOllamaModel(stage)
			out, err := twoaiOllamaCall(model, system, user)
			if err == nil {
				return out, "ollama:" + model, nil
			}
			// One failure marks the server down for the rest of the run. A
			// model that is not pulled, or a service that is stopped, fails
			// identically on every subsequent call, and retrying four hundred
			// times at a three-minute timeout would eat the whole run.
			ollamaMu.Lock()
			ollamaDown = true
			ollamaMu.Unlock()
			ollamaWarnOnce.Do(func() {
				fmt.Fprintf(os.Stderr,
					"twoai_llm: ollama unreachable at %s (%v), falling back to Claude for the rest of this run\n",
					twoaiOllamaHost(), err)
			})
		}
	}
	model := os.Getenv("TWOAI_BRIEF_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	out, err := twoaiClaudeCall(model, system, user)
	if err != nil {
		return "", "", err
	}
	return out, model, nil
}
