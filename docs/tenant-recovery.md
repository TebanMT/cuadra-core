# Respaldo y recuperación aislada por gimnasio

Estado: herramienta local implementada; automatización y operación remota
pendientes. No se ha desplegado ni ejecutado contra producción.

`cmd/tenant-backup` exporta las tablas públicas con `gym_id` desde una sola
vista consistente de PostgreSQL (`REPEATABLE READ`). Conserva importes,
fechas, soft deletes, journal y credenciales. Cifra el archivo completo con
AES-256-GCM, nonce aleatorio y autenticación. La llave debe tener 32 bytes y
conservarse aparte del respaldo; no se imprime el contenido de ninguna fila.

Es una herramienta de mantenimiento para un operador autorizado, **no una
API de exportación accesible a dueños**. El permiso SQL de lectura es lo que
autoriza la exportación. No pretende suplir RLS.

```sh
go build -tags server -o /tmp/tinta-tenant-backup ./cmd/tenant-backup
umask 077
openssl rand -out /tmp/tinta-recovery.key 32
DATABASE_URL='postgresql://tinta:isolated_e2e@127.0.0.1:55439/tinta_e2e_ejemplo?sslmode=disable' \
  /tmp/tinta-tenant-backup --gym UUID_DEL_GIMNASIO \
  --file /tmp/tinta-recovery.enc --key-file /tmp/tinta-recovery.key
```

Para restaurar, crea una base **vacía**, local, cuyo nombre comience con
`tinta_restore_`. Usa el rol propietario de las tablas; el rol de aplicación
no tiene permiso para alterar triggers/constraints.

```sh
DATABASE_URL='postgresql://tinta:isolated_e2e@127.0.0.1:55439/tinta_restore_ejemplo?sslmode=disable' \
  /tmp/tinta-tenant-backup --mode restore --migrations db_migrations/postgres \
  --file /tmp/tinta-recovery.enc --key-file /tmp/tinta-recovery.key
DATABASE_URL='postgresql://tinta:isolated_e2e@127.0.0.1:55439/tinta_restore_ejemplo?sslmode=disable' \
  /tmp/tinta-tenant-backup --mode verify \
  --file /tmp/tinta-recovery.enc --key-file /tmp/tinta-recovery.key
```

El restore exige la misma versión de migración, inventario completo de tablas
y filas del mismo gimnasio. Rechaza bases con cualquier dato de tenant, no
sobrescribe. Inserta las columnas no generadas dentro de una transacción,
suspende únicamente triggers de usuario para no recrear movimientos derivados,
valida las FKs de los grafos cíclicos antes de confirmar y restaura sus flags.
`verify` compara todas las filas, no sólo conteos. Un error revierte los datos
y cambios de esquema de esa transacción; las migraciones iniciales son previas.

La suite [E2E](../tests/e2e/README.md) ejecuta este simulacro, compara todas las
filas y prueba que una segunda restauración no sobrescriba lo existente.
Las pruebas unitarias rechazan llave equivocada, archivo truncado y alteración
de ciphertext/nonce/cabecera. No reemplazar la comprobación del restore por
el simple hecho de que la exportación termine sin error.

Limitaciones operativas pendientes:

- No incluye los bytes de fotos/objetos de R2: sólo sus referencias en DB.
- Las credenciales biométricas cifradas requieren conservar las llaves maestras
  externas correspondientes. Este archivo por sí solo no las recupera.
- No implementa PITR, retención remota, rotación/custodia de llaves, alertas,
  recuperación sobre una base compartida en servicio ni rollback de aplicación.
- No borra las SQLite de recepción ni decide cómo reconciliarlas después de
  una recuperación real. Eso necesita un procedimiento operativo probado.
- Separar migrador/propietario del rol de aplicación en infraestructura. El
  E2E lo hace localmente; no demuestra que producción esté configurada así.
- Validar las restricciones `tenant_*` de la migración 055 sobre el histórico
  real antes de declararlo íntegro. `NOT VALID` protege nuevas escrituras, no
  certifica registros anteriores.
- `Caddyfile` ahora deniega `/_internal/*` desde el proxy público; queda validar
  esa configuración con Caddy y desplegarla en una tarea autorizada. El handler
  también acepta únicamente scraping directo desde loopback, sin encabezados
  de proxy. El servidor general aún escucha en `:PORT`; revisar bind/firewall
  como parte de la configuración operativa antes de exponer infraestructura.
