package claude

import (
	"time"

	"github.com/IngTian/witness/internal/store"
)

// HookEvent is the part of a Claude Code hook payload capture reads; other fields
// (hook_event_name, cwd) are ignored — lenses are global, so nothing is keyed by repo.
type HookEvent struct {
	SessionID            string         `json:"session_id"`
	Prompt               string         `json:"prompt"`
	LastAssistantMessage string         `json:"last_assistant_message"`
	Effort               map[string]any `json:"effort"`
}

// Capture writes one L0 record for a hook event. It needs only the raw-append
// surface (issue #73-C1): NextSeq to number the turn, AppendRaw to store it.
func Capture(st store.RawWriter, e HookEvent, now time.Time) error {
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
