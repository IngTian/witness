package store

import (
	"database/sql"
	"encoding/json"
)

// facetIO is the L2 facet concern: the bi-temporal profile stored relationally in
// facets + facet_versions. The reviewer is the sole writer (WriteFacets); everything
// else reads (ReadFacets). A DB leaf — holds only the shared *sql.DB.
type facetIO struct{ db *sql.DB }

// ReadFacets loads the L2 profile (all facets across all lenses), in a
// deterministic order (lens, dimension, key) so the profile doesn't churn.
func (f *facetIO) ReadFacets() ([]Facet, error) {
	rows, err := f.db.Query(
		`SELECT f.id, f.lens, f.dimension, f.key, f.last_seen,
		        v.value, v.valid_from, v.valid_to, v.recorded_at, v.because_of, v.confidence
		   FROM facets f
		   LEFT JOIN facet_versions v ON v.facet_id = f.id
		  ORDER BY f.lens, f.dimension, f.key, v.pos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Facet
	var curID int64 = -1
	for rows.Next() {
		var (
			id                                int64
			lens, dim, key, lastSeen          string
			value, vFrom, vTo, recAt, because *string
			confidence                        *float64
		)
		if err := rows.Scan(&id, &lens, &dim, &key, &lastSeen,
			&value, &vFrom, &vTo, &recAt, &because, &confidence); err != nil {
			return nil, err
		}
		if id != curID {
			out = append(out, Facet{Lens: lens, Dimension: dim, Key: key, LastSeen: lastSeen})
			curID = id
		}
		if value == nil { // LEFT JOIN with no versions
			continue
		}
		fv := FacetVersion{
			Value: *value, ValidFrom: deref(vFrom), ValidTo: deref(vTo),
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

// WriteFacets atomically replaces the L2 profile. Only the reviewer calls this.
// The whole rewrite runs in one transaction; foreign_keys=ON cascades the old
// versions when their facets are deleted.
func (f *facetIO) WriteFacets(facets []Facet) error {
	tx, err := f.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM facets`); err != nil {
		tx.Rollback()
		return err
	}
	fStmt, err := tx.Prepare(`INSERT INTO facets(lens, dimension, key, last_seen) VALUES (?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer fStmt.Close()
	vStmt, err := tx.Prepare(
		`INSERT INTO facet_versions(facet_id, pos, value, valid_from, valid_to, recorded_at, because_of, confidence)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer vStmt.Close()
	for _, fc := range facets {
		res, err := fStmt.Exec(fc.Lens, fc.Dimension, fc.Key, fc.LastSeen)
		if err != nil {
			tx.Rollback()
			return err
		}
		id, _ := res.LastInsertId()
		for pos, v := range fc.Versions {
			because, _ := json.Marshal(v.BecauseOf)
			if v.BecauseOf == nil {
				because = []byte("[]")
			}
			if _, err := vStmt.Exec(id, pos, v.Value, v.ValidFrom, v.ValidTo, v.RecordedAt,
				string(because), v.Confidence); err != nil {
				tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
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
