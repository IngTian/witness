package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// CleanupWitnessDistillSessions removes private OpenCode sessions created only
// for witness distillation. It keys on structured session columns, never message
// text, so it cannot match a user's normal conversation by content.
func CleanupWitnessDistillSessions(ctx context.Context, dbPath string, before time.Time) (int64, error) {
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		var err error
		dbPath, err = DefaultDBPath()
		if err != nil {
			return 0, err
		}
	}
	db, err := sql.Open("sqlite", sqliteWriteURI(dbPath))
	if err != nil {
		return 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON`); err != nil {
		return 0, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// The self-traffic predicate is shared with the import filter (which negates it),
	// so read and delete can never disagree. Agent-authoritative when the column
	// exists; title fallback only for older schemas.
	where, args := selfTrafficWhere(hasSessionColumn(ctx, tx, "agent"))
	if !before.IsZero() {
		where += ` AND time_created < ?`
		args = append(args, before.UTC().UnixMilli())
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE witness_cleanup_ids(id TEXT PRIMARY KEY)`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO witness_cleanup_ids SELECT id FROM session WHERE `+where, args...); err != nil {
		return 0, err
	}

	for _, table := range []string{"part", "message", "session_message", "session_input", "session_context_epoch", "session_share", "todo"} {
		if hasTableColumn(ctx, tx, table, "session_id") {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE session_id IN (SELECT id FROM witness_cleanup_ids)`, table)); err != nil {
				return 0, err
			}
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM session WHERE id IN (SELECT id FROM witness_cleanup_ids)`)
	if err != nil {
		return 0, err
	}
	deleted, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

func sqliteWriteURI(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("cache", "shared")
	u.RawQuery = q.Encode()
	return u.String()
}

func hasSessionColumn(ctx context.Context, tx *sql.Tx, column string) bool {
	return hasTableColumn(ctx, tx, "session", column)
}

func hasTableColumn(ctx context.Context, tx *sql.Tx, table, column string) bool {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
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
		if name == column {
			return true
		}
	}
	return false
}
