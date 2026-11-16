#!/usr/bin/env python3
"""Pure cursor integrity and domain boundaries; no database/provider/network."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

CORE = Path(__file__).resolve().parents[2]
ART = Path(tempfile.mkdtemp(prefix='mancuerna-cursor-', dir='/tmp'))
os.chmod(ART, 0o700)
ENV = {k: v for k, v in os.environ.items() if k in ('PATH', 'HOME', 'TMPDIR', 'GOCACHE', 'GOMODCACHE')}
ENV['GOTOOLCHAIN'] = 'local'
ENV.setdefault('GOCACHE', str(ART / 'go-cache'))
PACKAGES = ['./src/shared/sync/personal/domain', './src/shared/sync/personal/infraestructure/security']
SOURCES = [Path(__file__).resolve(), CORE / 'go.mod', CORE / 'go.sum']
for package in PACKAGES:
    SOURCES.extend((CORE / package).glob('*.go'))
SOURCES.append(CORE / 'src/shared/sync/personal/infraestructure/security/README.md')


def hashes():
    return {str(p.relative_to(CORE)): hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(SOURCES)}


def run(args, name):
    with (ART / (name + '.log')).open('w') as output:
        code = subprocess.run(args, cwd=CORE, env=ENV, text=True, stdout=output, stderr=subprocess.STDOUT).returncode
    content = (ART / (name + '.log')).read_text()
    if code:
        raise RuntimeError(name + ': ' + content[-6000:])
    return content


result = {'passed': False, 'artifacts': str(ART), 'network_calls': 0,
          'scope': 'Pure cursor and domain tests; authorization, compaction and sync endpoints are outside this evidence.'}
start = time.monotonic()
before = hashes()
try:
    output = run(['go', 'test', '-race', '-count=1', '-json', '-run', '^TestCursor', *PACKAGES], 'unit-race')
    events = [json.loads(line) for line in output.splitlines() if line.startswith('{')]
    skipped = [e['Test'] for e in events if e.get('Action') == 'skip' and 'Test' in e]
    passed = [e['Test'] for e in events if e.get('Action') == 'pass' and '/' not in e.get('Test', '/')]
    required = ['DomainPositionDiscriminatesSyncAndStableLists', 'DomainBindingRequiresOwnerEpochKnownQueryAndBoundedSemanticInput',
                'IndependentVectorAndLosslessSequence', 'RejectsCrossOwnerQueryFiltersOrderEpochAndTampering',
                'TTLStrictClockAndUTCNormalization', 'RotationCopiesKeysAndBindsKeyVersion', 'RejectsInvalidKeyringConfiguration',
                'CanonicalFiltersAndContractLengthBoundary', 'StrictWireSchemaRejectsMalformedAuthenticatedClaims']
    required = ['TestCursor' + name for name in required]
    if skipped or any(name not in passed for name in required):
        raise RuntimeError('Required cursor case missing or skipped')
    run(['go', 'vet', *PACKAGES], 'vet')
    if before != hashes():
        raise RuntimeError('Sources changed during verification; repeat against stable sources')
    result['cases'] = required
    result['skipped'] = skipped
    result['passed'] = True
except Exception as error:
    result['error'] = str(error)
    print(str(error), file=sys.stderr)
finally:
    result['sources'] = hashes()
    result['seconds'] = round(time.monotonic() - start, 2)
    (ART / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print('Evidence: ' + str(ART / 'result.json'))
sys.exit(0 if result['passed'] else 1)
