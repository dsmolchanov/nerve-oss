#!/usr/bin/env python3
"""Run the actual sync collector setup with explicit synthetic command doubles."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / '.github/workflows/sync-to-cloud.yml'


def step(source, name):
    match = re.search(r'^      - name: ' + re.escape(name) + r'\n(.*?)(?=^      - |\Z)', source, re.M | re.S)
    if not match:
        raise ValueError('required actual sync step missing')
    return match[1]


def shell(body):
    match = re.search(r'^        run: \|\n((?:^          .*\n|^\n)+)', body, re.M)
    if not match:
        raise ValueError('required actual sync shell missing')
    return textwrap.dedent(match[1])


class CloudUtilityCollector(unittest.TestCase):
    def setUp(self):
        self.source = WORKFLOW.read_text()
        self.body = step(self.source, 'Qualify Cloud credential utility collector')
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        binary = self.root / 'bin'
        binary.mkdir()
        self.cid = 'a' * 64
        self.env = dict(os.environ, PATH=str(binary) + ':' + os.environ['PATH'],
                        GITHUB_ENV=str(self.root / 'github.env'),
                        COLLECTOR_CONTAINER_ID=self.cid, COLLECTOR_INITIAL_PORT='15432',
                        CAPTURE_ROOT=str(self.root))
        self.stub('sleep', '#!/bin/bash\nexit 0\n')
        self.stub('docker', '''#!/bin/bash
set -eu
printf '%s\\n' "$*" >> "$CAPTURE_ROOT/docker.calls"
[[ "${@: -1}" == "$COLLECTOR_CONTAINER_ID" ]] || exit 1
case "$1" in
  restart) [[ "$*" == "restart --time 5 $COLLECTOR_CONTAINER_ID" && "${FAILURE:-}" != restart ]] ;;
  inspect)
    [[ "${FAILURE:-}" != inspect ]] || exit 1
    case "${FAILURE:-}" in
      bad-port) echo foreign ;;
      range) echo 65536 ;;
      zero) echo 0 ;;
      *) echo 35432 ;;
    esac ;;
  *) exit 1 ;;
esac
''')
        self.stub('psql', '''#!/bin/bash
set -eu
printf '%s\\n' "$*" >> "$CAPTURE_ROOT/psql.calls"
[[ "$PGCONNECT_TIMEOUT" == 3 ]] || exit 1
case "$*" in
  *'SELECT 1'*) [[ "$1" == *:15432/* ]] || exit 1; if [[ "${FAILURE:-}" == initial ]]; then echo 0; else echo 1; fi ;;
  *'ALTER SYSTEM'*) [[ "$1" == *:15432/* && "${FAILURE:-}" != preload ]] || exit 1; [[ "$*" == *"shared_preload_libraries='pg_stat_statements'"* ]] ;;
  *) [[ "$1" == *:35432/* && "$*" == *'pg_stat_statements.track_utility'* && "$*" == *"IN('top','all')"* ]] || exit 1; if [[ "${FAILURE:-}" == final ]]; then echo f; else echo t; fi ;;
esac
''')
        self.stub('go', '''#!/bin/bash
set -eu
[[ "$NM_REQUIRE_DB" == 1 && "$NM_TEST_DB_DSN" == *:35432/* ]] || exit 1
printf '%s\\n' "$*" >> "$CAPTURE_ROOT/go.calls"
''')

    def stub(self, name, code):
        path = self.root / 'bin' / name
        path.write_text(code)
        path.chmod(0o700)

    def execute(self, **extra):
        path = Path(self.env['GITHUB_ENV'])
        path.unlink(missing_ok=True)
        return subprocess.run(['bash', '-e', '-c', shell(self.body)],
                              env=dict(self.env, **extra), capture_output=True,
                              text=True, timeout=10)

    def test_actual_setup_rebinds_before_complete_uncached_cloud_suite(self):
        result = self.execute()
        self.assertEqual(result.returncode, 0, result.stderr)
        values = dict(line.split('=', 1) for line in Path(self.env['GITHUB_ENV']).read_text().splitlines())
        self.assertEqual(values, {'NM_TEST_DB_DSN': 'postgres://neuralmail:neuralmail@127.0.0.1:35432/neuralmail?sslmode=disable'})
        body = step(self.source, 'Build and test cloud')
        self.assertNotIn('NM_TEST_DB_DSN:', body)
        self.assertIn('NM_REQUIRE_DB: "1"', body)
        result = subprocess.run(['bash', '-e', '-c', shell(body)],
                                env=dict(self.env, **values, NM_REQUIRE_DB='1'),
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root / 'go.calls').read_text().splitlines(),
                         ['build ./...', 'test ./... -count=1 -timeout=30m'])
        calls = (self.root / 'docker.calls').read_text().splitlines()
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0], 'restart --time 5 ' + self.cid)

    def test_every_failure_refuses_without_exporting_a_stale_endpoint(self):
        for failure in ('initial', 'preload', 'restart', 'inspect', 'bad-port', 'range', 'zero', 'final'):
            with self.subTest(failure=failure):
                result = self.execute(FAILURE=failure)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(Path(self.env['GITHUB_ENV']).exists())
        for key, value in [('COLLECTOR_CONTAINER_ID', 'foreign'),
                           ('COLLECTOR_INITIAL_PORT', 'foreign'),
                           ('COLLECTOR_INITIAL_PORT', '0'),
                           ('COLLECTOR_INITIAL_PORT', '65536')]:
            with self.subTest(key=key, value=value):
                self.assertNotEqual(self.execute(**{key: value}).returncode, 0)
                self.assertFalse(Path(self.env['GITHUB_ENV']).exists())

    def test_required_guard_order_and_contract_wiring(self):
        for name in ('Qualify Cloud credential utility collector', 'Build and test cloud', 'Create Pull Request'):
            self.assertIn("if: steps.changes.outputs.has_changes == 'true'", step(self.source, name))
        self.assertIn('timeout-minutes: 3', self.body)
        self.assertIn('timeout-minutes: 45', self.source)
        self.assertIn('${{ job.services.postgres.id }}', self.body)
        self.assertIn("${{ job.services.postgres.ports['5432'] }}", self.body)
        self.assertLess(self.source.index('name: Qualify Cloud credential utility collector'), self.source.index('name: Build and test cloud'))
        self.assertLess(self.source.index('name: Build and test cloud'), self.source.index('name: Create Pull Request'))
        ci = (ROOT / '.github/workflows/ci.yml').read_text()
        self.assertIn('python3 -S scripts/ci/test_sync_cloud_utility_collector.py', ci)
        self.assertIn('oss/scripts/sync/resolve_cloud_base.sh', self.source)
        self.assertIn('oss/scripts/sync/verify_exact_mirror.sh', self.source)


if __name__ == '__main__':
    unittest.main()
