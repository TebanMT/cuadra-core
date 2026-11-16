# Perfil personal — bloque B02 parcial

La entidad `Account`, su puerto y el repositorio PostgreSQL permiten leer el perfil y aplicar CAS dentro de una transacción personal existente. `GetByID` siempre recibe el propietario explícito; `UpdateProfile` sólo actualiza nombre, unidad, revisión y `updated_at`. No lee ni escribe teléfono, secretos o versión de seguridad. La consulta conserva RLS y el propietario explícito; cuentas ajenas y desconocidas devuelven el mismo error. Una cuenta inactiva o eliminada no acepta modificaciones.

El perfil inicial puede tener nombre vacío, como permite `Profile` de S00. Guardar exige entre 1 y 80 puntos de código Unicode, con contenido distinto de espacios, y `kg` o `lb`. Se recortan espacios exteriores, se preservan acentos/caso y se rechazan UTF-8 inválido/NUL. Fechas internas y modelos se normalizan a UTC. La revisión usa `int64` internamente y cadena decimal en el DTO; no pasa por punto flotante. Cambiar la preferencia no modifica ninguna serie histórica.

El repositorio **no abre ni confirma transacciones**, genera recibos, asigna secuencias ni publica journal. El coordinador central debe autenticar y comprobar cuenta/sesión/equipo activos, tomar el mutex personal, resolver la base del comando y persistir perfil + revisión + recibo + grupo de sync en una sola transacción. Los conflictos devueltos conservan la propuesta en memoria para ese coordinador; aquí no se crea ni resuelve un conflicto persistente.

La proyección GORM no es un esquema para `AutoMigrate` ni crea identidades. La migración 059 sigue siendo la autoridad. El DTO de actualización sólo contiene nombre/unidad; el dispatcher futuro debe validar el JSON completo contra el contrato antes de usarlo. No hay endpoints ni un caso de uso autónomo que pueda confirmar perfil sin journal.

Pruebas unitarias: `go test -race -tags server ./src/modules/personalaccounts/domain/account ./src/modules/personalaccounts/infraestructure/db/... ./src/modules/personalaccounts/interfaces/dto`.

Prueba PostgreSQL real: `python3 tests/mancuerna/profile_run.py`. Crea un cluster desechable por socket Unix, sin escucha TCP, aplica roles y migraciones 059/060, usa logins restringidos distintos para datos y auth y detiene el cluster al terminar. Ejecuta sólo las pruebas de perfil: aislamiento A/B, CAS concurrente, rollback después de una falla posterior y rechazo de estado inactivo/borrado. No lee `DATABASE_URL`, `.env` ni conexiones existentes. El bootstrap mínimo del ledger sirve sólo a este bloque; `tests/mancuerna/run.py` acredita compatibilidad con el esquema Tinta completo. Los artefactos y hashes quedan en `/tmp/mancuerna-profile-*/result.json`.

Esto **no implementa por sí solo MC1-US-005**: faltan integración auth/API/sync, edición móvil online, persistencia local de la lectura y recorridos completos con cuenta real de pruebas. Estado: componente implementado y pendiente de integración/validación, nunca DONE sin revisión del usuario.
