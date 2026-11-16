#!/usr/bin/env python3
"""Manual SMS client for an existing isolated runtime; never starts API or PG.

Only `start --allow-one-real-sms` can request an SMS. No resend, automatic
retry, ledger creation/reset, dotenv sourcing or remote base URL is supported.
`check` reads a human OTP from a hidden TTY prompt or --otp-stdin; the OTP is
never stored. After an uncertain response use `poll`, not another start/check.
All stdout is an allowlisted summary. Private continuation/identity and the
limited logout ticket live in persistent 0600 custody, separate from evidence.
The USD1 reservation is a conservative bound confirmed by the operator, not
Twilio billing evidence. Setup and confirmation of the real TTL happen first.
"""
import argparse
import base64
import datetime as dt
import getpass
import hashlib
import http.client
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import time
import uuid

CORE = Path(__file__).resolve().parents[2]
_spec = importlib.util.spec_from_file_location('mancuerna_local', CORE / 'scripts/mancuerna-local/local.py')
local = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(local)
PRIVATE_ROOT = CORE / 'tmp/mancuerna-real-verify'
LEDGER = PRIVATE_ROOT / 'ledger/verify-ledger.json'
VERIFY_FILE = CORE / '.env.mancuerna.verify-test.local'
BASE = '/api/mancuerna/v1'
STATE_KEYS = {'version', 'runtime_hash', 'ledger_hash', 'destination', 'started_at', 'start_id', 'attempt_id', 'new_account_id',
              'candidate_id', 'public_key', 'seed', 'continuation', 'start_hash', 'phase', 'attempt', 'check', 'checks', 'logout'}
PHASES = {'starting', 'start-uncertain', 'awaiting-code', 'checking', 'check-uncertain', 'verified',
          'authorization_required', 'authorization_pending', 'consumed', 'authorized', 'denied', 'expired', 'cancelled',
          'logging-out', 'logout-uncertain', 'closed'}


class ClientError(Exception):
    """Only fixed safe labels are printed; never interpolate a caught error."""


class SafeParser(argparse.ArgumentParser):
    def error(self, message):
        # argparse normally echoes unrecognized arguments, which could include
        # an accidentally supplied phone/OTP. Keep diagnostics value-free.
        raise ClientError('INVALID_CLI_ARGUMENTS')


def require(value, label):
    if not value:
        raise ClientError(label)


def canonical(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()


def digest(value):
    return hashlib.sha256(value).hexdigest()


def b64(value):
    return base64.urlsafe_b64encode(value).decode().rstrip('=')


def identifier(value):
    require(isinstance(value, str) and re.fullmatch(r'[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}', value), 'INVALID_IDENTIFIER')
    return value


def secret(value):
    require(isinstance(value, str) and re.fullmatch(r'[A-Za-z0-9_.-]{32,512}', value), 'INVALID_SECRET')
    return value


def binary(value, size):
    require(isinstance(value, str) and re.fullmatch(r'[A-Za-z0-9_-]+', value), 'INVALID_BINARY')
    decoded = base64.urlsafe_b64decode(value + '=' * ((4 - len(value) % 4) % 4))
    require(len(decoded) == size and b64(decoded) == value, 'INVALID_BINARY')
    return value


def timestamp(value):
    require(isinstance(value, str) and re.fullmatch(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{3})?Z', value), 'INVALID_EXPIRY')
    return dt.datetime.fromisoformat(value.replace('Z', '+00:00')).timestamp()


def replace_private(path, value):
    """Atomic private write, sync and exact readback; never erase on error."""
    if path.exists():
        local.read_private(path)
    tmp = path.parent / ('.pending-' + secrets.token_hex(8))
    raw = local.json_bytes(value)
    local.write_new(tmp, raw)
    os.replace(tmp, path)
    local.sync_dir(path.parent)
    require(local.read_private(path) == raw, 'CUSTODY_READBACK_FAILED')


def node_crypto(seed, payload=None):
    # Stock Node crypto only; stdin/stdout are private in-memory pipes. No
    # arguments, environment defaults, temporary keys, npm or package install.
    script = """const fs=require('node:fs'),c=require('node:crypto');
const x=JSON.parse(fs.readFileSync(0,'utf8'));
const key=c.createPrivateKey({key:Buffer.concat([Buffer.from('302e020100300506032b657004220420','hex'),Buffer.from(x.seed,'base64url')]),format:'der',type:'pkcs8'});
const out=x.payload===null?c.createPublicKey(key).export({format:'der',type:'spki'}).subarray(-32):c.sign(null,Buffer.from(x.payload,'base64url'),key);
process.stdout.write(out.toString('base64url'));"""
    env = {key: os.environ[key] for key in ('PATH', 'HOME', 'TMPDIR') if key in os.environ}
    result = subprocess.run(['node', '-e', script], input=canonical({'seed': seed, 'payload': b64(payload) if payload is not None else None}),
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=env, timeout=10, check=False)
    require(result.returncode == 0, 'IDENTITY_OPERATION_FAILED')
    return binary(result.stdout.decode('ascii'), 64 if payload is not None else 32)


def preflight(runtime):
    """No network: validates existing files and the guard's read-only status."""
    runtime = local.path_arg(str(runtime))
    local.validate_state(runtime)
    config = local.json_private(runtime / 'config.json')
    approved = local.env_values(VERIFY_FILE, local.VERIFY_KEYS, True)
    destinations = approved['MANCUERNA_TEST_ALLOWED_DESTINATIONS'].split(',')
    require(approved['MANCUERNA_TEST_BUDGET_USD'] == '5' and len(set(destinations)) == 2 and
            all(re.fullmatch(r'\+[1-9][0-9]{7,14}', item) for item in destinations), 'AUTHORIZED_CONFIG_INVALID')
    require(config['verify']['service_sid'] == approved['MANCUERNA_VERIFY_SERVICE_SID'] and
            re.fullmatch(r'VA[0-9a-fA-F]{32}', approved['MANCUERNA_VERIFY_SERVICE_SID']) and
            approved['MANCUERNA_VERIFY_SERVICE_SID'] != 'VA' + '0' * 32, 'SERVICE_NOT_AUTHORIZED')
    require(config['local_verify_guard']['destinations'] == destinations and
            local.path_arg(config['local_verify_guard']['ledger_path']) == LEDGER.resolve(), 'LEDGER_OR_DESTINATIONS_CHANGED')
    # Validate both DSNs without connecting. Never read DATABASE_URL or .env.
    for name in ('auth_database_url', 'data_database_url'):
        from urllib.parse import urlsplit, parse_qs
        uri = urlsplit(config[name]); query = parse_qs(uri.query)
        require(uri.scheme in ('postgres', 'postgresql') and uri.hostname == '127.0.0.1' and uri.port == local.PORT and
                re.fullmatch(r'/mancuerna_local_test_[a-z0-9_]+', uri.path) and query.get('host') == [str(runtime / 'socket')] and
                query.get('sslmode') == ['disable'] and set(query) == {'host', 'sslmode'}, 'NONLOCAL_DATABASE_REJECTED')
    ledger = local.json_private(LEDGER)
    require(ledger['account_sid'] == config['verify']['account_sid'] and ledger['service_sid'] == config['verify']['service_sid'], 'LEDGER_OWNER_CHANGED')
    stats = local.guard_status(runtime)
    require(stats['BudgetCents'] == 500 and stats['MarginCents'] == 100 and
            stats['ReservedCents'] == stats['StartsReserved'] * 100 and stats['RemainingStarts'] == 4 - stats['StartsReserved'] and
            0 <= stats['StartsReserved'] <= 4, 'BUDGET_GUARD_INVALID')
    return {'runtime': runtime, 'config': config, 'destinations': destinations, 'stats': stats,
            'runtime_hash': digest(local.read_private(runtime / 'complete.json')),
            'ledger_hash': digest(str(ledger['id']).encode()), 'validity': config['verify']['validity_seconds']}


def request(context, method, route, body=None, headers=None):
    """One HTTP request, fixed loopback, no proxy/redirect/retry/logging."""
    status = local.control(context['runtime'], 'status')
    require(status.get('state') == 'running' and status.get('address') == '127.0.0.1:18883', 'LOCAL_RUNTIME_NOT_RUNNING')
    require((method, route) in {('POST', '/auth/start'), ('POST', '/auth/check'), ('POST', '/auth/logout')} or
            method == 'GET' and re.fullmatch(r'/auth/attempts/[0-9a-f-]{36}', route), 'ROUTE_NOT_ALLOWED')
    connection = http.client.HTTPConnection('127.0.0.1', 18883, timeout=45)
    try:
        values = {'Accept': 'application/json', 'X-App-Version': '0.1.0', **(headers or {})}
        if body is not None:
            values['Content-Type'] = 'application/json'
        connection.request(method, BASE + route, body=canonical(body) if body is not None else None, headers=values)
        response = connection.getresponse()
        raw = response.read(32769)
        require(len(raw) <= 32768 and response.getheader('Cache-Control') == 'no-store', 'UNSAFE_RESPONSE')
        value = json.loads(raw)
        require(isinstance(value, dict), 'INVALID_RESPONSE')
        # Error messages, request IDs and any response body never leave here.
        return response.status, value
    finally:
        connection.close()


def load_state(path, context, destination):
    value = local.json_private(path)
    require(set(value) == STATE_KEYS and value['version'] == 1 and value['runtime_hash'] == context['runtime_hash'] and
            value['ledger_hash'] == context['ledger_hash'] and value['destination'] == destination and value['phase'] in PHASES,
            'CUSTODY_BINDING_CHANGED')
    for key in ('start_id', 'attempt_id', 'new_account_id', 'candidate_id'):
        identifier(value[key])
    require(type(value['checks']) is int and 0 <= value['checks'] <= 5, 'CHECK_BUDGET_INVALID')
    require(type(value['started_at']) is int and value['started_at'] > 0, 'INVALID_START_TIME')
    require(re.fullmatch(r'[0-9a-f]{64}', value['start_hash']), 'CUSTODY_HASH_INVALID')
    if value['phase'] != 'closed':
        secret(value['continuation']); binary(value['seed'], 32); binary(value['public_key'], 32)
        require(digest(canonical(start_body(value, context))) == value['start_hash'], 'START_BODY_CHANGED')
    if value['check'] is not None:
        require(set(value['check']) == {'id'}, 'INVALID_CHECK_REFERENCE')
        identifier(value['check']['id'])
    if value['logout'] is not None:
        require(set(value['logout']) == {'id', 'ticket'}, 'INVALID_LOGOUT_REFERENCE')
        identifier(value['logout']['id']); secret(value['logout']['ticket'])
    return value


def start_body(state, context):
    return {'attempt_id': state['attempt_id'], 'phone': context['destinations'][state['destination'] - 1],
            'country': 'MX', 'channel': 'sms', 'purpose': 'access', 'new_account_id': state['new_account_id'],
            'continuation_secret': state['continuation'], 'candidate': {'id': state['candidate_id'], 'platform': 'android',
            'label': 'Tinta ensayo SMS local', 'public_key': state['public_key']}}


def save_attempt(state, value, validity):
    require(value.get('id') == state['attempt_id'] and value.get('state') in
            {'pending', 'uncertain', 'verified', 'authorization_required', 'authorization_pending', 'consumed', 'denied', 'expired', 'cancelled'}, 'ATTEMPT_BINDING_INVALID')
    nonce = binary(value.get('nonce'), 32); expiry = timestamp(value.get('expires_at'))
    # The API exposes its configured expiry, not independent Twilio evidence.
    # A mismatching first response blocks check; no hard-coded ten-minute UI.
    require(abs(expiry - state['started_at'] / 1000 - validity) <= 60, 'API_EXPIRY_CONFIG_MISMATCH')
    state['attempt'] = {'nonce': nonce, 'expires_at': value['expires_at'], 'state': value['state']}


def apply_attempt_phase(state, confirmed_check=False):
    observed = state['attempt']['state']
    if observed == 'pending':
        if confirmed_check or state['phase'] in {'starting', 'start-uncertain'}:
            # A direct, successfully decoded check response confirms that
            # this code was not approved. Another human check is now allowed.
            state['phase'] = 'awaiting-code'
        elif state['phase'] == 'checking':
            state['phase'] = 'check-uncertain'
        # A pending GET after a lost check response does not establish that
        # operation's outcome. Neither it nor a stale pending GET may reopen
        # an attempt already observed in a terminal/verified state.
    elif observed == 'uncertain':
        state['phase'] = 'check-uncertain' if state['check'] is not None else 'start-uncertain'
    else:
        # The HTTP contract carries these states inside Attempt, including
        # expired/denied/cancelled under top-level status=pending.
        state['phase'] = observed


def accept_result(state, response, context, confirmed_check=False):
    status = response.get('status')
    if status == 'authorized':
        session = response.get('session', {})
        require(session.get('device_id') == state['candidate_id'], 'SESSION_DEVICE_MISMATCH')
        identifier(session.get('account_id')); identifier(session.get('session_id'))
        ticket = secret(session.get('revocation_ticket'))
        state['logout'] = state['logout'] or {'id': str(uuid.uuid4()), 'ticket': ticket}
        require(state['logout']['ticket'] == ticket, 'LOGOUT_TICKET_CHANGED')
        # Never copy bearer/refresh tokens or a recovery kit into this runner.
        state['phase'] = 'authorized'
    else:
        require(status in {'pending', 'authorization_required'}, 'RESULT_STATUS_INVALID')
        save_attempt(state, response.get('attempt', {}), context['validity'])
        apply_attempt_phase(state, confirmed_check)
        if status == 'authorization_required':
            require(state['attempt']['state'] == 'authorization_required', 'RESULT_STATE_MISMATCH')


def summary(action, state, context, http_status=None):
    result = {'action': action, 'phase': state['phase'] if state else 'preflight', 'real_sms_requested_by_this_action': action == 'start',
              'budget_cents': 500, 'reserved_cents': context['stats']['ReservedCents'],
              'remaining_start_reservations': context['stats']['RemainingStarts'], 'billing_measured': False}
    if http_status is not None:
        result['http_status'] = http_status
    if state and state['attempt']:
        result['remaining_seconds'] = max(0, int(timestamp(state['attempt']['expires_at']) - time.time()))
    return result


def execute(args, otp_reader=None):
    context = preflight(local.path_arg(args.state_dir))
    if args.command == 'preflight':
        return summary('preflight', None, context)
    require(args.destination in (1, 2), 'EXPLICIT_DESTINATION_REQUIRED')
    client = PRIVATE_ROOT / ('client-' + str(args.destination))
    if args.command == 'start':
        require(args.allow_one_real_sms and args.confirm_service_validity_seconds == context['validity'], 'EXPLICIT_SEND_AND_VALIDITY_REQUIRED')
        require(type(args.unit_cost_upper_bound_cents) is int and 1 <= args.unit_cost_upper_bound_cents <= 100, 'CONFIRMED_COST_BOUND_REQUIRED')
        require(context['stats']['RemainingStarts'] > 0, 'NO_START_BUDGET')
        if not client.exists():
            client.mkdir(mode=0o700)
    local.directory(client)
    path = client / 'continuation.json'
    with local.lock(client / 'client.lock'):
        if args.command == 'start':
            require(not path.exists(), 'EXISTING_ATTEMPT_REQUIRES_POLL_NOT_SEND')
            seed = b64(secrets.token_bytes(32))
            state = {'version': 1, 'runtime_hash': context['runtime_hash'], 'ledger_hash': context['ledger_hash'], 'destination': args.destination,
                     'started_at': int(time.time() * 1000),
                     'start_id': str(uuid.uuid4()), 'attempt_id': str(uuid.uuid4()), 'new_account_id': str(uuid.uuid4()), 'candidate_id': str(uuid.uuid4()),
                     'seed': seed, 'public_key': node_crypto(seed), 'continuation': b64(secrets.token_bytes(32)),
                     'phase': 'starting', 'attempt': None, 'check': None, 'checks': 0, 'logout': None, 'start_hash': ''}
            body = start_body(state, context); state['start_hash'] = digest(canonical(body))
            replace_private(path, state)  # Intent precedes any provider effect.
            try:
                status, response = request(context, 'POST', '/auth/start', body, {'Idempotency-Key': state['start_id']})
                require(status == 200, 'START_NOT_CONFIRMED_USE_POLL')
                save_attempt(state, response, context['validity'])
                apply_attempt_phase(state)
            except BaseException:
                state['phase'] = 'start-uncertain'; replace_private(path, state)
                raise ClientError('START_NOT_CONFIRMED_USE_POLL') from None
        else:
            state = load_state(path, context, args.destination)
            if args.command == 'status':
                return summary('status', state, context)
            if args.command == 'check':
                require(state['phase'] == 'awaiting-code' and state['attempt'] is not None and state['checks'] < 5, 'CHECK_NOT_ADMITTED_USE_POLL')
                require(timestamp(state['attempt']['expires_at']) > time.time(), 'ATTEMPT_EXPIRED_NO_SEND')
                code = (otp_reader or (lambda: read_otp(args.otp_stdin)))()
                require(isinstance(code, str) and re.fullmatch(r'[0-9]{6}', code), 'SIX_DIGIT_OTP_REQUIRED')
                expiry = dt.datetime.fromtimestamp(timestamp(state['attempt']['expires_at']), dt.timezone.utc).isoformat(timespec='milliseconds').replace('+00:00', 'Z')
                payload = {'v': 1, 'purpose': 'resume_device', 'attempt_id': state['attempt_id'], 'device_id': state['candidate_id'],
                           'nonce': state['attempt']['nonce'], 'expires_at': expiry}
                body = {'attempt_id': state['attempt_id'], 'code': code, 'device_signature': node_crypto(state['seed'], canonical(payload))}
                state['check'] = {'id': str(uuid.uuid4())}
                state['checks'] += 1; state['phase'] = 'checking'; replace_private(path, state)
                try:
                    status, response = request(context, 'POST', '/auth/check', body, {'Idempotency-Key': state['check']['id'], 'X-Attempt-Secret': state['continuation']})
                    if status == 200:
                        accept_result(state, response, context, confirmed_check=True)
                    else:
                        state['phase'] = 'check-uncertain'
                except BaseException:
                    state['phase'] = 'check-uncertain'; replace_private(path, state)
                    raise ClientError('CHECK_NOT_CONFIRMED_USE_POLL') from None
                finally:
                    code = None; body = None  # No OTP or its body is written to custody/evidence.
            elif args.command == 'poll':
                require(state['phase'] not in {'closed', 'logging-out', 'logout-uncertain'}, 'POLL_NOT_ADMITTED')
                status, response = request(context, 'GET', '/auth/attempts/' + state['attempt_id'], headers={'X-Attempt-Secret': state['continuation']})
                require(status == 200, 'POLL_NOT_CONFIRMED_NO_AUTOMATIC_SEND')
                accept_result(state, response, context)
            elif args.command == 'logout':
                require(state['logout'] is not None and state['phase'] in {'authorized', 'logging-out', 'logout-uncertain'}, 'LOGOUT_NOT_ADMITTED')
                state['phase'] = 'logging-out'; replace_private(path, state)
                try:
                    status, response = request(context, 'POST', '/auth/logout', {'revocation_ticket': state['logout']['ticket']}, {'Idempotency-Key': state['logout']['id']})
                    require(status == 200 and response.get('status') == 'completed' and response.get('operation_id') == state['logout']['id'], 'LOGOUT_NOT_CONFIRMED')
                    state.update(phase='closed', seed=None, continuation=None, logout=None, attempt=None, check=None)
                except BaseException:
                    state['phase'] = 'logout-uncertain'; replace_private(path, state)
                    raise ClientError('LOGOUT_NOT_CONFIRMED_RETRY_SAME_TICKET') from None
            else:
                raise ClientError('UNKNOWN_ACTION')
        replace_private(path, state)
        context['stats'] = local.guard_status(context['runtime'])
        return summary(args.command, state, context, status)


def read_otp(stdin):
    if stdin:
        value = sys.stdin.readline(32)
        require(len(value) <= 16, 'SIX_DIGIT_OTP_REQUIRED')
        return value.strip()
    require(sys.stdin.isatty(), 'USE_PRIVATE_TTY_OR_OTP_STDIN')
    return getpass.getpass('Código SMS (oculto): ').strip()


def main(argv=None):
    os.umask(0o077)
    parser = SafeParser(description=__doc__)
    parser.add_argument('command', choices=('preflight', 'start', 'check', 'poll', 'status', 'logout'))
    parser.add_argument('--state-dir', required=True, help='Existing isolated runtime, never an external API')
    parser.add_argument('--destination', type=int, choices=(1, 2), help='Index in the authorized private config; never a phone argument')
    parser.add_argument('--allow-one-real-sms', action='store_true')
    parser.add_argument('--confirm-service-validity-seconds', type=int)
    parser.add_argument('--unit-cost-upper-bound-cents', type=int, help='Operator-confirmed bound including provider/channel fees, at most100')
    parser.add_argument('--otp-stdin', action='store_true', help='Read one user-supplied OTP from stdin, never argv/environment/file')
    try:
        result = execute(parser.parse_args(argv))
        print(json.dumps(result, sort_keys=True))
        return 0
    except (ClientError, local.LocalError) as error:
        # Both classes are exclusively fixed labels; no caught raw data.
        print(json.dumps({'ok': False, 'reason': str(error)}), file=sys.stderr)
    except (Exception, KeyboardInterrupt):
        print(json.dumps({'ok': False, 'reason': 'LOCAL_CLIENT_UNAVAILABLE_NO_AUTOMATIC_RETRY'}), file=sys.stderr)
    return 1


if __name__ == '__main__':
    sys.exit(main())
