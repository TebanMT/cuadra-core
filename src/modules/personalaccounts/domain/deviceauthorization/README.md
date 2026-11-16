# Autorización desde otro dispositivo

Implementación de servidor para MC1-US-002. Un OTP de acceso verificado de una cuenta existente puede solicitar autorización, pero no crea un dispositivo ni una sesión. La solicitud conserva el candidato exacto del intento y un código de comparación de seis dígitos; ese código no es un factor de autenticación.

`StartDeviceAuthorization` requiere el secreto del intento, cuenta ya resuelta, teléfono todavía vigente y estado verificado. La vigencia es como máximo diez minutos y nunca rebasa la del intento. El recibo de inicio pertenece a cuenta e intento. La auditoría inicial admite actor nulo sólo para `device_authorization_requested`, comprobando dentro de la transacción el intento, el teléfono y el recibo exacto.

`ListDeviceAuthorizations` muestra únicamente solicitudes pendientes vigentes. La paginación conserva microsegundos y orden por fecha e identificador; su cursor firmado está ligado a cuenta, epoch, consulta y filtros. La sesión activa se comprueba dentro de la transacción de cuenta.

`DecideDeviceAuthorization` exige una sesión activa de otro dispositivo y una prueba `authorize_device` ligada al hash de la decisión completa. El mutex de cuenta serializa aprobar, denegar, retirar sesiones y cambiar teléfono. Aprobar crea el dispositivo y sesión exactos, consume prueba e intento y guarda ambos recibos y auditoría en una misma transacción. Denegar cancela el intento y no crea credenciales. Un identificador de dispositivo ocupado nunca sobrescribe una clave ni se adopta, aunque pertenezca a la misma cuenta.

Los recibos separan dos destinatarios:

- El aprobador recibe únicamente un Ack durable ligado a su SID/DID original. Su recibo pertenece sólo a la cuenta y tiene propósito `device_authorization_decision`. `result_secret_hash` guarda un digest de binding de actor, no una credencial ni una referencia de recuperación pública.
- El candidato recibe un resultado cifrado independiente de propósito `device_authorization_result`, perteneciente exactamente a cuenta e intento. El intento consumido referencia esta operación interna. La ventana original de recuperación es de diez minutos; no se reutiliza el recibo del check OTP.

`GetDeviceAuthorizationStatus` sólo recupera ese efecto ya comprometido. Comprueba recibos, candidato y clave, creación del dispositivo/sesión y sesión candidata aún activa. No consume pruebas, consulta al proveedor ni crea otra sesión. La salida se invalida al vencer la ventana o retirar la sesión candidata. Cerrar después la sesión del aprobador no deshace una aprobación ya comprometida. Los estados públicos de autorización se proyectan sobre el intento sin agregar estados a la tabla de challenges.

La implementación conserva las migraciones existentes. Las invariantes de tiempo/estado se validan en dominio y repositorio: pendiente sin decisión, aprobación con actor distinto del candidato y decisión anterior al vencimiento, denegación sin aprobador, decisión nunca anterior a creación. El esquema conserva la relación única entre solicitud e intento y las claves foráneas de cuenta. Un refuerzo futuro del DDL debe mantener compatibles las cancelaciones históricas de solicitudes aprobadas aún no consumidas.

Las pruebas de PostgreSQL ejercitan roles restringidos reales, concurrencia, aislamiento, rollback, ventanas, claves ocupadas, cambio de teléfono, cancelación y auditoría. El recorrido HTTP firma el payload público de prueba sin usar el serializador criptográfico del servidor. La entrega/check de OTP usa un proveedor sintético: estas pruebas no acreditan Verify real, custodia de clave en un equipo físico ni integración móvil. Este bloque de servidor permanece implementado y pendiente de validación del usuario; no declara completa la historia en todas sus plataformas.
