"""Two real tenant accounts through the production HTTP router."""
import json
import time
import uuid


def check_isolation(s):
    email = 'isolation-' + uuid.uuid4().hex + '@tinta-test.invalid'
    account = s.request(s.cloud, 'POST', '/api/v1/auth/signup', {'full_name':'Otro Dueño', 'email':email, 'password':s.password, 'password_confirm':s.password})
    other = account['gym_id']
    s.sql(f"UPDATE users SET email_verified_at=NOW() WHERE gym_id='{other}'; UPDATE gyms SET setup_completed_at=NOW(),subscription_plan='standard_monthly',all_features_access=false WHERE id='{other}'")
    auth = s.request(s.cloud, 'POST', '/api/v1/auth/login', {'email':email, 'password':s.password})
    token = auth['access_token']
    catalog = s.request(s.cloud, 'GET', '/api/v1/products?gym_id='+s.gym, token=token)
    assert catalog['items'] == [], catalog
    s.request(s.cloud, 'PATCH', '/api/v1/products/'+s.product, {'name':'Intrusión', 'price':1, 'stock_minimum':0}, token=token, expected=(422,))
    s.request(s.cloud, 'POST', '/api/v1/products/'+s.product+'/adjust-stock', {'movement_type':'restock', 'quantity':2, 'idempotency_key':str(uuid.uuid4())}, token=token, expected=(422,))
    foreign_payment = s.sql(f"SELECT id FROM payments WHERE gym_id='{s.gym}' LIMIT 1")
    s.request(s.cloud, 'GET', '/api/v1/payments/'+foreign_payment+'/receipt.pdf', token=token, expected=(422,))
    foreign_expense = s.sql(f"SELECT id FROM expenses WHERE gym_id='{s.gym}' LIMIT 1")
    s.request(s.cloud, 'GET', '/api/v1/expenses/'+foreign_expense, token=token, expected=(422,))
    s.request(s.cloud, 'GET', '/api/v1/analytics', token=token, expected=(402,))
    s.sql(f"UPDATE gyms SET all_features_access=true WHERE id='{other}'")
    s.request(s.cloud, 'GET', '/api/v1/analytics', token=token)
    s.check('Dos gimnasios: listados, IDs de productos/pagos/gastos y gates Standard/beneficio sin bypass')
    # Forged outer IDs AND payload IDs must not authorize cross-tenant writes.
    timestamp = int(time.time()*1000)
    item = {'queue_id':str(uuid.uuid4()), 'entity_type':'products', 'entity_id':s.product, 'operation':'upsert', 'client_version':999,
            'payload':{'id':s.product,'gym_id':other,'name':'Intrusión sync','price':1,'stock':0,'stock_minimum':0,'active':True,'version':999,'created_at':timestamp,'updated_at':timestamp}}
    result = s.request(s.cloud, 'POST', '/api/v1/sync/push', {'client_id':str(uuid.uuid4()),'schema_version':6,'batch':[item]},token=token)
    assert result['results'][0]['status'] == 'rejected_unauthorized', result
    data = s.request(s.cloud, 'GET', '/api/v1/sync/full?limit=1000', token=token)
    assert s.product not in json.dumps(data), data
    assert s.sql(f"SELECT gym_id FROM products WHERE id='{s.product}'") == s.gym
    assert s.sql(f"SELECT COUNT(*) FROM sync_entities WHERE gym_id='{other}' AND entity_id='{s.product}'") == '0'
    s.check('Dos gimnasios: push con UUID ajeno rechazado y descarga sin datos del vecino')

    # Public bootstrap consumes a narrowly resolved token, without a global
    # user query or a privileged application connection.
    bootstrap = s.request(s.cloud, 'POST', '/api/v1/auth/installer-token', {}, token=token)
    headers = {'X-Cuadra-Client-ID': str(uuid.uuid4())}
    redeemed = s.request(s.cloud, 'POST', '/api/v1/auth/redeem-installer', {'token': bootstrap['token']}, headers=headers)
    assert redeemed['gym_id'] == other
    s.request(s.cloud, 'POST', '/api/v1/auth/redeem-installer', {'token': bootstrap['token']}, headers=headers, expected=(410,))
    s.check('RLS: vinculación inicial resuelve un gimnasio y el código solo se consume una vez')
    notifications = []
    for gym in (s.gym, other):
        identity = str(uuid.uuid4())
        owner = s.sql(f"SELECT id FROM users WHERE gym_id='{gym}' AND role='owner' LIMIT 1")
        s.sql(f"INSERT INTO notification_queue(id,gym_id,channel,template_key,recipient_type,recipient_id,recipient_address,payload) VALUES('{identity}','{gym}','in_app','rls_worker','user','{owner}','local-test','{{}}')")
        notifications.append(identity)
    identities = ','.join("'"+identity+"'" for identity in notifications)
    s.wait('scoped background dispatch for both gyms', lambda: s.sql(f"SELECT COUNT(*) FROM notification_queue WHERE id IN ({identities}) AND status='sent'") == '2')
    s.check('RLS: tarea de fondo procesa ambos gimnasios por separado sin mensajes externos')
