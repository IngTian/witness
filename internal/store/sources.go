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
