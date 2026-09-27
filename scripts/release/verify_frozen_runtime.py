#!/usr/bin/env python3
"""Validate the reviewed v0.0.25 pin against the downloaded candidate artifact."""
import argparse
import hashlib
import json
from pathlib import Path
import re


def require(condition, reason):
    if not condition:
        raise ValueError(reason)


def verify(pin_path: Path, candidate_path: Path, manifest_path: Path):
    pin = json.loads(pin_path.read_text())
    candidate = json.loads(candidate_path.read_text())
    manifest_bytes = manifest_path.read_bytes()
    manifest = json.loads(manifest_bytes)
    require(re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", pin["version"]), "invalid version")
    require(re.fullmatch(r"[0-9a-f]{40}", pin["source_revision"]), "invalid source revision")
    require(re.fullmatch(r"sha256:[0-9a-f]{64}", pin["candidate_index_digest"]), "invalid index digest")
    require(re.fullmatch(r"sha256:[0-9a-f]{64}", pin["candidate_linux_amd64_digest"]), "invalid platform digest")
    require(re.fullmatch(r"[0-9a-f]{64}", pin["candidate_manifest_sha256"]), "invalid manifest digest")
    require(candidate["schema_version"] == 2 and candidate["kind"] == "mcp2026-runtime-successor-candidate" and candidate["artifact_role"] == "C", "candidate contract differs")
    require(candidate["producer"] == {"repository": "dsmolchanov/nerve-oss", "ref": "refs/heads/main", "workflow": ".github/workflows/docker-publish.yml", "run_id": pin["candidate_run_id"]}, "candidate producer differs")
    require(candidate["source_revision"] == pin["source_revision"], "candidate source differs")
    require(candidate["image"] == "ghcr.io/dsmolchanov/nerve-runtime@" + pin["candidate_index_digest"], "candidate image differs")
    require(candidate["index_digest"] == pin["candidate_index_digest"] and candidate["linux_amd64_digest"] == pin["candidate_linux_amd64_digest"], "candidate digest differs")
    require(candidate["manifest_sha256"] == pin["candidate_manifest_sha256"] == hashlib.sha256(manifest_bytes).hexdigest(), "candidate manifest hash differs")
    require(manifest["runtime_version"] == pin["version"] and manifest["build_commit"] == pin["source_revision"], "embedded runtime identity differs")
    require(manifest["core_schema_min_required"] == "31" and manifest["core_schema_max_supported"] == "31", "candidate schema window differs")
    return pin


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("pin", type=Path)
    parser.add_argument("candidate", type=Path)
    parser.add_argument("manifest", type=Path)
    args = parser.parse_args()
    verify(args.pin, args.candidate, args.manifest)
    print("frozen runtime candidate identity verified")


if __name__ == "__main__":
    main()
