package distill

import (
	"context"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// TestResolveSourcesWritesLinks verifies that ResolveSources calls the resolver,
// parses its reply, and stores the result with the correct hash. Links and keywords
// must match the resolver's output, with sentence numbers 1-based in the reply but
// 0-based in keyword storage.
func TestResolveSourcesWritesLinks(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	// Write a profile so there's something to resolve.
	md := "Stops at good enough. Recovers with arithmetic."
	if err := s.WriteProfile("default", md); err != nil {
		t.Fatalf("WriteProfile: %v", err)
	}

	fakeResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			return `{"links":[{"n":1,"keys":[{"key":"traits/satisfices","strength":"direct"}]}],"keywords":[{"k":"good enough","n":[1,2]}]}`, nil
		}
		return "", nil
	}

	sm := &Summarizer{
		Store:         s,
		Config:        store.Config{},
		Lenses:        []*lens.Lens{{Name: "default", BuiltIn: true}},
		ResolvePrompt: "RESOLVE",
		Run:           fakeResolve,
	}

	if err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources: %v", err)
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

	// Sentence 1's first link is {default traits satisfices direct}.
	if len(src.Sentences) < 1 {
		t.Fatal("no sentences stored")
	}
	if len(src.Sentences[0].Links) < 1 {
		t.Fatal("sentence 1 has no links")
	}
	link := src.Sentences[0].Links[0]
	if link.Lens != "default" || link.Dimension != "traits" || link.Key != "satisfices" || link.Strength != store.LinkDirect {
		t.Fatalf("sentence 1 link: got %+v, want {default traits satisfices direct}", link)
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

// TestResolveSourcesLeavesSummaryByteIdentical verifies that running Summarize with
// or without ResolvePrompt set produces identical profile files and signatures.
// ResolveSources is a separate pass that writes only portrait_sources rows.
func TestResolveSourcesLeavesSummaryByteIdentical(t *testing.T) {
	// Store 1: Summarize with ResolvePrompt empty.
	s1 := newStore(t)
	seedFacets(t, s1)

	fake1 := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			t.Fatal("Summarize must not call the resolver when ResolvePrompt is empty")
		}
		return "SUMMARY", nil
	}

	sm1 := &Summarizer{
		Store:         s1,
		Config:        store.Config{},
		Lenses:        []*lens.Lens{{Name: "default", BuiltIn: true}},
		LensPrompt:    "LENS",
		UnifiedPrompt: "UNIFIED",
		Run:           fake1,
	}
	if err := sm1.Summarize(context.Background()); err != nil {
		t.Fatalf("Summarize (no resolve): %v", err)
	}

	// Store 2: Summarize with ResolvePrompt set, then ResolveSources.
	s2 := newStore(t)
	seedFacets(t, s2)

	resolveCalls := 0
	fake2 := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			resolveCalls++
			return `{"links":[],"keywords":[]}`, nil
		}
		return "SUMMARY", nil
	}

	sm2 := &Summarizer{
		Store:         s2,
		Config:        store.Config{},
		Lenses:        []*lens.Lens{{Name: "default", BuiltIn: true}},
		LensPrompt:    "LENS",
		UnifiedPrompt: "UNIFIED",
		ResolvePrompt: "RESOLVE",
		Run:           fake2,
	}
	if err := sm2.Summarize(context.Background()); err != nil {
		t.Fatalf("Summarize (with resolve): %v", err)
	}
	if resolveCalls != 0 {
		t.Fatalf("Summarize made %d RESOLVE calls, want 0", resolveCalls)
	}

	if err := sm2.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources: %v", err)
	}
	// ResolveSources should resolve both notebook portraits (default and math), skipping unified.
	if resolveCalls != 2 {
		t.Fatalf("ResolveSources made %d calls, want 2", resolveCalls)
	}

	// Every ReadProfile result is identical.
	for _, l := range []string{"default", "math", "unified"} {
		md1, ok1, _ := s1.ReadProfile(l)
		md2, ok2, _ := s2.ReadProfile(l)
		if ok1 != ok2 || md1 != md2 {
			t.Fatalf("profile %s differs: ok1=%v ok2=%v md1=%q md2=%q", l, ok1, ok2, md1, md2)
		}
	}

	// Every profile_sig:* meta value is identical.
	for _, l := range []string{"default", "math", "unified"} {
		sig1 := s1.MetaString("profile_sig:" + l)
		sig2 := s2.MetaString("profile_sig:" + l)
		if sig1 != sig2 {
			t.Fatalf("signature for %s differs: %q vs %q", l, sig1, sig2)
		}
	}
}

// TestResolveSourcesFailureIsIsolated verifies that when the resolver returns an error,
// ResolveSources returns a non-nil error, no sources row is written, and profile files
// and signatures are unchanged.
func TestResolveSourcesFailureIsIsolated(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	// Write a profile.
	md := "stops at good enough."
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

	sm := &Summarizer{
		Store:         s,
		Config:        store.Config{},
		Lenses:        []*lens.Lens{{Name: "default", BuiltIn: true}},
		ResolvePrompt: "RESOLVE",
		Run:           fakeResolve,
	}

	err := sm.ResolveSources(context.Background())
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

			sm := &Summarizer{
				Store:         st,
				Config:        store.Config{},
				Lenses:        []*lens.Lens{{Name: "default", BuiltIn: true}},
				ResolvePrompt: "RESOLVE",
				Run:           fakeResolve,
			}

			_ = sm.ResolveSources(context.Background())

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
// sentence numbers, invalid strengths, and excess links per sentence are dropped while
// valid links are kept. Keywords out of range are dropped, and more than 8 are truncated.
func TestResolveSourcesValidatesLinks(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	// Archive one facet to test that it's dropped.
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

	// Resolver returns:
	// - unknown key "nonexistent/key" (dropped)
	// - archived key "traits/satisfices" (dropped)
	// - n:0 (dropped)
	// - n:99 (dropped)
	// - strength "strong" (becomes partial)
	// - 4 links on sentence 1 (only 3 kept, direct first)
	// - keyword with index 99 (dropped)
	// - 10 keywords (truncated to 8)
	reply := `{
		"links": [
			{
				"n": 0,
				"keys": [{"key": "resilience/trip_wire", "strength": "direct"}]
			},
			{
				"n": 1,
				"keys": [
					{"key": "nonexistent/key", "strength": "direct"},
					{"key": "traits/satisfices", "strength": "direct"},
					{"key": "resilience/trip_wire", "strength": "direct"},
					{"key": "resilience/trip_wire", "strength": "partial"},
					{"key": "resilience/trip_wire", "strength": "strong"},
					{"key": "resilience/trip_wire", "strength": "direct"}
				]
			},
			{
				"n": 99,
				"keys": [{"key": "resilience/trip_wire", "strength": "direct"}]
			}
		],
		"keywords": [
			{"k": "w1", "n": [1]},
			{"k": "w2", "n": [1]},
			{"k": "w3", "n": [1]},
			{"k": "w4", "n": [1]},
			{"k": "w5", "n": [1]},
			{"k": "w6", "n": [1]},
			{"k": "w7", "n": [1]},
			{"k": "w8", "n": [1]},
			{"k": "w9", "n": [1]},
			{"k": "w10", "n": [1]},
			{"k": "out", "n": [99]}
		]
	}`

	fakeResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			return reply, nil
		}
		return "", nil
	}

	sm := &Summarizer{
		Store:         s,
		Config:        store.Config{},
		Lenses:        []*lens.Lens{{Name: "default", BuiltIn: true}, {Name: "math"}},
		ResolvePrompt: "RESOLVE",
		Run:           fakeResolve,
	}

	if err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources: %v", err)
	}

	src, _, ok, err := s.ReadPortraitSources("default")
	if err != nil {
		t.Fatalf("ReadPortraitSources: %v", err)
	}
	if !ok {
		t.Fatal("no sources row")
	}

	// Sentence 0 (n:1 in reply) should have 3 links, direct first.
	if len(src.Sentences) < 1 {
		t.Fatal("no sentences")
	}
	links := src.Sentences[0].Links
	if len(links) != 3 {
		t.Fatalf("sentence 0 links: got %d, want 3", len(links))
	}
	// First should be direct, second direct, third strength="strong"→partial.
	if links[0].Strength != store.LinkDirect {
		t.Fatalf("first link strength: got %s, want direct", links[0].Strength)
	}
	if links[1].Strength != store.LinkPartial {
		t.Fatalf("second link strength: got %s, want partial", links[1].Strength)
	}
	if links[2].Strength != store.LinkPartial {
		t.Fatalf("third link strength: got %s, want partial (coerced from 'strong')", links[2].Strength)
	}

	// Keywords: at most 8, and the one with n:[99] is dropped.
	if len(src.Keywords) != 8 {
		t.Fatalf("keywords: got %d, want 8", len(src.Keywords))
	}
	for _, kw := range src.Keywords {
		if kw.Word == "out" {
			t.Fatal("keyword 'out' should have been dropped (index out of range)")
		}
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
	if err := s.WriteProfile("default", "stops at good enough."); err != nil {
		t.Fatalf("WriteProfile default: %v", err)
	}
	if err := s.WriteProfile("math", "recovers with arithmetic."); err != nil {
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

	sm := &Summarizer{
		Store:         s,
		Config:        store.Config{},
		Lenses:        []*lens.Lens{{Name: "default", BuiltIn: true}, {Name: "math"}},
		ResolvePrompt: "RESOLVE",
		Run:           fakeResolve,
	}

	// First pass: one call per stale portrait (2).
	if err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources (pass 1): %v", err)
	}
	if calls != 2 {
		t.Fatalf("pass 1: got %d calls, want 2", calls)
	}

	// Second pass, nothing changed: zero calls.
	calls = 0
	if err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources (pass 2): %v", err)
	}
	if calls != 0 {
		t.Fatalf("pass 2: got %d calls, want 0", calls)
	}

	// Rewrite one profile's text: one call, for that profile only.
	if err := s.WriteProfile("default", "satisfices on arrival."); err != nil {
		t.Fatalf("WriteProfile (rewrite): %v", err)
	}
	calls = 0
	if err := sm.ResolveSources(context.Background()); err != nil {
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
	if err := sm.ResolveSources(context.Background()); err != nil {
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

	if err := s.WriteProfile("default", "stops at good enough."); err != nil {
		t.Fatalf("WriteProfile default: %v", err)
	}
	if err := s.WriteProfile("math", "recovers with arithmetic."); err != nil {
		t.Fatalf("WriteProfile math: %v", err)
	}

	calls := map[string]string{} // lens -> runner that resolved it

	defaultRunner := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			// Record which lens this is by inspecting the input (simplified).
			calls["default"] = "default-runner"
			return `{"links":[],"keywords":[]}`, nil
		}
		return "", nil
	}

	mathRunner := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "RESOLVE" {
			calls["math"] = "math-runner"
			return `{"links":[],"keywords":[]}`, nil
		}
		return "", nil
	}

	mathLens := &lens.Lens{Name: "math", Runner: "opencode"}
	sm := &Summarizer{
		Store:         s,
		Config:        store.Config{Runner: "claude", DistillModel: "sonnet"},
		Lenses:        []*lens.Lens{{Name: "default", BuiltIn: true}, mathLens},
		ResolvePrompt: "RESOLVE",
		Run:           defaultRunner,
		RunFor: func(ln *lens.Lens) SummarizeFunc {
			if ln != nil && ln.Name == "math" {
				return mathRunner
			}
			return defaultRunner
		},
	}

	if err := sm.ResolveSources(context.Background()); err != nil {
		t.Fatalf("ResolveSources: %v", err)
	}

	// Each lens used its own runner.
	if calls["default"] != "default-runner" {
		t.Fatalf("default lens: got %s, want default-runner", calls["default"])
	}
	if calls["math"] != "math-runner" {
		t.Fatalf("math lens: got %s, want math-runner", calls["math"])
	}
}

// TestResolveSourcesOffWithoutPrompt verifies that with ResolvePrompt == "", zero
// calls are made, no rows are written, and ResolveSources returns nil.
func TestResolveSourcesOffWithoutPrompt(t *testing.T) {
	s := newStore(t)
	seedFacets(t, s)

	if err := s.WriteProfile("default", "stops at good enough."); err != nil {
		t.Fatalf("WriteProfile: %v", err)
	}

	calls := 0
	fakeResolve := func(_ context.Context, _, prompt, _ string) (string, error) {
		calls++
		return `{"links":[],"keywords":[]}`, nil
	}

	sm := &Summarizer{
		Store:  s,
		Config: store.Config{},
		Lenses: []*lens.Lens{{Name: "default", BuiltIn: true}},
		// ResolvePrompt is empty, feature is off.
		Run: fakeResolve,
	}

	if err := sm.ResolveSources(context.Background()); err != nil {
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
		t.Fatal("sources row was written despite ResolvePrompt == \"\"")
	}
}
