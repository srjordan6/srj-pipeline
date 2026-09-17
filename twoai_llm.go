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
// NO FALLBACK. Stephen, 2026-09-16: I do not want Claude at all. If Ollama
// cannot handle it, just fail and tell me. So the default is Ollama, and a
// stage that cannot get an answer from it reports the error and writes
// nothing. TWOAI_LLM_FALLBACK=claude restores the paid fallback for a run;
// nothing reaches Anthropic unless that is set, or a stage is routed there
// explicitly with TWOAI_LLM_<STAGE>=anthropic.
//
// This reverses the posture the file opened with, that a stopped local
// service should degrade to working-and-paid rather than to nothing. The
// owner has chosen nothing, and a stage that writes nothing says so in the
// log with the reason, which is the part that matters.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// twoaiLLMFor answers which provider a given stage should use.
//
//	TWOAI_LLM              default for every stage: "ollama" or "anthropic"
//	TWOAI_LLM_<STAGE>      override for one stage, same values
//
// Unset means ollama. Anthropic is reached only when a stage is routed there
// by name.
func twoaiLLMFor(stage string) string {
	if v := strings.TrimSpace(os.Getenv("TWOAI_LLM_" + strings.ToUpper(stage))); v != "" {
		return strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("TWOAI_LLM")); v != "" {
		return strings.ToLower(v)
	}
	return "ollama"
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
	// The default depends on where the request is going. Stephen, 2026-09-16:
	// move all to DeepSeek. Ollama Cloud's catalogue (read 2026-09-12 with
	// his key) carries the deepseek-v4 family; v4.1-flash is the one he chose
	// for the insurance seed and now for everything. Local keeps Mistral
	// Small, which fits a 12GB card, for anyone running without the cloud.
	if strings.Contains(twoaiOllamaHost(), "ollama.com") {
		return "deepseek-v4.1-flash"
	}
	return "mistral-small"
}

// twoaiOllamaTimeout is how long one non-streaming generation may take. 180
// seconds was fine for a summary. A large cloud model producing two thousand
// tokens of structured JSON can take longer than that to return its FIRST
// byte, because a non-streaming call answers only when it is finished, and
// the client reported that as "context deadline exceeded while awaiting
// headers" - which reads as the server being down when it was simply still
// writing. OLLAMA_TIMEOUT_SEC raises it for a run.
func twoaiOllamaTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("OLLAMA_TIMEOUT_SEC")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 180 * time.Second
}

// twoaiOllamaThink says whether a stage should ask the model to reason
// before answering. DeepSeek V4 Pro has three modes, no thinking, thinking
// and max thinking, and runs in the first unless the request says
// otherwise. Routing the judgment stages to the deep thinker and leaving
// thinking off would buy the price of the large model and none of the
// reasoning. OLLAMA_THINK_<STAGE>=true turns it on for one stage,
// OLLAMA_THINK=true for all. Thinking output arrives in its own field and
// never reaches the response text, so the parser sees only the answer.
func twoaiOllamaThink(stage string) bool {
	for _, k := range []string{"OLLAMA_THINK_" + strings.ToUpper(stage), "OLLAMA_THINK"} {
		if v := strings.ToLower(strings.TrimSpace(os.Getenv(k))); v != "" {
			return v == "1" || v == "true" || v == "yes" || v == "on"
		}
	}
	return false
}

// twoaiOllamaCall runs one completion against the local server. Same shape as
// twoaiClaudeCall so a stage can swap between them without knowing which it
// got.
func twoaiOllamaCall(model, system, user string) (string, error) {
	return twoaiOllamaCallThink(model, system, user, false)
}

func twoaiOllamaCallThink(model, system, user string, think bool) (string, error) {
	// THINKING SPENDS THE OUTPUT BUDGET. Reasoning tokens count against
	// num_predict. DeepSeek V4 Pro on the insurance template reasoned for
	// 52,000 to 66,000 characters on the harder items, about 13,000 to
	// 16,000 tokens, before writing a word of JSON - measured 2026-09-17 -
	// and a 16,000 budget cut it off at the answer. A retry re-spends all of
	// that thinking, so the budget is sixteen times the answer budget by
	// default, 64,000 on the standard 4,000. Stephen, on seeing the retries:
	// give it a bigger budget. The cost of unused headroom is nothing; the
	// cost of a cutoff is the whole thinking pass again. TWOAI_THINK_MULTIPLIER
	// tunes it.
	budget := twoaiMaxTokens()
	if think {
		mult := 16
		if v := strings.TrimSpace(os.Getenv("TWOAI_THINK_MULTIPLIER")); v != "" {
			fmt.Sscanf(v, "%d", &mult)
		}
		budget *= mult
	}
	payload := map[string]any{
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
			"num_predict": budget,
		},
	}
	if think {
		payload["think"] = true
	}
	body, _ := json.Marshal(payload)
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
	client := &http.Client{Timeout: twoaiOllamaTimeout()}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("ollama %d: %.200s", resp.StatusCode, b)
	}
	var out struct {
		Response   string `json:"response"`
		Thinking   string `json:"thinking"`
		Error      string `json:"error"`
		DoneReason string `json:"done_reason"`
		EvalCount  int    `json:"eval_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Error != "" {
		return "", fmt.Errorf("ollama: %s", out.Error)
	}
	if strings.TrimSpace(out.Response) == "" {
		// Say what actually happened, because the fix differs. A budget
		// exhausted by thinking wants a bigger num_predict; a genuinely empty
		// answer from a working model wants a different prompt. Neither is
		// the server being down.
		if out.DoneReason == "length" {
			return "", fmt.Errorf("output budget exhausted before the answer (eval_count=%d, thinking=%d chars); raise TWOAI_MAX_TOKENS", out.EvalCount, len(out.Thinking))
		}
		return "", fmt.Errorf("ollama returned an empty response (done_reason=%s, thinking=%d chars)", out.DoneReason, len(out.Thinking))
	}
	// Open models emit Unicode hyphens and dashes freely - U+2011 non-breaking
	// hyphen in "contact‑center", U+2010 in compounds - seen in every
	// gpt-oss:120b sample on 2026-09-12. They look right in a terminal and
	// break site search, since a reader types ASCII. Normalised here so no
	// stage has to remember. Em and en dashes become commas, per house style.
	r := strings.NewReplacer("\u2011", "-", "\u2010", "-", "\u2212", "-",
		"\u2014", ", ", "\u2013", ", ", " , ", ", ", ",,", ",")
	return r.Replace(out.Response), nil
}

// twoaiStripMarkdown removes heading and emphasis markup from model prose.
//
// Stephen, 2026-09-12: stray # headings visible on the news pages. 289 rows
// carried them - 103 news summaries, every one opening with a bare
// "# Summary" that adds nothing, and 186 analyses. Every prompt involved
// says no headings. The models emit them anyway, often enough that
// instructing harder is not the fix.
//
// These fields are rendered as PLAIN TEXT by the templates, not as Markdown,
// so a # reaches the reader as a # and a ** as two asterisks. The right
// place to handle that is here, once, on the way out of every model call,
// rather than in each of the dozen templates that render this prose or each
// of the prompts that fail to prevent it.
//
// A leading heading line is dropped entirely, because "# Summary" above a
// summary is a label, not content. Later headings keep their text and lose
// their hashes, because those usually do carry meaning.
var (
	mdLeadHeadRe = regexp.MustCompile(`\A#{1,6}[ \t]+[^\n]*\n+`)
	mdHeadRe     = regexp.MustCompile(`(?m)^#{1,6}[ \t]+`)
	mdBoldRe     = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
)

func twoaiStripMarkdown(s string) string {
	s = mdLeadHeadRe.ReplaceAllString(s, "")
	s = mdHeadRe.ReplaceAllString(s, "")
	s = mdBoldRe.ReplaceAllString(s, "$1")
	return strings.TrimSpace(s)
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
	// Fallback to Claude is off unless asked for by name.
	noFallback := !strings.EqualFold(strings.TrimSpace(os.Getenv("TWOAI_LLM_FALLBACK")), "claude")
	if want == "ollama" {
		ollamaMu.Lock()
		down := ollamaDown
		ollamaMu.Unlock()
		if !down {
			model := twoaiOllamaModel(stage)
			think := twoaiOllamaThink(stage)
			out, err := twoaiOllamaCallThink(model, system, user, think)
			if err == nil {
				return twoaiStripMarkdown(out), "ollama:" + model, nil
			}
			// A TIMEOUT IS NOT THE SERVER BEING DOWN, and neither is a reply
			// with nothing in it. Both are one call going wrong. Marking the
			// server down on a timeout sent every remaining insurance item to
			// Claude on 2026-09-16; marking it down on an empty response
			// stopped the whole run after one item on the 17th, when the
			// model had simply spent its budget thinking. Only a refused
			// connection or an HTTP error from the server marks it down.
			isTimeout := strings.Contains(err.Error(), "deadline exceeded") || strings.Contains(err.Error(), "Timeout")
			isPerCall := isTimeout || strings.Contains(err.Error(), "empty response") || strings.Contains(err.Error(), "budget exhausted")
			if isPerCall {
				fmt.Fprintf(os.Stderr, "twoai_llm: ollama call failed on %s (%v), retrying once\n", stage, err)
				if out, err2 := twoaiOllamaCallThink(model, system, user, think); err2 == nil {
					return twoaiStripMarkdown(out), "ollama:" + model, nil
				} else {
					err = err2
				}
				if noFallback {
					return "", "", fmt.Errorf("ollama failed twice: %w", err)
				}
			} else {
				// One failure marks the server down for the rest of the run. A
				// model that is not pulled, or a service that is stopped, fails
				// identically on every subsequent call.
				ollamaMu.Lock()
				ollamaDown = true
				ollamaMu.Unlock()
			}
			if noFallback {
				ollamaWarnOnce.Do(func() {
					fmt.Fprintf(os.Stderr,
						"twoai_llm: ollama unreachable at %s (%v); no fallback, nothing is written for the rest of this run\n",
						twoaiOllamaHost(), err)
				})
				return "", "", fmt.Errorf("ollama unavailable: %w", err)
			}
			ollamaWarnOnce.Do(func() {
				fmt.Fprintf(os.Stderr,
					"twoai_llm: ollama unreachable at %s (%v), falling back to Claude for the rest of this run\n",
					twoaiOllamaHost(), err)
			})
		} else if noFallback {
			return "", "", fmt.Errorf("ollama marked down earlier in this run")
		}
	}
	// Only reached when a stage is routed to anthropic by name, or fallback
	// was requested by name. Neither happens by default.
	model := os.Getenv("TWOAI_BRIEF_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	out, err := twoaiAnthropicCall(model, system, user)
	if err != nil {
		return "", "", err
	}
	return twoaiStripMarkdown(out), model, nil
}
