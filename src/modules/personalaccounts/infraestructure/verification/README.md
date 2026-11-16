# Adaptador Verify de Mancuerna

Implementación de transporte, todavía sin rutas, cuenta ni presupuesto persistente. Las pruebas HTTP usan respuestas sintéticas con forma de Verify y servidores locales. No acreditan entrega SMS/WhatsApp, configuración del servicio, costo ni integración real de MC1-TEC-003.

`NewTwilio` exige cuenta, servicio, credenciales, vigencia conocida del servicio y timeout. No lee variables de entorno ni hace conexiones al construir. El destino productivo del adaptador está fijado a HTTPS de Verify; los tests usan una entrada privada para loopback. No sigue redirecciones ni repite llamadas por su cuenta.

El puerto de dominio ofrece `Start`, `Check` y `Fetch`. El caso de uso debe reservar límites y registrar intención en una transacción corta antes de llamar, salir de la transacción durante HTTP y persistir el resultado después. Este adaptador no reemplaza esos pasos. Reenviar o cambiar de WhatsApp a SMS implica una llamada explícita a `Start` bajo el mismo intento y presupuesto; nunca se hace automáticamente por error.

`Check` recibe el SID persistido de la verificación y su teléfono esperado, y envía el código mediante formulario. Las respuestas se comprueban contra cuenta, servicio, SID y destino antes de aceptar `status`. `valid` es legado y se ignora. Un envío no puede aprobar: un `approved` recibido en `Start` se trata como incierto. `Fetch` consulta sólo un SID conocido y no envía códigos. No existe consulta inventada por teléfono cuando un timeout inicial dejó el SID desconocido.

Errores y resultados:

| Resultado | Significado para el caso de uso |
| --- | --- |
| `pending` | Verify mantiene la verificación pendiente; no prueba entrega |
| `approved` de Check/Fetch | Verify reportó aprobación ligada a esa referencia; aún deben cumplirse intento, propósito y autorización personal |
| HTTP 404 | `verification_missing`; no permite inferir aprobación, expiración ni agotamiento |
| HTTP 429, código 60202 | Intentos de comprobación agotados |
| Otros HTTP 429 | Límite temporal; `RetryAfter` es consejo, no autorización para reenviar |
| Timeout/error de transporte/5xx/respuesta inválida tras POST | `verification_uncertain`; persistir incertidumbre, no aprobar ni repetir a ciegas |
| Error de transporte/5xx/respuesta inválida de Fetch | `verification_unavailable`; no cambiar estado de acceso |
| HTTP 401/403 | Configuración o permiso del proveedor inválido |

La vigencia se calcula desde `date_created` del proveedor y la duración del servicio configurada explícitamente; reenviar no añade tiempo. El caso de uso debe además limitar el intento a diez minutos y a su vencimiento ya persistido. No aprobar una respuesta recibida fuera de un intento válido. La duración configurada no se verifica automáticamente contra Twilio: requiere evidencia de la configuración del servicio aislado antes del ensayo real.

Las fallas no contienen mensajes, cuerpos, URL, teléfonos, códigos ni secretos recibidos del proveedor. Sólo exponen clasificación, HTTP status, código numérico y `RetryAfter` acotado. Los cuerpos se limitan a 64 KiB; respuestas con campos críticos inconsistentes, JSON duplicado o contenido adicional inválido fallan cerradas.

## Fuentes consultadas el 18-oct-2026

- [Verification API](https://www.twilio.com/docs/verify/api/verification): rutas, formularios, canales y consulta por SID.
- [Verification Check](https://www.twilio.com/docs/verify/api/verification-check): comprobar por SID; `status` sustituye `valid`; el mismo 404 puede seguir a aprobación, expiración o agotamiento.
- [Rate limits and timeouts](https://www.twilio.com/docs/verify/api/rate-limits-and-timeouts): vigencia predeterminada de diez minutos, personalizable por soporte entre dos minutos y veinticuatro horas; mismo código durante su vigencia.
- [Verify WhatsApp](https://www.twilio.com/docs/verify/whatsapp): WhatsApp y SMS comparten el código; el servicio/sender requieren configuración real.
- [60202](https://www.twilio.com/docs/api/errors/60202) y [60203](https://www.twilio.com/docs/api/errors/60203): límites de comprobación y envío.

Se usa HTTP estándar acotado porque el SDK instalado `twilio-go v1.30.5` no recibe contexto en sus métodos Verify y construye estas peticiones con `context.Background`. No se modifica el proveedor existente de notificaciones de Tinta.
