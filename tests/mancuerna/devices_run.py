#!/usr/bin/env python3
"""US007 domain and real restricted PostgreSQL checks, with synthetic actors.

Creates a fresh socket-only cluster. No DATABASE_URL, .env, existing database,
listener, or provider calls. HTTP/mobile integration remains central work.
"""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlencode

CORE = Path(__file__).resolve().parents[2]
PG = Path(os.environ.get('MANCUERNA_DEVICES_PG_BIN', '/Applications/Postgres.app/Contents/Versions/latest/bin'))
ART = Path(tempfile.mkdtemp(prefix='mancuerna-devices-', dir='/tmp'))
os.chmod(ART, 0o700)
ENV = {key: value for key, value in os.environ.items() if key in ('PATH', 'HOME', 'TMPDIR', 'GOCACHE', 'GOMODCACHE')}
ENV.update(PGHOST=str(ART), PGPORT='55456', PGUSER='mc_devices_owner', PGDATABASE='mancuerna_devices_test', PGSSLMODE='disable', PGPASSFILE='/dev/null', TINTA_MODE='test', GOTOOLCHAIN='local')
ENV.setdefault('GOCACHE', str(ART / 'go-cache'))
started = False
start = time.monotonic()
result = {'passed': False, 'artifacts': str(ART), 'provider_calls': 0, 'scope': 'Device list/revoke app and repositories with real restricted PostgreSQL; no HTTP or mobile journey.'}


def run(arguments, name, env=None, input=None):
    with (ART / (name + '.log')).open('w') as output:
        process = subprocess.run([str(x) for x in arguments], cwd=CORE, env=env or ENV, input=input, text=True, stdout=output, stderr=subprocess.STDOUT)
    content = (ART / (name + '.log')).read_text()
    if process.returncode:
        raise RuntimeError(name + ': ' + content[-6500:])
    return content


def sql(statement, name):
    return run([PG / 'psql', '-XqAt', '-v', 'ON_ERROR_STOP=1'], name, input=statement)


def dsn(role):
    return 'postgresql://' + role + '@localhost:55456/mancuerna_devices_test?' + urlencode({'host': str(ART), 'sslmode': 'disable', 'passfile': '/dev/null'})


try:
    run([PG / 'initdb', '-D', ART / 'pg', '-U', 'mc_devices_owner', '--auth-local=trust', '--auth-host=reject', '--no-locale', '--encoding=UTF8'], 'initdb')
    run([PG / 'pg_ctl', '-D', ART / 'pg', '-l', ART / 'postgres.log', '-o', "-c listen_addresses='' -k " + str(ART) + ' -p 55456', '-w', 'start'], 'start')
    started = True
    run([PG / 'createdb', '--maintenance-db=template1', 'mancuerna_devices_test'], 'createdb')
    result['postgres'] = sql('SHOW server_version', 'version').strip()
    sql('CREATE TABLE public._migrations(version integer PRIMARY KEY,name text NOT NULL)', 'ledger')
    sql((CORE / 'scripts/db/mancuerna_roles.sql').read_text(), 'roles')
    for migration in ['059_mancuerna_personal.sql', '060_mancuerna_attempt_operations.sql', '061_mancuerna_refresh_logout_tickets.sql', '062_mancuerna_device_proof_binding.sql']:
        sql((CORE / 'db_migrations/postgres' / migration).read_text(), 'migration-' + migration[:3])
    sql('CREATE ROLE mc_devices_data LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION; GRANT mancuerna_data TO mc_devices_data; CREATE ROLE mc_devices_auth LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION; GRANT mancuerna_auth TO mc_devices_auth', 'runtime-logins')
    test_env = {**ENV, 'MANCUERNA_TEST_OWNER_DSN': dsn('mc_devices_owner'), 'MANCUERNA_TEST_AUTH_DSN': dsn('mc_devices_auth'), 'MANCUERNA_TEST_DATA_DSN': dsn('mc_devices_data')}
    packages = ['./src/modules/personalaccounts/domain/device', './src/modules/personalaccounts/infraestructure/db/repositories']
    output = run(['go', 'test', '-race', '-count=1', '-json', '-tags', 'server,integration', '-run', '^TestDevices', *packages], 'devices-tests', test_env)
    events = [json.loads(line) for line in output.splitlines() if line.startswith('{')]
    skipped = [e['Test'] for e in events if e.get('Action') == 'skip' and 'Test' in e]
    passed = [e['Test'] for e in events if e.get('Action') == 'pass' and '/' not in e.get('Test', '/')]
    required = ['TestDevicesRevocationTargetIsExclusiveAndClosed', 'TestDevicesListedProjectionRejectsInvalidChronologyAndIdentity',
                'TestDevicesPostgresKeysetPrecisionOwnerEpochAndRevokedActor', 'TestDevicesPostgresProofBindingConcurrentReplayAndTargetIsolation',
                'TestDevicesPostgresOthersCancelsOnlyDependentUnconsumedApprovals', 'TestDevicesPostgresSelfAllRollbackAndLateOutcomeCannotRevive',
                'TestDevicesPostgresSingleSelfAndGlobalReceiptCollisionFailClosed']
    if skipped or any(name not in passed for name in required):
        raise RuntimeError('Required real PostgreSQL/device test missing or skipped')
    run(['go', 'vet', '-tags', 'server', './src/modules/personalaccounts/app', *packages], 'vet', test_env)
    result['cases'] = required
    result['skipped'] = skipped
    result['passed'] = True
except Exception as error:
    result['error'] = str(error)
    print(str(error), file=sys.stderr)
finally:
    if started:
        try:
            run([PG / 'pg_ctl', '-D', ART / 'pg', '-m', 'immediate', '-w', 'stop'], 'stop')
        except Exception as error:
            result['passed'] = False
            result['stop_error'] = str(error)
    sources = [Path(__file__).resolve(), CORE / 'go.mod', CORE / 'go.sum', CORE / 'scripts/db/mancuerna_roles.sql']
    for pattern in ['db_migrations/postgres/0[56][0-9]_mancuerna*.sql',
                    'src/modules/personalaccounts/app/*devices*.go', 'src/modules/personalaccounts/domain/device/*.go',
                    'src/modules/personalaccounts/domain/repository/devices.go', 'src/modules/personalaccounts/infraestructure/db/models/listed_device.go',
                    'src/modules/personalaccounts/infraestructure/db/repositories/devices*.go',
                    'src/modules/personalaccounts/infraestructure/db/repositories/auth_session.go',
                    'src/modules/personalaccounts/infraestructure/db/repositories/auth_attempt_postgres.go',
                    'src/modules/personalaccounts/infraestructure/db/repositories/device_proof.go',
                    'src/shared/domain/personal*.go', 'src/shared/sync/personal/domain/cursor*.go',
                    'src/shared/sync/personal/infraestructure/security/*.go']:
        sources.extend(CORE.glob(pattern))
    result['sources'] = {str(p.relative_to(CORE)): hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(sources)}
    result['seconds'] = round(time.monotonic() - start, 2)
    (ART / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print('Evidence: ' + str(ART / 'result.json'))
sys.exit(0 if result['passed'] else 1)
