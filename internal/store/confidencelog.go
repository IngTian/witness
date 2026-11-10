package store

import "context"

// Event kinds for confidence log entries.
const (
	EventNew        = "new"
	EventReopen     = "reopen"
	EventRewrite    = "rewrite"
	EventReinforce  = "reinforce"
	EventDowngrade  = "downgrade"
	EventCompaction = "compaction"
	// EventRetire: a review retired the facet because no observation it cited is in the archive
	// any more. Its own kind, not "compaction" (no model judged anything) and not "downgrade"
	// (confidence did not move: From is the decayed value it held, To the stored one, unchanged).
	// Every other kind is a CONFIRMED value, which is how the view draws log rows, so the view
	// skips this one (view/traits.go); page.js never sees kinds.
	EventRetire = "retire"
)

// ConfidenceEvent records one confidence change for a facet. At holds the run's
// timestamp (RFC3339 UTC); ClockSeq is the version's ConfidenceSeq AFTER the event;
// From is what confidence was before this event; To is what was stored after; Kind
// names the reason (new/reopen/rewrite/reinforce/downgrade/compaction/retire).
type ConfidenceEvent struct {
	Lens, Dimension, Key string
	At                   string  // RFC3339 UTC — the run's time
	ClockSeq             int64   // the version's ConfidenceSeq after the event
	From, To             float64 // From: what the step started from; To: stored after
	Kind                 string
}

// ReadConfidenceLog reads all confidence events, ordered by id (insertion order).
func (s *Store) ReadConfidenceLog() ([]ConfidenceEvent, error) {
	return readConfidenceLog(context.Background(), s.db)
}

// readConfidenceLog is ReadConfidenceLog on either a *sql.DB or a *sql.Tx, so ReadViewInput
// reads the log inside its one transaction with the same query and scan.
func readConfidenceLog(ctx context.Context, q rowQuerier) ([]ConfidenceEvent, error) {
	rows, err := q.QueryContext(ctx, `SELECT lens, dimension, key, at, clock_seq, from_conf, to_conf, kind
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
