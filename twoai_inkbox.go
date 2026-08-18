package main

// inkbox_pull: mirrors the Inkbox mailboxes and A2A task inboxes into
// project_bridge, so the two inboxes a project has become one place to look.
//
// The reason this stage exists at all: a Claude session reads project_bridge
// at start and can query Postgres directly, but it cannot reach Inkbox. Mail
// that stays in Inkbox is mail nobody sees until a human runs the CLI. Landing
// it in SQL is what makes an arriving message actually arrive.
//
// Idempotency is a sync log keyed by (kind, external_id) with a unique index,
// so a re-run inserts nothing twice even if the mark-read call failed last
// time. Mail is marked read after it is mirrored, matching what email_route
// does with Gmail; A2A tasks have no read state, so the log is the only guard.
//
// Auth: each identity's own agent-scoped key from INKBOX_KEY_<HANDLE>, falling
// back to the org key in INKBOX_API_KEY. An identity with neither is skipped
// with a line saying so, never silently.
//
// The response shapes are read defensively through map[string]any rather than
// fixed structs. Inkbox's published reference documents the paths and the
// parameters but not the field names, so a stage that assumed them would fail
// as a nil dereference on a field rename. Instead a message whose id cannot be
// found is counted and its key set is logged once, which says what to fix.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const ibAPI = "https://inkbox.ai/api/v1"

// ibIdentity ties an Inkbox handle to the project_bridge mailbox it feeds.
// to_project must match project_registry.project_key, because that is what a
// session looks itself up by.
type ibIdentity struct {
	handle  string
	mailbox string
	project string
}

var ibIdentities = []ibIdentity{
	{handle: "theworldofai", mailbox: "theworldofai@inkboxmail.com", project: "theworldofai"},
	{handle: "srj", mailbox: "srj@inkboxmail.com", project: "srj"},
	{handle: "coordinator", mailbox: "coordinator@inkboxmail.com", project: "coordinator"},
}

func ibKey(handle string) string {
	v := os.Getenv("INKBOX_KEY_" + strings.ToUpper(strings.ReplaceAll(handle, "-", "_")))
	if v == "" {
		v = os.Getenv("INKBOX_API_KEY")
	}
	return v
}

func ibDo(key, method, path string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ibAPI+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", key)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("inkbox %s %s: %d %s", method, path, resp.StatusCode, string(b[:min(len(b), 200)]))
	}
	return b, nil
}

// ibItems pulls the list out of whichever envelope the endpoint uses. Inkbox
// returns {"items":[...]} on the endpoints seen so far, but a bare array and a
// {"data":[...]} envelope both cost one line to tolerate and save a failed run.
func ibItems(raw []byte) []map[string]any {
	var asObj map[string]any
	if json.Unmarshal(raw, &asObj) == nil {
		for _, k := range []string{"items", "data", "messages", "tasks", "results"} {
			if arr, ok := asObj[k].([]any); ok {
				return ibCast(arr)
			}
		}
	}
	var asArr []any
	if json.Unmarshal(raw, &asArr) == nil {
		return ibCast(asArr)
	}
	return nil
}

func ibCast(arr []any) []map[string]any {
	out := make([]map[string]any, 0, len(arr))
	for _, v := range arr {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// ibStr returns the first non-empty string among the given keys, descending one
// level into nested objects for dotted keys like "from.address".
func ibStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		cur := any(m)
		ok := true
		for _, part := range strings.Split(k, ".") {
			mm, isMap := cur.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			cur, ok = mm[part]
			if !ok {
				break
			}
		}
		if !ok {
			continue
		}
		if s, isStr := cur.(string); isStr && s != "" {
			return s
		}
	}
	return ""
}

func ibKeys(m map[string]any) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(out, ", ")
}

// ibRecord writes one item into the sync log and, when it is new, into the
// destination project's bridge mailbox. Returns true when it was new.
func ibRecord(db *sql.DB, kind, externalID, project, topic, body string) (bool, error) {
	var logID int
	err := db.QueryRow(`INSERT INTO inkbox_sync_log (kind, external_id, to_project, topic)
		VALUES ($1,$2,$3,$4) ON CONFLICT (kind, external_id) DO NOTHING RETURNING id`,
		kind, externalID, project, topic).Scan(&logID)
	if err == sql.ErrNoRows {
		return false, nil // already mirrored on an earlier run
	}
	if err != nil {
		return false, err
	}
	if topic == "" {
		topic = "(no subject)"
	}
	_, err = db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body)
		VALUES ('inkbox', $1, $2, $3)`, project, topic, body)
	return true, err
}

func inkboxPull(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS inkbox_sync_log (
		id serial PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now(),
		kind text NOT NULL, external_id text NOT NULL,
		to_project text NOT NULL, topic text,
		UNIQUE (kind, external_id))`); err != nil {
		return err
	}

	totalMail, totalTasks, skipped, unreadable := 0, 0, 0, 0

	for _, id := range ibIdentities {
		key := ibKey(id.handle)
		if key == "" {
			fmt.Printf("inkbox_pull: %s skipped, no INKBOX_KEY_%s and no INKBOX_API_KEY\n",
				id.handle, strings.ToUpper(id.handle))
			skipped++
			continue
		}

		// ---- mail ----
		raw, err := ibDo(key, "GET", "/mail/mailboxes/"+url.PathEscape(id.mailbox)+"/messages", nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "inkbox_pull mail list:", err)
		} else {
			for _, m := range ibItems(raw) {
				if b, _ := m["is_read"].(bool); b {
					continue
				}
				msgID := ibStr(m, "id", "message_id", "messageId", "uuid")
				if msgID == "" {
					if unreadable == 0 {
						fmt.Fprintf(os.Stderr, "inkbox_pull: mail item has no id field; keys were: %s\n", ibKeys(m))
					}
					unreadable++
					continue
				}
				subject := ibStr(m, "subject", "title")
				from := ibStr(m, "from", "from_address", "fromAddress", "from.address", "sender")
				text := ibStr(m, "text", "body_text", "bodyText", "body", "snippet", "preview")
				body := fmt.Sprintf("Inkbox mail to %s\nFrom: %s\n\n%s", id.mailbox, from, text)
				fresh, err := ibRecord(db, "mail", msgID, id.project, subject, body)
				if err != nil {
					fmt.Fprintln(os.Stderr, "inkbox_pull record mail:", err)
					continue
				}
				if !fresh {
					continue
				}
				totalMail++
				// Mark read only after the bridge row is committed. The wrong
				// order loses mail permanently if the insert fails.
				if _, err := ibDo(key, "PATCH",
					"/mail/mailboxes/"+url.PathEscape(id.mailbox)+"/messages/"+url.PathEscape(msgID),
					map[string]any{"is_read": true}); err != nil {
					fmt.Fprintln(os.Stderr, "inkbox_pull mark read:", err)
				}
			}
		}

		// ---- a2a tasks ----
		raw, err = ibDo(key, "GET", "/identities/"+url.PathEscape(id.handle)+"/a2a/tasks", nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "inkbox_pull a2a list:", err)
			continue
		}
		for _, t := range ibItems(raw) {
			taskID := ibStr(t, "id", "task_id", "taskId")
			if taskID == "" {
				if unreadable == 0 {
					fmt.Fprintf(os.Stderr, "inkbox_pull: a2a item has no id field; keys were: %s\n", ibKeys(t))
				}
				unreadable++
				continue
			}
			caller := ibStr(t, "caller.handle", "requester.handle", "caller", "requesterHandle")
			state := ibStr(t, "state", "status.state")
			ctx := ibStr(t, "contextId", "context_id")
			topic := fmt.Sprintf("A2A task from %s", caller)
			body := fmt.Sprintf("Inkbox A2A task for %s\nFrom: %s\nState: %s\nTask: %s\nContext: %s\n\n"+
				"Read it with: inkbox a2a sent-task %s -i %s",
				id.handle, caller, state, taskID, ctx, taskID, id.handle)
			fresh, err := ibRecord(db, "a2a", taskID, id.project, topic, body)
			if err != nil {
				fmt.Fprintln(os.Stderr, "inkbox_pull record a2a:", err)
				continue
			}
			if fresh {
				totalTasks++
			}
		}
	}

	fmt.Printf("inkbox_pull: mail=%d tasks=%d identities_skipped=%d unreadable=%d\n",
		totalMail, totalTasks, skipped, unreadable)
	return nil
}
