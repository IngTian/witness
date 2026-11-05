package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Link strength constants: direct = the trait fully states what the sentence asserts,
// or one of its claims; partial = covers only part of a claim, or is related.
const (
	LinkDirect  = "direct"
	LinkPartial = "partial"
)

// TraitLink is one facet a portrait sentence rests on.
type TraitLink struct {
	Lens      string `json:"lens"`
	Dimension string `json:"dimension"`
	Key       string `json:"key"`
	Strength  string `json:"strength"` // LinkDirect or LinkPartial
}

// SourcedSentence is one sentence of the portrait, plus the traits it cites.
type SourcedSentence struct {
	Text  string      `json:"text"`
	Links []TraitLink `json:"links"`
}

// Keyword is a salient word appearing in the portrait, with 0-based indexes
// into the Sentences array.
type Keyword struct {
	Word      string `json:"word"`
	Sentences []int  `json:"sentences"`
}

// PortraitSources records which facets each sentence of a portrait rests on,
// plus keywords and (for unified) which notebook-specific portraits it inherits
// from. Derived data: the self-heal rebuilds these after a merge, so merge does
// not copy them.
type PortraitSources struct {
	Sentences []SourcedSentence `json:"sentences"`
	Keywords  []Keyword         `json:"keywords"`
	Inherits  map[string]string `json:"inherits"` // unified only: notebook -> that notebook's source text_hash
}

// PortraitHash returns the lowercase hex SHA256 of the exact markdown bytes.
func PortraitHash(md string) string {
	h := sha256.Sum256([]byte(md))
	return hex.EncodeToString(h[:])
}

// WritePortraitSources upserts the sources for a portrait. resolved_at is the
// timestamp of the resolution pass that produced this, stored as RFC3339 UTC.
func (s *Store) WritePortraitSources(portrait, textHash string, src PortraitSources, at time.Time) error {
	b, err := json.Marshal(src)
	if err != nil {
		return fmt.Errorf("marshal sources: %w", err)
	}
	resolvedAt := at.UTC().Format(time.RFC3339)
	_, err = s.db.Exec(`INSERT INTO portrait_sources(portrait, text_hash, resolved_at, sources)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(portrait) DO UPDATE SET
			text_hash = excluded.text_hash,
			resolved_at = excluded.resolved_at,
			sources = excluded.sources`,
		portrait, textHash, resolvedAt, string(b))
	if err != nil {
		return fmt.Errorf("write portrait_sources: %w", err)
	}
	return nil
}

// ReadPortraitSources reads the sources for a portrait. Returns ok=false if
// the portrait has no sources row.
func (s *Store) ReadPortraitSources(portrait string) (src PortraitSources, textHash string, ok bool, err error) {
	var sourcesJSON string
	err = s.db.QueryRow(`SELECT text_hash, sources FROM portrait_sources WHERE portrait = ?`, portrait).
		Scan(&textHash, &sourcesJSON)
	if err == sql.ErrNoRows {
		return PortraitSources{}, "", false, nil
	}
	if err != nil {
		return PortraitSources{}, "", false, fmt.Errorf("read portrait_sources: %w", err)
	}
	if err := json.Unmarshal([]byte(sourcesJSON), &src); err != nil {
		return PortraitSources{}, "", false, fmt.Errorf("unmarshal sources: %w", err)
	}
	return src, textHash, true, nil
}

// DeletePortraitSources removes the sources row for a portrait.
func (s *Store) DeletePortraitSources(portrait string) error {
	_, err := s.db.Exec(`DELETE FROM portrait_sources WHERE portrait = ?`, portrait)
	if err != nil {
		return fmt.Errorf("delete portrait_sources: %w", err)
	}
	return nil
}

// ListPortraitSources returns all portrait names that have sources.
func (s *Store) ListPortraitSources() ([]string, error) {
	rows, err := s.db.Query(`SELECT portrait FROM portrait_sources ORDER BY portrait`)
	if err != nil {
		return nil, fmt.Errorf("list portrait_sources: %w", err)
	}
	defer rows.Close()
	var portraits []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		portraits = append(portraits, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return portraits, nil
}

// CurrentPortraitSources returns the sources for a portrait only when they
// describe the portrait's current text, with links remapped through compaction
// merges. Returns ok=false when: no sources row, portrait file missing, or the
// stored text_hash doesn't match the current profile's hash.
//
// A link to an archived facet with merged_into is rewritten to the survivor
// (following chains if needed); an archived facet without merged_into
// (retired) is dropped. Links to nonexistent facets are also dropped. After
// remapping, links are deduped per sentence by (lens, dimension, key) keeping
// the stronger strength, and returned direct first (stable).
func (s *Store) CurrentPortraitSources(portrait string) (PortraitSources, bool, error) {
	// Read stored sources
	src, storedHash, ok, err := s.ReadPortraitSources(portrait)
	if err != nil {
		return PortraitSources{}, false, err
	}
	if !ok {
		return PortraitSources{}, false, nil
	}

	// Empty row (zero sentences) reads as "not recorded yet".
	if len(src.Sentences) == 0 {
		return PortraitSources{}, false, nil
	}

	// Read current profile text
	text, exists, err := s.ReadProfile(portrait)
	if err != nil {
		return PortraitSources{}, false, err
	}
	if !exists {
		return PortraitSources{}, false, nil
	}

	// Compare hashes
	currentHash := PortraitHash(text)
	if currentHash != storedHash {
		return PortraitSources{}, false, nil
	}

	// Read all facets (including archived) to build a lookup map
	facets, err := s.ReadFacets()
	if err != nil {
		return PortraitSources{}, false, err
	}

	// Build facet map: (lens, dimension, key) -> facet
	facetMap := make(map[facetKey]*Facet)
	for i := range facets {
		fk := facetKey{facets[i].Lens, facets[i].Dimension, facets[i].Key}
		facetMap[fk] = &facets[i]
	}

	// Remap links through merge chains
	for i := range src.Sentences {
		var remapped []TraitLink
		for _, link := range src.Sentences[i].Links {
			resolved := resolveLink(link, facetMap)
			if resolved != nil {
				remapped = append(remapped, *resolved)
			}
		}
		// Dedupe by (lens, dimension, key), keep stronger, sort direct first (stable).
		src.Sentences[i].Links = DedupeLinks(remapped)
	}

	return src, true, nil
}

// facetKey is the composite key that uniquely identifies a facet.
type facetKey struct{ lens, dimension, key string }

// resolveLink follows a link through merge chains to its survivor, or returns
// nil if the link should be dropped (nonexistent, retired, or cycle).
func resolveLink(link TraitLink, facetMap map[facetKey]*Facet) *TraitLink {
	fk := facetKey{link.Lens, link.Dimension, link.Key}
	visited := make(map[facetKey]bool)

	for {
		if visited[fk] {
			// Cycle detected — drop
			return nil
		}
		visited[fk] = true

		f, exists := facetMap[fk]
		if !exists {
			// Facet doesn't exist — drop
			return nil
		}

		if f.ArchivedAt == "" {
			// Active facet — this is the survivor
			return &TraitLink{
				Lens:      fk.lens,
				Dimension: fk.dimension,
				Key:       fk.key,
				Strength:  link.Strength,
			}
		}

		if f.MergedInto == "" {
			// Retired (archived without merge target) — drop
			return nil
		}

		// Follow merge. MergedInto stores a FacetRef ("dimension/key")
		dim, key, ok := ParseFacetRef(f.MergedInto)
		if !ok {
			// Malformed merged_into — drop
			return nil
		}
		fk = facetKey{fk.lens, dim, key}
	}
}

// DedupeLinks deduplicates links by (lens, dimension, key), keeping the stronger strength
// (direct > partial), and returns them with direct first, then partial, preserving insertion
// order within each strength. Exported for use by both store and distill packages.
func DedupeLinks(links []TraitLink) []TraitLink {
	type lk struct{ lens, dimension, key string }
	seen := make(map[lk]int) // key -> index in deduped slice
	var deduped []TraitLink

	for _, link := range links {
		k := lk{link.Lens, link.Dimension, link.Key}
		idx, exists := seen[k]
		if !exists {
			// First occurrence: append and record index.
			seen[k] = len(deduped)
			deduped = append(deduped, link)
		} else {
			// Duplicate: keep stronger strength (direct > partial).
			if link.Strength == LinkDirect && deduped[idx].Strength == LinkPartial {
				deduped[idx].Strength = LinkDirect
			}
		}
	}

	// Stable sort: direct first, then partial, preserving insertion order within each group.
	var direct, partial []TraitLink
	for _, link := range deduped {
		if link.Strength == LinkDirect {
			direct = append(direct, link)
		} else {
			partial = append(partial, link)
		}
	}
	return append(direct, partial...)
}

// dedupLinks is deprecated: use DedupeLinks.
func dedupLinks(links []TraitLink) []TraitLink {
	return DedupeLinks(links)
}

// sortLinksDirectFirst is deprecated: DedupeLinks now handles sorting.
func sortLinksDirectFirst(links []TraitLink) []TraitLink {
	var direct, partial []TraitLink
	for _, link := range links {
		if link.Strength == LinkDirect {
			direct = append(direct, link)
		} else {
			partial = append(partial, link)
		}
	}
	return append(direct, partial...)
}
