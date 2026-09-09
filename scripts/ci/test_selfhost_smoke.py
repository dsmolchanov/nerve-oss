#!/usr/bin/env python3
"""Credential-safe diagnostics for both restored-database process consumers."""
import contextlib
import io
import secrets
import subprocess
import traceback
import unittest
from unittest.mock import patch

from selfhost_smoke import run_command


class ProcessDiagnosticsTest(unittest.TestCase):
    def test_failure_matrix_never_logs_credentials(self):
        env = {name: secrets.token_hex(32) for name in
               ['NERVE_API_KEY', 'POSTGRES_PASSWORD', 'STALWART_PASSWORD']}
        secret_output = ' '.join(env.values()).encode()
        for database in ['restore_smoke', 'restore_empty']:
            command = ['docker', 'compose', 'run', '-e',
                       f'NERVE_DB_DSN=postgres://neuralmail:{env["POSTGRES_PASSWORD"]}@postgres:5432/{database}',
                       'cortex', 'serve', '--with-worker=false']
            cases = {
                'timeout': subprocess.TimeoutExpired(command, 900, output=secret_output, stderr=secret_output),
                'spawn': OSError(5, secret_output.decode(), ' '.join(command)),
                'process': subprocess.CalledProcessError(1, command, output=secret_output, stderr=secret_output),
                'nonzero': subprocess.CompletedProcess(command, 1, b'', secret_output),
                'truncate': subprocess.CompletedProcess(command, 1, b'', secret_output + b'x' * 3980),
            }
            for kind, outcome in cases.items():
                with self.subTest(database=database, kind=kind):
                    kwargs = {'side_effect': outcome} if isinstance(outcome, Exception) else {'return_value': outcome}
                    output = io.StringIO()
                    with patch('selfhost_smoke.subprocess.run', **kwargs), contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
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
                    self.assertNotIn('NERVE_DB_DSN=postgres', rendered)

    def test_success_preserves_output_and_timeout(self):
        result = subprocess.CompletedProcess(['docker'], 0, b'healthy\n', b'')
        with patch('selfhost_smoke.subprocess.run', return_value=result) as runner:
            self.assertEqual(run_command(['docker'], {}), 'healthy\n')
            self.assertEqual(runner.call_args.kwargs['timeout'], 900)


if __name__ == '__main__':
    unittest.main()
