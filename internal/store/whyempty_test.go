package store

import "testing"

// WhyEmpty is what lets an empty answer name the layer that is actually missing (#166).
// The MCP tests pin the WORDING against a fake; this pins the CLASSIFICATION against a
// real database, walking an archive through all three states in order.
func TestWhyEmptyWalksTheLayers(t *testing.T) {
	s := tempStore(t)

	// Nothing wired: no raw turns at all. This is the state that used to be reported as
	// "the reviewer may not have run yet", which is advice that never comes true.
	if got := s.WhyEmpty(LensDefault); got != NoCapture {
		t.Fatalf("fresh archive: got %v, want NoCapture", got)
	}

	// Capture working, nothing mined into this lens yet.
	if err := s.AppendRaw(RawRecord{
		TS: "2026-01-01T00:00:00Z", Session: "s1", Seq: 1, Role: "user", Text: "hello",
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.WhyEmpty(LensDefault); got != NoObservations {
		t.Fatalf("raw present, no observations: got %v, want NoObservations", got)
	}

	// Observations exist, so the reviewer genuinely is the missing step — the ONLY state
	// where the old message was the truth.
	if err := s.AppendObservations([]Observation{{
		ID: "obs_test0000001", TS: "2026-01-01T00:00:01Z", Session: "s1",
		Lens: LensDefault, Dimension: "habits", Observation: "did a thing",
	}}); err != nil {
		t.Fatal(err)
	}
	if got := s.WhyEmpty(LensDefault); got != NoFacets {
		t.Fatalf("observations present, no facets: got %v, want NoFacets", got)
	}

	// Per-lens, not global: a different lens is still unmined even though capture works
	// and another lens has observations.
	if got := s.WhyEmpty("other"); got != NoObservations {
		t.Fatalf("other lens: got %v, want NoObservations", got)
	}
}
