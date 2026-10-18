"""Real React → HTTP → SQLite/PostgreSQL paths; no mocked application API."""
import re
import uuid
from pathlib import Path
from playwright.sync_api import sync_playwright, expect
import harness as base


def run_ui(suite):
    with sync_playwright() as playwright:
        browser = playwright.chromium.launch()
        # Regression: cash uses the gym's business day on a UTC computer too.
        context = browser.new_context(viewport={'width': 1440, 'height': 1000}, timezone_id='UTC')
        context.tracing.start(screenshots=True, snapshots=True)
        page = context.new_page()
        try:
            page.goto(suite.args.desktop_ui + '/auth/login-email')
            page.locator('#email').fill(suite.email)
            page.locator('#password').fill(suite.password)
            page.get_by_role('button', name='Entrar', exact=True).click()
            page.wait_for_url(suite.args.desktop_ui + '/')
            expect(page.get_by_text('MODO DEV', exact=False)).to_have_count(0)
            suite.check('Navegador: login real en recepción, modo test')

            # Open a physical cash session using the actual form.
            page.goto(suite.args.desktop_ui + '/reports/cash-close')
            expect(page.get_by_role('button', name='Agregar efectivo', exact=True)).to_be_visible()
            opening = page.get_by_role('button', name=re.compile(r'^Abrir caja'))
            if opening.count():
                opening.click()
                dialog = page.get_by_role('dialog')
                dialog.locator('#cash-opening').fill('500')
                with page.expect_response(lambda r: r.url.endswith('/api/v1/cash-sessions/open') and r.request.method == 'POST') as saved:
                    dialog.get_by_role('button', name='Confirmar efectivo inicial').click()
                assert saved.value.ok, f'Abrir caja: HTTP {saved.value.status}: {saved.value.text()[:600]}'
                expect(dialog).to_have_count(0)
                suite.check('Navegador: apertura de caja con fecha del gimnasio desde un equipo UTC')

            # The remaining forms capture local dates. Match the fixture gym
            # explicitly instead of depending on the runner's timezone.
            gym_context = browser.new_context(
                viewport={'width': 1440, 'height': 1000},
                timezone_id=suite.timezone, storage_state=context.storage_state())
            context.tracing.stop(path=str(suite.root / 'browser-cash-trace.zip'))
            context.close()
            context = gym_context
            context.tracing.start(screenshots=True, snapshots=True)
            page = context.new_page()

            # Disconnect only the cloud transport. React and SQLite remain real.
            suite.wait('queues before browser offline sale', suite.settled)
            before = suite.stock(suite.cloud, suite.product)
            base.Faults.offline = True
            page.goto(suite.args.desktop_ui + '/sales')
            page.get_by_role('button', name='Agregar Agua prueba final', exact=True).click()
            page.get_by_role('button', name='Cobrar $25.00', exact=True).click()
            dialog = page.get_by_role('dialog')
            dialog.get_by_role('button', name='Exacto', exact=True).click()
            with page.expect_response(lambda r: r.url.endswith('/api/v1/sales') and r.request.method == 'POST') as saved:
                dialog.get_by_role('button', name='Cobrar $25.00', exact=True).click()
            assert saved.value.ok, saved.value.status
            assert suite.stock(suite.edges[0], suite.product) == before - 1
            assert suite.stock(suite.cloud, suite.product) == before
            assert not suite.settled()
            page.screenshot(path=str(suite.root / 'browser-offline-sale.png'))
            base.Faults.omit_sale_items = True
            base.Faults.offline = False
            suite.wait('incomplete sale is visible for recovery', lambda: suite.request(suite.edges[0], 'GET', '/api/v1/sync/status')['queue_stuck_count'] > 0)
            assert suite.stock(suite.cloud, suite.product) == before
            page.get_by_role('button', name='Estado de sincronización', exact=True).click()
            dialog = page.get_by_role('dialog')
            retry = dialog.get_by_role('button', name='Reintentar operación completa', exact=True)
            expect(retry).to_be_visible()
            expect(dialog.get_by_text('La operación sigue guardada en esta recepción.', exact=False)).to_be_visible()
            page.screenshot(path=str(suite.root / 'browser-incomplete-sale.png'))
            base.Faults.omit_sale_items = False
            retry.click()
            page.keyboard.press('Escape')
            suite.wait('browser sale reaches cloud and other desk', lambda: all(suite.stock(b, suite.product) == before - 1 for b in [suite.cloud, *suite.edges]))
            suite.wait('browser sale queue settles', suite.settled)
            suite.check('Navegador: venta offline, lote incompleto rechazado entero y recuperación desde UI sin duplicados')

            # Fixtures establish a new unpaid member; the payment itself is UI.
            plan = suite.request(suite.edges[0], 'POST', '/api/v1/membership-types', {'name':'Mensual navegador', 'price':321.5, 'duration_days':30})
            member = suite.request(suite.edges[0], 'POST', '/api/v1/members', {'full_name':'Socio Navegador', 'no_phone':True, 'membership_type_id':plan['id'], 'charge_first_payment':False})
            page.goto(suite.args.desktop_ui + '/members/' + member['member_id'] + '?action=pay')
            dialog = page.get_by_role('dialog')
            expect(dialog).to_be_visible()
            with page.expect_response(lambda r: '/api/v1/payments' in r.url and r.request.method == 'POST') as saved:
                dialog.get_by_role('button', name='Cobrar $321.50', exact=True).click()
            assert saved.value.ok, saved.value.status
            suite.wait('membership payment reaches cloud', lambda: suite.sql(f"SELECT COUNT(*) FROM payments WHERE gym_id='{suite.gym}' AND member_id='{member['member_id']}' AND amount=321.50") == '1')
            suite.check('Navegador: cobro de primera membresía, pago persistido en cloud')

            page.goto(suite.args.desktop_ui + '/products/purchases/new')
            page.get_by_role('button', name='Agua prueba final', exact=True).click()
            page.get_by_label('Cantidad de Agua prueba final', exact=True).fill('2')
            page.get_by_label('Costo de Agua prueba final', exact=True).fill('8.25')
            page.get_by_label('Ya recibí los productos', exact=True).check()
            page.get_by_label('Pagado', exact=True).check()
            page.get_by_label('Pagado desde', exact=True).select_option('external')
            with page.expect_response(lambda r: '/api/v1/inventory-purchase-registrations' in r.url and r.request.method == 'POST') as saved:
                page.get_by_role('button', name='Guardar compra', exact=True).click()
            assert saved.value.ok, saved.value.status
            suite.wait('browser purchase stock', lambda: all(suite.stock(b, suite.product) == before + 1 for b in [suite.cloud, *suite.edges]))
            suite.wait('browser purchase queues', suite.settled)
            suite.check('Navegador: compra recibida y pagada, stock y pago sincronizados')

            page.goto(suite.args.desktop_ui + '/reports/cash-close')
            page.get_by_role('button', name='Hacer corte', exact=True).click()
            report = suite.request(suite.edges[0], 'GET', '/api/v1/cash-close?date=' + suite.day)
            dialog = page.get_by_role('dialog')
            dialog.locator('#cash-counted').fill(str(report['session']['expected_cash']))
            dialog.get_by_label('Retirar efectivo al terminar').uncheck()
            with page.expect_response(lambda r: '/api/v1/cash-close' in r.url and r.request.method == 'POST') as saved:
                dialog.get_by_role('button', name='Guardar corte', exact=True).click()
            assert saved.value.ok, saved.value.status
            expect(dialog).to_have_count(0)
            suite.wait('cash close queues', suite.settled)
            suite.check('Navegador: corte de caja sin diferencia y sincronizado')
            page.screenshot(path=str(suite.root / 'browser-cash.png'))

            # A real permanent dependency rejection: two offline catalogs use
            # the same name with distinct UUIDs. Resolve it through the UI.
            suite.wait('queues before permanent dependency failure', suite.settled)
            base.Faults.offline = True
            name = 'Duplicado ' + uuid.uuid4().hex[:8]
            local_product = suite.request(suite.edges[0], 'POST', '/api/v1/products', {'name':name,'category':'Bebidas','price':12.50,'initial_stock':4,'stock_minimum':0})['product_id']
            sale = suite.request(suite.edges[0], 'POST', '/api/v1/sales', {'payment_method':'transfer','line_items':[{'product_id':local_product,'quantity':1}],'idempotency_key':str(uuid.uuid4())})
            remote_product = suite.request(suite.cloud, 'POST', '/api/v1/products', {'name':name,'category':'Bebidas','price':12.50,'initial_stock':0,'stock_minimum':0})['product_id']
            base.Faults.offline = False
            suite.wait('permanent duplicate is actionable', lambda: any(item['entity_id']==local_product and item['kind']=='duplicate' for item in suite.request(suite.edges[0],'GET','/api/v1/sync/status').get('queue_stuck_items',[])), timeout=65)
            assert suite.sql(f"SELECT COUNT(*) FROM sales WHERE id='{sale['sale_id']}'") == '0'
            page.get_by_role('button',name='Estado de sincronización',exact=True).click()
            page.get_by_role('dialog').get_by_role('button',name='Abrir para renombrar',exact=True).click()
            edit = page.get_by_role('dialog')
            edit.locator('#p-name').fill(name + ' recepción')
            with page.expect_response(lambda r: '/api/v1/products/' in r.url and r.request.method == 'PATCH') as renamed:
                edit.get_by_role('button',name=re.compile(r'^Guardar')).click()
            assert renamed.value.ok, renamed.value.status
            expect(edit).to_have_count(0)
            suite.wait('corrected catalog and complete sale reach cloud', lambda: suite.sql(f"SELECT COUNT(*) FROM sales WHERE id='{sale['sale_id']}'") == '1', timeout=65)
            suite.wait('repaired financial queue',suite.settled,timeout=65)
            # The explicit retry also reloads any catalog row quarantined while
            # the duplicate existed. No direct SQLite edits or discarded money.
            page.get_by_role('button',name='Estado de sincronización',exact=True).click()
            dialog=page.get_by_role('dialog')
            if suite.request(suite.edges[0],'GET','/api/v1/sync/status').get('quarantined_count',0):
                dialog.get_by_role('button',name='Sincronizar ahora',exact=True).click()
            page.keyboard.press('Escape')
            suite.wait('both catalog identities recovered',lambda: suite.local(0,f"SELECT COUNT(*) FROM products WHERE id IN ('{local_product}','{remote_product}')")[0][0]==2,timeout=65)
            suite.wait('quarantine recovered',lambda: suite.request(suite.edges[0],'GET','/api/v1/sync/status').get('quarantined_count',0)==0,timeout=65)
            assert suite.stock(suite.cloud,local_product)==3
            suite.check('Navegador: rechazo permanente de catálogo, renombrar y recuperar venta completa sin editar SQLite')

            cloud = context.new_page()
            cloud.goto(suite.args.dashboard_ui + '/auth/login')
            cloud.locator('#email').fill(suite.email)
            cloud.locator('#password').fill(suite.password)
            cloud.get_by_role('button', name='Entrar', exact=True).click()
            cloud.wait_for_url(suite.args.dashboard_ui + '/')
            cloud.goto(suite.args.dashboard_ui + '/products')
            expect(cloud.get_by_text('Agua prueba final', exact=True)).to_be_visible()
            expect(cloud.get_by_text('MODO DEV', exact=False)).to_have_count(0)
            cloud.screenshot(path=str(suite.root / 'browser-cloud.png'))
            suite.check('Navegador: login del dueño y consulta del catálogo cloud')
            from held_count import run_feature_ui
            run_feature_ui(suite, page, cloud, context)
        except Exception:
            page.screenshot(path=str(suite.root / 'browser-failure.png'))
            (suite.root / 'browser-failure.txt').write_text(page.locator('body').inner_text())
            raise
        finally:
            base.Faults.offline = False
            base.Faults.omit_sale_items = False
            context.tracing.stop(path=str(suite.root / 'browser-trace.zip'))
            browser.close()
