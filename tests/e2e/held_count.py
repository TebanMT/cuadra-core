"""Retained-message recovery and offline physical counts, using real databases."""
import base64
import concurrent.futures
import hashlib
import hmac
import json
import uuid
import harness as base


def operator_token(token):
    head, body, _ = token.split('.')
    claims = json.loads(base64.urlsafe_b64decode(body + '===')); claims['role'] = 'operator'
    body = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=')
    signature = base64.urlsafe_b64encode(hmac.new(b'isolated-tinta-final-test-secret-2026', (head+'.'+body).encode(), hashlib.sha256).digest()).decode().rstrip('=')
    return head+'.'+body+'.'+signature


def check_counts(s):
    ids = [s.request(s.edges[0], 'POST', '/api/v1/products', {'name': name, 'price': 10, 'initial_stock': stock})['product_id'] for name, stock in [('Conteo API Agua', 10), ('Conteo API Cero', 5)]]
    s.wait('count products synced', lambda: all(s.stock(b, ids[0]) == 10 for b in [s.cloud, *s.edges]))
    s.wait('count initial queues', s.settled)
    base.Faults.offline = True
    def line(identity, quantity):
        p = next(p for p in s.products(s.edges[0]) if p['id'] == identity)
        return {'product_id': identity, 'revision': p['count_revision'], 'quantity': quantity}
    body = {'id': str(uuid.uuid4()), 'reason': 'Conteo de prueba', 'purchases_checked': True, 'lines': [line(ids[0], 8), line(ids[1], 0)]}
    sale = s.request(s.edges[0], 'POST', '/api/v1/sales', {'payment_method':'transfer', 'line_items':[{'product_id':ids[0],'quantity':2}], 'idempotency_key':str(uuid.uuid4())})
    conflict = s.request(s.edges[0], 'POST', '/api/v1/product-counts', body, expected=(409,))
    assert len(conflict['items']) == 1 and conflict['items'][0]['stock'] == 8
    assert s.stock(s.edges[0], ids[1]) == 5
    body['lines'][0] = line(ids[0], 6)  # Count again AFTER the sale: missing two.
    # Another reception has an independent sale while disconnected.
    s.request(s.edges[1], 'POST', '/api/v1/sales', {'payment_method':'transfer', 'line_items':[{'product_id':ids[0],'quantity':1}], 'idempotency_key':str(uuid.uuid4())})
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        replies = list(pool.map(lambda _: s.request(s.edges[0], 'POST', '/api/v1/product-counts', body), range(8)))
    assert all(r == replies[0] for r in replies)
    assert s.stock(s.edges[0],ids[0]) == 6 and s.stock(s.edges[0],ids[1]) == 0
    assert s.stock(s.cloud,ids[0]) == 10
    s.stop('edge0'); s.edge(0)
    assert s.request(s.edges[0],'POST','/api/v1/product-counts',body) == replies[0]
    base.Faults.drop_push = True; base.Faults.offline = False
    s.wait('counts and both sales converge',lambda: all(s.stock(b,ids[0])==5 and s.stock(b,ids[1])==0 for b in [s.cloud,*s.edges]),timeout=65)
    s.wait('count queues settle',s.settled)
    assert s.sql(f"SELECT COUNT(*) FROM stock_movements WHERE product_id IN ('{ids[0]}','{ids[1]}') AND movement_type='count_correction'") == '2'
    assert s.sql(f"SELECT COUNT(*) FROM sales WHERE id='{sale['sale_id']}'") == '1'
    assert s.sql(f"SELECT SUM(line_total) FROM sale_items WHERE product_id='{ids[0]}'") == '30.00'
    assert s.sql(f"SELECT COUNT(*) FROM inventory_purchases WHERE product_id IN ('{ids[0]}','{ids[1]}')") == '0'
    # Replaying on cloud after a response was lost uses the synced journal too.
    assert s.request(s.cloud,'POST','/api/v1/product-counts',body) == replies[0]
    # Same guarantees for a cloud-originated positive count.
    row=next(p for p in s.products(s.cloud) if p['id']==ids[1])
    cloud_body={'id':str(uuid.uuid4()),'reason':'Revisión cloud','purchases_checked':True,'lines':[{'product_id':ids[1],'revision':row['count_revision'],'quantity':4}]}
    first=s.request(s.cloud,'POST','/api/v1/product-counts',cloud_body)
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        assert all(r==first for r in pool.map(lambda _:s.request(s.cloud,'POST','/api/v1/product-counts',cloud_body),range(8)))
    s.wait('cloud count to offline receipts',lambda: all(s.stock(b,ids[1])==4 for b in s.edges))
    s.check('Conteo: cero, venta durante captura, rollback, ocho reintentos, reinicio offline y dos ventas conservadas tras sync',{'final_stock':5,'sales_total':30,'count_movements':2})


def seed_held(s):
    owner=s.sql(f"SELECT id FROM users WHERE gym_id='{s.gym}' AND role='owner' LIMIT 1")
    s.sql(f"UPDATE users SET phone='+525500009877' WHERE id='{owner}'; UPDATE gyms SET subscription_plan='standard_monthly',all_features_access=false WHERE id='{s.gym}'")
    s.request(s.cloud,'GET','/api/v1/held-notifications',expected=(402,))
    s.sql(f"UPDATE gyms SET all_features_access=true WHERE id='{s.gym}'; UPDATE sync_entities SET version=version+1,server_updated_at=clock_timestamp() WHERE gym_id='{s.gym}' AND entity_type='gyms'")
    s.request(s.cloud,'GET','/api/v1/held-notifications')
    s.request(s.cloud,'GET','/api/v1/held-notifications',token=operator_token(s.tokens[s.cloud]),expected=(403,))
    s.request(s.edges[0],'GET','/api/v1/held-notifications',token=operator_token(s.tokens[s.edges[0]]),expected=(403,))
    ids=[]
    for i in range(23):
        identity=str(uuid.uuid4()); ids.append(identity)
        key='broadcast_freeform' if i < 2 else 'operator_welcome_pin'
        payload=json.dumps({'member_first_name':'Prueba','gym_name':'Gym de prueba','message':'Aviso local de prueba'})
        s.sql(f"INSERT INTO notification_queue(id,gym_id,channel,template_key,recipient_type,recipient_id,recipient_address,payload,status,error_message,created_at) VALUES('{identity}','{s.gym}','whatsapp','{key}','user','{owner}','+525500009877','{payload}','held','Llegó tarde. Espera tu decisión.',NOW()-INTERVAL '30 days'+INTERVAL '{i} seconds')")
    first=s.request(s.cloud,'GET','/api/v1/held-notifications'); second=s.request(s.cloud,'GET','/api/v1/held-notifications?page=2')
    assert len(first['items'])==20 and len(second['items'])==3
    blocked=next(x for x in first['items'] if x['blocked'])
    s.request(s.cloud,'POST','/api/v1/held-notifications/'+blocked['id'],{'version':blocked['version'],'action':'send'},expected=(422,))
    s.check('Mensajes: paginación, credenciales bloqueadas, dueño/recepción y beneficio de lanzamiento con gates reales')
    return ids


def run_feature_ui(s, page, cloud, context):
    from playwright.sync_api import expect
    ids=seed_held(s)
    # Refresh authentication/profile so the launch grant appears in both UIs.
    cloud.reload(); page.reload()
    count_id=s.request(s.edges[0],'POST','/api/v1/products',{'name':'Conteo Navegador','price':10,'initial_stock':10})['product_id']
    s.wait('browser count catalog',lambda:s.stock(s.cloud,count_id)==10)
    s.wait('browser count queues',s.settled);base.Faults.offline=True
    page.goto(s.args.desktop_ui+'/products')
    page.get_by_role('button',name='Contar productos',exact=True).click()
    dialog=page.get_by_role('dialog')
    dialog.get_by_label('Contadas: Conteo Navegador',exact=True).fill('8')
    dialog.get_by_label('Ya registré las compras recibidas.',exact=False).check()
    dialog.get_by_role('button',name='Continuar después',exact=True).click()
    page.reload(); page.get_by_role('button',name='Contar productos',exact=True).click()
    expect(dialog.get_by_label('Contadas: Conteo Navegador',exact=True)).to_have_value('8')
    sale_page=context.new_page();sale_page.goto(s.args.desktop_ui+'/sales')
    sale_page.get_by_role('button',name='Agregar Conteo Navegador',exact=True).click()
    sale_page.get_by_role('button',name='Cobrar $10.00',exact=True).click()
    sale_dialog=sale_page.get_by_role('dialog');sale_dialog.get_by_role('button',name='Transferencia',exact=True).click()
    sale_dialog.get_by_role('button',name='Cobrar $10.00',exact=True).click();expect(sale_dialog).to_have_count(0)
    assert s.stock(s.edges[0],count_id)==9
    dialog.get_by_role('button',name='Revisar diferencias',exact=True).click()
    dialog.get_by_role('button',name='Confirmar conteo',exact=True).click()
    expect(dialog.get_by_text('Hay productos que cambiaron.',exact=False)).to_be_visible()
    dialog.get_by_label('Buscar en el conteo',exact=True).fill('Conteo Navegador')
    page.screenshot(path=str(s.root/'count-sale-conflict.png'))
    dialog.get_by_label('Contadas: Conteo Navegador',exact=True).fill('7')
    dialog.get_by_role('button',name='Ya volví a contar',exact=True).click()
    dialog.get_by_label('Ya registré las compras recibidas.',exact=False).check()
    dialog.get_by_role('button',name='Revisar diferencias',exact=True).click()
    page.screenshot(path=str(s.root/'count-review.png'))
    # Drop responses AFTER the real server commits, without replacing API data.
    def lost_response(route):
        route.fetch();route.abort('failed')
    page.route('**/api/v1/product-counts',lost_response)
    dialog.get_by_role('button',name='Confirmar conteo',exact=True).click()
    expect(dialog.get_by_text('Aún no confirmamos el resultado.',exact=False)).to_be_visible(timeout=15000)
    assert s.stock(s.edges[0],count_id)==7
    page.screenshot(path=str(s.root/'count-lost-response.png'))
    page.unroute('**/api/v1/product-counts',lost_response)
    page.reload();page.get_by_role('button',name='Contar productos',exact=True).click()
    dialog.get_by_role('button',name='Comprobar guardado',exact=True).click()
    expect(dialog.get_by_text('Conteo confirmado en esta computadora.',exact=True)).to_be_visible()
    page.screenshot(path=str(s.root/'count-offline-confirmed.png'))
    dialog.get_by_role('button',name='Listo',exact=True).click()
    base.Faults.offline=False
    s.wait('browser count sync',lambda:all(s.stock(b,count_id)==7 for b in [s.cloud,*s.edges]),timeout=65)
    assert s.sql(f"SELECT COUNT(*) FROM stock_movements WHERE product_id='{count_id}' AND movement_type='count_correction'")=='1'
    s.check('Navegador: borrador persistente, recepción vende durante conteo, revisión obligatoria, respuesta perdida y confirmación offline sin duplicados')
    cloud.goto(s.args.dashboard_ui+'/settings/messages')
    cloud.get_by_role('button',name='Mensajes retenidos',exact=False).click()
    expect(cloud.get_by_text('Acceso del personal',exact=False).first).to_be_visible()
    cloud.screenshot(path=str(s.root/'held-cloud-list.png'))
    cloud.get_by_role('button',name='Siguiente',exact=True).click()
    # Second page includes the two actionable messages and one blocked code.
    expect(cloud.locator('article')).to_have_count(3)
    enabled=cloud.locator('article button:not([disabled])').filter(has_text='Enviar').first
    expect(enabled).to_be_enabled()
    enabled.click()
    cloud.get_by_role('button',name='Enviar mensaje',exact=True).click()
    expect(cloud.get_by_text('Mensaje en cola.',exact=False)).to_be_visible()
    s.wait('recovered message sent once by stdout test provider',lambda:s.sql(f"SELECT COUNT(*) FROM notification_queue WHERE id IN ('{ids[0]}','{ids[1]}') AND status='sent'")=='1')
    cloud.get_by_role('button',name='Descartar',exact=True).first.click()
    cloud.get_by_role('button',name='Descartar mensaje',exact=True).click()
    expect(cloud.get_by_text('Mensaje descartado.',exact=True)).to_be_visible()
    cloud.get_by_role('button',name='Descartar',exact=True).first.click()
    cloud.get_by_role('button',name='Descartar mensaje',exact=True).click()
    expect(cloud.locator('article')).to_have_count(20)
    expect(cloud.get_by_role('button',name='Siguiente',exact=True)).to_have_count(0)
    cloud.screenshot(path=str(s.root/'held-decisions.png'))
    page.goto(s.args.desktop_ui+'/settings/mensajes')
    page.get_by_role('button',name='Mensajes retenidos',exact=False).click()
    expect(page.get_by_text('Acceso del personal',exact=False).first).to_be_visible()
    page.screenshot(path=str(s.root/'held-desktop-cloud.png'))
    s.check('Navegador: mensajes en Ajustes/Mensajes, consulta cloud desde desktop, enviar y descartar explícitos, sin mensajes reales')
    if s.args.keep:
        # Leave one fresh test decision for manual acceptance. It still uses
        # the isolated stdout provider; no production contact is involved.
        manual=str(uuid.uuid4())
        s.sql(f"INSERT INTO notification_queue(id,gym_id,channel,template_key,recipient_type,recipient_id,recipient_address,payload,status,error_message,created_at) SELECT '{manual}',gym_id,channel,template_key,recipient_type,recipient_id,recipient_address,payload,'held','Prueba manual: revisa antes de enviar.',NOW()-INTERVAL '31 days' FROM notification_queue WHERE id='{ids[0]}'")
        rows=s.request(s.cloud,'GET','/api/v1/held-notifications?page=2')['items']
        assert any(x['id']==manual and not x['blocked'] for x in rows)
        s.check('Aceptación manual: un aviso permitido retenido, exclusivamente con proveedor stdout')
