package claude

import (
	"time"

	"github.com/IngTian/claude-witness/internal/store"
)

type HookEvent struct {
	HookEventName        string         `json:"hook_event_name"`
	SessionID            string         `json:"session_id"`
	Prompt               string         `json:"prompt"`
	LastAssistantMessage string         `json:"last_assistant_message"`
	Effort               map[string]any `json:"effort"`
	Cwd                  string         `json:"cwd"`
}

func Capture(st *store.Store, e HookEvent, now time.Time) error {
	if e.SessionID == "" {
		return nil
	}
	var rec store.RawRecord
	switch {
	case e.Prompt != "":
		rec = store.RawRecord{Role: "user", Text: e.Prompt}
	case e.LastAssistantMessage != "":
		rec = store.RawRecord{Role: "assistant", Text: e.LastAssistantMessage}
		if e.Effort != nil {
			if lvl, ok := e.Effort["level"].(string); ok {
				rec.Effort = lvl
			}
		}
	default:
		return nil
	}
	rec.TS = now.UTC().Format(time.RFC3339)
	rec.Session = e.SessionID
	rec.Seq = st.NextSeq(e.SessionID)
	return st.AppendRaw(rec)
}
