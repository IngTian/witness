package distill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// StaleSources returns the list of portraits on disk whose sources are missing or
// stale (text_hash != PortraitHash(current md)). Rows whose portrait is no longer
// on disk are deleted without a model call.
func (sm *Summarizer) StaleSources() ([]string, error) {
	if sm.ResolvePrompt == "" {
		return nil, nil // feature is off
	}

	// Enumerate portraits on disk.
	onDisk, err := sm.Store.ListProfiles()
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}

	// Remove sources rows for portraits that no longer exist.
	stored, err := sm.Store.ListPortraitSources()
	if err != nil {
		return nil, fmt.Errorf("list portrait sources: %w", err)
	}
	diskSet := make(map[string]bool, len(onDisk))
	for _, p := range onDisk {
		diskSet[p] = true
	}
	for _, p := range stored {
		if !diskSet[p] {
			// Best-effort: log but don't fail the enumeration.
			if err := sm.Store.DeletePortraitSources(p); err != nil {
				slog.Warn("sources: could not delete row for missing portrait", "portrait", p, "err", err)
			}
		}
	}

	// Check each on-disk portrait for staleness.
	var stale []string
	for _, p := range onDisk {
		if p == store.ProfileUnified {
			// Task 5 adds unified; skip it here.
			continue
		}
		md, ok, err := sm.Store.ReadProfile(p)
		if err != nil || !ok {
			// Read failure or disappeared between enum and read: skip.
			continue
		}
		wantHash := store.PortraitHash(md)
		_, gotHash, ok, err := sm.Store.ReadPortraitSources(p)
		if err != nil {
			// Read failure: treat as stale so it can self-heal.
			stale = append(stale, p)
			continue
		}
		if !ok || gotHash != wantHash {
			stale = append(stale, p)
		}
	}
	return stale, nil
}

// ResolveSources resolves every stale portrait (links each sentence to the traits it
// rests on). Returns the joined failures; portraits that succeed are durably stored.
func (sm *Summarizer) ResolveSources(ctx context.Context) error {
	if sm.ResolvePrompt == "" {
		return nil // feature is off
	}

	stale, err := sm.StaleSources()
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}

	// Index the active lenses by name for per-lens runner routing.
	byName := make(map[string]*lens.Lens, len(sm.Lenses))
	for _, l := range sm.Lenses {
		byName[l.Name] = l
	}

	var failures []error
	for _, portrait := range stale {
		ln := byName[portrait]
		if err := sm.resolveOneNotebook(ctx, portrait, ln); err != nil {
			slog.Warn("sources: failed to resolve portrait", "portrait", portrait, "err", err)
			failures = append(failures, fmt.Errorf("resolve %s: %w", portrait, err))
		}
	}
	return errors.Join(failures...)
}

// resolveOneNotebook resolves a single notebook portrait: reads the profile, reads the
// facets, calls the resolver, parses and validates the reply, and writes the result.
func (sm *Summarizer) resolveOneNotebook(ctx context.Context, portrait string, ln *lens.Lens) error {
	md, ok, err := sm.Store.ReadProfile(portrait)
	if err != nil {
		return fmt.Errorf("read profile: %w", err)
	}
	if !ok || strings.TrimSpace(md) == "" {
		// Portrait disappeared or is empty: nothing to resolve.
		return nil
	}

	sentences := SplitSentences(md)
	if len(sentences) == 0 {
		// No sentences: write an empty result so it's no longer stale.
		return sm.Store.WritePortraitSources(portrait, store.PortraitHash(md), store.PortraitSources{}, time.Now())
	}

	// Read the active facets for this lens only — the resolver is shown only this
	// lens's facets and may cite only those (a default portrait cannot cite a math
	// facet the resolver never saw).
	allFacets, err := sm.Store.ReadActiveFacets()
	if err != nil {
		return fmt.Errorf("read facets: %w", err)
	}
	var lensFacets []store.Facet
	for _, f := range allFacets {
		if f.Lens == portrait && f.Current() != nil {
			lensFacets = append(lensFacets, f)
		}
	}

	// Build the valid-key set from the same facets shown to the resolver.
	validKeys := make(map[string]bool)
	for _, f := range lensFacets {
		key := f.Dimension + "/" + f.Key
		validKeys[key] = true
	}

	input := renderResolveInput(sentences, lensFacets)
	model := ModelFor(sm.Config, ln, PhaseReview)
	reply, err := sm.runFor(ln)(ctx, model, sm.ResolvePrompt, input)
	if err != nil {
		return fmt.Errorf("resolver call: %w", err)
	}

	src, err := parseLensLinks(reply, sentences, len(sentences), validKeys, portrait)
	if err != nil {
		return fmt.Errorf("parse reply: %w", err)
	}

	return sm.Store.WritePortraitSources(portrait, store.PortraitHash(md), src, time.Now())
}

// renderResolveInput formats the sentences and facets as the resolver expects.
// Each facet's text is its FULL current value (ruling R5).
func renderResolveInput(sentences []string, facets []store.Facet) string {
	var b strings.Builder
	fmt.Fprintln(&b, "PROFILE SENTENCES:")
	for i, s := range sentences {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "FACETS (dimension/key: text):")
	for _, f := range facets {
		cur := f.Current()
		if cur == nil {
			continue
		}
		fmt.Fprintf(&b, "- %s/%s: %s\n", f.Dimension, f.Key, cur.Value)
	}
	return b.String()
}

// resolverReply is the JSON structure the resolver returns.
type resolverReply struct {
	Links    []resolverLink    `json:"links"`
	Keywords []resolverKeyword `json:"keywords"`
}

type resolverLink struct {
	N    int               `json:"n"`    // 1-based sentence number
	Keys []resolverLinkKey `json:"keys"` // facet references
}

type resolverLinkKey struct {
	Key      string `json:"key"`      // "dimension/key"
	Strength string `json:"strength"` // "direct" or "partial"
}

type resolverKeyword struct {
	K string `json:"k"` // the keyword phrase
	N []int  `json:"n"` // 1-based sentence numbers
}

// parseLensLinks extracts and validates the resolver's JSON reply. It returns an error
// when there is no parseable JSON object. Validation: unknown/archived keys are dropped;
// out-of-range sentence numbers are dropped; invalid strengths are coerced to partial;
// duplicate keys within a sentence keep the stronger strength (direct > partial); links
// are stable-sorted direct-first then capped at 3 per sentence. Keyword indexes out of
// range are dropped, and more than 8 keywords are truncated. lensName is the notebook
// portrait's lens (filled into each link). sentenceTexts are the split sentences (for
// setting SourcedSentence.Text).
func parseLensLinks(reply string, sentenceTexts []string, nSentences int, validKeys map[string]bool, lensName string) (store.PortraitSources, error) {
	// Extract the substring from the first { to the last }.
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start == -1 || end == -1 || start >= end {
		return store.PortraitSources{}, fmt.Errorf("no JSON object found")
	}
	jsonText := reply[start : end+1]

	var rr resolverReply
	if err := json.Unmarshal([]byte(jsonText), &rr); err != nil {
		return store.PortraitSources{}, fmt.Errorf("unmarshal: %w", err)
	}

	// Build sentences with validated links. Multiple reply entries with the same n are
	// merged (the resolver may send two link blocks for one sentence).
	sentences := make([]store.SourcedSentence, nSentences)
	for i, txt := range sentenceTexts {
		sentences[i].Text = txt
	}

	// Group links by sentence number, then process each sentence.
	bySentence := make(map[int][]store.TraitLink)
	for _, rl := range rr.Links {
		// Validate n is in range (1-based in reply, 0-based in storage).
		if rl.N < 1 || rl.N > nSentences {
			continue
		}
		idx := rl.N - 1
		// Filter and validate all keys in this entry.
		for _, k := range rl.Keys {
			// Unknown or archived key: drop.
			if !validKeys[k.Key] {
				continue
			}
			// Validate strength: direct or partial, else coerce to partial.
			strength := k.Strength
			if strength != store.LinkDirect && strength != store.LinkPartial {
				strength = store.LinkPartial
			}
			// Split "dimension/key" into parts.
			parts := strings.SplitN(k.Key, "/", 2)
			if len(parts) != 2 {
				continue
			}
			bySentence[idx] = append(bySentence[idx], store.TraitLink{
				Lens:      lensName,
				Dimension: parts[0],
				Key:       parts[1],
				Strength:  strength,
			})
		}
	}

	// For each sentence: dedupe by key (keeping stronger strength), stable sort direct-first, cap at 3.
	for idx, links := range bySentence {
		// Dedupe: for each key, keep the stronger strength (direct > partial).
		seen := make(map[string]string) // "dimension/key" -> strength
		var deduped []store.TraitLink
		for _, link := range links {
			key := link.Dimension + "/" + link.Key
			existing, ok := seen[key]
			if !ok {
				// First occurrence: keep it.
				seen[key] = link.Strength
				deduped = append(deduped, link)
			} else if link.Strength == store.LinkDirect && existing == store.LinkPartial {
				// Upgrade: replace partial with direct.
				seen[key] = store.LinkDirect
				for i := range deduped {
					if deduped[i].Dimension == link.Dimension && deduped[i].Key == link.Key {
						deduped[i].Strength = store.LinkDirect
						break
					}
				}
			}
			// else: already seen with same or stronger strength, skip.
		}
		// Stable sort: direct before partial (preserves original order within each strength).
		var direct, partial []store.TraitLink
		for _, link := range deduped {
			if link.Strength == store.LinkDirect {
				direct = append(direct, link)
			} else {
				partial = append(partial, link)
			}
		}
		sorted := append(direct, partial...)
		// Cap at 3.
		if len(sorted) > 3 {
			sorted = sorted[:3]
		}
		sentences[idx].Links = sorted
	}

	// Validate keywords: drop out-of-range indexes, truncate to 8.
	var keywords []store.Keyword
	for _, kw := range rr.Keywords {
		var idxs []int
		for _, n := range kw.N {
			// Convert 1-based to 0-based and check range.
			if n < 1 || n > nSentences {
				continue
			}
			idxs = append(idxs, n-1)
		}
		// Only keep keywords that have at least one valid index.
		if len(idxs) > 0 {
			keywords = append(keywords, store.Keyword{
				Word:      kw.K,
				Sentences: idxs,
			})
		}
	}
	if len(keywords) > 8 {
		keywords = keywords[:8]
	}

	return store.PortraitSources{
		Sentences: sentences,
		Keywords:  keywords,
	}, nil
}
