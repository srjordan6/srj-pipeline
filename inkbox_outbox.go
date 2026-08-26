package main

// Outbound Inkbox traffic, driven by a SQL queue.
//
// WHY A QUEUE AND NOT A DIRECT CALL. Each project is a separate Claude session,
// and a session holds exactly one Inkbox connector for the whole account, bound
// to one identity. In practice that binding is theworldofai, so an srj session
// physically cannot send as srj: Inkbox rejects the reply with
// a2a_not_task_worker, which is correct behaviour. Two A2A tasks sat unanswered
// for exactly that reason.
//
// This cron holds all three agent keys legitimately, so any project writes a row
// saying who it is sending as, and this stage sends it with the right key. The
// queue also buys retries, an audit trail of every agent message, and one place
// to look when something did not arrive.
//
// SCOPE, deliberately: email send and A2A task REPLY. Starting a NEW A2A task is
// not in the REST API at all (verified against inkbox.ai/api/openapi.json: the
// a2a paths expose list, get, and reply, nothing that creates one). A new task
// goes over the JSON-RPC binding advertised on each agent card, which is a
// different protocol and worth its own stage rather than a guess bolted onto
// this one. project_bridge remains the channel for opening a conversation.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/lib/pq"
)

// Two attempts, then the row is parked as failed. A transient network blip
// deserves a retry; a 400 from a malformed payload will fail identically
// forever, and retrying it every fifteen minutes only buries the real error.
const ibOutboxMaxAttempts = 2

type ibOutboxRow struct {
	id       int64
	from     string
	channel  string
	to       []string
	subject  string
	body     string
	taskID   string
	intent   string
	attempts int
}

func inkboxOutbox(db *sql.DB) error {
	rows, err := db.Query(`SELECT id, from_handle, channel,
			COALESCE(to_addrs, '{}'), COALESCE(subject,''), body,
			COALESCE(task_id,''), COALESCE(intent,''), attempts
		FROM inkbox_outbox
		WHERE status = 'pending' AND attempts < $1
		ORDER BY created_at
		LIMIT 50`, ibOutboxMaxAttempts)
	if err != nil {
		return err
	}
	var queue []ibOutboxRow
	for rows.Next() {
		var r ibOutboxRow
		if err := rows.Scan(&r.id, &r.from, &r.channel, pq.Array(&r.to),
			&r.subject, &r.body, &r.taskID, &r.intent, &r.attempts); err != nil {
			rows.Close()
			return err
		}
		queue = append(queue, r)
	}
	rows.Close()

	if len(queue) == 0 {
		fmt.Println("inkbox_outbox: nothing pending")
		return nil
	}

	var sent, failed int
	for _, r := range queue {
		key := ibKey(r.from)
		if key == "" {
			markOutboxFailed(db, r, fmt.Sprintf("no INKBOX_KEY_%s in the environment",
				strings.ToUpper(r.from)))
			failed++
			continue
		}

		var mailbox string
		for _, id := range ibIdentities {
			if id.handle == r.from {
				mailbox = id.mailbox
			}
		}
		if mailbox == "" {
			markOutboxFailed(db, r, "handle is not one of the known identities")
			failed++
			continue
		}

		var raw []byte
		var sendErr error
		switch r.channel {
		case "email":
			// Field names come from the OpenAPI MessageSend schema: recipients
			// is an object with to/cc/bcc, not a bare array.
			raw, sendErr = ibDo(key, "POST",
				"/mail/mailboxes/"+url.PathEscape(mailbox)+"/messages",
				map[string]any{
					"recipients": map[string]any{"to": r.to},
					"subject":    r.subject,
					"body_text":  r.body,
				})
		case "a2a_reply":
			raw, sendErr = ibDo(key, "POST",
				"/identities/"+url.PathEscape(r.from)+"/a2a/tasks/"+url.PathEscape(r.taskID)+"/reply",
				map[string]any{
					"intent": r.intent,
					"parts":  []any{map[string]any{"text": r.body}},
				})
		default:
			markOutboxFailed(db, r, "unknown channel "+r.channel)
			failed++
			continue
		}

		if sendErr != nil {
			// attempts is incremented whether or not this was the last try, so
			// the row retires itself rather than needing a sweeper.
			if _, err := db.Exec(`UPDATE inkbox_outbox
				SET attempts = attempts + 1, error = $2,
					status = CASE WHEN attempts + 1 >= $3 THEN 'failed' ELSE 'pending' END
				WHERE id = $1`, r.id, sendErr.Error(), ibOutboxMaxAttempts); err != nil {
				return err
			}
			fmt.Printf("inkbox_outbox: %s %s id=%d attempt %d failed: %v\n",
				r.from, r.channel, r.id, r.attempts+1, sendErr)
			failed++
			continue
		}

		external := ibExternalID(raw)
		if _, err := db.Exec(`UPDATE inkbox_outbox
			SET status = 'sent', sent_at = now(), attempts = attempts + 1,
				external_id = NULLIF($2,''), error = NULL
			WHERE id = $1`, r.id, external); err != nil {
			return err
		}
		fmt.Printf("inkbox_outbox: sent %s as %s id=%d external=%s\n",
			r.channel, r.from, r.id, external)
		sent++
	}

	fmt.Printf("inkbox_outbox: sent=%d failed=%d of %d queued\n", sent, failed, len(queue))
	return nil
}

func markOutboxFailed(db *sql.DB, r ibOutboxRow, msg string) {
	if _, err := db.Exec(`UPDATE inkbox_outbox
		SET status = 'failed', attempts = attempts + 1, error = $2
		WHERE id = $1`, r.id, msg); err != nil {
		fmt.Println("inkbox_outbox: could not mark row failed:", err)
		return
	}
	fmt.Printf("inkbox_outbox: id=%d not sendable: %s\n", r.id, msg)
}

// ibExternalID pulls whatever id the API returned so a sent row can be traced
// back to the real message or task. The two endpoints name it differently, and
// neither name is guaranteed, so a miss is recorded as empty rather than fatal.
func ibExternalID(raw []byte) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"message_id", "id", "task_id"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
