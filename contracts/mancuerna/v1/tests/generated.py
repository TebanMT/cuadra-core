#!/usr/bin/env python3
"""Regenerate in isolation and compare; never silently repair committed output."""
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

root = Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix='mancuerna-contract-') as tmp:
    target = Path(tmp)
    (target / 'fixtures').mkdir()
    for name in ('build_contract.py', 'build_examples.py'):
        shutil.copy2(root / name, target / name)
        subprocess.run([sys.executable, str(target / name)], check=True)
    for name in ('openapi.json', 'types.ts', 'fixtures/contracts.json', 'fixtures/journeys.json', 'fixtures/consumer.ts'):
        if (root / name).read_bytes() != (target / name).read_bytes():
            raise SystemExit(f'Generated contract differs: {name}. Run pnpm generate and review.')
print('Generated contract matches canonical source.')
