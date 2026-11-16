#!/usr/bin/env python3
"""Foreground fixture supervisor: real PG/API, test-only no-network Verify.

Never reads real credentials, .env or existing database configuration. This is
separate from the real Verify runtime and must not be evidence of SMS delivery.
"""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import uuid
from types import SimpleNamespace

import local


def main():
    os.umask(0o077)
    resume = len(sys.argv) == 3 and sys.argv[1] == '--resume'
    local.require(len(sys.argv) == 1 or resume, 'Usage: mobile_fixture.py [--resume /absolute/fixture-root]')
    root = local.path_arg(sys.argv[2]) if resume else Path(tempfile.mkdtemp(prefix='mc-mobile-fixture-', dir='/tmp')).resolve()
    state = root / 'runtime'
    credentials, verify = root / 'synthetic-credentials', root / 'synthetic-verify'
    if not resume:
        local.write_new(credentials, ('TWILIO_ACCOUNT_SID=AC' + '0' * 32 + '\nTWILIO_AUTH_TOKEN=' + 'a' * 32 + '\n').encode())
        local.write_new(verify, ('MANCUERNA_VERIFY_SERVICE_SID=VA' + '0' * 32 + '\nMANCUERNA_TEST_ALLOWED_DESTINATIONS=+525555559901,+525555559902\nMANCUERNA_TEST_BUDGET_USD=5\n').encode())
    args = SimpleNamespace(state_dir=str(state), ledger_dir=str(root / 'ledger'), pg_bin='/Applications/Postgres.app/Contents/Versions/latest/bin',
                           credentials_file=str(credentials), verify_file=str(verify), validity_seconds=600, recovery_window_seconds=60, recovery_max_attempts=6)
    pg = fixture = None
    stopping = False
    def stop(*_):
        nonlocal stopping
        stopping = True
    signal.signal(signal.SIGTERM, stop); signal.signal(signal.SIGINT, stop)
    suffix = '-resume-' + uuid.uuid4().hex if resume else ''
    result = {'passed': False, 'scope': 'Synthetic Verify; actual PostgreSQL/API', 'external_verify_requests': 0, 'resumed_same_database_and_ledger': resume}
    try:
        if not resume:
            # Other agents may edit application sources during a native journey.
            # Compile only from a private stable allowlisted copy without .env.
            copy = Path(local.run(['python3', 'tests/mancuerna/copy_sources.py'], 'Source snapshot failed').decode().strip())
            result['source_manifest'] = str(copy.parent / 'source-manifest.json')
            local.CORE = copy
            local.MIGRATIONS = tuple(copy / 'db_migrations/postgres' / p.name for p in local.MIGRATIONS)
            local.setup(args)
        manifest, identity = local.validate_state(state)
        if resume:
            c = local.json_private(state / 'config.json')
            local.require(c['verify']['account_sid'] == 'AC'+'0'*32 and c['verify']['service_sid'] == 'VA'+'0'*32 and c['verify']['password'] == 'a'*32,
                          'Only synthetic fixture resumption is supported')
            previous = local.json_private(root / 'result.json')
            local.require(previous.get('passed') is True and not (state / 'fixture-control.sock').exists() and not (state / 'fixture-ready').exists(), 'Fixture must be stopped cleanly before resumption')
            binary = root / 'mobile-fixture'
            local.require(binary.is_file() and not binary.is_symlink() and binary.stat().st_mode & 0o077 == 0, 'Private original fixture binary required')
            result['source_manifest'] = previous.get('source_manifest')
        else:
            local.run(['go', 'test', '-c', '-tags', 'server,mobilefixture', '-o', root / 'mobile-fixture', './cmd/server'], 'Fixture compilation failed')
        pg = local.postgres(state, identity, Path(manifest['pg_bin']))
        with (root / ('fixture' + suffix + '.log')).open('wb') as log:
            fixture = subprocess.Popen([str(root / 'mobile-fixture'), '-test.run=^TestMancuernaMobileFixture$', '-test.timeout=30m'],
                                       cwd=local.CORE, env={**local.clean_env(), 'MANCUERNA_MOBILE_FIXTURE_DIR': str(state)},
                                       stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT)
            until = time.monotonic() + 30
            while not (state / 'fixture-ready').exists():
                local.require(fixture.poll() is None and not stopping, 'Fixture stopped before readiness')
                local.require(time.monotonic() < until, 'Fixture readiness timed out')
                time.sleep(0.1)
            print(json.dumps({'state': 'ready', 'runtime': str(state), 'control_socket': str(state / 'fixture-control.sock'), 'api': 'http://127.0.0.1:18884/api/mancuerna/v1', 'verify': 'synthetic; no external transport'}), flush=True)
            while fixture.poll() is None and not stopping:
                local.require(pg.poll() is None, 'Fixture PostgreSQL exited')
                time.sleep(0.2)
            if stopping:
                local.stop_child(fixture)
            result['passed'] = fixture.returncode == 0
    except Exception:
        result['error'] = 'Fixture failed; inspect private synthetic logs'
    finally:
        local.stop_child(fixture)
        local.stop_child(pg, signal.SIGINT)
        output = root / ('result' + suffix + '.json')
        local.write_new(output, local.json_bytes(result))
        print(json.dumps({'state': 'stopped', 'result': str(output), 'passed': result['passed']}), flush=True)
    return 0 if result['passed'] else 1


if __name__ == '__main__':
    sys.exit(main())
