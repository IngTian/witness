package store

import "testing"

// The L1 dedup identity. Every writer (worker, MCP record_observation) mints ids through
// a change here silently duplicates every re-recorded observation.
// a change here silently duplicates every re-recorded observation.
// is sha1("sess|default|hello")[:6] in hex, computed independently.
func TestObservationIDKnownVector(t *testing.T) {
	if got, want := ObservationID("sess", "default", "hello"), "obs_9e8bb9da008f"; got != want {
		t.Fatalf("ObservationID = %q, want %q", got, want)
	}
}
