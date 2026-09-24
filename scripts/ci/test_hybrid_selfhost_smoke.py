#!/usr/bin/env python3
"""Guards for the hybrid image smoke: credential safety and its own isolation."""
import contextlib
import io
import json
import os
import secrets
import subprocess
import tempfile
import traceback
import unittest
from pathlib import Path
from unittest.mock import patch

from hybrid_selfhost_smoke import run_command, try_command


class ProcessDiagnosticsTest(unittest.TestCase):
    # The smoke runs Compose commands whose environment carries the sandbox
    # passwords. A process failure must never chain argv or output into a CI
    # log, where the password would then be in the build record forever.
    def test_failure_matrix_never_logs_credentials(self):
        env = {name: secrets.token_hex(32) for name in
               ['NERVE_API_KEY', 'POSTGRES_PASSWORD', 'STALWART_PASSWORD']}
        secret_output = ' '.join(env.values()).encode()
        command = ['docker', 'compose', 'exec', '-T', '-e',
                   f'POSTGRES_PASSWORD={env["POSTGRES_PASSWORD"]}',
                   'cortex', '/app/neuralmail', 'hybrid', 'status']
        cases = {
            'timeout': subprocess.TimeoutExpired(command, 900, output=secret_output, stderr=secret_output),
            'spawn': OSError(5, secret_output.decode(), ' '.join(command)),
            'process': subprocess.CalledProcessError(1, command, output=secret_output, stderr=secret_output),
            'nonzero': subprocess.CompletedProcess(command, 1, b'', secret_output),
            'truncate': subprocess.CompletedProcess(command, 1, b'', secret_output + b'x' * 3980),
        }
        for kind, outcome in cases.items():
            with self.subTest(kind=kind):
                kwargs = {'side_effect': outcome} if isinstance(outcome, Exception) else {'return_value': outcome}
                output = io.StringIO()
                with patch('hybrid_selfhost_smoke.subprocess.run', **kwargs), \
                        contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
                    try:
                        run_command(command, env)
                    except RuntimeError as error:
                        print(f'FAIL {error}')
                        traceback.print_exc()
                    else:
                        self.fail('Process failure was not propagated')
                rendered = output.getvalue()
                self.assertIn('RuntimeError', rendered)
                for secret in env.values():
                    self.assertNotIn(secret, rendered)
                    self.assertNotIn(secret[-20:], rendered)
                self.assertNotIn('POSTGRES_PASSWORD=', rendered)

    # try_command exists for the one command the smoke expects to fail. It
    # must hand the refusal back instead of raising, or the assertion about
    # what an unpaired host reports could never run.
    def test_expected_refusal_is_returned_not_raised(self):
        refusal = subprocess.CompletedProcess(['docker'], 1, b'', b'hybrid: hybrid installation not connected\n')
        with patch('hybrid_selfhost_smoke.subprocess.run', return_value=refusal):
            result = try_command(['docker'], {})
        self.assertEqual(result.returncode, 1)
        self.assertIn(b'not connected', result.stderr)

    def test_success_preserves_output_and_timeout(self):
        result = subprocess.CompletedProcess(['docker'], 0, b'healthy\n', b'')
        with patch('hybrid_selfhost_smoke.subprocess.run', return_value=result) as runner:
            self.assertEqual(run_command(['docker'], {}), 'healthy\n')
            self.assertEqual(runner.call_args.kwargs['timeout'], 900)


class IsolationTest(unittest.TestCase):
    # The smoke must never adopt or tear down a developer's running sandbox.
    # It builds its own project name and clears inherited Compose and Nerve
    # variables before doing anything.
    def test_smoke_isolates_its_compose_project(self):
        source = (__import__('pathlib').Path(__file__).with_name('hybrid_selfhost_smoke.py')).read_text()
        self.assertIn("project = 'nerve-hybrid-smoke-' + secrets.token_hex(4)", source)
        self.assertIn("'COMPOSE_PROJECT_NAME': project", source)
        self.assertIn("COMPOSE_DISABLE_ENV_FILE", source)
        self.assertIn("startswith(('COMPOSE_', 'NERVE_', 'NM_', 'STALWART_'))", source)
        self.assertIn("{'services': {'cortex': published}}", source)
        self.assertIn("run('docker', 'pull', '--platform', 'linux/amd64', args.image)", source)
        self.assertIn("startup += ['--pull', 'always', '--no-build'] if args.image else ['--build']", source)
        self.assertIn("recreate += ['--pull', 'never', '--no-build'] if args.image else ['--build']", source)
        self.assertIn("r'@sha256:[0-9a-f]{64}$'", source)
        # The teardown must not carry --profile full or a project it did not
        # create, and must remove the volumes it made.
        self.assertIn("'down', '-t', '5', '-v', '--remove-orphans'", source)

    def test_published_image_override_does_not_introduce_services(self):
        root = Path(__file__).parents[2]
        image = 'ghcr.io/example/runtime@sha256:' + 'a' * 64
        override_body = {'services': {'cortex': {
            'build': None, 'image': image, 'platform': 'linux/amd64',
        }}}
        env = os.environ.copy()
        env.update({
            'NERVE_API_KEY': secrets.token_hex(32),
            'POSTGRES_PASSWORD': secrets.token_hex(32),
            'STALWART_PASSWORD': secrets.token_hex(32),
        })
        with tempfile.TemporaryDirectory(prefix='hybrid-compose-model-') as directory:
            override = Path(directory) / 'published-image.json'
            override.write_text(json.dumps(override_body))
            command = ['docker', 'compose', '--env-file', '/dev/null',
                       '-f', str(root / 'docker-compose.yml'),
                       '-f', str(override), 'config', '--services']
            rendered = subprocess.run(command, cwd=root, env=env, check=True,
                                      text=True, stdout=subprocess.PIPE).stdout.splitlines()
        self.assertNotIn('migrate', rendered)
        self.assertEqual(set(rendered), {'cortex', 'postgres', 'redis', 'mailpit'})

    def test_release_publish_proves_the_pushed_digest_before_release(self):
        workflow = (Path(__file__).parents[2] / '.github/workflows/docker-publish.yml').read_text()
        publish_job, candidate_job = workflow.split('  candidate:', 1)
        smoke = 'Prove hybrid self-host lifecycle against published release bytes'
        self.assertIn('id: publish-image', publish_job)
        self.assertIn(smoke, publish_job)
        self.assertIn('steps.publish-image.outputs.digest', publish_job)
        self.assertLess(publish_job.index(smoke), publish_job.index('gh release create'))
        self.assertNotIn(smoke, candidate_job)


if __name__ == '__main__':
    unittest.main()
