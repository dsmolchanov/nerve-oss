#!/usr/bin/env python3
"""Build and run real runtime entrypoints against deliberately invalid dependencies."""
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
with tempfile.TemporaryDirectory() as temp:
    env = {key: value for key, value in os.environ.items() if not key.startswith(('NERVE_', 'NM_'))}
    env.update(NM_CLOUD_MODE='true', NERVE_SCHEMA_TRANSITION_MODE='quiescent',
               NM_DB_DSN='not a valid database DSN', NM_REDIS_URL='invalid://',
               NM_CONFIG=str(Path(temp)/'missing.yaml'))
    # Config.Load requires an existing explicit config; use an empty config.
    config = Path(temp)/'config.yaml'
    config.write_text('{}\n')
    env['NM_CONFIG'] = str(config)
    for name in ('neuralmail', 'neuralmaild'):
        binary = Path(temp)/name
        subprocess.run(['go', 'build', '-o', str(binary), './cmd/'+name], cwd=ROOT, check=True, timeout=180)
        for command in (('serve', 'worker') if name == 'neuralmaild' else ()):
            with socket.socket() as sock:
                sock.bind(('127.0.0.1', 0))
                address = '127.0.0.1:' + str(sock.getsockname()[1])
            with tempfile.TemporaryFile() as log:
                process = subprocess.Popen([str(binary), command], env={**env, 'NM_HTTP_ADDR': address}, stdout=log, stderr=log)
                try:
                    if command == 'serve':
                        deadline = time.monotonic()+5
                        while True:
                            try:
                                urllib.request.urlopen(urllib.request.Request('http://'+address+'/mcp', data=b'{}', headers={'Authorization':'Bearer previously-issued-token'}), timeout=.5)
                                raise AssertionError('quiescent server accepted a write')
                            except urllib.error.HTTPError as error:
                                assert error.code == 503 and error.headers['Retry-After'] == '60'
                                error.close()
                                break
                            except urllib.error.URLError:
                                if process.poll() is not None or time.monotonic() >= deadline:
                                    log.seek(0)
                                    raise AssertionError(log.read().decode())
                                time.sleep(.05)
                    else:
                        time.sleep(.3)
                        assert process.poll() is None, 'worker initialized invalid dependencies instead of quiescing'
                    process.send_signal(signal.SIGTERM)
                    assert process.wait(timeout=3) == 0
                finally:
                    if process.poll() is None:
                        process.kill()
                        process.wait()
        ordinary = subprocess.run([str(binary), 'serve' if name == 'neuralmaild' else 'migrate-core'], env={**env, 'NERVE_SCHEMA_TRANSITION_MODE': ''}, capture_output=True, timeout=5)
        assert ordinary.returncode != 0, 'negative control unexpectedly initialized invalid database'
        assert b'schema quiescence' not in ordinary.stderr + ordinary.stdout
        for mode, command in [('typo', 'serve'), ('quiescent', 'migrate-core'), ('quiescent', 'mcp-stdio')]:
            result = subprocess.run([str(binary), command], env={**env, 'NERVE_SCHEMA_TRANSITION_MODE': mode}, capture_output=True, timeout=3)
            assert result.returncode != 0 and b'schema quiescence' in result.stderr
print('runtime denies ingress and suppresses workers; runtime and administrative CLI reject unsafe modes/commands')
