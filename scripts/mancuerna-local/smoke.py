#!/usr/bin/env python3
"""Actual local PostgreSQL/server lifecycle with generated, unusable credentials.

Never reads .env, never submits a valid auth/start and never invokes Verify.
Keeps private artifacts for review. Refuses an occupied API listener.
"""
import hashlib
import http.client
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

import local


def main():
    os.umask(0o077)
    root = Path(tempfile.mkdtemp(prefix='mc-local-smoke-', dir='/tmp')).resolve()
    state, ledger = root / 'runtime', root / 'ledger'
    credentials, verify = root / 'synthetic-credentials', root / 'synthetic-verify'
    local.write_new(credentials, ('TWILIO_ACCOUNT_SID=AC' + '0' * 32 + '\nTWILIO_AUTH_TOKEN=' + 'a' * 32 + '\nDATABASE_URL=forbidden.invalid\n').encode())
    local.write_new(verify, ('MANCUERNA_VERIFY_SERVICE_SID=VA' + '0' * 32 + '\nMANCUERNA_TEST_ALLOWED_DESTINATIONS=+525555559901,+525555559902\nMANCUERNA_TEST_BUDGET_USD=5\n').encode())
    passed = []
    result = {'passed': False, 'provider_requests': 0, 'credentials': 'synthetic only', 'cases': passed}
    started = False
    began = time.monotonic()

    def invoke(command, *arguments, success=True):
        p = subprocess.run([sys.executable, str(local.HERE / 'local.py'), command, '--state-dir', str(state), *map(str, arguments)],
                           cwd=local.CORE, env=local.clean_env(), capture_output=True, timeout=240)
        if (p.returncode == 0) != success:
            raise AssertionError('Local smoke command failed: ' + command)
        return json.loads(p.stdout) if success else None

    def request(method, path, body=None):
        conn = http.client.HTTPConnection(*local.ADDRESS, timeout=5)
        try:
            conn.request(method, path, body, {'Content-Type': 'application/json'})
            response = conn.getresponse()
            return response.status, response.read(8192), response.getheader('Cache-Control')
        finally:
            conn.close()

    setup_args = ('--ledger-dir', ledger, '--pg-bin', '/Applications/Postgres.app/Contents/Versions/latest/bin',
                  '--credentials-file', credentials, '--verify-file', verify, '--validity-seconds', '600',
                  '--recovery-window-seconds', '60', '--recovery-max-attempts', '6')
    try:
        prepared = invoke('setup', *setup_args)
        assert prepared['state'] == 'prepared' and not prepared['reused']
        baseline = prepared['ledger']
        assert baseline['StartsReserved'] == 0 and baseline['RemainingStarts'] == 4
        ledger_hash = local.digest(ledger / 'verify-ledger.json')
        config_hash = local.digest(state / 'config.json')
        assert invoke('setup', *setup_args)['reused']
        assert invoke('status')['state'] == 'stopped'
        passed.append('Actual personal migrations, generated roles/keys and idempotent setup')
        running = invoke('start'); started = True
        assert running['state'] == 'running' and running['ledger'] == baseline
        code, body, cache = request('GET', '/api/mancuerna/v1/me')
        assert code == 401 and cache == 'no-store' and json.loads(body)['code'] == 'AUTH_REQUIRED'
        assert request('POST', '/api/mancuerna/v1/auth/start', '{}')[0] == 400
        assert request('POST', '/api/mancuerna/v1/sync/snapshots', '{}')[0] == 401
        assert request('GET', '/api/gyms')[0] == 404
        assert invoke('start')['state'] == 'running'
        passed.append('Loopback server is ready, enforces authentication and rejects malformed start without OTP')
        assert invoke('stop')['state'] == 'stopped'; started = False
        assert invoke('status')['state'] == 'stopped'
        assert not local.server_ready()
        assert invoke('start')['state'] == 'running'; started = True
        assert invoke('status')['ledger'] == baseline
        assert invoke('stop')['state'] == 'stopped'; started = False
        assert local.digest(ledger / 'verify-ledger.json') == ledger_hash
        assert local.digest(state / 'config.json') == config_hash
        passed.append('Stop/restart preserves original keys, configuration and untouched separate ledger')
        # A changed source cannot silently replace a prepared runtime or reset budget.
        invoke('setup', *setup_args[:-1], '7', success=False)
        assert local.digest(state / 'config.json') == config_hash
        assert local.digest(ledger / 'verify-ledger.json') == ledger_hash
        passed.append('Changed setup rejected without replacing runtime or ledger')
        result['passed'] = True
    except Exception:
        result['error'] = 'Local lifecycle smoke failed; private output retained, no secrets printed'
    finally:
        if started:
            try:
                invoke('stop')
            except Exception:
                result['passed'] = False
                result['cleanup_required'] = True
        result['seconds'] = round(time.monotonic() - began, 2)
        result['sources'] = {p.relative_to(local.CORE).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
                             for p in sorted(local.HERE.glob('*')) if p.suffix in ('.py', '.go')}
        local.write_new(root / 'result.json', local.json_bytes(result))
        print(json.dumps({'passed': result['passed'], 'result': str(root / 'result.json'), 'cases': passed}))
    return 0 if result['passed'] else 1


if __name__ == '__main__':
    sys.exit(main())
