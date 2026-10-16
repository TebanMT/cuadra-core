# Aislamiento de gimnasios en Tinta

La migración 057 activa RLS en todas las tablas con `gym_id` (47 en este
esquema) y en las tres tablas de tokens de autenticación mediante su usuario.
Sin contexto de gimnasio las lecturas privadas devuelven cero filas y las
escrituras privadas fallan. `releases`, `_migrations` y `whatsapp_opt_outs`
son globales deliberadamente; no contienen registros operativos del gimnasio.

## Conexión de la aplicación

La API exige un rol PostgreSQL independiente: `NOSUPERUSER NOBYPASSRLS
NOCREATEDB NOCREATEROLE`, sin propiedad de tablas ni pertenencia al rol dueño.
También verifica que las tablas por gimnasio tengan RLS y que la migración
057 esté aplicada. Una configuración anterior con el rol dueño **impide que
esta versión inicie**; no hay bypass de desarrollo para RLS.

JWT y credenciales de recepción establecen el gimnasio en el contexto. Cada
Command, Query y snapshot usa una transacción con `set_config(..., true)`
(equivalente a SET LOCAL). Commit/rollback limpia el contexto antes de devolver
la conexión al pool. Los callers cierran Query; los gates liberan su lectura
antes del siguiente handler, incluso con un pool de una sola conexión.
Un contexto anidado no puede cambiar de gimnasio.

Autenticación y vinculación inicial usan funciones SECURITY DEFINER acotadas
que resuelven únicamente el ID a partir del email o hash del token. La
validación de contraseña, vigencia y consumo sigue en la transacción del
gimnasio. Los workers enumeran IDs paginados y procesan un gimnasio por
transacción. Hay funciones específicas para contadores globales de lanzamiento,
revocación de credenciales inactivas y registro de eventos anónimos del proveedor.
No existe una conexión administrativa en la API.

Las políticas usan ENABLE RLS, sin FORCE: el dueño de las migraciones debe
poder ejecutar esas funciones acotadas. Su search_path está fijado y PUBLIC
no tiene acceso al esquema privado. El rol runtime no puede ser ese dueño ni
heredar su rol. Esta es la separación que se comprueba, no una política
probada con un superusuario que la elude. El contexto representa una identidad
ya autenticada por la aplicación; RLS no sustituye esa autenticación.

## Preparación del servidor antes de publicar

**Estos pasos no se ejecutaron en producción.** Requieren preparar la
configuración del servidor una vez, antes del siguiente release de esta API.

1. Conserva el rol dueño actual (`tinta` en el despliegue existente) sólo para
   migraciones. Como administrador de PostgreSQL crea un rol nuevo:

   ```sql
   CREATE ROLE tinta_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
   ```

   Asigna una contraseña distinta mediante `\password tinta_app` en psql.
   No concedas pertenencia a `tinta`, propiedad de tablas ni privilegios de
   creación. Las credenciales no van al repositorio ni al entorno de la API.
2. Instala el servicio `scripts/systemd/tinta-migrate.service` y su usuario
   de sistema `tinta-migrator` con `scripts/deploy/install-system-files.sh`.
   Crea `/etc/tinta/migrate.env`, dueño root y modo 0600, a partir de
   `scripts/deploy/migrate.env.example`. Contiene el DATABASE_URL del dueño y
   `TINTA_APP_DB_ROLE=tinta_app`; systemd lo entrega sólo al migrador.
3. Cambia únicamente el DATABASE_URL de `/opt/tinta/tinta-server.env` para
   usar `tinta_app`, con su contraseña distinta. Mantén las demás variables.
   No reinicies la API con ese rol hasta aplicar la migración y sus permisos.
4. El flujo actualizado compila dos binarios: servidor y `cmd/migrate`.
   El migrador aplica SQL con el dueño y `--app-role tinta_app` concede DML,
   lectura y funciones acotadas al runtime. Revoca CREATE de los esquemas y
   TRUNCATE/REFERENCES/TRIGGER; `_migrations` sólo permite lectura al runtime.
   El servicio de migración se ejecuta **antes** de reiniciar la API. Tanto
   `02-deploy-binary.sh` como `release.yml` comprueban que exista el servicio;
   fallan si la migración no termina correctamente. No ejecutarlos para una
   revisión local: sí publican al servidor configurado.
5. Revisa el inicio y los recorridos de login, vinculación y sync con dos
   gimnasios de prueba. Conserva respaldo previo y plan de recuperación. Las
   restricciones históricas NOT VALID de 055 requieren su validación operativa
   aparte; RLS no corrige referencias históricas inconsistentes.

Para desarrollo local también se requieren dos roles. Ejecuta `cmd/migrate`
con el dueño y `--app-role` antes de iniciar `cmd/server` con el rol limitado.
El runner E2E crea, configura y elimina ambos entornos automáticamente en
127.0.0.1:55439. No cambies la API al dueño para resolver un error de permisos.

## Verificación reproducible

`TestTenantRLSRuntimePoolAndFailClosed` usa PostgreSQL real con una sola
conexión limitada: alterna dos gimnasios y ausencia de contexto, prueba
lecturas sin WHERE, escrituras ajenas, rollback/panic, snapshots, workers y
gate seguido de handler. La suite HTTP usa el mismo tipo de rol para login,
bootstrap de un solo uso, catálogo, sync, tareas in_app de ambos gimnasios y
respaldo por tenant. Ver `tests/e2e/README.md`.

No se certifica aquí una revisión de cada endpoint, archivos externos R2,
infraestructura remota, ni protección frente a un proceso que ya controla las
credenciales y puede suplantar explícitamente el contexto SQL.
