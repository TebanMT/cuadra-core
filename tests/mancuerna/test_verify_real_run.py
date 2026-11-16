"""Offline client tests: inert transport, no API/Twilio/processes or real config."""
import argparse
import contextlib
import datetime as dt
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

_spec = importlib.util.spec_from_file_location('verify_real_run', Path(__file__).with_name('verify_real_run.py'))
client = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(client)
real_request = client.request


class ClientTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='verify-client-inert-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve(); os.chmod(self.root, 0o700)
        self.runtime = self.root / 'runtime'; self.runtime.mkdir(mode=0o700)
        self.ledger = self.root / 'ledger'; self.ledger.mkdir(mode=0o700)
        self.verify = self.root / 'verify.env'
        self.destinations = ['+525555559901', '+525555559902']  # Invented, never sent.
        self.write(self.verify, ('MANCUERNA_VERIFY_SERVICE_SID=VA' + '1' * 32 + '\nMANCUERNA_TEST_ALLOWED_DESTINATIONS=' + ','.join(self.destinations) + '\nMANCUERNA_TEST_BUDGET_USD=5\n').encode())
        self.write(self.runtime / 'complete.json', b'{"synthetic":true}\n')
        self.config = {'verify': {'account_sid': 'AC' + '2' * 32, 'service_sid': 'VA' + '1' * 32, 'validity_seconds': 600},
                       'local_verify_guard': {'destinations': self.destinations, 'ledger_path': str(self.ledger / 'verify-ledger.json')}}
        from urllib.parse import urlencode
        for key in ('auth_database_url', 'data_database_url'):
            self.config[key] = 'postgresql://synthetic:invented@127.0.0.1:55449/mancuerna_local_test_synthetic?' + urlencode({'host': str(self.runtime / 'socket'), 'sslmode': 'disable'})
        self.save_config()
        self.write(self.ledger / 'verify-ledger.json', client.local.json_bytes({'id': 'synthetic-ledger', 'account_sid': self.config['verify']['account_sid'], 'service_sid': self.config['verify']['service_sid']}))
        self.stats = {'BudgetCents': 500, 'MarginCents': 100, 'ReservedCents': 0, 'StartsReserved': 0, 'RemainingStarts': 4}
        self.calls = []
        for target, value in [(client, {'PRIVATE_ROOT': self.root, 'LEDGER': self.ledger / 'verify-ledger.json', 'VERIFY_FILE': self.verify})]:
            patcher = patch.multiple(target, **value); patcher.start(); self.addCleanup(patcher.stop)
        for target, name, value in [
            (client.local, 'validate_state', lambda _: ({}, {})),
            (client.local, 'guard_status', lambda _: self.stats.copy()),
            (client, 'node_crypto', lambda seed, payload=None: client.b64(bytes([3]) * (64 if payload is not None else 32))),
            (client, 'request', self.transport),
        ]:
            patcher = patch.object(target, name, value); patcher.start(); self.addCleanup(patcher.stop)

    def write(self, path, raw):
        path.write_bytes(raw); path.chmod(0o600)

    def save_config(self):
        self.write(self.runtime / 'config.json', client.local.json_bytes(self.config))

    def args(self, command, **values):
        return argparse.Namespace(command=command, state_dir=str(self.runtime), destination=1, allow_one_real_sms=True,
                                  confirm_service_validity_seconds=600, unit_cost_upper_bound_cents=100, otp_stdin=False, **values)

    def state(self):
        return json.loads((self.root / 'client-1/continuation.json').read_bytes())

    def attempt(self, state=None):
        state = state or self.state()
        expires = dt.datetime.fromtimestamp(state['started_at'] / 1000 + 600, dt.timezone.utc).isoformat(timespec='milliseconds').replace('+00:00', 'Z')
        return {'id': state['attempt_id'], 'nonce': client.b64(bytes([4]) * 32), 'expires_at': expires, 'state': 'pending', 'destination_masked': 'do-not-copy', 'resend_after_seconds': 30}

    def transport(self, context, method, route, body=None, headers=None):
        state = self.state()
        self.calls.append((method, route, body, headers))
        if route == '/auth/start':
            self.assertEqual(state['phase'], 'starting')
            self.assertEqual(body['phone'], self.destinations[0]); self.assertEqual(body['channel'], 'sms')
            self.stats.update(ReservedCents=100, StartsReserved=1, RemainingStarts=3)
            return 200, self.attempt(state)
        if route == '/auth/check':
            self.assertEqual(state['phase'], 'checking')
            self.assertEqual(set(state['check']), {'id'})
            return 200, {'status': 'authorized', 'session': {'account_id': state['new_account_id'], 'device_id': state['candidate_id'],
                         'session_id': '00000000-0000-4000-8000-000000000001', 'revocation_ticket': 'synthetic-limited-ticket-' + 'x' * 32,
                         'access_token': 'NEVER_COPY_BEARER', 'refresh_token': 'NEVER_COPY_REFRESH'}, 'recovery_kit': {'secret': 'NEVER_COPY_KIT'}}
        if route == '/auth/logout':
            self.assertEqual(state['phase'], 'logging-out')
            return 200, {'status': 'completed', 'operation_id': state['logout']['id']}
        return 200, {'status': 'pending', 'attempt': self.attempt(state)}

    def test_preflight_has_no_transport_and_refuses_nonlocal_or_unapproved_inputs(self):
        self.assertEqual(client.execute(self.args('preflight'))['remaining_start_reservations'], 4)
        self.assertEqual(self.calls, [])
        for change in ('dsn', 'destinations', 'ledger', 'budget', 'permissions'):
            with self.subTest(change=change):
                previous = json.loads(json.dumps(self.config)); previous_stats = self.stats.copy()
                if change == 'dsn': self.config['auth_database_url'] = 'postgresql://test:unused@foreign.invalid:55449/test'
                if change == 'destinations': self.config['local_verify_guard']['destinations'] = self.destinations[::-1]
                if change == 'ledger': self.config['local_verify_guard']['ledger_path'] = str(self.root / 'other.json')
                if change == 'budget': self.stats['BudgetCents'] = 1000
                self.save_config()
                if change == 'permissions': self.verify.chmod(0o644)
                with self.assertRaises((client.ClientError, client.local.LocalError)): client.execute(self.args('preflight'))
                self.verify.chmod(0o600); self.config = previous; self.stats = previous_stats; self.save_config()
        self.assertEqual(self.calls, [])

    def test_start_needs_explicit_action_bound_and_validity_never_resends(self):
        for key, value in [('allow_one_real_sms', False), ('confirm_service_validity_seconds', 120), ('unit_cost_upper_bound_cents', 101)]:
            args = self.args('start'); setattr(args, key, value)
            with self.assertRaises(client.ClientError): client.execute(args)
        self.assertEqual(self.calls, [])
        result = client.execute(self.args('start'))
        self.assertEqual(result['phase'], 'awaiting-code'); self.assertEqual(result['reserved_cents'], 100)
        with self.assertRaises(client.ClientError): client.execute(self.args('start'))
        self.assertEqual(len(self.calls), 1)
        saved = (self.root / 'client-1/continuation.json').read_text()
        self.assertNotIn(self.destinations[0], saved); self.assertNotIn('do-not-copy', saved)

    def test_no_budget_or_custody_failure_prevents_all_http(self):
        self.stats.update(StartsReserved=4, ReservedCents=400, RemainingStarts=0)
        with self.assertRaises(client.ClientError): client.execute(self.args('start'))
        self.stats.update(StartsReserved=0, ReservedCents=0, RemainingStarts=4)
        with patch.object(client, 'replace_private', side_effect=OSError('synthetic failure')):
            with self.assertRaises(OSError): client.execute(self.args('start'))
        self.assertEqual(self.calls, [])

    def test_lost_start_response_retains_exact_attempt_and_poll_does_not_resend(self):
        with patch.object(client, 'request', side_effect=OSError('private transport detail')):
            with self.assertRaisesRegex(client.ClientError, '^START_NOT_CONFIRMED_USE_POLL$'): client.execute(self.args('start'))
        original = self.state(); self.assertEqual(original['phase'], 'start-uncertain')
        with self.assertRaises(client.ClientError): client.execute(self.args('start'))
        self.assertEqual(client.execute(self.args('poll'))['phase'], 'awaiting-code')
        self.assertEqual(self.state()['attempt_id'], original['attempt_id'])
        self.assertTrue(all(method == 'GET' for method, *_ in self.calls))

    def test_expiry_mismatch_blocks_check_and_pending_poll_cannot_hide_it(self):
        def incorrect(*args, **kwargs):
            response = self.attempt(); response['expires_at'] = dt.datetime.fromtimestamp(time.time() + 120, dt.timezone.utc).isoformat(timespec='milliseconds').replace('+00:00', 'Z')
            return 200, response
        with patch.object(client, 'request', incorrect):
            with self.assertRaises(client.ClientError): client.execute(self.args('start'))
        with self.assertRaises(client.ClientError): client.execute(self.args('check'), lambda: '123456')
        self.assertEqual(self.calls, [])

    def test_check_requires_human_code_and_stores_no_otp_tokens_or_kit_then_logout_cleans(self):
        client.execute(self.args('start'))
        with self.assertRaises(client.ClientError): client.execute(self.args('check'), lambda: 'bad-code')
        self.assertEqual(len(self.calls), 1)
        result = client.execute(self.args('check'), lambda: '987654')
        self.assertEqual(result['phase'], 'authorized')
        saved = (self.root / 'client-1/continuation.json').read_text()
        for forbidden in ('987654', 'NEVER_COPY_BEARER', 'NEVER_COPY_REFRESH', 'NEVER_COPY_KIT', self.destinations[0]):
            self.assertNotIn(forbidden, saved); self.assertNotIn(forbidden, json.dumps(result))
        self.assertEqual(client.execute(self.args('logout'))['phase'], 'closed')
        self.assertIsNone(self.state()['seed']); self.assertIsNone(self.state()['continuation']); self.assertIsNone(self.state()['logout'])
        with self.assertRaises(client.ClientError): client.execute(self.args('start'))

    def test_uncertain_check_never_replays_or_clears_it_on_pending_poll(self):
        client.execute(self.args('start'))
        with patch.object(client, 'request', side_effect=TimeoutError('private OTP must never appear')):
            with self.assertRaisesRegex(client.ClientError, '^CHECK_NOT_CONFIRMED_USE_POLL$'): client.execute(self.args('check'), lambda: '987654')
        original = self.state()['check']; self.assertEqual(self.state()['phase'], 'check-uncertain')
        self.assertEqual(client.execute(self.args('poll'))['phase'], 'check-uncertain')
        with self.assertRaises(client.ClientError): client.execute(self.args('check'), lambda: '987654')
        self.assertEqual(self.state()['check'], original); self.assertEqual(self.state()['checks'], 1)

    def test_confirmed_pending_check_allows_a_new_manual_code_without_another_sms(self):
        client.execute(self.args('start'))
        def wrong_code(context, method, route, body=None, headers=None):
            self.calls.append((method, route, body, headers))
            return 200, {'status': 'pending', 'attempt': self.attempt()}
        with patch.object(client, 'request', wrong_code):
            self.assertEqual(client.execute(self.args('check'), lambda: '111111')['phase'], 'awaiting-code')
        previous_check = self.state()['check']['id']
        self.assertEqual(client.execute(self.args('poll'))['phase'], 'awaiting-code')
        self.assertEqual(client.execute(self.args('check'), lambda: '987654')['phase'], 'authorized')
        self.assertNotEqual(self.state()['check']['id'], previous_check)
        self.assertEqual(self.state()['checks'], 2)
        self.assertEqual(sum(route == '/auth/start' for _, route, *_ in self.calls), 1)
        self.assertEqual(self.stats['ReservedCents'], 100)

    def test_api_uncertain_is_decoded_but_never_admits_another_check(self):
        client.execute(self.args('start'))
        def uncertain(context, method, route, body=None, headers=None):
            response = self.attempt(); response['state'] = 'uncertain'
            return 200, {'status': 'pending', 'attempt': response}
        with patch.object(client, 'request', uncertain):
            self.assertEqual(client.execute(self.args('poll'))['phase'], 'start-uncertain')
            with self.assertRaises(client.ClientError): client.execute(self.args('check'), lambda: '987654')
        self.assertEqual(client.execute(self.args('poll'))['phase'], 'awaiting-code')
        with patch.object(client, 'request', uncertain):
            self.assertEqual(client.execute(self.args('check'), lambda: '987654')['phase'], 'check-uncertain')
            self.assertEqual(client.execute(self.args('poll'))['phase'], 'check-uncertain')
        original = self.state()['check']
        self.assertEqual(client.execute(self.args('poll'))['phase'], 'check-uncertain')
        with self.assertRaises(client.ClientError): client.execute(self.args('check'), lambda: '987654')
        self.assertEqual(self.state()['check'], original)

    def test_pending_top_level_uses_nested_attempt_terminals_and_never_reopens_code_entry(self):
        client.execute(self.args('start')); path = self.root / 'client-1/continuation.json'; original = path.read_bytes()
        for nested in ('expired', 'denied', 'cancelled', 'authorization_pending', 'verified', 'consumed'):
            with self.subTest(state=nested):
                self.write(path, original)
                def response(*args, **kwargs):
                    attempt = self.attempt(); attempt['state'] = nested
                    return 200, {'status': 'pending', 'attempt': attempt}
                with patch.object(client, 'request', response):
                    self.assertEqual(client.execute(self.args('poll'))['phase'], nested)
                with self.assertRaises(client.ClientError): client.execute(self.args('check'), lambda: '987654')
                # Even a later pending GET cannot undo an observed terminal.
                self.assertEqual(client.execute(self.args('poll'))['phase'], nested)
        self.assertEqual(sum(route == '/auth/start' for _, route, *_ in self.calls), 1)

    def test_noncontract_top_level_status_is_rejected_instead_of_becoming_a_terminal(self):
        client.execute(self.args('start'))
        for status in ('verified', 'denied', 'expired', 'cancelled'):
            with self.subTest(status=status), patch.object(client, 'request', return_value=(200, {'status': status, 'attempt': self.attempt()})):
                with self.assertRaises(client.ClientError): client.execute(self.args('poll'))
                self.assertEqual(self.state()['phase'], 'awaiting-code')

    def test_custody_rejects_changed_config_unknown_fields_and_permissive_files(self):
        client.execute(self.args('start')); path = self.root / 'client-1/continuation.json'; original = path.read_bytes()
        for change in ('unknown', 'binding', 'permissions'):
            with self.subTest(change=change):
                value = json.loads(original)
                if change == 'unknown': value['otp'] = 'do-not-accept'
                if change == 'binding': value['runtime_hash'] = '0' * 64
                self.write(path, client.local.json_bytes(value))
                if change == 'permissions': path.chmod(0o644)
                with self.assertRaises((client.ClientError, client.local.LocalError)): client.execute(self.args('status'))
                self.write(path, original)
        self.assertEqual(len(self.calls), 1)

    def test_raw_exceptions_never_reach_stdout_or_stderr(self):
        output = io.StringIO()
        with patch.object(client, 'preflight', side_effect=RuntimeError('NEVER_PRINT_SECRET')), contextlib.redirect_stderr(output):
            code = client.main(['preflight', '--state-dir', str(self.runtime)])
        self.assertEqual(code, 1); self.assertNotIn('NEVER_PRINT_SECRET', output.getvalue())
        self.assertEqual(json.loads(output.getvalue())['reason'], 'LOCAL_CLIENT_UNAVAILABLE_NO_AUTOMATIC_RETRY')

    def test_invalid_cli_never_echoes_a_secret_argument(self):
        output = io.StringIO()
        with contextlib.redirect_stderr(output):
            code = client.main(['preflight', '--state-dir', str(self.runtime), '--otp', 'NEVER_PRINT_OTP'])
        self.assertEqual(code, 1); self.assertNotIn('NEVER_PRINT_OTP', output.getvalue())
        self.assertEqual(json.loads(output.getvalue())['reason'], 'INVALID_CLI_ARGUMENTS')

    def test_http_transport_is_fixed_loopback_bounded_no_store_and_never_retries(self):
        calls = []
        class Connection:
            def __init__(self, host, port, timeout): calls.append(('connect', host, port, timeout))
            def request(self, method, path, body, headers): calls.append(('request', method, path, body, headers))
            def getresponse(self): return self
            def getheader(self, name): return 'no-store'
            def read(self, maximum): calls.append(('read', maximum)); return b'{"status":"pending"}'
            def close(self): calls.append(('close',))
            status = 200
        context = client.preflight(self.runtime)
        with patch.object(client.local, 'control', return_value={'state': 'running', 'address': '127.0.0.1:18883'}), patch.object(client.http.client, 'HTTPConnection', Connection):
            self.assertEqual(real_request(context, 'POST', '/auth/check', {'code': 'synthetic'}, {'X-Attempt-Secret': 'private'})[0], 200)
            self.assertEqual(calls[0], ('connect', '127.0.0.1', 18883, 45))
            self.assertEqual(calls[1][1:3], ('POST', '/api/mancuerna/v1/auth/check'))
            self.assertEqual(calls[2], ('read', 32769)); self.assertEqual(calls[3], ('close',))
            self.assertEqual(len(calls), 4)
            with patch.object(Connection, 'getresponse', side_effect=OSError('private details')):
                with self.assertRaises(OSError): real_request(context, 'POST', '/auth/check', {})
            self.assertEqual(sum(call[0] == 'connect' for call in calls), 2)
            with self.assertRaises(client.ClientError): real_request(context, 'GET', 'https://foreign.invalid')
            self.assertEqual(sum(call[0] == 'connect' for call in calls), 2)
            with patch.object(Connection, 'getheader', return_value='public'):
                with self.assertRaises(client.ClientError): real_request(context, 'POST', '/auth/check', {})


if __name__ == '__main__':
    unittest.main()
