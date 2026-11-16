#!/usr/bin/env python3
"""Phone verification application checks in a disposable, socket-only cluster.

No DATABASE_URL/.env, existing server, Twilio service or plaintext secrets.
This harness verifies durable reservations and RLS/CAS, not a completed login.
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
PG = Path(os.environ.get('MANCUERNA_PHONE_PG_BIN', '/Applications/Postgres.app/Contents/Versions/latest/bin'))
ART = Path(tempfile.mkdtemp(prefix='mancuerna-phone-', dir='/tmp'))
os.chmod(ART, 0o700)
ENV = {key: value for key, value in os.environ.items() if key in ('PATH', 'HOME', 'TMPDIR', 'GOCACHE', 'GOMODCACHE')}
ENV.update(PGHOST=str(ART), PGPORT='55452', PGUSER='mc_phone_owner', PGDATABASE='mancuerna_phone_test', PGSSLMODE='disable', TINTA_MODE='test')
ENV.setdefault('GOCACHE', str(ART / 'go-cache'))
started = False
start = time.monotonic()
result = {'passed': False, 'artifacts': str(ART), 'scope': 'Check/resend/get with real PostgreSQL and an explicit provider double; no real Verify or account journey completed.'}


def run(arguments, name, env=None, input=None):
    with (ART / (name + '.log')).open('w') as output:
        process = subprocess.run([str(x) for x in arguments], cwd=CORE, env=env or ENV, input=input, text=True, stdout=output, stderr=subprocess.STDOUT)
    content = (ART / (name + '.log')).read_text()
    if process.returncode:
        raise RuntimeError(name + ': ' + content[-6000:])
    return content


def sql(statement, name):
    return run([PG / 'psql', '-XqAt', '-v', 'ON_ERROR_STOP=1'], name, input=statement)


def dsn(role):
    return 'postgresql://' + role + '@localhost:55452/mancuerna_phone_test?' + urlencode({'host': str(ART), 'sslmode': 'disable'})


try:
    run([PG / 'initdb', '-D', ART / 'pg', '-U', 'mc_phone_owner', '--auth-local=trust', '--auth-host=reject', '--no-locale', '--encoding=UTF8'], 'initdb')
    run([PG / 'pg_ctl', '-D', ART / 'pg', '-l', ART / 'postgres.log', '-o', "-c listen_addresses='' -k " + str(ART) + ' -p 55452', '-w', 'start'], 'start')
    started = True
    run([PG / 'createdb', '--maintenance-db=template1', 'mancuerna_phone_test'], 'createdb')
    result['postgres'] = sql('SHOW server_version', 'version').strip()
    sql('CREATE TABLE public._migrations(version integer PRIMARY KEY,name text NOT NULL)', 'ledger')
    sql((CORE / 'scripts/db/mancuerna_roles.sql').read_text(), 'roles')
    for migration in ['059_mancuerna_personal.sql', '060_mancuerna_attempt_operations.sql']:
        sql((CORE / 'db_migrations/postgres' / migration).read_text(), 'migration-' + migration[:3])
    sql('CREATE ROLE mc_phone_auth LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION; GRANT mancuerna_auth TO mc_phone_auth', 'runtime-login')
    test_env = {**ENV, 'MANCUERNA_TEST_AUTH_DSN': dsn('mc_phone_auth')}
    packages = ['./src/modules/personalaccounts/app', './src/modules/personalaccounts/infraestructure/db/repositories']
    output = run(['go', 'test', '-race', '-count=1', '-v', '-tags', 'server,integration', '-run', 'TestPhone', *packages], 'phone-tests', test_env)
    required = ['TestPhoneCheckPostgresDirectProofIdempotencyAndDetachedCommit', 'TestPhoneGetPostgresCannotCreditFetchApprovalOrMissing', 'TestPhoneResendPostgresExplicitCooldownFallbackAndBudgetRollback', 'TestPhoneCheckPostgresRacedReceiptRollsBackExtraBudget', 'TestPhoneCheckPostgresBudgetExhaustionRollsBackAttemptAndReceipt', 'TestPhoneResendPostgresFetchBeforeExplicitReleaseAndLateCheckIgnored', 'TestPhoneGetPostgresStaleFetchCannotOverwriteResend', 'TestPhoneCheckPostgresReceiptFailureAndMandatoryConfiguration']
    for name in required:
        if '--- PASS: ' + name not in output:
            raise AssertionError('Required real PostgreSQL test did not pass: ' + name)
    run(['go', 'vet', '-tags', 'server', *packages], 'vet', test_env)
    result['cases'] = required
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
    sources = [CORE / 'src/modules/personalaccounts/domain/errors/attempt.go', CORE / 'src/modules/personalaccounts/domain/repository/attempt.go', CORE / 'db_migrations/postgres/059_mancuerna_personal.sql', CORE / 'db_migrations/postgres/060_mancuerna_attempt_operations.sql', CORE / 'scripts/db/mancuerna_roles.sql', CORE / 'src/modules/personalaccounts/infraestructure/db/repositories/account_postgres_integration_test.go', CORE / 'src/modules/personalaccounts/infraestructure/db/repositories/account_postgres.go', Path(__file__).resolve()]
    for relative, pattern in [
        ('src/modules/personalaccounts/app', '*.go'),
        ('src/modules/personalaccounts/domain/attempt', '*.go'),
        ('src/modules/personalaccounts/domain/budget', '*.go'),
        ('src/modules/personalaccounts/domain/receipt', '*.go'),
        ('src/modules/personalaccounts/domain/repository', '*.go'),
        ('src/modules/personalaccounts/infraestructure/budget', '*.go'),
        ('src/modules/personalaccounts/infraestructure/db/models', '*.go'),
        ('src/modules/personalaccounts/infraestructure/db/repositories', '*receipt*.go'),
        ('src/modules/personalaccounts/infraestructure/db/repositories', '*budget*.go'),
        ('src/modules/personalaccounts/infraestructure/db/repositories', 'phone_verification*.go'),
        ('src/modules/personalaccounts/domain/verification', '*.go'),
        ('src/modules/personalaccounts/infraestructure/db/models', 'auth_attempt*.go'),
        ('src/modules/personalaccounts/infraestructure/db/repositories', 'auth_attempt*.go'),
        ('src/shared/domain', 'personal*.go'),
    ]:
        sources.extend((CORE / relative).glob(pattern))
    result['sources'] = {str(path.relative_to(CORE)): hashlib.sha256(path.read_bytes()).hexdigest() for path in sorted(sources)}
    result['seconds'] = round(time.monotonic() - start, 2)
    (ART / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print('Evidence: ' + str(ART / 'result.json'), flush=True)
sys.exit(0 if result['passed'] else 1)
