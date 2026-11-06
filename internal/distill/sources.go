package distill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/sentence"
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
			// Check unified staleness separately.
			if sm.isUnifiedStale() {
				stale = append(stale, p)
			}
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

// notebookData holds a notebook's PortraitSources and its text_hash.
type notebookData struct {
	src  store.PortraitSources
	hash string
}

// isUnifiedStale returns true when the unified portrait is stale (ruling R9):
//   - No row exists, OR
//   - Own hash differs, OR
//   - For each (lens, h) in src.Inherits, the stored text_hash of that lens's row != h
//     (a missing row counts as different), OR
//   - A lens that has a CURRENT row (row hash == PortraitHash of its profile now) is
//     absent from Inherits.
func (sm *Summarizer) isUnifiedStale() bool {
	if sm.ResolveUnifiedPrompt == "" {
		return false // feature is off
	}

	md, ok, err := sm.Store.ReadProfile(store.ProfileUnified)
	if err != nil || !ok {
		return false // no unified portrait on disk
	}

	wantHash := store.PortraitHash(md)
	src, gotHash, ok, err := sm.Store.ReadPortraitSources(store.ProfileUnified)
	if err != nil || !ok {
		return true // no row or read failure: stale
	}
	if gotHash != wantHash {
		return true // own hash differs
	}

	// For each (lens, h) in Inherits, check that lens's stored text_hash == h.
	for lens, inheritedHash := range src.Inherits {
		_, storedHash, ok, err := sm.Store.ReadPortraitSources(lens)
		if err != nil || !ok {
			return true // missing row or read failure: different
		}
		if storedHash != inheritedHash {
			return true // notebook changed
		}
	}

	// Check if any notebook has a CURRENT row (hash matches its profile) that's absent from Inherits.
	// M3: check all on-disk notebooks (ListProfiles minus unified), not only sm.Lenses — a disabled
	// lens still has a portrait the unified writer used.
	allNotebooks, err := sm.Store.ListProfiles()
	if err != nil {
		return true // list failure: assume stale
	}
	for _, name := range allNotebooks {
		if name == store.ProfileUnified {
			continue // skip unified itself
		}
		if _, alreadyInherited := src.Inherits[name]; alreadyInherited {
			continue // already checked above
		}
		// Check if this notebook has a current row.
		_, storedHash, ok, err := sm.Store.ReadPortraitSources(name)
		if err != nil || !ok {
			continue // no row or read failure: skip
		}
		// Read the notebook's profile to check if the row is current.
		nbMd, nbOK, nbErr := sm.Store.ReadProfile(name)
		if nbErr != nil || !nbOK {
			continue // no profile or read failure: skip
		}
		if store.PortraitHash(nbMd) == storedHash {
			// This notebook has a current row but is absent from Inherits: stale.
			return true
		}
	}

	return false
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
		if portrait == store.ProfileUnified {
			continue // resolve unified after all notebooks
		}
		ln := byName[portrait]
		if err := sm.resolveOneNotebook(ctx, portrait, ln); err != nil {
			slog.Warn("sources: failed to resolve portrait", "portrait", portrait, "err", err)
			failures = append(failures, fmt.Errorf("resolve %s: %w", portrait, err))
		}
	}

	// Resolve unified after all notebooks (it inherits from them).
	// Check staleness AFTER the notebook loop so a notebook resolved in this same call
	// triggers unified in the same call.
	if sm.ResolveUnifiedPrompt != "" && sm.isUnifiedStale() {
		if err := sm.resolveUnified(ctx); err != nil {
			slog.Warn("sources: failed to resolve unified portrait", "err", err)
			failures = append(failures, fmt.Errorf("resolve unified: %w", err))
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

	sentences := sentence.SplitSentences(md)
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

// extractJSONObject extracts the substring from the first { to the last } in reply.
func extractJSONObject(reply string) (string, error) {
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start == -1 || end == -1 || start >= end {
		return "", fmt.Errorf("no JSON object found")
	}
	return reply[start : end+1], nil
}

// finalizeLinks deduplicates links by lens/dimension/key (keeping stronger strength),
// stable-sorts direct-first, and caps at 3.
func finalizeLinks(links []store.TraitLink) []store.TraitLink {
	// Dedupe: for each key, keep the stronger strength (direct > partial), and sort
	// direct first, stable. Uses the same helper as CurrentPortraitSources for consistency.
	deduped := store.DedupeLinks(links)
	// Cap at 3.
	if len(deduped) > 3 {
		deduped = deduped[:3]
	}
	return deduped
}

// parseKeywords validates keyword sentence indexes (1-based in input, 0-based in output),
// drops out-of-range indexes, and truncates to 8 keywords.
func parseKeywords(keywords []resolverKeyword, nSentences int) []store.Keyword {
	var result []store.Keyword
	for _, kw := range keywords {
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
			// M5: cap keyword text at 64 runes (trim, drop empty).
			word := strings.TrimSpace(kw.K)
			if len([]rune(word)) > 64 {
				word = string([]rune(word)[:64])
			}
			if word != "" {
				result = append(result, store.Keyword{
					Word:      word,
					Sentences: idxs,
				})
			}
		}
	}
	if len(result) > 8 {
		result = result[:8]
	}
	return result
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
	jsonText, err := extractJSONObject(reply)
	if err != nil {
		return store.PortraitSources{}, err
	}

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
		sentences[idx].Links = finalizeLinks(links)
	}

	keywords := parseKeywords(rr.Keywords, nSentences)

	return store.PortraitSources{
		Sentences: sentences,
		Keywords:  keywords,
	}, nil
}

// resolveUnified resolves the unified portrait by inheriting links from notebook sentences.
func (sm *Summarizer) resolveUnified(ctx context.Context) error {
	if sm.ResolveUnifiedPrompt == "" {
		return nil // feature is off
	}

	md, ok, err := sm.Store.ReadProfile(store.ProfileUnified)
	if err != nil {
		return fmt.Errorf("read profile: %w", err)
	}
	if !ok || strings.TrimSpace(md) == "" {
		return nil // no unified portrait or empty
	}

	sentences := sentence.SplitSentences(md)
	if len(sentences) == 0 {
		// No sentences: write an empty result so it's no longer stale.
		return sm.Store.WritePortraitSources(store.ProfileUnified, store.PortraitHash(md), store.PortraitSources{}, time.Now())
	}

	// Collect all notebook sources rows that have current data (M3: use all on-disk notebooks,
	// not only sm.Lenses — a disabled lens still has a portrait the unified writer used).
	allNotebooks, err := sm.Store.ListProfiles()
	if err != nil {
		return fmt.Errorf("list profiles: %w", err)
	}
	notebooks := make(map[string]notebookData)
	for _, name := range allNotebooks {
		if name == store.ProfileUnified {
			continue // skip unified itself
		}
		src, hash, ok, err := sm.Store.ReadPortraitSources(name)
		if err != nil || !ok {
			continue // notebook has no row or read failure: skip it
		}
		// Verify the notebook's row is current (its hash matches the profile on disk).
		nbMd, nbOK, nbErr := sm.Store.ReadProfile(name)
		if nbErr != nil || !nbOK {
			continue
		}
		if store.PortraitHash(nbMd) != hash {
			continue // stale row: skip it
		}
		notebooks[name] = notebookData{src: src, hash: hash}
	}

	// Skip the model call when there are no current notebook rows.
	if len(notebooks) == 0 {
		return sm.Store.WritePortraitSources(store.ProfileUnified, store.PortraitHash(md), store.PortraitSources{}, time.Now())
	}

	// Build the input for the unified resolver.
	input := renderUnifiedInput(sentences, notebooks, sm.Lenses)
	model := ModelFor(sm.Config, nil, PhaseReview)
	reply, err := sm.Run(ctx, model, sm.ResolveUnifiedPrompt, input)
	if err != nil {
		return fmt.Errorf("resolver call: %w", err)
	}

	src, err := parseUnifiedLinks(reply, sentences, len(sentences), notebooks)
	if err != nil {
		return fmt.Errorf("parse reply: %w", err)
	}

	return sm.Store.WritePortraitSources(store.ProfileUnified, store.PortraitHash(md), src, time.Now())
}

// renderUnifiedInput formats the unified sentences and notebook sentences as the
// unified resolver expects. M2: notebooks are rendered in stable order (order of sm.Lenses,
// then any remaining notebooks sorted by name).
func renderUnifiedInput(sentences []string, notebooks map[string]notebookData, lensOrder []*lens.Lens) string {
	var b strings.Builder
	fmt.Fprintln(&b, "PORTRAIT SENTENCES:")
	for i, s := range sentences {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "NOTEBOOK SENTENCES:")

	// Build stable order: sm.Lenses first, then remaining notebooks sorted.
	var order []string
	seen := make(map[string]bool)
	for _, ln := range lensOrder {
		if _, ok := notebooks[ln.Name]; ok {
			order = append(order, ln.Name)
			seen[ln.Name] = true
		}
	}
	// Add any remaining notebooks (not in lensOrder) in sorted order.
	var remaining []string
	for name := range notebooks {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	order = append(order, remaining...)

	// Render notebooks in stable order.
	for _, name := range order {
		nb := notebooks[name]
		for i, sent := range nb.src.Sentences {
			fmt.Fprintf(&b, "%s#%d: %s\n", name, i+1, sent.Text)
		}
	}
	return b.String()
}

// unifiedReply is the JSON structure the unified resolver returns.
type unifiedReply struct {
	Links    []unifiedLink     `json:"links"`
	Keywords []resolverKeyword `json:"keywords"`
}

type unifiedLink struct {
	N    int          `json:"n"`    // 1-based unified sentence number
	Refs []unifiedRef `json:"refs"` // notebook sentence references
}

type unifiedRef struct {
	Ref      string `json:"ref"`      // "lens#n" (1-based)
	Strength string `json:"strength"` // "direct" or "partial"
}

// parseUnifiedLinks extracts and validates the unified resolver's JSON reply. It inherits
// links from the referenced notebook sentences. The strength is partial if either the ref
// or the notebook link is partial. Duplicates are removed by key (keeping stronger strength),
// then sorted direct-first, then capped at 3 per sentence. Refs to a notebook with no current
// row, or n out of range, are dropped. Inherits[lens] records the notebook row's text_hash used.
// sentenceTexts are the split unified sentences (for setting SourcedSentence.Text).
func parseUnifiedLinks(reply string, sentenceTexts []string, nSentences int, notebooks map[string]notebookData) (store.PortraitSources, error) {
	jsonText, err := extractJSONObject(reply)
	if err != nil {
		return store.PortraitSources{}, err
	}

	var ur unifiedReply
	if err := json.Unmarshal([]byte(jsonText), &ur); err != nil {
		return store.PortraitSources{}, fmt.Errorf("unmarshal: %w", err)
	}

	// Build sentences with inherited links.
	sentences := make([]store.SourcedSentence, nSentences)
	for i, txt := range sentenceTexts {
		sentences[i].Text = txt
	}

	// Group refs by sentence number, then process each sentence.
	bySentence := make(map[int][]store.TraitLink)
	for _, ul := range ur.Links {
		// Validate n is in range (1-based in reply, 0-based in storage).
		if ul.N < 1 || ul.N > nSentences {
			continue
		}
		idx := ul.N - 1
		// Process each ref.
		for _, ref := range ul.Refs {
			// Parse "lens#n" format.
			parts := strings.Split(ref.Ref, "#")
			if len(parts) != 2 {
				continue // malformed ref
			}
			lensName := parts[0]
			var sentNum int
			if _, err := fmt.Sscanf(parts[1], "%d", &sentNum); err != nil {
				continue // malformed n
			}

			// Lookup the notebook.
			nbData, ok := notebooks[lensName]
			if !ok {
				continue // notebook has no current row
			}
			// Validate sentNum is in range (1-based in ref, 0-based in storage).
			if sentNum < 1 || sentNum > len(nbData.src.Sentences) {
				continue // out of range
			}
			nbSentIdx := sentNum - 1

			// Validate ref strength: direct or partial, else coerce to partial.
			refStrength := ref.Strength
			if refStrength != store.LinkDirect && refStrength != store.LinkPartial {
				refStrength = store.LinkPartial
			}

			// Inherit the notebook sentence's links, weakening if the ref is partial.
			for _, nbLink := range nbData.src.Sentences[nbSentIdx].Links {
				strength := nbLink.Strength
				// Weaken to partial if either the ref or the link is partial.
				if refStrength == store.LinkPartial || nbLink.Strength == store.LinkPartial {
					strength = store.LinkPartial
				}
				bySentence[idx] = append(bySentence[idx], store.TraitLink{
					Lens:      nbLink.Lens,
					Dimension: nbLink.Dimension,
					Key:       nbLink.Key,
					Strength:  strength,
				})
			}
		}
	}

	// For each sentence: dedupe by key (keeping stronger strength), stable sort direct-first, cap at 3.
	for idx, links := range bySentence {
		sentences[idx].Links = finalizeLinks(links)
	}

	keywords := parseKeywords(ur.Keywords, nSentences)

	// Build Inherits map: record the text_hash of each notebook used.
	inherits := make(map[string]string)
	for lens, nbData := range notebooks {
		inherits[lens] = nbData.hash
	}

	return store.PortraitSources{
		Sentences: sentences,
		Keywords:  keywords,
		Inherits:  inherits,
	}, nil
}
