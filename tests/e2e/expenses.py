#!/usr/bin/env python3
"""Extends the isolated purchase suite with real expense conflict round trips."""
import importlib.util
from pathlib import Path
import sqlite3
import time
import uuid

import harness as base

class ExpenseSuite(base.Suite):
    def expense(self, location, identity):
        return self.request(location, 'GET', f'/api/v1/expenses/{identity}')

    def change_expense(self, location, identity, amount, cash=False):
        current = self.expense(location, identity)
        body = {'version': current['version'], 'paid_on': self.day, 'amount': amount,
                'category': 'renta', 'payment_method': 'cash' if cash else 'transfer',
                'paid_from': 'cash_drawer' if cash else 'gym_fund', 'classification': 'fixed',
                'correction_reason': 'Corrección de prueba aislada'}
        if cash:
            body['cash_drawer_id'] = self.gym
        return self.request(location, 'PATCH', f'/api/v1/expenses/{identity}', body)

    def shared_expense(self):
        created = self.request(self.cloud, 'POST', '/api/v1/expenses', {
            'paid_on': self.day, 'amount': 100, 'category': 'renta', 'payment_method': 'transfer',
            'paid_from': 'gym_fund', 'classification': 'fixed', 'idempotency_key': str(uuid.uuid4())})
        identity = created['expense_id']
        self.wait('expense reaches both desks', lambda: all(self.expense(edge, identity)['amount'] == 100 for edge in self.edges))
        self.wait('expense queues settle', self.settled)
        return identity

    def run(self):
        super().run()
        # Test-fixture entitlement only, in these three disposable databases.
        self.sql(f"UPDATE gyms SET subscription_plan='plus_monthly' WHERE id='{self.gym}'")
        for index in range(2):
            with sqlite3.connect(self.root / f'edge{index}' / 'tinta.db') as db:
                db.execute('UPDATE gyms SET subscription_plan=? WHERE id=?', ('plus_monthly', self.gym))
        identity = self.shared_expense()
        base.Faults.offline = True
        self.change_expense(self.edges[0], identity, 100, cash=True)
        self.change_expense(self.cloud, identity, 125)
        dropped = base.Faults.dropped_pushes
        base.Faults.drop_push = True
        base.Faults.offline = False
        self.wait('canonical expense after lost response', lambda: all(self.expense(edge, identity)['amount'] == 125 and self.expense(edge, identity)['paid_from'] == 'gym_fund' for edge in self.edges))
        self.wait('expense conflict queues settle', self.settled)
        assert base.Faults.dropped_pushes > dropped
        assert self.sql(f"SELECT COUNT(*) FROM cash_movements WHERE gym_id='{self.gym}' AND expense_id='{identity}' AND deleted_at IS NULL") == '0'
        for index in range(2):
            assert self.local(index, f"SELECT COUNT(*) FROM cash_movements WHERE expense_id='{identity}' AND deleted_at IS NULL")[0][0] == 0
        self.check('Gasto cloud gana corrección offline: caja consistente tras perder la respuesta y reintentar')

        identity = self.shared_expense()
        base.Faults.offline = True
        self.change_expense(self.edges[0], identity, 175)
        self.stop('edge0')
        self.change_expense(self.edges[1], identity, 150, cash=True)
        base.Faults.offline = False
        self.wait('second desk cash expense reaches cloud', lambda: self.expense(self.cloud, identity)['paid_from'] == 'cash_drawer')
        self.edge(0)
        self.login(self.edges[0])
        self.wait('restarted desk gets canonical cash dependency', lambda: self.expense(self.edges[0], identity)['amount'] == 150 and self.expense(self.edges[0], identity)['paid_from'] == 'cash_drawer')
        self.wait('canonical dependency queues settle', self.settled)
        for index in range(2):
            assert self.local(index, f"SELECT amount FROM cash_movements WHERE expense_id='{identity}' AND deleted_at IS NULL") == [(15000,)]
            assert self.local(index, 'PRAGMA foreign_key_check') == []
        self.check('Reinicio offline recibe gasto ganador y salida de caja desconocida sin romper referencias')

        identity = self.shared_expense()
        base.Faults.offline = True
        self.change_expense(self.cloud, identity, 110)
        time.sleep(.02)  # Distinct millisecond timestamps for deterministic LWW.
        self.change_expense(self.edges[0], identity, 120)
        base.Faults.offline = False
        self.wait('newer offline correction wins', lambda: self.expense(self.cloud, identity)['amount'] == 120 and all(self.expense(edge, identity)['amount'] == 120 for edge in self.edges))
        self.wait('client-win queues settle', self.settled)
        versions = [self.expense(location, identity)['version'] for location in [self.cloud, *self.edges]]
        assert len(set(versions)) == 1, versions
        self.change_expense(self.cloud, identity, 130)
        self.wait('next cloud correction reaches originating desk', lambda: all(self.expense(edge, identity)['amount'] == 130 for edge in self.edges))
        self.wait('subsequent cloud correction settles', self.settled)
        self.check('Corrección offline ganadora conserva versiones coherentes y recibe la siguiente edición cloud')

if __name__ == '__main__':
    base.Suite = ExpenseSuite
    base.main()
