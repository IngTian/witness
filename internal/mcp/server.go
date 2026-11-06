// Package mcp exposes the witness archive over the Model Context Protocol so any
// agent can read it (search_observations) and write decision-aware observations in-session
// (record_observation). Pure-Go SDK; stdio transport.
package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/IngTian/witness/internal/store"
	"github.com/IngTian/witness/internal/vector"
)

// searchInput is the typed argument for search_observations.
type searchInput struct {
	Query string `json:"query" jsonschema:"the growth/pattern question to search for"`
	Lens  string `json:"lens,omitempty" jsonschema:"which lens to search, by the name 'witness lens list' shows (e.g. 'math'); omit for 'default'"`
	K     int    `json:"k,omitempty" jsonschema:"max results (default 8)"`
}

// getFacetsInput is the typed argument for get_facets (structured L2 facets).
type getFacetsInput struct {
	Lens string `json:"lens,omitempty" jsonschema:"which lens to read, by the name 'witness lens list' shows (e.g. 'math'); omit for 'default'"`
}

// getProfileInput is the typed argument for get_profile (the L4 narrative).
// NOTE the asymmetry from search/facets above: those default to the 'default'
// LENS, but the profile defaults to 'unified' — the cross-lens aggregate VIEW that
// blends every active lens (default + math + …), which is NOT itself a lens. Pass
// 'default' to read the default lens's own narrative instead of the blend.
type getProfileInput struct {
	Lens string `json:"lens,omitempty" jsonschema:"omit for 'unified' (the cross-lens portrait blending ALL lenses — an aggregate view, not a lens); or name one lens: 'default' (that lens's own narrative), 'math', etc."`
}

// deleteInput is the typed argument for delete_observation.
type deleteInput struct {
	ObsID string `json:"obs_id" jsonschema:"the obs_id to delete (get ids from search_observations)"`
}

// recordInput is the typed argument for record_observation.
type recordInput struct {
	Session     string `json:"session" jsonschema:"the current session_id"`
	Lens        string `json:"lens,omitempty" jsonschema:"lens tag (default 'default')"`
	Dimension   string `json:"dimension" jsonschema:"which axis this observation belongs to"`
	Observation string `json:"observation" jsonschema:"the noticed growth/change, one sentence"`
	Evidence    string `json:"evidence,omitempty" jsonschema:"short anchor to what happened"`
	Poignancy   int    `json:"poignancy,omitempty" jsonschema:"1-10 salience (default 5)"`
}

// Bounds on record_observation input. record_observation is unauthenticated
// (any MCP client / in-session agent can call it), so its input is untrusted:
// cap text sizes and quantity to bound disk growth, and clamp poignancy so one
// call can't force an expensive review by passing an absurd salience.
const (
	maxObsLen           = 2000
	maxEvidenceLen      = 2000
	maxDimensionLen     = 100
	maxPoignancy        = 10
	maxStagedPerSession = 200
	// maxStagedTotal bounds the staged buffer across ALL sessions. Generous relative to the
	// per-session cap (many concurrent sessions are legitimate) but finite, because the
	// per-session cap is keyed on an agent-supplied session id and so is evadable on its own.
	maxStagedTotal = 5000
	// maxLensLen bounds the lens name. It was the ONE recordInput field neither trimmed nor
	// length-checked, so `{"lens": "default "}` — exactly the trailing-whitespace noise the other
	// four fields are trimmed for — was stored as a DISTINCT lens from "default": the write
	// reported success naming the lens, but the observation landed under a lens no drain, review,
	// or `witness facets` call would ever look at. (There is no filesystem escape: profileFileName
	// rejects separators and "..", so that half of the lead is wrong.)
	maxLensLen = 100
	// maxQueryLen bounds a search query before it reaches the tokenizer. The tokenizer allocates
	// per-byte alignment tables and a per-byte ChangeMap, so a multi-megabyte query costs on the
	// order of a gigabyte per megabyte of input inside the LONG-LIVED MCP server process — a
	// confused agent pasting a whole file into search_observations could OOM the server that
	// serves every other tool. 8000 chars is far above any real semantic query (the corpus is
	// observation-sized text) and far below the danger zone.
	maxQueryLen = 8000
)

// normalizeRecord validates and clamps record_observation input. Pure (no IO) so
// it is unit-testable; the quantity bound (StagedCount) is enforced in the handler.
func normalizeRecord(in recordInput) (recordInput, error) {
	in.Session = strings.TrimSpace(in.Session)
	in.Observation = strings.TrimSpace(in.Observation)
	in.Dimension = strings.TrimSpace(in.Dimension)
	in.Evidence = strings.TrimSpace(in.Evidence)
	if in.Session == "" || in.Observation == "" {
		return in, fmt.Errorf("session and observation are required")
	}
	if len(in.Observation) > maxObsLen {
		return in, fmt.Errorf("observation too long (%d > %d chars)", len(in.Observation), maxObsLen)
	}
	if len(in.Evidence) > maxEvidenceLen {
		return in, fmt.Errorf("evidence too long (%d > %d chars)", len(in.Evidence), maxEvidenceLen)
	}
	if len(in.Dimension) > maxDimensionLen {
		return in, fmt.Errorf("dimension too long (%d > %d chars)", len(in.Dimension), maxDimensionLen)
	}
	// Lens gets the SAME trim + bound as every other field. It was the one that had neither, so
	// `{"lens": "default "}` (trailing whitespace — precisely the noise the others are trimmed
	// for) became a DISTINCT lens: the call reported `recorded (default /...)` while the
	// observation landed under a lens name no drain, review, or `witness facets` will ever query.
	in.Lens = strings.TrimSpace(in.Lens)
	if len(in.Lens) > maxLensLen {
		return in, fmt.Errorf("lens too long (%d > %d chars)", len(in.Lens), maxLensLen)
	}
	if strings.ContainsAny(in.Lens, "/\\") || strings.Contains(in.Lens, "..") {
		// Defense in depth. profileFileName already rejects these, so there is no known escape
		// — but a lens name reaches a FILE PATH there, and this is agent-supplied input, so
		// refusing it at the boundary means a future writer cannot inherit a hole.
		return in, fmt.Errorf("invalid lens name %q", in.Lens)
	}
	if in.Lens == "" {
		in.Lens = store.LensDefault
	}
	switch {
	case in.Poignancy < 1:
		in.Poignancy = 5 // default
	case in.Poignancy > maxPoignancy:
		in.Poignancy = maxPoignancy // clamp; never let an absurd value force a review
	}
	return in, nil
}

// Embedder is the slice of the embedder the MCP tools need (search_observations
// embeds the query). An interface so tests can drive the server over an in-memory
// transport without loading the embedding model. *embed.Embedder satisfies it.
type Embedder interface {
	Embed(text string) ([]float32, error)
}

// Serve runs the MCP stdio server until the context is cancelled.
func Serve(ctx context.Context, st store.MCPStore, emb Embedder, opts Options) error {
	return newServer(st, emb, opts).Run(ctx, &mcpsdk.StdioTransport{})
}

// Options is what cmd knows and this leaf package must not import: the ldflags-injected version,
// a model runner, the lens registry, the install targets. Every zero value works, so the
// fake-driven tests set only what they exercise.
type Options struct {
	// Version is reported in the MCP initialize handshake; "" → "dev".
	Version string
	// Refresh regenerates stale profiles before get_profile reads one; nil serves what is on disk.
	Refresh RefreshProfiles
	// DataName maps the lens name an agent passes — the registered name `witness lens list` shows
	// and every command takes — to the name that lens's rows are tagged with (its lens.json
	// `name`; the shipped corpus-example mines as "regime"). nil uses names as given.
	DataName func(lens string) string
	// Wire is the copyable "connect an editor" command, built from the install targets so it
	// names every editor `witness wire` accepts.
	Wire string
}

// RefreshProfiles regenerates stale L4 summaries before get_profile reads one (issue #100,
// read-time generation). nil disables it — the tool then serves whatever is on disk, which is
// exactly the pre-#100 behavior.
//
// It is a CALLBACK rather than a widening of store.MCPStore on purpose. Regenerating needs a
// model runner, the lens registry, and the machine-wide WorkerLock — none of which belong in a
// store interface, and all of which live in cmd. Injecting one function keeps this package's
// dependency surface as it was (a read interface plus an embedder) and keeps the fake-driven
// tests working unchanged with nil.
//
// The implementation is expected to be non-fatal and lock-guarded: it must serve cached rather
// than queue behind a running drain (see cmd/commands/profilepull.go). An error here is
// reported to the agent alongside the cached profile, never in place of it.
type RefreshProfiles func() error

// newServer builds the MCP server with all tools registered. Split from Serve so
// tests can drive it over an in-memory transport.
//
// Every tool's handler returns `any` (nil) as its structured-output value, NOT a
// typed struct. The go-sdk derives an output schema from a non-`any` return type
// and then emits `structuredContent` on every result (server.go:307). A spec-
// compliant client (Claude Code) reads that structuredContent and IGNORES the
// text Content — so a typed-but-empty return made every tool look like it
// returned `{}` even though the real payload was in Content. Returning `any`/nil
// suppresses the output schema so clients use Content. (See TestNoStructuredOutput.)
func newServer(st store.MCPStore, emb Embedder, opts Options) *mcpsdk.Server {
	version, refresh := opts.Version, opts.Refresh
	if strings.TrimSpace(version) == "" {
		version = "dev"
	}
	dataName := opts.DataName
	if dataName == nil {
		dataName = func(lens string) string { return lens }
	}
	wire := opts.Wire
	if wire == "" {
		wire = "witness wire <editor>"
	}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "witness",
		Version: version,
	}, nil)

	// search_observations: read-time recall, tag-filtered, default lens.
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "search_observations",
		Description: "Search the user's growth archive for observations related to a query. " +
			"Returns past observations about how the user thinks/works/changes. " +
			"Defaults to the 'default' lens; pass lens='math' etc. for another lens.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in searchInput) (*mcpsdk.CallToolResult, any, error) {
		lens := in.Lens
		if lens == "" {
			lens = store.LensDefault
		}
		data := dataName(lens)
		k := in.K
		if k <= 0 {
			k = 8
		}
		// Bound the query BEFORE the expensive work. It went straight to the tokenizer, which
		// allocates per-byte alignment tables plus a per-byte ChangeMap — on the order of a
		// gigabyte per megabyte of input, inside the LONG-LIVED MCP server process that serves
		// every other tool. A confused agent pasting a whole file into search_observations could
		// therefore OOM the server rather than just get a bad answer. Refusing early also skips
		// the ReadObservations below, so an oversized call costs nothing.
		query := strings.TrimSpace(in.Query)
		if query == "" {
			return errResult("query is required"), nil, nil
		}
		if len(query) > maxQueryLen {
			return errResult(fmt.Sprintf("query too long (%d > %d chars); search takes a short "+
				"semantic phrase, not a document", len(query), maxQueryLen)), nil, nil
		}
		// One lens, not the whole corpus filtered in Go: this runs per query in a long-lived
		// process, and every lens's decoded embeddings cost ~5x the memory (889 MB vs 177 MB at
		// 150k observations over 5 lenses). Search keeps the same lens filter, so hits are identical.
		obs, err := st.ReadObservations(data)
		if err != nil {
			return errResult(fmt.Sprintf("read archive: %v", err)), nil, nil
		}
		qv, err := emb.Embed(query)
		if err != nil {
			return errResult(fmt.Sprintf("embed query: %v", err)), nil, nil
		}
		hits := vector.Search(obs, qv, data, k)
		return textResult(renderHits(lens, hits, func() string { return emptyHint(st, lens, data, wire, true) })), nil, nil
	})

	// record_observation: active in-session write. Stages to the session buffer;
	// the session-end worker passes it through verbatim (authoritative).
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "record_observation",
		Description: "Record a decision-aware observation about the user's growth/change in THIS session. " +
			"Use when you (the assistant) notice something worth tracking that you have context for now " +
			"but a later reviewer would miss. Stored verbatim. Tag with the relevant lens.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in recordInput) (*mcpsdk.CallToolResult, any, error) {
		in, err := normalizeRecord(in)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		// Staged under the lens's MINED name: the row drains into L1 verbatim and the review reads
		// the lens by that name, so the agent's (registered) name would orphan it — after
		// reporting success.
		data := dataName(in.Lens)
		o := store.Observation{
			ID:          store.ObservationID(in.Session, data, in.Observation),
			TS:          time.Now().UTC().Format(time.RFC3339),
			Session:     in.Session,
			Lens:        data,
			Dimension:   in.Dimension,
			Observation: in.Observation,
			Evidence:    in.Evidence,
			Poignancy:   in.Poignancy,
			Source:      "active",
		}
		// Two bounds, both enforced atomically in the store so concurrent MCP processes can't
		// each pass a check and race past them:
		//   - per SESSION, the original cap on disk growth from a runaway loop;
		//   - TOTAL, because the session id is AGENT-SUPPLIED. A confused or runaway agent that
		//     varies it (per-turn ids, a hallucinated id, an incrementing counter) gets a fresh
		//     per-session budget on every call, so the per-session cap alone bounds nothing —
		//     and each row is up to 2000 chars of observation plus 2000 of evidence, durable in
		//     L1 and fed to the review model. A total cap has no key, so it cannot be evaded by
		//     picking a different one.
		inserted, err := st.StageObservationCapped(o, maxStagedPerSession, maxStagedTotal)
		if err != nil {
			return errResult(fmt.Sprintf("stage: %v", err)), nil, nil
		}
		if !inserted {
			// At a cap, or this exact observation was already recorded (a no-op
			// dedup). Check the dedup case FIRST: a duplicate recorded while the
			// session is at the cap must report "already recorded", not a spurious
			// "too many" error (a count>=limit check alone can't tell them apart).
			if st.StagedExists(o.Session, o.ID) {
				return textResult("already recorded (" + in.Lens + "/" + in.Dimension + ")"), nil, nil
			}
			// Name the cap that actually blocked it, so a client is not told to split work
			// across sessions when the GLOBAL buffer is what is full (splitting would not help).
			if st.StagedTotal() >= maxStagedTotal {
				return errResult(fmt.Sprintf("the in-session observation buffer is full (%d staged "+
					"across all sessions); they clear as sessions end and are distilled",
					maxStagedTotal)), nil, nil
			}
			return errResult(fmt.Sprintf("too many in-session observations (limit %d per session)", maxStagedPerSession)), nil, nil
		}
		return textResult("recorded (" + in.Lens + "/" + in.Dimension + ")"), nil, nil
	})

	// get_facets: read the synthesized L2 facets for a lens — structured, cited,
	// current facet values. No embedding needed. The precise, machine-readable view
	// (trajectory arcs, mastery markers, resilience patterns).
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "get_facets",
		Description: "Read the synthesized growth facets (structured attributes) for a lens. " +
			"Returns current facet values — trajectory arcs, mastery markers, resilience patterns. " +
			"Defaults to the 'default' lens; pass lens='math' etc. for another lens. " +
			"(Facets are per-lens, so there is no cross-lens 'unified' facet set — that aggregate exists only for the narrative; use get_profile for it.) " +
			"For a readable prose overview instead, use get_profile.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in getFacetsInput) (*mcpsdk.CallToolResult, any, error) {
		lensName := in.Lens
		if lensName == "" {
			lensName = store.LensDefault
		}
		data := dataName(lensName)
		facets, err := st.ReadActiveFacets()
		if err != nil {
			return errResult(fmt.Sprintf("read facets: %v", err)), nil, nil
		}
		hw, err := st.ObservationHighWater()
		if err != nil {
			return errResult(fmt.Sprintf("read observation high-water: %v", err)), nil, nil
		}
		return textResult(renderFacets(lensName, data, facets, hw, func() string { return emptyHint(st, lensName, data, wire, false) })), nil, nil
	})

	// get_profile: read the L4 narrative summary (prose) for a lens — the human-
	// readable portrait distilled from the facets. Omit lens for the cross-lens
	// 'unified' portrait (an AGGREGATE VIEW blending every lens — not a lens itself).
	// Note the asymmetry vs search/get_facets, which default to the 'default' LENS.
	// Regenerated in the background after each review; may not exist yet on a brand-
	// new archive.
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "get_profile",
		Description: "Read the narrative growth profile (readable prose) for a lens. " +
			"Omit lens (or pass 'unified') for the cross-lens portrait — an aggregate blending ALL active lenses, not a lens itself. " +
			"Pass lens='default' for that lens's OWN narrative (distinct from the unified blend), or lens='math' etc. for another lens. " +
			"(This defaults to the unified aggregate, whereas search_observations/get_facets default to the 'default' lens.) " +
			"For precise structured attributes instead, use get_facets.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in getProfileInput) (*mcpsdk.CallToolResult, any, error) {
		lensName := in.Lens
		if lensName == "" {
			lensName = store.ProfileUnified
		}
		// Regenerate on read if stale (#100). Measured ~13s for a real regeneration and ~0.02s
		// when the signature already matches, so blocking the tool call is acceptable and an
		// unread profile costs nothing. NON-FATAL: a refresh failure still serves what is on
		// disk — a stale narrative is strictly better than none — and the note is appended to
		// the reply so the agent knows what it is holding rather than being misled.
		var stale string
		if refresh != nil {
			if err := refresh(); err != nil {
				stale = "\n\n---\n(note: this profile could not be refreshed just now — " +
					err.Error() + " — so it may not reflect the newest facets.)"
			}
		}
		data := dataName(lensName)
		md, ok, err := st.ReadProfile(data)
		if err != nil {
			return errResult(fmt.Sprintf("read profile: %v", err)), nil, nil
		}
		if !ok {
			// stale rides along: when a refresh failed, it is the actual reason there is no profile.
			return textResult(fmt.Sprintf("No narrative profile for '%s' yet. %s Use get_facets for the current structured attributes.%s",
				lensName, emptyHint(st, lensName, data, wire, false), stale)), nil, nil
		}
		return textResult(md + stale), nil, nil
	})

	// delete_observation: prune one L1 observation by id. Facets are reviewer-owned
	// (read-only); the supported way to correct the profile is to fix its inputs —
	// add a better observation, or remove a wrong/stale one here. The next review
	// re-derives the profile from what's left.
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "delete_observation",
		Description: "Delete one observation from the archive by obs_id (get ids from search_observations). " +
			"Use to prune a wrong or stale observation; the profile re-derives from the remaining ones on the next review. " +
			"The profile (facets) itself is not directly editable — observations are the input layer.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in deleteInput) (*mcpsdk.CallToolResult, any, error) {
		id := strings.TrimSpace(in.ObsID)
		if id == "" {
			return errResult("obs_id is required"), nil, nil
		}
		deleted, err := st.DeleteObservation(id)
		if err != nil {
			return errResult(fmt.Sprintf("delete: %v", err)), nil, nil
		}
		if !deleted {
			return textResult("no observation with id " + id), nil, nil
		}
		return textResult("deleted " + id), nil, nil
	})

	return server
}

func textResult(s string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: s}}}
}
func errResult(s string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{IsError: true, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: s}}}
}

// emptyHint names the remedy that actually applies when a lens shows nothing. Deliberately NOT a
// blanket "the reviewer may not have run yet": that is true only in the last case, and telling
// someone whose archive has no capture wired that waiting will help is how a never-filling install
// looks like a working one. lens is the name the agent passed (what every command takes); data is
// the name its rows are tagged with.
//
// The last case differs by surface: search is top-k with no score threshold, so zero hits never
// means "your query missed" — it means the lens has no observations carrying embeddings.
func emptyHint(st store.MCPStore, lens, data, wire string, search bool) string {
	reason := st.WhyEmpty(data)
	switch {
	case reason == store.NoCapture:
		return "This archive holds no raw turns at all, so nothing is capturing — the MCP server only READS. " +
			"Wire an editor: `" + wire + "`. Waiting will not help."
	case lens == store.ProfileUnified:
		// Not a lens: nothing is mined into it, so a lens remedy (backfilling a reserved name) can only fail.
		return "The unified portrait is written from the enabled lenses' facets; get_facets on a lens shows what it holds."
	case reason == store.NoObservations:
		return fmt.Sprintf("Capture is working, but nothing has been mined into '%s' yet. "+
			"Check the lens is enabled (`witness lens list`); `witness lens backfill %s` mines it over existing history.", lens, lens)
	case search:
		return fmt.Sprintf("'%s' has observations, but none of them carry embeddings, so nothing is searchable. "+
			"Check `witness doctor` for the embedder's status.", lens)
	default:
		return fmt.Sprintf("'%s' has observations but no facets yet — the reviewer folds them once enough accumulate; "+
			"force one with `witness worker review`.", lens)
	}
}

func renderHits(lens string, hits []vector.Hit, hint func() string) string {
	if len(hits) == 0 {
		return fmt.Sprintf("No observations found in the '%s' lens. %s", lens, hint())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Growth observations (%s lens), most relevant first:\n\n", lens)
	for _, h := range hits {
		o := h.Obs
		fmt.Fprintf(&b, "- [%s · %s] %s\n", o.TS, o.Dimension, o.Observation)
		if o.Evidence != "" {
			fmt.Fprintf(&b, "  evidence: %s\n", o.Evidence)
		}
		fmt.Fprintf(&b, "  id: %s\n", o.ID) // for delete_observation
	}
	return b.String()
}

// renderFacets reports each facet's CURRENT confidence — decayed to the observation high-water
// mark — not the value stored at its last confirmation. The stored number only falls when a
// review cites the facet, so an agent shown it would read a trait nothing has evidenced in
// thousands of observations as a certainty. lensName is shown; data is what the facets are tagged with.
func renderFacets(lensName, data string, facets []store.Facet, decaySeq int64, hint func() string) string {
	var b strings.Builder
	count := 0
	for _, f := range facets {
		if f.Lens != data {
			continue
		}
		cur := f.Current()
		if cur == nil {
			continue
		}
		fmt.Fprintf(&b, "- **%s/%s** (confidence %.2f): %s\n", f.Dimension, f.Key, cur.DecayedConfidence(decaySeq), cur.Value)
		count++
	}
	if count == 0 {
		return fmt.Sprintf("No facets found for lens '%s'. %s", lensName, hint())
	}
	header := fmt.Sprintf("Growth facets (%s lens) — %d:\n\n", lensName, count)
	return header + b.String()
}
