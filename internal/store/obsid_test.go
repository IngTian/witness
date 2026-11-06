package store

import "testing"

// The L1 dedup identity. Every writer (worker, MCP record_observation, the CLI) mints ids through
// ObservationID, and existing archives already hold ids in this exact form — a change here
// silently duplicates every re-recorded observation and orphans emerge_seen marks. The vector
// is sha1("sess|default|hello")[:6] in hex, computed independently.
func TestObservationIDKnownVector(t *testing.T) {
	if got, want := ObservationID("sess", "default", "hello"), "obs_9e8bb9da008f"; got != want {
		t.Fatalf("ObservationID = %q, want %q", got, want)
	}
}
