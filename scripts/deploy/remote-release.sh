#!/usr/bin/env bash
# remote-release.sh — paso final de un deploy. Corre EN EL SERVIDOR como el
# usuario `tinta`; lo suben y lo llaman release.yml y 02-deploy-binary.sh.
#
# Antes de llamarlo, el caller ya subió (sin activar nada):
#   /opt/tinta/bin/tinta-server.new    API nueva
#   /opt/tinta/bin/tinta-migrate.new   migrador nuevo
#   /opt/tinta/migrations/             SQL del mismo commit
#
# Orden. La API vieja sigue atendiendo hasta el paso 4:
#   1. Respaldo previo, si tinta-backup.timer está activo.
#   2. Migraciones con el rol dueño (tinta-migrate.service). Si fallan, se
#      descarta la API nueva: la anterior sigue en disco y en memoria.
#   3. Copia la API actual a tinta-server.prev.
#   4. Cambia a la API nueva y reinicia.
#   5. Espera a que /health reporte la versión nueva. Si no llega, regresa a
#      tinta-server.prev y reinicia. Las migraciones ya aplicadas se quedan
#      (son aditivas; la API anterior convivió con ellas desde el paso 2).
#      La API que no arrancó queda en tinta-server.failed para revisarla.
#
# Uso:
#   bash remote-release.sh <versión-esperada>
#   SKIP_MIGRATE=1   solo cambia el binario (sin respaldo ni migraciones).

set -euo pipefail

EXPECTED="${1:?Uso: remote-release.sh <versión-esperada>}"
BIN_DIR="${BIN_DIR:-/opt/tinta/bin}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8080/health}"
SYSTEMCTL="${SYSTEMCTL:-/bin/systemctl}"
JOURNALCTL="${JOURNALCTL:-/bin/journalctl}"
# ~12 s: menos de lo que tarda systemd en agotar StartLimitBurst=5 con
# RestartSec=5s, para que el rollback todavía pueda reiniciar el servicio.
HEALTH_TRIES="${HEALTH_TRIES:-12}"
HEALTH_SLEEP="${HEALTH_SLEEP:-1}"

API="${BIN_DIR}/tinta-server"
MIGRATOR="${BIN_DIR}/tinta-migrate"

log() { echo "[deploy] $*"; }
fail() {
  echo "[deploy] ✗ $*" >&2
  exit 1
}
# sudo -n: nunca se queda esperando contraseña. Las entradas permitidas
# viven en /etc/sudoers.d/ (00-bootstrap.sh e install-system-files.sh).
as_root() {
  if [ "${NO_SUDO:-0}" = "1" ]; then "$@"; else sudo -n "$@"; fi
}
logs_of() {
  as_root "${JOURNALCTL}" -u "$1" -n 50 --no-pager 2>/dev/null ||
    log "(sin permiso para leer el journal de $1; corre install-system-files.sh)"
}
health_body() { curl -fsS --max-time 2 "${HEALTH_URL}" 2>/dev/null || true; }
version_of() { sed -n 's/.*"version":"\([^"]*\)".*/\1/p'; }
# wait_for <texto>: espera a que el cuerpo de /health lo contenga.
wait_for() {
  local want="$1" i
  for i in $(seq 1 "${HEALTH_TRIES}"); do
    case "$(health_body)" in
      *"${want}"*) return 0 ;;
    esac
    sleep "${HEALTH_SLEEP}"
  done
  return 1
}

[ -f "${API}.new" ] || fail "No existe ${API}.new: el caller tiene que subirlo antes."
chmod +x "${API}.new"

if [ "${SKIP_MIGRATE:-0}" != "1" ]; then
  [ -f "${MIGRATOR}.new" ] || fail "No existe ${MIGRATOR}.new: el caller tiene que subirlo antes."
  chmod +x "${MIGRATOR}.new"

  if "${SYSTEMCTL}" is-enabled --quiet tinta-backup.timer 2>/dev/null; then
    log "→ Respaldo previo (tinta-backup)..."
    if ! as_root "${SYSTEMCTL}" start tinta-backup; then
      logs_of tinta-backup
      rm -f "${API}.new" "${MIGRATOR}.new"
      fail "El respaldo previo falló. No se migró ni se cambió la API."
    fi
  else
    log "⚠ tinta-backup.timer no está activo: se migra sin respaldo previo."
  fi

  log "→ Migraciones (tinta-migrate)..."
  mv -f "${MIGRATOR}.new" "${MIGRATOR}"
  if ! as_root "${SYSTEMCTL}" start tinta-migrate; then
    logs_of tinta-migrate
    rm -f "${API}.new"
    fail "Las migraciones fallaron. La API sigue con la versión anterior; no se cambió el binario."
  fi
  log "✓ Migraciones aplicadas."
fi

PREV_VERSION="$(health_body | version_of)"
if [ -f "${API}" ]; then
  cp -p "${API}" "${API}.prev"
fi
mv -f "${API}.new" "${API}"
log "→ Reiniciando tinta-server (${PREV_VERSION:-sin versión previa} → ${EXPECTED})..."
as_root "${SYSTEMCTL}" restart tinta-server || true

if wait_for "\"version\":\"${EXPECTED}\""; then
  log "✓ /health reporta ${EXPECTED}."
  exit 0
fi

log "✗ ${EXPECTED} no respondió en ${HEALTH_URL}. Últimos logs:"
logs_of tinta-server
if [ ! -f "${API}.prev" ]; then
  fail "La API nueva no arrancó y no había binario anterior para regresar."
fi
mv -f "${API}" "${API}.failed"
cp -p "${API}.prev" "${API}"
as_root "${SYSTEMCTL}" reset-failed tinta-server 2>/dev/null || true
as_root "${SYSTEMCTL}" restart tinta-server || true
if [ -n "${PREV_VERSION}" ]; then
  back="\"version\":\"${PREV_VERSION}\""
else
  back='"status":"ok"'
fi
if wait_for "${back}"; then
  fail "Deploy revertido: la API volvió a ${PREV_VERSION:-la versión anterior}. La que falló quedó en ${API}.failed."
fi
fail "Se regresó al binario anterior, pero /health no responde. Revisa el servidor ya: systemctl status tinta-server."
