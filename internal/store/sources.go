package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Link strength constants: direct = the sentence restates the facet's wording
// verbatim; partial = the sentence rests on the facet but is not a direct copy.
const (
	LinkDirect  = "direct"
	LinkPartial = "partial"
)

// TraitLink is one facet a portrait sentence rests on.
type TraitLink struct {
	Lens      string
	Dimension string
	Key       string
	Strength  string // LinkDirect or LinkPartial
}

// SourcedSentence is one sentence of the portrait, plus the traits it cites.
type SourcedSentence struct {
	Text  string
	Links []TraitLink
}

// Keyword is a salient word appearing in the portrait, with 0-based indexes
// into the Sentences array.
type Keyword struct {
	Word      string
	Sentences []int
}

// PortraitSources records which facets each sentence of a portrait rests on,
// plus keywords and (for unified) which notebook-specific portraits it inherits
// from. Derived data: the self-heal rebuilds these after a merge, so merge does
// not copy them.
type PortraitSources struct {
	Sentences []SourcedSentence
	Keywords  []Keyword
	Inherits  map[string]string // unified only: notebook -> that notebook's source text_hash
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
		// Dedupe by (lens, dimension, key), keep stronger strength
		remapped = dedupLinks(remapped)
		// Sort direct first (stable)
		remapped = sortLinksDirectFirst(remapped)
		src.Sentences[i].Links = remapped
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

// dedupLinks keeps one link per (lens, dimension, key), preferring direct
// over partial strength.
func dedupLinks(links []TraitLink) []TraitLink {
	type lk struct{ lens, dimension, key string }
	seen := make(map[lk]TraitLink)
	for _, link := range links {
		k := lk{link.Lens, link.Dimension, link.Key}
		existing, exists := seen[k]
		if !exists {
			seen[k] = link
			continue
		}
		// Keep stronger: direct > partial
		if link.Strength == LinkDirect && existing.Strength != LinkDirect {
			seen[k] = link
		}
	}
	var out []TraitLink
	for _, link := range seen {
		out = append(out, link)
	}
	return out
}

// sortLinksDirectFirst returns links with direct first, then partial, stable.
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
