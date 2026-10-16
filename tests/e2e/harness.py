#!/usr/bin/env python3
"""Real cloud + two SQLite sidecars, HTTP faults and persistence assertions.

Requires locally built server/sidecar binaries and an empty, isolated PostgreSQL
database. Only localhost on the dedicated test port is accepted. No application
credentials or .env files are inherited by the child processes.
"""
import argparse
import base64
import concurrent.futures
from datetime import datetime
import http.client
import http.server
import json
import os
from pathlib import Path
import signal
import sqlite3
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from zoneinfo import ZoneInfo


class Faults:
    offline = False
    offline_marker = None
    omit_sale_items = False
    omit_marker = None
    omitted_pushes = 0
    drop_push = False
    dropped_pushes = 0
    page_size = 2
    full_pages = 0
    block_full_after = None
    blocked_full = 0
    lock = threading.Lock()


class Proxy(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        self.forward()

    def do_POST(self):
        self.forward()

    def forward(self):
        path = self.path
        with Faults.lock:
            blocked = Faults.offline or (Faults.offline_marker is not None and Faults.offline_marker.exists())
            if path.startswith('/api/v1/sync/full'):
                Faults.full_pages += 1
                if Faults.block_full_after is not None and Faults.full_pages > Faults.block_full_after:
                    blocked = True
                    Faults.blocked_full += 1
        if blocked:
            self.send_error(503, 'Intentional isolated connectivity failure')
            return
        if path.startswith(('/api/v1/sync/pull', '/api/v1/sync/full')):
            parts = urllib.parse.urlsplit(path)
            q = urllib.parse.parse_qs(parts.query)
            q['limit'] = [str(Faults.page_size)]
            path = parts.path + '?' + urllib.parse.urlencode(q, doseq=True)
        body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
        headers = {k: v for k, v in self.headers.items() if k.lower() not in ('host', 'connection')}
        # Simulate a financially incomplete HTTP batch, without touching SQLite.
        if path.startswith('/api/v1/sync/push') and (Faults.omit_sale_items or (Faults.omit_marker is not None and Faults.omit_marker.exists())):
            request = json.loads(body)
            if any(item['entity_type'] == 'sale_items' for item in request['batch']):
                request['batch'] = [item for item in request['batch'] if item['entity_type'] != 'sale_items']
                body = json.dumps(request).encode()
                headers = {k:v for k,v in headers.items() if k.lower() != 'content-length'}
                headers['Content-Length'] = str(len(body))
                Faults.omitted_pushes += 1
        upstream = http.client.HTTPConnection('127.0.0.1', 18080, timeout=15)
        try:
            upstream.request(self.command, path, body=body, headers=headers)
            response = upstream.getresponse()
            content = response.read()
            with Faults.lock:
                if path.startswith('/api/v1/sync/push') and Faults.drop_push and response.status == 200:
                    Faults.drop_push = False
                    Faults.dropped_pushes += 1
                    self.close_connection = True
                    return  # Cloud committed, sidecar receives no acknowledgment.
            self.send_response(response.status)
            for name, value in response.getheaders():
                if name.lower() not in ('content-length', 'connection', 'transfer-encoding'):
                    self.send_header(name, value)
            self.send_header('Content-Length', str(len(content)))
            self.end_headers()
            self.wfile.write(content)
        except (ConnectionError, TimeoutError, BrokenPipeError):
            self.close_connection = True
        finally:
            upstream.close()


class Suite:
    def __init__(self, args):
        self.args = args
        self.root = Path(args.runtime).resolve()
        self.root.mkdir(parents=True, exist_ok=True)
        self.processes = {}
        self.tokens = {}
        self.checks = []
        self.cloud = 'http://127.0.0.1:18080'
        self.edges = ['http://127.0.0.1:19091', 'http://127.0.0.1:19092']
        self.email = 'final-' + uuid.uuid4().hex[:10] + '@tinta-test.invalid'
        self.password = 'PruebaLocal2026!'
        self.day = datetime.now(ZoneInfo('America/Mexico_City')).date().isoformat()

    def check(self, name, details=None):
        item = {'scenario': name, 'status': 'passed', 'details': details}
        self.checks.append(item)
        (self.root / 'results.json').write_text(json.dumps(self.checks, ensure_ascii=False, indent=2))
        print('PASS ' + name, flush=True)

    def request(self, base, method, path, data=None, expected=(200, 201, 204), token=None, headers=None):
        hdr = {'Content-Type': 'application/json', 'X-Local-Token': 'dev-no-token'}
        jwt = token or self.tokens.get(base)
        # A laptop may sleep during an interactive run; refresh only this
        # harness's own session, never an explicit authorization test token.
        if jwt and token is None and path != '/api/v1/auth/login':
            claims = json.loads(base64.urlsafe_b64decode(jwt.split('.')[1] + '==='))
            if claims['exp'] < time.time() + 30:
                jwt = self.login(base)['access_token']
        if jwt:
            hdr['Authorization'] = 'Bearer ' + jwt
        hdr.update(headers or {})
        req = urllib.request.Request(base + path, data=None if data is None else json.dumps(data).encode(), headers=hdr, method=method)
        try:
            with urllib.request.urlopen(req, timeout=20) as response:
                code, raw = response.status, response.read()
        except urllib.error.HTTPError as error:
            code, raw = error.code, error.read()
        if code not in expected:
            raise AssertionError(f'{method} {path}: HTTP {code}: {raw[:600]!r}')
        if not raw:
            return None
        result = json.loads(raw)
        return result.get('data', result)

    def wait(self, label, fn, timeout=45):
        end = time.monotonic() + timeout
        last = None
        while time.monotonic() < end:
            try:
                value = fn()
                if value:
                    return value
            except Exception as error:
                last = repr(error)
            time.sleep(.25)
        raise AssertionError(f'Timeout: {label}; last error={last}')

    def sql(self, query):
        output = subprocess.check_output([self.args.psql, self.args.fixture_dsn or self.args.dsn, '-XAt', '-v', 'ON_ERROR_STOP=1', '-c', query], text=True)
        return output.strip()

    def local(self, index, query):
        with sqlite3.connect(self.root / f'edge{index}' / 'tinta.db') as db:
            return db.execute(query).fetchall()

    def launch(self, name, binary, env):
        folder = self.root / name
        folder.mkdir(exist_ok=True)
        clean = {k: os.environ[k] for k in ('PATH', 'HOME', 'TMPDIR') if k in os.environ}
        clean.update({'TINTA_MODE': 'test', 'JWT_SECRET': 'isolated-tinta-final-test-secret-2026', 'EMAIL_PROVIDER': 'stdout', 'WHATSAPP_PROVIDER': 'stdout', 'GIN_MODE': 'release'})
        clean.update(env)
        log = open(folder / 'process.log', 'ab')
        process = subprocess.Popen([binary], cwd=folder, env=clean, stdout=log, stderr=subprocess.STDOUT)
        log.close()
        self.processes[name] = process

    def stop(self, name):
        process = self.processes.pop(name, None)
        if process and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=8)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()

    def edge(self, index):
        self.launch(f'edge{index}', self.args.sidecar, {
            'SIDECAR_PORT': str(19091 + index), 'SIDECAR_DB_PATH': str(self.root / f'edge{index}' / 'tinta.db'),
            'UPLOADS_DIR': str(self.root / f'edge{index}' / 'uploads'), 'SIDECAR_NO_PARENT_WATCH': '1',
            'SIDECAR_GMK_INMEMORY': '1', 'LOCAL_AUTH_TOKEN': 'dev-no-token', 'SYNC_INTERVAL_S': '1',
            'TINTA_CLOUD_URL': 'http://127.0.0.1:18081', 'CORS_ALLOWED_ORIGINS': 'http://localhost:5200,http://127.0.0.1:5200',
        })
        self.wait('sidecar health', lambda: self.request(self.edges[index], 'GET', '/health'))

    def login(self, base):
        auth = self.request(base, 'POST', '/api/v1/auth/login', {'email': self.email, 'password': self.password})
        self.tokens[base] = auth['access_token']
        return auth

    def products(self, base):
        return self.request(base, 'GET', '/api/v1/products?page_size=100&status=all')['items']

    def stock(self, base, product):
        return next((p['stock'] for p in self.products(base) if p['id'] == product), None)

    def purchases(self, base):
        return self.request(base, 'GET', '/api/v1/inventory-purchases?status=all&page_size=100')['items']

    def purchase(self, base, identity):
        return next((p for p in self.purchases(base) if p['id'] == identity), None)

    def settled(self):
        return all(self.local(i, 'SELECT COUNT(*) FROM sync_queue WHERE synced_at IS NULL')[0][0] == 0 for i in range(2))

    def run(self):
        self.launch('cloud', self.args.server, {
            'PORT': '18080', 'DATABASE_URL': self.args.dsn, 'MIGRATIONS_DIR': str(Path(self.args.repo) / 'db_migrations/postgres'),
            'PUBLIC_BASE_URL': self.cloud, 'DASHBOARD_BASE_URL': 'http://localhost:5199',
            'CORS_ALLOWED_ORIGINS': 'http://localhost:5199,http://127.0.0.1:5199',
            'UPLOADS_DIR': str(self.root / 'cloud/uploads'),
        })
        self.wait('cloud health', lambda: self.request(self.cloud, 'GET', '/health'))
        account = self.request(self.cloud, 'POST', '/api/v1/auth/signup', {'full_name': 'Dueño de Pruebas', 'email': self.email, 'password': self.password, 'password_confirm': self.password})
        self.gym = account['gym_id']
        # Fixture preparation only; every purchase/receipt uses public HTTP.
        self.sql(f"UPDATE users SET email_verified_at=NOW() WHERE gym_id='{self.gym}'; UPDATE gyms SET name='Gimnasio de Pruebas Finales',setup_completed_at=NOW() WHERE id='{self.gym}'")
        self.login(self.cloud)
        for i in range(2):
            self.edge(i)
            self.login(self.edges[i])
        self.wait('initial downloads', lambda: all(self.local(i, "SELECT COUNT(*) FROM sync_state WHERE key='initial_sync_completed_at_ms' AND value<>''")[0][0] > 0 for i in range(2)))
        self.check('Cloud real, dos escritorios y migraciones completas')

        p = self.request(self.edges[0], 'POST', '/api/v1/products', {'name': 'Agua prueba final', 'price': 25, 'initial_stock': 5, 'stock_minimum': 1})
        self.product = p['product_id']
        self.wait('catalog upload and second desktop', lambda: self.stock(self.cloud, self.product) == 5 and self.stock(self.edges[1], self.product) == 5)
        self.check('Catálogo desktop → cloud → segundo desktop')

        local_body = {'movement_type': 'restock', 'quantity': 6, 'cost': 8.5, 'is_purchase': True, 'purchase_status': 'paid', 'paid_on': self.day, 'payment_method': 'cash', 'paid_from': 'external', 'idempotency_key': str(uuid.uuid4())}
        local_order = self.request(self.edges[0], 'POST', f'/api/v1/products/{self.product}/adjust-stock', local_body)
        retry = self.request(self.edges[0], 'POST', f'/api/v1/products/{self.product}/adjust-stock', local_body)
        assert retry == local_order
        self.wait('local paid purchase sync', lambda: self.purchase(self.cloud, local_order['purchase_id']) is not None and self.stock(self.edges[1], self.product) == 11)
        assert self.purchase(self.cloud, local_order['purchase_id'])['total_amount'] == 51
        self.check('Compra pagada al recibir en desktop, reintento y réplica', {'quantity': 6, 'amount': 51, 'stock': 11})

        unpaid_body = dict(local_body, quantity=4, purchase_status='unpaid', idempotency_key=str(uuid.uuid4()))
        for key in ('paid_on', 'payment_method', 'paid_from'):
            unpaid_body.pop(key)
        unpaid = self.request(self.edges[0], 'POST', f'/api/v1/products/{self.product}/adjust-stock', unpaid_body)
        self.wait('unpaid arrives cloud', lambda: self.purchase(self.cloud, unpaid['purchase_id']) is not None)
        pending = self.purchase(self.cloud, unpaid['purchase_id'])
        self.request(self.cloud, 'POST', f"/api/v1/inventory-purchases/{pending['id']}/pay", {'version': pending['version'], 'paid_on': self.day, 'payment_method': 'transfer', 'paid_from': 'gym_fund', 'idempotency_key': str(uuid.uuid4())})
        self.wait('cloud payment returns desktop', lambda: all((self.purchase(base, pending['id']) or {}).get('status') == 'paid' for base in self.edges))
        self.check('Compra recibida sin pagar en desktop y pagada después desde cloud')

        remote_body = {'id': str(uuid.uuid4()), 'product_id': self.product, 'quantity': 10, 'unit_cost': 9.75, 'paid_on': self.day, 'payment_method': 'transfer', 'paid_from': 'gym_fund'}
        self.remote = remote_body['id']
        self.request(self.cloud, 'POST', '/api/v1/inventory-purchases', remote_body)
        self.request(self.cloud, 'POST', '/api/v1/inventory-purchases', remote_body)
        self.wait('remote order downloaded', lambda: all((self.purchase(base, self.remote) or {}).get('receipt_status') == 'pending' for base in self.edges))
        assert all(self.stock(base, self.product) == 15 for base in [self.cloud, *self.edges])
        self.check('Pago cloud por recibir sin adelantar existencias ni duplicar el pago')

        operator = self.request(self.cloud, 'POST', '/api/v1/users', {'full_name': 'Recepción de Pruebas', 'phone': '+525500001234'})
        self.wait('operator sync', lambda: all(self.local(i, f"SELECT COUNT(*) FROM users WHERE id='{operator['user_id']}'")[0][0] == 1 for i in range(2)))
        self.wait('queue clean before disconnection', self.settled)
        Faults.offline = True
        operator_auth = self.request(self.edges[0], 'POST', '/api/v1/auth/login-pin', {'user_id': operator['user_id'], 'pin': operator['pin']})
        operator_token = operator_auth['access_token']
        deliveries = self.request(self.edges[0], 'GET', '/api/v1/inventory-purchase-deliveries', token=operator_token)
        assert deliveries['total'] == 1
        assert not any(key in deliveries['items'][0] for key in ('unit_cost', 'total_amount', 'paid_from'))
        self.request(self.edges[0], 'GET', '/api/v1/inventory-purchases', expected=(403,), token=operator_token)
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            list(pool.map(lambda _: self.request(self.edges[0], 'POST', f'/api/v1/inventory-purchases/{self.remote}/receive', {'quantity': 10}, token=operator_token), range(8)))
        self.request(self.edges[1], 'POST', f'/api/v1/inventory-purchases/{self.remote}/receive', {'quantity': 10})
        assert all(self.stock(base, self.product) == 25 for base in self.edges)
        assert self.stock(self.cloud, self.product) == 15
        self.check('Recepción offline por operador, ocho solicitudes simultáneas y otra terminal')

        self.request(self.cloud, 'POST', f'/api/v1/inventory-purchases/{self.remote}/correct', {'version': 1, 'quantity': 10, 'unit_cost': 10.25, 'correction_reason': 'Costo confirmado por proveedor', 'idempotency_key': str(uuid.uuid4())})
        self.stop('edge0')
        self.edge(0)
        assert self.stock(self.edges[0], self.product) == 25
        assert self.local(0, "SELECT COUNT(*) FROM sync_queue WHERE entity_type='inventory_purchase_receipts' AND synced_at IS NULL")[0][0] == 1
        self.check('Reinicio offline conserva recepción y cola pendiente; dueño corrige pago en cloud')
        Faults.drop_push = True
        Faults.offline = False
        self.wait('receipt uploaded after dropped acknowledgment', lambda: Faults.dropped_pushes == 1 and self.settled(), timeout=65)
        self.wait('financial correction returns', lambda: all((self.purchase(base, self.remote) or {}).get('total_amount') == 102.5 for base in self.edges))
        assert all(self.stock(base, self.product) == 25 for base in [self.cloud, *self.edges])
        assert self.sql(f"SELECT COUNT(*) FROM inventory_purchase_receipts WHERE gym_id='{self.gym}'") == '1'
        assert self.sql(f"SELECT COUNT(*) FROM stock_movements WHERE gym_id='{self.gym}' AND idempotency_key LIKE 'purchase-receipt:%'") == '1'
        for i in range(2):
            assert self.local(i, 'SELECT COUNT(*) FROM inventory_purchase_receipts')[0][0] == 1
            assert not self.local(i, 'PRAGMA foreign_key_check')
        self.check('Respuesta de sync perdida después del commit, recuperación sin duplicados', {'stock': 25, 'remote_payment': 102.5})

        before = [self.purchase(base, self.remote) for base in [self.cloud, *self.edges]]
        self.request(self.edges[0], 'POST', f'/api/v1/inventory-purchases/{self.remote}/receive', {'quantity': 9}, expected=(422,))
        self.request(self.edges[0], 'POST', f'/api/v1/inventory-purchases/{self.remote}/reopen', {'version': before[1]['version'], 'correction_reason': 'Prueba de rechazo local'}, expected=(422,))
        self.request(self.edges[0], 'POST', f'/api/v1/inventory-purchases/{self.remote}/correct', {'version': before[1]['version'], 'quantity': 10, 'unit_cost': 11, 'correction_reason': 'Prueba de rechazo local', 'idempotency_key': str(uuid.uuid4())}, expected=(422,))
        self.check('Se rechazan cantidad distinta y modificación local del pago remoto')

        # Force a real full download with tiny pages, stop it after one page,
        # then change cloud data and restart the same SQLite database.
        self.stop('edge1')
        with sqlite3.connect(self.root / 'edge1/tinta.db') as db:
            db.execute("UPDATE sync_state SET value='' WHERE key IN ('initial_sync_completed_at_ms','full_sync_cursor','full_sync_started_at_ms','last_pull_cursor')")
        Faults.full_pages = 0
        Faults.block_full_after = 1
        self.edge(1)
        self.wait('full download interrupted after first page', lambda: Faults.blocked_full > 0)
        self.stop('edge1')
        anchor = self.local(1, "SELECT value FROM sync_state WHERE key='full_sync_started_at_ms'")[0][0]
        assert anchor
        extra = dict(remote_body, id=str(uuid.uuid4()), quantity=2, unit_cost=7.25)
        self.request(self.cloud, 'POST', '/api/v1/inventory-purchases', extra)
        Faults.block_full_after = None
        self.edge(1)
        self.wait('full download resumes and catches concurrent purchase', lambda: self.purchase(self.edges[1], extra['id']) is not None)
        assert self.stock(self.edges[1], self.product) == 25
        self.check('Descarga completa paginada, interrupción, reinicio y cambio cloud concurrente')

        self.wait('latest cloud purchase reaches both desktops', lambda: all(self.purchase(base, extra['id']) is not None for base in self.edges))
        self.wait('final sync settles', self.settled)
        paid_total = float(self.sql(f"SELECT SUM(total_amount) FROM inventory_purchases WHERE gym_id='{self.gym}' AND status='paid'"))
        assert paid_total == 202
        for i in range(2):
            assert self.local(i, "SELECT SUM(total_amount) FROM inventory_purchases WHERE status='paid'")[0][0] == 20200
            assert self.local(i, 'PRAGMA integrity_check') == [('ok',)]
        self.check('Importes idénticos en PostgreSQL y ambos SQLite; integridad de bases', {'total_paid': paid_total, 'stock': 25})
        for base in [self.cloud, *self.edges]:
            history = self.request(base, 'GET', f'/api/v1/inventory-purchases?status=all&from={self.day}&to={self.day}')
            assert history['total'] == 4, history
            assert all(item['recorded_on'] == self.day for item in history['items']), history
        self.check('Historial del día local incluye compras de la tarde y fecha consistente')
        report_values = []
        for base in [self.cloud, *self.edges]:
            report = self.request(base, 'GET', f'/api/v1/reports?period=custom&from={self.day}&to={self.day}')
            report_values.append(report['totals']['inventory_purchases'])
            assert report['integrity']['missing_purchase_amount_count'] == 0, report['integrity']
            assert report['integrity']['legacy_purchase_count'] == 0, report['integrity']
        assert report_values[0] == report_values[1] == report_values[2], report_values
        assert report_values[0]['value'] == 202, report_values
        self.check('Reportes HTTP coinciden entre cloud y ambos escritorios', report_values[0])
        state = {'email': self.email, 'password': self.password, 'gym_id': self.gym, 'product_id': self.product, 'remote_id': self.remote, 'pending_id': extra['id'], 'cloud': self.cloud, 'desktop': self.edges[0]}
        (self.root / 'ui-state.json').write_text(json.dumps(state, indent=2))
        print('READY_FOR_UI ' + str(self.root), flush=True)

    def cleanup(self):
        for name in list(self.processes):
            self.stop(name)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--dsn', required=True)
    parser.add_argument('--fixture-dsn', help='isolated fixture and assertion connection; never used by the HTTP server')
    parser.add_argument('--server', required=True)
    parser.add_argument('--sidecar', required=True)
    parser.add_argument('--repo', required=True)
    parser.add_argument('--psql', required=True)
    parser.add_argument('--runtime', required=True)
    parser.add_argument('--keep', action='store_true')
    parser.add_argument('--browser', action='store_true')
    parser.add_argument('--desktop-ui', default='http://127.0.0.1:5200')
    parser.add_argument('--dashboard-ui', default='http://127.0.0.1:5199')
    parser.add_argument('--upgrade-sidecar')
    args = parser.parse_args()
    url = urllib.parse.urlsplit(args.dsn)
    if url.hostname != '127.0.0.1' or url.port != 55439 or not url.path.startswith('/tinta_e2e_'):
        parser.error('Only the isolated localhost:55439/tinta_e2e_* database is allowed')
    suite = Suite(args)
    Faults.offline_marker = suite.root / 'offline'
    Faults.omit_marker = suite.root / 'omit-sale-items'
    proxy = http.server.ThreadingHTTPServer(('127.0.0.1', 18081), Proxy)
    threading.Thread(target=proxy.serve_forever, daemon=True).start()
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    try:
        suite.run()
        if args.keep:
            print('MANUAL_READY ' + str(suite.root / 'ui-state.json'), flush=True)
            while not (suite.root / 'stop').exists():
                time.sleep(.5)
    finally:
        suite.cleanup()
        proxy.shutdown()


if __name__ == '__main__':
    main()
