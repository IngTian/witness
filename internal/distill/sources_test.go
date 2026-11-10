package distill

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// TestResolveSourcesWritesLinks verifies that ResolveSources calls the resolver,
// parses its reply, and stores the result with the correct hash. Also tests R5 (facet's
// full Value is sent, not its Claim) and lens scoping (another lens's key is absent from
// the input). Links and keywords must match the resolver's output, with sentence numbers
// 1-based in the reply but 0-based in keyword storage. Sentence Text must be set.
func TestResolveSourcesWritesLinks(t *testing.T) {
	s := newStore(t)
	// Seed default facets with a distinct claim to test R5.
	if err := s.WriteFacets([]store.Facet{
		{Lens: "default", Dimension: "traits", Key: "satisfices",
			Versions: []store.FacetVersion{{Value: "stops at good enough, rarely gold-plates", Claim: "satisfices", Confidence: 0.9}}},
		{Lens: "math", Dimension: "resilience", Key: "trip_wire",
			Versions: []store.FacetVersion{{Value: "recovers with arithmetic", Confidence: 0.87}}},
	}); err != nil {
		t.Fatalf("seed facets: %v", err)
	}

	// Write a profile so there's something to resolve.
	md := "Stops at good enough. Recovers with arithmetic."
	if err := s.WriteProfile("default", md); err != nil {
		t.Fatalf("WriteProfile: %v", err)
	}

	var capturedInput string
	fakeResolve := func(_ context.Context, _, prompt, input string) (string, error) {
		if prompt == "RESOLVE" {
			capturedInput = input
			return `{"links":[{"n":1,"keys":[{"key":"traits/satisfices","strength":"direct"}]}],"keywords":[{"k":"good enough","n":[1,2]}]}`, nil
		}
		return "", nil
	}

	sm := &Resolver{
		Store:      s,
		Config:     store.Config{},
		Lenses:     []*lens.Lens{{Name: "default"}},
		LensPrompt: "RESOLVE",
		Run:        fakeResolve,
	}

	resolved, err := sm.ResolveSources(context.Background())
	if err != nil {
		t.Fatalf("ResolveSources: %v", err)
	}
	// It reports what it resolved (the worker logs the count): the one notebook, and not the
	// unified portrait, which has no file and whose prompt is unset.
	if len(resolved) != 1 || resolved[0] != "default" {
		t.Fatalf("resolved = %v, want [default]", resolved)
	}
	if again, _ := sm.ResolveSources(context.Background()); len(again) != 0 {
		t.Fatalf("a second pass with nothing stale resolved %v, want nothing", again)
	}

	// R5: the facet's FULL value is present in the input.
	if !strings.Contains(capturedInput, "stops at good enough, rarely gold-plates") {
		t.Fatalf("input missing facet's full Value; got:\n%s", capturedInput)
	}
	// R5: the claim is NOT used as the facet text.
	if strings.Count(capturedInput, "satisfices") > 1 {
		// The key "satisfices" appears once as "traits/satisfices:". If the claim were used
		// as text, it would appear twice (in the key and as the text). Count > 1 means it did.
		t.Fatalf("input used the claim as facet text; got:\n%s", capturedInput)
	}
	// Lens scoping: another lens's key (math's trip_wire) is absent.
	if strings.Contains(capturedInput, "trip_wire") {
		t.Fatalf("input contained another lens's key; got:\n%s", capturedInput)
	}

	// Assert: the row exists.
	src, hash, ok, err := s.ReadPortraitSources("default")
	if err != nil {
		t.Fatalf("ReadPortraitSources: %v", err)
	}
	if !ok {
		t.Fatal("no sources row was written")
	}

	// The hash equals PortraitHash(md).
	want := store.PortraitHash(md)
	if hash != want {
		t.Fatalf("hash mismatch: got %s, want %s", hash, want)
	}

	// Sentence text is set.
	if len(src.Sentences) < 2 {
		t.Fatalf("sentences: got %d, want 2", len(src.Sentences))
	}
	if src.Sentences[0].Text != "Stops at good enough." {
		t.Fatalf("sentence 0 text: got %q, want %q", src.Sentences[0].Text, "Stops at good enough.")
	}

	// Sentence 0's first link is {default traits satisfices direct}.
	if len(src.Sentences[0].Links) < 1 {
		t.Fatal("sentence 0 has no links")
	}
	link := src.Sentences[0].Links[0]
	if link.Lens != "default" || link.Dimension != "traits" || link.Key != "satisfices" || link.Strength != store.LinkDirect {
		t.Fatalf("sentence 0 link: got %+v, want {default traits satisfices direct}", link)
	}

	// Keywords are stored with 0-based indexes.
	if len(src.Keywords) != 1 {
		t.Fatalf("keywords: got %d, want 1", len(src.Keywords))
	}
	kw := src.Keywords[0]
	if kw.Word != "good enough" {
		t.Fatalf("keyword word: got %q, want %q", kw.Word, "good enough")
	}
	// Reply had n:[1,2] (1-based) → stored as [0,1].
	if len(kw.Sentences) != 2 || kw.Sentences[0] != 0 || kw.Sentences[1] != 1 {
		t.Fatalf("keyword indexes: got %v, want [0 1]", kw.Sentences)
	}
}

// TestResolveSourcesFailureIsIsolated verifies that when the resolver returns an error,
// ResolveSources returns a non-nil error, no sources row is written, and profile files
// and signatures are unchanged.
func TestResolveSourcesFailureIsIsolated(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	// Write a profile.
	md := "Stops at good enough."
	if err := s.WriteProfile("default", md); err != nil {
		t.Fatalf("WriteProfile: %v", err)
	}

	// Capture the signature before resolution.
	sigBefore := s.MetaString("profile_sig:default")

	fakeResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			return "", context.DeadlineExceeded
		}
		return "", nil
	}

	sm := &Resolver{
		Store:      s,
		Config:     store.Config{},
		Lenses:     []*lens.Lens{{Name: "default"}},
		LensPrompt: "RESOLVE",
		Run:        fakeResolve,
	}

	_, err := sm.ResolveSources(context.Background())
	if err == nil {
		t.Fatal("ResolveSources should return an error when the resolver fails")
	}

	// No sources row was written.
	_, _, ok, rerr := s.ReadPortraitSources("default")
	if rerr != nil {
		t.Fatalf("ReadPortraitSources: %v", rerr)
	}
	if ok {
		t.Fatal("sources row was written despite resolver failure")
	}

	// Profile file and signature are unchanged.
	mdAfter, ok, _ := s.ReadProfile("default")
	if !ok || mdAfter != md {
		t.Fatalf("profile changed: ok=%v md=%q, want %q", ok, mdAfter, md)
	}
	sigAfter := s.MetaString("profile_sig:default")
	if sigAfter != sigBefore {
		t.Fatalf("signature changed: before=%q after=%q", sigBefore, sigAfter)
	}
}

// TestResolveSourcesIgnoresUnparseableReply verifies that replies which cannot be
// parsed as JSON store nothing. A reply wrapped in prose or a ```json fence IS parsed.
func TestResolveSourcesIgnoresUnparseableReply(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	cases := []struct {
		name  string
		reply string
		want  bool // should a row be written?
	}{
		{"plain text", "sure!", false},
		{"truncated json", "```json\n{\"links\": ", false},
		{"empty string", "", false},
		{"wrapped valid json", "Here:\n```json\n{\"links\":[],\"keywords\":[]}\n```", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh store per case.
			st := newStore(t)
			seedFacets(t, st)
			if err := st.WriteProfile("default", "test."); err != nil {
				t.Fatalf("WriteProfile: %v", err)
			}

			fakeResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
				if prompt == "RESOLVE" {
					return tc.reply, nil
				}
				return "", nil
			}

			sm := &Resolver{
				Store:      st,
				Config:     store.Config{},
				Lenses:     []*lens.Lens{{Name: "default"}},
				LensPrompt: "RESOLVE",
				Run:        fakeResolve,
			}

			_, _ = sm.ResolveSources(context.Background())

			_, _, ok, err := st.ReadPortraitSources("default")
			if err != nil {
				t.Fatalf("ReadPortraitSources: %v", err)
			}
			if ok != tc.want {
				t.Fatalf("sources row written: got %v, want %v", ok, tc.want)
			}
		})
	}
}

// TestResolveSourcesValidatesLinks verifies that unknown/archived keys, out-of-range
// sentence numbers, invalid strengths, excess links per sentence, and keys from another
// lens are dropped while valid links are kept. Duplicate keys are deduped (keeping stronger
// strength), multiple reply entries with the same n are merged, then results are sorted
// direct-first, then capped at 3. Keywords out of range are dropped, and more than 8 are
// truncated.
func TestResolveSourcesValidatesLinks(t *testing.T) {
	s := newStore(t)
	// Seed 5 default facets (4 active after archiving one) so we can test the 3-cap with 4+ keys.
	if err := s.WriteFacets([]store.Facet{
		{Lens: "default", Dimension: "traits", Key: "satisfices",
			Versions: []store.FacetVersion{{Value: "stops at good enough", Confidence: 0.9}}},
		{Lens: "default", Dimension: "traits", Key: "pragmatic",
			Versions: []store.FacetVersion{{Value: "prefers working code", Confidence: 0.85}}},
		{Lens: "default", Dimension: "traits", Key: "direct",
			Versions: []store.FacetVersion{{Value: "values clarity", Confidence: 0.88}}},
		{Lens: "default", Dimension: "thinking", Key: "lazy",
			Versions: []store.FacetVersion{{Value: "efficient not careless", Confidence: 0.92}}},
		{Lens: "default", Dimension: "thinking", Key: "concrete",
			Versions: []store.FacetVersion{{Value: "prefers examples", Confidence: 0.86}}},
		{Lens: "math", Dimension: "resilience", Key: "trip_wire",
			Versions: []store.FacetVersion{{Value: "recovers with arithmetic", Confidence: 0.87}}},
	}); err != nil {
		t.Fatalf("seed facets: %v", err)
	}

	// Archive one default facet to test that it's dropped.
	facets, _ := s.ReadActiveFacets()
	for i, f := range facets {
		if f.Lens == "default" && f.Key == "satisfices" {
			facets[i].ArchivedAt = time.Now().UTC().Format(time.RFC3339)
		}
	}
	if err := s.WriteFacets(facets); err != nil {
		t.Fatalf("archive facet: %v", err)
	}

	// Write a 2-sentence profile.
	md := "First sentence. Second sentence."
	if err := s.WriteProfile("default", md); err != nil {
		t.Fatalf("WriteProfile: %v", err)
	}

	// Resolver returns TWO separate "links" entries for n:1 (they must be merged):
	// Entry 1 has: pragmatic(D), pragmatic(P), direct(P)
	// Entry 2 has: lazy(P), concrete(D)
	// After merge + dedupe: pragmatic(D), direct(P), lazy(P), concrete(D) = 4 distinct keys.
	// After sort direct-first: pragmatic(D), concrete(D), direct(P), lazy(P).
	// After cap at 3: pragmatic(D), concrete(D), direct(P).
	// A truncate-then-sort bug would give: pragmatic(D), direct(P), lazy(P) (dropping concrete).
	//
	// Also tests:
	// - unknown key "nonexistent/key" (dropped)
	// - archived key "traits/satisfices" (dropped)
	// - another lens's key "resilience/trip_wire" (dropped)
	// - n:0 (dropped)
	// - n:99 (dropped)
	// - strength "strong" (becomes partial) — on sentence 2, where no cap can hide the result
	// - keyword with index 99 FIRST (dropped, not just masked by the 8-cap)
	// - keyword with mixed n:[1,99] (stores [0], dropping the 99)
	// - 10 more keywords (truncated to 8 total)
	reply := `{
		"links": [
			{
				"n": 0,
				"keys": [{"key": "traits/pragmatic", "strength": "direct"}]
			},
			{
				"n": 1,
				"keys": [
					{"key": "nonexistent/key", "strength": "direct"},
					{"key": "traits/satisfices", "strength": "direct"},
					{"key": "resilience/trip_wire", "strength": "direct"},
					{"key": "traits/pragmatic", "strength": "direct"},
					{"key": "traits/pragmatic", "strength": "partial"},
					{"key": "traits/direct", "strength": "partial"}
				]
			},
			{
				"n": 1,
				"keys": [
					{"key": "thinking/lazy", "strength": "partial"},
					{"key": "thinking/concrete", "strength": "direct"}
				]
			},
			{
				"n": 2,
				"keys": [{"key": "thinking/lazy", "strength": "strong"}]
			},
			{
				"n": 99,
				"keys": [{"key": "traits/pragmatic", "strength": "direct"}]
			}
		],
		"keywords": [
			{"k": "out", "n": [99]},
			{"k": "mixed", "n": [1, 99]},
			{"k": "w1", "n": [1]},
			{"k": "w2", "n": [1]},
			{"k": "w3", "n": [1]},
			{"k": "w4", "n": [1]},
			{"k": "w5", "n": [1]},
			{"k": "w6", "n": [1]},
			{"k": "w7", "n": [1]},
			{"k": "w8", "n": [1]}
		]
	}`

	fakeResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			return reply, nil
		}
		return "", nil
	}

	sm := &Resolver{
		Store:      s,
		Config:     store.Config{},
		Lenses:     []*lens.Lens{{Name: "default"}, {Name: "math"}},
		LensPrompt: "RESOLVE",
		Run:        fakeResolve,
	}

	if _, err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources: %v", err)
	}

	src, _, ok, err := s.ReadPortraitSources("default")
	if err != nil {
		t.Fatalf("ReadPortraitSources: %v", err)
	}
	if !ok {
		t.Fatal("no sources row")
	}

	// Sentence 0 (n:1 in reply) should have exactly 3 links after merge + dedupe + sort + cap.
	// The two reply entries for n:1 are merged; after filtering and deduplication, 4 distinct
	// keys remain (pragmatic, direct, lazy, concrete). After sort direct-first: [pragmatic(D),
	// concrete(D), direct(P), lazy(P)]. After cap at 3: [pragmatic(D), concrete(D), direct(P)].
	if len(src.Sentences) < 1 {
		t.Fatal("no sentences")
	}
	links := src.Sentences[0].Links
	if len(links) != 3 {
		t.Fatalf("sentence 0 links: got %d, want 3", len(links))
	}

	// Assert deduplication: pragmatic appears exactly once (was direct+partial, kept direct).
	pragmaticCount := 0
	for _, link := range links {
		if link.Dimension == "traits" && link.Key == "pragmatic" {
			pragmaticCount++
			if link.Strength != store.LinkDirect {
				t.Fatalf("pragmatic link strength: got %s, want direct (upgraded from partial)", link.Strength)
			}
		}
	}
	if pragmaticCount != 1 {
		t.Fatalf("pragmatic appears %d times, want exactly 1 (deduped)", pragmaticCount)
	}

	// Assert same-n merge: concrete (from the second n:1 entry) is present.
	concreteFound := false
	for _, link := range links {
		if link.Dimension == "thinking" && link.Key == "concrete" {
			concreteFound = true
			if link.Strength != store.LinkDirect {
				t.Fatalf("concrete link strength: got %s, want direct", link.Strength)
			}
		}
	}
	if !concreteFound {
		t.Fatal("concrete link not found; same-n entries were not merged")
	}

	// Assert direct-first cap: the first link is direct, and concrete (a direct from the 4th
	// distinct key) is present. A truncate-then-sort bug would cap at [pragmatic(D), direct(P),
	// lazy(P)] and drop concrete.
	if links[0].Strength != store.LinkDirect {
		t.Fatalf("first link strength: got %s, want direct (direct-first sort)", links[0].Strength)
	}

	// Keywords: "out" (n:[99] only) is dropped; "mixed" (n:[1,99]) stores [0]; then w1-w8
	// (truncated to 8 total, so "mixed" and 7 of the w*).
	if len(src.Keywords) != 8 {
		t.Fatalf("keywords: got %d, want 8", len(src.Keywords))
	}
	for _, kw := range src.Keywords {
		if kw.Word == "out" {
			t.Fatal("keyword 'out' should have been dropped (all indexes out of range)")
		}
	}
	// Find "mixed" and assert it stored [0], dropping the 99.
	foundMixed := false
	for _, kw := range src.Keywords {
		if kw.Word == "mixed" {
			foundMixed = true
			if len(kw.Sentences) != 1 || kw.Sentences[0] != 0 {
				t.Fatalf("keyword 'mixed': got indexes %v, want [0] (dropped 99)", kw.Sentences)
			}
		}
	}
	if !foundMixed {
		t.Fatal("keyword 'mixed' should have been kept (has valid index 1)")
	}

	// The invalid "strong" is coerced to partial, not dropped and not promoted. It sits alone on
	// sentence 2: on sentence 1 the 3-link cap removed it, so neither outcome was observable.
	if len(src.Sentences) < 2 {
		t.Fatal("sentence 2 missing")
	}
	if got := src.Sentences[1].Links; len(got) != 1 || got[0].Key != "lazy" || got[0].Strength != store.LinkPartial {
		t.Fatalf("sentence 2 links = %+v, want lazy coerced to partial", got)
	}
}

// TestResolveSourcesSelfHeals verifies that:
// - First pass: one resolver call per stale portrait.
// - Second pass with nothing changed: zero calls.
// - After rewriting one profile's text: one call, for that profile only.
// - After deleting a profile file: its sources row is deleted and no call is made.
func TestResolveSourcesSelfHeals(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	// Write two profiles.
	if err := s.WriteProfile("default", "Stops at good enough."); err != nil {
		t.Fatalf("WriteProfile default: %v", err)
	}
	if err := s.WriteProfile("math", "Recovers with arithmetic."); err != nil {
		t.Fatalf("WriteProfile math: %v", err)
	}

	calls := 0
	fakeResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			calls++
			return `{"links":[],"keywords":[]}`, nil
		}
		return "", nil
	}

	sm := &Resolver{
		Store:      s,
		Config:     store.Config{},
		Lenses:     []*lens.Lens{{Name: "default"}, {Name: "math"}},
		LensPrompt: "RESOLVE",
		Run:        fakeResolve,
	}

	// First pass: one call per stale portrait (2).
	if _, err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources (pass 1): %v", err)
	}
	if calls != 2 {
		t.Fatalf("pass 1: got %d calls, want 2", calls)
	}

	// Second pass, nothing changed: zero calls.
	calls = 0
	if _, err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources (pass 2): %v", err)
	}
	if calls != 0 {
		t.Fatalf("pass 2: got %d calls, want 0", calls)
	}

	// Rewrite one profile's text: one call, for that profile only.
	if err := s.WriteProfile("default", "Satisfices on arrival."); err != nil {
		t.Fatalf("WriteProfile (rewrite): %v", err)
	}
	calls = 0
	if _, err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources (pass 3): %v", err)
	}
	if calls != 1 {
		t.Fatalf("pass 3: got %d calls, want 1", calls)
	}

	// Delete a profile file: its sources row is deleted and no call is made.
	if err := s.DeleteProfile("math"); err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}
	calls = 0
	if _, err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources (pass 4): %v", err)
	}
	if calls != 0 {
		t.Fatalf("pass 4: got %d calls, want 0", calls)
	}
	// The sources row for math is deleted.
	_, _, ok, err := s.ReadPortraitSources("math")
	if err != nil {
		t.Fatalf("ReadPortraitSources: %v", err)
	}
	if ok {
		t.Fatal("sources row for deleted profile still exists")
	}
}

// TestResolveSourcesUsesPerLensRunner verifies that ResolveSources dispatches each
// lens's resolver call to its own runner via RunFor, and uses ModelFor with PhaseReview.
func TestResolveSourcesUsesPerLensRunner(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	if err := s.WriteProfile("default", "Stops at good enough."); err != nil {
		t.Fatalf("WriteProfile default: %v", err)
	}
	if err := s.WriteProfile("math", "Recovers with arithmetic."); err != nil {
		t.Fatalf("WriteProfile math: %v", err)
	}

	type call struct {
		runner string
		model  string
	}
	calls := make(map[string]call) // lens -> {runner, model}

	defaultRunner := func(_ context.Context, model, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			calls["default"] = call{runner: "default-runner", model: model}
			return `{"links":[],"keywords":[]}`, nil
		}
		return "", nil
	}

	mathRunner := func(_ context.Context, model, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			calls["math"] = call{runner: "math-runner", model: model}
			return `{"links":[],"keywords":[]}`, nil
		}
		return "", nil
	}

	mathLens := &lens.Lens{Name: "math", Runner: "opencode", ReviewModel: "haiku"}
	sm := &Resolver{
		Store:      s,
		Config:     store.Config{Runner: "claude", DistillModel: "sonnet"},
		Lenses:     []*lens.Lens{{Name: "default"}, mathLens},
		LensPrompt: "RESOLVE",
		Run:        defaultRunner,
		RunFor: func(ln *lens.Lens) SummarizeFunc {
			if ln != nil && ln.Name == "math" {
				return mathRunner
			}
			return defaultRunner
		},
	}

	if _, err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources: %v", err)
	}

	// Each lens used its own runner.
	if calls["default"].runner != "default-runner" {
		t.Fatalf("default lens runner: got %s, want default-runner", calls["default"].runner)
	}
	if calls["math"].runner != "math-runner" {
		t.Fatalf("math lens runner: got %s, want math-runner", calls["math"].runner)
	}

	// Each lens used ModelFor(cfg, lens, PhaseReview).
	// default: no ReviewModel override → falls back to Config.DistillModel = "sonnet".
	if calls["default"].model != "sonnet" {
		t.Fatalf("default lens model: got %s, want sonnet", calls["default"].model)
	}
	// math: ReviewModel = "haiku" overrides the default.
	if calls["math"].model != "haiku" {
		t.Fatalf("math lens model: got %s, want haiku", calls["math"].model)
	}
}

// TestResolveSourcesOffWithoutPrompt verifies that with LensPrompt == "", zero
// calls are made, no rows are written, and ResolveSources returns nil.
func TestResolveSourcesOffWithoutPrompt(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	if err := s.WriteProfile("default", "Stops at good enough."); err != nil {
		t.Fatalf("WriteProfile: %v", err)
	}

	calls := 0
	fakeResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		calls++
		return `{"links":[],"keywords":[]}`, nil
	}

	sm := &Resolver{
		Store:  s,
		Config: store.Config{},
		Lenses: []*lens.Lens{{Name: "default"}},
		// LensPrompt is empty, feature is off.
		Run: fakeResolve,
	}

	if _, err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources: %v", err)
	}
	if calls != 0 {
		t.Fatalf("got %d calls, want 0", calls)
	}

	// No sources row.
	_, _, ok, err := s.ReadPortraitSources("default")
	if err != nil {
		t.Fatalf("ReadPortraitSources: %v", err)
	}
	if ok {
		t.Fatal("sources row was written despite LensPrompt == \"\"")
	}
}

// TestResolveUnifiedInheritsNotebookLinks verifies that the unified portrait inherits
// links from the notebook sentences it references. The unified fake reply refs "default#1"
// as direct. The unified sentence then carries default sentence 1's direct link, and
// Inherits["default"] equals default's text_hash.
func TestResolveUnifiedInheritsNotebookLinks(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	// Write notebook portraits and resolve them first.
	if err := s.WriteProfile("default", "Stops at good enough."); err != nil {
		t.Fatalf("WriteProfile default: %v", err)
	}
	if err := s.WriteProfile(store.ProfileUnified, "First unified sentence."); err != nil {
		t.Fatalf("WriteProfile unified: %v", err)
	}

	fakeResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			return `{"links":[{"n":1,"keys":[{"key":"traits/satisfices","strength":"direct"}]}],"keywords":[]}`, nil
		}
		if prompt == "RESOLVE_UNIFIED" {
			return `{"links":[{"n":1,"refs":[{"ref":"default#1","strength":"direct"}]}],"keywords":[]}`, nil
		}
		return "", nil
	}

	sm := &Resolver{
		Store:         s,
		Config:        store.Config{},
		Lenses:        []*lens.Lens{{Name: "default"}},
		LensPrompt:    "RESOLVE",
		UnifiedPrompt: "RESOLVE_UNIFIED",
		Run:           fakeResolve,
	}

	if _, err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources: %v", err)
	}

	// Read the unified portrait's sources.
	src, hash, ok, err := s.ReadPortraitSources(store.ProfileUnified)
	if err != nil {
		t.Fatalf("ReadPortraitSources unified: %v", err)
	}
	if !ok {
		t.Fatal("no unified sources row")
	}

	// The hash equals PortraitHash of the unified profile.
	wantHash := store.PortraitHash("First unified sentence.")
	if hash != wantHash {
		t.Fatalf("unified hash: got %s, want %s", hash, wantHash)
	}

	// The unified sentence inherits default sentence 1's link.
	if len(src.Sentences) < 1 || len(src.Sentences[0].Links) < 1 {
		t.Fatal("unified sentence 0 has no links")
	}
	link := src.Sentences[0].Links[0]
	if link.Lens != "default" || link.Dimension != "traits" || link.Key != "satisfices" || link.Strength != store.LinkDirect {
		t.Fatalf("unified sentence 0 link: got %+v, want {default traits satisfices direct}", link)
	}

	// Inherits["default"] equals default's text_hash.
	_, defaultHash, ok, err := s.ReadPortraitSources("default")
	if err != nil || !ok {
		t.Fatalf("ReadPortraitSources default: ok=%v err=%v", ok, err)
	}
	if src.Inherits == nil || src.Inherits["default"] != defaultHash {
		t.Fatalf("Inherits[default]: got %v, want %s", src.Inherits, defaultHash)
	}
}

// TestResolveUnifiedWeakensPartialRefs verifies that a partial ref turns a direct
// notebook link into partial.
func TestResolveUnifiedWeakensPartialRefs(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	if err := s.WriteProfile("default", "Stops at good enough."); err != nil {
		t.Fatalf("WriteProfile default: %v", err)
	}
	if err := s.WriteProfile(store.ProfileUnified, "First unified sentence."); err != nil {
		t.Fatalf("WriteProfile unified: %v", err)
	}

	fakeResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			// Notebook has a direct link.
			return `{"links":[{"n":1,"keys":[{"key":"traits/satisfices","strength":"direct"}]}],"keywords":[]}`, nil
		}
		if prompt == "RESOLVE_UNIFIED" {
			// Unified refs it as partial.
			return `{"links":[{"n":1,"refs":[{"ref":"default#1","strength":"partial"}]}],"keywords":[]}`, nil
		}
		return "", nil
	}

	sm := &Resolver{
		Store:         s,
		Config:        store.Config{},
		Lenses:        []*lens.Lens{{Name: "default"}},
		LensPrompt:    "RESOLVE",
		UnifiedPrompt: "RESOLVE_UNIFIED",
		Run:           fakeResolve,
	}

	if _, err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources: %v", err)
	}

	src, _, ok, err := s.ReadPortraitSources(store.ProfileUnified)
	if err != nil || !ok {
		t.Fatalf("ReadPortraitSources unified: ok=%v err=%v", ok, err)
	}

	// The inherited link is weakened to partial.
	if len(src.Sentences) < 1 || len(src.Sentences[0].Links) < 1 {
		t.Fatal("unified sentence 0 has no links")
	}
	link := src.Sentences[0].Links[0]
	if link.Strength != store.LinkPartial {
		t.Fatalf("unified link strength: got %s, want partial (weakened by partial ref)", link.Strength)
	}
}

// TestResolveUnifiedReResolvesWhenANotebookChanges verifies that when default's
// profile changes, unified becomes stale BECAUSE Inherits["default"] no longer equals
// default's stored text_hash. This test resolves all; rewrites default's profile;
// re-resolves ONLY default (by making the unified resolver fail in pass 2); then
// asserts unified is stale because the Inherits comparison detects the change.
func TestResolveUnifiedReResolvesWhenANotebookChanges(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	if err := s.WriteProfile("default", "Stops at good enough."); err != nil {
		t.Fatalf("WriteProfile default: %v", err)
	}
	if err := s.WriteProfile(store.ProfileUnified, "First unified sentence."); err != nil {
		t.Fatalf("WriteProfile unified: %v", err)
	}

	pass := 0
	fakeResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			return `{"links":[{"n":1,"keys":[{"key":"traits/satisfices","strength":"direct"}]}],"keywords":[]}`, nil
		}
		if prompt == "RESOLVE_UNIFIED" {
			pass++
			if pass == 1 {
				// Pass 1: succeed.
				return `{"links":[{"n":1,"refs":[{"ref":"default#1","strength":"direct"}]}],"keywords":[]}`, nil
			}
			// Pass 2: fail (so only the notebook path runs).
			return "", context.DeadlineExceeded
		}
		return "", nil
	}

	sm := &Resolver{
		Store:         s,
		Config:        store.Config{},
		Lenses:        []*lens.Lens{{Name: "default"}},
		LensPrompt:    "RESOLVE",
		UnifiedPrompt: "RESOLVE_UNIFIED",
		Run:           fakeResolve,
	}

	// Pass 1: resolve all (default + unified).
	if _, err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources (pass 1): %v", err)
	}

	// Capture the unified Inherits["default"] from pass 1.
	unifiedSrc1, _, ok, err := s.ReadPortraitSources(store.ProfileUnified)
	if err != nil || !ok {
		t.Fatalf("ReadPortraitSources unified (pass 1): ok=%v err=%v", ok, err)
	}
	inheritedHash1 := unifiedSrc1.Inherits["default"]

	// Rewrite default's text.
	if err := s.WriteProfile("default", "Satisfices on arrival."); err != nil {
		t.Fatalf("WriteProfile default (rewrite): %v", err)
	}

	// Pass 2: re-resolve ONLY default (unified resolver will fail).
	_, _ = sm.ResolveSources(context.Background()) // Ignore error (unified fails by design).

	// Read default's new stored text_hash.
	_, defaultHash2, ok, err := s.ReadPortraitSources("default")
	if err != nil || !ok {
		t.Fatalf("ReadPortraitSources default (pass 2): ok=%v err=%v", ok, err)
	}

	// Unified is now stale because Inherits["default"] (still inheritedHash1) differs
	// from default's new stored hash (defaultHash2).
	if !sm.isUnifiedStale() {
		t.Fatal("unified is not stale after default changed; expected stale due to Inherits mismatch")
	}

	// Assert the test would fail if the Inherits comparison were deleted:
	// If isUnifiedStale didn't check Inherits, it would return false (unified's own hash
	// didn't change, and default is no longer in sm.Lenses after the loop). This test
	// pins that the Inherits loop detects the change.
	if inheritedHash1 == defaultHash2 {
		t.Fatal("test precondition failed: default's hash didn't change after rewrite")
	}
}

// TestResolveUnifiedSkipsMissingNotebooks verifies that when math has no row
// (its resolver failed), unified still resolves. Refs to math#… are dropped,
// and Inherits has no math entry.
func TestResolveUnifiedSkipsMissingNotebooks(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	// Write default and math profiles, but only default will be resolved successfully.
	if err := s.WriteProfile("default", "Stops at good enough."); err != nil {
		t.Fatalf("WriteProfile default: %v", err)
	}
	if err := s.WriteProfile("math", "Recovers with arithmetic."); err != nil {
		t.Fatalf("WriteProfile math: %v", err)
	}
	if err := s.WriteProfile(store.ProfileUnified, "First unified sentence."); err != nil {
		t.Fatalf("WriteProfile unified: %v", err)
	}

	fakeResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			// Fail for math (return error).
			return "", context.DeadlineExceeded
		}
		if prompt == "RESOLVE_UNIFIED" {
			// Unified refs both default#1 and math#1.
			return `{"links":[{"n":1,"refs":[{"ref":"default#1","strength":"direct"},{"ref":"math#1","strength":"direct"}]}],"keywords":[]}`, nil
		}
		return "", nil
	}

	// Override Run to succeed for default only.
	successfulResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			return `{"links":[{"n":1,"keys":[{"key":"traits/satisfices","strength":"direct"}]}],"keywords":[]}`, nil
		}
		if prompt == "RESOLVE_UNIFIED" {
			return `{"links":[{"n":1,"refs":[{"ref":"default#1","strength":"direct"},{"ref":"math#1","strength":"direct"}]}],"keywords":[]}`, nil
		}
		return "", nil
	}

	sm := &Resolver{
		Store:         s,
		Config:        store.Config{},
		Lenses:        []*lens.Lens{{Name: "default"}, {Name: "math"}},
		LensPrompt:    "RESOLVE",
		UnifiedPrompt: "RESOLVE_UNIFIED",
		Run:           successfulResolve,
		RunFor: func(ln *lens.Lens) SummarizeFunc {
			if ln != nil && ln.Name == "math" {
				return fakeResolve // Fails for math.
			}
			return successfulResolve
		},
	}

	// ResolveSources should return an error for math's failure.
	_, err := sm.ResolveSources(context.Background())
	if err == nil {
		t.Fatal("ResolveSources returned nil; expected a joined error for math's failure")
	}

	// Unified sources should exist.
	src, _, ok, err := s.ReadPortraitSources(store.ProfileUnified)
	if err != nil {
		t.Fatalf("ReadPortraitSources unified: %v", err)
	}
	if !ok {
		t.Fatal("unified sources row was not written despite default succeeding")
	}

	// The unified sentence has only default's link (math#1 was dropped).
	if len(src.Sentences) < 1 || len(src.Sentences[0].Links) < 1 {
		t.Fatal("unified sentence 0 has no links")
	}
	for _, link := range src.Sentences[0].Links {
		if link.Lens == "math" {
			t.Fatal("unified sentence has a math link; should have been dropped (no math row)")
		}
	}

	// Inherits has no math entry.
	if _, hasMath := src.Inherits["math"]; hasMath {
		t.Fatal("Inherits has a math entry; should be absent (no math row)")
	}
	// Inherits["default"] is present.
	if _, hasDefault := src.Inherits["default"]; !hasDefault {
		t.Fatal("Inherits missing default entry")
	}
}

// TestResolveUnifiedLoopGuard verifies that when a notebook's resolver always fails,
// unified is called only once in the first pass and then never again (ruling R9 prevents
// it from being stale when nothing changes).
func TestResolveUnifiedLoopGuard(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	// Write profiles.
	if err := s.WriteProfile("default", "Stops at good enough."); err != nil {
		t.Fatalf("WriteProfile default: %v", err)
	}
	if err := s.WriteProfile("math", "Recovers with arithmetic."); err != nil {
		t.Fatalf("WriteProfile math: %v", err)
	}
	if err := s.WriteProfile(store.ProfileUnified, "First unified sentence."); err != nil {
		t.Fatalf("WriteProfile unified: %v", err)
	}

	unifiedCalls := 0
	successfulResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			return `{"links":[{"n":1,"keys":[{"key":"traits/satisfices","strength":"direct"}]}],"keywords":[]}`, nil
		}
		if prompt == "RESOLVE_UNIFIED" {
			unifiedCalls++
			return `{"links":[{"n":1,"refs":[{"ref":"default#1","strength":"direct"}]}],"keywords":[]}`, nil
		}
		return "", nil
	}

	mathFailsResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			// Math always fails.
			return "", context.DeadlineExceeded
		}
		return "", nil
	}

	sm := &Resolver{
		Store:         s,
		Config:        store.Config{},
		Lenses:        []*lens.Lens{{Name: "default"}, {Name: "math"}},
		LensPrompt:    "RESOLVE",
		UnifiedPrompt: "RESOLVE_UNIFIED",
		Run:           successfulResolve,
		RunFor: func(ln *lens.Lens) SummarizeFunc {
			if ln != nil && ln.Name == "math" {
				return mathFailsResolve
			}
			return successfulResolve
		},
	}

	// Pass 1: resolve default + unified (math fails).
	_, _ = sm.ResolveSources(context.Background()) // Ignore error (math fails).

	if unifiedCalls != 1 {
		t.Fatalf("pass 1: unified calls = %d, want 1", unifiedCalls)
	}

	// Pass 2: nothing changed, math still fails. Unified should NOT be called.
	_, _ = sm.ResolveSources(context.Background())

	if unifiedCalls != 1 {
		t.Fatalf("pass 2: unified calls = %d, want 1 (no re-call when nothing changed)", unifiedCalls)
	}

	// Pass 3: still nothing changed. Unified should still NOT be called.
	_, _ = sm.ResolveSources(context.Background())

	if unifiedCalls != 1 {
		t.Fatalf("pass 3: unified calls = %d, want 1 (no re-call when nothing changed)", unifiedCalls)
	}
}

// TestResolveUnifiedSkipsWhenAllNotebookResolversFail verifies that when all notebook
// resolvers fail, the unified resolver is never called (there are no current notebook rows).
func TestResolveUnifiedSkipsWhenAllNotebookResolversFail(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	// Write notebook and unified profiles.
	if err := s.WriteProfile("default", "Stops at good enough."); err != nil {
		t.Fatalf("WriteProfile default: %v", err)
	}
	if err := s.WriteProfile("math", "Recovers with arithmetic."); err != nil {
		t.Fatalf("WriteProfile math: %v", err)
	}
	if err := s.WriteProfile(store.ProfileUnified, "First unified sentence."); err != nil {
		t.Fatalf("WriteProfile unified: %v", err)
	}

	unifiedCalls := 0
	failingResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			// All notebook resolvers fail.
			return "", context.DeadlineExceeded
		}
		if prompt == "RESOLVE_UNIFIED" {
			unifiedCalls++
			return `{"links":[],"keywords":[]}`, nil
		}
		return "", nil
	}

	sm := &Resolver{
		Store:         s,
		Config:        store.Config{},
		Lenses:        []*lens.Lens{{Name: "default"}, {Name: "math"}},
		LensPrompt:    "RESOLVE",
		UnifiedPrompt: "RESOLVE_UNIFIED",
		Run:           failingResolve,
	}

	// Run ResolveSources: both notebook resolvers fail, so no notebook sources rows exist.
	// Unified should NOT be called (no current notebook rows).
	_, _ = sm.ResolveSources(context.Background()) // Ignore error (notebooks fail).

	if unifiedCalls != 0 {
		t.Fatalf("unified calls = %d, want 0 (all notebook resolvers failed)", unifiedCalls)
	}

	// An empty unified sources row IS written (the guard writes it without calling the model).
	src, _, ok, err := s.ReadPortraitSources(store.ProfileUnified)
	if err != nil {
		t.Fatalf("ReadPortraitSources unified: %v", err)
	}
	if !ok {
		t.Fatal("unified sources row was not written (expected an empty row from the guard)")
	}

	// The row is empty (no sentences with links, no keywords, no Inherits).
	if len(src.Sentences) > 0 && len(src.Sentences[0].Links) > 0 {
		t.Fatal("unified row has links; expected empty")
	}
	if len(src.Keywords) > 0 {
		t.Fatal("unified row has keywords; expected empty")
	}
	if len(src.Inherits) > 0 {
		t.Fatal("unified row has Inherits; expected empty")
	}
}

// F095: the resolver also sends every facet's full text in one call. A failure names that size
// and the lens's review_model as the lever, in the error the worker logs; the unified resolver,
// which runs on the default model, names the default review_model.
func TestResolveSourcesFailureNamesInputSizeAndLever(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)
	for name, md := range map[string]string{"default": "Stops at good enough.", store.ProfileUnified: "Whole."} {
		if err := s.WriteProfile(name, md); err != nil {
			t.Fatal(err)
		}
	}
	const lever = "; if that is more than the model's context window, use a larger-context model: witness config set review_model <model>"
	sm := &Resolver{Store: s, Lenses: []*lens.Lens{{Name: "default"}}, LensPrompt: "RESOLVE", UnifiedPrompt: "RESOLVE_UNIFIED"}

	var sent string
	sm.Run = func(_ context.Context, _, _, input string) (string, error) {
		sent = input
		return "", context.DeadlineExceeded
	}
	_, err := sm.ResolveSources(context.Background())
	if want := "(input " + InputSize(len([]rune(sent))) + lever + " --lens default)"; err == nil || sent == "" || !strings.Contains(err.Error(), want) {
		t.Fatalf("want the resolver error to carry %q, got %v", want, err)
	}

	// The notebook now resolves, so the unified resolver runs, and fails.
	sm.Run = func(_ context.Context, _, prompt, input string) (string, error) {
		if prompt == "RESOLVE" {
			return `{"links":[{"n":1,"keys":[{"key":"traits/satisfices","strength":"direct"}]}]}`, nil
		}
		sent = input
		return "", context.DeadlineExceeded
	}
	_, err = sm.ResolveSources(context.Background())
	if want := "resolve unified: resolver call: context deadline exceeded (input " + InputSize(len([]rune(sent))) + lever + ")"; err == nil || err.Error() != want {
		t.Fatalf("want the unified resolver error %q, got %v", want, err)
	}
}
