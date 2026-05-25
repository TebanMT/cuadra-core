// Package releases vive en src/application/ (no en src/modules/) porque no
// es un Bounded Context: no tiene agregados, no tiene reglas de dominio que
// hagan invariantes, no hay flujo de comandos del operador del gym contra
// el modelo. Es un read-side del manifest de Tauri updater + una mutación
// administrativa que escribe el pipeline de release (no un operador).
//
// La superficie HTTP son dos endpoints:
//
//   - GET  /api/v1/releases/latest/:target/:current_version
//     Público (no requiere JWT). Lo consulta el binario Tauri al boot y
//     cada ~6h con su target triple + versión actual. Sin auth porque
//     el pubkey ed25519 va embebido en el binario y la firma del payload
//     lo protege; un atacante MITM no puede inyectar un instalador
//     válido sin la llave privada.
//
//   - POST /api/v1/admin/releases
//     Autenticado con un shared secret (env var RELEASES_ADMIN_TOKEN) que
//     el GitHub Actions release.yml inyecta al subir un manifest. NO usa
//     JWT porque el pipeline no es un usuario de un gym.
//
// El staged rollout determinista (ADR-005 §2.4) usa el header opcional
// X-Tinta-Gym-ID: si viene, hash(gym_id) % 100 < rollout_percent decide.
// Sin gym_id sólo se ofrece el release con rollout_percent == 100 (= GA).
package releases
