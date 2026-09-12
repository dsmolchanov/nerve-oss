#!/usr/bin/env python3
"""Image-level smoke for a hybrid installation's durable local state.

A hybrid pairing is the first piece of runtime state that does not live in
Postgres: it is a private key in a file. That makes three things worth
proving against the published image rather than in a unit test.

  clean start    A host configured for hybrid but not yet paired must serve.
                 Setup is a multi-step, two-person process, and an operator
                 cannot complete it if the daemon refuses to run meanwhile.

  backup/restore A database dump does not carry the installation, because the
                 key was never in the database. Restoring a backup onto a new
                 host therefore produces a runtime that serves but is not
                 paired, and the operator must be told so rather than discover
                 it when mail stops.

  upgrade        Replacing the container must not regenerate the key. The key
                 is admitted to Cloud's protected inventory by hand; losing it
                 on every upgrade would mean re-admitting it every time.

Only Python's standard library. This never selects or drops an existing
Compose project.
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
STATE_PATH = '/var/lib/nerve/hybrid-installation.json'


def run_command(command, env, timeout=900):
    # Process exceptions include argv and output; never chain them into CI
    # tracebacks, where a password would end up in the log.
    try:
        result = subprocess.run(command, env=env, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise RuntimeError(f'Command timed out after {timeout} seconds') from None
    except (OSError, subprocess.SubprocessError):
        raise RuntimeError('Command could not complete') from None
    if result.returncode:
        detail = result.stderr.decode(errors='replace')
        for name in ['NERVE_API_KEY', 'POSTGRES_PASSWORD', 'STALWART_PASSWORD']:
            value = env.get(name)
            if value:
                detail = detail.replace(value, '[redacted]')
        # Redact before truncating, so the boundary cannot expose part of a key.
        raise RuntimeError(f'Command failed ({result.returncode}): {detail[-4000:]}')
    return result.stdout.decode()


def try_command(command, env, timeout=120):
    """Run a command that is expected to fail, and return the result.

    Unlike run_command this never raises on a nonzero exit: the caller is
    asserting on the refusal itself.
    """
    return subprocess.run(command, env=env, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, timeout=timeout)


def wait_for(check, description, timeout=120):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if check():
                return True
        except (OSError, urllib.error.URLError, RuntimeError):
            pass
        time.sleep(1)
    raise RuntimeError(f'Timed out: {description}')


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.parse_args()
    os.chdir(ROOT)
    project = 'nerve-hybrid-smoke-' + secrets.token_hex(4)
    with tempfile.TemporaryDirectory(prefix='nerve-hybrid-selfhost-') as tmp:
        tmp = Path(tmp)
        env = os.environ.copy()
        # Explicit file, env and project isolate this run from a developer's .env.
        for key in list(env):
            if key.startswith(('COMPOSE_', 'NERVE_', 'NM_', 'STALWART_')) or key == 'POSTGRES_PASSWORD':
                del env[key]
        env.update({'COMPOSE_PROJECT_NAME': project,
                    'COMPOSE_FILE': str(ROOT / 'docker-compose.yml'),
                    'COMPOSE_ENV_FILES': str(tmp / '.env'), 'COMPOSE_DISABLE_ENV_FILE': '1',
                    'NERVE_API_KEY': secrets.token_hex(32),
                    'POSTGRES_PASSWORD': secrets.token_hex(32),
                    'STALWART_PASSWORD': secrets.token_hex(32)})
        (tmp / '.env').write_text('')
        for key in ['NERVE_HTTP_PORT', 'NERVE_VIEWER_PORT', 'NERVE_SMTP_PORT', 'NERVE_INBOUND_PORT',
                    'NERVE_JMAP_PORT', 'NERVE_SUBMISSION_PORT', 'NERVE_IMAPS_PORT']:
            env[key] = str(free_port())

        def run(*command, timeout=900):
            return run_command(list(command), env, timeout=timeout)

        def compose(*command, timeout=900):
            return run('docker', 'compose', *command, timeout=timeout)

        def cortex(*command):
            return compose('exec', '-T', 'cortex', *command)

        def ready():
            with urllib.request.urlopen(f'http://127.0.0.1:{env["NERVE_HTTP_PORT"]}/readyz', timeout=3) as response:
                return response.status == 200

        def hybrid_state():
            """The installation file as the container sees it, or None."""
            try:
                raw = cortex('cat', STATE_PATH)
            except RuntimeError:
                return None
            return json.loads(raw)

        try:
            compose('up', '-d', '--build', '--wait', '--wait-timeout', '240')
            wait_for(ready, 'runtime ready with hybrid configured but unpaired')
            if hybrid_state() is not None:
                raise AssertionError('A fresh image already carries an installation')
            # The daemon must serve throughout setup: pairing needs an operator
            # to admit a key and an owner to approve it, and neither can happen
            # against a runtime that refuses to start.
            print('PASS clean start: hybrid configured, not paired, runtime serving', flush=True)

            # Stand in for a completed pairing. Reaching a real Cloud is not
            # this test's job; what it proves is what the image does with the
            # file a pairing leaves behind.
            connect = ['/app/neuralmail', 'hybrid', 'connect',
                       '-cloud-url', 'https://cloud.invalid',
                       '-token-endpoint', 'https://auth.invalid/oauth/token',
                       '-resource', 'https://runtime.invalid/mcp',
                       '-client-id', 'hybrid-smoke-client', '-generation', '1',
                       '-cloud-inbox-id', '11111111-1111-4111-8111-111111111111',
                       '-authority-id', 'cloud.invalid']
            # The pairing cannot complete against an unreachable Cloud, and it
            # is not meant to: the key and parameters must still be written, or
            # an operator could never get past the admission step offline.
            try:
                cortex(*connect)
            except RuntimeError:
                pass
            state = hybrid_state()
            if state is None:
                raise AssertionError('connect left no state for the operator to resume from')
            if state.get('phase') != 'connecting' or not state.get('key', {}).get('kid'):
                raise AssertionError(f'unexpected state after connect: {state.get("phase")}')
            key_id = state['key']['kid']

            mode = cortex('stat', '-c', '%a', STATE_PATH).strip()
            if mode != '600':
                raise AssertionError(f'installation state has mode {mode}, want 600')
            print(f'PASS pairing key {key_id[:12]}… written owner-only inside the image', flush=True)

            # Upgrade: recreate the container from a rebuilt image. The key is
            # admitted to Cloud by hand, so regenerating it on every upgrade
            # would mean re-admitting it every time.
            compose('up', '-d', '--build', '--force-recreate', '--wait', '--wait-timeout', '240')
            wait_for(ready, 'runtime ready after upgrade')
            upgraded = hybrid_state()
            if upgraded is None:
                raise AssertionError('the upgrade destroyed the installation key')
            if upgraded['key']['kid'] != key_id:
                raise AssertionError('the upgrade regenerated the installation key')
            if upgraded.get('version') != state.get('version'):
                raise AssertionError('the upgrade rewrote the state version')
            print('PASS upgrade: container recreated, same installation key', flush=True)

            # A database backup does not carry the installation, because the
            # key was never in the database. An operator restoring onto a new
            # host gets a serving runtime that is not paired.
            compose('stop', 'cortex')
            dump = tmp / 'backup.dump'
            run('bash', 'scripts/selfhost/backup.sh', str(dump))
            if key_id.encode() in dump.read_bytes():
                raise AssertionError('the database dump carries installation key material')
            compose('start', 'cortex')
            wait_for(ready, 'runtime ready after backup')

            # A fresh host restoring that dump: same database, no state volume.
            # It must say plainly that it is not paired, so the operator finds
            # out now rather than when mail stops arriving.
            restored = try_command(['docker', 'compose', 'run', '--rm', '--no-deps', '-T',
                                    '-e', 'NERVE_HYBRID_STATE_PATH=/var/lib/nerve/absent.json',
                                    '--entrypoint', '/app/neuralmail', 'cortex', 'hybrid', 'status'], env)
            if restored.returncode == 0:
                raise AssertionError('a restored host reported an installation it does not have')
            message = restored.stderr.decode(errors='replace')
            if 'not connected' not in message:
                raise AssertionError(f'a restored host did not say it is unpaired: {message[-500:]}')
            print('PASS backup/restore: a database dump carries no installation, '
                  'and a restored host reports itself unpaired', flush=True)
        except Exception as error:
            print(f'FAIL {error}', flush=True)
            raise
        finally:
            subprocess.run(['docker', 'compose', 'down', '-t', '5', '-v', '--remove-orphans'],
                           env=env, check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == '__main__':
    main()
