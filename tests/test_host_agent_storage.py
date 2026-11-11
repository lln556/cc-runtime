"""Mailbox confinement and descriptor lifetime tests; no live host tasks."""
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "host-agent/src"))
from storage import Mailbox


class StorageTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.files = Mailbox(self.root / "mailbox")
        self.files.mkdir(".")

    def test_json_round_trip_preserves_unicode_and_cleans_failed_writes(self):
        self.files.atomic("jobs/test/status.json", {"text": "测试"})
        self.assertEqual(self.files.read("jobs/test/status.json"), {"text": "测试"})
        with self.assertRaises(TypeError):
            self.files.atomic("jobs/test/status.json", {"invalid": object()})
        self.assertEqual(self.files.read("jobs/test/status.json"), {"text": "测试"})
        self.assertEqual(list((self.files.root / "jobs/test").glob(".tmp-*")), [])

    def test_root_symlink_and_path_escape_are_refused(self):
        alias = self.root / "alias"
        alias.symlink_to(self.files.root)
        with self.assertRaises(OSError):
            Mailbox(alias).atomic("status.json", {})
        for path in ("../outside.json", self.files.root / "../outside.json", self.root / "outside.json"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                self.files.atomic(path, {})
        self.assertFalse((self.root / "outside.json").exists())

    def test_read_refuses_symlinks_hardlinks_and_special_files(self):
        target = self.root / "private.json"
        target.write_text('{"private": true}')
        (self.files.root / "symlink.json").symlink_to(target)
        os.link(target, self.files.root / "hardlink.json")
        os.mkfifo(self.files.root / "fifo.json")
        (self.files.root / "directory.json").mkdir()
        for name in ("symlink.json", "hardlink.json", "fifo.json", "directory.json"):
            with self.subTest(name=name), self.assertRaises((OSError, ValueError)):
                self.files.read(name)

    def test_read_refuses_a_file_changed_to_symlink_at_open(self):
        self.files.atomic("status.json", {"public": True})
        target = self.root / "private.json"
        target.write_text('{"private": true}')
        original_open = os.open
        swapped = []

        def swap(path, flags, *args, **kwargs):
            if path == "status.json" and not swapped:
                swapped.append(True)
                (self.files.root / path).unlink()
                (self.files.root / path).symlink_to(target)
            return original_open(path, flags, *args, **kwargs)

        with patch("os.open", side_effect=swap), self.assertRaises(OSError):
            self.files.read("status.json")
        self.assertTrue(swapped)

    def test_atomic_replace_does_not_modify_destination_link_targets(self):
        for kind in ("symlink", "hardlink"):
            with self.subTest(kind=kind):
                target = self.root / (kind + "-private.json")
                target.write_text("host-only")
                destination = self.files.root / (kind + ".json")
                if kind == "symlink":
                    destination.symlink_to(target)
                else:
                    os.link(target, destination)
                self.files.atomic(destination, {"public": True})
                self.assertEqual(target.read_text(), "host-only")
                self.assertEqual(self.files.read(destination), {"public": True})

    def test_json_reads_are_bounded_and_require_objects(self):
        for value in ([], "x" * (self.files.MAX_JSON + 1)):
            with self.subTest(value_type=type(value).__name__):
                self.files.atomic("status.json", value)
                with self.assertRaises(ValueError):
                    self.files.read("status.json")

    def test_event_log_stays_on_opened_file_when_path_is_replaced(self):
        target = self.root / "host.log"
        target.write_text("host-only\n")
        path = self.files.root / "events.jsonl"
        with self.files.event_log(path) as stream:
            self.files.append_event(stream, {"event": 1})
            path.rename(path.with_suffix(".original"))
            path.symlink_to(target)
            self.files.append_event(stream, {"event": 2})
        self.assertEqual(target.read_text(), "host-only\n")
        events = path.with_suffix(".original").read_text().splitlines()
        self.assertEqual([json.loads(line) for line in events], [{"event": 1}, {"event": 2}])

    def test_event_log_refuses_hardlink_added_after_open(self):
        path = self.files.root / "events.jsonl"
        with self.files.event_log(path) as stream:
            os.link(path, self.root / "host.log")
            with self.assertRaises(ValueError):
                self.files.append_event(stream, {"event": "not written"})
        self.assertEqual((self.root / "host.log").read_text(), "")

    def test_queue_listing_and_quarantine_do_not_follow_file_links(self):
        target = self.root / "host.json"
        target.write_text("host-only")
        path = self.files.root / "linked.json"
        path.symlink_to(target)
        self.assertEqual(self.files.json_files("."), [path])
        self.files.invalidate(path)
        quarantined = path.with_suffix(".invalid")
        self.assertTrue(quarantined.is_symlink())
        self.files.unlink(quarantined)
        self.assertFalse(quarantined.is_symlink())
        self.assertEqual(target.read_text(), "host-only")


if __name__ == "__main__":
    unittest.main()
