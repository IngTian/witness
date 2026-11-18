package store

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// recsOf builds session "s" records from "role:text" specs, all stamped ts.
func recsOf(ts string, specs ...string) []RawRecord {
	out := make([]RawRecord, len(specs))
	for i, spec := range specs {
		role, text, _ := strings.Cut(spec, ":")
		out[i] = RawRecord{Session: "s", Seq: i, TS: ts, Role: role, Text: text}
	}
	return out
}

func rawIDs(t *testing.T, s *Store, session string) (ids []int64) {
	t.Helper()
	rows, err := s.db.Query(`SELECT id FROM raw WHERE session = ? ORDER BY id`, session)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// A replace-import (an edited ingest record, an OpenCode/Codex rewind, a re-key) hands the store the
// session's whole current transcript. It keeps the leading rows whose (role, text) are unchanged,
// under their old ids, and rewrites only from the first changed turn. Each lens's watermark clamps
// to the kept count, so the unchanged turns are not mined again beside their old observations, and
// a lens's backoff clears only when rows were dropped: a re-key that changes no turn leaves it.
func TestReplaceImportKeepsUnchangedPrefix(t *testing.T) {
	base := []string{"user:a", "assistant:b", "user:c", "assistant:d", "user:e"}
	all := strings.Join(base, "|")
	for _, tc := range []struct {
		name     string
		in       []RawRecord
		keep     int
		texts    string
		def, oth int
		cleared  bool
	}{
		{"edit in the middle", recsOf("", "user:a", "assistant:b", "user:X", "assistant:d", "user:e"), 2,
			"user:a|assistant:b|user:X|assistant:d|user:e", 2, 1, true},
		{"edit of the last turn", recsOf("", "user:a", "assistant:b", "user:c", "assistant:d", "user:E"), 4,
			"user:a|assistant:b|user:c|assistant:d|user:E", 4, 1, true},
		{"rewind", recsOf("", "user:a", "assistant:b"), 2, "user:a|assistant:b", 2, 1, true},
		{"rewind plus new branch", recsOf("", "user:a", "assistant:b", "user:Y", "assistant:Z"), 2,
			"user:a|assistant:b|user:Y|assistant:Z", 2, 1, true},
		{"re-key with the same text", recsOf("2030-01-01T00:00:00Z", base...), 5, all, 5, 1, false},
		{"same text, different role", recsOf("", "user:a", "user:b", "user:c", "assistant:d", "user:e"), 1,
			"user:a|user:b|user:c|assistant:d|user:e", 1, 1, true},
		{"full change", recsOf("", "user:V", "assistant:W"), 0, "user:V|assistant:W", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tempStore(t)
			if err := s.ApplyRawImport(SessionMeta{Session: "s"}, recsOf("2026-01-01T00:00:00Z", base...), "", "", false); err != nil {
				t.Fatal(err)
			}
			if err := s.MarkDistilled("s", LensDefault, 5); err != nil {
				t.Fatal(err)
			}
			if err := s.MarkDistilled("s", "other", 1); err != nil {
				t.Fatal(err)
			}
			s.IncRetry("s", "other")
			if err := s.SetNextAttempt("s", "other", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := s.SetDrift("s", "other"); err != nil {
				t.Fatal(err)
			}
			before := rawIDs(t, s, "s")

			if err := s.ApplyRawImport(SessionMeta{Session: "s"}, tc.in, "", "", true); err != nil {
				t.Fatal(err)
			}

			after := rawIDs(t, s, "s")
			if len(after) < tc.keep || !slices.Equal(after[:tc.keep], before[:tc.keep]) {
				t.Fatalf("kept rows were re-inserted: ids %v, before %v", after, before)
			}
			if len(after) > tc.keep && after[tc.keep] <= before[len(before)-1] {
				t.Fatalf("rows past keep must be new ids: ids %v, before %v", after, before)
			}
			recs, err := s.ReadRaw("s")
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range recs {
				got = append(got, r.Role+":"+r.Text)
			}
			if strings.Join(got, "|") != tc.texts {
				t.Fatalf("raw = %v, want %s", got, tc.texts)
			}
			if d, o := s.DistilledCount("s", LensDefault), s.DistilledCount("s", "other"); d != tc.def || o != tc.oth {
				t.Fatalf("progress default %d other %d, want %d and %d", d, o, tc.def, tc.oth)
			}
			cleared := s.RetryCount("s", "other") == 0 && !s.LensBackedOff("s", "other", time.Now()) && s.DriftAt("s", "other") == ""
			if cleared != tc.cleared {
				t.Fatalf("backoff cleared = %v, want %v", cleared, tc.cleared)
			}
		})
	}
}

// The suffix delete keeps the generation CAS sound: a mine whose snapshot reached into the dropped
// tail is refused, and a mine of only the kept prefix still lands.
func TestReplaceImportKeepsUnchangedPrefixCAS(t *testing.T) {
	s := tempStore(t)
	if err := s.ApplyRawImport(SessionMeta{Session: "s"}, recsOf("", "user:a", "assistant:b"), "", "", false); err != nil {
		t.Fatal(err)
	}
	_, highB, _ := s.ReadRawSnapshot("s")
	if err := s.ApplyRawImport(SessionMeta{Session: "s"}, recsOf("", "user:c", "assistant:d", "user:e"), "", "", false); err != nil {
		t.Fatal(err)
	}
	_, highE, _ := s.ReadRawSnapshot("s")
	if err := s.ApplyRawImport(SessionMeta{Session: "s"}, recsOf("", "user:a", "assistant:b", "user:X"), "", "", true); err != nil {
		t.Fatal(err)
	}

	if ok, _ := s.MarkDistilledIfCurrent("s", LensDefault, 5, highE); ok {
		t.Fatal("high id in the dropped tail must be refused")
	}
	if ok, _ := s.MarkDistilledIfCurrent("s", LensDefault, 2, highB); !ok || s.DistilledCount("s", LensDefault) != 2 {
		t.Fatalf("a mine of the kept prefix must land: ok=%v count=%d", ok, s.DistilledCount("s", LensDefault))
	}
}
