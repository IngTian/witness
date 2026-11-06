PRAGMA user_version=8;
CREATE TABLE facet_versions (
  facet_id    INTEGER NOT NULL REFERENCES facets(id) ON DELETE CASCADE,
  pos         INTEGER NOT NULL,
  value       TEXT NOT NULL DEFAULT '',
  valid_from  TEXT NOT NULL DEFAULT '',
  valid_to    TEXT NOT NULL DEFAULT '',
  recorded_at TEXT NOT NULL DEFAULT '',
  because_of  TEXT NOT NULL DEFAULT '[]',
  confidence  REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (facet_id, pos)
);
CREATE TABLE facets (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  lens      TEXT NOT NULL,
  dimension TEXT NOT NULL,
  key       TEXT NOT NULL,
  last_seen TEXT NOT NULL DEFAULT '',
  UNIQUE(lens, dimension, key)
);
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL DEFAULT ''
);
CREATE TABLE observations (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  obs_id      TEXT    NOT NULL UNIQUE,
  ts          TEXT    NOT NULL DEFAULT '',
  session     TEXT    NOT NULL DEFAULT '',
  lens        TEXT    NOT NULL DEFAULT '',
  dimension   TEXT    NOT NULL DEFAULT '',
  observation TEXT    NOT NULL DEFAULT '',
  evidence    TEXT    NOT NULL DEFAULT '',
  poignancy   INTEGER NOT NULL DEFAULT 0,
  source      TEXT    NOT NULL DEFAULT '',
  embedding   BLOB
);
CREATE TABLE progress (
  session      TEXT    NOT NULL,
  lens         TEXT    NOT NULL DEFAULT 'default',
  distilled    INTEGER NOT NULL DEFAULT 0,
  retries      INTEGER NOT NULL DEFAULT 0,
  distilled_at TEXT    NOT NULL DEFAULT '',
  next_attempt TEXT    NOT NULL DEFAULT '',
  drift_at     TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (session, lens)
);
CREATE TABLE raw (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  session TEXT    NOT NULL,
  seq     INTEGER NOT NULL,
  ts      TEXT    NOT NULL DEFAULT '',
  role    TEXT    NOT NULL DEFAULT '',
  effort  TEXT    NOT NULL DEFAULT '',
  text    TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE session_meta (
  session  TEXT PRIMARY KEY,
  cwd      TEXT NOT NULL DEFAULT '',
  started  TEXT NOT NULL DEFAULT '',
  platform TEXT NOT NULL DEFAULT ''
);
CREATE TABLE staged (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  session TEXT NOT NULL,
  obs_id  TEXT NOT NULL DEFAULT '',
  payload TEXT NOT NULL
);
CREATE INDEX idx_obs_lens ON observations(lens);
CREATE INDEX idx_progress_distilled_at ON progress(distilled_at, session);
CREATE INDEX idx_progress_lens_next ON progress(lens, next_attempt);
CREATE INDEX idx_raw_session ON raw(session, id);
CREATE INDEX idx_staged_session ON staged(session);
CREATE UNIQUE INDEX idx_staged_unique ON staged(session, obs_id);
