# Tinta Access — prueba con Arduino Nano

Este firmware valida el primer actuador local de Tinta. Un pulso en el LED
representa el pulso que más adelante activará un módulo de relevador para una
puerta o torniquete.

## Conexión

El sketch usa `LED_BUILTIN` (`D13` en el Nano), por lo que puede probarse sin
cablear nada adicional. Para un LED externo:

```text
D13 ── resistencia 220–330 Ω ── ánodo LED
GND ─────────────────────────── cátodo LED
```

No conectes una chapa, solenoide o relevador desnudo directamente al pin. Esa
etapa necesitará un módulo aislado o transistor, diodo flyback y alimentación
separada según la carga.

## Cargar el firmware

Abre `tinta-access-led/tinta-access-led.ino` en Arduino IDE, selecciona
**Arduino Nano**, el procesador correcto y el puerto USB. Muchos Nano clon usan
**ATmega328P (Old Bootloader)**.

Con Arduino CLI, reemplaza `<PUERTO>` y usa una de estas variantes:

```bash
arduino-cli compile --fqbn arduino:avr:nano tinta-access-led
arduino-cli upload -p <PUERTO> --fqbn arduino:avr:nano tinta-access-led
```

Para un clon con bootloader anterior, usa
`arduino:avr:nano:cpu=atmega328old` como FQBN.

## Configurar el sidecar

Tinta detecta automáticamente el puerto cuando sólo hay un candidato USB. Si
hay varios, configura explícitamente:

```bash
export TINTA_ACCESS_ARDUINO_PORT=/dev/cu.usbserial-XXXX
```

En Windows el valor normalmente será `COM3`, `COM4`, etc. El baudrate default
es `115200` y puede cambiarse con `TINTA_ACCESS_ARDUINO_BAUD`.

Con el sidecar iniciado y una sesión de dueño, las rutas locales son:

```text
GET  /api/v1/access/device
POST /api/v1/access/device/test-pulse   {"duration_ms":1000}
```

La prueba termina correctamente sólo después de recibir `STARTED` y
`COMPLETED` del Nano. Esto confirma que el firmware cambió el pin; todavía no
confirma eléctricamente que el LED encendió. Una puerta real necesitará un
sensor independiente para confirmar su estado físico.
