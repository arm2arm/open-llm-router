#!/usr/bin/env python3
import importlib.util
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("pinned_updates", ROOT / "deploy/release/pinned_updates.py")
updates = importlib.util.module_from_spec(spec)
spec.loader.exec_module(updates)


class PinnedUpdatesTests(unittest.TestCase):
    def test_repository_inventory_covers_each_actual_pin(self):
        images, tools = updates.inventory(ROOT)
        self.assertTrue(any("postgres:" in ref for ref in images))
        self.assertTrue(any("vllm-openai-cpu" in ref for ref in images))
        self.assertEqual(set(module for module, _ in tools), set(updates.TOOLS))

    def test_new_pins_need_an_update_source(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "Makefile").write_text("")
            (root / "Dockerfile").write_text("FROM example/unknown:1@sha256:" + "a" * 64)
            with self.assertRaisesRegex(ValueError, "Untracked pinned image"):
                updates.inventory(root)
            (root / "Dockerfile").write_text("")
            (root / "Makefile").write_text("go run example.org/tool@v1.0.0")
            with self.assertRaisesRegex(ValueError, "Untracked pinned tool"):
                updates.inventory(root)

    def test_postgres_backports_are_visible_without_a_new_major(self):
        tags = [dict(name=name) for name in ["REL_19_BETA1", "REL_19_1", "REL_17_8", "REL_17_9"]]
        self.assertEqual(updates.latest("postgres/postgres", lambda _: tags), "REL_19_1, REL_17_9")

    def test_module_tags_sort_numerically(self):
        tags = [dict(name=name) for name in ["v1.9.0", "v1.10.0", "v2.0.0-rc1"]]
        self.assertEqual(updates.latest("golang/vuln", lambda _: tags), "v1.10.0")

    def test_upstream_failure_cannot_publish_an_incomplete_report(self):
        def failed(_):
            raise RuntimeError("upstream unavailable")
        with self.assertRaisesRegex(RuntimeError, "upstream unavailable"):
            updates.review(ROOT, failed)

    def test_latest_release_rejects_prereleases(self):
        with self.assertRaises(ValueError):
            updates.latest("grafana/grafana", lambda _: dict(tag_name="v99-rc1", prerelease=True))

    def issue(self, number=1, state="open", digest="previous"):
        return dict(number=number, title=updates.TITLE, state=state,
                    body=f"{updates.MARKER}{digest} -->", user=dict(type="Bot"))

    def publish(self, issues, digest="new"):
        calls = []
        def fetch(endpoint, method="GET", payload=None):
            calls.append((endpoint, method, payload))
            return issues if method == "GET" else {}
        result = updates.publish("owner/repo", digest, "review body", fetch)
        return result, calls

    def test_new_release_opens_one_issue(self):
        result, calls = self.publish([])
        self.assertEqual(result, "opened review")
        self.assertEqual(calls[-1][1:], ("POST", dict(title=updates.TITLE, body="review body")))

    def test_next_release_updates_existing_open_review(self):
        result, calls = self.publish([self.issue(number=42)])
        self.assertEqual(result, "updated existing review")
        self.assertTrue(calls[-1][0].endswith("/42"))
        self.assertEqual(calls[-1][1], "PATCH")

    def test_reviewed_or_current_snapshot_does_not_reopen_or_duplicate(self):
        for state in ["open", "closed"]:
            result, calls = self.publish([self.issue(state=state, digest="new")])
            self.assertEqual(result, "already reviewed or tracked")
            self.assertEqual(len(calls), 1)


if __name__ == "__main__":
    unittest.main()
