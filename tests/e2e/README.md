# Regresión real de Tinta

El ejecutor levanta cloud, un proxy de fallos y dos sidecars con SQLite real.
Usa PostgreSQL real, un rol de aplicación `NOSUPERUSER NOBYPASSRLS` y bases
PostgreSQL únicas que elimina al terminar, incluso si falla una aserción.
Las SQLite sintéticas quedan en la carpeta privada de artefactos para diagnóstico.
No lee `.env`
para los procesos Go; email y WhatsApp usan stdout y no hay credenciales de
proveedores. Los frontends reciben URLs locales y `VITE_TINTA_MODE=test`.

## Preparación

Requisitos: Go del `go.mod`, CGO, PostgreSQL/psql, Python 3.12 o posterior.
Para navegador: Node 22, pnpm 9, dependencias instaladas en desktop/dashboard.
Puede usarse una instancia local existente dedicada en `127.0.0.1:55439`, o:

```sh
docker run --name tinta-e2e-pg -e POSTGRES_USER=tinta -e POSTGRES_PASSWORD=isolated_e2e -e POSTGRES_DB=postgres -p 127.0.0.1:55439:5432 -d postgres:16
docker exec tinta-e2e-pg pg_isready -U tinta -d postgres
python3 -m venv /tmp/tinta-e2e-venv
/tmp/tinta-e2e-venv/bin/pip install -r tests/e2e/requirements.txt
/tmp/tinta-e2e-venv/bin/python -m playwright install chromium
```

En Linux, Playwright puede requerir `install --with-deps chromium`. Espera a
que PostgreSQL acepte conexiones. Los puertos 18080, 18081, 19091, 19092 deben
estar libres; navegador añade 5199 y 5200. El ejecutor falla ante un puerto
ocupado y no termina procesos ajenos.

Desde `cuadra-core`:

```sh
/tmp/tinta-e2e-venv/bin/python tests/e2e/run.py \
  --admin-dsn 'postgresql://tinta:isolated_e2e@127.0.0.1:55439/postgres?sslmode=disable' \
  --browser --desktop ../cuadra-desktop --dashboard ../cuadra-dashboard
```

Sin `--browser` sólo requiere Python estándar. El DSN administrador se usa
para crear/migrar/borrar las bases sintéticas y el rol temporal. **El servidor
HTTP sí se ejecuta con ese rol sin superusuario ni bypass.** Esto verifica
RLS activo: el runtime no es dueño de tablas ni miembro del rol dueño.
Las mutaciones de preparación se ejecutan con una conexión de fixtures
separada, nunca dentro de la API. Ver `docs/tenant-isolation.md`.

Para una versión publicada en Git, añade `--sidecar-ref v1.2.2` o `v1.2.1`.
Se archiva ese commit sin modificarlo, se compila su sidecar y se ejecutan los
recorridos; después ambos procesos se sustituyen por el binario actual sobre
las mismas bases SQLite. Se comparan cantidad y suma de pagos y existencias.
El cloud siempre es el candidato actual: se conserva API compatible primero,
desktop después. No se simula ni publica un release.

## Qué se comprueba

- Login, bootstrap paginado, dos dispositivos, propagación en ambas direcciones.
- Compras pagadas/pendientes, recepción independiente, concurrencia de ocho
  solicitudes, rollback de una compra multiproducto inválida.
- Reinicio offline, cola persistida, respuesta perdida después del commit,
  reenvío idempotente y descarga inicial interrumpida.
- Gastos concurrentes y sus salidas de caja, versiones y correcciones siguientes.
- Dos ventas offline desde stock 10: cantidades 2 y 3, resultado 5 y $98.75.
- Dos gimnasios: catálogos, cambios con UUID ajeno, recibos PDF, gastos,
  push manipulado y full sync. Standard sin beneficio recibe 402 en analytics;
  con beneficio recibe 200, bajo `TINTA_MODE=test`.
- Bootstrap público de un solo uso y worker in_app de dos gimnasios, sin
  proveedores externos.
- Conteo físico con cero, diferencias positivas/negativas, venta durante la
  captura, rollback completo, ocho reintentos, reinicio offline, respuesta de
  sync perdida y movimientos de dos recepciones conservados al reconectar.
- Recuperación de retenidos: paginación, permisos de dueño, Standard/beneficio
  de lanzamiento, credenciales antiguas bloqueadas y envío a stdout.
- En Chromium: login, venta offline, lote financiero incompleto, recuperación
  desde el diálogo, rechazo de catálogo duplicado y renombrado; membresía, compra, corte de caja
  y consulta cloud. Los datos de preparación se crean por API; las operaciones
  verificadas se hacen en formularios reales. Para el conteo se descarta una
  respuesta después del commit real: no se sustituye ningún dato de API.
  Se comprueban el borrador persistente, la revisión por una venta simultánea,
  la confirmación offline y la recuperación del mismo intento. En Mensajes se
  envía/descarta individualmente y se verifica paginación desde ambas interfaces.
- Respaldo cifrado por gimnasio, restauración vacía aislada, comparación de
  cada fila, exclusión del otro gimnasio y rechazo de sobrescritura.

Las pruebas Go complementan esto con devoluciones, abonos, correcciones,
grafos financieros, escritura/cola atómicas y restricciones de base. No todos
esos casos pasan todavía por un navegador.

## Evidencia y CI

`--artifacts <directorio vacío>` conserva `versions.json`, `complete.json`,
`runtime/results.json`, capturas, trazas y logs. Registra commits, hash del
árbol local (incluye archivos nuevos), binarios, PostgreSQL y rol SQL. Las
trazas y SQLite contienen datos y tokens **sintéticos**. La carpeta es privada
(0700); no se debe reutilizar para una base real ni subir completa a CI.

`.github/workflows/e2e.yml` ejecuta las tres combinaciones en PR/push. La fila
actual incluye navegador y exige `TINTA_E2E_READ_TOKEN`, con acceso **sólo de
lectura** a `TebanMT/cuadra-desktop` y `TebanMT/cuadra-dashboard`. Su ausencia
falla explícitamente: no produce una supuesta cobertura verde de navegador.
En ejecución manual se pueden fijar refs de las interfaces o excluir
explícitamente el navegador. CI publica sólo resúmenes y capturas sintéticas,
no llaves, trazas, bases ni archivos de sesión. El workflow no despliega.

Pendiente fuera del entorno local: configurar ese secreto, ejecutar el
workflow en GitHub/Linux/PostgreSQL 16 y exigirlo en las reglas de integración.
Las verificaciones locales de esta entrega usaron macOS/PostgreSQL 18.3.

## Recorrido manual sin editar bases

1. Ejecuta el comando de navegador con `--keep --artifacts /tmp/tinta-aceptacion`
   usando una carpeta vacía. Espera `MANUAL_READY`.
2. Abre `http://127.0.0.1:5200/auth/login-email` y
   `http://127.0.0.1:5199/auth/login`. Las credenciales sintéticas están en
   `/tmp/tinta-aceptacion/runtime/ui-state.json`.
3. Anota las unidades de **Agua prueba final** en recepción y cloud. Crea el
   archivo `/tmp/tinta-aceptacion/runtime/offline` (puede ser vacío): corta
   sólo el transporte hacia cloud de ambas terminales.
4. En Venta rápida vende una unidad, cobra el importe exacto en efectivo y
   revisa que recepción reduzca stock y muestre cambios pendientes. Cloud
   debe conservar el valor anterior.
5. Elimina **sólo el archivo `offline`**. Espera que la cola se vacíe y refresca
   cloud: debe mostrar una unidad menos, un solo pago y una sola venta.
6. En Productos registra dos unidades a $8.25, recibidas y pagadas desde
   dinero personal. Deben sumarse dos unidades y una compra de $16.50.
7. En Socios cobra una membresía; en Caja registra el conteo igual al efectivo
   esperado, sin retiro. Revisa el corte sin diferencia y el recibo del cobro.
8. Crea `/tmp/tinta-aceptacion/runtime/stop`. El ejecutor cierra sus procesos,
   hace respaldo/restauración/verificación y elimina sus bases y rol. Conserva
   la evidencia local. Al finalizar puede detenerse el contenedor dedicado.

La prueba de navegador usa Chromium contra React y el sidecar. No sustituye
pruebas del instalador Tauri/NSIS, actualizador firmado, Windows ni hardware.

Los pasos de aceptación de los puntos 7 y 10, capturas y límites están en
`audits/2026-10-14-mensajes-conteo/RESULTADO.md` desde el directorio padre
del repositorio. El archivo automático de mensajes de 90 días está preparado
pero desactivado hasta confirmar esa política; no se borra físicamente nada.

## Rechazo financiero y recuperación manual

En el entorno `--keep`, crea `runtime/offline`, registra una venta y crea
`runtime/omit-sale-items` antes de quitar `offline`. Ese archivo hace que el
proxy omita partidas del envío, sin modificar las bases. En recepción aparece
la operación pendiente; cloud no debe registrar su pago ni descontar stock.
Abre **Estado de sincronización**, quita `omit-sale-items` y pulsa
**Reintentar operación completa**. Deben llegar pago, venta, partidas y
movimientos una sola vez. Las capturas automáticas documentan el mismo recorrido.

Para un rechazo de catálogo, desconecta, crea un producto y véndelo en recepción;
crea en cloud otro producto con el mismo nombre antes de reconectar. El diálogo
permite **Abrir para renombrar**. Cambia el nombre, guarda y reintenta: la venta
se conserva y se envía completa. **Sincronizar ahora** también vuelve a descargar
registros que se habían apartado por el conflicto, sin editar SQLite.

La recepción actual incluye las filas relacionadas aunque el límite normal
parta un lote. Cloud valida el conjunto en una sola transacción. Los tags
anteriores conservan compatibilidad cuando envían operaciones completas; si
parten una operación sin enviar sus dependencias, cloud la rechaza entera y la
cola se conserva. Se requiere actualizar esa recepción y reintentar; no se
reconoce un pago parcial para vaciar su cola. El límite HTTP absoluto sigue
siendo 8 MiB / 2000 filas: una operación que lo exceda requiere diagnóstico,
no se fragmenta comprometiendo la integridad del dinero.
