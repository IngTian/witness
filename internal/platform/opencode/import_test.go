package opencode

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// Fixture rows name only the columns the 2.2.7 schema requires (NOT NULL, no default) plus the
// few the importer reads, so refreshing testdata/opencode-2.2.7-schema.sql breaks none of them.
// Message ids are native-shaped (msgID) because the fork-copy marker is defined by that shape: a
// fixture id with a '_' after "msg_", or none of the prefix, lets an over-matching marker pass.

// mutateV2DB runs stmts in one Exec with foreign keys off, so a fixture needs no project row.
func mutateV2DB(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA foreign_keys=OFF;\n" + strings.Join(stmts, "\n")); err != nil {
		t.Fatal(err)
	}
}

// seedV2DB creates a temporary DB with OpenCode 2.2.7's schema and rows, and returns its path.
func seedV2DB(t *testing.T, rows ...string) string {
	t.Helper()
	schema, err := os.ReadFile("testdata/opencode-2.2.7-schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "opencode.db")
	mutateV2DB(t, path, append([]string{string(schema)}, rows...)...)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// msgID is a native-shaped id ("msg_" + 12 hex + 14 alphanumerics, no '_'), unique per
// (session, seq). It is hashed rather than counted so id order differs from seq order, as on
// real DBs (storage §3): a reader that sorts by id gets the turns out of order.
func msgID(ses string, seq int64) string {
	return fmt.Sprintf("msg_%x", sha256.Sum256(fmt.Appendf(nil, "%s/%d", ses, seq)))[:30]
}

// session is a top-level session in /repo created at 1000; set adds column/value pairs
// (parent_id, fork_session_id, agent).
func session(id string, updated int64, set ...string) string {
	cols := "id, project_id, slug, directory, version, time_created, time_updated"
	vals := fmt.Sprintf("'%s', 'prj', 'slug', '/repo', '2.2.7', 1000, %d", id, updated)
	for i := 0; i+1 < len(set); i += 2 {
		cols += ", " + set[i]
		vals += ", '" + set[i+1] + "'"
	}
	return "INSERT INTO session_v2 (" + cols + ") VALUES (" + vals + ");"
}

func touch(ses string, updated int64) string {
	return fmt.Sprintf("UPDATE session_v2 SET time_updated = %d WHERE id = '%s';", updated, ses)
}

// row is one session_message with an explicit id; msg gives it the native one.
func row(id, ses, typ string, seq, created int64, data string) string {
	return fmt.Sprintf("INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('%s', '%s', '%s', %d, %d, %d, '%s');",
		id, ses, typ, seq, created, created, data)
}

func msg(ses, typ string, seq, created int64, data string) string {
	return row(msgID(ses, seq), ses, typ, seq, created, data)
}

func user(ses string, seq, created int64, text string) string {
	return msg(ses, "user", seq, created, fmt.Sprintf(`{"time":{"created":%d},"text":%q}`, created, text))
}

// assistant is one model step; completed 0 leaves it streaming. items are its inline content,
// none when its text sits in session_part rows.
func assistant(ses string, seq, created, completed int64, items ...string) string {
	tm := fmt.Sprintf(`{"created":%d}`, created)
	if completed > 0 {
		tm = fmt.Sprintf(`{"created":%d,"completed":%d}`, created, completed)
	}
	return msg(ses, "assistant", seq, created, fmt.Sprintf(`{"time":%s,"agent":"build","content":[%s]}`, tm, strings.Join(items, ",")))
}

func idle(ses string, seq, created int64) string {
	return msg(ses, "idle", seq, created, `{"outcome":"completed"}`)
}

// item is one content item, inline or as a session_part's data.
func item(typ, text string) string { return fmt.Sprintf(`{"type":%q,"text":%q}`, typ, text) }

const toolItem = `{"type":"tool","id":"call_1","name":"bash","state":{"status":"completed"}}`

// part stores content item it at ordinal of message msgID(ses, seq); its type comes from it.
func part(ses string, seq, ordinal int64, it string) string {
	return fmt.Sprintf("INSERT INTO session_part (message_id, session_id, ordinal, type, time_created, time_updated, data) VALUES ('%s', '%s', %d, json_extract('%s', '$.type'), 0, 0, '%s');",
		msgID(ses, seq), ses, ordinal, it, it)
}

// v1Only is a DB OpenCode v1 wrote; v1Leftover is a v2 DB that still holds v1's table, as an
// in-place migration in the given phase leaves it.
const v1Only = `DROP TABLE session_v2; DROP TABLE session_message; DROP TABLE session_part;
	CREATE TABLE session (id text PRIMARY KEY, time_updated integer);`

func v1Leftover(phase string) string {
	return `CREATE TABLE session (id text PRIMARY KEY, time_updated integer);
	INSERT INTO kv (key, value, time_created, time_updated) VALUES ('migration.v1-v2', '{"phase":"` + phase + `"}', 1000, 1000);`
}

// seedOpenCodeDB is the shared fixture: ses_test (/repo, created 1000, updated 5000) with a user
// turn, one completed assistant whose text sits in session_part around a tool item, and a later
// assistant still streaming.
func seedOpenCodeDB(t *testing.T) string {
	t.Helper()
	return seedV2DB(t,
		session("ses_test", 5000),
		user("ses_test", 1, 1100, "help me debug this"),
		assistant("ses_test", 2, 2000, 4000),
		part("ses_test", 2, 0, item("text", "first note")),
		part("ses_test", 2, 1, toolItem),
		part("ses_test", 2, 2, item("text", "final answer")),
		assistant("ses_test", 3, 6000, 0, item("text", "partial stream")),
	)
}

func openRO(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteURI(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func readTurns(t *testing.T, path, ses string) []Turn {
	t.Helper()
	turns, err := ReadTurns(context.Background(), openRO(t, path), ses)
	if err != nil {
		t.Fatal(err)
	}
	return turns
}

func roleText(turns []Turn) (out []string) {
	for _, x := range turns {
		out = append(out, x.Role+":"+x.Text)
	}
	return out
}

// testStore opens a fresh witness archive under a temp WITNESS_HOME.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// mustImport imports ids (all sessions when none) and fails the test on error.
func mustImport(t *testing.T, st store.ImportStore, path string, ids ...string) platform.ImportStats {
	t.Helper()
	stats, err := (&Importer{Store: st, DBPath: path}).Import(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func TestDefaultDBPathOrder(t *testing.T) {
	// Every path is built natively (filepath.Join, t.TempDir) because "/abs/x.db" is not absolute
	// on Windows, and every case sets every variable ("" = unset; DefaultDBPath trims) so the
	// developer's own OPENCODE_DB or XDG_DATA_HOME cannot leak in. os.UserHomeDir reads USERPROFILE
	// on Windows, HOME elsewhere.
	dir := t.TempDir()
	home, data := filepath.Join(dir, "home"), filepath.Join(dir, "data")
	w, abs := filepath.Join(dir, "w.db"), filepath.Join(dir, "x.db")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cases := []struct{ name, witness, opencode, xdg, want string }{
		{"WITNESS_OPENCODE_DB wins", w, abs, data, w},
		{"OPENCODE_DB absolute as is", "", abs, data, abs},
		{"OPENCODE_DB relative resolved", "", "rel.db", data, filepath.Join(data, "opencode", "rel.db")},
		{"OPENCODE_DB :memory: ignored", "", ":memory:", data, filepath.Join(data, "opencode", "opencode.db")},
		{"XDG_DATA_HOME default", "", "", data, filepath.Join(data, "opencode", "opencode.db")},
		{"HOME fallback", "", "", "", filepath.Join(home, ".local", "share", "opencode", "opencode.db")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WITNESS_OPENCODE_DB", tc.witness)
			t.Setenv("OPENCODE_DB", tc.opencode)
			t.Setenv("XDG_DATA_HOME", tc.xdg)
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
		wantEnd string // the error's exact tail: a probe naming more than what is missing fails
	}{
		{"fresh v2 DB", "", nil, ""},
		{"v1 only", v1Only, ErrOpenCodeV1, ""},
		{"v2 missing session_part", `DROP TABLE session_part;`, ErrOpenCodeSchema, ": missing session_part"},
		// Only a DB with NO v2 table is v1's; a partial v2 beside v1's table is a broken v2
		{"v2 missing session_part beside v1's table", `DROP TABLE session_part;` + v1Leftover("completed"), ErrOpenCodeSchema, ": missing session_part"},
		{"v2 missing a column", `ALTER TABLE session_v2 DROP COLUMN fork_session_id;`, ErrOpenCodeSchema, ": missing session_v2.fork_session_id"},
		{"v2 with v1 + migration running", v1Leftover("running"), ErrOpenCodeSchema, "still migrating this database from v1; import again when it finishes"},
		{"v2 with v1 + migration completed", v1Leftover("completed"), nil, ""},
		// A NULL phase scanned into a string was a raw "converting NULL" error, not a refusal
		{"v2 with v1 + migration record without a phase", `CREATE TABLE session (id text PRIMARY KEY);
			INSERT INTO kv (key, value, time_created, time_updated) VALUES ('migration.v1-v2', '{}', 1000, 1000);`,
			ErrOpenCodeSchema, "still migrating this database from v1; import again when it finishes"},
		{"v2 with v1 + no kv table", `DROP TABLE kv; CREATE TABLE session (id text PRIMARY KEY);`,
			ErrOpenCodeSchema, "witness cannot tell whether OpenCode finished migrating this database from v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ProbeSchema(context.Background(), openRO(t, seedV2DB(t, tc.stmt)))
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ProbeSchema() err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ProbeSchema() err = %v, want %v", err, tc.wantErr)
			}
			if !strings.HasSuffix(err.Error(), tc.wantEnd) {
				t.Fatalf("ProbeSchema() err = %v, want it to end %q", err, tc.wantEnd)
			}
		})
	}
}

func TestReadTurnsAppliesEveryV2SkipRule(t *testing.T) {
	const s = "ses_all"
	path := seedV2DB(t,
		session(s, 12000),
		user(s, 1, 1100, "first question"),
		assistant(s, 2, 2000, 2500, item("reasoning", "think"), item("text", "Let me check."), toolItem),
		assistant(s, 3, 3000, 3500),
		part(s, 3, 0, item("text", "part one")),
		part(s, 3, 1, item("reasoning", "hidden")),
		part(s, 3, 2, item("text", "part two")),
		idle(s, 4, 3600),
		// A subagent report keeps the id it was minted with earlier, so it sorts before seq 1's
		row("msg_"+strings.Repeat("0", 26), s, "synthetic", 5, 5000, `{"text":"<subagent sessionID=child>child says</subagent>","metadata":{"source":"subagent"}}`),
		assistant(s, 6, 6000, 6500, item("text", "after the report")),
		msg(s, "shell", 7, 7000, `{"command":"ls","status":"completed"}`),
		msg(s, "compaction", 8, 8000, `{"status":"completed"}`),
		msg(s, "user", 9, 9000, `{"time":{"created":9000},"files":[{"name":"f.txt"}]}`),
		assistant(s, 10, 10000, 0, item("text", "streaming")),
		msg(s, "assistant", 11, 11000, `{"time":{"created":11000,"completed":11500},"error":{"message":"failed"},"content":[]}`),
		msg(s, "assistant", 12, 12000, `{"time":{"created":12000,"completed":12500},"error":{"message":"partial"},"content":[`+item("text", "partial before error")+`]}`),
	)

	turns := readTurns(t, path, s)
	var gotSeq, gotTS []int64
	for _, turn := range turns {
		gotSeq = append(gotSeq, turn.Seq)
		gotTS = append(gotTS, turn.TS)
	}
	want := []string{"user:first question", "assistant:Let me check.", "assistant:part one\n\npart two",
		"assistant:after the report", "assistant:partial before error"}
	if got := roleText(turns); !reflect.DeepEqual(got, want) {
		t.Fatalf("turns = %#v, want %#v", got, want)
	}
	if want := []int64{1, 2, 3, 6, 12}; !reflect.DeepEqual(gotSeq, want) {
		t.Fatalf("seq = %v, want %v", gotSeq, want)
	}
	if want := []int64{1100, 2000, 3000, 6000, 12000}; !reflect.DeepEqual(gotTS, want) {
		t.Fatalf("TS = %v, want %v", gotTS, want)
	}
}

func TestReadTurnsSkipsForkCopies(t *testing.T) {
	path := seedV2DB(t,
		// A fork copies the source's rows with ids msg_<event id>_<seq> that keep its seq (storage §6)
		session("ses_fork", 3000, "fork_session_id", "ses_src"),
		row(msgID("ses_src", 1)+"_1", "ses_fork", "user", 1, 1100, `{"time":{"created":1100},"text":"copied user"}`),
		row(msgID("ses_src", 2)+"_2", "ses_fork", "assistant", 2, 2000, `{"time":{"created":2000,"completed":2000},"content":[`+item("text", "copied assistant")+`]}`),
		row("msg_fork_notice_ses_fork", "ses_fork", "synthetic", 4, 2500, `{"text":"<fork-notice>"}`),
		user("ses_fork", 3, 3000, "my own turn"),
		// The same _<seq> shape outside a fork is an ordinary row: the marker is gated on fork_session_id
		session("ses_normal", 1000),
		row(msgID("ses_x", 1)+"_1", "ses_normal", "user", 1, 1100, `{"time":{"created":1100},"text":"not a fork copy"}`),
	)
	if got, want := roleText(readTurns(t, path, "ses_fork")), []string{"user:my own turn"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("turns = %#v, want %#v (fork copies must be skipped, native rows kept)", got, want)
	}
	if got, want := roleText(readTurns(t, path, "ses_normal")), []string{"user:not a fork copy"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("non-fork session must keep _N ids, got %#v", got)
	}
}

func TestImporterMirrorsOpenCodeTextParts(t *testing.T) {
	dbPath := seedOpenCodeDB(t)
	st := testStore(t)

	if stats := mustImport(t, st, dbPath); stats.Sessions != 1 || stats.Records != 2 {
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
	if stats := mustImport(t, st, dbPath); stats.Records != 0 {
		t.Fatalf("second import should be idempotent, wrote %d records", stats.Records)
	}
}

func TestImporterSkipsChildSessions(t *testing.T) {
	dbPath := seedV2DB(t,
		session("ses_parent", 2000),
		session("ses_child", 3000, "parent_id", "ses_parent"),
		user("ses_parent", 1, 1100, "parent user"),
		user("ses_child", 1, 1100, "child user"),
	)
	st := testStore(t)

	if stats := mustImport(t, st, dbPath); stats.Sessions != 1 {
		t.Fatalf("stats.Sessions = %d, want 1 (child skipped)", stats.Sessions)
	}
	if st.RawCount(SessionPrefix+"ses_child") != 0 {
		t.Fatalf("child session was imported when it should be skipped")
	}
	if st.RawCount(SessionPrefix+"ses_parent") == 0 {
		t.Fatalf("parent session was not imported")
	}
	// The plugin imports children by id: a targeted import of one returns 0 records
	if stats := mustImport(t, st, dbPath, "ses_child"); stats.Records != 0 {
		t.Fatalf("targeted import of child returned %d records, want 0", stats.Records)
	}
}

func TestImporterTargetsSessionsWithoutAdvancingFullSyncWatermark(t *testing.T) {
	dbPath := seedV2DB(t,
		session("ses_test", 5000),
		session("ses_other", 9000),
		user("ses_test", 1, 1100, "test user"),
		user("ses_other", 1, 6100, "do not import me"),
	)
	st := testStore(t)
	if err := st.SetMetaString(syncMetaKey, "1234"); err != nil {
		t.Fatal(err)
	}

	if stats := mustImport(t, st, dbPath, "ses_test"); stats.Sessions != 1 || stats.Records != 1 {
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
	dbPath := seedV2DB(t,
		session("ses_mutable", 6000),
		user("ses_mutable", 1, 1100, "USER-ONE"),
		assistant("ses_mutable", 2, 2000, 0, item("text", "ASSISTANT-ONE")),
		user("ses_mutable", 3, 3000, "USER-TWO"),
	)
	st := testStore(t)

	if stats := mustImport(t, st, dbPath); stats.Records != 2 {
		t.Fatalf("initial import wrote %d records, want 2", stats.Records)
	}
	if err := st.MarkDistilled(SessionPrefix+"ses_mutable", store.LensDefault, 2); err != nil {
		t.Fatal(err)
	}

	mutateV2DB(t, dbPath,
		`UPDATE session_message SET data = json_set(data, '$.time.completed', 2500) WHERE session_id = 'ses_mutable' AND seq = 2;`,
		touch("ses_mutable", 7000))
	if stats := mustImport(t, st, dbPath); stats.Records != 3 {
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
	st := testStore(t)

	mustImport(t, st, dbPath)
	if err := st.MarkDistilled(SessionPrefix+"ses_test", store.LensDefault, 2); err != nil {
		t.Fatal(err)
	}

	mutateV2DB(t, dbPath,
		"UPDATE session_part SET data = '"+item("text", "updated answer")+"' WHERE message_id = '"+msgID("ses_test", 2)+"' AND ordinal = 2;",
		touch("ses_test", 7000))
	mustImport(t, st, dbPath)

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
	dbPath := seedV2DB(t,
		session("ses_revert", 5000),
		user("ses_revert", 1, 1100, "turn one"),
		assistant("ses_revert", 2, 2000, 2000, item("text", "turn two")),
		user("ses_revert", 3, 3000, "turn three"),
	)
	fake := newFakeImportStore()

	mustImport(t, fake, dbPath)
	if len(fake.imports) != 1 || len(fake.imports[0].records) != 3 {
		t.Fatalf("initial import: got %d calls, want 1 call with 3 records", len(fake.imports))
	}

	// A revert commit deletes the rows from its boundary on and touches time_updated (storage §3)
	mutateV2DB(t, dbPath, `DELETE FROM session_message WHERE seq >= 3;`, touch("ses_revert", 6000))
	mustImport(t, fake, dbPath)
	if len(fake.imports) != 2 {
		t.Fatalf("after revert: got %d import calls, want 2", len(fake.imports))
	}
	if second := fake.imports[1]; !second.replace || len(second.records) != 2 {
		t.Fatalf("after revert: replace = %v with %d records, want true with 2 (rows were deleted)", second.replace, len(second.records))
	}
}

func TestImportSkipsLegacyWitnessDistillSessions(t *testing.T) {
	dbPath := seedV2DB(t,
		session("ses_scratch", 5000, "agent", "witness-distill"),
		session("ses_real", 6000), // agent NULL: imported, the `agent IS NOT ?` NULL-safety pin
		user("ses_scratch", 1, 1100, "scratch user"),
		user("ses_real", 1, 1100, "real user"),
	)
	fake := newFakeImportStore()

	if stats := mustImport(t, fake, dbPath); stats.Sessions != 1 {
		t.Fatalf("stats.Sessions = %d, want 1 (scratch skipped)", stats.Sessions)
	}
	if fake.rawCount[SessionPrefix+"ses_scratch"] != 0 {
		t.Fatalf("witness-distill session was imported")
	}
	if fake.rawCount[SessionPrefix+"ses_real"] == 0 {
		t.Fatalf("real session was not imported")
	}
	if stats := mustImport(t, fake, dbPath, "ses_scratch"); stats.Records != 0 {
		t.Fatalf("explicitly named scratch session returned %d records, want 0", stats.Records)
	}
}

func TestImportRefusesADatabaseItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		stmt string
		want error
	}{
		{"v1-only DB", v1Only, ErrOpenCodeV1},
		{"mid-migration DB", v1Leftover("running"), ErrOpenCodeSchema},
	} {
		fake := newFakeImportStore()
		_, err := (&Importer{Store: fake, DBPath: seedV2DB(t, tc.stmt)}).Import(context.Background(), nil)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if len(fake.imports) > 0 || len(fake.meta) > 0 {
			t.Fatalf("%s: wrote %d imports and meta %v, want nothing", tc.name, len(fake.imports), fake.meta)
		}
	}
}

func TestImportReadsAWALDatabaseCopiedWithoutItsSidecars(t *testing.T) {
	dbPath := seedV2DB(t, session("ses_test", 5000), user("ses_test", 1, 1100, "user text"), "PRAGMA journal_mode=WAL;")
	for _, sidecar := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + sidecar); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if stats := mustImport(t, newFakeImportStore(), dbPath); stats.Sessions != 1 {
		t.Fatalf("import after WAL sidecars removed: stats = %+v, want 1 session", stats)
	}
}

func TestTranscriptDigestMatchesTheImportedL0(t *testing.T) {
	dbPath := seedOpenCodeDB(t)
	fake := newFakeImportStore()
	mustImport(t, fake, dbPath)
	if len(fake.imports) == 0 || len(fake.imports[0].records) == 0 {
		t.Fatal("no records were imported")
	}

	var entries []platform.TranscriptEntry
	for _, r := range fake.imports[0].records {
		entries = append(entries, platform.TranscriptEntry{Role: r.Role, Text: r.Text})
	}
	fromImport, fromRead := platform.TranscriptDigest(entries), TranscriptDigest(readTurns(t, dbPath, "ses_test"))
	if fromImport != fromRead {
		t.Fatalf("digest from import = %q, from ReadTurns = %q, want equal", fromImport, fromRead)
	}
}

func TestImportRecordsTheNewestActivityItSaw(t *testing.T) {
	dbPath := seedV2DB(t,
		session("ses_new", 9500),
		session("ses_old", 5500),
		user("ses_new", 1, 1100, "new"),
		idle("ses_new", 2, 9000),
		user("ses_old", 1, 1100, "old"),
		idle("ses_old", 2, 5000),
	)
	fake := newFakeImportStore()

	mustImport(t, fake, dbPath, "ses_new")
	if got := LastImported(fake).UnixMilli(); got != 9000 {
		t.Fatalf("after importing ses_new: LastImported = %d, want 9000 (its idle, not time_updated)", got)
	}
	if fake.meta[syncMetaKey] != "" {
		t.Fatalf("targeted import advanced full-sync watermark")
	}
	mustImport(t, fake, dbPath, "ses_old")
	if got := LastImported(fake).UnixMilli(); got != 9000 {
		t.Fatalf("after importing ses_old: LastImported = %d, want 9000 (unchanged)", got)
	}
}

// An idle written with no step after it (an interrupt before the first step, a refused route) is
// newer than the session's time_updated. Seen recorded on time_updated read that turn as unimported
// for as long as no later turn moved it.
func TestATurnEndWithNoNewStepDoesNotReadAsStale(t *testing.T) {
	const updated = 1_000_000
	idleAt := updated + (StaleAfter + time.Minute).Milliseconds()
	dbPath := seedV2DB(t,
		session("ses_cut", updated),
		user("ses_cut", 1, updated, "stop that"),
		idle("ses_cut", 2, idleAt),
	)
	fake := newFakeImportStore()
	mustImport(t, fake, dbPath, "ses_cut")

	newest, err := NewestActivity(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if lag := newest.Sub(LastImported(fake)); lag > StaleAfter {
		t.Fatalf("after importing the session, NewestActivity is %s ahead of LastImported, want <= %s", lag, StaleAfter)
	}
}

// Unwire clears only the seen watermark: drain-start capture stops, and a re-wire still resumes
// from the full-sync watermark instead of re-reading every session.
func TestForgetCaptureStopsTheDrainStartImportAndKeepsTheSyncWatermark(t *testing.T) {
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir()) // no plugin file
	dbPath := seedV2DB(t, session("ses_a", 5000), user("ses_a", 1, 1100, "a"), idle("ses_a", 2, 4000))
	fake := newFakeImportStore()
	mustImport(t, fake, dbPath)
	if !(Platform{}).ImportAtDrainStart(fake) {
		t.Fatal("after an import: ImportAtDrainStart = false, want true")
	}
	if err := ForgetCapture(fake); err != nil {
		t.Fatal(err)
	}
	if (Platform{}).ImportAtDrainStart(fake) {
		t.Fatal("after ForgetCapture: ImportAtDrainStart = true, want false")
	}
	if fake.meta[syncMetaKey] != "5000" {
		t.Fatalf("after ForgetCapture: sync watermark = %q, want 5000 (kept)", fake.meta[syncMetaKey])
	}
}

func TestNewestActivity(t *testing.T) {
	if _, err := NewestActivity(context.Background(), filepath.Join(t.TempDir(), "missing.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: err = %v, want fs.ErrNotExist", err)
	}

	dbPath := seedV2DB(t,
		session("ses_top", 10000),
		session("ses_child", 11000, "parent_id", "ses_top"),
		user("ses_top", 1, 1100, "top user"),
		idle("ses_top", 2, 8000),
		user("ses_child", 1, 1100, "child user"),
		idle("ses_child", 2, 9000),
	)
	newest, err := NewestActivity(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if newest.UnixMilli() != 8000 {
		t.Fatalf("NewestActivity = %d, want 8000 (child idle is ignored)", newest.UnixMilli())
	}

	if _, err := NewestActivity(context.Background(), seedV2DB(t, v1Only)); !errors.Is(err, ErrOpenCodeV1) {
		t.Fatalf("v1-only DB: err = %v, want ErrOpenCodeV1", err)
	}
}

func TestNewestActivityIgnoresATurnStillRunning(t *testing.T) {
	dbPath := seedV2DB(t,
		session("ses_long", 9_000_000), // a 2.3.0 turn running for hours moves time_updated at every step
		user("ses_long", 1, 1100, "user"),
		idle("ses_long", 2, 8000),
		user("ses_long", 3, 8500, "next user"),
		assistant("ses_long", 4, 9000, 0, item("text", "streaming")),
	)
	newest, err := NewestActivity(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if newest.UnixMilli() != 8000 {
		t.Fatalf("NewestActivity = %d, want 8000 (unsettled assistant is ignored)", newest.UnixMilli())
	}
}

func TestReadersNeverWriteTheSourceDB(t *testing.T) {
	dbPath := seedV2DB(t, session("ses_test", 5000), user("ses_test", 1, 1100, "user"), "PRAGMA journal_mode=WAL;")
	sum := func() [32]byte {
		b, err := os.ReadFile(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	before := sum()

	fake := newFakeImportStore()
	mustImport(t, fake, dbPath)
	mustImport(t, fake, dbPath, "ses_test")
	if _, err := NewestActivity(context.Background(), dbPath); err != nil {
		t.Fatal(err)
	}
	db := openRO(t, dbPath)
	if err := ProbeSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	db.Close() // before hashing: a writer would checkpoint on the last close

	if after := sum(); before != after {
		t.Fatalf("opencode.db was modified: before=%x after=%x", before, after)
	}
	if _, err := os.Stat(dbPath + "-journal"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s-journal: stat err = %v, want not-exist (a reader wrote in rollback mode)", dbPath, err)
	}
}

// ImportAtDrainStart answers true only when OpenCode is wired: a plugin file exists
// OR an earlier import already recorded the user having OpenCode sessions. An unwired
// machine never gets captured through the drain-start gate.
func TestImportAtDrainStartFollowsTheWiring(t *testing.T) {
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
	fake := newFakeImportStore()
	p := Platform{}

	// No plugin file, no prior import → false.
	if got := p.ImportAtDrainStart(fake); got {
		t.Fatal("unwired machine must not trigger drain-start import, got true")
	}

	// Plugin file written → true.
	pluginPath := PluginPath()
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginPath, []byte("// test plugin"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !PluginInstalled() {
		t.Fatal("plugin file written but PluginInstalled() = false")
	}
	if got := p.ImportAtDrainStart(fake); !got {
		t.Fatal("plugin installed → ImportAtDrainStart must be true, got false")
	}

	// No plugin file but seenMetaKey set → true (npm users).
	if err := os.Remove(pluginPath); err != nil {
		t.Fatal(err)
	}
	if PluginInstalled() {
		t.Fatal("plugin file removed but PluginInstalled() = true")
	}
	fake.meta[seenMetaKey] = "1700000000000"
	if got := LastImported(fake); got.IsZero() {
		t.Fatal("seenMetaKey set but LastImported is zero")
	}
	if got := p.ImportAtDrainStart(fake); !got {
		t.Fatal("prior import recorded → ImportAtDrainStart must be true, got false")
	}
}
