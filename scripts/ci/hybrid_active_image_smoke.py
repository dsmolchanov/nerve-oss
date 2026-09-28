#!/usr/bin/env python3
"""Exercise active hybrid routing in an isolated self-host image.

The Cloud fixture shares only the test runtime's loopback network namespace.
It never reaches a provider: inbound and replies are synthetic, and the
outbound receipt is recorded locally for assertions. This qualifies the image
wiring and persistence; it is not a production Cloud or provider canary.
"""

import argparse
import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time
import urllib.request
import uuid

FIXTURE_PORT = 18089
STATE_PATH = '/var/lib/nerve/hybrid-installation.json'


def complete_sends(path):
    """Return complete JSONL events, never a file still being written."""
    if not path.exists():
        return None
    raw = path.read_text()
    if not raw or not raw.endswith('\n'):
        return None
    try:
        return [json.loads(line) for line in raw.splitlines()]
    except ValueError:
        return None


def observe_single_send(read_events, terminal, quiet_seconds=6, timeout=120,
                        now=time.monotonic, pause=time.sleep):
    """Require one complete event and a terminal outbox throughout a quiet window.

    Waiting for a file to exist can race its writer. Checking the first line
    alone can miss a second provider call after the worker retries. The outbox
    terminal state closes that retry path; the quiet window also observes a
    delayed duplicate in the fixture before teardown.
    """
    deadline = now() + timeout
    stable_since = None
    while now() < deadline:
        events = read_events()
        if events is not None and len(events) > 1:
            raise AssertionError('synthetic reply reached Cloud more than once')
        if events is not None and len(events) == 1 and terminal():
            if stable_since is None:
                stable_since = now()
            if now() - stable_since >= quiet_seconds:
                return events[0]
        else:
            stable_since = None
        pause(0.1)
    raise RuntimeError('synthetic reply never reached one complete, settled send')


def fixture(state_dir):
    """A loopback-only synthetic Cloud with an explicit admission/approval file."""
    state_dir = Path(state_dir)

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            if self.path == '/health':
                return self.answer(200, {'ready': True})
            return self.answer(404, {})

        def answer(self, status, body):
            encoded = json.dumps(body).encode()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def do_POST(self):
            if self.headers.get('Content-Length', '').isdigit():
                size = int(self.headers['Content-Length'])
            else:
                return self.answer(400, {})
            if size > 1 << 20:
                return self.answer(413, {})
            raw = self.rfile.read(size)
            admission = json.loads((state_dir / 'admission.json').read_text()) if (state_dir / 'admission.json').exists() else {}
            if self.path == '/oauth/token':
                from urllib.parse import parse_qs
                assertion = parse_qs(raw.decode()).get('client_assertion', [''])[0]
                try:
                    header = json.loads(base64.urlsafe_b64decode(assertion.split('.')[0] + '==='))
                except (ValueError, IndexError):
                    return self.answer(401, {'error': 'invalid_client'})
                if header.get('kid') != admission.get('kid'):
                    return self.answer(401, {'error': 'invalid_client'})
                return self.answer(200, {'access_token': 'fixture-token', 'token_type': 'Bearer',
                                         'expires_in': 900, 'scope': 'nerve:email.read nerve:email.reply'})
            if self.headers.get('Authorization') != 'Bearer fixture-token':
                return self.answer(401, {})
            try:
                request = json.loads(raw)
            except ValueError:
                return self.answer(400, {})
            action = self.path.removeprefix('/v1/hybrid/')
            if action == 'begin':
                return self.answer(200, {'pairing': {'id': admission['pairing_id']},
                                         'secret': admission['pairing_secret']})
            if action == 'complete':
                if not admission.get('approved') or request.get('pairing_id') != admission['pairing_id'] or request.get('pairing_secret') != admission['pairing_secret']:
                    return self.answer(404, {})
                return self.answer(200, {'id': admission['installation_id'], 'org_id': admission['org_id']})
            if request.get('installation_id') != admission.get('installation_id') or request.get('inbox_id') != admission.get('inbox_id'):
                return self.answer(403, {})
            if action == 'status':
                return self.answer(200, {'installation_id': admission['installation_id'], 'state': 'active'})
            if action == 'poll':
                if (state_dir / 'acked').exists():
                    return self.answer(200, {'delivery': None})
                return self.answer(200, {'delivery': {
                    'id': admission['delivery_id'], 'org_id': admission['org_id'],
                    'installation_id': admission['installation_id'], 'inbox_id': admission['inbox_id'],
                    'provider_message_id': '<fixture-inbound@example.invalid>',
                    'sender': 'sender@example.invalid', 'recipient': 'dev@local.nerve.email',
                    'subject': 'Synthetic hybrid inbound', 'body': 'Local fixture body',
                    'state': 'leased', 'lease_token': admission['lease_token']}})
            if action == 'ack':
                if request.get('delivery_id') != admission['delivery_id'] or request.get('lease_token') != admission['lease_token']:
                    return self.answer(403, {})
                (state_dir / 'acked').write_text('yes')
                return self.answer(200, {})
            if action == 'send':
                if request.get('kind') != 'reply' or request.get('to') != ['sender@example.invalid']:
                    return self.answer(400, {})
                with (state_dir / 'sends.jsonl').open('a') as events:
                    events.write(json.dumps(request) + '\n')
                return self.answer(200, {
                    'org_id': admission['org_id'], 'installation_id': admission['installation_id'],
                    'operation_key': request['operation_key'], 'status': 'sent',
                    'provider_message_id': '<fixture-reply@example.invalid>', 'technical_units': 1})
            if action == 'receipt':
                return self.answer(404, {})
            return self.answer(404, {})

    ThreadingHTTPServer(('127.0.0.1', FIXTURE_PORT), Handler).serve_forever()


def smoke(image, successor_core_head, predecessor_migration_image):
    from hybrid_selfhost_smoke import free_port, run_command, try_command, wait_for
    from selfhost_smoke import MCP

    root = Path(__file__).resolve().parents[2]
    if image and not __import__('re').search(r'@sha256:[0-9a-f]{64}$', image):
        raise ValueError('--image must be an immutable digest reference')
    if successor_core_head is not None and (not image or successor_core_head < 1 or not predecessor_migration_image):
        raise ValueError('successor schema setup requires the image, a positive Core head and predecessor image')
    if predecessor_migration_image and successor_core_head is None:
        raise ValueError('predecessor image requires successor schema setup')
    if predecessor_migration_image and not __import__('re').search(r'@sha256:[0-9a-f]{64}$', predecessor_migration_image):
        raise ValueError('predecessor image must be an immutable digest reference')
    project = 'nerve-hybrid-active-' + secrets.token_hex(4)
    with tempfile.TemporaryDirectory(prefix='nerve-hybrid-active-') as directory:
        tmp = Path(directory)
        env = os.environ.copy()
        for key in list(env):
            if key.startswith(('COMPOSE_', 'NERVE_', 'NM_', 'STALWART_')) or key == 'POSTGRES_PASSWORD':
                del env[key]
        env.update({'COMPOSE_PROJECT_NAME': project, 'COMPOSE_DISABLE_ENV_FILE': '1',
                    'COMPOSE_ENV_FILES': str(tmp / '.env'),
                    'NERVE_API_KEY': secrets.token_hex(32),
                    'POSTGRES_PASSWORD': secrets.token_hex(32),
                    'STALWART_PASSWORD': secrets.token_hex(32),
                    'NERVE_HTTP_PORT': str(free_port()),
                    'NERVE_VIEWER_PORT': str(free_port()),
                    'NERVE_SMTP_PORT': str(free_port())})
        (tmp / '.env').write_text('')
        override = {'services': {'cortex': {'environment': {
            'NERVE_ALLOW_OUTBOUND': 'true',
            'NERVE_OUTBOUND_DOMAIN_ALLOWLIST': 'example.invalid'}},
            'cloud-fixture': {
            'image': 'python:3.12-alpine', 'network_mode': 'service:cortex',
            'volumes': [f'{root / "scripts/ci/hybrid_active_image_smoke.py"}:/fixture.py:ro',
                        f'{tmp}:/fixture-state'],
            'command': ['python3', '/fixture.py', '--fixture', '/fixture-state']}}}
        if image:
            override['services']['cortex'].update({'build': None, 'image': image, 'platform': 'linux/amd64'})
        if successor_core_head is not None:
            override['services']['cortex']['environment']['NM_MIGRATE_ON_START'] = 'verify'
        override_path = tmp / 'compose.json'
        override_path.write_text(json.dumps(override))
        env['COMPOSE_FILE'] = os.pathsep.join((str(root / 'docker-compose.yml'), str(override_path)))

        def run(*command):
            return run_command(list(command), env)

        def compose(*command):
            return run('docker', 'compose', *command)

        def sql(query, database='neuralmail'):
            return compose('exec', '-T', 'postgres', 'psql', '-U', 'neuralmail', '-d', database,
                           '-X', '-A', '-t', '-v', 'ON_ERROR_STOP=1', '-c', query).strip()

        def ready():
            with urllib.request.urlopen(f'http://127.0.0.1:{env["NERVE_HTTP_PORT"]}/readyz', timeout=3) as response:
                return response.status == 200

        try:
            if image:
                run('docker', 'pull', '--platform', 'linux/amd64', image)
            if successor_core_head is not None:
                compose('up', '-d', '--pull', 'always', '--wait', '--wait-timeout', '240',
                        'postgres', 'redis', 'mailpit')
                run('docker', 'pull', '--platform', 'linux/amd64', predecessor_migration_image)
                predecessor_override = tmp / 'predecessor.json'
                predecessor_override.write_text(json.dumps({'services': {'cortex': {
                    'build': None, 'image': predecessor_migration_image,
                    'platform': 'linux/amd64'}}}))
                successor_files = env['COMPOSE_FILE']
                env['COMPOSE_FILE'] = successor_files + os.pathsep + str(predecessor_override)
                try:
                    for scope, head in (('core', 29), ('cloud', 3)):
                        compose('run', '--rm', '--no-deps', '-T', '--entrypoint',
                                '/app/nerve-migrate', 'cortex', 'up', '--scope', scope, '--to', str(head))
                finally:
                    env['COMPOSE_FILE'] = successor_files
                compose('run', '--rm', '--no-deps', '-T', '--entrypoint', '/app/nerve-migrate',
                        'cortex', 'up', '--scope', 'core', '--to', str(successor_core_head))
            startup = ['up', '-d', '--wait', '--wait-timeout', '240']
            startup += ['--no-build'] if image else ['--build']
            compose(*startup, 'cortex')
            wait_for(ready, 'unpaired runtime ready')
            compose('up', '-d', 'cloud-fixture')
            wait_for(lambda: compose('exec', '-T', 'cortex', 'wget', '-q', '-O', '/dev/null',
                                     f'http://127.0.0.1:{FIXTURE_PORT}/health') == '',
                     'local Cloud fixture ready')
            inbox_id = str(uuid.uuid4())
            admission = {'kid': '', 'approved': False, 'pairing_id': str(uuid.uuid4()),
                         'pairing_secret': secrets.token_urlsafe(32), 'installation_id': str(uuid.uuid4()),
                         'org_id': str(uuid.uuid4()), 'inbox_id': inbox_id,
                         'delivery_id': str(uuid.uuid4()), 'lease_token': str(uuid.uuid4())}
            (tmp / 'admission.json').write_text(json.dumps(admission))
            connect = ['docker', 'compose', 'exec', '-T', 'cortex', '/app/neuralmail', 'hybrid',
                       'connect', '-allow-running', '-cloud-url', f'http://127.0.0.1:{FIXTURE_PORT}',
                       '-token-endpoint', f'http://127.0.0.1:{FIXTURE_PORT}/oauth/token',
                       '-resource', 'https://runtime.example.invalid/mcp', '-client-id', 'fixture-client',
                       '-generation', '1', '-cloud-inbox-id', inbox_id,
                       '-authority-id', 'fixture-cloud', '-local-inbox', 'dev@local.nerve.email']
            refused = try_command(connect, env)
            if refused.returncode == 0:
                raise AssertionError('unadmitted installation connected')
            state = json.loads(compose('exec', '-T', 'cortex', 'cat', STATE_PATH))
            if state.get('phase') != 'connecting':
                raise AssertionError('failed first connect did not preserve pending state')
            admission['kid'] = state['key']['kid']
            admission['approved'] = True
            (tmp / 'admission.json').write_text(json.dumps(admission))
            compose('exec', '-T', 'cortex', '/app/neuralmail', 'hybrid', 'connect', '-allow-running')
            state = json.loads(compose('exec', '-T', 'cortex', 'cat', STATE_PATH))
            if state.get('phase') != 'installed' or state.get('installation_id') != admission['installation_id']:
                raise AssertionError('image did not complete and persist pairing')
            if sql("SELECT inbound_provider || '/' || outbound_provider FROM inboxes WHERE address='dev@local.nerve.email'") != 'hybrid/hybrid':
                raise AssertionError('local mailbox was not routed through hybrid')
            compose('restart', 'cortex')
            wait_for(ready, 'paired runtime ready')
            wait_for(lambda: (tmp / 'acked').exists(), 'synthetic inbound durable ACK')
            if sql("SELECT count(*) FROM messages WHERE internet_message_id='<fixture-inbound@example.invalid>'") != '1':
                raise AssertionError('synthetic inbound was not stored exactly once')
            client = MCP(env['NERVE_HTTP_PORT'], env['NERVE_API_KEY'])
            threads = client.tool('list_threads', {'inbox_id': state['local_mailbox']['inbox_id'], 'limit': 10})['threads']
            if len(threads) != 1:
                raise AssertionError('synthetic inbound did not appear in the local mailbox')
            client.tool('send_reply', {'thread_id': threads[0]['ID'],
                                       'body_or_draft_id': 'Synthetic hybrid reply',
                                       'idempotency_key': 'hybrid-active-smoke-reply'})
            sent = observe_single_send(
                lambda: complete_sends(tmp / 'sends.jsonl'),
                lambda: sql('SELECT count(*) FROM outbox_messages') == '1' and
                        sql("SELECT count(*) FROM outbox_messages WHERE status='sent'") == '1')
            if sent.get('body') != 'Synthetic hybrid reply':
                raise AssertionError('reply body differed at the fixture')
            print('PASS image pairing, active inbound, durable ACK and one synthetic reply', flush=True)

            # A coordinated database backup after active traffic must recover
            # both the inbound message and the settled reply. It deliberately
            # does not recover the installation key: that lives in the host's
            # owner-only state volume, outside PostgreSQL.
            compose('stop', 'cloud-fixture', 'cortex')
            dump = tmp / 'active-backup.dump'
            run('bash', 'scripts/selfhost/backup.sh', str(dump))
            restored_db = 'restore_hybrid_active'
            run('bash', 'scripts/selfhost/restore.sh', str(dump), restored_db)
            if sql("SELECT count(*) FROM messages WHERE internet_message_id='<fixture-inbound@example.invalid>'",
                   restored_db) != '1':
                raise AssertionError('restored database lost the acknowledged inbound message')
            if sql("SELECT count(*) FROM outbox_messages WHERE status='sent'", restored_db) != '1':
                raise AssertionError('restored database lost the settled synthetic reply')
            if sql("SELECT inbound_provider || '/' || outbound_provider FROM inboxes WHERE address='dev@local.nerve.email'",
                   restored_db) != 'hybrid/hybrid':
                raise AssertionError('restored database lost the hybrid mailbox route')
            restored_env = env.copy()
            restored_env['NERVE_DB_DSN'] = ('postgres://neuralmail:' + env['POSTGRES_PASSWORD'] +
                                             '@postgres:5432/' + restored_db + '?sslmode=disable')
            restored = try_command(['docker', 'compose', 'run', '--rm', '--no-deps', '-T',
                                    '-e', 'NERVE_DB_DSN', '-v', '/var/lib/nerve',
                                    '--entrypoint', '/app/neuralmail', 'cortex', 'hybrid', 'status'], restored_env)
            if restored.returncode == 0 or b'not connected' not in restored.stderr:
                raise AssertionError('database-only restore did not refuse an absent installation key')

            # Restart the original host with its preserved state volume. The
            # durable ACK and sent outbox row must not cause another delivery.
            compose('start', 'cortex')
            wait_for(ready, 'paired runtime ready after backup')
            compose('up', '-d', '--force-recreate', 'cloud-fixture')
            wait_for(lambda: compose('exec', '-T', 'cortex', 'wget', '-q', '-O', '/dev/null',
                                     f'http://127.0.0.1:{FIXTURE_PORT}/health') == '',
                     'local Cloud fixture ready after backup')
            if sql("SELECT count(*) FROM messages WHERE internet_message_id='<fixture-inbound@example.invalid>'") != '1':
                raise AssertionError('original host replayed acknowledged inbound after backup')
            observe_single_send(lambda: complete_sends(tmp / 'sends.jsonl'),
                                lambda: sql("SELECT count(*) FROM outbox_messages WHERE status='sent'") == '1')
            print('PASS active backup/restore: recovered mail, absent installation key and no replay', flush=True)
        finally:
            subprocess.run(['docker', 'compose', 'down', '-t', '5', '-v', '--remove-orphans'],
                           env=env, check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--fixture', help=argparse.SUPPRESS)
    parser.add_argument('--image', help='immutable image digest to qualify (default: build checkout)')
    parser.add_argument('--successor-core-head', type=int,
                        help='prepare the isolated Core schema for a published successor image')
    parser.add_argument('--predecessor-migration-image',
                        help='immutable predecessor image used to prepare Cloud 3')
    args = parser.parse_args()
    if args.fixture:
        fixture(args.fixture)
    else:
        smoke(args.image, args.successor_core_head, args.predecessor_migration_image)


if __name__ == '__main__':
    main()
