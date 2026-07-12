package opencode

import (
	"context"
	"database/sql"
)

// MarkerName is the single identifier witness stamps on its OWN distillation
// sessions in OpenCode, so they are never re-ingested as user sessions. It is set
// as BOTH the session's agent and (legacy) title at creation — see server.go
// createSession. This one const replaces the three former declarations
// (witnessDistillTitle, openCodeAgentName, and a bare literal).
const MarkerName = "witness-distill"

// selfTrafficWhere builds the SQL predicate that matches witness's own distill
// sessions. AGENT is authoritative: witness sets agent=MarkerName at creation and
// OpenCode never rewrites it, whereas OpenCode's auto-titler CAN overwrite a
// session's title after the fact. So when the session table has an `agent` column
// we key on it alone; the title match is a fallback ONLY for older OpenCode schemas
// with no agent column.
//
// Both sides of the self-traffic contract build from this one predicate so they can
// never diverge: cleanup uses it directly (DELETE these), import NEGATES it (skip
// these) — see the callers. hasAgent is resolved once per query from the live
// schema (hasSessionColumn), keeping this pure/testable.
func selfTrafficWhere(hasAgent bool) (clause string, args []any) {
	if hasAgent {
		return `agent = ?`, []any{MarkerName}
	}
	return `title = ?`, []any{MarkerName}
}

// sessionHasAgentColumn reports whether the OpenCode session table has an `agent`
// column, on a plain *sql.DB (the import path) rather than a tx. Mirrors
// hasSessionColumn, which operates on a *sql.Tx (the cleanup path).
func sessionHasAgentColumn(ctx context.Context, db *sql.DB) bool {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(session)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return false
		}
		if name == "agent" {
			return true
		}
	}
	return false
}
