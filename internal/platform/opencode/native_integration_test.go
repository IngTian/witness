package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

func TestNativeOpenCodeIsolatedSmoke(t *testing.T) {
	if os.Getenv("WITNESS_OPENCODE_INTEGRATION") != "1" {
		t.Skip("set WITNESS_OPENCODE_INTEGRATION=1 to run the real OpenCode smoke test")
	}
	model := os.Getenv("WITNESS_OPENCODE_INTEGRATION_MODEL")
	if model == "" {
		t.Fatal("WITNESS_OPENCODE_INTEGRATION_MODEL is required")
	}
	userDB, err := DefaultDBPath()
	if err != nil {
		t.Fatal(err)
	}
	auth, err := os.ReadFile(filepath.Join(filepath.Dir(userDB), "auth.json"))
	if err != nil {
		t.Fatalf("read OpenCode auth for isolated copy: %v", err)
	}

	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source-runtime")
	for _, authPath := range []string{
		filepath.Join(sourceRoot, "auth.json"),
		filepath.Join(sourceRoot, "xdg", "opencode", "auth.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(authPath, auth, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	source, err := StartOpenCodeServerIn(ctx, sourceRoot, model)
	if err != nil {
		t.Fatalf("start synthetic source: %v", err)
	}
	sessionID, err := source.createSession(ctx, model)
	if err != nil {
		_ = source.Close()
		t.Fatalf("create synthetic source session: %v", err)
	}
	if _, err := source.doJSON(ctx, http.MethodPost, "/session/"+sessionID+"/message", map[string]any{
		"noReply": true,
		"parts":   []map[string]any{{"type": "text", "text": "fixture context"}},
	}, http.StatusOK); err != nil {
		_ = source.Close()
		t.Fatalf("write synthetic source session: %v", err)
	}
	_ = source.Close()

	sourceDB := filepath.Join(sourceRoot, "opencode.db")
	t.Setenv("WITNESS_OPENCODE_DB", sourceDB)
	before := integrationSessionCounts(t, sourceDB)
	r := &runner{cfg: store.Config{Runner: "opencode", RuntimeRoot: filepath.Join(root, "runtime"), TriageModel: model}}
	if err := r.Open(ctx); err != nil {
		t.Fatalf("open isolated runner: %v", err)
	}
	defer func() { _ = r.Close() }()
	native := &platform.NativeSession{
		Session: SessionPrefix + sessionID, RawHigh: 1, Total: 1, Lens: "smoke", Input: "0:fixture",
		Digest: platform.TranscriptDigest([]platform.TranscriptEntry{{Role: "user", Text: "fixture context"}}),
	}
	reply, err := r.Run(platform.WithNativeSession(ctx, native), model, "Return exactly [] and nothing else.", "fixture")
	if err != nil {
		t.Fatalf("native mining: %v", err)
	}
	if strings.TrimSpace(reply) == "" {
		t.Fatal("native mining returned an empty reply")
	}
	if after := integrationSessionCounts(t, sourceDB); after != before {
		t.Fatalf("source OpenCode DB changed: before=%v after=%v", before, after)
	}
	isolatedDB := filepath.Join(root, "runtime", "opencode.db")
	if counts := integrationSessionCounts(t, isolatedDB); counts.sessions != 1 {
		t.Fatalf("isolated fork not retained before commit: %+v", counts)
	}
	if err := native.Finalize(); err != nil {
		t.Fatalf("finalize isolated fork: %v", err)
	}
	if counts := integrationSessionCounts(t, isolatedDB); counts.sessions != 0 {
		t.Fatalf("isolated fork retained after commit: %+v", counts)
	}
}

type integrationCounts struct{ sessions, messages, parts int }

func integrationSessionCounts(t *testing.T, path string) integrationCounts {
	t.Helper()
	db, err := sql.Open("sqlite", readOnlyURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var counts integrationCounts
	for table, target := range map[string]*int{"session": &counts.sessions, "message": &counts.messages, "part": &counts.parts} {
		if err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(target); err != nil {
			t.Fatalf("count %s in %s: %v", table, path, err)
		}
	}
	return counts
}
