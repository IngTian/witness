package opencode

import (
	"context"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// fakeImportStore is a hand-written store.ImportStore with NO witness *sql.DB behind
// it (issue #73-C1, Phase B): an in-memory meta map, a raw-row counter per session,
// and recorders for the import commit + platform stamp. It proves the OpenCode
// Importer depends only on the narrow store.ImportStore slice, not the whole *Store.
type fakeImportStore struct {
	meta     map[string]string
	rawCount map[string]int

	imports  []fakeImportCall
	stamped  map[string]string
	lockHeld bool
}

type fakeImportCall struct {
	meta       store.SessionMeta
	records    []store.RawRecord
	stateKey   string
	stateValue string
	replace    bool
}

func newFakeImportStore() *fakeImportStore {
	return &fakeImportStore{
		meta:     map[string]string{},
		rawCount: map[string]int{},
		stamped:  map[string]string{},
	}
}

func (f *fakeImportStore) MetaString(key string) string { return f.meta[key] }
func (f *fakeImportStore) SetMetaString(key, value string) error {
	f.meta[key] = value
	return nil
}
func (f *fakeImportStore) SetSessionPlatform(session, platform string) {
	f.stamped[session] = platform
}
func (f *fakeImportStore) ImportLock(name string) (func(), bool) {
	f.lockHeld = true
	return func() { f.lockHeld = false }, true
}
func (f *fakeImportStore) RawCount(session string) int { return f.rawCount[session] }
func (f *fakeImportStore) ApplyRawImport(meta store.SessionMeta, records []store.RawRecord, stateKey, stateValue string, replace bool) error {
	f.imports = append(f.imports, fakeImportCall{meta, records, stateKey, stateValue, replace})
	f.rawCount[meta.Session] += len(records) // reflect the write so a re-import sees it
	return nil
}

var _ store.ImportStore = (*fakeImportStore)(nil)

// TestImporterRunsAgainstFakeStore drives Importer.Import end-to-end reading a real
// OpenCode source DB but committing into a fake store.ImportStore — no witness *sql.DB
// exists. It asserts the day-one path takes the non-replace (append) branch, stamps the
// owning platform, and reports the right stats, proving the importer is decoupled from
// the concrete store.
func TestImporterRunsAgainstFakeStore(t *testing.T) {
	dbPath := seedOpenCodeDB(t) // real OpenCode source fixture (helper in import_test.go)
	fake := newFakeImportStore()

	im := &Importer{Store: fake, DBPath: dbPath}
	stats, err := im.Import(context.Background(), nil)
	if err != nil {
		t.Fatalf("Import against fake store: %v", err)
	}
	if stats.Sessions == 0 || stats.Records == 0 {
		t.Fatalf("expected a non-empty import, got %+v", stats)
	}
	if len(fake.imports) == 0 {
		t.Fatal("ApplyRawImport was never called on the fake store")
	}
	// Day-one: no prior watermark/rawCount, so the first commit must be an append
	// (replace=false), not a rebuild.
	if fake.imports[0].replace {
		t.Fatalf("day-one import should be append (replace=false), got replace=true")
	}
	// The owning platform is stamped for the imported session.
	for sess := range fake.stamped {
		if fake.stamped[sess] != "opencode" {
			t.Fatalf("session %q stamped %q, want opencode", sess, fake.stamped[sess])
		}
	}
	if len(fake.stamped) == 0 {
		t.Fatal("no session had its platform stamped")
	}
	// The full-sync watermark was persisted via the narrow MetaKV surface.
	if fake.meta[syncMetaKey] == "" {
		t.Fatalf("full-sync watermark %q not set on the fake store", syncMetaKey)
	}
}
