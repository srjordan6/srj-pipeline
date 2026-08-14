package main

// ---- twoai_repos: inference engines and AI frameworks, from GitHub --------
//
// The serving layer and the framework layer are both open source almost
// without exception, so the honest census is the repositories themselves:
// stars, licence, primary language, last push, and the project's own
// description. The repo list is curated (these are the canonical projects,
// each individually verified to be the official repository); the numbers on
// the page come from the GitHub REST API on every run.
//
// Unauthenticated GitHub allows 60 requests/hour per IP; one run costs one
// request per repo (57 total). GITHUB_API_TOKEN or GITHUB_TOKEN in the cron
// env, raises the ceiling to 5,000. A failed fetch keeps the previous rows.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"
)

var twoaiRepoSections = map[string][]string{
	"inference-engines": {
		"vllm-project/vllm", "ggml-org/llama.cpp", "NVIDIA/TensorRT-LLM",
		"sgl-project/sglang", "huggingface/text-generation-inference",
		"microsoft/onnxruntime", "mlc-ai/mlc-llm", "InternLM/lmdeploy",
		"ollama/ollama", "turboderp/exllamav2", "OpenNMT/CTranslate2",
		"Mozilla-Ocho/llamafile",
	},
	"ai-frameworks": {
		"langchain-ai/langchain", "run-llama/llama_index", "stanfordnlp/dspy",
		"microsoft/semantic-kernel", "microsoft/autogen", "crewAIInc/crewAI",
		"deepset-ai/haystack", "BerriAI/litellm", "pydantic/pydantic-ai",
		"huggingface/transformers", "huggingface/peft", "unslothai/unsloth",
		"pytorch/pytorch", "tensorflow/tensorflow", "keras-team/keras",
		"jax-ml/jax", "vercel/ai",
	},
	"autonomous-agents": {
		"Significant-Gravitas/AutoGPT", "OpenHands/OpenHands",
		"AntonOsika/gpt-engineer", "langroid/langroid",
	},
	"multi-agent-systems": {
		"microsoft/autogen", "crewAIInc/crewAI", "camel-ai/camel",
		"openai/swarm", "a2aproject/A2A",
	},
	"agent-memory-and-planning": {
		"mem0ai/mem0", "letta-ai/letta", "getzep/zep",
	},
	"coding-and-browser-agents": {
		"Aider-AI/aider", "cline/cline", "continuedev/continue",
		"browser-use/browser-use", "browserbase/stagehand",
	},
	"voice-and-service-agents": {
		"livekit/agents", "pipecat-ai/pipecat", "TEN-framework/ten-framework",
	},
	"mcp-sdks-registries": {
		"modelcontextprotocol/typescript-sdk", "modelcontextprotocol/python-sdk",
		"modelcontextprotocol/go-sdk", "modelcontextprotocol/java-sdk",
		"modelcontextprotocol/csharp-sdk", "modelcontextprotocol/registry",
		"modelcontextprotocol/inspector", "modelcontextprotocol/servers",
	},
}

func twoaiReposEnsure(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_repo_catalog (
		section text NOT NULL,
		repo text NOT NULL,
		data jsonb NOT NULL,
		fetched_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (section, repo))`)
	return err
}

func twoaiRepos(db *sql.DB, today string) (int, error) {
	if err := twoaiReposEnsure(db); err != nil {
		return 0, err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	fetch := func(repo string) (map[string]any, error) {
		req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+repo, nil)
		req.Header.Set("User-Agent", "theworldofai.org pipeline (contact: info@srjconsultingservices.com)")
		req.Header.Set("Accept", "application/vnd.github+json")
		// GITHUB_API_TOKEN if present, else the cron's existing GITHUB_TOKEN
		// (the contents-API PAT). Authentication alone lifts the API limit
		// from 60/hr to 5,000/hr, which the 57-repo sweep needs; no extra
		// permission is used or required.
		tok := os.Getenv("GITHUB_API_TOKEN")
		if tok == "" {
			tok = os.Getenv("GITHUB_TOKEN")
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("status %d", resp.StatusCode)
		}
		var d map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
			return nil, err
		}
		return d, nil
	}

	fetched, failed := 0, 0
	for section, repos := range twoaiRepoSections {
		for _, repo := range repos {
			d, err := fetch(repo)
			time.Sleep(300 * time.Millisecond)
			if err != nil {
				failed++
				fmt.Fprintf(os.Stderr, "twoai_repos: %s: %v (previous row keeps rendering)\n", repo, err)
				continue
			}
			lic := ""
			if l, ok := d["license"].(map[string]any); ok {
				lic, _ = l["spdx_id"].(string)
				if lic == "NOASSERTION" {
					lic = "custom"
				}
			}
			row := map[string]any{
				"repo":        repo,
				"name":        d["name"],
				"description": d["description"],
				"stars":       d["stargazers_count"],
				"language":    d["language"],
				"licence":     lic,
				"pushed_at":   d["pushed_at"],
				"archived":    d["archived"],
				"url":         d["html_url"],
				"homepage":    d["homepage"],
			}
			j, _ := json.Marshal(row)
			if _, err := db.Exec(`INSERT INTO twoai_repo_catalog (section, repo, data, fetched_at)
				VALUES ($1,$2,$3::jsonb, now())
				ON CONFLICT (section, repo) DO UPDATE SET data=EXCLUDED.data, fetched_at=now()`,
				section, repo, string(j)); err != nil {
				return 0, err
			}
			fetched++
		}
	}
	fmt.Printf("twoai_repos: fetched=%d failed=%d\n", fetched, failed)

	// Render one section page per key, from whatever the table holds.
	count := 0
	for section := range twoaiRepoSections {
		rows, err := db.Query(`SELECT data FROM twoai_repo_catalog WHERE section=$1`, section)
		if err != nil {
			return count, err
		}
		var repos []map[string]any
		for rows.Next() {
			var raw string
			if rows.Scan(&raw) != nil {
				continue
			}
			var d map[string]any
			if json.Unmarshal([]byte(raw), &d) == nil {
				repos = append(repos, d)
			}
		}
		rows.Close()
		if len(repos) == 0 {
			continue
		}
		sort.Slice(repos, func(i, j int) bool {
			a, _ := repos[i]["stars"].(float64)
			b, _ := repos[j]["stars"].(float64)
			return a > b
		})
		licences := map[string]int{}
		var totalStars float64
		for _, r := range repos {
			if l, _ := r["licence"].(string); l != "" {
				licences[l]++
			}
			if s, ok := r["stars"].(float64); ok {
				totalStars += s
			}
		}
		type lic struct {
			Licence string `json:"licence"`
			Count   int    `json:"count"`
		}
		var lics []lic
		for k, n := range licences {
			lics = append(lics, lic{k, n})
		}
		sort.Slice(lics, func(i, j int) bool { return lics[i].Count > lics[j].Count })

		var name, blurb string
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug=$1`, section).Scan(&name, &blurb)
		doc := map[string]any{
			"uid": twoaiUID("section:" + section), "tax": section,
			"name": name, "blurb": blurb, "source": "github",
			"repos": repos, "total": len(repos), "licences": lics,
			"stars_total": int(totalStars), "generated": today,
		}
		j, _ := json.Marshal(doc)
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ($1,'repo-section',$2::jsonb,$3,1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`,
			"repos/"+section+".json", string(j), section); err != nil {
			return count, err
		}
		count++
	}
	// ---- MCP Clients: curated applications that speak the protocol, each
	// row verified against its own site; the protocol's official client
	// list is the canonical registry and is linked as such.
	type clientRow struct {
		Name       string `json:"name"`
		Publisher  string `json:"publisher"`
		URL        string `json:"url"`
		OpenSource bool   `json:"open_source"`
		Repo       string `json:"repo,omitempty"`
		Note       string `json:"note"`
		Verified   string `json:"verified"`
	}
	var clients []clientRow
	if rows, err := db.Query(`SELECT name, publisher, url, open_source, COALESCE(repo,''), note, verified_on::text
		FROM twoai_mcp_clients ORDER BY name`); err == nil {
		for rows.Next() {
			var c clientRow
			if rows.Scan(&c.Name, &c.Publisher, &c.URL, &c.OpenSource, &c.Repo, &c.Note, &c.Verified) == nil {
				clients = append(clients, c)
			}
		}
		rows.Close()
	}
	if len(clients) > 0 {
		var cn, cb string
		db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug='mcp-clients'`).Scan(&cn, &cb)
		oss := 0
		for _, c := range clients {
			if c.OpenSource {
				oss++
			}
		}
		cj, _ := json.Marshal(map[string]any{
			"uid": twoaiUID("section:mcp-clients"), "tax": "mcp-clients",
			"name": cn, "blurb": cb, "clients": clients, "total": len(clients),
			"open_source": oss, "generated": today,
		})
		if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
			VALUES ('repos/mcp-clients.json','tech-section',$1::jsonb,'mcp-clients',1)
			ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
				taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`, string(cj)); err != nil {
			return count, err
		}
		count++
	}

	// ---- MCP Security and Authentication: the protocol's security model
	// documented from the specification itself, sources linked per point.
	var sn, sb string
	db.QueryRow(`SELECT name, COALESCE(blurb,'') FROM twoai_taxonomy WHERE slug='mcp-security'`).Scan(&sn, &sb)
	specAuth := "https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization"
	specSec := "https://modelcontextprotocol.io/specification/2025-06-18/basic/security_best_practices"
	sj, _ := json.Marshal(map[string]any{
		"uid": twoaiUID("section:mcp-security"), "tax": "mcp-security",
		"name": sn, "blurb": sb, "generated": today, "kind": "standard",
		"points": []map[string]string{
			{"name": "Authorization is OAuth 2.1", "source": specAuth,
				"desc": "Remote MCP servers authenticate clients with OAuth 2.1: authorization-server discovery, dynamic client registration, and PKCE. Local stdio servers inherit the trust of the process that launched them instead."},
			{"name": "The trust boundary sits at the server", "source": specSec,
				"desc": "Every connected server can read what the client sends it and shape what the model sees in return. Connecting a server is granting it a capability, and the specification requires explicit user consent for tools and resources."},
			{"name": "Tool descriptions are attack surface", "source": specSec,
				"desc": "A malicious server can carry instructions in tool names, descriptions, or results that try to steer the model - prompt injection by tool metadata. Clients are expected to show users what a tool will do before it runs."},
			{"name": "Confused-deputy and token-passthrough failures", "source": specSec,
				"desc": "The specification forbids passing a client's token through to upstream APIs and warns against proxy patterns where a server exercises a user's authority beyond what was consented."},
			{"name": "Credentials live with the user, not the model", "source": specAuth,
				"desc": "Secrets flow through the authorization layer, never through model-visible text; a server that asks the model for a password is misdesigned by specification."},
		},
	})
	if _, err := db.Exec(`INSERT INTO twoai_pages (path, kind, data, taxonomy_slug, url_count)
		VALUES ('repos/mcp-security.json','tech-section',$1::jsonb,'mcp-security',1)
		ON CONFLICT (path) DO UPDATE SET kind=EXCLUDED.kind, data=EXCLUDED.data,
			taxonomy_slug=EXCLUDED.taxonomy_slug, url_count=1, updated_at=now()`, string(sj)); err != nil {
		return count, err
	}
	count++

	return count, nil
}
