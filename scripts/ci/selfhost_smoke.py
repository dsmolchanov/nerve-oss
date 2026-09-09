#!/usr/bin/env python3
"""Isolated Docker smoke; never selects or drops an existing Compose project.

Uses only Python's standard library. --bind-data uses host directories when
Docker Desktop's VM disk is full; CI exercises the default named volumes.
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


def run_command(command, env, input=None):
    # Process exceptions include argv/output; never chain them into CI tracebacks.
    try:
        result = subprocess.run(command, env=env, input=input, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=900)
    except subprocess.TimeoutExpired:
        raise RuntimeError('Command timed out after 900 seconds') from None
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


def wait_for(check, description, timeout=90):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            result = check()
            if result:
                return result
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(1)
    raise RuntimeError(f"Timed out: {description}")


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


class MCP:
    def __init__(self, port, key):
        self.url = f'http://127.0.0.1:{port}/mcp'
        self.key = key
        self.session = ''
        self.seq = 0
        self.call('initialize', {'protocolVersion': '2025-11-25', 'capabilities': {},
                                'clientInfo': {'name': 'selfhost-smoke', 'version': '1'}})

    def call(self, method, params):
        self.seq += 1
        req = urllib.request.Request(self.url, json.dumps({
            'jsonrpc': '2.0', 'id': self.seq, 'method': method, 'params': params
        }).encode(), headers={'Content-Type': 'application/json', 'Accept': 'application/json',
                            'MCP-Protocol-Version': '2025-11-25',
                            'Authorization': 'Bearer ' + self.key,
                            'MCP-Session-Id': self.session})
        with urllib.request.urlopen(req, timeout=15) as response:
            self.session = response.headers.get('MCP-Session-Id', self.session)
            data = json.load(response)
        assert data.get('id') == self.seq, 'MCP response id mismatch'
        assert 'error' not in data, f'MCP {method} failed: {data.get("error")}'
        result = data['result']
        assert not result.get('isError'), f'MCP {method} tool error'
        return result

    def tool(self, name, arguments):
        return self.call('tools/call', {'name': name, 'arguments': arguments})

    def threads(self, inbox):
        return self.tool('list_threads', {'inbox_id': inbox, 'limit': 100})['threads'] or []


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bind-data', action='store_true')
    args = parser.parse_args()
    os.chdir(ROOT)
    project = 'nerve-smoke-' + secrets.token_hex(4)
    with tempfile.TemporaryDirectory(prefix='nerve-selfhost-') as tmp:
        tmp = Path(tmp)
        env = os.environ.copy()
        # Explicit file/env/project isolate this run from a developer's .env.
        for key in list(env):
            if key.startswith(('COMPOSE_', 'NERVE_', 'NM_', 'STALWART_')) or key == 'POSTGRES_PASSWORD':
                del env[key]
        env.update({'COMPOSE_PROJECT_NAME': project, 'COMPOSE_FILE': str(ROOT / 'docker-compose.yml'),
                    'COMPOSE_ENV_FILES': str(tmp / '.env'), 'COMPOSE_DISABLE_ENV_FILE': '1',
                    'NERVE_API_KEY': secrets.token_hex(32), 'POSTGRES_PASSWORD': secrets.token_hex(32),
                    'STALWART_PASSWORD': secrets.token_hex(32)})
        (tmp / '.env').write_text('')
        for key in ['NERVE_HTTP_PORT', 'NERVE_VIEWER_PORT', 'NERVE_SMTP_PORT', 'NERVE_INBOUND_PORT', 'NERVE_JMAP_PORT', 'NERVE_SUBMISSION_PORT', 'NERVE_IMAPS_PORT']:
            env[key] = str(free_port())
        if args.bind_data:
            override = {'services': {}}
            for service, target in {'postgres': '/var/lib/postgresql/data', 'redis': '/data',
                                    'mailpit': '/data', 'stalwart': '/opt/stalwart/data'}.items():
                directory = tmp / service
                directory.mkdir(mode=0o777)
                directory.chmod(0o777)
                # Compose merges volume entries by target; this replaces only data.
                override['services'][service] = {'volumes': [f'{directory}:{target}']}
            path = tmp / 'bind.json'
            path.write_text(json.dumps(override))
            env['COMPOSE_FILE'] += os.pathsep + str(path)

        def run(*command, input=None):
            return run_command(command, env, input=input)

        def compose(*command):
            return run('docker', 'compose', *command)

        def sql(query, db='neuralmail'):
            return compose('exec', '-T', 'postgres', 'psql', '-U', 'neuralmail', '-d', db,
                           '-X', '-A', '-t', '-v', 'ON_ERROR_STOP=1', '-c', query).strip()

        def ready():
            with urllib.request.urlopen(f'http://127.0.0.1:{env["NERVE_HTTP_PORT"]}/readyz', timeout=3) as response:
                return response.status == 200

        extra = project + '-probe'
        try:
            compose('up', '-d', '--build', '--wait', '--wait-timeout', '180')
            run('make', 'mcp-test')
            req = urllib.request.Request(f'http://127.0.0.1:{env["NERVE_HTTP_PORT"]}/mcp', b'{}',
                                         headers={'Content-Type': 'application/json', 'MCP-Protocol-Version': '2025-11-25'})
            try:
                urllib.request.urlopen(req, timeout=5)
                raise AssertionError('Unauthenticated MCP was accepted')
            except urllib.error.HTTPError as error:
                assert error.code == 401, f'Expected 401, got {error.code}'
            print('PASS sandbox startup, make mcp-test, bearer required', flush=True)

            compose('--profile', 'full', 'up', '-d', '--wait', '--wait-timeout', '120')
            # seed uses SMTP into Stalwart. Outbound replies use the Mailpit sink.
            def seed():
                try:
                    compose('exec', '-T', '-e', 'NERVE_SMTP_HOST=stalwart', '-e', 'NERVE_SMTP_PORT=25',
                            'cortex', '/app/neuralmail', 'seed')
                    return True
                except RuntimeError:
                    return False
            wait_for(seed, 'Stalwart accepts seed')
            client = MCP(env['NERVE_HTTP_PORT'], env['NERVE_API_KEY'])
            inbox = client.call('resources/read', {'uri': 'email://inboxes'})['inbox_ids'][0]
            threads = wait_for(lambda: client.threads(inbox) if len(client.threads(inbox)) == 5 else None,
                               'five SMTP messages ingested through JMAP', 120)
            print('PASS SMTP seed -> JMAP -> five MCP threads', flush=True)

            # Queue a real reply with the worker disabled, then restart all stores.
            compose('stop', 'cortex')
            compose('run', '-d', '--no-deps', '--service-ports', '--name', extra,
                    'cortex', 'serve', '--with-worker=false')
            wait_for(ready, 'runtime without worker')
            client = MCP(env['NERVE_HTTP_PORT'], env['NERVE_API_KEY'])
            payload = {'thread_id': threads[0]['ID'], 'body_or_draft_id': 'Selfhost persistence smoke reply',
                       'idempotency_key': 'selfhost-smoke-reply'}
            # Audit/replay IDs describe each invocation and intentionally change.
            def reply_identity():
                result = client.tool('send_reply', payload)
                return result['message_id'], result['status']
            reply = reply_identity()
            assert sql("SELECT count(*) FROM outbox_messages WHERE status='queued'") == '1'
            # Also prove Redis data is actually on disk across recreation.
            compose('exec', '-T', 'redis', 'redis-cli', 'SET', 'selfhost-smoke', 'persisted')
            run('docker', 'stop', '-t', '90', extra)
            run('docker', 'rm', extra)
            before = sql('SELECT count(*) FROM messages')
            compose('--profile', 'full', 'restart')
            wait_for(ready, 'runtime after restart')
            client = MCP(env['NERVE_HTTP_PORT'], env['NERVE_API_KEY'])
            assert reply_identity() == reply, 'Idempotent reply changed'
            wait_for(lambda: sql("SELECT count(*) FROM outbox_messages WHERE status='sent'") == '1', 'queued reply delivered')
            assert sql('SELECT count(*) FROM messages') == before
            assert reply_identity() == reply, 'Delivered retry changed'
            assert sql('SELECT count(*) FROM outbox_messages') == '1'
            assert compose('exec', '-T', 'redis', 'redis-cli', 'GET', 'selfhost-smoke').strip() == 'persisted'
            def viewer():
                with urllib.request.urlopen(f'http://127.0.0.1:{env["NERVE_VIEWER_PORT"]}/api/v1/messages', timeout=5) as response:
                    data = json.load(response)
                return data if data.get('total') == 1 else None
            mail = wait_for(viewer, 'one reply visible in Mailpit')
            message_id = mail['messages'][0]['ID']
            with urllib.request.urlopen(f'http://127.0.0.1:{env["NERVE_VIEWER_PORT"]}/api/v1/message/{message_id}', timeout=5) as response:
                assert 'Selfhost persistence smoke reply' in json.load(response)['Text']
            print('PASS pending outbox, Redis and messages survive restart; one delivered reply on retry', flush=True)

            baseline = client.threads(inbox)
            compose('stop', 'cortex')
            dump = tmp / 'backup.dump'
            run('bash', 'scripts/selfhost/backup.sh', str(dump))
            original_dump = dump.read_bytes()
            try:
                run('bash', 'scripts/selfhost/backup.sh', str(dump))
                raise AssertionError('Backup overwrote an existing file')
            except RuntimeError:
                pass
            assert dump.read_bytes() == original_dump
            run('bash', 'scripts/selfhost/restore.sh', str(dump), 'restore_smoke')
            for target in ['neuralmail', 'restore_smoke']:
                try:
                    run('bash', 'scripts/selfhost/restore.sh', str(dump), target)
                    raise AssertionError('Restore accepted an unsafe/existing target')
                except RuntimeError:
                    pass
            # Run from /tmp without configs/migrations in cwd, using the restored DB.
            dsn = f'postgres://neuralmail:{env["POSTGRES_PASSWORD"]}@postgres:5432/restore_smoke?sslmode=disable'
            compose('run', '-d', '--no-deps', '--service-ports', '--name', extra, '-w', '/tmp',
                    '-e', 'NERVE_CONFIG=/tmp/no-config.yaml', '-e', 'NERVE_REDIS_URL=redis://redis:6379/0',
                    '-e', 'NERVE_DB_DSN=' + dsn,
                    'cortex', 'serve', '--with-worker=false')
            wait_for(ready, 'restored runtime')
            restored = MCP(env['NERVE_HTTP_PORT'], env['NERVE_API_KEY'])
            assert restored.threads(inbox) == baseline, 'Restored list_threads differs'
            assert sql("SELECT count(*) FROM outbox_messages WHERE status='sent'", 'restore_smoke') == '1'
            print('PASS pg_dump -> new database -> pg_restore -> identical list_threads and outbox', flush=True)
            run('docker', 'stop', '-t', '90', extra)
            run('docker', 'rm', extra)
            compose('exec', '-T', 'postgres', 'createdb', '-U', 'neuralmail', 'restore_empty')
            compose('run', '-d', '--no-deps', '--service-ports', '--name', extra, '-w', '/tmp',
                    '-e', 'NERVE_CONFIG=/tmp/no-config.yaml', '-e', 'NERVE_REDIS_URL=redis://redis:6379/0',
                    '-e', 'NERVE_DB_DSN=' + dsn.replace('/restore_smoke?', '/restore_empty?'),
                    'cortex', 'serve', '--with-worker=false')
            wait_for(ready, 'fresh database migrated from empty cwd')
            assert sql("SELECT to_regclass('public.messages') IS NOT NULL", 'restore_empty') == 't'
            print('PASS standalone binary applies bundled migrations outside checkout', flush=True)
        except Exception as error:
            print(f'FAIL {error}', flush=True)
            raise
        finally:
            subprocess.run(['docker', 'rm', '-f', extra], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(['docker', 'compose', '--profile', 'full', 'down', '-t', '5', '-v', '--remove-orphans'],
                           env=env, check=True, stdout=subprocess.DEVNULL)


if __name__ == '__main__':
    main()
