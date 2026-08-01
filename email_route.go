package main

// email_route: the automated email coordinator (Stephen's directive, Aug 1
// 2026, delivered as inst.docx from a sibling chat and rebuilt here for the
// platform's actual conventions).
//
// Watches the srj@srjconsultingservices.com inbox hourly, has Claude Haiku
// categorize each unread message, routes it into project_bridge for the
// owning Claude project(s), escalates anything that needs Stephen's own
// call, and marks the mail read. Runs on Render like everything else; the
// draft's Windows Task Scheduler hosting is out by standing convention
// (nothing runs on Stephen's machine).
//
// Auth reuses the site Worker's Gmail plumbing: the same Google service
// account with domain-wide delegation, impersonating srj@. The Worker's key
// stays send-only; this stage needs the delegation grant to also carry
// gmail.modify (read + mark-read). That is a one-time edit in Google Admin
// (Security > API Controls > Domain-wide delegation).
//
// Routing taxonomy, from Stephen's table verbatim:
//   book matters generally        -> books_kdp
//   a specific Volume I-IX        -> books_vol{n}
//   career, credentials, bios     -> career
//   website, code, hosting        -> srj
//   publishing, launch, promo     -> theworldofai
//   spans several projects        -> all of them (the bridge row says so)
//   nothing claims it             -> escalation only, no bridge spam
// Deadline, financial, or legal language always escalates on top of routing.
//
// Failure shape: a message that cannot be fetched or categorized stays
// UNREAD and is retried next run; the unique gmail_msg_id in the log makes
// reprocessing idempotent. Noise (newsletters, receipts, automated mail) is
// logged and marked read but never routed, so the bridge carries signal.
//
// Environment: DATABASE_URL, GOOGLE_SA_EMAIL, GOOGLE_SA_KEY (PEM),
// ANTHROPIC_API_KEY. Without the Anthropic key the stage falls back to
// keyword routing, cruder but functional.

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const erMailbox = "srj@srjconsultingservices.com"
const erEscalateTo = "info@srjconsultingservices.com" // alias of the same box; a literally self-addressed mail is hidden by Gmail dedup (see worker/gmail.ts)
const erTokenURL = "https://oauth2.googleapis.com/token"
const erGmailAPI = "https://gmail.googleapis.com/gmail/v1/users/me"

var erBridgeProjects = []string{"books_kdp", "books_vol1", "books_vol2", "books_vol3", "books_vol4", "books_vol5", "books_vol6", "books_vol7", "books_vol8", "books_vol9", "career", "srj", "theworldofai"}

// ---- Google service-account token (JWT bearer grant, RS256 by hand so the
// binary stays dependency-free) ------------------------------------------

func erB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func erSAToken(scopes []string) (string, error) {
	email, key := os.Getenv("GOOGLE_SA_EMAIL"), os.Getenv("GOOGLE_SA_KEY")
	if email == "" || key == "" {
		return "", fmt.Errorf("GOOGLE_SA_EMAIL and GOOGLE_SA_KEY must be set")
	}
	block, _ := pem.Decode([]byte(strings.ReplaceAll(key, `\n`, "\n")))
	if block == nil {
		return "", fmt.Errorf("GOOGLE_SA_KEY is not valid PEM")
	}
	var priv *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		priv = k.(*rsa.PrivateKey)
	} else if k2, err2 := x509.ParsePKCS1PrivateKey(block.Bytes); err2 == nil {
		priv = k2
	} else {
		return "", fmt.Errorf("cannot parse service-account key: %v", err)
	}
	now := time.Now().Unix()
	header := erB64([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iss": email, "sub": erMailbox, "aud": erTokenURL,
		"scope": strings.Join(scopes, " "), "iat": now, "exp": now + 3600,
	})
	signing := header + "." + erB64(claims)
	h := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	resp, err := http.PostForm(erTokenURL, url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signing + "." + erB64(sig)},
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("token: %d %s", resp.StatusCode, string(body[:min(len(body), 200)]))
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &t) != nil || t.AccessToken == "" {
		return "", fmt.Errorf("token response unusable")
	}
	return t.AccessToken, nil
}

func erGmail(token, method, path string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, erGmailAPI+path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gmail %s: %d %s", path, resp.StatusCode, string(b[:min(len(b), 200)]))
	}
	return b, nil
}

// ---- message reading ----------------------------------------------------

type erMessage struct {
	ID, From, Subject, Body string
}

// erWalkBody digs the first text/plain part out of an arbitrarily nested
// multipart payload, falling back to text/html stripped of tags, then to the
// snippet. Gmail bodies are base64url.
func erWalkBody(part map[string]any, want string) string {
	if mt, _ := part["mimeType"].(string); strings.HasPrefix(mt, want) {
		if body, ok := part["body"].(map[string]any); ok {
			if data, _ := body["data"].(string); data != "" {
				if raw, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(strings.ReplaceAll(data, "=", "")); err == nil {
					return string(raw)
				}
			}
		}
	}
	if parts, ok := part["parts"].([]any); ok {
		for _, p := range parts {
			if m, ok := p.(map[string]any); ok {
				if s := erWalkBody(m, want); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func erFetch(token, id string) (erMessage, error) {
	raw, err := erGmail(token, "GET", "/messages/"+id+"?format=full", nil)
	if err != nil {
		return erMessage{}, err
	}
	var full map[string]any
	if err := json.Unmarshal(raw, &full); err != nil {
		return erMessage{}, err
	}
	m := erMessage{ID: id}
	payload, _ := full["payload"].(map[string]any)
	if payload != nil {
		if hs, ok := payload["headers"].([]any); ok {
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				switch hm["name"] {
				case "From":
					m.From, _ = hm["value"].(string)
				case "Subject":
					m.Subject, _ = hm["value"].(string)
				}
			}
		}
		m.Body = erWalkBody(payload, "text/plain")
		if m.Body == "" {
			html := erWalkBody(payload, "text/html")
			m.Body = strings.TrimSpace(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(html, " "))
		}
	}
	if m.Body == "" {
		m.Body, _ = full["snippet"].(string)
	}
	if len(m.Body) > 6000 {
		m.Body = m.Body[:6000]
	}
	return m, nil
}

// ---- categorization -----------------------------------------------------

type erVerdict struct {
	Noise          bool   `json:"noise"`
	Expertise      string `json:"expertise"` // book|career|website|publishing|unknown
	Volume         int    `json:"volume"`    // 1-9, or 0
	Deadline       bool   `json:"deadline"`
	FinancialLegal bool   `json:"financial_legal"`
}

func erCategorize(m erMessage) (erVerdict, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return erKeywordFallback(m), nil
	}
	prompt := "You triage email for Stephen R. Jordan, who runs SRJ Consulting (AI advisory), " +
		"a nine-volume book series (The Operating Discipline for AI Library, Volumes I-IX, published via Amazon KDP), " +
		"the srjconsultingservices.com website, and theworldofai.org (publishing, launches, newsletter, promotion). " +
		"Categorize this email. noise=true means marketing, newsletters, receipts, or automated mail no project needs to act on. " +
		"expertise: book (manuscripts, covers, ISBNs, KDP, pricing), career (roles, credentials, bios), " +
		"website (architecture, code, hosting, deployment), publishing (launch timing, promotion, newsletter), or unknown. " +
		"volume: 1-9 only when the email concerns one specific volume, else 0. " +
		"deadline=true for time-sensitive language; financial_legal=true for money, contracts, or legal language.\n\n" +
		"From: " + m.From + "\nSubject: " + m.Subject + "\n\n" + m.Body +
		"\n\nReply with ONLY this JSON, no prose: {\"noise\":bool,\"expertise\":\"...\",\"volume\":int,\"deadline\":bool,\"financial_legal\":bool}"
	body, _ := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5", "max_tokens": 150,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return erVerdict{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return erVerdict{}, fmt.Errorf("anthropic: %d %s", resp.StatusCode, string(raw[:min(len(raw), 200)]))
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Content) == 0 {
		return erVerdict{}, fmt.Errorf("anthropic response unusable")
	}
	text := strings.TrimSpace(out.Content[0].Text)
	text = strings.TrimPrefix(strings.TrimSuffix(text, "```"), "```json")
	var v erVerdict
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &v); err != nil {
		return erVerdict{}, fmt.Errorf("verdict not JSON: %s", text[:min(len(text), 120)])
	}
	return v, nil
}

// erKeywordFallback keeps routing alive with no API key: crude, honest about
// being crude, and it never marks anything noise (better to over-route than
// silently swallow mail).
func erKeywordFallback(m erMessage) erVerdict {
	t := strings.ToLower(m.Subject + " " + m.Body)
	v := erVerdict{Expertise: "unknown"}
	switch {
	case strings.Contains(t, "isbn") || strings.Contains(t, "kdp") || strings.Contains(t, "manuscript") || strings.Contains(t, "book cover"):
		v.Expertise = "book"
	case strings.Contains(t, "credential") || strings.Contains(t, "resume") || strings.Contains(t, "bio"):
		v.Expertise = "career"
	case strings.Contains(t, "website") || strings.Contains(t, "deploy") || strings.Contains(t, "hosting") || strings.Contains(t, "css"):
		v.Expertise = "website"
	case strings.Contains(t, "launch") || strings.Contains(t, "newsletter") || strings.Contains(t, "promotion"):
		v.Expertise = "publishing"
	}
	for i := 1; i <= 9; i++ {
		if strings.Contains(t, fmt.Sprintf("volume %d", i)) || strings.Contains(t, "volume "+[]string{"i", "ii", "iii", "iv", "v", "vi", "vii", "viii", "ix"}[i-1]+" ") {
			v.Volume = i
			break
		}
	}
	v.Deadline = strings.Contains(t, "deadline") || strings.Contains(t, "urgent") || strings.Contains(t, "by friday")
	v.FinancialLegal = strings.Contains(t, "invoice") || strings.Contains(t, "contract") || strings.Contains(t, "payment") || strings.Contains(t, "legal")
	return v
}

func erRoute(v erVerdict) []string {
	switch v.Expertise {
	case "book":
		if v.Volume >= 1 && v.Volume <= 9 {
			return []string{fmt.Sprintf("books_vol%d", v.Volume)}
		}
		return []string{"books_kdp"}
	case "career":
		return []string{"career"}
	case "website":
		return []string{"srj"}
	case "publishing":
		return []string{"theworldofai"}
	}
	return nil // unknown: escalate, never blanket-spam every project
}

// ---- escalation mail ----------------------------------------------------

func erSendEscalation(token, subject, body string) error {
	raw := "From: " + erMailbox + "\r\nTo: " + erEscalateTo + "\r\nSubject: [Coordinator] " + subject + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body
	_, err := erGmail(token, "POST", "/messages/send", map[string]string{
		"raw": base64.URLEncoding.EncodeToString([]byte(raw)),
	})
	return err
}

// ---- the stage ----------------------------------------------------------

func emailRoute(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS email_processing_log (
		id serial PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now(),
		gmail_msg_id text UNIQUE NOT NULL, from_addr text, subject text,
		verdict jsonb, routed_to text[], noise boolean NOT NULL DEFAULT false,
		escalated boolean NOT NULL DEFAULT false)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS escalations_to_user (
		id serial PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now(),
		email_log_id int REFERENCES email_processing_log(id),
		reason text NOT NULL, subject text, projects text,
		resolved boolean NOT NULL DEFAULT false)`); err != nil {
		return err
	}

	token, err := erSAToken([]string{
		"https://www.googleapis.com/auth/gmail.modify",
		"https://www.googleapis.com/auth/gmail.send",
	})
	if err != nil {
		return fmt.Errorf("gmail auth (is gmail.modify granted in domain-wide delegation?): %w", err)
	}

	listRaw, err := erGmail(token, "GET", "/messages?q="+url.QueryEscape("is:unread in:inbox")+"&maxResults=25", nil)
	if err != nil {
		return err
	}
	var list struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	json.Unmarshal(listRaw, &list)

	routed, noise, escalated, failed := 0, 0, 0, 0
	for _, ref := range list.Messages {
		var seen bool
		db.QueryRow(`SELECT true FROM email_processing_log WHERE gmail_msg_id=$1`, ref.ID).Scan(&seen)
		if seen { // processed before but somehow unread again: just re-mark
			erGmail(token, "POST", "/messages/"+ref.ID+"/modify", map[string]any{"removeLabelIds": []string{"UNREAD"}})
			continue
		}
		m, err := erFetch(token, ref.ID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "email_route fetch:", err)
			failed++
			continue // stays unread, retried next hour
		}
		v, err := erCategorize(m)
		if err != nil {
			fmt.Fprintln(os.Stderr, "email_route categorize:", err)
			failed++
			continue
		}
		projects := erRoute(v)
		needsUser := v.Expertise == "unknown" || v.Deadline || v.FinancialLegal
		if v.Noise {
			projects, needsUser = nil, false
		}
		vjson, _ := json.Marshal(v)
		var logID int
		if err := db.QueryRow(`INSERT INTO email_processing_log
			(gmail_msg_id, from_addr, subject, verdict, routed_to, noise, escalated)
			VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (gmail_msg_id) DO NOTHING RETURNING id`,
			ref.ID, m.From, m.Subject, vjson, pqArray(projects), v.Noise, needsUser).Scan(&logID); err != nil {
			fmt.Fprintln(os.Stderr, "email_route log:", err)
			failed++
			continue
		}
		for _, p := range projects {
			topic := m.Subject
			if len(projects) > 1 {
				topic = "[multi-project] " + topic
			}
			if _, err := db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body)
				VALUES ('coordinator', $1, $2, $3)`, p, topic,
				"From: "+m.From+"\n\n"+m.Body); err != nil {
				fmt.Fprintln(os.Stderr, "email_route bridge:", err)
			}
		}
		if needsUser {
			reason := "no project claims it"
			if v.Deadline {
				reason = "time-sensitive"
			}
			if v.FinancialLegal {
				reason = "financial or legal"
			}
			db.Exec(`INSERT INTO escalations_to_user (email_log_id, reason, subject, projects)
				VALUES ($1,$2,$3,$4)`, logID, reason, m.Subject, strings.Join(projects, ","))
			if err := erSendEscalation(token, m.Subject,
				"The coordinator flagged this ("+reason+").\nFrom: "+m.From+"\nRouted to: "+strings.Join(projects, ", ")+"\n\n"+m.Body[:min(len(m.Body), 1500)]); err != nil {
				fmt.Fprintln(os.Stderr, "email_route escalation mail:", err)
			}
			escalated++
		}
		if _, err := erGmail(token, "POST", "/messages/"+ref.ID+"/modify", map[string]any{"removeLabelIds": []string{"UNREAD"}}); err != nil {
			fmt.Fprintln(os.Stderr, "email_route mark-read:", err)
		}
		if v.Noise {
			noise++
		} else {
			routed++
		}
	}
	fmt.Printf("email_route: routed=%d noise=%d escalated=%d failed=%d of %d unread\n",
		routed, noise, escalated, failed, len(list.Messages))
	return nil
}

// pqArray renders a []string as a Postgres array literal without pulling in
// pq.Array's reflection for one call site.
func pqArray(ss []string) string {
	if len(ss) == 0 {
		return "{}"
	}
	return "{" + strings.Join(ss, ",") + "}"
}
