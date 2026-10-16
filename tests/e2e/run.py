#!/usr/bin/env python3
"""Reproducible local-only E2E, optional tagged sidecar and real browser UI.
Creates unique databases, builds binaries, starts processes and always cleans
up. A private artifact directory retains synthetic diagnostics on failure.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import socket
import signal
import subprocess
import sys
import tarfile
import tempfile
import urllib.parse
import uuid


def run():
    p = argparse.ArgumentParser()
    p.add_argument('--admin-dsn', required=True)
    p.add_argument('--artifacts')
    p.add_argument('--sidecar-ref', help='real local Git tag/commit; upgrade to current after scenarios')
    p.add_argument('--browser', action='store_true')
    p.add_argument('--keep', action='store_true', help='keep synthetic servers for manual acceptance; create runtime/stop to finish')
    p.add_argument('--desktop', type=Path)
    p.add_argument('--dashboard', type=Path)
    args = p.parse_args()
    url = urllib.parse.urlsplit(args.admin_dsn)
    if url.hostname != '127.0.0.1' or url.port != 55439 or set(urllib.parse.parse_qs(url.query)) - {'sslmode', 'connect_timeout'}:
        p.error('only explicit 127.0.0.1:55439 PostgreSQL is accepted')
    if args.browser and (not args.desktop or not args.dashboard):
        p.error('--browser requires --desktop and --dashboard repositories')
    ports = [18080, 18081, 19091, 19092] + ([5199, 5200] if args.browser else [])
    for port in ports:
        with socket.socket() as sock:
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            try:
                sock.bind(('127.0.0.1', port))
            except OSError:
                p.error(f'local test port {port} is occupied; no existing process will be stopped')
    repo = Path(__file__).resolve().parents[2]
    root = Path(args.artifacts or tempfile.mkdtemp(prefix='tinta-e2e-')).resolve()
    if root.exists() and any(root.iterdir()):
        p.error('artifact directory must be empty; previous evidence is never overwritten')
    root.mkdir(parents=True, exist_ok=True)
    os.chmod(root, 0o700)
    suffix = uuid.uuid4().hex
    name, restore_name, role = 'tinta_e2e_'+suffix, 'tinta_restore_'+suffix, 'tinta_app_'+suffix
    dsn = urllib.parse.urlunsplit(url._replace(path='/'+name))
    restore_dsn = urllib.parse.urlunsplit(url._replace(path='/'+restore_name))
    clean = {k: os.environ[k] for k in ('PATH', 'HOME', 'TMPDIR', 'GOCACHE', 'GOMODCACHE', 'CI') if k in os.environ}
    clean['TINTA_MODE'] = 'test'
    processes = []
    databases = []
    role_created = False

    def command(cmd, **kw):
        return subprocess.run([str(x) for x in cmd], check=True, **kw)

    def sql(connection, query):
        # Pass connection components through the environment, not argv/logs.
        target = urllib.parse.urlsplit(connection)
        env = {**clean, 'PGHOST':target.hostname, 'PGPORT':str(target.port), 'PGDATABASE':target.path[1:], 'PGUSER':urllib.parse.unquote(target.username or ''), 'PGPASSWORD':urllib.parse.unquote(target.password or ''), 'PGSSLMODE':'disable'}
        return subprocess.check_output(['psql', '-XAt', '-v', 'ON_ERROR_STOP=1', '-c', query], env=env, text=True).strip()

    try:
        for database in (name, restore_name):
            sql(args.admin_dsn, f'CREATE DATABASE "{database}"')
            databases.append(database)
        current_sidecar = root/'sidecar-current'
        for target, tag, package in [('server', 'server', './cmd/server'), ('sidecar-current', 'sidecar', './cmd/sidecar'), ('migrate', 'server', './cmd/migrate'), ('tenant-backup', 'server', './cmd/tenant-backup')]:
            command(['go', 'build', '-tags', tag, '-o', root/target, package], cwd=repo, env=clean)
        command([root/'migrate', '--dir', repo/'db_migrations/postgres'], env={**clean,'DATABASE_URL':dsn})
        password = uuid.uuid4().hex
        sql(args.admin_dsn, f"CREATE ROLE {role} LOGIN PASSWORD '{password}' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE")
        role_created = True
        command([root/'migrate', '--dir', repo/'db_migrations/postgres', '--app-role', role], env={**clean,'DATABASE_URL':dsn})
        # The real HTTP server and fixture requests run using this role.
        app_dsn = urllib.parse.urlunsplit(url._replace(netloc=f'{role}:{password}@127.0.0.1:55439', path='/'+name))
        assert sql(app_dsn, 'SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user') == 'f'
        sidecar = current_sidecar
        provenance = {'cloud_base':subprocess.check_output(['git','rev-parse','HEAD'],cwd=repo,text=True).strip(), 'mode':'test', 'postgres':sql(dsn,'SHOW server_version'), 'application_role':{'superuser':False,'bypassrls':False}, 'platform':sys.platform}
        provenance['cloud_diff_sha256'] = hashlib.sha256(subprocess.check_output(['git','diff','HEAD'],cwd=repo)).hexdigest()
        def source_digest(folder):
            digest = hashlib.sha256()
            paths = subprocess.check_output(['git','ls-files','-c','--others','--exclude-standard','-z'],cwd=folder).decode().split('\0')
            for name in sorted(set(paths) - {''}):
                path=folder/name
                digest.update(name.encode()+b'\0')
                digest.update(path.read_bytes() if path.is_file() else b'<missing>')
            return digest.hexdigest()
        provenance['cloud_source_sha256']=source_digest(repo)
        if args.sidecar_ref:
            ref = subprocess.check_output(['git','rev-parse',args.sidecar_ref+'^{commit}'],cwd=repo,text=True).strip()
            archive = root/'release.tar'
            command(['git','archive','--output',archive,ref],cwd=repo)
            source = root/'release';source.mkdir()
            with tarfile.open(archive) as tar:
                tar.extractall(source, filter='data')
            sidecar = root/'sidecar-release'
            command(['go','build','-tags','sidecar','-o',sidecar,'./cmd/sidecar'],cwd=source,env=clean)
            provenance['sidecar_ref'],provenance['sidecar_commit'] = args.sidecar_ref,ref
        provenance['binary_sha256'] = {path.name:hashlib.sha256(path.read_bytes()).hexdigest() for path in (root/'server',sidecar,current_sidecar)}
        for label, folder, port, env in [('desktop',args.desktop,5200,{'VITE_SIDECAR_URL':'http://127.0.0.1:19091'}),('dashboard',args.dashboard,5199,{'VITE_API_BASE_URL':'http://127.0.0.1:18080'})] if args.browser else []:
            folder=folder.resolve()
            provenance[label+'_commit']=subprocess.check_output(['git','rev-parse','HEAD'],cwd=folder,text=True).strip()
            provenance[label+'_source_sha256']=source_digest(folder)
            with open(root/(label+'.log'),'wb') as log:
                process=subprocess.Popen(['pnpm','exec','vite','--host','127.0.0.1','--port',str(port),'--strictPort'],cwd=folder,env={**clean,**env,'VITE_TINTA_MODE':'test'},stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
            processes.append(process)
        (root/'versions.json').write_text(json.dumps(provenance,indent=2))
        suite=[sys.executable,repo/'tests/e2e/concurrency.py','--dsn',app_dsn,'--fixture-dsn',dsn,'--repo',repo,'--server',root/'server','--sidecar',sidecar,'--psql','psql','--runtime',root/'runtime']
        if args.sidecar_ref:suite += ['--upgrade-sidecar',current_sidecar]
        if args.browser:suite += ['--browser']
        if args.keep:suite += ['--keep']
        command(suite,cwd=repo,env=clean)
        state=json.loads((root/'runtime/ui-state.json').read_text())
        key=root/'backup.key';key.write_bytes(os.urandom(32));os.chmod(key,0o600)
        encrypted=root/'gym.tinta-backup'
        common=['--key-file',key,'--file',encrypted]
        command([root/'tenant-backup','--gym',state['gym_id'],*common],env={**clean,'DATABASE_URL':app_dsn})
        command([root/'tenant-backup','--mode','restore','--migrations',repo/'db_migrations/postgres',*common],env={**clean,'DATABASE_URL':restore_dsn})
        command([root/'tenant-backup','--mode','verify',*common],env={**clean,'DATABASE_URL':restore_dsn})
        assert sql(restore_dsn, 'SELECT COUNT(*) FROM gyms') == '1', 'backup included another gym'
        result=subprocess.run([str(root/'tenant-backup'),'--mode','restore',*[str(x) for x in common]],cwd=repo,env={**clean,'DATABASE_URL':restore_dsn},capture_output=True)
        assert result.returncode != 0, 'restore overwrote an existing tenant'
        print('PASS encrypted tenant backup, exact restore verification and overwrite rejection',flush=True)
        (root/'complete.json').write_text(json.dumps({'passed':True,'backup_verified':True}))
    finally:
        for process in processes:
            os.killpg(process.pid, signal.SIGTERM)
            try:process.wait(timeout=10)
            except subprocess.TimeoutExpired:os.killpg(process.pid, signal.SIGKILL);process.wait()
        for database in reversed(databases):
            sql(args.admin_dsn, f'DROP DATABASE "{database}" WITH (FORCE)')
        if role_created:sql(args.admin_dsn, f'DROP ROLE {role}')
        print(f'Artifacts: {root}',flush=True)

if __name__=='__main__':
    run()
