#!/usr/bin/env python3
from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import re
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[2]
PATCH_RELATIVE = "scripts/release/mcp2026-legacy-bridge-v0.0.17.patch"
PATCH = ROOT / PATCH_RELATIVE
SOURCE_REPOSITORY = "https://github.com/dsmolchanov/nerve-oss"
SOURCE_REVISION = "a794be9f2697e0864d3a31da8f087577e9748f7e"
SOURCE_TREE = "1fe63ae43617c1b426b20ead2cc252893165a90d"
BASELINE_IMAGE = "ghcr.io/dsmolchanov/nerve-runtime@sha256:eaab11e78806e3ed730367c311b1fc30c1360e5be9897d329ec9208912f81765"
MCP_CONTRACT_SHA256 = "254bdc9366cba1ca6759a41bd4dfc902f4ccad4a8a224acfab843b8cd8b01b5c"
SDK_0_2_SHA256 = "9f0a7d6316bf47eef64236f96d1a7a151b5517641930422b1b16711da8b02540"
BEFORE_SHA256 = "82e2e4f1e8e64f5ac622f582e70a81e9d0f5802f38e712527d504afe96837c00"
AFTER_SHA256 = "bdb3c2e94338de0d07d93e7de2f2258efa6964ae9f951b3d10bf5107e03d154b"
ALLOWED_PATHS = ["internal/startup/migrations.go"]
WORKFLOW = ".github/workflows/mcp2026-legacy-bridge.yml"
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")


class VerificationError(ValueError):
    pass


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    return sha256_bytes(path.read_bytes())


def core_schema_sha256() -> str:
    migrations = sorted((ROOT / "internal/store/migrations/core").glob("*.sql"))
    if not migrations or migrations[-1].name != "0029_outbox_policy_fence.sql":
        raise VerificationError("checked-in Core authority head is not 0029")
    digest = hashlib.sha256()
    for migration in migrations:
        digest.update(migration.read_bytes())
    return digest.hexdigest()


def require_keys(value: dict[str, Any], expected: set[str], label: str) -> None:
    actual = set(value)
    if actual != expected:
        raise VerificationError(f"{label} keys differ: missing={sorted(expected - actual)} extra={sorted(actual - expected)}")


def require(condition: bool, message: str) -> None:
    if not condition:
        raise VerificationError(message)


def require_sha(value: Any, label: str) -> str:
    require(isinstance(value, str) and SHA256_RE.fullmatch(value) is not None, f"{label} must be lowercase SHA-256")
    return value


def require_digest(value: Any, label: str) -> str:
    require(isinstance(value, str) and DIGEST_RE.fullmatch(value) is not None, f"{label} must be sha256:<digest>")
    return value


def validate_run(run: Any, label: str, expected_image: str, expected_core: int) -> None:
    require(isinstance(run, dict), f"{label} must be an object")
    require_keys(
        run,
        {
            "image",
            "core_schema_version",
            "startup_mode",
            "health",
            "legacy_initialize",
            "legacy_tools",
            "legacy_resources",
            "legacy_errors",
            "sdk_0_2",
            "sql_behavior",
            "enqueue",
            "claim",
            "delivery",
            "inspection",
        },
        label,
    )
    require(run["image"] == expected_image, f"{label} image identity mismatch")
    require(run["core_schema_version"] == expected_core, f"{label} Core version mismatch")
    require(run["startup_mode"] == "verify", f"{label} did not use production verify mode")
    for gate in (
        "health", "legacy_initialize", "legacy_tools", "legacy_resources", "legacy_errors",
        "sdk_0_2", "sql_behavior", "enqueue", "claim", "delivery", "inspection",
    ):
        require(run[gate] is True, f"{label} did not prove {gate}")


def validate_receipt(receipt: Any) -> None:
    require(isinstance(receipt, dict), "receipt must be an object")
    require_keys(
        receipt,
        {
            "schema_version",
            "kind",
            "target_env",
            "bridge_id",
            "runtime_version",
            "source",
            "patch",
            "compatibility",
            "artifact",
            "verification",
            "producer",
            "built_at",
        },
        "receipt",
    )
    patch_sha = sha256_file(PATCH)
    suffix = patch_sha[:12]
    require(receipt["schema_version"] == 1, "unsupported receipt schema version")
    require(receipt["kind"] == "mcp2026-legacy-runtime-bridge", "wrong receipt kind")
    require(receipt["target_env"] == "cloud-production", "wrong target environment")
    require(receipt["bridge_id"] == f"r0-v0.0.17-core28-29-{suffix}", "bridge ID is not derived from the reviewed patch")
    require(receipt["runtime_version"] == f"r0-a794be9f-core28-29-{suffix}", "runtime version is not the non-semver bridge identity")
    require(receipt["runtime_version"] != "v0.0.17", "bridge must not reuse the immutable public version")

    source = receipt["source"]
    require(isinstance(source, dict), "source must be an object")
    require_keys(source, {"repository", "revision", "tree", "baseline_version", "baseline_image"}, "source")
    require(source["repository"] == SOURCE_REPOSITORY, "source repository mismatch")
    require(source["revision"] == SOURCE_REVISION, "source revision mismatch")
    require(source["tree"] == SOURCE_TREE, "source tree mismatch")
    require(source["baseline_version"] == "v0.0.17", "baseline version mismatch")
    require(source["baseline_image"] == BASELINE_IMAGE, "baseline artifact mismatch")

    patch = receipt["patch"]
    require(isinstance(patch, dict), "patch must be an object")
    require_keys(patch, {"path", "sha256", "allowed_paths", "before_sha256", "after_sha256"}, "patch")
    require(patch["path"] == PATCH_RELATIVE, "patch path mismatch")
    require(patch["sha256"] == patch_sha, "patch digest differs from checked-in authority")
    require(patch["allowed_paths"] == ALLOWED_PATHS, "patch path allowlist mismatch")
    require(patch["before_sha256"] == BEFORE_SHA256, "patch preimage mismatch")
    require(patch["after_sha256"] == AFTER_SHA256, "patch postimage mismatch")

    compatibility = receipt["compatibility"]
    require(isinstance(compatibility, dict), "compatibility must be an object")
    require_keys(
        compatibility,
        {"core_schema_min_required", "core_schema_max_supported", "core_schema_sha256", "mcp_contract_sha256", "startup_mode"},
        "compatibility",
    )
    require(compatibility["core_schema_min_required"] == 28, "R0 Core minimum must remain 28")
    require(compatibility["core_schema_max_supported"] == 29, "R0 Core maximum must be 29")
    require(compatibility["core_schema_sha256"] == core_schema_sha256(), "R0 Core schema authority mismatch")
    require(compatibility["mcp_contract_sha256"] == MCP_CONTRACT_SHA256, "R0 changed the v0.0.17 MCP contract")
    require(compatibility["startup_mode"] == "verify", "R0 production startup mode must be verify")

    artifact = receipt["artifact"]
    require(isinstance(artifact, dict), "artifact must be an object")
    require_keys(
        artifact,
        {
            "ghcr_image", "index_digest", "linux_amd64_digest", "reproducible_linux_amd64_digest",
            "binary_sha256", "reproducible_binary_sha256", "fly_image", "fly_digest",
        },
        "artifact",
    )
    index_digest = require_digest(artifact["index_digest"], "artifact.index_digest")
    linux_digest = require_digest(artifact["linux_amd64_digest"], "artifact.linux_amd64_digest")
    reproducible_linux_digest = require_digest(
        artifact["reproducible_linux_amd64_digest"], "artifact.reproducible_linux_amd64_digest"
    )
    fly_digest = require_digest(artifact["fly_digest"], "artifact.fly_digest")
    require(artifact["ghcr_image"] == f"ghcr.io/dsmolchanov/nerve-runtime@{index_digest}", "GHCR image is not bound to the index digest")
    require(index_digest != BASELINE_IMAGE.rsplit("@", 1)[1], "R0 index reused immutable v0.0.17 bytes")
    require(linux_digest != BASELINE_IMAGE.rsplit("@", 1)[1], "R0 platform reused immutable v0.0.17 bytes")
    require(linux_digest == reproducible_linux_digest, "independent R0 platform image digests differ")
    require(artifact["fly_image"] == f"registry.fly.io/nerve-runtime:r0-{suffix}", "Fly bridge tag mismatch")
    require(fly_digest == linux_digest, "Fly mirror does not resolve to the tested linux/amd64 bytes")
    binary_sha = require_sha(artifact["binary_sha256"], "artifact.binary_sha256")
    reproducible_sha = require_sha(artifact["reproducible_binary_sha256"], "artifact.reproducible_binary_sha256")
    require(binary_sha == reproducible_sha, "image binary differs from the reproducible bridge build")

    verification = receipt["verification"]
    require(isinstance(verification, dict), "verification must be an object")
    require_keys(
        verification,
        {
            "baseline_core28", "bridge_core28", "bridge_core29", "baseline_core29_rejected",
            "core28_wire_equivalent", "core28_compatibility",
        },
        "verification",
    )
    validate_run(verification["baseline_core28"], "baseline_core28", "baseline-v0.0.17", 28)
    validate_run(verification["bridge_core28"], "bridge_core28", "bridge-r0", 28)
    validate_run(verification["bridge_core29"], "bridge_core29", "bridge-r0", 29)
    require(verification["baseline_core29_rejected"] is True, "immutable v0.0.17 was not proven to reject Core 29")
    require(verification["core28_wire_equivalent"] is True, "R0 did not match v0.0.17 legacy wire behavior on Core 28")
    core28 = verification["core28_compatibility"]
    require(isinstance(core28, dict), "core28_compatibility must be an object")
    require_keys(
        core28,
        {"fixture_version", "sdk_0_2_sha256", "baseline_transcript_sha256", "bridge_transcript_sha256"},
        "core28_compatibility",
    )
    require(core28["fixture_version"] == "mcp2026-r0-core28-v1", "unknown Core 28 fixture suite")
    require(core28["sdk_0_2_sha256"] == SDK_0_2_SHA256, "immutable SDK 0.2.0 was substituted")
    baseline_transcript = require_sha(core28["baseline_transcript_sha256"], "baseline Core 28 transcript")
    bridge_transcript = require_sha(core28["bridge_transcript_sha256"], "bridge Core 28 transcript")
    require(baseline_transcript == bridge_transcript, "Core 28 compatibility transcript digests differ")

    producer = receipt["producer"]
    require(isinstance(producer, dict), "producer must be an object")
    require_keys(producer, {"repository", "ref", "workflow", "run_id", "run_attempt", "authority_revision"}, "producer")
    require(producer["repository"] == "dsmolchanov/nerve-oss", "producer repository mismatch")
    require(producer["ref"] == "refs/heads/main", "bridge must be produced from protected main")
    require(producer["workflow"] == WORKFLOW, "producer workflow mismatch")
    require(isinstance(producer["run_id"], int) and producer["run_id"] > 0, "invalid producer run ID")
    require(isinstance(producer["run_attempt"], int) and producer["run_attempt"] > 0, "invalid producer run attempt")
    require(isinstance(producer["authority_revision"], str) and re.fullmatch(r"[0-9a-f]{40}", producer["authority_revision"]) is not None, "invalid authority revision")
    expected_authority = os.environ.get("EXPECTED_AUTHORITY_REVISION", "")
    if expected_authority:
        require(producer["authority_revision"] == expected_authority, "authority revision differs from protected input")
    expected_index = os.environ.get("EXPECTED_BRIDGE_INDEX_DIGEST", "")
    if expected_index:
        require(index_digest == expected_index, "index digest differs from protected input")
    expected_platform = os.environ.get("EXPECTED_BRIDGE_PLATFORM_DIGEST", "")
    if expected_platform:
        require(linux_digest == expected_platform, "platform digest differs from protected input")

    built_at = receipt["built_at"]
    require(isinstance(built_at, str) and re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", built_at) is not None, "invalid built_at timestamp")
    parsed = datetime.strptime(built_at, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    require(parsed <= datetime.now(timezone.utc), "built_at is in the future")


def valid_fixture() -> dict[str, Any]:
    patch_sha = sha256_file(PATCH)
    suffix = patch_sha[:12]
    run = {
        "image": "bridge-r0",
        "core_schema_version": 28,
        "startup_mode": "verify",
        "health": True,
        "legacy_initialize": True,
        "legacy_tools": True,
        "legacy_resources": True,
        "legacy_errors": True,
        "sdk_0_2": True,
        "sql_behavior": True,
        "enqueue": True,
        "claim": True,
        "delivery": True,
        "inspection": True,
    }
    index = "sha256:" + "1" * 64
    platform = "sha256:" + "2" * 64
    return {
        "schema_version": 1,
        "kind": "mcp2026-legacy-runtime-bridge",
        "target_env": "cloud-production",
        "bridge_id": f"r0-v0.0.17-core28-29-{suffix}",
        "runtime_version": f"r0-a794be9f-core28-29-{suffix}",
        "source": {
            "repository": SOURCE_REPOSITORY,
            "revision": SOURCE_REVISION,
            "tree": SOURCE_TREE,
            "baseline_version": "v0.0.17",
            "baseline_image": BASELINE_IMAGE,
        },
        "patch": {
            "path": PATCH_RELATIVE,
            "sha256": patch_sha,
            "allowed_paths": ALLOWED_PATHS,
            "before_sha256": BEFORE_SHA256,
            "after_sha256": AFTER_SHA256,
        },
        "compatibility": {
            "core_schema_min_required": 28,
            "core_schema_max_supported": 29,
            "core_schema_sha256": core_schema_sha256(),
            "mcp_contract_sha256": MCP_CONTRACT_SHA256,
            "startup_mode": "verify",
        },
        "artifact": {
            "ghcr_image": f"ghcr.io/dsmolchanov/nerve-runtime@{index}",
            "index_digest": index,
            "linux_amd64_digest": platform,
            "reproducible_linux_amd64_digest": platform,
            "binary_sha256": "3" * 64,
            "reproducible_binary_sha256": "3" * 64,
            "fly_image": f"registry.fly.io/nerve-runtime:r0-{suffix}",
            "fly_digest": platform,
        },
        "verification": {
            "baseline_core28": {**run, "image": "baseline-v0.0.17"},
            "bridge_core28": run,
            "bridge_core29": {**run, "core_schema_version": 29},
            "baseline_core29_rejected": True,
            "core28_wire_equivalent": True,
            "core28_compatibility": {
                "fixture_version": "mcp2026-r0-core28-v1",
                "sdk_0_2_sha256": SDK_0_2_SHA256,
                "baseline_transcript_sha256": "6" * 64,
                "bridge_transcript_sha256": "6" * 64,
            },
        },
        "producer": {
            "repository": "dsmolchanov/nerve-oss",
            "ref": "refs/heads/main",
            "workflow": WORKFLOW,
            "run_id": 1,
            "run_attempt": 1,
            "authority_revision": "4" * 40,
        },
        "built_at": "2026-08-19T00:00:00Z",
    }


def self_test() -> None:
    fixture = valid_fixture()
    validate_receipt(fixture)
    mutations = [
        ("source revision", ("source", "revision"), "0" * 40),
        ("patch digest", ("patch", "sha256"), "0" * 64),
        ("patch allowlist", ("patch", "allowed_paths"), ["go.mod"]),
        ("Core maximum", ("compatibility", "core_schema_max_supported"), 28),
        ("baseline reuse", ("artifact", "index_digest"), BASELINE_IMAGE.rsplit("@", 1)[1]),
        ("binary mismatch", ("artifact", "reproducible_binary_sha256"), "5" * 64),
        ("platform mismatch", ("artifact", "reproducible_linux_amd64_digest"), "9" * 64),
        ("missing Core 29 proof", ("verification", "bridge_core29", "delivery"), False),
        ("SDK substitution", ("verification", "core28_compatibility", "sdk_0_2_sha256"), "7" * 64),
        ("transcript mismatch", ("verification", "core28_compatibility", "bridge_transcript_sha256"), "8" * 64),
        ("wrong workflow", ("producer", "workflow"), ".github/workflows/docker-publish.yml"),
    ]
    for label, path, value in mutations:
        candidate = copy.deepcopy(fixture)
        cursor: dict[str, Any] = candidate
        for key in path[:-1]:
            cursor = cursor[key]
        cursor[path[-1]] = value
        try:
            validate_receipt(candidate)
        except VerificationError:
            continue
        raise VerificationError(f"negative self-test accepted mutation: {label}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("receipt", nargs="?", type=Path)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    try:
        if args.self_test:
            self_test()
            print("legacy bridge verifier self-test passed")
            return 0
        if args.receipt is None:
            parser.error("receipt is required unless --self-test is used")
        receipt = json.loads(args.receipt.read_text(encoding="utf-8"))
        validate_receipt(receipt)
        print(f"legacy bridge receipt verified: {args.receipt} sha256={sha256_file(args.receipt)}")
        return 0
    except (OSError, json.JSONDecodeError, VerificationError) as error:
        print(f"legacy bridge receipt verification failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
