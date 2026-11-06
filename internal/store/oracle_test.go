package store

// Test-only oracles. They live in a _test.go file so the compiler forbids production use —
// the mine must read the raw count and generation id in ONE statement (ReadRawSnapshot), and
// staging enforces its caps atomically (StageObservationCapped) — while tests keep a second,
// independent read to check those paths against.

// MaxRawID returns the highest raw.id for a session — the session's raw
// "generation". raw.id is INTEGER PRIMARY KEY AUTOINCREMENT, so an id is NEVER
// reused after its row is deleted (that's the AUTOINCREMENT guarantee); a replace-
// import (DELETE + re-INSERT) therefore always yields a strictly higher max id
// than what a worker read before the replace. That property is what makes the
// watermark CAS sound. Returns 0 for a session with no raw rows.
func (r *rawIO) MaxRawID(session string) int64 {
	var id int64
	_ = r.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM raw WHERE session = ?`, session).Scan(&id)
	return id
}

// StagedCount returns how many active observations are currently staged for a session.
func (o *obsIO) StagedCount(session string) int {
	var n int
	_ = o.db.QueryRow(`SELECT COUNT(*) FROM staged WHERE session = ?`, session).Scan(&n)
	return n
}
