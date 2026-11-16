# Contrato de Mancuerna v1

Autoridad editable desde 18-oct-2026: `build_contract.py` y `build_examples.py`, adoptados del S00 aprobado el 15-oct. OpenAPI, tipos TypeScript y fixtures se generan juntos. S00 conserva la evidencia histórica 1.0.0; no es una segunda autoridad editable.

```sh
pnpm install --frozen-lockfile
pnpm generate
pnpm test
pnpm check
```

El consumidor móvil copia `types.ts`, fija versión y SHA-256 de OpenAPI/tipos, y comprueba igualdad antes de construir. Las fixtures acreditan formatos, no endpoints funcionando. El runner de integración del backend vive en `tests/mancuerna/run.py`.

## Cambios 1.0.1 respecto de S00

- Se añade `POST /me/recovery-key/confirm`, cuerpo `{recovery_key_id, saved: true}`, sesión personal activa e `Idempotency-Key`; respuesta `Ack`. La cuenta se deriva de sesión. Sólo admite su kit vigente no consumido/revocado. Registra `prepared_at` y recibo bajo mutex; no genera un secreto, rota ni concede permisos. Reintentar el mismo ID/hash devuelve el mismo recibo después de validar actor. Otro kit/propietario produce `NOT_FOUND`; un kit propio retirado no vuelve a vigente. Un reintento incompatible produce `IDEMPOTENCY_MISMATCH`.
- `Me.recovery_prepared` sólo es verdadero para el kit vigente con `prepared_at`. Emitir un kit nuevo deja el estado falso hasta confirmación. La UI exige que la persona confirme que lo guardó fuera del teléfono; «Ahora no» conserva el aviso. Un cliente 1.0.0 puede emitir y usar el kit, pero no afirma que está preparado. No cambia la validez criptográfica del kit.
- El DDL adoptado persiste `auth_challenges.proposed_account_id` del `new_account_id` ya obligatorio en 1.0.0. Permite completar/reintentar alta tras reiniciar API sin generar otra identidad. Para `phone_change` se persiste el ID de la cuenta autenticada (nunca un UUID nuevo); para recuperación no se crea una cuenta con este candidato. `recovery_keys.prepared_at` es nullable. La migración es 059, no se vuelve a ejecutar `s00/schema.sql` después de ella.

No se eliminan rutas, campos ni variantes 1.0.0. Reglas de seguridad y sincronización siguen en `docs/mancuerna/etapa-1/s00/SECURITY.md` y `SYNC.md` del workspace. La implementación debe validar actor, contexto, consumo, límites, invariantes y políticas además del schema.

Errata de seguridad en adopción 059: la política RLS del intento exige `tinta.gym_id` vacío tanto en la rama de cuenta como en la de `attempt_id`/hash. La referencia S00 sólo lo garantizaba en la primera. No cambia el protocolo; refuerza su separación documentada. Los constructores de JWT limitan `kid` para que el token emitido cumpla los 512 caracteres del contrato.

El material de `RoutineVersion.content_hash` v1 queda precisado en `docs/mancuerna/etapa-1/s00/SYNC.md`: SHA-256 canónico del DTO completo excepto su propio campo hash, sin prefijo ni normalización adicional. Los vectores y helpers compartidos están en implementación antes de habilitar el escritor. La lectura de carga externa usa escala decimal fija3; la idempotencia conserva el texto del comando original.

## Cambios 1.0.2

`DeviceAuthorization.created_at` expone la hora UTC de creación ya persistida por el dominio, tanto al solicitar como al listar autorizaciones. No requiere migración ni cambia expiración, decisión o identidad. Es opcional en el schema para poder leer snapshots anteriores; el servidor actual siempre lo emite. El cliente conserva la ausencia histórica como desconocida y exige una consulta con hora real antes de ofrecer una decisión nueva. No calcula la creación restando una duración a `expires_at`.

## Cambios 1.0.3

`GET /me/device-authorizations/{id}/decisions/{operation_id}` permite consultar una decisión anterior desde una sesión personal activa de la misma cuenta, incluso si tiene otro SID/dispositivo. Requiere exactamente los parámetros `original_session_id`, `original_device_id` y `decision`; no acepta cuerpo, credencial de intento/resultado, prueba de dispositivo ni `Idempotency-Key`. Son referencias sin autoridad. El servidor reconstruye el hash del POST original a partir de la ruta, operación y decisión; nunca ejecuta ese POST al consultar.

La consulta usa el mutex de cuenta compartido con decisiones, refresh y revocaciones. Primero valida la sesión actual. Si existe un recibo, exige propósito, propietario, operación, hash, autorización y digest del actor original coincidentes. Devuelve `completed` y su fecha real sin leer el ciphertext temporal ni devolver credenciales del candidato. Una referencia incompatible produce error; una cuenta ajena no obtiene el resultado.

Si no existe recibo, verifica que la sesión original pertenece a la cuenta y dispositivo indicados. Sólo devuelve `not_applied` cuando esa sesión está revocada o vencida, por lo que no puede ejecutar posteriormente esa decisión. En otro caso devuelve `unresolved`. Esta conclusión se refiere al efecto del actor original; no concede a una sesión nueva permiso para reenviarlo. Un `404`, una lista vacía o una prueba vencida no equivalen a `not_applied`: una sesión original activa todavía podría renovar su prueba. Todos los resultados incluyen la vinculación de operación/actor/hash y `checked_at`; `completed_at` es nulo fuera de `completed`.

La referencia local se conserva por instalación y cuenta fuera del AuthState que borra el logout. Debe confirmarse su escritura antes del primer envío o de retirar una acción heredada. No contiene tokens, firmas, PIN, claves ni kit; tampoco permite entrar en una cuenta. Una cuenta distinta no la consume. El cliente conserva una respuesta desconocida o inconclusa sin inventar aprobación, denegación ni ausencia. Esta ampliación no requiere DDL y no modifica los requisitos del POST de decisión original.
