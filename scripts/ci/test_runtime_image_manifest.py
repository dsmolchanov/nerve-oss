#!/usr/bin/env python3
"""Check an already built runtime image against its exact build manifest bytes.

Usage: test_runtime_image_manifest.py IMAGE EXPECTED_MANIFEST
This is local extraction/execution validation, not registry provenance proof.
"""
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile


def docker(*args):
    return subprocess.check_output(['docker',*args],timeout=120)


def main():
    image,expected_path=sys.argv[1:]
    expected=Path(expected_path).read_bytes()
    with tempfile.TemporaryDirectory() as temp:
        root=Path(temp)
        container=docker('create','--network','none',image).decode().strip()
        try:
            for name in ('runtime-manifest.json','nerve-runtime'):
                docker('cp',container+':/app/'+name,str(root/name))
        finally:
            docker('rm','-f',container)
        actual=(root/'runtime-manifest.json').read_bytes()
        assert actual==expected,'embedded manifest differs from candidate generator bytes'
        report=json.loads(docker('run','--rm','--network','none','--entrypoint','/app/nerve-runtime',
                                '-e','CORE_SCHEMA_MIN_REQUIRED=1','-e','CORE_SCHEMA_MAX_SUPPORTED=9999',
                                image,'compatibility','--json'))
        assert report['manifest']==json.loads(actual),'native report differs from embedded manifest'
        assert report['executable_sha256']==hashlib.sha256((root/'nerve-runtime').read_bytes()).hexdigest()
        assert report['database_verified'] is False and report['admission_verified'] is False
    print('embedded runtime manifest, native report and executable bytes agree')


if __name__=='__main__':main()
