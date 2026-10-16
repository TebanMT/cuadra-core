"""Independent offline sales must preserve both physical movements."""
import uuid
from purchases import PurchaseSuite, base


class ConcurrencySuite(PurchaseSuite):
    def run(self):
        super().run()
        product = self.request(self.edges[0], 'POST', '/api/v1/products', {
            'name': 'Concurrencia ' + uuid.uuid4().hex[:8], 'price': 19.75,
            'initial_stock': 10, 'stock_minimum': 0})['product_id']
        self.wait('shared starting inventory', lambda: all(self.stock(b, product) == 10 for b in [self.cloud, *self.edges]))
        self.wait('empty queues before concurrent sales', self.settled)
        base.Faults.offline = True
        for edge, quantity in zip(self.edges, [2, 3]):
            body = {'payment_method': 'cash', 'line_items': [{'product_id': product, 'quantity': quantity}], 'idempotency_key': str(uuid.uuid4())}
            first = self.request(edge, 'POST', '/api/v1/sales', body)
            assert self.request(edge, 'POST', '/api/v1/sales', body) == first
        self.stop('edge0')
        self.edge(0)
        base.Faults.drop_push = True
        base.Faults.offline = False
        self.wait('both offline sales preserve stock', lambda: all(self.stock(b, product) == 5 for b in [self.cloud, *self.edges]), timeout=65)
        self.wait('concurrent sales settle', self.settled)
        assert self.sql(f"SELECT SUM(delta) FROM stock_movements WHERE product_id='{product}' AND deleted_at IS NULL") == '5'
        assert self.sql(f"SELECT SUM(line_total) FROM sale_items WHERE product_id='{product}' AND deleted_at IS NULL") == '98.75'
        self.check('Dos ventas offline, reinicio y respuesta perdida: 5 unidades y $98.75, sin duplicados')
        from isolation import check_isolation
        check_isolation(self)
        if self.args.upgrade_sidecar:
            before = [self.local(i, 'SELECT COUNT(*),SUM(amount) FROM payments') for i in range(2)]
            self.args.sidecar = self.args.upgrade_sidecar
            for i in range(2):
                self.stop(f'edge{i}')
                self.edge(i)
                self.login(self.edges[i])
            self.wait('upgraded sidecars settle', self.settled)
            assert before == [self.local(i, 'SELECT COUNT(*),SUM(amount) FROM payments') for i in range(2)]
            assert all(self.stock(b, product) == 5 for b in [self.cloud, *self.edges])
            self.check('Upgrade real de ambos sidecars con datos: pagos, importes y stock conservados')
        if not self.args.upgrade_sidecar:
            from held_count import check_counts
            check_counts(self)
        if self.args.browser:
            from browser import run_ui
            run_ui(self)


if __name__ == '__main__':
    base.Suite = ConcurrencySuite
    base.main()
