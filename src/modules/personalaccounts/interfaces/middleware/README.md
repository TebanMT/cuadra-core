# Límite de autenticación personal

`NewPersonal(tokens, now)` valida exclusivamente el JWT Ed25519 personal y guarda sus claims en el contexto tipado de la petición. `GetClaims(ctx)` permite consumirlos sin depender de controladores. Cada caso de uso debe comprobar cuenta, equipo y sesión activos dentro de su propia transacción; una firma válida no acredita vigencia ni revocación.

La entrada exige una única cabecera `Authorization: Bearer …`, JWT de hasta 512 bytes y ausencia de credenciales en query. Se rechazan cabeceras duplicadas/combinadas, tokens administrativos y de sidecar, firmas alteradas y expiración. Las respuestas usan `no-store`, un identificador aleatorio nuevo y el error contractual `AUTH_REQUIRED`; no registran tokens, query, cuerpo ni errores criptográficos.

El contexto existente se conserva. Los ámbitos previos de intento, gym o cuenta incompatible no se eliminan para permitir acceso: `WithAuthenticatedPersonalAccount` exige ámbito exclusivo de la cuenta antes de guardar claims o invocar el controlador. Esto también protege rutas que necesitan el pool auth, cuya UoW permite intento+cuenta durante finalización. La prueba de integración valida y cierra ambos pools restringidos para comprobar que un ámbito inválido falla antes del controlador o SQL.

`python3 tests/mancuerna/security_boundary_run.py` ejecuta unitarias, race, vet e integración PostgreSQL real por socket Unix desechable, con roles restringidos y migraciones 059/060. También comprueba que el repositorio de recibos no registra SQL ni expone detalles PostgreSQL, preservando la clasificación de duplicados como conflicto de idempotencia. Los datos protegidos de esas pruebas son sintéticos; no se contacta a Verify ni se acredita un recorrido completo de cuenta. Los resultados y hashes se guardan en `/tmp/mancuerna-security-boundary-*/result.json`.

Componente implementado y pendiente de validación; no constituye una historia funcional completa ni pasa a DONE sin revisión del usuario.
