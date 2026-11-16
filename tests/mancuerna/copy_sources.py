#!/usr/bin/env python3
"""Make a private, credential-free source copy for a reproducible local run.

No .env, checkout metadata, build products or application configuration is
copied. The runner still creates its own disposable socket-only PostgreSQL.
Concurrent development may continue after this short copy has completed.
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import tempfile

CORE = Path(__file__).resolve().parents[2]
FOLDERS = ('src', 'cmd', 'infraestructure', 'db_migrations', 'scripts', 'tests', 'contracts')
SUFFIXES = {'.go', '.sql', '.py', '.ts', '.cjs', '.mjs'}
SOURCE_ASSETS = {'src/modules/training/infraestructure/timezone/iana_names.txt'}
EXCLUDED = {'node_modules', '__pycache__', 'evidence', '.git', '.venv'}


def files():
    result = [CORE / 'go.mod', CORE / 'go.sum']
    for folder in FOLDERS:
        for path in (CORE / folder).rglob('*'):
            relative = path.relative_to(CORE)
            if path.is_symlink() or not path.is_file() or any(part in EXCLUDED for part in relative.parts):
                continue
            if path.suffix in SUFFIXES or str(relative) in SOURCE_ASSETS or (folder == 'contracts' and path.suffix == '.json'):
                result.append(path)
    return sorted(result)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


for attempt in range(3):
    root = Path(tempfile.mkdtemp(prefix='mancuerna-source-', dir='/tmp'))
    os.chmod(root, 0o700)
    destination = root / 'cuadra-core'
    selected = files()
    manifest = {}
    for source in selected:
        relative = source.relative_to(CORE)
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, target)
        manifest[str(relative)] = digest(target)
    if selected == files() and all(digest(source) == manifest[str(source.relative_to(CORE))] for source in selected):
        (root / 'source-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
        print(destination)
        break
    # Only discard the newly allocated incomplete copy, never the workspace.
    shutil.rmtree(root)
else:
    raise SystemExit('Sources changed during the copy; retry after the current edit finishes.')
