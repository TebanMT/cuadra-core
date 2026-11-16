#!/usr/bin/env python3
"""Filesystem guard and proof vectors; zero real Verify calls or database access."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

CORE = Path(__file__).resolve().parents[2]
ART = Path(tempfile.mkdtemp(prefix='mancuerna-local-guard-', dir='/tmp'))
os.chmod(ART, 0o700)
ENV = {k: v for k, v in os.environ.items() if k in ('PATH', 'HOME', 'TMPDIR', 'GOCACHE', 'GOMODCACHE')}
ENV['GOTOOLCHAIN'] = 'local'
ENV.setdefault('GOCACHE', str(ART / 'go-cache'))
PACKAGE = './src/modules/personalaccounts/infraestructure/verification'
CRYPTO = './src/modules/personalaccounts/infraestructure/security'
result = {'passed': False, 'artifacts': str(ART), 'real_verify_calls': 0,
          'scope': 'Private filesystem, interprocess reservation and crypto vectors; provider calls are explicit synthetic doubles.'}
start = time.monotonic()


def run(args, name, env=None):
    with (ART / (name + '.log')).open('w') as output:
        code = subprocess.run(args, cwd=CORE, env=env or ENV, text=True, stdout=output, stderr=subprocess.STDOUT).returncode
    content = (ART / (name + '.log')).read_text()
    if code:
        raise RuntimeError(name + ': ' + content[-6000:])
    return content


try:
    output = run(['go', 'test', '-race', '-count=1', '-json', '-tags', 'server', '-run', '^TestLocalVerifyGuard', PACKAGE], 'guard')
    events = [json.loads(line) for line in output.splitlines() if line.startswith('{')]
    passed = [e['Test'] for e in events if e.get('Action') == 'pass' and '/' not in e.get('Test', '/')]
    required = ['RequiresExplicitInitializationAndPinsConfiguration', 'ReservesBeforeSendNeverRefundsAndSurvivesRestart',
                'OnlyKnownDestinationAndSIDReachCheckFetch', 'StorageFailureBeforeAndAfterSendFailsClosed',
                'RejectsCorruptionPermissiveFilesAndLinks', 'CancellationAndLockWaitDoNotSpend',
                'ConcurrentProcessesShareFourSlots', 'PublicConstructorPinsActualProvider', 'DoesNotBlockOnNonRegularFiles',
                'PersistsKnownSIDAfterCallerCancellation', 'ProcessExitKeepsReservation']
    required = ['TestLocalVerifyGuard' + name for name in required]
    if any(name not in passed for name in required):
        raise RuntimeError('Required filesystem guard case missing')
    run(['go', 'test', '-race', '-count=1', '-tags', 'server', CRYPTO], 'crypto')
    run(['go', 'vet', '-tags', 'server', PACKAGE, CRYPTO], 'vet')
    run(['go', 'test', '-c', '-tags', 'server', '-o', str(ART / 'verification-linux.test'), PACKAGE], 'linux-compile', {**ENV, 'GOOS': 'linux', 'GOARCH': 'amd64'})
    result['cases'] = required
    result['passed'] = True
except Exception as error:
    result['error'] = str(error)
    print(str(error), file=sys.stderr)
finally:
    sources = [Path(__file__).resolve()]
    for relative in ['src/modules/personalaccounts/infraestructure/verification', 'src/modules/personalaccounts/infraestructure/security', 'src/modules/personalaccounts/domain/security', 'src/modules/personalaccounts/domain/verification']:
        sources.extend((CORE / relative).glob('*.go'))
    result['sources'] = {str(p.relative_to(CORE)): hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(sources)}
    result['seconds'] = round(time.monotonic() - start, 2)
    (ART / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print('Evidence: ' + str(ART / 'result.json'))
sys.exit(0 if result['passed'] else 1)
