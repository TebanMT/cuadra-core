# tinta-bio — protocolo sidecar ↔ helper

NDJSON (una línea = un JSON) por stdin/stdout. stderr = logging humano.
El sidecar spawnea `tinta-bio.exe`, escribe comandos a stdin y lee eventos
de stdout. **stdin EOF = shutdown limpio del helper.**

Los FMD (templates FingerJet, ANSI 378 por dentro) viajan como strings
**opacos**: base64 del XML de `Fmd.SerializeXml`. El sidecar nunca los
parsea — los cifra con GMK, los guarda, y los devuelve tal cual.

## Eventos espontáneos (helper → sidecar)

```jsonl
{"event":"reader","state":"connected","name":"U.are.U 4500","serial":"..."}
{"event":"reader","state":"disconnected","code":"no_device"}
{"event":"sample","fmd":"<b64>","quality":"DP_QUALITY_GOOD","score":0}
{"event":"sample_rejected","code":"DP_QUALITY_TIMED_OUT","quality":"..."}
{"event":"error","code":"capture_handler","detail":"..."}
```

- `sample` = dedazo con extracción exitosa. El sidecar decide qué hacer
  (check-in → identify; enroll en curso → acumular pre-enroll).
- `sample_rejected` = hubo dedazo pero calidad/extracción falló. Sirve
  para feedback "vuelve a poner el dedo" sin registrar nada.

## Comandos (sidecar → helper) y sus respuestas

Toda respuesta es `{"event":"result","id":"<eco del comando>",...}`.

### ping
```json
{"cmd":"ping","id":"1"}
→ {"event":"result","id":"1","ok":true,"state":"connected","galleryEpoch":"e42","gallerySize":151}
```
Health-check + verificación de qué galería tiene cargada.

### gallery — reemplazo total del cache 1:N
```json
{"cmd":"gallery","id":"2","epoch":"e43","candidates":[{"ref":"<fingerprint_id>","fmd":"<b64>"},...]}
→ {"event":"result","id":"2","ok":true,"gallerySize":152}
```
El sidecar la manda al boot y cada vez que cambia (enroll / baja de
huella / cambio de gym activo). `ref` = id del template en SQLite
(member_fingerprints.id) — el sidecar resuelve ref→socio.

### identify — 1:N contra la galería cacheada
```json
{"cmd":"identify","id":"3","probe":"<b64>","farDivisor":100000,"max":1}
→ {"event":"result","id":"3","ok":true,"matches":["<ref>"],"galleryEpoch":"e43"}
```
- `matches` vacío = no match (socio no reconocido).
- `farDivisor`: FAR objetivo = 1/farDivisor (default 100k, igual que el
  sample oficial del SDK).
- El sidecar DEBE validar `galleryEpoch` contra su epoch actual; si
  difieren, re-mandar gallery y reintentar (carrera enroll vs identify).

### enroll — N capturas del mismo dedo → FMD de enrollment
```json
{"cmd":"enroll","id":"4","fmds":["<b64>","<b64>","<b64>"]}
→ {"event":"result","id":"4","ok":true,"fmd":"<b64 enrollment>"}
→ {"event":"result","id":"4","ok":false,"code":"DP_ENROLLMENT_INVALID_SET"}
```
Los `fmds` son los `sample.fmd` que el sidecar acumuló durante el flujo
de registro (mismas 3 capturas del flujo actual). En fallo, el FE pide
otro dedazo.

### compare — 1:1 (diagnóstico)
```json
{"cmd":"compare","id":"5","probe":"<b64>","fmds":["<b64>"]}
→ {"event":"result","id":"5","ok":true,"score":214748}
```
`score` = disimilitud FingerJet (menor = más parecido; umbral típico
`PROBABILITY_ONE/100000`). Para colisiones de enroll usar `identify`.

## Ciclo de vida

- El helper enumera/abre el lector solo, con backoff 1s→8s; el sidecar
  no gestiona hardware, solo escucha `reader.state`.
- No hay watchdog de padre: si el sidecar muere, el pipe de stdin se
  cierra → EOF → el helper sale solo. Sin huérfanos.
- `TINTA_BIO_READER=<serial>` (env) fija un lector específico.
- `tinta-bio --list` imprime lectores y sale (diagnóstico manual).
