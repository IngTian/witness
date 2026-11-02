package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// facetIO is the L2 facet concern: the bi-temporal profile stored relationally in
// facets + facet_versions. The reviewer is the sole writer (WriteFacets); everything
// else reads (ReadFacets). A DB leaf — holds only the shared *sql.DB.
type facetIO struct{ db *sql.DB }

// ReadFacets loads the L2 profile (all facets across all lenses, archived ones INCLUDED), in
// a deterministic order (lens, dimension, key) so the profile doesn't churn.
//
// This is the whole-history read: export, `witness facets --all` and the trajectory expansion
// need archived facets, and — critically — so does every writer, because WriteFacets replaces
// the entire table and would DROP any facet missing from the slice it is handed. Callers that
// want the live profile (the review prompt, the portrait, MCP) want ReadActiveFacets.
func (f *facetIO) ReadFacets() ([]Facet, error) { return f.readFacets(false) }

// ReadActiveFacets loads only the live profile — the derived compact set that feeds the model,
// the narrative and MCP. Archived facets are excluded along with their versions, so the cost
// of a long compaction history is not paid on every read.
//
// It must NOT be used to assemble a WriteFacets argument: that would delete every archived
// facet and destroy exactly the history compaction is designed to keep.
func (f *facetIO) ReadActiveFacets() ([]Facet, error) { return f.readFacets(true) }

func (f *facetIO) readFacets(activeOnly bool) ([]Facet, error) {
	return readFacetsFrom(context.Background(), f.db, activeOnly)
}

// rowQuerier is what reading needs from either a *sql.DB or a *sql.Tx. The merge reads this
// archive's facets INSIDE its transaction, so a facet change that lands while it runs either
// precedes the read or waits for the commit — read outside, a concurrent write (a
// delete_observation resetting a review cursor) would be silently overwritten by the merge.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readFacetsFrom(ctx context.Context, q rowQuerier, activeOnly bool) ([]Facet, error) {
	where := ""
	if activeOnly {
		where = ` WHERE f.archived_at = ''`
	}
	rows, err := q.QueryContext(ctx,
		`SELECT f.id, f.lens, f.dimension, f.key, f.last_seen, f.archived_at, f.merged_into, f.archive_reason,
		        v.value, v.claim, v.confidence_seq, v.valid_from, v.valid_to, v.recorded_at, v.because_of, v.confidence
		   FROM facets f
		   LEFT JOIN facet_versions v ON v.facet_id = f.id`+where+`
		  ORDER BY f.lens, f.dimension, f.key, v.pos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Facet
	var curID int64 = -1
	for rows.Next() {
		var (
			id                                       int64
			lens, dim, key, lastSeen                 string
			archivedAt, mergedInto, reason           string
			value, claim, vFrom, vTo, recAt, because *string
			confSeq                                  *int64
			confidence                               *float64
		)
		if err := rows.Scan(&id, &lens, &dim, &key, &lastSeen, &archivedAt, &mergedInto, &reason,
			&value, &claim, &confSeq, &vFrom, &vTo, &recAt, &because, &confidence); err != nil {
			return nil, err
		}
		if id != curID {
			out = append(out, Facet{
				Lens: lens, Dimension: dim, Key: key, LastSeen: lastSeen,
				ArchivedAt: archivedAt, MergedInto: mergedInto, ArchiveReason: reason,
			})
			curID = id
		}
		if value == nil { // LEFT JOIN with no versions
			continue
		}
		fv := FacetVersion{
			Value: *value, Claim: deref(claim), ConfidenceSeq: derefI(confSeq), ValidFrom: deref(vFrom), ValidTo: deref(vTo),
			RecordedAt: deref(recAt), Confidence: derefF(confidence),
		}
		if because != nil {
			_ = json.Unmarshal([]byte(*because), &fv.BecauseOf)
		}
		fc := &out[len(out)-1]
		fc.Versions = append(fc.Versions, fv)
	}
	return out, rows.Err()
}

// WriteFacets atomically replaces the L2 profile. The reviewer and the compaction pass are
// its only callers. The whole rewrite runs in one transaction; foreign_keys=ON cascades the old
// versions when their facets are deleted.
//
// It is a REPLACE, so the slice must be the complete profile — every lens, archived facets
// included. Hand it the output of ReadActiveFacets and it will delete every archived facet and
// every other lens.
func (f *facetIO) WriteFacets(facets []Facet) error {
	tx, err := f.db.Begin()
	if err != nil {
		return err
	}
	if err := writeFacetsTx(tx, facets); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// writeFacetsTx is WriteFacets inside a caller's transaction: the same whole-table REPLACE, so the
// same rule holds — the slice must be the complete profile. It exists for the merge, which must
// write L2 in the same transaction as the L0/L1 rows the facets cite, or a crash between the two
// leaves facets citing observations that never arrived.
func writeFacetsTx(tx *sql.Tx, facets []Facet) error {
	if _, err := tx.Exec(`DELETE FROM facets`); err != nil {
		return err
	}
	fStmt, err := tx.Prepare(
		`INSERT INTO facets(lens, dimension, key, last_seen, archived_at, merged_into, archive_reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer fStmt.Close()
	vStmt, err := tx.Prepare(
		`INSERT INTO facet_versions(facet_id, pos, value, claim, confidence_seq, valid_from, valid_to, recorded_at, because_of, confidence)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer vStmt.Close()
	for _, fc := range facets {
		// archived_at/merged_into ride through on every write. The reviewer reads the whole
		// profile and writes it back each fold, so a column dropped here would be erased on
		// the next review rather than at compaction time — a silent, delayed data loss.
		res, err := fStmt.Exec(fc.Lens, fc.Dimension, fc.Key, fc.LastSeen, fc.ArchivedAt, fc.MergedInto, fc.ArchiveReason)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		for pos, v := range fc.Versions {
			because, _ := json.Marshal(v.BecauseOf)
			if v.BecauseOf == nil {
				because = []byte("[]")
			}
			if _, err := vStmt.Exec(id, pos, v.Value, v.Claim, v.ConfidenceSeq, v.ValidFrom, v.ValidTo, v.RecordedAt,
				string(because), v.Confidence); err != nil {
				return err
			}
		}
	}
	return nil
}

// FacetTrajectory expands ONE active facet into the full history behind it: the facets that
// were merged into it, the facets merged into those, and so on. The returned slice is
// breadth-first from the survivor outward and carries every tributary's complete version
// history, which is what makes a compact profile honest — a node in the live set can always
// be unfolded into the evidence and the wording it came from.
//
// The survivor itself is NOT included; the caller already has it. An unknown or
// never-absorbing facet yields nil, which is the correct answer (nothing was folded in).
//
// ponytail: resolves in Go over a whole-profile read rather than a recursive SQL CTE. At the
// measured scale (2,039 facets) that is microseconds and needs no new query to reason about;
// if an archive ever reaches a scale where reading all facets to expand one is the bottleneck,
// the fix is a recursive CTE on (lens, merged_into), which the idx_facets_active index does
// NOT serve.
func (f *facetIO) FacetTrajectory(lens, dimension, key string) ([]Facet, error) {
	all, err := f.ReadFacets()
	if err != nil {
		return nil, err
	}
	// merged_into -> the facets naming it. Built once; a facet can absorb many.
	children := make(map[string][]Facet)
	for _, fc := range all {
		if fc.Lens != lens || fc.MergedInto == "" {
			continue
		}
		children[fc.MergedInto] = append(children[fc.MergedInto], fc)
	}
	var out []Facet
	seen := map[string]bool{}
	queue := []string{FacetRef(dimension, key)}
	for len(queue) > 0 {
		ref := queue[0]
		queue = queue[1:]
		if seen[ref] { // a cycle would otherwise spin forever; see mergeCycles in distill
			continue
		}
		seen[ref] = true
		for _, child := range children[ref] {
			out = append(out, child)
			queue = append(queue, FacetRef(child.Dimension, child.Key))
		}
	}
	return out, nil
}

// LensFacetCounts reports how many facets a lens has active vs archived — the one-line answer
// to "is compaction working" that doctor and the compact command both want, without loading
// every value.
func (f *facetIO) LensFacetCounts(lens string) (active, archived int) {
	_ = f.db.QueryRow(
		`SELECT COUNT(*) FROM facets WHERE lens = ? AND archived_at = ''`, lens).Scan(&active)
	_ = f.db.QueryRow(
		`SELECT COUNT(*) FROM facets WHERE lens = ? AND archived_at != ''`, lens).Scan(&archived)
	return active, archived
}

func derefI(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func derefF(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
