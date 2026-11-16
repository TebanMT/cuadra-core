#!/usr/bin/env python3
"""Fresh isolated real Go/JWT/PostgreSQL + mobile coordinator US002 journey.

Run only after central source freeze. Reuses the existing foreground synthetic
mobile fixture without changing backend product or its control routes. Never
reads .env or production configuration. The Node runner explicitly substitutes
in-memory custody and local presence; this is not a native/device journey.
"""
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time

CORE = Path(__file__).resolve().parents[2]
MOBILE = CORE.parent / 'mancuerna-mobile'
sys.path.insert(0, str(CORE / 'scripts/mancuerna-local'))
import local

PREFIX = '/api/mancuerna/v1'
QUERY = """SELECT json_build_object(
'accounts',(SELECT count(*) FROM mancuerna.accounts),
'devices',(SELECT count(*) FROM mancuerna.devices),
'device_methods',(SELECT json_object_agg(authorized_by_method,n) FROM (SELECT authorized_by_method,count(*) n FROM mancuerna.devices GROUP BY authorized_by_method) x),
'sessions',(SELECT count(*) FROM mancuerna.sessions),
'live_sessions',(SELECT count(*) FROM mancuerna.sessions WHERE revoked_at IS NULL),
'authorizations',(SELECT json_object_agg(status,n) FROM (SELECT status,count(*) n FROM mancuerna.device_authorizations GROUP BY status) x),
'authorization_chronology_valid',(SELECT bool_and(created_at IS NOT NULL AND created_at<expires_at AND decided_at>=created_at) FROM mancuerna.device_authorizations),
'approved_candidate_sessions',(SELECT count(*) FROM mancuerna.device_authorizations a JOIN mancuerna.auth_challenges c ON c.id=a.challenge_id JOIN mancuerna.sessions s ON s.account_id=a.account_id AND s.device_id=c.candidate_device_id WHERE a.status='approved'),
'denied_candidate_sessions',(SELECT count(*) FROM mancuerna.device_authorizations a JOIN mancuerna.auth_challenges c ON c.id=a.challenge_id JOIN mancuerna.sessions s ON s.account_id=a.account_id AND s.device_id=c.candidate_device_id WHERE a.status='denied'),
'decision_receipts',(SELECT count(*) FROM mancuerna.auth_receipts WHERE purpose='device_authorization_decision'),
'candidate_result_receipts',(SELECT count(*) FROM mancuerna.auth_receipts WHERE purpose='device_authorization_result'),
'device_proofs',(SELECT count(*) FROM mancuerna.device_proofs),
'consumed_device_proofs',(SELECT count(*) FROM mancuerna.device_proofs WHERE purpose='authorize_device' AND verified_at IS NOT NULL AND consumed_at IS NOT NULL),
'approval_events',(SELECT count(*) FROM mancuerna.security_events WHERE event_type='device_authorization_approved'),
'denial_events',(SELECT count(*) FROM mancuerna.security_events WHERE event_type='device_authorization_denied'),
'vaults',(SELECT count(*) FROM mancuerna.vault_keys),
'workouts',(SELECT count(*) FROM mancuerna.workouts),
'snapshots',(SELECT count(*) FROM mancuerna.sync_snapshots))"""
EXPECTED = {
    'accounts': 2, 'devices': 3, 'device_methods': {'signup': 2, 'device': 1},
    'sessions': 3, 'live_sessions': 0, 'authorizations': {'approved': 1, 'denied': 1},
    'authorization_chronology_valid': True, 'approved_candidate_sessions': 1, 'denied_candidate_sessions': 0,
    'decision_receipts': 2, 'candidate_result_receipts': 1, 'device_proofs': 2, 'consumed_device_proofs': 2,
    'approval_events': 1, 'denial_events': 1, 'vaults': 0, 'workouts': 0, 'snapshots': 0,
}


def require(condition, code):
    if not condition:
        raise RuntimeError(code)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def unique(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, 'DUPLICATE_JSON_PROPERTY')
        result[key] = value
    return result


def read_json(path, maximum=2_097_152):
    require(path.is_file() and not path.is_symlink(), 'INVALID_RESULT_PATH')
    raw = path.read_bytes()
    require(len(raw) <= maximum, 'RESULT_SIZE')
    return json.loads(raw, object_pairs_hook=unique)


def save(path, value):
    local.write_new(path, (json.dumps(value, indent=2) + '\n').encode())


def control(runtime, method='GET', route='/status'):
    class UnixHTTP(http.client.HTTPConnection):
        def connect(self):
            self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            self.sock.settimeout(5)
            self.sock.connect(str(runtime / 'fixture-control.sock'))
    call = UnixHTTP('localhost')
    try:
        call.request(method, route, body='{}' if method == 'POST' else None, headers={'Content-Type': 'application/json'})
        response = call.getresponse()
        raw = response.read(131_073)
        require(len(raw) <= 131_072 and response.status == (204 if method == 'POST' else 200), 'CONTROL_FAILED')
        return None if method == 'POST' else json.loads(raw, object_pairs_hook=unique)
    finally:
        call.close()


def sanitized_control(raw):
    require(raw.get('scope') == 'synthetic Verify only; real API and PostgreSQL' and raw.get('state') == 'running', 'CONTROL_SCOPE')
    calls, statuses, provider = raw.get('api_calls'), raw.get('api_response_statuses'), raw.get('provider')
    require(isinstance(calls, dict) and isinstance(statuses, dict) and isinstance(provider, dict), 'CONTROL_SHAPE')
    require(set(statuses) == set(calls), 'CONTROL_STATUS_COVERAGE')
    for path, count in calls.items():
        require(path.startswith(PREFIX + '/') and '?' not in path and type(count) is int and count >= 0, 'CONTROL_COUNT')
        require(all(re.fullmatch('[1-5][0-9]{2}', code) and type(n) is int and n >= 0 for code, n in statuses[path].items()), 'CONTROL_STATUS')
        require(sum(statuses[path].values()) == count, 'CONTROL_STATUS_COUNT')
    require(set(provider) <= {'synthetic_starts', 'synthetic_checks', 'synthetic_fetches', 'blocked_network_requests', 'blocked_destination_requests'}, 'CONTROL_PROVIDER')
    require(all(type(n) is int and n >= 0 for n in provider.values()), 'CONTROL_PROVIDER_COUNT')
    return {'api_calls': calls, 'api_response_statuses': statuses, 'provider': provider,
            'scope': 'Existing fixture counts selected routes only; proof/decision traces are client observations plus independent PostgreSQL receipts.'}


def source_match(result):
    before, after = result.get('sourcesBefore'), result.get('sourcesAfter')
    expected = {'scripts/device-authorization-go-fixture.ts', 'package.json', 'package-lock.json', 'tsconfig.json'}
    expected.update(str(p.relative_to(MOBILE)) for p in (MOBILE / 'src').rglob('*') if p.is_file() and p.suffix in {'.ts', '.tsx'})
    require(isinstance(before, dict) and isinstance(after, dict) and set(before) == set(after) == expected, 'MOBILE_SOURCE_SET')
    require(result.get('sourceSetUnchanged') is True, 'MOBILE_SOURCES_CHANGED')
    for name in expected:
        path = MOBILE / name
        require(not path.is_symlink() and re.fullmatch('[a-f0-9]{64}', before[name]) and before[name] == after[name] == digest(path.read_bytes()), 'MOBILE_SOURCE_MISMATCH')
    return len(expected)


def observations(runtime):
    # Used for both successful assertions and failure diagnostics. Validate the
    # synthetic runtime and live control endpoint before querying its private PG.
    manifest, identity = local.validate_state(runtime)
    verify = local.json_private(runtime / 'config.json')['verify']
    require(verify['account_sid'] == 'AC' + '0' * 32 and verify['service_sid'] == 'VA' + '0' * 32 and verify['password'] == 'a' * 32, 'NOT_SYNTHETIC')
    pg = Path(manifest['pg_bin'])
    metrics = sanitized_control(control(runtime))
    require(local.sql(runtime, identity, pg, 'SELECT system_identifier FROM pg_control_system()') == manifest['system_id'], 'POSTGRES_IDENTITY')
    database = json.loads(local.sql(runtime, identity, pg, QUERY), object_pairs_hook=unique)
    return database, metrics, identity, pg


def aggregate(runtime, runner):
    database, metrics, identity, pg = observations(runtime)
    require(database == EXPECTED, 'DATABASE_AGGREGATE_MISMATCH')
    require(metrics['provider'].get('synthetic_starts') == metrics['provider'].get('synthetic_checks') == 4, 'VERIFY_COUNT')
    require(all(metrics['provider'].get(key, 0) == 0 for key in ['synthetic_fetches', 'blocked_network_requests', 'blocked_destination_requests']), 'UNEXPECTED_PROVIDER_REQUEST')
    require(all(metrics['api_calls'].get(PREFIX + path, 0) == n for path, n in {'/auth/start': 4, '/auth/check': 4, '/auth/logout': 3, '/auth/refresh': 0, '/commands': 0, '/sync/push': 0, '/me/vaults': 0}.items()), 'API_COUNTS')
    require(runner.get('passed') is True and runner.get('failure') is None and len(runner.get('checks', [])) == 9, 'RUNNER_FAILED')
    require(all(runner.get(key) is True for key in ['replayExact', 'candidateResultExact', 'foreignActorRejected', 'changedDecisionRejected', 'candidateOtpHasNoAuthority', 'approvalCreatedAtObserved', 'denialCreatesNoLocalSession', 'crossSessionResultRead', 'liveOriginalUnresolved', 'revokedOriginalNotApplied', 'foreignResultRejected']), 'RUNNER_REQUIRED_CHECKS')
    require(runner.get('presenceCalls') == 2 and runner.get('lostResponses') == 1 and runner.get('distinctAuthorizedSessions') == runner.get('cleanupRequests') == runner.get('cleanupConfirmed') == 3, 'RUNNER_COUNTS')
    traces = runner.get('decisionTraces', [])
    require(len(traces) == 5 and sum(t['status'] == 200 for t in traces) == 3, 'DECISION_TRACE_COUNTS')
    receipts = json.loads(local.sql(runtime, identity, pg, "SELECT coalesce(json_agg(operation_id::text ORDER BY operation_id),'[]'::json) FROM mancuerna.auth_receipts WHERE purpose='device_authorization_decision'"))
    # Database operation IDs stay in this process; only hashes are reported.
    receipt_hashes = sorted(digest(value.encode()) for value in receipts)
    observed_hashes = sorted({t['operationHash'] for t in traces if t['status'] == 200})
    require(receipt_hashes == observed_hashes and len(receipt_hashes) == 2, 'RECEIPT_BINDING')
    return {'passed': True, 'database': database, 'expected_database': EXPECTED, 'control': metrics,
            'decision_receipt_hashes': receipt_hashes, 'mobile_source_files': source_match(runner),
            'external_verify_requests': 0, 'production_accessed': False,
            'scope': 'Real API/PostgreSQL approval/denial, exact replay and new-session result lookup before/after original logout; not native custody/presence',
            'limitations': ['Local presence is injected confirmed; memory AuthState survives coordinator reconstruction only',
                           'Existing control endpoint does not count proof/decision routes; actual client response hashes and PostgreSQL receipts are checked separately',
                           'Aggregate precedes server shutdown; wrapper result separately confirms supervisor exit']}


def record_failure_observation(runtime, out, payload):
    # Never turn a failed journey into PASS or replace its original failure.
    # The real count may include a candidate session whose response was not read.
    try:
        database, metrics, _, _ = observations(runtime)
        save(out / 'failure-aggregate.json', {
            'diagnostic_only': True, 'journey_passed': False,
            'original_failure': payload.get('error'), 'database': database, 'control': metrics,
            'scope': 'Observed before shutdown after failure; no expected aggregate or zero-live-session assertion',
        })
        payload['failure_aggregate'] = 'failure-aggregate.json'
    except Exception:
        payload['failure_aggregate_unavailable'] = True


def main():
    require(len(sys.argv) == 1, 'USAGE_NO_ARGUMENTS')
    os.umask(0o077)
    out = Path(tempfile.mkdtemp(prefix='mc-device-authorization-run-', dir='/private/tmp'))
    supervisor = None
    runtime = None
    phase = 'prepare'
    payload = {'passed': False, 'scope': 'Fresh local synthetic Verify + real API/JWT/PostgreSQL and mobile coordinator',
               'external_verify_requests': 0, 'production_accessed': False, 'native_journey': False}
    started = time.monotonic()
    try:
        require(MOBILE.is_dir() and (MOBILE / 'scripts/device-authorization-go-fixture.ts').is_file(), 'MOBILE_RUNNER_MISSING')
        environment = local.clean_env()
        # Environment is allowlisted: no .env, DATABASE_URL, cloud URLs or provider credentials.
        with (out / 'supervisor.log').open('wb') as output:
            supervisor = subprocess.Popen([sys.executable, 'scripts/mancuerna-local/mobile_fixture.py'], cwd=CORE, env=environment,
                                          stdin=subprocess.DEVNULL, stdout=output, stderr=subprocess.STDOUT)
            phase = 'fixture-ready'
            until = time.monotonic() + 420
            while runtime is None:
                require(supervisor.poll() is None and time.monotonic() < until, 'FIXTURE_NOT_READY')
                # Read only the supervisor JSON status; never copy private config or logs.
                lines = (out / 'supervisor.log').read_text(errors='replace').splitlines()
                for line in lines:
                    if not line.startswith('{'):
                        continue
                    try:
                        message = json.loads(line)
                    except json.JSONDecodeError:
                        continue
                    if message.get('state') == 'ready':
                        path = Path(message['runtime']).resolve()
                        require(re.fullmatch(r'/private/tmp/mc-mobile-fixture-[A-Za-z0-9_]+/runtime', str(path)), 'RUNTIME_PATH')
                        runtime = path
                        break
                if runtime is None:
                    time.sleep(0.2)
            payload['runtime'] = str(runtime)
            print(json.dumps({'state': 'ready', 'runtime': str(runtime), 'synthetic_verify_only': True}), flush=True)
            phase = 'mobile-runner'
            with (out / 'mobile-runner.log').open('wb') as mobile_log:
                mobile_process = subprocess.run(['node', '--experimental-strip-types', 'scripts/device-authorization-go-fixture.ts', str(runtime)],
                                                cwd=MOBILE, env=environment, stdin=subprocess.DEVNULL, stdout=mobile_log, stderr=subprocess.STDOUT, timeout=180)
            payload['mobile_exit_code'] = mobile_process.returncode
            messages = []
            for line in (out / 'mobile-runner.log').read_text(errors='replace').splitlines():
                if line.startswith('{'):
                    try:
                        messages.append(json.loads(line))
                    except json.JSONDecodeError:
                        pass
            last = next((m for m in reversed(messages) if isinstance(m.get('result'), str)), None)
            require(last is not None, 'MOBILE_RESULT_MISSING')
            result_path = Path(last['result'])
            require(re.fullmatch(r'/private/tmp/mc-device-authorization-go-[A-Za-z0-9]+/result\.json', str(result_path)) and result_path.resolve() == result_path, 'MOBILE_RESULT_PATH')
            runner = read_json(result_path)
            raw = result_path.read_bytes()
            require(read_json(result_path.parent / 'allowlist.json') == {'version': 1, 'files': [{'file': 'result.json', 'bytes': len(raw), 'sha256': digest(raw)}]}, 'MOBILE_ALLOWLIST')
            local.write_new(out / 'mobile-result.json', raw)
            require(mobile_process.returncode == 0, 'MOBILE_RUNNER_FAILED')
            phase = 'database-aggregate'
            save(out / 'aggregate.json', aggregate(runtime, runner))
            phase = 'stop'
            control(runtime, 'POST', '/stop')
            supervisor.wait(timeout=30)
            require(supervisor.returncode == 0, 'SUPERVISOR_FAILED')
            payload['supervisor_exit_code'] = supervisor.returncode
            runtime_result = read_json(runtime.parent / 'result.json')
            require(runtime_result.get('passed') is True, 'SUPERVISOR_RESULT_FAILED')
            require(not (runtime / 'fixture-ready').exists() and not (runtime / 'fixture-control.sock').exists(), 'RUNTIME_NOT_CLOSED')
            source_path = Path(runtime_result['source_manifest']).resolve()
            require(re.fullmatch(r'/private/tmp/mancuerna-source-[A-Za-z0-9_]+/source-manifest\.json', str(source_path)), 'BACKEND_MANIFEST_PATH')
            manifest = read_json(source_path)
            for name, value in manifest.items():
                path = CORE / name
                require(path.resolve().is_relative_to(CORE) and path.is_file() and not path.is_symlink() and digest(path.read_bytes()) == value, 'BACKEND_SOURCE_CHANGED')
            shutil.copyfile(source_path, out / 'backend-source-manifest.json')
            payload.update(passed=True, runtime=str(runtime), backend_source_files=len(manifest), checks=len(runner['checks']),
                           postgres_stopped_by_supervisor=True, runtime_markers_absent=True, live_sessions=0)
            phase = 'complete'
    except Exception:
        payload['error'] = {'phase': phase, 'code': 'DEVICE_AUTHORIZATION_MOBILE_RUN_FAILED'}
    finally:
        if not payload['passed'] and runtime is not None and supervisor is not None and supervisor.poll() is None:
            record_failure_observation(runtime, out, payload)
        # Only our direct child can be stopped; never search ports or signal unrelated processes.
        if supervisor is not None and supervisor.poll() is None:
            if runtime is not None:
                try:
                    control(runtime, 'POST', '/stop')
                    supervisor.wait(timeout=30)
                except Exception:
                    local.stop_child(supervisor, signal.SIGTERM)
            else:
                local.stop_child(supervisor, signal.SIGTERM)
        if supervisor is not None:
            payload['supervisor_exit_code'] = supervisor.returncode
        if runtime is not None and supervisor is not None and supervisor.returncode == 0:
            try:
                ended = read_json(runtime.parent / 'result.json')
                payload['supervisor_result_passed'] = ended.get('passed') is True
                payload['runtime_markers_absent'] = not (runtime / 'fixture-ready').exists() and not (runtime / 'fixture-control.sock').exists()
                payload['postgres_stopped_by_supervisor'] = payload['supervisor_result_passed'] and payload['runtime_markers_absent']
            except Exception:
                payload['supervisor_result_passed'] = False
        payload['phase'] = phase
        payload['seconds'] = round(time.monotonic() - started, 3)
        payload['logs_private_not_allowlisted'] = True
        save(out / 'result.json', payload)
        safe = sorted(p for p in out.iterdir() if p.suffix == '.json')
        save(out / 'allowlist.json', {'version': 1, 'files': [{'file': p.name, 'bytes': p.stat().st_size, 'sha256': digest(p.read_bytes())} for p in safe]})
        print(json.dumps({'passed': payload['passed'], 'phase': phase, 'evidence': str(out / 'result.json'), 'supervisor_exit_code': payload.get('supervisor_exit_code')}), flush=True)
    return 0 if payload['passed'] else 1


if __name__ == '__main__':
    sys.exit(main())
