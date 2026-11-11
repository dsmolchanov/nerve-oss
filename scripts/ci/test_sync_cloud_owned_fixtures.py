#!/usr/bin/env python3
"""Execute actual producer shell with explicit filesystem/command doubles."""
import os
import re
from pathlib import Path
import subprocess
import tempfile
import unittest

from test_sync_cloud_utility_collector import ROOT, WORKFLOW, shell, step


def require_suite_budgets(source):
    # Check actual enclosing YAML metadata, not occurrences inside shell/comments.
    job = re.search(r'^  sync:\n(.*?)(?=^    steps:)', source, re.M | re.S)
    if not job:
        raise ValueError('missing sync job metadata')
    body = step(source, 'Build and test cloud').split('        run:', 1)[0]
    job_values = re.findall(r'^    timeout-minutes: (.+)$', job[1], re.M)
    step_values = re.findall(r'^        timeout-minutes: (.+)$', body, re.M)
    if job_values != ['210'] or step_values != ['180']:
        raise ValueError('finite 210min job and 180min complete-suite step required')
    if int(job_values[0]) - int(step_values[0]) < 30:
        raise ValueError('setup and unconditional cleanup reserve required')


class CloudOwnedFixtures(unittest.TestCase):
    def setUp(self):
        self.source = WORKFLOW.read_text()
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        for directory in ('bin', 'runner', 'oss/scripts/sync', 'cloud/scripts/ci'):
            (self.root / directory).mkdir(parents=True)
        self.env = dict(os.environ, PATH=str(self.root / 'bin') + ':' + os.environ['PATH'],
                        RUNNER_TEMP=str(self.root / 'runner'), CAPTURE_ROOT=str(self.root),
                        HEAD_REF='b' * 40, SYNTHETIC_BASE_REF='a' * 40,
                        GITHUB_OUTPUT=str(self.root / 'github.output'))
        self.write('oss/scripts/sync/resolve_cloud_base.sh', '''#!/bin/bash
set -eu
[[ "$*" == "$CAPTURE_ROOT/oss $CAPTURE_ROOT/cloud $HEAD_REF" && "${FAILURE:-}" != resolver ]] || exit 1
printf '%s\n' "$SYNTHETIC_BASE_REF"
''')
        self.write('oss/scripts/sync/apply_to_cloud.sh', '''#!/bin/bash
set -eu
[[ "$*" == "$CAPTURE_ROOT/oss $CAPTURE_ROOT/cloud $SYNTHETIC_BASE_REF $HEAD_REF $RUNNER_TEMP/nerve-oss-cloud-changed-files.txt" && "${FAILURE:-}" != materializer ]] || exit 1
printf '%s' "${SHARED_CHANGE:-}" > "$5"
''')
        self.write('bin/go', '''#!/bin/bash
set -eu
printf '%s\n' "$*" >> "$CAPTURE_ROOT/go.calls"
[[ "${FAILURE:-}" != "$1" ]]
''')
        self.write('bin/python3', '''#!/bin/bash
set -eu
printf '%s\n' "$*" >> "$CAPTURE_ROOT/python.calls"
[[ "$*" == "-S $CAPTURE_ROOT/cloud/scripts/ci/cleanup_test_postgres.py --ledger $RUNNER_TEMP/nerve-test-postgres.jsonl" && "${FAILURE:-}" != cleanup ]]
''')

    def write(self, path, content):
        target = self.root / path
        target.write_text(content)
        target.chmod(0o700)
        return target

    def run_step(self, name, **extra):
        return subprocess.run(['bash', '-e', '-c', shell(step(self.source, name))],
                              cwd=self.root, env=dict(self.env, **extra),
                              capture_output=True, text=True, timeout=10)

    def test_actual_apply_distinguishes_pin_only_shared_and_already_adopted(self):
        for shared, base, expected in (('', 'a' * 40, 'true'),
                                       ('internal/store/store.go\n', 'b' * 40, 'true'),
                                       ('', 'b' * 40, 'false')):
            with self.subTest(shared=bool(shared), advanced=base != self.env['HEAD_REF']):
                Path(self.env['GITHUB_OUTPUT']).unlink(missing_ok=True)
                result = self.run_step('Apply manifest-owned changes',
                                       SHARED_CHANGE=shared, SYNTHETIC_BASE_REF=base)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(Path(self.env['GITHUB_OUTPUT']).read_text(),
                                 'has_changes=' + expected + '\n')
                report = Path(self.env['RUNNER_TEMP']) / 'nerve-oss-cloud-changed-files.txt'
                self.assertEqual(report.read_text(), shared)

    def test_actual_resolver_and_materializer_failures_cannot_emit_success(self):
        for failure in ('resolver', 'materializer'):
            with self.subTest(failure=failure):
                Path(self.env['GITHUB_OUTPUT']).unlink(missing_ok=True)
                result = self.run_step('Apply manifest-owned changes', FAILURE=failure)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(Path(self.env['GITHUB_OUTPUT']).exists())

    def test_actual_nested_budgets_and_each_missing_or_shortened_boundary(self):
        require_suite_budgets(self.source)
        for old, new in [('    timeout-minutes: 210', '    timeout-minutes: 0'),
                         ('    timeout-minutes: 210', '    timeout-minutes: 45'),
                         ('    timeout-minutes: 210', '    timeout-minutes: 180'),
                         ('    timeout-minutes: 210\n', ''),
                         ('        timeout-minutes: 180', '        timeout-minutes: 0'),
                         ('        timeout-minutes: 180', '        timeout-minutes: 120'),
                         ('        timeout-minutes: 180', '        timeout-minutes: 210'),
                         ('        timeout-minutes: 180\n', '')]:
            with self.subTest(mutation=(old, new)):
                self.assertIn(old, self.source)
                with self.assertRaises(ValueError):
                    require_suite_budgets(self.source.replace(old, new))

    def test_cached_filtered_unbounded_or_shortened_actual_commands_refuse(self):
        original = self.source
        for old, new in [('-timeout=120m', '-timeout=0'),
                         ('-timeout=120m', '-timeout=30m'),
                         ('-timeout=120m', '-timeout=45m'),
                         ('-timeout=120m', '-timeout=90m'),
                         ('-timeout=120m', ''), ('-count=1', ''),
                         ('go test ./...', 'go test ./internal/store'),
                         ('go test ./...', 'go test ./... -run TestOnly')]:
            with self.subTest(mutation=(old, new)):
                self.assertIn(old, original)
                self.source = original.replace(old, new)
                (self.root / 'go.calls').unlink(missing_ok=True)
                with self.assertRaises(AssertionError):
                    self.test_actual_complete_build_and_suite_commands_and_failure_boundaries()
        self.source = original

    def test_actual_complete_build_and_suite_commands_and_failure_boundaries(self):
        result = self.run_step('Build and test cloud')
        self.assertEqual(result.returncode, 0, result.stderr)
        log = self.root / 'go.calls'
        self.assertEqual(log.read_text().splitlines(),
                         ['build ./...', 'test ./... -count=1 -timeout=120m'])
        for failure, expected in [('build', ['build ./...']),
                                  ('test', ['build ./...', 'test ./... -count=1 -timeout=120m'])]:
            with self.subTest(failure=failure):
                log.unlink()
                self.assertNotEqual(self.run_step('Build and test cloud', FAILURE=failure).returncode, 0)
                self.assertEqual(log.read_text().splitlines(), expected)

    def test_actual_cleanup_invokes_only_materialized_sweeper_and_propagates_failure(self):
        self.write('cloud/scripts/ci/cleanup_test_postgres.py', '# reviewed Cloud command double\n')
        for failure, success in [('', True), ('cleanup', False)]:
            with self.subTest(failure=failure):
                result = self.run_step('Verify owned Cloud PostgreSQL fixture cleanup', FAILURE=failure)
                self.assertEqual(result.returncode == 0, success, result.stderr)
        self.assertTrue((self.root / 'python.calls').exists(), 'actual cleanup did not invoke the reviewed sweeper')
        calls = (self.root / 'python.calls').read_text().splitlines()
        self.assertEqual(calls, ['-S ' + str(self.root / 'cloud/scripts/ci/cleanup_test_postgres.py') +
                                ' --ledger ' + str(Path(self.env['RUNNER_TEMP']) / 'nerve-test-postgres.jsonl')] * 2)

    def test_actual_older_cloud_without_sweeper_only_succeeds_without_ledger(self):
        result = self.run_step('Verify owned Cloud PostgreSQL fixture cleanup')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.root / 'python.calls').exists())
        ledger = Path(self.env['RUNNER_TEMP']) / 'nerve-test-postgres.jsonl'
        ledger.write_text('opaque owned intent\n')
        result = self.run_step('Verify owned Cloud PostgreSQL fixture cleanup')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('without its reviewed sweeper', result.stdout)
        self.assertEqual(ledger.read_text(), 'opaque owned intent\n')
        ledger.unlink()
        ledger.symlink_to(self.root / 'nonexistent-target')
        self.assertNotEqual(self.run_step('Verify owned Cloud PostgreSQL fixture cleanup').returncode, 0)
        self.assertTrue(ledger.is_symlink())

    def test_actual_workflow_cleanup_runs_before_guarded_publication(self):
        cleanup = step(self.source, 'Verify owned Cloud PostgreSQL fixture cleanup')
        self.assertIn('if: always()', cleanup)
        self.assertIn('timeout-minutes: 3', cleanup)
        self.assertNotIn('continue-on-error', self.source)
        self.assertNotIn('/tmp/changed_files.txt', self.source)
        names = ['Apply manifest-owned changes', 'Verify exact mirror', 'Advance OSS source authority',
                 'Qualify Cloud credential utility collector', 'Build and test cloud',
                 'Verify owned Cloud PostgreSQL fixture cleanup', 'Create Pull Request']
        positions = [self.source.index('name: ' + name) for name in names]
        self.assertEqual(positions, sorted(positions))
        for name in names[1:5] + ['Create Pull Request']:
            body = step(self.source, name)
            self.assertIn("if: steps.changes.outputs.has_changes == 'true'", body)
            self.assertNotIn('always()', body)
        ci = (ROOT / '.github/workflows/ci.yml').read_text()
        self.assertIn('python3 -S scripts/ci/test_sync_cloud_owned_fixtures.py', ci)
        self.assertIn('python3 -S scripts/ci/test_sync_cloud_utility_collector.py', ci)


if __name__ == '__main__':
    unittest.main()
