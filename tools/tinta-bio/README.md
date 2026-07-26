# tinta-bio — motor biométrico (Windows)

Proceso hijo del sidecar: captura del U.are.U 4500 + FingerJet
(extracción y matching 1:N) vía el DigitalPersona Biometric SDK 3.6.1.
Reemplaza al stack Lite Client + NBIS (ADR-004-quater). Protocolo NDJSON
por stdio: ver [PROTOCOL.md](./PROTOCOL.md).

## Build

Los assemblies del SDK de HID **no se commitean** (EULA §1.1(c): los
componentes runtime sólo se redistribuyen dentro de la app final). El
csproj los busca en `DpSdkLibDir` (default: `./vendor/Lib`).

Local (Windows con el SDK instalado):

```bash
dotnet publish -c Release -r win-x64 -p:DpSdkLibDir="C:\Program Files\DigitalPersona\U.are.U SDK\Windows\Lib" -o out
```

CI: `.github/workflows/build-tinta-bio-windows.yml` — baja el zip de
`Lib/` del mirror R2 privado y publica el exe como release asset en tags
`tinta-bio-v*`. Secrets requeridos:

- `DP_SDK_LIBS_URL` — URL del zip con `Lib/DotNET` + `Lib/x64` (zipear la
  carpeta `Lib` del SDK instalado y subirla al bucket privado).
- `DP_SDK_LIBS_SHA256` — sha256 del zip.

## Deployment en la PC del gym

1. El instalador de Tinta corre el **RTE** del SDK en silencio
   (`RTE/x64/setup.exe` con flags silent, o `InstallOnly.bat`): instala
   drivers del lector + DLLs nativas (dpfpdd/dpfj) a nivel sistema.
2. `tinta-bio.exe` (self-contained, sin dependencia de .NET runtime) va
   en `bundle.resources` de Tauri junto al sidecar, que lo spawnea.
3. Sin RTE instalado el helper arranca pero reporta
   `reader.disconnected` — la app degrada a PIN/manual, igual que hoy.

## Por qué no hay tests aquí

El valor del helper es 95% interop con hardware/SDK reales — mockearlo
sería testear el mock. La validación es: (a) smoke test del workflow
(arranque + shutdown limpio + evento reader sin hardware), (b) el sample
oficial del SDK ya validado en hardware real, (c) e2e manual del flujo
enroll/check-in en la PC del gym antes de cada release que lo toque. La
lógica de protocolo que sí es testeable vive del lado Go (parser de
eventos + orquestación), con el helper fakeado por un script.
