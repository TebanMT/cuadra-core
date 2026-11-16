#!/usr/bin/env python3
"""Local-only setup and supervisor. Only a credential-free readiness GET; no OTP."""
import argparse
import contextlib
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import secrets
import signal
import socket
import stat
import subprocess
import sys
import time
from urllib.parse import quote, urlencode

HERE = Path(__file__).resolve().parent
CORE = HERE.parents[1]
PORT = 55449
ADDRESS = ('127.0.0.1', 18883)
RUNTIME_FILES = ('config.json', 'keys.json', 'identity.json', 'owner.password', 'owner.pgpass', 'server', 'helper',
                 'pg/PG_VERSION', 'pg/pg_hba.conf', 'pg/postgresql.conf', 'pg/postgresql.auto.conf')
CREDENTIAL_KEYS = {'TWILIO_ACCOUNT_SID', 'TWILIO_AUTH_TOKEN'}
VERIFY_KEYS = {'MANCUERNA_VERIFY_SERVICE_SID', 'MANCUERNA_TEST_ALLOWED_DESTINATIONS', 'MANCUERNA_TEST_BUDGET_USD'}
MIGRATIONS = tuple(sorted((CORE / 'db_migrations/postgres').glob('0[56][0-9]_*.sql')))
MIGRATIONS = tuple(p for p in MIGRATIONS if 59 <= int(p.name[:3]) <= 65)


class LocalError(Exception):
    """Messages are fixed labels only, never exception interpolation."""


def require(value, label):
    if not value:
        raise LocalError(label)


def path_arg(raw):
    p = Path(raw)
    require(p.is_absolute() and '..' not in p.parts, 'An absolute path without parent traversal is required')
    require(not p.is_symlink(), 'Symlink paths are not accepted')
    return p.resolve()


def directory(path):
    s = path.lstat()
    require(stat.S_ISDIR(s.st_mode) and s.st_uid == os.getuid() and stat.S_IMODE(s.st_mode) == 0o700,
            'A private owned directory with mode 0700 is required')


def read_private(path, maximum=65536):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        s = os.fstat(fd)
        require(stat.S_ISREG(s.st_mode) and s.st_nlink == 1 and s.st_uid == os.getuid()
                and stat.S_IMODE(s.st_mode) == 0o600 and s.st_size <= maximum, 'Invalid private file')
        with os.fdopen(fd, 'rb', closefd=False) as f:
            value = f.read(maximum + 1)
        require(len(value) <= maximum, 'Private file exceeds size limit')
        return value
    finally:
        os.close(fd)


def write_new(path, raw, mode=0o600):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    try:
        with os.fdopen(fd, 'wb', closefd=False) as f:
            f.write(raw)
            f.flush()
            os.fsync(fd)
    finally:
        os.close(fd)
    sync_dir(path.parent)


def sync_dir(path):
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def json_bytes(value):
    return (json.dumps(value, sort_keys=True, separators=(',', ':')) + '\n').encode()


def json_private(path):
    def unique(pairs):
        result = {}
        for k, v in pairs:
            require(k not in result, 'Duplicate JSON property')
            result[k] = v
        return result
    return json.loads(read_private(path), object_pairs_hook=unique)


def env_values(path, allowed, dedicated=False):
    """Read selected assignments, never source a shell or expand a value."""
    selected = {}
    for raw in read_private(path).decode('utf-8').splitlines():
        line = raw.strip()
        if not line or line.startswith('#'):
            continue
        match = re.match(r'(?:export\s+)?([A-Z][A-Z0-9_]*)\s*=(.*)\Z', line)
        if not match:
            require(not dedicated, 'Malformed dedicated Verify configuration')
            continue
        name, value = match.groups()
        if name not in allowed:
            require(not dedicated, 'Unknown dedicated Verify configuration key')
            continue
        require(name not in selected, 'Duplicate selected configuration key')
        value = value.strip()
        if not dedicated and len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        require(value and not any(c in value for c in '$`\\\r\n\x00\"\'') and not any(c.isspace() for c in value),
                'Selected values must be literal nonempty strings')
        selected[name] = value
    require(set(selected) == allowed, 'Required explicit configuration keys are missing')
    return selected


def source_configuration(credentials, verify_file, validity, recovery_window, recovery_max):
    require(120 <= validity <= 86400, 'Explicit Verify validity must be 120..86400 seconds')
    require(1 <= recovery_window <= 86400 * 366 and 1 <= recovery_max <= 2147483647,
            'Explicit recovery policy is required')
    c = env_values(credentials, CREDENTIAL_KEYS)
    v = env_values(verify_file, VERIFY_KEYS, True)
    require(re.fullmatch(r'AC[0-9a-fA-F]{32}', c['TWILIO_ACCOUNT_SID']), 'Invalid Account SID')
    require(re.fullmatch(r'[A-Za-z0-9_-]{16,256}', c['TWILIO_AUTH_TOKEN']), 'Invalid auth token format')
    require(re.fullmatch(r'VA[0-9a-fA-F]{32}', v['MANCUERNA_VERIFY_SERVICE_SID']), 'Invalid Verify service SID')
    destinations = v['MANCUERNA_TEST_ALLOWED_DESTINATIONS'].split(',')
    require(len(destinations) == 2 and len(set(destinations)) == 2 and
            all(re.fullmatch(r'\+[1-9][0-9]{7,14}', d) for d in destinations), 'Exactly two distinct literal E.164 destinations are required')
    require(v['MANCUERNA_TEST_BUDGET_USD'] == '5', 'The authorized local planning budget must be exactly 5')
    return {
        'verify': {'account_sid': c['TWILIO_ACCOUNT_SID'], 'username': c['TWILIO_ACCOUNT_SID'],
                   'password': c['TWILIO_AUTH_TOKEN'], 'service_sid': v['MANCUERNA_VERIFY_SERVICE_SID'],
                   'validity_seconds': validity, 'timeout_seconds': 20},
        'destinations': destinations,
        'recovery_limit': {'window_seconds': recovery_window, 'max_attempts': recovery_max},
    }


def clean_env():
    # Disable dependency downloads; installed Go modules must already exist.
    e = {k: os.environ[k] for k in ('PATH', 'HOME', 'TMPDIR', 'GOCACHE', 'GOMODCACHE') if k in os.environ}
    e.update(LANG='C', LC_ALL='C', GOPROXY='off', GOSUMDB='off', GOTOOLCHAIN='local')
    return e


def run(args, label, *, env=None, data=None):
    p = subprocess.run([str(a) for a in args], cwd=CORE, env=env or clean_env(), input=data,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=180)
    require(p.returncode == 0, label)
    return p.stdout


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


@contextlib.contextmanager
def lock(path):
    fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        s = os.fstat(fd)
        require(stat.S_ISREG(s.st_mode) and s.st_nlink == 1 and s.st_uid == os.getuid()
                and stat.S_IMODE(s.st_mode) == 0o600, 'Invalid runtime lock')
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise LocalError('Another local operation owns this runtime') from None
        yield
    finally:
        os.close(fd)


def owner_env(state, identity):
    return {**clean_env(), 'PGHOST': str(state / 'socket'), 'PGPORT': str(PORT),
            'PGDATABASE': identity['database'], 'PGUSER': identity['owner'], 'PGSSLMODE': 'disable',
            'PGPASSFILE': str(state / 'owner.pgpass'), 'PGSERVICEFILE': os.devnull,
            'PGCONNECT_TIMEOUT': '2'}


def sql(state, identity, pg_bin, text, database=None):
    env = owner_env(state, identity)
    if database:
        env['PGDATABASE'] = database
    return run([pg_bin / 'psql', '-XqAt', '-v', 'ON_ERROR_STOP=1'], 'Local database operation failed', env=env, data=text.encode()).decode().strip()


def stop_child(child, sig=signal.SIGTERM):
    # Popen retains a direct, unreaped child. Its PID cannot be recycled while
    # we signal it; no arbitrary PID, PID file or port-owner search is used.
    if child is not None and child.poll() is None:
        child.send_signal(sig)
        try:
            child.wait(timeout=8)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait(timeout=5)


def postgres(state, identity, pg_bin):
    require(not (state / 'pg/postmaster.pid').exists(), 'Existing PostgreSQL PID file requires review; no process was signaled')
    child = subprocess.Popen([str(pg_bin / 'postgres'), '-D', str(state / 'pg'), '-c', "listen_addresses=",
                              '-c', 'unix_socket_permissions=0700', '-k', str(state / 'socket'), '-p', str(PORT)],
                             env=clean_env(), stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        until = time.monotonic() + 15
        while time.monotonic() < until:
            require(child.poll() is None, 'Local PostgreSQL did not start')
            try:
                if sql(state, identity, pg_bin, 'SELECT 1', 'template1') == '1':
                    return child
            except LocalError:
                pass
            time.sleep(0.1)
        raise LocalError('Local PostgreSQL startup timed out')
    except BaseException:
        stop_child(child, signal.SIGINT)
        raise


def guard_status(state):
    return json.loads(run([state / 'helper', 'ledger-status', state / 'config.json'], 'Existing Verify ledger is unavailable'))


def validate_state(state):
    directory(state)
    manifest = json_private(state / 'complete.json')
    require(manifest['format'] == 1 and manifest['state_dir'] == str(state), 'Runtime manifest does not match this directory')
    require(set(manifest['files']) == set(RUNTIME_FILES), 'Incomplete runtime manifest')
    for name, expected in manifest['files'].items():
        p = state / name
        s = p.lstat()
        require(stat.S_ISREG(s.st_mode) and s.st_nlink == 1 and s.st_uid == os.getuid() and s.st_mode & 0o077 == 0,
                'Runtime file ownership or permissions changed')
        require(digest(p) == expected, 'Runtime files changed; explicit review is required')
    directory(state / 'socket')
    directory(state / 'pg')
    c = json_private(state / 'config.json')
    ledger = path_arg(c['local_verify_guard']['ledger_path'])
    require(not ledger.is_relative_to(state) and not state.is_relative_to(ledger.parent), 'Ledger must be outside the runtime directory')
    directory(ledger.parent)
    guard_status(state)
    return manifest, json_private(state / 'identity.json')


def setup(args):
    state, ledger_dir, pg_bin = path_arg(args.state_dir), path_arg(args.ledger_dir), path_arg(args.pg_bin)
    require(len(str(state / 'control.sock').encode()) < 100 and len(str(state / 'socket').encode()) < 85,
            'Choose a shorter private runtime path for Unix sockets')
    require(not ledger_dir.is_relative_to(state) and not state.is_relative_to(ledger_dir), 'Ledger and runtime directories must be separate')
    source = source_configuration(path_arg(args.credentials_file), path_arg(args.verify_file), args.validity_seconds,
                                  args.recovery_window_seconds, args.recovery_max_attempts)
    if state.exists():
        with lock(state / 'setup.lock'):
            manifest, _ = validate_state(state)
            current = json_private(state / 'config.json')
            require(current['verify'] == source['verify'] and current['local_verify_guard']['destinations'] == source['destinations']
                    and current['recovery_limit'] == source['recovery_limit'] and manifest['pg_bin'] == str(pg_bin)
                    and Path(current['local_verify_guard']['ledger_path']).parent == ledger_dir, 'Setup inputs differ from the existing runtime')
        return {'state': 'prepared', 'reused': True, 'ledger': guard_status(state)}
    require(len(MIGRATIONS) == 7, 'Required personal migrations are unavailable')
    # Existing ledger directory is allowed only if completely empty. Its
    # ledger or permanent lock can never trigger initialization again.
    if ledger_dir.exists():
        directory(ledger_dir)
        require(not any(ledger_dir.iterdir()), 'Existing ledger material cannot be initialized again')
    else:
        ledger_dir.mkdir(mode=0o700)
    state.mkdir(mode=0o700)
    with lock(state / 'setup.lock'):
        directory(state)
        (state / 'socket').mkdir(mode=0o700)
        run(['go', 'build', '-tags', 'server', '-o', state / 'helper', './scripts/mancuerna-local'], 'Local helper build failed')
        run(['go', 'build', '-tags', 'server', '-o', state / 'server', './cmd/server'], 'Local server build failed')
        os.chmod(state / 'helper', 0o700)
        os.chmod(state / 'server', 0o700)
        run([state / 'helper', 'keys', state / 'keys.json'], 'Key generation failed')
        suffix = secrets.token_hex(6)
        identity = {'owner': 'ml_owner_' + suffix, 'auth': 'ml_auth_' + suffix, 'data': 'ml_data_' + suffix,
                    'database': 'mancuerna_local_test_' + suffix, 'runtime_id': secrets.token_hex(32)}
        write_new(state / 'identity.json', json_bytes(identity))
        owner_password, auth_password, data_password = (secrets.token_hex(32) for _ in range(3))
        write_new(state / 'owner.password', (owner_password + '\n').encode())
        write_new(state / 'owner.pgpass', ('*:*:*:' + identity['owner'] + ':' + owner_password + '\n').encode())
        run([pg_bin / 'initdb', '-D', state / 'pg', '-U', identity['owner'], '--pwfile', state / 'owner.password',
             '--auth-local=scram-sha-256', '--auth-host=reject', '--no-locale', '--encoding=UTF8'], 'Isolated PostgreSQL initialization failed')
        def dsn(role, password):
            return f'postgresql://{role}:{quote(password, safe="")}@127.0.0.1:{PORT}/{identity["database"]}?' + urlencode({'host': str(state / 'socket'), 'sslmode': 'disable'})
        c = json_private(state / 'keys.json')
        c.update(minimum_app_version='0.1.0', policy_version=None, auth_database_url=dsn(identity['auth'], auth_password),
                 data_database_url=dsn(identity['data'], data_password), verify=source['verify'], recovery_limit=source['recovery_limit'],
                 local_verify_guard={'ledger_path': str(ledger_dir / 'verify-ledger.json'), 'destinations': source['destinations']},
                 otp_limits={k: {'window_seconds': 3600, 'max_send': 4, 'max_check': 20} for k in ('destination', 'origin', 'service')},
                 max_attempt_checks=5, resend_cooldown_seconds=30, reconcile_after_seconds=25)
        write_new(state / 'config.json', json_bytes(c))
        child = postgres(state, identity, pg_bin)
        try:
            sql(state, identity, pg_bin, 'CREATE DATABASE ' + identity['database'], 'template1')
            sql(state, identity, pg_bin, 'CREATE TABLE public._migrations(version integer PRIMARY KEY,name text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())')
            sql(state, identity, pg_bin, (CORE / 'scripts/db/mancuerna_roles.sql').read_text())
            for migration in MIGRATIONS:
                sql(state, identity, pg_bin, migration.read_text())
            for kind, password in (('auth', auth_password), ('data', data_password)):
                # Identifiers and literals below are generated lowercase hex,
                # never external SQL or credential source text.
                sql(state, identity, pg_bin, f"CREATE ROLE {identity[kind]} LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '{password}'; GRANT mancuerna_{kind} TO {identity[kind]}")
            require(sql(state, identity, pg_bin, 'SELECT max(version) FROM mancuerna.schema_versions') == '6', 'Personal schema revision mismatch')
            system_id = sql(state, identity, pg_bin, 'SELECT system_identifier FROM pg_control_system()')
        finally:
            stop_child(child, signal.SIGINT)
        # Explicit, one-time setup action after the schema succeeds. Neither
        # start nor resume nor stop contains a ledger initialization path.
        run([state / 'helper', 'ledger-init', state / 'config.json'], 'One-time Verify ledger initialization failed')
        guard_status(state)
        manifest = {'format': 1, 'state_dir': str(state), 'pg_bin': str(pg_bin), 'system_id': system_id,
                    'files': {name: digest(state / name) for name in RUNTIME_FILES},
                    'migration_sources': {p.name: digest(p) for p in MIGRATIONS},
                    'created_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}
        write_new(state / 'complete.json', json_bytes(manifest))
    return {'state': 'prepared', 'reused': False, 'ledger': guard_status(state)}


def control(state, command):
    _, identity = validate_state(state)
    endpoint = state / 'control.sock'
    if not endpoint.exists():
        require(not (state / 'pg/postmaster.pid').exists(), 'Unmanaged PostgreSQL state requires review; no process was signaled')
        return {'state': 'stopped', 'ledger': guard_status(state)}
    s = endpoint.lstat()
    require(stat.S_ISSOCK(s.st_mode) and s.st_uid == os.getuid() and s.st_mode & 0o077 == 0, 'Invalid supervisor socket')
    with socket.socket(socket.AF_UNIX) as sock:
        sock.settimeout(15)
        try:
            sock.connect(str(endpoint))
        except ConnectionRefusedError:
            raise LocalError('Stale supervisor socket; inspect runtime without killing a PID') from None
        sock.sendall(json_bytes({'command': command, 'runtime_id': identity['runtime_id']}))
        data = b''
        while not data.endswith(b'\n') and len(data) < 4096:
            chunk = sock.recv(4096)
            if not chunk:
                break
            data += chunk
        return json.loads(data)


def server_ready():
    # Fixed loopback, no redirects, proxy, credentials or provider operation.
    connection = http.client.HTTPConnection(*ADDRESS, timeout=1)
    try:
        connection.request('GET', '/api/mancuerna/v1/me')
        response = connection.getresponse()
        body = response.read(4097)
        return (response.status == 401 and response.getheader('Cache-Control') == 'no-store'
                and len(body) <= 4096 and json.loads(body).get('code') == 'AUTH_REQUIRED')
    except (OSError, ValueError, http.client.HTTPException, AttributeError):
        return False
    finally:
        connection.close()


def supervise(state):
    manifest, identity = validate_state(state)
    pg_bin = Path(manifest['pg_bin'])
    pg_child = server = None
    endpoint = state / 'control.sock'
    stopping = False
    def requested(*_):
        nonlocal stopping
        stopping = True
    signal.signal(signal.SIGTERM, requested)
    signal.signal(signal.SIGINT, requested)
    with lock(state / 'runtime.lock'):
        if endpoint.exists():
            s = endpoint.lstat()
            require(stat.S_ISSOCK(s.st_mode) and s.st_uid == os.getuid(), 'Unexpected supervisor endpoint')
            endpoint.unlink()
        try:
            pg_child = postgres(state, identity, pg_bin)
            require(sql(state, identity, pg_bin, 'SELECT system_identifier FROM pg_control_system()') == manifest['system_id'], 'PostgreSQL cluster identity changed')
            # A pre-existing listener is never reused or stopped.
            with socket.socket() as probe:
                try:
                    probe.bind(ADDRESS)
                except OSError:
                    raise LocalError('Local API address is already occupied; no process was signaled') from None
            server = subprocess.Popen([str(state / 'server')], cwd=CORE,
                                      env={**clean_env(), 'MANCUERNA_LOCAL_ONLY': '1', 'MANCUERNA_CONFIG_FILE': str(state / 'config.json'), 'TINTA_MODE': 'test'},
                                      stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            ready_until = time.monotonic() + 15
            while not server_ready():
                require(not stopping and server.poll() is None and pg_child.poll() is None,
                        'Local server exited before readiness')
                require(time.monotonic() < ready_until, 'Local server readiness timed out')
                time.sleep(0.1)
            with socket.socket(socket.AF_UNIX) as listener:
                listener.bind(str(endpoint))
                os.chmod(endpoint, 0o600)
                listener.listen(4)
                listener.settimeout(0.2)
                while not stopping:
                    require(pg_child.poll() is None and server.poll() is None, 'A managed child exited; runtime stopped')
                    try:
                        conn, _ = listener.accept()
                    except socket.timeout:
                        continue
                    with conn:
                        conn.settimeout(1)
                        request = conn.recv(4096)
                        try:
                            message = json.loads(request)
                            require(message.get('runtime_id') == identity['runtime_id'] and message.get('command') in ('status', 'stop'), 'Invalid supervisor request')
                            if message['command'] == 'stop':
                                stopping = True
                                stop_child(server)
                                stop_child(pg_child, signal.SIGINT)
                            result = {'state': 'stopped' if stopping else 'running', 'address': '127.0.0.1:18883', 'ledger': guard_status(state)}
                        except (ValueError, LocalError, OSError):
                            result = {'state': 'request_rejected'}
                        conn.sendall(json_bytes(result))
        finally:
            stop_child(server)
            stop_child(pg_child, signal.SIGINT)
            if endpoint.exists() and stat.S_ISSOCK(endpoint.lstat().st_mode):
                endpoint.unlink()


def start(state):
    validate_state(state)
    if (state / 'control.sock').exists():
        return control(state, 'status')
    child = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), '_supervise', '--state-dir', str(state)],
                             env=clean_env(), stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                             start_new_session=True)
    until = time.monotonic() + 20
    while time.monotonic() < until:
        require(child.poll() is None, 'Local supervisor did not start; no external process was stopped')
        if (state / 'control.sock').exists():
            return control(state, 'status')
        time.sleep(0.1)
    # Only our own direct supervisor child may receive this signal.
    stop_child(child)
    raise LocalError('Local supervisor startup timed out')


def main(argv=None):
    os.umask(0o077)
    parser = argparse.ArgumentParser(description='Prepare and control an isolated Mancuerna local test runtime; never sends OTP.')
    commands = parser.add_subparsers(dest='command', required=True)
    p = commands.add_parser('setup')
    for name in ('state-dir', 'ledger-dir', 'pg-bin', 'credentials-file', 'verify-file'):
        p.add_argument('--' + name, required=True)
    p.add_argument('--validity-seconds', required=True, type=int)
    p.add_argument('--recovery-window-seconds', required=True, type=int)
    p.add_argument('--recovery-max-attempts', required=True, type=int)
    for name in ('start', 'status', 'stop', '_supervise'):
        p = commands.add_parser(name)
        p.add_argument('--state-dir', required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == 'setup':
            result = setup(args)
        else:
            state = path_arg(args.state_dir)
            if args.command == '_supervise':
                supervise(state)
                return 0
            result = start(state) if args.command == 'start' else control(state, args.command)
        print(json.dumps(result, sort_keys=True))
        return 0
    except LocalError as error:
        print(str(error), file=sys.stderr)
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        print('Local operation failed; private material was not printed. Incomplete setup requires review.', file=sys.stderr)
    return 1


if __name__ == '__main__':
    sys.exit(main())
