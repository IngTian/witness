package commands

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// raw.ts must always be canonical RFC3339, because the store compares it as a STRING
// (SampleRecentSessions orders by MAX(ts), LastRawTS takes MAX(ts)). An unvalidated caller ts
// in any other shape sorts wrong: "07/26/2026" and "1754300000" both compare BELOW "2026-…".
func TestIngestNormalizesTimestamps(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))

	today := time.Now().UTC().Format("2006-01-02")
	batch := fmt.Sprintf(`{"text":"us style","id":"d1","session":"US","ts":"07/26/2026"}
{"text":"epoch","id":"d2","session":"EPOCH","ts":"1754300000"}
{"text":"date only","id":"d3","session":"DATEONLY","ts":"%[1]s"}
{"text":"rfc3339","id":"d4","session":"GOOD","ts":"%[1]sT00:00:00Z"}
{"text":"no ts at all","id":"d5","session":"NOTS"}
`, today)
	if _, _, err := cmdIngest(strings.NewReader(batch), true); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Every stored ts parses as RFC3339 — the invariant the rest of the engine assumes.
	for _, s := range []string{"file:US", "file:EPOCH", "file:DATEONLY", "file:GOOD", "file:NOTS"} {
		got, err := st.ReadRaw(s)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: want 1 record, got %d", s, len(got))
		}
		if _, err := time.Parse(time.RFC3339, got[0].TS); err != nil {
			t.Errorf("%s: stored ts %q is not RFC3339: %v", s, got[0].TS, err)
		}
	}

	// A date-only ts is HONORED (the schema documents that shape), not replaced by now.
	dateOnly, _ := st.ReadRaw("file:DATEONLY")
	if got := dateOnly[0].TS; !strings.HasPrefix(got, today) {
		t.Errorf("a date-only ts must be preserved, got %q", got)
	}
}

// normalizeIngestTS directly: accepted shapes are canonicalized, junk falls back to now.
func TestNormalizeIngestTS(t *testing.T) {
	const fallback = "2030-01-01T00:00:00Z"
	for in, want := range map[string]string{
		"2026-08-04T12:30:00Z": "2026-08-04T12:30:00Z",
		"2026-08-04":           "2026-08-04T00:00:00Z",
		"2026-08-04 12:30:00":  "2026-08-04T12:30:00Z",
		"":                     fallback,
		"   ":                  fallback,
		"07/26/2026":           fallback, // US style: would sort below an RFC3339 ts
		"1754300000":           fallback, // epoch seconds: same hazard
		"yesterday":            fallback,
	} {
		if got := normalizeIngestTS(in, fallback); got != want {
			t.Errorf("normalizeIngestTS(%q) = %q, want %q", in, got, want)
		}
	}
}
