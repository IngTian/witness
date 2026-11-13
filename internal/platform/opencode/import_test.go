package opencode

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// seedV2DB creates a temporary v2 DB with the given inserts and returns its path.
func seedV2DB(t *testing.T, inserts string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schemaBytes, err := os.ReadFile("testdata/opencode-2.2.7-schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schemaBytes)); err != nil {
		t.Fatal(err)
	}
	if inserts != "" {
		if _, err := db.Exec(inserts); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// seedOpenCodeDB is the standard test fixture: session ses_test with a user turn,
// a completed assistant with text parts, and a partial streaming assistant.
func seedOpenCodeDB(t *testing.T) string {
	t.Helper()
	return seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, 'Test Project', NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_test', 'proj1', NULL, NULL, NULL, NULL, 'test-slug', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 5000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg_user1234567890abc', 'ses_test', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"help me debug this"}');
		INSERT INTO session_message VALUES ('msg_asst1234567890abc', 'ses_test', 'assistant', 2, 2000, 4000, '{"time":{"created":2000,"completed":4000},"agent":"build","model":{"id":"gpt-4"},"content":[]}');
		INSERT INTO session_part VALUES ('msg_asst1234567890abc', 'ses_test', 0, 'text', NULL, 2100, 2100, '{"text":"first note"}');
		INSERT INTO session_part VALUES ('msg_asst1234567890abc', 'ses_test', 1, 'tool', 'call_abc', 2200, 2200, '{"type":"tool","id":"call_abc","name":"bash"}');
		INSERT INTO session_part VALUES ('msg_asst1234567890abc', 'ses_test', 2, 'text', NULL, 3000, 3000, '{"text":"final answer"}');
		INSERT INTO session_message VALUES ('msg_partial1234567abc', 'ses_test', 'assistant', 3, 6000, 6000, '{"time":{"created":6000},"agent":"build","model":{"id":"gpt-4"},"content":[{"type":"text","text":"partial stream"}]}');
	`)
}

func TestDefaultDBPathOrder(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "WITNESS_OPENCODE_DB wins",
			env:  map[string]string{"WITNESS_OPENCODE_DB": "/w.db", "OPENCODE_DB": "/x.db", "XDG_DATA_HOME": "/d"},
			want: "/w.db",
		},
		{
			name: "OPENCODE_DB absolute as is",
			env:  map[string]string{"OPENCODE_DB": "/abs/x.db", "XDG_DATA_HOME": "/d"},
			want: "/abs/x.db",
		},
		{
			name: "OPENCODE_DB relative resolved",
			env:  map[string]string{"OPENCODE_DB": "rel.db", "XDG_DATA_HOME": "/d"},
			want: "/d/opencode/rel.db",
		},
		{
			name: "OPENCODE_DB :memory: ignored",
			env:  map[string]string{"OPENCODE_DB": ":memory:", "XDG_DATA_HOME": "/d"},
			want: "/d/opencode/opencode.db",
		},
		{
			name: "XDG_DATA_HOME default",
			env:  map[string]string{"XDG_DATA_HOME": "/d"},
			want: "/d/opencode/opencode.db",
		},
		{
			name: "HOME fallback",
			env:  map[string]string{"HOME": "/home/user"},
			want: "/home/user/.local/share/opencode/opencode.db",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := DefaultDBPath()
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("DefaultDBPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProbeSchemaGuards(t *testing.T) {
	cases := []struct {
		name    string
		stmt    string
		wantErr error
		wantMsg string
	}{
		{
			name:    "fresh v2 DB",
			stmt:    "", // schema is already v2
			wantErr: nil,
		},
		{
			name: "v1 only",
			stmt: `DROP TABLE session_v2; DROP TABLE session_message; DROP TABLE session_part;
			       CREATE TABLE session (id text PRIMARY KEY, time_updated integer);`,
			wantErr: ErrOpenCodeV1,
		},
		{
			name:    "v2 missing session_part",
			stmt:    `DROP TABLE session_part;`,
			wantErr: ErrOpenCodeSchema,
			wantMsg: "session_part",
		},
		{
			name: "v2 with v1 + migration running",
			stmt: `CREATE TABLE session (id text PRIMARY KEY, time_updated integer);
			       INSERT INTO kv VALUES ('migration.v1-v2', '{"phase":"running"}', 1000, 1000);`,
			wantErr: ErrOpenCodeSchema,
			wantMsg: "migrating",
		},
		{
			name: "v2 with v1 + migration completed",
			stmt: `CREATE TABLE session (id text PRIMARY KEY, time_updated integer);
			       INSERT INTO kv VALUES ('migration.v1-v2', '{"phase":"completed"}', 1000, 1000);`,
			wantErr: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := seedV2DB(t, tc.stmt)
			db, err := sql.Open("sqlite", sqliteURI(dbPath))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			err = ProbeSchema(context.Background(), db)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ProbeSchema() err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ProbeSchema() err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantMsg != "" && !contains(err.Error(), tc.wantMsg) {
				t.Fatalf("ProbeSchema() err = %v, want message containing %q", err, tc.wantMsg)
			}
		})
	}
}

func TestReadTurnsAppliesEveryV2SkipRule(t *testing.T) {
	dbPath := seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, NULL, NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_all', 'proj1', NULL, NULL, NULL, NULL, 'test', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 12000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg_u1_1234567890abc', 'ses_all', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"first question"}');
		INSERT INTO session_message VALUES ('msg_a2_1234567890abc', 'ses_all', 'assistant', 2, 2000, 2500, '{"time":{"created":2000,"completed":2500},"agent":"build","model":{},"content":[{"type":"reasoning","text":"think"},{"type":"text","text":"Let me check."},{"type":"tool","id":"call1"}]}');
		INSERT INTO session_message VALUES ('msg_a3_1234567890abc', 'ses_all', 'assistant', 3, 3000, 3500, '{"time":{"created":3000,"completed":3500},"content":[]}');
		INSERT INTO session_part VALUES ('msg_a3_1234567890abc', 'ses_all', 0, 'text', NULL, 3100, 3100, '{"text":"part one"}');
		INSERT INTO session_part VALUES ('msg_a3_1234567890abc', 'ses_all', 1, 'reasoning', NULL, 3200, 3200, '{"text":"hidden"}');
		INSERT INTO session_part VALUES ('msg_a3_1234567890abc', 'ses_all', 2, 'text', NULL, 3300, 3300, '{"text":"part two"}');
		INSERT INTO session_message VALUES ('msg_idle_1234567abc', 'ses_all', 'idle', 4, 3600, 3600, '{"outcome":"completed"}');
		INSERT INTO session_message VALUES ('msg_synth123456789abc', 'ses_all', 'synthetic', 5, 5000, 5000, '{"text":"<subagent sessionID=child>child says</subagent>","metadata":{"source":"subagent"}}');
		INSERT INTO session_message VALUES ('msg_a6_1234567890abc', 'ses_all', 'assistant', 6, 6000, 6500, '{"time":{"created":6000,"completed":6500},"content":[{"type":"text","text":"after the report"}]}');
		INSERT INTO session_message VALUES ('msg_shell12345678abc', 'ses_all', 'shell', 7, 7000, 7000, '{"command":"ls","status":"completed"}');
		INSERT INTO session_message VALUES ('msg_compact1234567abc', 'ses_all', 'compaction', 8, 8000, 8000, '{"status":"completed"}');
		INSERT INTO session_message VALUES ('msg_u9_1234567890abc', 'ses_all', 'user', 9, 9000, 9000, '{"time":{"created":9000},"files":[{"name":"f.txt"}]}');
		INSERT INTO session_message VALUES ('msg_a10_123456789abc', 'ses_all', 'assistant', 10, 10000, 10000, '{"time":{"created":10000},"content":[{"type":"text","text":"streaming"}]}');
		INSERT INTO session_message VALUES ('msg_a11_123456789abc', 'ses_all', 'assistant', 11, 11000, 11500, '{"time":{"created":11000,"completed":11500},"error":{"message":"failed"},"content":[]}');
		INSERT INTO session_message VALUES ('msg_a12_123456789abc', 'ses_all', 'assistant', 12, 12000, 12500, '{"time":{"created":12000,"completed":12500},"error":{"message":"partial"},"content":[{"type":"text","text":"partial before error"}]}');
	`)
	db, err := sql.Open("sqlite", sqliteURI(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	turns, err := ReadTurns(context.Background(), db, "ses_all")
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	var gotSeq []int64
	var gotTS []int64
	for _, turn := range turns {
		got = append(got, turn.Role+":"+turn.Text)
		gotSeq = append(gotSeq, turn.Seq)
		gotTS = append(gotTS, turn.TS)
	}

	want := []string{"user:first question", "assistant:Let me check.", "assistant:part one\n\npart two",
		"assistant:after the report", "assistant:partial before error"}
	wantSeq := []int64{1, 2, 3, 6, 12}
	wantTS := []int64{1100, 2000, 3000, 6000, 12000}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("turns = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(gotSeq, wantSeq) {
		t.Fatalf("seq = %v, want %v", gotSeq, wantSeq)
	}
	if !reflect.DeepEqual(gotTS, wantTS) {
		t.Fatalf("TS = %v, want %v", gotTS, wantTS)
	}
}

func TestReadTurnsSkipsForkCopies(t *testing.T) {
	dbPath := seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, NULL, NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_fork', 'proj1', NULL, NULL, 'ses_src', '{"type":"through","messageID":"msg_x"}', 'fork', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 3000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg1234567890abcdefg_1', 'ses_fork', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"copied user"}');
		INSERT INTO session_message VALUES ('msg1234567890abcdefg_2', 'ses_fork', 'assistant', 2, 2000, 2000, '{"time":{"created":2000,"completed":2000},"content":[{"type":"text","text":"copied assistant"}]}');
		INSERT INTO session_message VALUES ('msgforknoticesesfork', 'ses_fork', 'synthetic', 20, 2500, 2500, '{"text":"<fork-notice>"}');
		INSERT INTO session_message VALUES ('msgown1234567890abcd', 'ses_fork', 'user', 3, 3000, 3000, '{"time":{"created":3000},"text":"my own turn"}');
	`)
	db, err := sql.Open("sqlite", sqliteURI(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	turns, err := ReadTurns(context.Background(), db, "ses_fork")
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, turn := range turns {
		got = append(got, turn.Role+":"+turn.Text)
	}

	want := []string{"user:my own turn"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("turns = %#v, want %#v (fork copies must be skipped)", got, want)
	}

	// The same _<seq> pattern in a NON-fork session is kept
	dbPath2 := seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, NULL, NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_normal', 'proj1', NULL, NULL, NULL, NULL, 'normal', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 1000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg1234567890abcdefg_1', 'ses_normal', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"not a fork copy"}');
	`)
	db2, err := sql.Open("sqlite", sqliteURI(dbPath2))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	turns2, err := ReadTurns(context.Background(), db2, "ses_normal")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns2) != 1 || turns2[0].Text != "not a fork copy" {
		t.Fatalf("non-fork session must keep _N ids, got %+v", turns2)
	}
}

func TestImporterMirrorsOpenCodeTextParts(t *testing.T) {
	dbPath := seedOpenCodeDB(t)
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	stats, err := (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 1 || stats.Records != 2 {
		t.Fatalf("stats = %+v, want 1 session / 2 records", stats)
	}

	raw, err := st.ReadRaw(SessionPrefix + "ses_test")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 {
		t.Fatalf("got %d raw records, want 2", len(raw))
	}
	if raw[0].Role != "user" || raw[0].Text != "help me debug this" {
		t.Fatalf("bad user record: %+v", raw[0])
	}
	if raw[1].Role != "assistant" || raw[1].Text != "first note\n\nfinal answer" {
		t.Fatalf("assistant text parts not merged: %+v", raw[1])
	}
	// The dedup state must land in the namespace the store reaps and merges
	if st.MetaString(store.OpenCodeImportKeysNS+":"+SessionPrefix+"ses_test") == "" {
		t.Fatalf("import state is not under the store's %q namespace", store.OpenCodeImportKeysNS)
	}

	stats, err = (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 0 {
		t.Fatalf("second import should be idempotent, wrote %d records", stats.Records)
	}
}

func TestImporterSkipsChildSessions(t *testing.T) {
	dbPath := seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, NULL, NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_parent', 'proj1', NULL, NULL, NULL, NULL, 'parent', '/repo', NULL, 'Parent Session', '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 2000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_v2 VALUES ('ses_child', 'proj1', NULL, 'ses_parent', NULL, NULL, 'child', '/repo', NULL, 'Child Session', '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, 'agent_id', NULL, 1000, 3000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg_p1_1234567890abc', 'ses_parent', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"parent user"}');
		INSERT INTO session_message VALUES ('msg_c1_1234567890abc', 'ses_child', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"child user"}');
	`)
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	stats, err := (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 1 {
		t.Fatalf("stats.Sessions = %d, want 1 (child skipped)", stats.Sessions)
	}
	if st.RawCount(SessionPrefix+"ses_child") != 0 {
		t.Fatalf("child session was imported when it should be skipped")
	}
	if st.RawCount(SessionPrefix+"ses_parent") == 0 {
		t.Fatalf("parent session was not imported")
	}

	// Targeted import of a child returns 0 records
	stats, err = (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), []string{"ses_child"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 0 {
		t.Fatalf("targeted import of child returned %d records, want 0", stats.Records)
	}
}

func TestImporterTargetsSessionsWithoutAdvancingFullSyncWatermark(t *testing.T) {
	dbPath := seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, NULL, NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_test', 'proj1', NULL, NULL, NULL, NULL, 'test', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 5000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_v2 VALUES ('ses_other', 'proj1', NULL, NULL, NULL, NULL, 'other', '/other', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 6000, 9000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg_t1_1234567890abc', 'ses_test', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"test user"}');
		INSERT INTO session_message VALUES ('msg_o1_1234567890abc', 'ses_other', 'user', 1, 6100, 6100, '{"time":{"created":6100},"text":"do not import me"}');
	`)
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetMetaString(syncMetaKey, "1234"); err != nil {
		t.Fatal(err)
	}

	stats, err := (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), []string{"ses_test"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 1 || stats.Records != 1 {
		t.Fatalf("stats = %+v, want one targeted session", stats)
	}
	if got := st.RawCount(SessionPrefix + "ses_other"); got != 0 {
		t.Fatalf("non-target session imported %d records", got)
	}
	if got := st.MetaString(syncMetaKey); got != "1234" {
		t.Fatalf("targeted import advanced full sync watermark to %q", got)
	}
}

func TestImporterRebuildsWhenCompletedAssistantAppearsMidSession(t *testing.T) {
	dbPath := seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, NULL, NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_mutable', 'proj1', NULL, NULL, NULL, NULL, 'mutable', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 6000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg_u1_1234567890abc', 'ses_mutable', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"USER-ONE"}');
		INSERT INTO session_message VALUES ('msg_a1_1234567890abc', 'ses_mutable', 'assistant', 2, 2000, 2000, '{"time":{"created":2000},"content":[{"type":"text","text":"ASSISTANT-ONE"}]}');
		INSERT INTO session_message VALUES ('msg_u2_1234567890abc', 'ses_mutable', 'user', 3, 3000, 3000, '{"time":{"created":3000},"text":"USER-TWO"}');
	`)
	mutateV2DB := func(path, stmt string) {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	stats, err := (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 2 {
		t.Fatalf("initial import wrote %d records, want 2", stats.Records)
	}
	if err := st.MarkDistilled(SessionPrefix+"ses_mutable", store.LensDefault, 2); err != nil {
		t.Fatal(err)
	}

	mutateV2DB(dbPath, `
		UPDATE session_message SET data = '{"time":{"created":2000,"completed":2500},"content":[{"type":"text","text":"ASSISTANT-ONE"}]}' WHERE id = 'msg_a1_1234567890abc';
		UPDATE session_v2 SET time_updated = 7000 WHERE id = 'ses_mutable';
	`)
	stats, err = (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 3 {
		t.Fatalf("rebuild wrote %d records, want 3", stats.Records)
	}
	raw, err := st.ReadRaw(SessionPrefix + "ses_mutable")
	if err != nil {
		t.Fatal(err)
	}
	var rolesAndText []string
	for _, r := range raw {
		rolesAndText = append(rolesAndText, r.Role+":"+r.Text)
	}
	want := []string{"user:USER-ONE", "assistant:ASSISTANT-ONE", "user:USER-TWO"}
	if !reflect.DeepEqual(rolesAndText, want) {
		t.Fatalf("raw = %#v, want %#v", rolesAndText, want)
	}
	if got := st.DistilledCount(SessionPrefix+"ses_mutable", store.LensDefault); got != 0 {
		t.Fatalf("rebuild should reset distill progress, got %d", got)
	}
}

func TestImporterRebuildsWhenImportedTextChanges(t *testing.T) {
	dbPath := seedOpenCodeDB(t)
	mutateV2DB := func(path, stmt string) {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDistilled(SessionPrefix+"ses_test", store.LensDefault, 2); err != nil {
		t.Fatal(err)
	}

	mutateV2DB(dbPath, `
		UPDATE session_part SET data = '{"text":"updated answer"}' WHERE message_id = 'msg_asst1234567890abc' AND ordinal = 2;
		UPDATE session_v2 SET time_updated = 7000 WHERE id = 'ses_test';
	`)
	if _, err := (&Importer{Store: st, DBPath: dbPath}).Import(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	raw, err := st.ReadRaw(SessionPrefix + "ses_test")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 {
		t.Fatalf("got %d raw records, want rebuilt 2 records", len(raw))
	}
	if raw[1].Text != "first note\n\nupdated answer" {
		t.Fatalf("assistant text was not rebuilt: %+v", raw[1])
	}
	if got := st.DistilledCount(SessionPrefix+"ses_test", store.LensDefault); got != 0 {
		t.Fatalf("changed text should reset distill progress, got %d", got)
	}
}

func TestImporterReplacesAfterARevertDeletesRows(t *testing.T) {
	dbPath := seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, NULL, NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_revert', 'proj1', NULL, NULL, NULL, NULL, 'revert', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 5000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg_u1_1234567890abc', 'ses_revert', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"turn one"}');
		INSERT INTO session_message VALUES ('msg_a2_1234567890abc', 'ses_revert', 'assistant', 2, 2000, 2000, '{"time":{"created":2000,"completed":2000},"content":[{"type":"text","text":"turn two"}]}');
		INSERT INTO session_message VALUES ('msg_u3_1234567890abc', 'ses_revert', 'user', 3, 3000, 3000, '{"time":{"created":3000},"text":"turn three"}');
	`)
	mutateV2DB := func(path, stmt string) {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	fake := newFakeImportStore()
	im := &Importer{Store: fake, DBPath: dbPath}

	// Initial import
	if _, err := im.Import(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.imports) != 1 || len(fake.imports[0].records) != 3 {
		t.Fatalf("initial import: got %d calls with %d records, want 1 call with 3 records",
			len(fake.imports), len(fake.imports[0].records))
	}

	// Revert deletes seq >= 3
	mutateV2DB(dbPath, `
		DELETE FROM session_message WHERE seq >= 3;
		UPDATE session_v2 SET time_updated = 6000 WHERE id = 'ses_revert';
	`)

	if _, err := im.Import(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.imports) != 2 {
		t.Fatalf("after revert: got %d import calls, want 2", len(fake.imports))
	}
	secondCall := fake.imports[1]
	if !secondCall.replace {
		t.Fatalf("after revert: replace = false, want true (rows were deleted)")
	}
	if len(secondCall.records) != 2 {
		t.Fatalf("after revert: got %d records, want 2", len(secondCall.records))
	}
}

func TestImportSkipsLegacyWitnessDistillSessions(t *testing.T) {
	dbPath := seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, NULL, NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_scratch', 'proj1', NULL, NULL, NULL, NULL, 'scratch', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, 'witness-distill', NULL, 1000, 5000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_v2 VALUES ('ses_real', 'proj1', NULL, NULL, NULL, NULL, 'real', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 6000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg_s1_1234567890abc', 'ses_scratch', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"scratch user"}');
		INSERT INTO session_message VALUES ('msg_r1_1234567890abc', 'ses_real', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"real user"}');
	`)

	fake := newFakeImportStore()
	im := &Importer{Store: fake, DBPath: dbPath}

	stats, err := im.Import(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 1 {
		t.Fatalf("stats.Sessions = %d, want 1 (scratch skipped)", stats.Sessions)
	}
	if fake.rawCount[SessionPrefix+"ses_scratch"] != 0 {
		t.Fatalf("witness-distill session was imported")
	}
	if fake.rawCount[SessionPrefix+"ses_real"] == 0 {
		t.Fatalf("real session was not imported")
	}

	// Even when named by id
	stats, err = im.Import(context.Background(), []string{"ses_scratch"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 0 {
		t.Fatalf("explicitly named scratch session returned %d records, want 0", stats.Records)
	}
}

func TestImportRefusesADatabaseItCannotRead(t *testing.T) {
	// v1-only DB
	dbPath := seedV2DB(t, `
		DROP TABLE session_v2; DROP TABLE session_message; DROP TABLE session_part;
		CREATE TABLE session (id text PRIMARY KEY, time_updated integer);
	`)
	fake := newFakeImportStore()
	_, err := (&Importer{Store: fake, DBPath: dbPath}).Import(context.Background(), nil)
	if !errors.Is(err, ErrOpenCodeV1) {
		t.Fatalf("v1-only DB: err = %v, want ErrOpenCodeV1", err)
	}
	if len(fake.imports) > 0 {
		t.Fatalf("v1-only DB: wrote %d imports, want 0", len(fake.imports))
	}
	if fake.meta[syncMetaKey] != "" {
		t.Fatalf("v1-only DB: wrote meta, want nothing")
	}

	// Mid-migration DB
	dbPath2 := seedV2DB(t, `
		CREATE TABLE session (id text PRIMARY KEY, time_updated integer);
		INSERT INTO kv VALUES ('migration.v1-v2', '{"phase":"running"}', 1000, 1000);
	`)
	fake2 := newFakeImportStore()
	_, err = (&Importer{Store: fake2, DBPath: dbPath2}).Import(context.Background(), nil)
	if !errors.Is(err, ErrOpenCodeSchema) {
		t.Fatalf("mid-migration DB: err = %v, want ErrOpenCodeSchema", err)
	}
	if len(fake2.imports) > 0 {
		t.Fatalf("mid-migration DB: wrote %d imports, want 0", len(fake2.imports))
	}
}

func TestImportReadsAWALDatabaseCopiedWithoutItsSidecars(t *testing.T) {
	dbPath := seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, NULL, NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_test', 'proj1', NULL, NULL, NULL, NULL, 'test', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 5000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg_u1_1234567890abc', 'ses_test', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"user text"}');
	`)

	// Turn on WAL mode
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	// Remove sidecars
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	if err := os.Remove(walPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Remove(shmPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	fake := newFakeImportStore()
	stats, err := (&Importer{Store: fake, DBPath: dbPath}).Import(context.Background(), nil)
	if err != nil {
		t.Fatalf("import after WAL sidecars removed: %v", err)
	}
	if stats.Sessions != 1 {
		t.Fatalf("import after WAL sidecars removed: stats = %+v, want 1 session", stats)
	}
}

func TestTranscriptDigestMatchesTheImportedL0(t *testing.T) {
	dbPath := seedOpenCodeDB(t)
	fake := newFakeImportStore()

	if _, err := (&Importer{Store: fake, DBPath: dbPath}).Import(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.imports) == 0 || len(fake.imports[0].records) == 0 {
		t.Fatal("no records were imported")
	}

	var entries []platform.TranscriptEntry
	for _, r := range fake.imports[0].records {
		entries = append(entries, platform.TranscriptEntry{Role: r.Role, Text: r.Text})
	}
	digestFromImport := platform.TranscriptDigest(entries)

	db, err := sql.Open("sqlite", sqliteURI(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	turns, err := ReadTurns(context.Background(), db, "ses_test")
	if err != nil {
		t.Fatal(err)
	}
	digestFromRead := TranscriptDigest(turns)

	if digestFromImport != digestFromRead {
		t.Fatalf("digest from import = %q, from ReadTurns = %q, want equal", digestFromImport, digestFromRead)
	}
}

func TestImportRecordsTheNewestActivityItSaw(t *testing.T) {
	dbPath := seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, NULL, NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_new', 'proj1', NULL, NULL, NULL, NULL, 'new', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 9000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_v2 VALUES ('ses_old', 'proj1', NULL, NULL, NULL, NULL, 'old', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 5000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg_n1_1234567890abc', 'ses_new', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"new"}');
		INSERT INTO session_message VALUES ('msg_o1_1234567890abc', 'ses_old', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"old"}');
	`)

	fake := newFakeImportStore()
	im := &Importer{Store: fake, DBPath: dbPath}

	// Targeted import of newer session
	if _, err := im.Import(context.Background(), []string{"ses_new"}); err != nil {
		t.Fatal(err)
	}
	if LastImported(fake).UnixMilli() != 9000 {
		t.Fatalf("after importing ses_new: LastImported = %d, want 9000", LastImported(fake).UnixMilli())
	}
	if fake.meta[syncMetaKey] != "" {
		t.Fatalf("targeted import advanced full-sync watermark")
	}

	// Targeted import of older session leaves 9000
	if _, err := im.Import(context.Background(), []string{"ses_old"}); err != nil {
		t.Fatal(err)
	}
	if LastImported(fake).UnixMilli() != 9000 {
		t.Fatalf("after importing ses_old: LastImported = %d, want 9000 (unchanged)", LastImported(fake).UnixMilli())
	}
}

func TestNewestActivity(t *testing.T) {
	// Missing file
	_, err := NewestActivity(context.Background(), "/nonexistent/path.db")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: err = %v, want fs.ErrNotExist", err)
	}

	// v2 DB with idle at 8000
	dbPath := seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, NULL, NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_top', 'proj1', NULL, NULL, NULL, NULL, 'top', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 10000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_v2 VALUES ('ses_child', 'proj1', NULL, 'ses_top', NULL, NULL, 'child', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, 'subagent', NULL, 1000, 11000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg_t1_1234567890abc', 'ses_top', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"top user"}');
		INSERT INTO session_message VALUES ('msg_t2_1234567890abc', 'ses_top', 'idle', 2, 8000, 8000, '{"outcome":"completed"}');
		INSERT INTO session_message VALUES ('msg_c1_1234567890abc', 'ses_child', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"child user"}');
		INSERT INTO session_message VALUES ('msg_c2_1234567890abc', 'ses_child', 'idle', 2, 9000, 9000, '{"outcome":"completed"}');
	`)
	newest, err := NewestActivity(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if newest.UnixMilli() != 8000 {
		t.Fatalf("NewestActivity = %d, want 8000 (child idle is ignored)", newest.UnixMilli())
	}

	// v1-only DB
	dbPath2 := seedV2DB(t, `
		DROP TABLE session_v2; DROP TABLE session_message; DROP TABLE session_part;
		CREATE TABLE session (id text PRIMARY KEY, time_updated integer);
	`)
	_, err = NewestActivity(context.Background(), dbPath2)
	if !errors.Is(err, ErrOpenCodeV1) {
		t.Fatalf("v1-only DB: err = %v, want ErrOpenCodeV1", err)
	}
}

func TestNewestActivityIgnoresATurnStillRunning(t *testing.T) {
	dbPath := seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, NULL, NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_long', 'proj1', NULL, NULL, NULL, NULL, 'long', '/repo', NULL, NULL, '2.3.0', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 9000000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg_u1_1234567890abc', 'ses_long', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"user"}');
		INSERT INTO session_message VALUES ('msg_i1_1234567890abc', 'ses_long', 'idle', 2, 8000, 8000, '{"outcome":"completed"}');
		INSERT INTO session_message VALUES ('msg_u2_1234567890abc', 'ses_long', 'user', 3, 8500, 8500, '{"time":{"created":8500},"text":"next user"}');
		INSERT INTO session_message VALUES ('msg_a2_1234567890abc', 'ses_long', 'assistant', 4, 9000, 9000, '{"time":{"created":9000},"content":[{"type":"text","text":"streaming"}]}');
	`)
	newest, err := NewestActivity(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if newest.UnixMilli() != 8000 {
		t.Fatalf("NewestActivity = %d, want 8000 (unsettled assistant is ignored)", newest.UnixMilli())
	}
}

func TestReadersNeverWriteTheSourceDB(t *testing.T) {
	dbPath := seedV2DB(t, `
		INSERT INTO project VALUES ('proj1', '/repo', NULL, NULL, NULL, NULL, NULL, 1000, 1000, NULL, 0, NULL, '[]', NULL);
		INSERT INTO session_v2 VALUES ('ses_test', 'proj1', NULL, NULL, NULL, NULL, 'test', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 5000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_message VALUES ('msg_u1_1234567890abc', 'ses_test', 'user', 1, 1100, 1100, '{"time":{"created":1100},"text":"user"}');
	`)

	// Turn on WAL mode
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	// Hash the DB file
	hashFile := func(path string) string {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%x", h.Sum(nil))
	}
	before := hashFile(dbPath)

	// Full import
	fake := newFakeImportStore()
	if _, err := (&Importer{Store: fake, DBPath: dbPath}).Import(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// Targeted import
	if _, err := (&Importer{Store: fake, DBPath: dbPath}).Import(context.Background(), []string{"ses_test"}); err != nil {
		t.Fatal(err)
	}

	// NewestActivity
	if _, err := NewestActivity(context.Background(), dbPath); err != nil {
		t.Fatal(err)
	}

	// ProbeSchema
	db2, err := sql.Open("sqlite", sqliteURI(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := ProbeSchema(context.Background(), db2); err != nil {
		db2.Close()
		t.Fatal(err)
	}
	db2.Close()

	after := hashFile(dbPath)
	if before != after {
		t.Fatalf("opencode.db was modified: before=%s after=%s", before, after)
	}

	// No -journal file
	journalPath := dbPath + "-journal"
	if _, err := os.Stat(journalPath); err == nil {
		t.Fatalf("%s exists, reader wrote in non-WAL mode", journalPath)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || findInString(s, sub))
}

func findInString(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
