# Roles de Mancuerna

La migración 059 es sólo aditiva y exige tres grupos NOLOGIN. `mancuerna_roles.sql` se ejecuta explícitamente por un administrador del cluster **antes** de aplicar migraciones, con conexión de mantenimiento del entorno autorizado. No crea credenciales ni concede permisos a Tinta. Valida grupos existentes; no los reconfigura para ocultar permisos inseguros.

Crear por separado logins distintos para `mancuerna_data` y `mancuerna_auth`, sin propiedad/CREATE/BYPASSRLS ni membresías cruzadas. Ningún runtime recibe `mancuerna_identity`; es dueño exclusivo de funciones estrechas. El constructor personal comprueba privilegios efectivos, RLS y separación. El migrador nunca es un pool runtime.

Ejemplo de secuencia para un **entorno aislado** cuya conexión de mantenimiento ya fue configurada mediante `PG*`:

```sh
psql -X -v ON_ERROR_STOP=1 -f scripts/db/mancuerna_roles.sql
# Ejecutar después cmd/migrate con DATABASE_URL explícito de ese mismo entorno.
```

No ejecutar esto contra producción dentro del objetivo de desarrollo. El ensayo reproducible `python3 tests/mancuerna/run.py` prepara sus roles, credenciales y datos sintéticos sin aceptar conexiones externas. CI y el runner E2E existente provisionan los grupos en sus clusters de prueba antes de la migración. La habilitación de rutas/configuración personal sigue pendiente de integrar; 059 no las expone.

Reversión funcional: conservar esquema/datos y deshabilitar las rutas personales cuando existan. No eliminar tablas ni roles para revertir una app. Los exportadores por gimnasio siguen sin incluir cuentas personales, por lo que no sirven como respaldo completo de Mancuerna.
