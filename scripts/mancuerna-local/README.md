# Entorno local aislado de cuenta

`local.py` prepara una base nueva y controla sólo los procesos que inicia. Usa
PostgreSQL por socket Unix privado y API en `127.0.0.1:18883`. Nunca carga el
entorno de Tinta ni realiza operaciones de Verify. La comprobación de arranque
es un GET sin credenciales a `/api/mancuerna/v1/me`, que debe responder401 con
`AUTH_REQUIRED` y `no-store` antes de reportar `running`.

## Preparación explícita

La configuración exige rutas absolutas y archivos privados0600 del usuario.
Los directorios nuevos se crean0700. El archivo de credenciales sólo aporta
`TWILIO_ACCOUNT_SID` y `TWILIO_AUTH_TOKEN`: se analizan como valores literales,
sin ejecutar un shell ni usar variables ambientales como fallback. El archivo
Verify dedicado sólo acepta `MANCUERNA_VERIFY_SERVICE_SID`,
`MANCUERNA_TEST_ALLOWED_DESTINATIONS` y `MANCUERNA_TEST_BUDGET_USD`.

Ejecutar `python3 scripts/mancuerna-local/local.py setup --help` muestra los
parámetros. Son obligatorios la vigencia real del servicio y la ventana/cuota
de comprobaciones del kit. No deducir su vigencia de un ejemplo. Este ensayo
está autorizado para dos destinos y USD5; el archivo privado conserva esos
valores, nunca esta documentación. SMS por ahora, WhatsApp pendiente.

Se generan seis propósitos de llaves independientes, usuarios auth/data
restringidos y migraciones059–064. No se modifica una base existente. El ledger
de gasto debe vivir en otro directorio, fuera del runtime. Su inicialización es
única y explícita al final de `setup`; un archivo o lock anterior impide volver
a inicializarlo. El manifiesto `complete.json` sólo aparece cuando todo terminó.
Un setup parcial se rechaza y requiere inspección; no borra ni reinicia crédito.

## Arranque y cierre

`start`, `status` y `stop` requieren `--state-dir` apuntando al runtime preparado.
El supervisor valida hashes/permisos, identidad del cluster y llaves persistidas.
Sólo escucha loopback; otro listener en el puerto hace fallar el arranque.
Una ejecución repetida de `setup` reutiliza exclusivamente la configuración
idéntica. Cambios de parámetros/binarios requieren revisión explícita.

El control usa un socket privado y un identificador aleatorio del runtime.
El supervisor cierra sólo sus hijos directos; nunca mata un PID de un archivo
ni busca procesos ajenos por puerto. `stop` conserva base, configuración y
ledger. No borrar el ledger, reemplazarlo o moverlo para obtener más envíos.
Los errores impresos omiten configuración y salida privada de subprocesos.

## Evidencia y límites

`python3 -m unittest discover -s scripts/mancuerna-local -p test_local.py`
ejecuta11pruebas sin red. `go test -race -tags server ./scripts/mancuerna-local`
comprueba tres casos de llaves, permisos y ledger.

`python3 scripts/mancuerna-local/smoke.py` crea un runtime desechable con
credenciales inventadas, aplica migraciones y arranca/reinicia los procesos
reales. Comprueba401/400/404 sin enviar un auth/start válido; preserva hashes
de configuración/ledger y rechaza cambios de setup. Guarda un resultado sin
secretos en su directorio privado de `/tmp`. El ensayo19-oct pasó y no consumió
ninguna reserva. Esto no acredita un SMS, una cuenta móvil ni despliegue.
