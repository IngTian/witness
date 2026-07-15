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
	Lens  string `json:"lens,omitempty" jsonschema:"which lens to search; omit for the always-on 'default' lens (cross-domain), or name a repo lens like 'math'"`
	K     int    `json:"k,omitempty" jsonschema:"max results (default 8)"`
}

// getFacetsInput is the typed argument for get_facets (structured L2 facets).
type getFacetsInput struct {
	Lens string `json:"lens,omitempty" jsonschema:"which lens to read; omit for the always-on 'default' lens (cross-domain), or name a repo lens like 'math'"`
}

// getProfileInput is the typed argument for get_profile (the L4 narrative).
// NOTE the asymmetry from search/facets above: those default to the 'default'
// LENS, but the profile defaults to 'unified' — the cross-lens aggregate VIEW that
// blends every active lens (default + math + …), which is NOT itself a lens. Pass
// 'default' to read the default lens's own narrative instead of the blend.
type getProfileInput struct {
	Lens string `json:"lens,omitempty" jsonschema:"omit for 'unified' (the cross-lens portrait blending ALL lenses — an aggregate view, not a lens); or name one lens: 'default' (the always-on lens's own narrative), 'math', etc."`
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

// Serve runs the MCP stdio server until the context is cancelled. version is the
// witness build version to report in the MCP initialize handshake; it is passed
// in from cmd (which owns the ldflags-injected version) so this leaf package does
// not import cmd. Empty falls back to "dev".
func Serve(ctx context.Context, st *store.Store, emb Embedder, version string) error {
	return newServer(st, emb, version).Run(ctx, &mcpsdk.StdioTransport{})
}

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
func newServer(st *store.Store, emb Embedder, version string) *mcpsdk.Server {
	if strings.TrimSpace(version) == "" {
		version = "dev"
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
			"Defaults to the 'default' lens (cross-domain); pass lens='math' etc. for a repo-scoped view.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in searchInput) (*mcpsdk.CallToolResult, any, error) {
		lens := in.Lens
		if lens == "" {
			lens = store.LensDefault // default view = the cross-domain person
		}
		k := in.K
		if k <= 0 {
			k = 8
		}
		obs, err := st.ReadObservations("") // read all; vector.Search filters by lens
		if err != nil {
			return errResult(fmt.Sprintf("read archive: %v", err)), nil, nil
		}
		qv, err := emb.Embed(in.Query)
		if err != nil {
			return errResult(fmt.Sprintf("embed query: %v", err)), nil, nil
		}
		hits := vector.Search(obs, qv, lens, k)
		return textResult(renderHits(lens, hits)), nil, nil
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
		o := store.Observation{
			ID:          obsID(in.Session, in.Lens, in.Observation),
			TS:          time.Now().UTC().Format(time.RFC3339),
			Session:     in.Session,
			Lens:        in.Lens,
			Dimension:   in.Dimension,
			Observation: in.Observation,
			Evidence:    in.Evidence,
			Poignancy:   in.Poignancy,
			Source:      "active",
		}
		// Bound how many an agent can stage per session (caps disk growth and stops a
		// runaway loop from flooding L1 / forcing reviews). Enforced atomically in the
		// store so concurrent MCP processes can't both pass a check and race past it.
		inserted, err := st.StageObservationCapped(o, maxStagedPerSession)
		if err != nil {
			return errResult(fmt.Sprintf("stage: %v", err)), nil, nil
		}
		if !inserted {
			// At the cap, or this exact observation was already recorded (a no-op
			// dedup). Check the dedup case FIRST: a duplicate recorded while the
			// session is at the cap must report "already recorded", not a spurious
			// "too many" error (a count>=limit check alone can't tell them apart).
			if st.StagedExists(o.Session, o.ID) {
				return textResult("already recorded (" + in.Lens + "/" + in.Dimension + ")"), nil, nil
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
			"Defaults to the always-on 'default' lens (cross-domain); pass lens='math' etc. for a repo lens. " +
			"(Facets are per-lens, so there is no cross-lens 'unified' facet set — that aggregate exists only for the narrative; use get_profile for it.) " +
			"For a readable prose overview instead, use get_profile.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in getFacetsInput) (*mcpsdk.CallToolResult, any, error) {
		lensName := in.Lens
		if lensName == "" {
			lensName = store.LensDefault
		}
		facets, err := st.ReadFacets()
		if err != nil {
			return errResult(fmt.Sprintf("read facets: %v", err)), nil, nil
		}
		return textResult(renderFacets(lensName, facets)), nil, nil
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
			"Pass lens='default' for the always-on lens's OWN narrative (distinct from the unified blend), or lens='math' etc. for another lens. " +
			"(This defaults to the unified aggregate, whereas search_observations/get_facets default to the 'default' lens.) " +
			"For precise structured attributes instead, use get_facets.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in getProfileInput) (*mcpsdk.CallToolResult, any, error) {
		lensName := in.Lens
		if lensName == "" {
			lensName = store.LensUnified
		}
		md, ok, err := st.ReadProfile(lensName)
		if err != nil {
			return errResult(fmt.Sprintf("read profile: %v", err)), nil, nil
		}
		if !ok {
			return textResult(fmt.Sprintf("No narrative profile for '%s' yet — it is generated in the background after the next review. Use get_facets for the current structured attributes.", lensName)), nil, nil
		}
		return textResult(md), nil, nil
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

func renderHits(lens string, hits []vector.Hit) string {
	if len(hits) == 0 {
		return fmt.Sprintf("No observations found in the '%s' lens.", lens)
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

func renderFacets(lensName string, facets []store.Facet) string {
	var b strings.Builder
	count := 0
	for _, f := range facets {
		if f.Lens != lensName {
			continue
		}
		cur := f.Current()
		if cur == nil {
			continue
		}
		fmt.Fprintf(&b, "- **%s/%s** (confidence %.2f): %s\n", f.Dimension, f.Key, cur.Confidence, cur.Value)
		count++
	}
	if count == 0 {
		return fmt.Sprintf("No facets found for lens '%s'. The reviewer may not have run yet.", lensName)
	}
	header := fmt.Sprintf("Growth facets (%s lens) — %d:\n\n", lensName, count)
	return header + b.String()
}

func obsID(session, lens, text string) string {
	// match the worker's id scheme so active+mined dedup is coherent
	h := sha1sum(session + "|" + lens + "|" + text)
	return "obs_" + h[:12]
}
