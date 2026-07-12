package opencode

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/store"
)

const captureKeysMetaPrefix = "opencode_capture_keys:"

// Capture mirrors a single OpenCode plugin event into witness L0 when the event
// carries a complete text message. It is intentionally best-effort: SQLite import
// remains the source-of-truth reconcile path for missed or mutable events.
func Capture(st *store.Store, data []byte, now time.Time) (bool, error) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return false, err
	}
	event := unwrapEvent(root)
	sessionID := strings.TrimSpace(findString(event, "sessionID", "sessionId", "session_id"))
	if sessionID == "" {
		return false, nil
	}
	role := strings.TrimSpace(findString(event, "role"))
	if role != "user" && role != "assistant" {
		return false, nil
	}
	if role == "assistant" && !findCompleted(event) {
		return false, nil
	}
	text := strings.TrimSpace(strings.Join(findTextParts(event), "\n\n"))
	if text == "" {
		return false, nil
	}

	messageID := strings.TrimSpace(findMessageID(event))
	if messageID == "" {
		messageID = fallbackMessageID(role, text)
	}
	session := SessionPrefix + sessionID
	key := messageKey(messageID, role, text)
	stateKey := captureKeysMetaPrefix + session
	keys := parseImportKeys(st.MetaString(stateKey))
	if containsString(keys, key) || containsString(parseImportKeys(st.MetaString(importKeysMetaPrefix+session)), key) {
		return false, nil
	}
	keys = append(keys, key)
	stateValue, err := json.Marshal(keys)
	if err != nil {
		return false, err
	}
	rec := store.RawRecord{
		TS:      now.UTC().Format(time.RFC3339),
		Session: session,
		Seq:     st.RawCount(session),
		Role:    role,
		Text:    text,
	}
	meta := store.SessionMeta{Session: session, Cwd: strings.TrimSpace(findString(event, "cwd", "directory")), Started: rec.TS}
	if err := st.ApplyRawImport(meta, []store.RawRecord{rec}, stateKey, string(stateValue), false); err != nil {
		return false, err
	}
	// Record the owning platform so platform.ForSession is column-authoritative for
	// this session (the id prefix remains the fallback for un-stamped rows).
	st.SetSessionPlatform(session, "opencode")
	return true, nil
}

func unwrapEvent(v any) any {
	if m, ok := v.(map[string]any); ok {
		if e, ok := m["event"]; ok {
			return e
		}
	}
	return v
}

func findString(v any, keys ...string) string {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			if s := findString(item, keys...); s != "" {
				return s
			}
		}
	case map[string]any:
		for _, key := range keys {
			if s, ok := x[key].(string); ok && s != "" {
				return s
			}
		}
		for _, key := range []string{"info", "message", "properties", "data", "part"} {
			if child, ok := x[key]; ok {
				if s := findString(child, keys...); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func findTextParts(v any) []string {
	switch x := v.(type) {
	case []any:
		var out []string
		for _, item := range x {
			out = append(out, findTextParts(item)...)
		}
		return out
	case map[string]any:
		if typ, hasType := x["type"].(string); hasType {
			// Only OpenCode text parts are natural-language dialogue. Tool results,
			// patches, files, reasoning, and step metadata are structured execution
			// artifacts; do not recurse into their payloads and accidentally import
			// command output or file diffs as user/assistant dialogue.
			if typ == "text" {
				if text, ok := x["text"].(string); ok && strings.TrimSpace(text) != "" {
					return []string{text}
				}
				return nil
			}
			if isNonDialoguePartType(typ) {
				return nil
			}
		}
		var out []string
		for _, key := range []string{"part", "parts", "message", "properties", "data", "event"} {
			if child, ok := x[key]; ok {
				out = append(out, findTextParts(child)...)
			}
		}
		return out
	default:
		return nil
	}
}

func isNonDialoguePartType(typ string) bool {
	switch typ {
	case "tool", "patch", "file", "reasoning", "step-start", "step-finish", "compaction", "subtask", "agent":
		return true
	default:
		return false
	}
}

func findCompleted(v any) bool {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			if findCompleted(item) {
				return true
			}
		}
	case map[string]any:
		for _, key := range []string{"completed", "finish"} {
			if b, ok := x[key].(bool); ok && b {
				return true
			}
			if f, ok := x[key].(float64); ok && f > 0 {
				return true
			}
		}
		if timeMap, ok := x["time"].(map[string]any); ok {
			if findCompleted(timeMap) {
				return true
			}
		}
		for _, key := range []string{"info", "message", "properties", "data"} {
			if child, ok := x[key]; ok && findCompleted(child) {
				return true
			}
		}
	}
	return false
}

func findMessageID(v any) string {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			if id := findMessageID(item); id != "" {
				return id
			}
		}
	case map[string]any:
		if _, hasRole := x["role"]; hasRole {
			if id, ok := x["id"].(string); ok && id != "" {
				return id
			}
		}
		for _, key := range []string{"info", "message", "properties", "data"} {
			if child, ok := x[key]; ok {
				if id := findMessageID(child); id != "" {
					return id
				}
			}
		}
	}
	return ""
}

func fallbackMessageID(role, text string) string {
	h := sha256.Sum256([]byte(role + "\x00" + text))
	return "event_" + fmt.Sprintf("%x", h[:8])
}

func containsString(list []string, needle string) bool {
	for _, item := range list {
		if item == needle {
			return true
		}
	}
	return false
}
