package store

import (
	"context"
	"database/sql"
)

// ViewNote carries one observation's key facts for the read-only HTML view. It omits
// the embedding (privacy: never exported) and the source field (internal classification).
type ViewNote struct {
	ID, TS, Session, Lens, Dimension, Observation, Evidence string
	Poignancy                                               int
}

// ViewTurn records one raw turn's metadata (session + timestamp) for activity counting.
// Text is never read — the view shows only aggregate turn counts, not turn content.
type ViewTurn struct {
	Session, TS string
}

// ViewInput is everything the view builder needs, read consistently in one transaction.
// Facets includes archived ones (the "rewrite" count needs their version history); Notes
// and Turns are complete (the view filters by session prefix); Log is insertion-ordered;
// Clocks are each lens's observation clock for decayed confidence display.
type ViewInput struct {
	Facets []Facet              // ALL facets, archived included
	Notes  []ViewNote           // every observation, never the embedding
	Turns  []ViewTurn           // every raw row: session + ts only, never text
	Log    []ConfidenceEvent    // ORDER BY id
	Clocks map[string]LensClock // each lens's observation seqs, ascending (the decay clocks)
}

// ReadViewInput loads all view ingredients in ONE read transaction. It never reads turn
// text or observation embeddings (privacy: the view contains neither). The transaction is
// read-only and rolls back unconditionally (never commits), so even a mid-read cancellation
// leaves the store usable. With MaxOpenConns(1), leaking an open tx would hang every future
// call; the defer ensures cleanup on every path.
func (s *Store) ReadViewInput(ctx context.Context) (*ViewInput, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	// Unconditional rollback: read-only tx, no commit ever intended. Ensures cleanup on
	// every exit path (normal return, early error, context cancellation). With
	// MaxOpenConns(1), a leaked tx blocks every later call.
	defer tx.Rollback()

	var in ViewInput

	// Read ALL facets (archived included) — the view needs version history for rewrite counts
	in.Facets, err = readFacetsFrom(ctx, tx, false)
	if err != nil {
		return nil, err
	}

	// Read all observations: never the embedding (privacy: view must not export it). The same
	// pass builds each lens's decay clock; ORDER BY seq keeps every clock ascending.
	in.Clocks = map[string]LensClock{}
	rows, err := tx.QueryContext(ctx,
		`SELECT seq, obs_id, ts, session, lens, dimension, observation, evidence, poignancy
		   FROM observations ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n ViewNote
		var seq int64
		if err := rows.Scan(&seq, &n.ID, &n.TS, &n.Session, &n.Lens, &n.Dimension, &n.Observation, &n.Evidence, &n.Poignancy); err != nil {
			rows.Close()
			return nil, err
		}
		in.Notes = append(in.Notes, n)
		in.Clocks[n.Lens] = append(in.Clocks[n.Lens], seq)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Read all raw rows: session + ts only, never text (privacy: view shows only aggregates)
	rows, err = tx.QueryContext(ctx, `SELECT session, ts FROM raw ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var turn ViewTurn
		if err := rows.Scan(&turn.Session, &turn.TS); err != nil {
			rows.Close()
			return nil, err
		}
		in.Turns = append(in.Turns, turn)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Read confidence log, ORDER BY id (insertion order)
	in.Log, err = readConfidenceLog(ctx, tx)
	if err != nil {
		return nil, err
	}

	return &in, nil
}
