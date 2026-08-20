#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PATCH_RELATIVE="scripts/release/mcp2026-legacy-bridge-v0.0.17.patch"
PATCH_PATH="${ROOT_DIR}/${PATCH_RELATIVE}"
PINNED_SOURCE_REVISION="a794be9f2697e0864d3a31da8f087577e9748f7e"
PINNED_SOURCE_TREE="1fe63ae43617c1b426b20ead2cc252893165a90d"
PINNED_BASELINE_IMAGE="ghcr.io/dsmolchanov/nerve-runtime@sha256:eaab11e78806e3ed730367c311b1fc30c1360e5be9897d329ec9208912f81765"
PINNED_MCP_CONTRACT_SHA256="254bdc9366cba1ca6759a41bd4dfc902f4ccad4a8a224acfab843b8cd8b01b5c"
PINNED_SDK_0_2_SHA256="9f0a7d6316bf47eef64236f96d1a7a151b5517641930422b1b16711da8b02540"
PINNED_BEFORE_SHA256="82e2e4f1e8e64f5ac622f582e70a81e9d0f5802f38e712527d504afe96837c00"
PINNED_AFTER_SHA256="bdb3c2e94338de0d07d93e7de2f2258efa6964ae9f951b3d10bf5107e03d154b"
ALLOWED_PATH="internal/startup/migrations.go"
REPRODUCIBLE_BUILD_TIME="2026-08-19T00:00:00Z"

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

hash_core_schema() {
  local digest_file
  digest_file="$(mktemp)"
  while IFS= read -r migration; do
    cat "$migration" >>"$digest_file"
  done < <(find "${ROOT_DIR}/internal/store/migrations/core" -type f -name '*.sql' | LC_ALL=C sort)
  sha256_file "$digest_file"
  rm -f "$digest_file"
}

patch_sha256() {
  sha256_file "$PATCH_PATH"
}

bridge_suffix() {
  patch_sha256 | cut -c1-12
}

bridge_version() {
  printf 'r0-a794be9f-core28-29-%s' "$(bridge_suffix)"
}

bridge_id() {
  printf 'r0-v0.0.17-core28-29-%s' "$(bridge_suffix)"
}

require_pinned_authority() {
  local source_tree contract_sha core_head patch_stat
  source_tree="$(git -C "$ROOT_DIR" rev-parse "${PINNED_SOURCE_REVISION}^{tree}")"
  [[ "$source_tree" == "$PINNED_SOURCE_TREE" ]] || {
    echo "pinned v0.0.17 source tree mismatch" >&2
    exit 1
  }
  contract_sha="$(git -C "$ROOT_DIR" show "${PINNED_SOURCE_REVISION}:docs/MCP_Contract.md" | sha256_stream)"
  [[ "$contract_sha" == "$PINNED_MCP_CONTRACT_SHA256" ]] || {
    echo "pinned v0.0.17 MCP contract mismatch" >&2
    exit 1
  }
  core_head="$(find "${ROOT_DIR}/internal/store/migrations/core" -type f -name '*.sql' -maxdepth 1 -print | LC_ALL=C sort | tail -n 1)"
  [[ "$(basename "$core_head")" == "0029_outbox_policy_fence.sql" ]] || {
    echo "R0 bridge requires exact Core 0029 authority head" >&2
    exit 1
  }
  patch_stat="$(git -C "$ROOT_DIR" apply --unidiff-zero --numstat "$PATCH_PATH")"
  [[ "$patch_stat" == $'1\t1\tinternal/startup/migrations.go' ]] || {
    echo "legacy bridge patch changes more than the exact allowlist" >&2
    exit 1
  }
}

sha256_stream() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  else
    shasum -a 256 | awk '{print $1}'
  fi
}

prepare_source() {
  local destination="$1"
  local metadata_path="$2"
  require_pinned_authority
  [[ ! -e "$destination" ]] || {
    echo "destination already exists: $destination" >&2
    exit 1
  }
  mkdir -p "$destination"
  git -C "$ROOT_DIR" archive "$PINNED_SOURCE_REVISION" | tar -x -C "$destination"

  [[ "$(sha256_file "${destination}/${ALLOWED_PATH}")" == "$PINNED_BEFORE_SHA256" ]] || {
    echo "pinned source file does not match the reviewed preimage" >&2
    exit 1
  }
  patch --directory="$destination" --strip=1 --fuzz=0 --dry-run <"$PATCH_PATH" >/dev/null
  patch --directory="$destination" --strip=1 --fuzz=0 <"$PATCH_PATH" >/dev/null
  [[ "$(sha256_file "${destination}/${ALLOWED_PATH}")" == "$PINNED_AFTER_SHA256" ]] || {
    echo "patched source file does not match the reviewed postimage" >&2
    exit 1
  }
  [[ "$(grep -Ec '^\s*CoreMinRequired\s+int64 = 28$' "${destination}/${ALLOWED_PATH}")" == "1" ]] || {
    echo "patched source lost Core minimum 28" >&2
    exit 1
  }
  [[ "$(grep -Ec '^\s*CoreMaxSupported\s+int64 = 29$' "${destination}/${ALLOWED_PATH}")" == "1" ]] || {
    echo "patched source did not widen Core maximum to 29" >&2
    exit 1
  }

  mkdir -p "$(dirname "$metadata_path")"
  jq -n \
    --arg bridge_id "$(bridge_id)" \
    --arg runtime_version "$(bridge_version)" \
    --arg source_revision "$PINNED_SOURCE_REVISION" \
    --arg source_tree "$PINNED_SOURCE_TREE" \
    --arg baseline_image "$PINNED_BASELINE_IMAGE" \
    --arg patch_path "$PATCH_RELATIVE" \
    --arg patch_sha256 "$(patch_sha256)" \
    --arg before_sha256 "$PINNED_BEFORE_SHA256" \
    --arg after_sha256 "$PINNED_AFTER_SHA256" \
    --arg mcp_contract_sha256 "$PINNED_MCP_CONTRACT_SHA256" \
    --arg core_schema_sha256 "$(hash_core_schema)" \
    --arg build_time "$REPRODUCIBLE_BUILD_TIME" \
    '{
      bridge_id: $bridge_id,
      runtime_version: $runtime_version,
      source_revision: $source_revision,
      source_tree: $source_tree,
      baseline_image: $baseline_image,
      patch_path: $patch_path,
      patch_sha256: $patch_sha256,
      allowed_paths: ["internal/startup/migrations.go"],
      before_sha256: $before_sha256,
      after_sha256: $after_sha256,
      mcp_contract_sha256: $mcp_contract_sha256,
      core_schema_sha256: $core_schema_sha256,
      core_schema_min_required: 28,
      core_schema_max_supported: 29,
      build_time: $build_time
    }' >"$metadata_path"
}

build_binary() {
  local source_dir="$1"
  local output_dir="$2"
  local metadata_path="$3"
  local runtime_version core_schema_sha256 mcp_contract_sha256
  source_dir="$(cd "$source_dir" && pwd)"
  mkdir -p "$output_dir"
  output_dir="$(cd "$output_dir" && pwd)"
  metadata_path="$(cd "$(dirname "$metadata_path")" && pwd)/$(basename "$metadata_path")"
  runtime_version="$(jq -er '.runtime_version' "$metadata_path")"
  core_schema_sha256="$(jq -er '.core_schema_sha256' "$metadata_path")"
  mcp_contract_sha256="$(jq -er '.mcp_contract_sha256' "$metadata_path")"
  build_one() {
    local output="$1"
    local cache_name="$2"
    local build_cache="${R0_GO_BUILD_CACHE_ROOT:-${output_dir}}/go-build-cache-${cache_name}"
    mkdir -p "$build_cache"
    (
      cd "$source_dir"
      GOCACHE="$build_cache" CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
        -trimpath -buildvcs=false \
        -ldflags "-s -w -buildid=mcp2026-r0-v0.0.17-core28-29 \
          -X neuralmail/internal/release.RuntimeVersion=${runtime_version} \
          -X neuralmail/internal/release.MCPContractHash=${mcp_contract_sha256} \
          -X neuralmail/internal/release.CoreSchemaHash=${core_schema_sha256} \
          -X neuralmail/internal/release.BuildCommit=${PINNED_SOURCE_REVISION} \
          -X neuralmail/internal/release.BuildTime=${REPRODUCIBLE_BUILD_TIME}" \
        -o "$output" ./cmd/neuralmaild
    )
  }

  build_one "${output_dir}/nerve-runtime.first" first
  build_one "${output_dir}/nerve-runtime.second" second
  local first_sha second_sha
  first_sha="$(sha256_file "${output_dir}/nerve-runtime.first")"
  second_sha="$(sha256_file "${output_dir}/nerve-runtime.second")"
  [[ "$first_sha" == "$second_sha" ]] || {
    echo "R0 binary is not reproducible: ${first_sha} != ${second_sha}" >&2
    exit 1
  }
  cp "${output_dir}/nerve-runtime.first" "${output_dir}/nerve-runtime"
  printf '%s\n' "$first_sha" >"${output_dir}/nerve-runtime.sha256"
  echo "reproducible R0 binary sha256=${first_sha}"
}

verify_image() {
  local image="$1"
  local metadata_path="$2"
  local expected_version expected_contract expected_core labels
  expected_version="$(jq -er '.runtime_version' "$metadata_path")"
  expected_contract="$(jq -er '.mcp_contract_sha256' "$metadata_path")"
  expected_core="$(jq -er '.core_schema_sha256' "$metadata_path")"
  labels="$(docker image inspect "$image" --format '{{json .Config.Labels}}')"
  [[ "$(jq -er '."io.nerve.runtime.version"' <<<"$labels")" == "$expected_version" ]]
  [[ "$(jq -er '."io.nerve.runtime.mcp-contract-sha256"' <<<"$labels")" == "$expected_contract" ]]
  [[ "$(jq -er '."io.nerve.runtime.core-schema-sha256"' <<<"$labels")" == "$expected_core" ]]
  [[ "$(jq -er '."io.nerve.runtime.core-schema-min-required"' <<<"$labels")" == "28" ]]
  [[ "$(jq -er '."io.nerve.runtime.core-schema-max-supported"' <<<"$labels")" == "29" ]]
  [[ "$(jq -er '."io.nerve.runtime.legacy-bridge-source"' <<<"$labels")" == "$PINNED_SOURCE_REVISION" ]]
  [[ "$(jq -er '."io.nerve.runtime.legacy-bridge-patch-sha256"' <<<"$labels")" == "$(patch_sha256)" ]]
  [[ "$image" != "$PINNED_BASELINE_IMAGE" ]] || {
    echo "R0 must be a distinct image from immutable v0.0.17" >&2
    exit 1
  }
  docker run --rm --entrypoint /app/nerve-runtime "$image" 2>&1 | grep -Fq 'Usage: nerve-runtime'
  echo "R0 image labels and entrypoint verified"
}

stage_image_context() {
  local binary_path="$1"
  local output_dir="$2"
  [[ -f "$binary_path" ]] || {
    echo "missing reproducible R0 binary: $binary_path" >&2
    exit 1
  }
  [[ ! -e "$output_dir" ]] || {
    echo "image context already exists: $output_dir" >&2
    exit 1
  }
  mkdir -p "$output_dir"
  cp "$binary_path" "${output_dir}/nerve-runtime"
  chmod 0555 "${output_dir}/nerve-runtime"
  TZ=UTC touch -t 202608190000.00 "${output_dir}/nerve-runtime"
}

replace_database_name() {
  python3 - "$1" "$2" <<'PY'
import sys
from urllib.parse import urlsplit, urlunsplit

source = urlsplit(sys.argv[1])
database = sys.argv[2]
print(urlunsplit((source.scheme, source.netloc, "/" + database, source.query, source.fragment)))
PY
}

artifact_test_root=""
artifact_test_provider_pid=""
artifact_test_containers=()
artifact_test_databases=()
artifact_test_admin_dsn=""
artifact_test_sdk_python=""

cleanup_artifact_test() {
  local container database
  for container in "${artifact_test_containers[@]:-}"; do
    docker rm -f "$container" >/dev/null 2>&1 || true
  done
  if [[ -n "$artifact_test_provider_pid" ]]; then
    kill "$artifact_test_provider_pid" >/dev/null 2>&1 || true
    wait "$artifact_test_provider_pid" >/dev/null 2>&1 || true
  fi
  if [[ -n "$artifact_test_admin_dsn" ]]; then
    for database in "${artifact_test_databases[@]:-}"; do
      psql "$artifact_test_admin_dsn" -v ON_ERROR_STOP=1 \
        -c "DROP DATABASE IF EXISTS \"${database}\" WITH (FORCE)" >/dev/null 2>&1 || true
    done
  fi
  if [[ -n "$artifact_test_root" ]]; then
    rm -rf "$artifact_test_root"
  fi
}

start_fake_resend() {
  local port="$1"
  local server_path="${artifact_test_root}/fake_resend.py"
  cat >"$server_path" <<'PY'
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        request = json.loads(body)
        assert self.path == "/emails"
        assert request.get("subject") == "R0 bridge delivery"
        assert self.headers.get("Idempotency-Key")
        time.sleep(2)
        payload = json.dumps({"id": "bridge-provider-message"}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, format, *args):
        return


ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), Handler).serve_forever()
PY
  python3 "$server_path" "$port" &
  artifact_test_provider_pid="$!"
  for _ in $(seq 1 50); do
    if python3 - "$port" 2>/dev/null <<'PY'
import socket
import sys

with socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=0.2):
    pass
PY
    then
      return
    fi
    sleep 0.1
  done
  echo "fake Resend probe did not start" >&2
  exit 1
}

wait_for_http() {
  local url="$1"
  local container="$2"
  for _ in $(seq 1 100); do
    if curl --silent --show-error --fail "$url" >/dev/null 2>&1; then
      return
    fi
    if [[ "$(docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null || true)" != "true" ]]; then
      docker logs "$container" >&2 || true
      echo "runtime container exited before health: $container" >&2
      exit 1
    fi
    sleep 0.1
  done
  docker logs "$container" >&2 || true
  echo "runtime health probe timed out: $container" >&2
  exit 1
}

legacy_post() {
  local port="$1"
  local api_key="$2"
  local session_id="$3"
  local payload="$4"
  local output_stem="$5"
  local headers=(--header 'Content-Type: application/json' --header 'MCP-Protocol-Version: 2025-11-25')
  if [[ -n "$api_key" ]]; then
    headers+=(--header "X-Nerve-Cloud-Key: ${api_key}")
  fi
  if [[ -n "$session_id" ]]; then
    headers+=(--header "MCP-Session-Id: ${session_id}")
  fi
  curl --silent --show-error \
    --dump-header "${output_stem}.headers" \
    --output "${output_stem}.json" \
    --write-out '%{http_code}' \
    "${headers[@]}" \
    --data "$payload" \
    "http://127.0.0.1:${port}/mcp" >"${output_stem}.status"
}

run_sdk_0_2_fixture() {
  local port="$1"
  local api_key="$2"
  local inbox_id="$3"
  local thread_id="$4"
  local output_path="$5"
  "$artifact_test_sdk_python" - "$port" "$api_key" "$inbox_id" "$thread_id" "$output_path" <<'PY'
import asyncio
import json
import sys

from nerve_email import NerveClient, __version__


async def main() -> None:
    port, api_key, inbox_id, thread_id, output_path = sys.argv[1:]
    if __version__ != "0.2.0":
        raise SystemExit(f"unexpected immutable SDK version: {__version__}")
    async with NerveClient(
        base_url=f"http://127.0.0.1:{port}",
        api_key=api_key,
        timeout=10,
        max_retries=0,
        rest_base_url=f"http://127.0.0.1:{port}",
    ) as client:
        result = {
            "sdk_version": __version__,
            "health": await client.health_check(),
            "tools": await client.list_tools(),
            "inboxes": await client.list_inboxes(),
            "threads": await client.list_threads(inbox_id=inbox_id, limit=10),
            "thread": await client.get_thread(thread_id=thread_id),
        }
    with open(output_path, "w", encoding="utf-8") as output:
        json.dump(result, output, sort_keys=True, separators=(",", ":"))
        output.write("\n")


asyncio.run(main())
PY
}

build_compatibility_transcript() {
  local label="$1"
  local database_dsn="$2"
  local output_path="${artifact_test_root}/${label}-compatibility.json"
  local sql_path="${artifact_test_root}/${label}-sql.json"
  psql "$database_dsn" -Atqc "
    SELECT json_build_object(
      'org_count', (SELECT count(*) FROM orgs),
      'inbox_count', (SELECT count(*) FROM inboxes),
      'thread_count', (SELECT count(*) FROM threads),
      'message_count', (SELECT count(*) FROM messages),
      'outbox_count', (SELECT count(*) FROM outbox_messages),
      'outbox_status', (SELECT status FROM outbox_messages WHERE subject = 'R0 bridge delivery'),
      'provider_message_id', (SELECT provider_message_id FROM outbox_messages WHERE subject = 'R0 bridge delivery'),
      'tool_idempotency_succeeded', (SELECT count(*) FROM tool_idempotency WHERE status = 'succeeded'),
      'tool_idempotency_started', (SELECT count(*) FROM tool_idempotency WHERE status = 'started')
    )::text
  " >"$sql_path"
  python3 - "$artifact_test_root" "$label" "$sql_path" "$output_path" <<'PY'
import json
import re
import sys
from pathlib import Path

root, label, sql_path, output_path = Path(sys.argv[1]), sys.argv[2], Path(sys.argv[3]), Path(sys.argv[4])
uuid_re = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}", re.I)
time_re = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z")


def normalize(value):
    if isinstance(value, dict):
        return {key: normalize(value[key]) for key in sorted(value)}
    if isinstance(value, list):
        return [normalize(item) for item in value]
    if isinstance(value, str):
        return time_re.sub("<timestamp>", uuid_re.sub("<uuid>", value))
    return value


def response(name):
    body = json.loads((root / f"{label}-{name}.json").read_text())
    headers = (root / f"{label}-{name}.headers").read_text().replace("\r", "").splitlines()
    content_type = next((line.split(":", 1)[1].strip() for line in headers if line.lower().startswith("content-type:")), "")
    session_present = any(line.lower().startswith("mcp-session-id:") for line in headers)
    return {
        "status": int((root / f"{label}-{name}.status").read_text()),
        "content_type": content_type,
        "session_header_present": session_present,
        "body": normalize(body),
    }


names = [
    "initialize", "tools", "resources-list", "resources-read", "missing-session",
    "unknown-method", "compose", "list-threads", "inspect", "subscription-error",
    "quota-error", "rate-error", "idempotency-null-scan", "idempotency-error",
]
transcript = {
    "fixture_version": "mcp2026-r0-core28-v1",
    "http_mcp": {name: response(name) for name in names},
    "sdk_0_2": normalize(json.loads((root / f"{label}-sdk-0.2.json").read_text())),
    "sql": normalize(json.loads(sql_path.read_text())),
}
output_path.write_text(json.dumps(transcript, sort_keys=True, separators=(",", ":")) + "\n")
PY
  sha256_file "$output_path" >"${artifact_test_root}/${label}-compatibility.sha256"
}

run_artifact_case() {
  local label="$1"
  local image="$2"
  local core_version="$3"
  local port="$4"
  local migrate_binary="$5"
  local database="mcp2026_r0_${label}"
  local database_dsn container_dsn runtime_container worker_container
  local api_key="mcp2026-r0-test-key"
  local api_key_sha inbox_id session_id thread_id org_id
  local init_body tools_body compose_body inspect_body
  local claim_observed=false
  [[ "$database" =~ ^[a-z0-9_]+$ ]] || {
    echo "invalid bridge test database name" >&2
    exit 1
  }
  artifact_test_databases+=("$database")
  psql "$artifact_test_admin_dsn" -v ON_ERROR_STOP=1 -c "CREATE DATABASE \"${database}\"" >/dev/null
  database_dsn="$(replace_database_name "$artifact_test_admin_dsn" "$database")"
  NM_DB_DSN="$database_dsn" "$migrate_binary" up --scope core --to "$core_version" >/dev/null
  container_dsn="${database_dsn/127.0.0.1/host.docker.internal}"
  container_dsn="${container_dsn/localhost/host.docker.internal}"
  runtime_container="mcp2026-r0-${label}-runtime"
  worker_container="mcp2026-r0-${label}-worker"
  artifact_test_containers+=("$runtime_container" "$worker_container")

  docker run --detach --name "$runtime_container" \
    --add-host host.docker.internal:host-gateway \
    --publish "127.0.0.1:${port}:8088" \
    --env NM_CLOUD_MODE=1 \
    --env NM_MIGRATE_ON_START=verify \
    --env NM_DB_DSN="$container_dsn" \
    --env NM_HTTP_ADDR=:8088 \
    --env NM_REDIS_URL=redis://host.docker.internal:16379/0 \
    --env NM_SMTP_FROM=bridge@local.nerve.email \
    --env NM_ALLOW_OUTBOUND=1 \
    --env NM_RESEND_API_KEY=bridge-test \
    --env NM_RESEND_BASE_URL=http://host.docker.internal:18080 \
    "$image" serve >/dev/null
  wait_for_http "http://127.0.0.1:${port}/healthz" "$runtime_container"

  api_key_sha="$(printf '%s' "$api_key" | sha256_stream)"
  psql "$database_dsn" -v ON_ERROR_STOP=1 \
    --set=key_sha="$api_key_sha" <<'SQL' >/dev/null
WITH target_org AS (
  SELECT id FROM orgs ORDER BY created_at, id LIMIT 1
)
INSERT INTO org_entitlements (
  org_id, plan_code, subscription_status, mcp_rpm, monthly_units,
  max_inboxes, max_domains, features, usage_period_start, usage_period_end
)
SELECT id, 'bridge-test', 'active', 1000, 100000, 10, 10,
       '{"email_send_enabled":true}'::jsonb, now() - interval '1 day', now() + interval '30 days'
FROM target_org
ON CONFLICT (org_id) DO UPDATE SET subscription_status = 'active';

WITH target_org AS (
  SELECT id FROM orgs ORDER BY created_at, id LIMIT 1
)
INSERT INTO cloud_api_keys (org_id, key_prefix, key_hash, label, scopes)
SELECT id, 'mcp2026-r0', :'key_sha', 'R0 artifact test', ARRAY['nerve:email.read','nerve:email.send']
FROM target_org;

UPDATE inboxes SET outbound_provider = 'resend';
SQL
  inbox_id="$(psql "$database_dsn" -Atqc "SELECT id FROM inboxes ORDER BY created_at, id LIMIT 1")"
  [[ -n "$inbox_id" ]] || {
    echo "R0 startup did not create its legacy default inbox" >&2
    exit 1
  }

  init_body="${artifact_test_root}/${label}-initialize.json"
  tools_body="${artifact_test_root}/${label}-tools.json"
  compose_body="${artifact_test_root}/${label}-compose.json"
  inspect_body="${artifact_test_root}/${label}-inspect.json"
  legacy_post "$port" "$api_key" "" \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}' \
    "${artifact_test_root}/${label}-initialize"
  [[ "$(<"${artifact_test_root}/${label}-initialize.status")" == "200" ]]
  session_id="$(tr -d '\r' <"${artifact_test_root}/${label}-initialize.headers" | awk 'tolower($1) == "mcp-session-id:" {print $2}')"
  [[ -n "$session_id" ]] || {
    echo "legacy initialize did not return MCP-Session-Id" >&2
    exit 1
  }
  jq -e '.result.protocolVersion == "2025-11-25"' "$init_body" >/dev/null
  legacy_post "$port" "$api_key" "$session_id" \
    '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
    "${artifact_test_root}/${label}-tools"
  [[ "$(<"${artifact_test_root}/${label}-tools.status")" == "200" ]]
  jq -e '.result.tools | any(.name == "compose_email")' "$tools_body" >/dev/null

  legacy_post "$port" "$api_key" "$session_id" \
    '{"jsonrpc":"2.0","id":3,"method":"resources/list","params":{}}' \
    "${artifact_test_root}/${label}-resources-list"
  legacy_post "$port" "$api_key" "$session_id" \
    '{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"email://inboxes"}}' \
    "${artifact_test_root}/${label}-resources-read"
  legacy_post "$port" "$api_key" "" \
    '{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{}}' \
    "${artifact_test_root}/${label}-missing-session"
  legacy_post "$port" "$api_key" "$session_id" \
    '{"jsonrpc":"2.0","id":6,"method":"nerve/unknown","params":{}}' \
    "${artifact_test_root}/${label}-unknown-method"

  jq -n --arg inbox_id "$inbox_id" '{
    jsonrpc: "2.0", id: 7, method: "tools/call",
    params: {name: "compose_email", arguments: {
      inbox_id: $inbox_id,
      to: "recipient@local.nerve.email",
      subject: "R0 bridge delivery",
      body: "artifact-level legacy delivery proof",
      idempotency_key: "mcp2026-r0-delivery"
    }}
  }' >"${artifact_test_root}/${label}-compose-request.json"
  legacy_post "$port" "$api_key" "$session_id" \
    "$(<"${artifact_test_root}/${label}-compose-request.json")" \
    "${artifact_test_root}/${label}-compose"
  [[ "$(<"${artifact_test_root}/${label}-compose.status")" == "200" ]]
  jq -e '.result.status == "queued" and (.result.thread_id | type == "string")' "$compose_body" >/dev/null
  thread_id="$(jq -er '.result.thread_id' "$compose_body")"

  docker run --detach --name "$worker_container" \
    --add-host host.docker.internal:host-gateway \
    --env NM_CLOUD_MODE=1 \
    --env NM_MIGRATE_ON_START=verify \
    --env NM_DB_DSN="$container_dsn" \
    --env NM_REDIS_URL=redis://host.docker.internal:16379/0 \
    --env NM_RESEND_API_KEY=bridge-test \
    --env NM_RESEND_BASE_URL=http://host.docker.internal:18080 \
    "$image" worker >/dev/null

  for _ in $(seq 1 100); do
    if [[ "$(psql "$database_dsn" -Atqc "SELECT status FROM outbox_messages WHERE subject = 'R0 bridge delivery'")" == "sending" ]]; then
      claim_observed=true
      break
    fi
    sleep 0.05
  done
  [[ "$claim_observed" == true ]] || {
    docker logs "$worker_container" >&2 || true
    echo "R0 worker did not expose the claimed/sending state" >&2
    exit 1
  }
  for _ in $(seq 1 200); do
    if [[ "$(psql "$database_dsn" -Atqc "SELECT status FROM outbox_messages WHERE subject = 'R0 bridge delivery'")" == "sent" ]]; then
      break
    fi
    sleep 0.1
  done
  [[ "$(psql "$database_dsn" -Atqc "SELECT status || ':' || coalesce(provider_message_id, '') FROM outbox_messages WHERE subject = 'R0 bridge delivery'")" == "sent:bridge-provider-message" ]] || {
    docker logs "$worker_container" >&2 || true
    echo "R0 worker did not complete provider delivery" >&2
    exit 1
  }
  if [[ "$core_version" == "29" ]]; then
    [[ "$(psql "$database_dsn" -Atqc "SELECT autonomous_policy_epoch IS NULL AND provider_started_at IS NULL AND provider_resolved_at IS NULL FROM outbox_messages WHERE subject = 'R0 bridge delivery'")" == "t" ]] || {
      echo "legacy R0 row did not preserve null Core 0029 fence semantics" >&2
      exit 1
    }
  fi

  jq -n --arg inbox_id "$inbox_id" '{jsonrpc: "2.0", id: 8, method: "tools/call", params: {name: "list_threads", arguments: {inbox_id: $inbox_id, limit: 10}}}' \
    >"${artifact_test_root}/${label}-list-threads-request.json"
  legacy_post "$port" "$api_key" "$session_id" \
    "$(<"${artifact_test_root}/${label}-list-threads-request.json")" \
    "${artifact_test_root}/${label}-list-threads"
  jq -n --arg thread_id "$thread_id" '{jsonrpc: "2.0", id: 9, method: "tools/call", params: {name: "get_thread", arguments: {thread_id: $thread_id}}}' \
    >"${artifact_test_root}/${label}-inspect-request.json"
  legacy_post "$port" "$api_key" "$session_id" \
    "$(<"${artifact_test_root}/${label}-inspect-request.json")" \
    "${artifact_test_root}/${label}-inspect"
  jq -e '.. | strings | select(. == "R0 bridge delivery")' "$inspect_body" >/dev/null

  run_sdk_0_2_fixture "$port" "$api_key" "$inbox_id" "$thread_id" \
    "${artifact_test_root}/${label}-sdk-0.2.json"

  org_id="$(psql "$database_dsn" -Atqc "SELECT id FROM orgs ORDER BY created_at, id LIMIT 1")"
  psql "$database_dsn" -v ON_ERROR_STOP=1 -c \
    "UPDATE org_entitlements SET subscription_status = 'canceled' WHERE org_id = '${org_id}'" >/dev/null
  legacy_post "$port" "$api_key" "$session_id" \
    "$(<"${artifact_test_root}/${label}-list-threads-request.json")" \
    "${artifact_test_root}/${label}-subscription-error"
  jq -e '.error.code == -32041' "${artifact_test_root}/${label}-subscription-error.json" >/dev/null

  psql "$database_dsn" -v ON_ERROR_STOP=1 -c \
    "UPDATE org_entitlements SET subscription_status = 'active', monthly_units = 0 WHERE org_id = '${org_id}'" >/dev/null
  legacy_post "$port" "$api_key" "$session_id" \
    "$(<"${artifact_test_root}/${label}-list-threads-request.json")" \
    "${artifact_test_root}/${label}-quota-error"
  jq -e '.error.code == -32040' "${artifact_test_root}/${label}-quota-error.json" >/dev/null

  psql "$database_dsn" -v ON_ERROR_STOP=1 -c \
    "UPDATE org_entitlements SET monthly_units = 100000, mcp_rpm = 0 WHERE org_id = '${org_id}'" >/dev/null
  legacy_post "$port" "$api_key" "$session_id" \
    "$(<"${artifact_test_root}/${label}-list-threads-request.json")" \
    "${artifact_test_root}/${label}-rate-error"
  jq -e '.error.code == -32042 and .error.data.retry_after_seconds == 60' \
    "${artifact_test_root}/${label}-rate-error.json" >/dev/null

  psql "$database_dsn" -v ON_ERROR_STOP=1 \
    --set=org_id="$org_id" <<'SQL' >/dev/null
UPDATE org_entitlements SET mcp_rpm = 1000 WHERE org_id = :'org_id'::uuid;
INSERT INTO tool_idempotency (org_id, tool_name, idempotency_key, status, cached_response, updated_at)
VALUES
  (:'org_id'::uuid, 'compose_email', 'mcp2026-r0-null-scan', 'started', NULL, now()),
  (:'org_id'::uuid, 'compose_email', 'mcp2026-r0-in-progress', 'started', 'null'::jsonb, now())
ON CONFLICT (org_id, tool_name, idempotency_key) DO UPDATE
SET status = EXCLUDED.status, cached_response = EXCLUDED.cached_response, updated_at = EXCLUDED.updated_at;
SQL
  jq -n --arg inbox_id "$inbox_id" '{
    jsonrpc: "2.0", id: 10, method: "tools/call",
    params: {name: "compose_email", arguments: {
      inbox_id: $inbox_id, to: "recipient@local.nerve.email", subject: "blocked null scan",
      body: "must not enqueue", idempotency_key: "mcp2026-r0-null-scan"
    }}
  }' >"${artifact_test_root}/${label}-idempotency-null-request.json"
  legacy_post "$port" "$api_key" "$session_id" \
    "$(<"${artifact_test_root}/${label}-idempotency-null-request.json")" \
    "${artifact_test_root}/${label}-idempotency-null-scan"
  jq -e '.error.code == -32000 and (.error.message | contains("cached_response"))' \
    "${artifact_test_root}/${label}-idempotency-null-scan.json" >/dev/null

  jq -n --arg inbox_id "$inbox_id" '{
    jsonrpc: "2.0", id: 11, method: "tools/call",
    params: {name: "compose_email", arguments: {
      inbox_id: $inbox_id, to: "recipient@local.nerve.email", subject: "blocked duplicate",
      body: "must not enqueue", idempotency_key: "mcp2026-r0-in-progress"
    }}
  }' >"${artifact_test_root}/${label}-idempotency-request.json"
  legacy_post "$port" "$api_key" "$session_id" \
    "$(<"${artifact_test_root}/${label}-idempotency-request.json")" \
    "${artifact_test_root}/${label}-idempotency-error"
  jq -e '.error.code == -32043 and .error.data.retry_after_seconds == 2' \
    "${artifact_test_root}/${label}-idempotency-error.json" >/dev/null || {
      cat "${artifact_test_root}/${label}-idempotency-error.json" >&2
      echo "legacy idempotency-in-progress fixture did not return -32043" >&2
      exit 1
    }

  build_compatibility_transcript "$label" "$database_dsn"

  jq -n \
    --arg image "$(if [[ "$label" == baseline28 ]]; then echo baseline-v0.0.17; else echo bridge-r0; fi)" \
    --argjson core "$core_version" \
    '{
      image: $image,
      core_schema_version: $core,
      startup_mode: "verify",
      health: true,
      legacy_initialize: true,
      legacy_tools: true,
      legacy_resources: true,
      legacy_errors: true,
      sdk_0_2: true,
      sql_behavior: true,
      enqueue: true,
      claim: true,
      delivery: true,
      inspection: true
    }' >"${artifact_test_root}/${label}-result.json"

  docker rm -f "$worker_container" "$runtime_container" >/dev/null
  psql "$artifact_test_admin_dsn" -v ON_ERROR_STOP=1 -c "DROP DATABASE \"${database}\" WITH (FORCE)" >/dev/null
}

prove_baseline_rejects_core29() {
  local baseline_image="$1"
  local migrate_binary="$2"
  local database="mcp2026_r0_baseline_reject29"
  local database_dsn container_dsn container="mcp2026-r0-baseline-reject29"
  artifact_test_databases+=("$database")
  artifact_test_containers+=("$container")
  psql "$artifact_test_admin_dsn" -v ON_ERROR_STOP=1 -c "CREATE DATABASE \"${database}\"" >/dev/null
  database_dsn="$(replace_database_name "$artifact_test_admin_dsn" "$database")"
  NM_DB_DSN="$database_dsn" "$migrate_binary" up --scope core --to 29 >/dev/null
  container_dsn="${database_dsn/127.0.0.1/host.docker.internal}"
  container_dsn="${container_dsn/localhost/host.docker.internal}"
  docker run --detach --name "$container" \
    --add-host host.docker.internal:host-gateway \
    --env NM_CLOUD_MODE=1 \
    --env NM_MIGRATE_ON_START=verify \
    --env NM_DB_DSN="$container_dsn" \
    --env NM_REDIS_URL=redis://host.docker.internal:16379/0 \
    "$baseline_image" serve >/dev/null
  for _ in $(seq 1 50); do
    if [[ "$(docker inspect -f '{{.State.Running}}' "$container")" != "true" ]]; then
      break
    fi
    sleep 0.1
  done
  if [[ "$(docker inspect -f '{{.State.Running}}' "$container")" == "true" ]]; then
    echo "immutable v0.0.17 unexpectedly started on Core 29" >&2
    exit 1
  fi
  docker logs "$container" >"${artifact_test_root}/baseline-reject29.log" 2>&1
  grep -Fq 'outside binary compatibility window [28,28]' "${artifact_test_root}/baseline-reject29.log"
  docker rm -f "$container" >/dev/null 2>&1 || true
  psql "$artifact_test_admin_dsn" -v ON_ERROR_STOP=1 -c "DROP DATABASE \"${database}\" WITH (FORCE)" >/dev/null
}

test_artifacts() {
  local baseline_image="$1"
  local bridge_image="$2"
  local admin_dsn="$3"
  local migrate_binary="$4"
  local sdk_python="$5"
  local output_path="$6"
  local baseline_transcript_sha bridge_transcript_sha
  [[ "$baseline_image" == "$PINNED_BASELINE_IMAGE" ]] || {
    echo "artifact test baseline must be the immutable v0.0.17 digest" >&2
    exit 1
  }
  [[ -x "$migrate_binary" ]] || {
    echo "missing current authority migration binary: $migrate_binary" >&2
    exit 1
  }
  [[ -x "$sdk_python" ]] || {
    echo "missing immutable SDK 0.2 fixture interpreter: $sdk_python" >&2
    exit 1
  }
  artifact_test_root="$(mktemp -d)"
  artifact_test_admin_dsn="$admin_dsn"
  artifact_test_sdk_python="$sdk_python"
  trap cleanup_artifact_test EXIT
  start_fake_resend 18080
  run_artifact_case baseline28 "$baseline_image" 28 18028 "$migrate_binary"
  run_artifact_case bridge28 "$bridge_image" 28 18128 "$migrate_binary"
  cmp "${artifact_test_root}/baseline28-compatibility.json" \
    "${artifact_test_root}/bridge28-compatibility.json"
  baseline_transcript_sha="$(<"${artifact_test_root}/baseline28-compatibility.sha256")"
  bridge_transcript_sha="$(<"${artifact_test_root}/bridge28-compatibility.sha256")"
  [[ "$baseline_transcript_sha" == "$bridge_transcript_sha" ]]
  run_artifact_case bridge29 "$bridge_image" 29 18129 "$migrate_binary"
  prove_baseline_rejects_core29 "$baseline_image" "$migrate_binary"
  jq -n \
    --slurpfile baseline "${artifact_test_root}/baseline28-result.json" \
    --slurpfile bridge28 "${artifact_test_root}/bridge28-result.json" \
    --slurpfile bridge29 "${artifact_test_root}/bridge29-result.json" \
    --arg baseline_sha "$baseline_transcript_sha" \
    --arg bridge_sha "$bridge_transcript_sha" \
    --arg sdk_sha "$PINNED_SDK_0_2_SHA256" \
    '{
      baseline_core28: $baseline[0],
      bridge_core28: $bridge28[0],
      bridge_core29: $bridge29[0],
      baseline_core29_rejected: true,
      core28_wire_equivalent: ($baseline_sha == $bridge_sha),
      core28_compatibility: {
        fixture_version: "mcp2026-r0-core28-v1",
        sdk_0_2_sha256: $sdk_sha,
        baseline_transcript_sha256: $baseline_sha,
        bridge_transcript_sha256: $bridge_sha
      }
    }' >"$output_path"
  echo "R0 artifact-level Core 28/29 verification passed"
}

emit_outputs() {
  require_pinned_authority
  local output_path="${1:-${GITHUB_OUTPUT:-}}"
  [[ -n "$output_path" ]] || {
    echo "usage: $0 outputs OUTPUT_PATH" >&2
    exit 2
  }
  {
    printf 'bridge_id=%s\n' "$(bridge_id)"
    printf 'bridge_suffix=%s\n' "$(bridge_suffix)"
    printf 'runtime_version=%s\n' "$(bridge_version)"
    printf 'patch_sha256=%s\n' "$(patch_sha256)"
    printf 'core_schema_sha256=%s\n' "$(hash_core_schema)"
    printf 'mcp_contract_sha256=%s\n' "$PINNED_MCP_CONTRACT_SHA256"
    printf 'source_revision=%s\n' "$PINNED_SOURCE_REVISION"
    printf 'baseline_image=%s\n' "$PINNED_BASELINE_IMAGE"
    printf 'build_time=%s\n' "$REPRODUCIBLE_BUILD_TIME"
  } >>"$output_path"
}

usage() {
  cat >&2 <<'USAGE'
usage:
  build_mcp2026_legacy_bridge.sh prepare SOURCE_DIR METADATA_JSON
  build_mcp2026_legacy_bridge.sh build-binary SOURCE_DIR OUTPUT_DIR METADATA_JSON
  build_mcp2026_legacy_bridge.sh verify-image IMAGE METADATA_JSON
  build_mcp2026_legacy_bridge.sh stage-image BINARY OUTPUT_DIR
  build_mcp2026_legacy_bridge.sh test-artifacts BASELINE_IMAGE BRIDGE_IMAGE ADMIN_DSN MIGRATE_BINARY SDK_PYTHON OUTPUT_JSON
  build_mcp2026_legacy_bridge.sh outputs OUTPUT_PATH
USAGE
  exit 2
}

command_name="${1:-}"
case "$command_name" in
  prepare)
    [[ $# == 3 ]] || usage
    prepare_source "$2" "$3"
    ;;
  build-binary)
    [[ $# == 4 ]] || usage
    build_binary "$2" "$3" "$4"
    ;;
  verify-image)
    [[ $# == 3 ]] || usage
    verify_image "$2" "$3"
    ;;
  stage-image)
    [[ $# == 3 ]] || usage
    stage_image_context "$2" "$3"
    ;;
  test-artifacts)
    [[ $# == 7 ]] || usage
    test_artifacts "$2" "$3" "$4" "$5" "$6" "$7"
    ;;
  outputs)
    [[ $# == 2 ]] || usage
    emit_outputs "$2"
    ;;
  *) usage ;;
esac
