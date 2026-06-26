#!/usr/bin/env bash
# 02-deploy-binary.sh — build local + scp + systemd reload.
#
# Se ejecuta DESDE TU LAPTOP. Cross-compila tinta-server para
# linux/amd64, lo sube a /opt/tinta/bin/, copia las migraciones SQL,
# las aplica a Postgres y reinicia el servicio.
#
# El binario `tinta-server` no usa CGO (solo el sidecar local), así que
# cross-compilar desde Mac funciona sin tooling extra.
#
# Uso:
#   SERVER=204.168.214.238 \
#     bash 02-deploy-binary.sh
#
# Variables opcionales:
#   SSH_USER (default: tinta) — usuario para el deploy. tinta ya tiene
#     NOPASSWD para systemctl restart tinta-server.
#   SKIP_MIGRATE=1 — solo redeploy del binario, sin tocar SQL.
#   SKIP_RESTART=1 — copia archivos pero no reinicia (útil para precargar).

set -euo pipefail

: "${SERVER:?Falta SERVER (ip o hostname)}"
SSH_USER="${SSH_USER:-tinta}"
REMOTE_BIN="/opt/tinta/bin/tinta-server"
REMOTE_MIGRATIONS="/opt/tinta/migrations"

# Resolver el repo root desde la ubicación de este script.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}"

echo "→ Build linux/amd64 desde ${REPO_ROOT}..."
mkdir -p bin
# Estampar la versión igual que release.yml. git describe da el tag
# (vX.Y.Z) si HEAD está taggeado, "<tag>-<n>-g<sha>" si hay commits encima,
# o SHA corto si no hay tags. -dirty marca un working tree con cambios.
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
echo "  versión: ${VERSION}"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -tags server -trimpath \
  -ldflags="-s -w -X main.version=${VERSION}" \
  -o bin/tinta-server-linux-amd64 ./cmd/server

# Tamaño para sanity check.
ls -lh bin/tinta-server-linux-amd64

echo "→ Subiendo binario a ${SSH_USER}@${SERVER}:${REMOTE_BIN}..."
# Subir a un path .new y rotar atómicamente para evitar binario corrupto si
# el scp se corta.
scp bin/tinta-server-linux-amd64 "${SSH_USER}@${SERVER}:${REMOTE_BIN}.new"
ssh "${SSH_USER}@${SERVER}" "
  set -e
  chmod +x ${REMOTE_BIN}.new
  mv ${REMOTE_BIN}.new ${REMOTE_BIN}
"

if [ "${SKIP_MIGRATE:-0}" != "1" ]; then
  echo "→ Subiendo migraciones SQL a ${REMOTE_MIGRATIONS}..."
  # Solo copiamos los .sql. El binario los aplica al arrancar
  # (ApplyPostgresMigrations, version-aware) en el restart de abajo — NO los
  # corremos con psql acá. Re-aplicar todo desde 001 se rompía cuando una
  # migración posterior evolucionaba el esquema (p.ej. 020 dropeó el índice
  # único de huellas y re-correr 001 lo recreaba sobre datos multi-huella).
  rsync -avz --delete \
    db_migrations/postgres/ \
    "${SSH_USER}@${SERVER}:${REMOTE_MIGRATIONS}/"
fi

if [ "${SKIP_RESTART:-0}" != "1" ]; then
  echo "→ Reiniciando tinta-server..."
  ssh "${SSH_USER}@${SERVER}" "sudo /bin/systemctl restart tinta-server"

  echo "→ Esperando health..."
  sleep 2
  ssh "${SSH_USER}@${SERVER}" "sudo /bin/systemctl status tinta-server --no-pager -l | head -20"

  # Health check directo al puerto interno (no expuesto a internet).
  ssh "${SSH_USER}@${SERVER}" "curl -fsS http://127.0.0.1:8080/health || echo '(no /health endpoint, ok)'"
fi

echo ""
echo "✓ Deploy completo."
