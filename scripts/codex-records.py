#!/usr/bin/env python3
"""Read-only Codex capture experiment for issue #162 (Python standard library).

stdout is PRIVATE NDJSON for witness ingest; --probe emits only counts and field
names. Never reads config.toml/auth.json or sends data to a model or service.
"""
import argparse
import datetime
import json
import os
from pathlib import Path
import sqlite3
import sys


def connect(path):
    return sqlite3.connect(path.resolve().as_uri() + "?mode=ro", uri=True)


def records(home, session):
    db_path = home / "thread_history_1.sqlite"
    if db_path.exists():
        with connect(db_path) as db:
            found = db.execute("SELECT 1 FROM thread_items WHERE thread_id=? LIMIT 1", (session,)).fetchone()
            if found:
                for item_id, ms, body in db.execute(
                    "SELECT item_id, created_at_ms, item_json FROM thread_items "
                    "WHERE thread_id=? AND item_type IN ('userMessage','agentMessage') "
                    "ORDER BY rollout_ordinal, item_id", (session,)
                ):
                    item = json.loads(body)
                    if item["type"] == "userMessage":
                        role = "user"
                        text = "\n".join(c["text"] for c in item.get("content", []) if c.get("type") == "text")
                    else:
                        role, text = "assistant", item.get("text", "")
                    if text.strip():
                        yield {"id": f"codex:{session}:{item_id}", "session": f"codex:{session}",
                               "ts": datetime.datetime.fromtimestamp(ms / 1000, datetime.timezone.utc).isoformat(),
                               "role": role, "text": text}
                return
    for folder in ("sessions", "archived_sessions"):
        for path in sorted((home / folder).rglob("*.jsonl")):
            if session not in path.name:
                continue
            with path.open("rb") as stream:
                while True:
                    offset = stream.tell()
                    line = stream.readline()
                    if not line or not line.endswith(b"\n"):
                        break  # incomplete live tail is retried next time
                    row = json.loads(line)
                    if row.get("type") != "response_item":
                        continue
                    item = row.get("payload", {})
                    if item.get("type") != "message" or item.get("role") not in ("user", "assistant"):
                        continue
                    text = "\n".join(c["text"] for c in item.get("content", [])
                                     if c.get("type") in ("input_text", "output_text"))
                    if text.strip():
                        item_id = item.get("id") or f"offset-{offset}"
                        yield {"id": f"codex:{session}:{item_id}", "session": f"codex:{session}",
                               "ts": row["timestamp"], "role": item["role"], "text": text}
            return
    raise ValueError("session not found in supported local history")


def probe(home):
    paths = [p for folder in ("sessions", "archived_sessions") for p in (home / folder).rglob("*.jsonl")]
    out = {"jsonl_files": len(paths), "jsonl_bytes": sum(p.stat().st_size for p in paths)}
    db_path = home / "thread_history_1.sqlite"
    if db_path.exists():
        with connect(db_path) as db:
            out["projected_threads"] = db.execute("SELECT COUNT(DISTINCT thread_id) FROM thread_items").fetchone()[0]
            out["item_types"] = dict(db.execute("SELECT item_type, COUNT(*) FROM thread_items GROUP BY item_type"))
            out["message_fields"] = {}
            for kind in ("userMessage", "agentMessage"):
                row = db.execute("SELECT item_json FROM thread_items WHERE item_type=? LIMIT 1", (kind,)).fetchone()
                if row:
                    out["message_fields"][kind] = sorted(json.loads(row[0]))
    return out


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--home", type=Path, default=Path(os.environ.get("CODEX_HOME", Path.home() / ".codex")))
    parser.add_argument("--session", help="native session id; required to export private message text")
    parser.add_argument("--probe", action="store_true", help="safe counts and field names only")
    args = parser.parse_args()
    if args.probe:
        print(json.dumps(probe(args.home), indent=2))
    elif args.session:
        for record in records(args.home, args.session.removeprefix("codex:")):
            print(json.dumps(record, ensure_ascii=False))
    else:
        parser.error("choose --probe or --session")


if __name__ == "__main__":
    sys.stdout.reconfigure(encoding="utf-8")
    main()
