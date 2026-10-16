#!/usr/bin/env python3
"""Real local cloud + two desktops. Extends the existing financial regression suite."""
import importlib.util
from pathlib import Path
import concurrent.futures
import uuid

import expenses as previous
base=previous.base

class PurchaseSuite(previous.ExpenseSuite):
    def register(self, location, product, quantity=20, received=False, paid=False):
        body={'id':str(uuid.uuid4()),'items':[{'product_id':product,'quantity':quantity,'unit_cost':10}], 'received':received,'paid':paid}
        if paid: body.update(paid_on=self.day,payment_method='transfer',paid_from='gym_fund')
        return body,self.request(location,'POST','/api/v1/inventory-purchase-registrations',body)['items']
    def run(self):
        super().run()
        item=self.request(self.edges[0],'POST','/api/v1/products',{'name':'Agua con conteo pendiente','price':25,'initial_stock':0,'stock_minimum':2})['product_id']
        self.wait('new product sync',lambda: all(any(p['id']==item for p in self.products(edge)) for edge in [self.cloud,*self.edges]))
        self.wait('queues before offline sale',self.settled)
        base.Faults.offline=True
        sale_body={'payment_method':'cash','line_items':[{'product_id':item,'quantity':3}],'idempotency_key':str(uuid.uuid4())}
        sale=self.request(self.edges[0],'POST','/api/v1/sales',sale_body)
        retry=self.request(self.edges[0],'POST','/api/v1/sales',sale_body)
        assert sale==retry,(sale,retry)
        assert self.stock(self.edges[0],item)==-3
        assert self.stock(self.cloud,item)==0
        body,lines=self.register(self.cloud,item,paid=True)
        order=lines[0]['id']
        assert lines[0]['receipt_status']=='pending'
        base.Faults.offline=False
        self.wait('negative stock uploads intact',lambda: all(self.stock(edge,item)==-3 for edge in [self.cloud,*self.edges]))
        self.wait('order reaches desks',lambda: all(self.purchase(edge,order) is not None for edge in self.edges))
        self.check('Venta offline en cero: cobra una vez y sincroniza -3 sin inventar compras')
        self.request(self.edges[0],'POST',f'/api/v1/inventory-purchases/{order}/receive',{'quantity':20})
        self.wait('late receipt offsets sold units',lambda: all(self.stock(edge,item)==17 for edge in [self.cloud,*self.edges]))
        self.request(self.edges[1],'POST',f'/api/v1/inventory-purchases/{order}/receive',{'quantity':20})
        assert self.stock(self.edges[1],item)==17
        replay=self.request(self.cloud,'POST','/api/v1/inventory-purchase-registrations',body)['items'][0]
        assert replay['id']==order and replay['receipt_status']=='received'
        self.check('Compra cloud por 20 recibida después de vender 3: quedan 17 en los tres equipos')
        self.wait('queues before local pending purchase',self.settled)
        base.Faults.offline=True
        body,lines=self.register(self.edges[0],item,quantity=4)
        order=lines[0]['id']
        assert not lines[0]['remote'] and lines[0]['separate_receipt']
        assert self.stock(self.edges[0],item)==17
        self.stop('edge0');self.edge(0)
        replay=self.request(self.edges[0],'POST','/api/v1/inventory-purchase-registrations',body)['items'][0]
        assert replay['id']==order
        dropped=base.Faults.dropped_pushes;base.Faults.drop_push=True;base.Faults.offline=False
        self.wait('local pending purchase syncs after lost reply',lambda: self.settled() and base.Faults.dropped_pushes>dropped)
        self.wait('local purchase visible on cloud',lambda: self.purchase(self.cloud,order) is not None)
        current=self.purchase(self.cloud,order)
        self.request(self.cloud,'POST',f'/api/v1/inventory-purchases/{order}/pay',{'version':current['version'],'paid_on':self.day,'payment_method':'transfer','paid_from':'gym_fund','idempotency_key':str(uuid.uuid4())})
        self.request(self.edges[1],'POST',f'/api/v1/inventory-purchases/{order}/receive',{'quantity':4})
        self.wait('independent payment and receipt converge',lambda: all(self.stock(edge,item)==21 and self.purchase(edge,order)['status']=='paid' and self.purchase(edge,order)['receipt_status']=='received' for edge in [self.cloud,*self.edges]))
        self.check('Compra desktop pendiente: reinicio, respuesta perdida, pago cloud y recepción en otra terminal')
        body={'id':str(uuid.uuid4()),'items':[{'product_id':item,'quantity':2,'unit_cost':10},{'product_id':self.product,'quantity':3,'unit_cost':7.25}],'received':True,'paid':True,'paid_on':self.day,'payment_method':'transfer','paid_from':'gym_fund'}
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            results=list(pool.map(lambda _: self.request(self.cloud,'POST','/api/v1/inventory-purchase-registrations',body),range(8)))
        assert all([v['id'] for v in r['items']]==[v['id'] for v in results[0]['items']] for r in results)
        self.wait('multi-product purchase sync',lambda: all(self.stock(edge,item)==23 for edge in [self.cloud,*self.edges]))
        changed=dict(body,received=False)
        self.request(self.cloud,'POST','/api/v1/inventory-purchase-registrations',changed,expected=(422,))
        self.check('Varios productos y ocho registros simultáneos: una compra por producto, sin duplicar recepción o pago')
        before=len(self.purchases(self.cloud))
        invalid=dict(body,id=str(uuid.uuid4()),items=[body['items'][0],dict(body['items'][1],unit_cost=0)])
        self.request(self.cloud,'POST','/api/v1/inventory-purchase-registrations',invalid,expected=(400,422))
        assert len(self.purchases(self.cloud))==before
        assert self.stock(self.cloud,item)==23
        self.check('Falla en el segundo producto revierte compra, inventario y pago completos')
        # Paid from reception, separate receipt: local financial actions remain possible.
        paid_body={'id':str(uuid.uuid4()),'items':[{'product_id':item,'quantity':1,'unit_cost':10}], 'received':True,'paid':True,'paid_on':self.day,'payment_method':'cash','paid_from':'cash_drawer','cash_drawer_id':self.gym}
        local=self.request(self.edges[0],'POST','/api/v1/inventory-purchase-registrations',paid_body)['items'][0]
        self.wait('cash purchase round trip',lambda: all((self.purchase(edge,local['id']) or {}).get('cash_movement_id')==local['cash_movement_id'] for edge in [self.cloud,*self.edges]))
        self.wait('final queues clean',self.settled)
        for i in range(2):
            assert self.local(i,'PRAGMA foreign_key_check')==[]
            assert self.local(i,'PRAGMA integrity_check')==[('ok',)]
        self.check('Compra pagada en caja y recibida: dinero e inventario consistentes en ambos escritorios y cloud')
if __name__=='__main__':
    base.Suite=PurchaseSuite
    base.main()
