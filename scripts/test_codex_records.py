"""Regression coverage for the read-only ingest converter's EOF handling."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("codex_records", Path(__file__).with_name("codex-records.py"))
converter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(converter)


class RolloutEOFTests(unittest.TestCase):
    def test_complete_and_partial_final_records(self):
        header = json.dumps({"type": "session_meta", "payload": {"id": "sample"}}) + "\n"
        def message(text):
            return json.dumps({"type": "response_item", "timestamp": "2026-10-31T12:00:00Z",
                               "payload": {"type": "message", "role": "user",
                                           "content": [{"type": "input_text", "text": text}]}})
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            folder = home / "sessions"
            folder.mkdir()
            path = folder / "rollout-sample.jsonl"
            path.write_text(header + message("first") + "\n" + message("last"), encoding="utf-8")
            self.assertEqual([r["text"] for r in converter.records(home, "sample")], ["first", "last"])
            path.write_text(header + message("first") + "\n" + message("last")[:-3], encoding="utf-8")
            self.assertEqual([r["text"] for r in converter.records(home, "sample")], ["first"])


if __name__ == "__main__":
    unittest.main()
