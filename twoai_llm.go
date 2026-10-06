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
	// The default depends on where the request is going. Stephen, 2026-09-17:
	// I want Pro on everything. So the cloud default is deepseek-v4-pro, the
	// 1.6T reasoning model, for every stage that does not name its own. Flash
	// remains available per stage with OLLAMA_MODEL_<STAGE>=deepseek-v4.1-flash
	// for anyone who wants the cheaper tier on an extractive job. Thinking is a
	// separate switch and stays off unless OLLAMA_THINK or OLLAMA_THINK_<STAGE>
	// turns it on, so Pro without thinking is still a fast intuitive answer.
	// Local keeps Mistral Small, which fits a 12GB card, for anyone running
	// without the cloud.
	// Stephen, 2026-10-02: the ask box on the site keeps Pro (twoai-site
	// worker.ts, its own default), everything in the pipeline runs Flash.
	// OLLAMA_MODEL and OLLAMA_MODEL_<STAGE> in pipeline.env still win, so a
	// stage that must have Pro names it there.
	if strings.Contains(twoaiOllamaHost(), "ollama.com") {
		return "deepseek-v4.1-flash"
	}
	return "mistral-small"
}

// PEAK HOURS. Stephen, 2026-10-02: off-peak pricing applies outside 12:00 to
// 18:00 UTC on weekdays and all day at weekends, and the bulk work should
// stay in those hours. twoaiPeakAt says whether a moment is inside the
// weekday peak window.
func twoaiPeakAt(t time.Time) bool {
	u := t.UTC()
	if u.Weekday() == time.Saturday || u.Weekday() == time.Sunday {
		return false
	}
	return u.Hour() >= 12 && u.Hour() < 18
}

// twoaiBulkStages are the stages that work through a backlog a few items a
// run and lose nothing by waiting: the next off-peak run picks up where
// this one would have. Daily news, the weekly recap and the mail router
// are not here, because their value is in being on time. TWOAI_PEAK_OK=1
// runs everything regardless, for a day when the backlog matters more than
// the price.
var twoaiBulkStages = map[string]bool{
	"page_readings": true, "sector_analysis": true, "twoai_source_pages": true, "art": true, "twoai_lang_pages": true,
	"point_briefs": true, "paper_explain": true, "news_mine": true, "ma_readings": true, "learning_readings": true,
	"benchmark_readings": true, "lawsuit_fill": true, "company_profiles": true, "case_studies": true, "vendor_enrich": true,
	// The enacted laws writer is the one stage pipeline.env keeps on Pro with
	// thinking, the dearest call in the run, and it works through a backlog.
	"ENACTED_LAWS": true,
	// CVE headlines and defence sections, a 600 row backlog at a dozen a run.
	"cve_writer": true,
	// The why-it-matters line on news stories, ten a run over the live archive.
	"news_why": true,
	// The AI paragraph on each CWE page, 107 classes, once a month at most.
	"cwe_writer": true,
	// The strengths and limits reading on each model family page, three a run.
	"model_family_reading": true,
	// Topic sorting for the IntuitionLabs library on Healthcare, a 530 title backlog.
	"library_topics": true, "ext_summaries": true, "ext_fact_check": true,
	// Every visitor-facing table put into English, sixty values a run (bridge
	// row 508). The harvest-time translator, translate_title, is not here: a
	// new headline must be English the day it is shown.
	"twoai_english_sweep": true,
}

var twoaiPeakNoted sync.Map

// twoaiDeferForPeak reports whether a stage's model work is held for
// off-peak hours right now, saying so once per stage per run.
func twoaiDeferForPeak(stage string) bool {
	if !twoaiBulkStages[stage] || !twoaiPeakAt(time.Now()) {
		return false
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("TWOAI_PEAK_OK"))); v == "1" || v == "true" || v == "yes" || v == "on" {
		return false
	}
	if _, seen := twoaiPeakNoted.LoadOrStore(stage, true); !seen {
		fmt.Printf("%s: deferred, peak pricing hours (Mon-Fri 12:00-18:00 UTC), the next off-peak run does this work\n", stage)
	}
	return true
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
	// THE MODEL DECIDES WHETHER TO THINK, so the budget cannot depend on the
	// flag. The scheduled run of 2026-09-17 had no thinking flag set for the
	// enacted-laws stage; DeepSeek Pro reasoned for 18,000 characters on each
	// statute regardless, hit the 4,000 answer budget before writing a word,
	// and twenty bills failed twice each. Reasoning tokens count against
	// num_predict whether or not the request asked for them. Unused headroom
	// costs nothing, so the ceiling is sixteen times the answer budget for
	// every call, 64,000 on the standard 4,000. TWOAI_THINK_MULTIPLIER tunes
	// it. The think flag now only controls whether the request ASKS for
	// reasoning; it never narrows the room to finish.
	mult := 16
	if v := strings.TrimSpace(os.Getenv("TWOAI_THINK_MULTIPLIER")); v != "" {
		fmt.Sscanf(v, "%d", &mult)
	}
	budget := twoaiMaxTokens() * mult
	// Context window. 8,192 fits a summary job. A statute does not: the
	// enacted-laws stage hands the model a whole bill, and a 40,000 token bill
	// in an 8,192 window is read from the middle with the title cut off.
	// OLLAMA_NUM_CTX raises it; Pro takes a million.
	numCtx := 8192
	if v := strings.TrimSpace(os.Getenv("OLLAMA_NUM_CTX")); v != "" {
		fmt.Sscanf(v, "%d", &numCtx)
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
			"num_ctx":     numCtx,
			"num_predict": budget,
		},
	}
	if think {
		payload["think"] = true
	} else if strings.Contains(strings.ToLower(model), "deepseek") {
		// SAY NO, DO NOT JUST FAIL TO SAY YES. deepseek-v4.1-flash thinks
		// unless told not to: the 06:05 run of 2026-10-02 spent the whole
		// 64,000 token budget on 286,291 characters of thinking for a
		// language page nobody asked it to reason about, three times in one
		// run, each one paid for and each one retried. An explicit false
		// turns it off. Only sent to models known to accept the flag, since
		// a local model that does not would refuse the request outright.
		payload["think"] = false
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
			if twoaiDeferForPeak(stage) {
				return "", "", fmt.Errorf("%s: deferred to off-peak hours", stage)
			}
			model := twoaiOllamaModel(stage)
			think := twoaiOllamaThink(stage)
			out, err := twoaiOllamaCallThink(model, system, user, think)
			if err == nil {
				return twoaiStripMarkdown(out), "ollama:" + model, nil
			}
			// THINKING THAT SPENDS THE WHOLE BUDGET IS NOT AN ANSWER. Row 357,
			// 2026-10-02: a twoai_lang_pages call on a thinking model hit
			// eval_count=64000 with nothing in the response. The same request
			// is made once more without thinking, which is a fast intuitive
			// answer from the same model, before anything is given up on.
			if think && strings.Contains(err.Error(), "budget exhausted") {
				fmt.Fprintf(os.Stderr, "%s: thinking spent the output budget, retrying without it\n", stage)
				if out2, err2 := twoaiOllamaCallThink(model, system, user, false); err2 == nil {
					return twoaiStripMarkdown(out2), "ollama:" + model, nil
				}
			}
			// A TIMEOUT IS NOT THE SERVER BEING DOWN, and neither is a reply
			// with nothing in it. Both are one call going wrong. Marking the
			// server down on a timeout sent every remaining insurance item to
			// Claude on 2026-09-16; marking it down on an empty response
			// stopped the whole run after one item on the 17th, when the
			// model had simply spent its budget thinking. Only a refused
			// connection or an HTTP error from the server marks it down.
			isTimeout := strings.Contains(err.Error(), "deadline exceeded") || strings.Contains(err.Error(), "Timeout")
			// A 5xx from the server is one request the server could not serve,
			// not the server being gone; Ollama Cloud returned a 500 with a
			// reference id on the first statute of 2026-09-17 and answered the
			// next request normally. Only a refused connection, a DNS failure or
			// a 4xx that says the key or model is wrong marks it down.
			is5xx := strings.Contains(err.Error(), "ollama 5")
			isPerCall := isTimeout || is5xx || strings.Contains(err.Error(), "empty response") || strings.Contains(err.Error(), "budget exhausted")
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
	// NO ANTHROPIC, BY ANY ROUTE. Stephen, 2026-09-17: I am still being charged,
	// cut all ties with the API. Until today a stage could still be sent there
	// with TWOAI_LLM=anthropic, TWOAI_LLM_<STAGE>=anthropic or
	// TWOAI_LLM_FALLBACK=claude. Those switches now do nothing but fail loudly,
	// so a stale env var on a cron cannot spend money. twoaiAnthropicCall stays
	// in the tree uncalled; bringing it back is a code change, not a setting.
	return "", "", fmt.Errorf("stage %s is routed to anthropic, which was removed on 2026-09-17: unset TWOAI_LLM, TWOAI_LLM_%s and TWOAI_LLM_FALLBACK", stage, strings.ToUpper(stage))
}
