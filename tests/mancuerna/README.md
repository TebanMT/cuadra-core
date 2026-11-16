# Pruebas de implementación Mancuerna

B01/B02 comprueba contrato, migración 059, aislamiento/UoW, primitivas de seguridad, perfil y presupuestos persistentes de OTP. No inicia API ni envía OTP; no acredita todavía historias de cuenta completas.

```sh
# Desde cuadra-core. PostgreSQL nativo + Go/CGO instalados.
python3 tests/mancuerna/run.py
# Si PostgreSQL no está en la ubicación predeterminada de Postgres.app:
MANCUERNA_PG_BIN=/ruta/a/bin python3 tests/mancuerna/run.py

cd contracts/mancuerna/v1
pnpm install --frozen-lockfile
pnpm test
pnpm check
```

El runner siempre crea su propio cluster en un directorio privado temporal, con socket Unix y `listen_addresses=''`, y lo detiene al terminar. No acepta un DSN externo, no lee `.env`, no restaura backups. La URI de tests usa `host=<directorio temporal>` para conservar el socket aun cuando la prueba legacy sustituye el usuario. Las variables `DATABASE_URL` y `MANCUERNA_TEST_*` de los procesos hijos se construyen dentro del runner; no se heredan del entorno.

Se comprueba migración fallida sin roles sin efectos parciales, rechazo de roles inseguros, provisión repetida, migración repetida, igualdad del esquema/datos anteriores de Tinta y separación de credenciales. Las pruebas Go con `server,integration` y `-race` acreditan contexto personal, intentos, mutex, snapshots, cierre de Query, rollback/error/pánico, grants adversariales y rechazo de ámbitos mixtos desde ambos UoW. El runner exige por nombre que las pruebas de integración críticas hayan pasado; una suite compilada sin el tag no cuenta.

La prueba legacy de UoW/JWT es una comprobación de compatibilidad. La implementación criptográfica personal tiene sus propios tests; ninguna de las dos equivale a endpoints de cuenta integrados. Se ejecutan además auth/sync actuales con PostgreSQL sintético y se compilan server/sidecar. `result.json` conserva hashes de fuentes, versiones, duración y ruta de logs; el resumen puede copiarse al registro de entrega sin incluir secretos.

En macOS el sandbox puede impedir `shmget` de PostgreSQL; ejecutar con autorización local al cluster aislado. Una falla de preparación se registra como falla, nunca como prueba aprobada. No reutilizar una base existente para eludirla.

Pendientes: Verify real autorizado, endpoints y recorridos móviles completos, cifrado/lifecycle físico, pruebas de restore/eliminación y matriz de plataformas/versiones. La suite S00 en docs conserva su baseline 001–058 y DDL histórico; no aplicar su schema después de 059.
