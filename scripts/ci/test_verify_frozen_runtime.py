#!/usr/bin/env python3
import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "release"))
from verify_frozen_runtime import verify


class FrozenRuntimeVerificationTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.manifest = {
            "runtime_version": "v0.0.25",
            "build_commit": "7b95eae7ca89c3b99decd989d04d18a40fa2b9a1",
            "core_schema_min_required": "31",
            "core_schema_max_supported": "31",
        }
        self.pin = {
            "version": "v0.0.25",
            "source_revision": self.manifest["build_commit"],
            "candidate_run_id": 36104220018,
            "candidate_index_digest": "sha256:" + "a" * 64,
            "candidate_linux_amd64_digest": "sha256:" + "b" * 64,
        }
        self.candidate = {
            "schema_version": 2,
            "kind": "mcp2026-runtime-successor-candidate",
            "artifact_role": "C",
            "producer": {
                "repository": "dsmolchanov/nerve-oss",
                "ref": "refs/heads/main",
                "workflow": ".github/workflows/docker-publish.yml",
                "run_id": self.pin["candidate_run_id"],
            },
            "source_revision": self.pin["source_revision"],
            "image": "ghcr.io/dsmolchanov/nerve-runtime@" + self.pin["candidate_index_digest"],
            "index_digest": self.pin["candidate_index_digest"],
            "linux_amd64_digest": self.pin["candidate_linux_amd64_digest"],
        }

    def check(self):
        files = [self.root / name for name in ("pin.json", "candidate.json", "manifest.json")]
        files[2].write_text(json.dumps(self.manifest))
        digest = hashlib.sha256(files[2].read_bytes()).hexdigest()
        self.pin["candidate_manifest_sha256"] = digest
        self.candidate["manifest_sha256"] = digest
        files[0].write_text(json.dumps(self.pin))
        files[1].write_text(json.dumps(self.candidate))
        return verify(*files)

    def test_accepts_exact_candidate(self):
        self.assertEqual(self.check()["version"], "v0.0.25")

    def test_rejects_changed_candidate_source_and_digest(self):
        for field, value in (
            ("source_revision", "c" * 40),
            ("index_digest", "sha256:" + "c" * 64),
            ("linux_amd64_digest", "sha256:" + "c" * 64),
        ):
            with self.subTest(field=field):
                original = self.candidate[field]
                self.candidate[field] = value
                with self.assertRaises(ValueError):
                    self.check()
                self.candidate[field] = original

    def test_rejects_wrong_role_producer_and_schema_window(self):
        self.candidate["artifact_role"] = "B"
        with self.assertRaises(ValueError):
            self.check()
        self.candidate["artifact_role"] = "C"
        self.candidate["producer"]["run_id"] += 1
        with self.assertRaises(ValueError):
            self.check()
        self.candidate["producer"]["run_id"] -= 1
        self.manifest["core_schema_max_supported"] = "32"
        with self.assertRaises(ValueError):
            self.check()


if __name__ == "__main__":
    unittest.main()
