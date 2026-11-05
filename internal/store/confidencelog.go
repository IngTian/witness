package store

// Event kinds for confidence log entries.
const (
	EventNew        = "new"
	EventReopen     = "reopen"
	EventRewrite    = "rewrite"
	EventReinforce  = "reinforce"
	EventDowngrade  = "downgrade"
	EventCompaction = "compaction"
)

// ConfidenceEvent records one confidence change for a facet. At holds the run's
// timestamp (RFC3339 UTC); ClockSeq is the version's ConfidenceSeq AFTER the event;
// From is what confidence was before this event; To is what was stored after; Kind
// names the reason (new/reopen/rewrite/reinforce/downgrade/compaction).
type ConfidenceEvent struct {
	Lens, Dimension, Key string
	At                   string  // RFC3339 UTC — the run's time
	ClockSeq             int64   // the version's ConfidenceSeq after the event
	From, To             float64 // From: what the step started from; To: stored after
	Kind                 string
}

// WriteFacetsLogged writes facets and confidence events in ONE transaction. It is the
// logged variant of WriteFacets: the facets table is replaced just as in WriteFacets,
// and the events are appended to confidence_log. If events is nil or empty, no log
// rows are written. The transaction rolls back on any error, so either both succeed
// or neither is persisted.
func (f *facetIO) WriteFacetsLogged(facets []Facet, events []ConfidenceEvent) error {
	tx, err := f.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// Write facets (same full-table replace as WriteFacets)
	if err = writeFacetsTx(tx, facets); err != nil {
		return err
	}

	// Write confidence events if any
	if len(events) > 0 {
		stmt, err := tx.Prepare(
			`INSERT INTO confidence_log(lens, dimension, key, at, clock_seq, from_conf, to_conf, kind)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		for _, ev := range events {
			if _, err := stmt.Exec(ev.Lens, ev.Dimension, ev.Key, ev.At, ev.ClockSeq, ev.From, ev.To, ev.Kind); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// ReadConfidenceLog reads all confidence events, ordered by id (insertion order).
func (s *Store) ReadConfidenceLog() ([]ConfidenceEvent, error) {
	rows, err := s.db.Query(`SELECT lens, dimension, key, at, clock_seq, from_conf, to_conf, kind
	                         FROM confidence_log ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []ConfidenceEvent
	for rows.Next() {
		var ev ConfidenceEvent
		if err := rows.Scan(&ev.Lens, &ev.Dimension, &ev.Key, &ev.At, &ev.ClockSeq, &ev.From, &ev.To, &ev.Kind); err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}
