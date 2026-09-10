#!/usr/bin/env python3
"""Execute the documented upgrade commands without starting services or sending mail."""
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


def block(document, marker):
    match = re.search(r'<!-- ' + marker + r' -->\s*```sh\n(.*?)\n```', document, re.S)
    if not match:
        raise AssertionError(f"Missing executable documentation block: {marker}")
    return match.group(1)


class UpgradeDocumentationTest(unittest.TestCase):
    def test_preflight_precedes_delivery_and_failure_stops_cutover(self):
        document = (ROOT / 'docs/SELF_HOSTING.md').read_text()
        preflight = block(document, 'upgrade-preflight')
        enable = block(document, 'upgrade-enable-delivery')
        self.assertLess(document.index('<!-- upgrade-preflight -->'),
                        document.index('<!-- upgrade-enable-delivery -->'))
        docker = shutil.which('docker')
        self.assertIsNotNone(docker, 'Docker Compose is required; no daemon is needed')
        for fail in ('0', '1'):
            for separator in (':', ';'):
                with self.subTest(mcp_failure=fail, separator=separator), tempfile.TemporaryDirectory() as tmp:
                    directory = Path(tmp)
                    # Preserve an existing deployment override and project/profile settings.
                    base_override = directory / 'deployment.json'
                    base_override.write_text(json.dumps({'services': {'cortex': {
                        'environment': {'UPGRADE_DOC_TEST': 'preserved'}}}}))
                    wrapper = directory / 'docker'
                    wrapper.write_text('''#!/usr/bin/env python3
import json, os, subprocess, sys
result = subprocess.run([os.environ['REAL_DOCKER'], 'compose', 'config', '--format', 'json'],
                        check=True, capture_output=True, text=True)
config = json.loads(result.stdout)
with open(os.environ['TRACE'], 'a') as trace:
    trace.write(json.dumps({'args': sys.argv[1:], 'command': config['services']['cortex']['command'],
                           'project': config['name'],
                           'preserved': config['services']['cortex']['environment']['UPGRADE_DOC_TEST']}) + '\\n')
''')
                    wrapper.chmod(0o755)
                    make = directory / 'make'
                    make.write_text('''#!/usr/bin/env python3
import json, os, sys
assert sys.argv[1:] == ['mcp-test']
with open(os.environ['TRACE'], 'a') as trace:
    trace.write(json.dumps({'mcp-test': True}) + '\\n')
sys.exit(int(os.environ['FAIL_MCP']))
''')
                    make.chmod(0o755)
                    env = {**os.environ, 'PATH': tmp + os.pathsep + os.environ['PATH'],
                           'REAL_DOCKER': docker, 'TRACE': str(directory / 'trace'),
                           'FAIL_MCP': fail, 'TMPDIR': tmp,
                           'COMPOSE_PROJECT_NAME': 'upgrade-doc-test', 'COMPOSE_PROFILES': 'full',
                           'COMPOSE_PATH_SEPARATOR': separator,
                           'COMPOSE_FILE': separator.join([str(ROOT / 'docker-compose.yml'), str(base_override)]),
                           'NERVE_API_KEY': '1' * 64, 'POSTGRES_PASSWORD': '2' * 64,
                           'STALWART_PASSWORD': '3' * 64}
                    result = subprocess.run(['sh', '-c', preflight + '\n' + enable],
                                            cwd=ROOT, env=env, capture_output=True, text=True)
                    self.assertEqual(result.returncode, int(fail), result.stderr)
                    events = [json.loads(line) for line in (directory / 'trace').read_text().splitlines()]
                    self.assertEqual(events[0]['command'], ['serve', '--with-worker=false'])
                    self.assertEqual(events[1], {'mcp-test': True})
                    self.assertEqual(len(events), 2 if fail == '1' else 3)
                    for event in events:
                        if 'command' in event:
                            self.assertEqual(event['project'], 'upgrade-doc-test')
                            self.assertEqual(event['preserved'], 'preserved')
                    if fail == '0':
                        self.assertEqual(events[2]['command'], ['serve', '--with-worker=true'])
                        self.assertIn('--force-recreate', events[2]['args'])


if __name__ == '__main__':
    unittest.main()
