#!/usr/bin/env bash
# 02-deploy-binary.sh — deploy de emergencia desde tu laptop.
#
# El camino normal es release.yml (git tag). Este script hace lo mismo sin
# pasar por Actions:
#   1. Exporta un commit con `git archive` (default HEAD) a una carpeta
#      temporal y compila ahí tinta-server + tinta-migrate (linux/amd64).
#      Lo que no está en commit (cambios, archivos nuevos, migraciones a
#      medias) NO se despliega nunca, aunque esté en tu carpeta.
#   2. Sube binarios como .new y el SQL de ese commit, sin activar nada.
#   3. Corre scripts/deploy/remote-release.sh en el servidor: respaldo
#      previo → migraciones → cambio de binario → /health con la versión
#      nueva, con rollback automático si la API nueva no arranca.
#
# El binario `tinta-server` no usa CGO (solo el sidecar local), así que
# cross-compilar desde Mac funciona sin tooling extra.
#
# Uso:
#   SERVER=204.168.214.238 bash scripts/deploy/02-deploy-binary.sh
#   SERVER=... REF=v1.4.0 bash scripts/deploy/02-deploy-binary.sh
#
# Variables opcionales:
#   REF (default: HEAD) — tag, rama o SHA a desplegar.
#   SSH_USER (default: tinta) — usuario para el deploy.
#   SKIP_MIGRATE=1 — solo binario: sin respaldo, sin migraciones, sin SQL.
#   SKIP_RESTART=1 — sube todo pero no activa. Para activarlo después:
#     ssh tinta@$SERVER "bash /opt/tinta/bin/remote-release.sh <versión>"

set -euo pipefail

: "${SERVER:?Falta SERVER (ip o hostname)}"
SSH_USER="${SSH_USER:-tinta}"
REF="${REF:-HEAD}"
REMOTE_BIN_DIR="/opt/tinta/bin"
REMOTE_MIGRATIONS="/opt/tinta/migrations"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}"

SHA="$(git rev-parse --verify "${REF}^{commit}")"
# Igual que release.yml: "v1.4.0" en un tag exacto, "v1.4.0-3-gabc1234"
# con commits encima, SHA corto sin tags. Nunca -dirty: se compila el commit.
VERSION="$(git describe --tags --always "${SHA}")"
if [ -n "$(git status --porcelain)" ]; then
  echo "⚠ Tienes cambios sin commit en ${REPO_ROOT}. No se despliegan: va ${REF} (${SHA:0:12})."
fi
echo "→ Desplegando ${VERSION} (${SHA:0:12}) a ${SSH_USER}@${SERVER}"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
git archive --format=tar "${SHA}" | tar -x -C "${WORK}"

echo "→ Build linux/amd64 desde el commit exportado..."
(
  cd "${WORK}"
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -tags server -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o "${WORK}/out/tinta-server" ./cmd/server
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -tags server -trimpath -ldflags="-s -w" \
    -o "${WORK}/out/tinta-migrate" ./cmd/migrate
)
ls -lh "${WORK}/out/tinta-server"

# Sin el setup de RLS (servicio tinta-migrate) no se sube nada.
ssh "${SSH_USER}@${SERVER}" "/bin/systemctl cat tinta-migrate >/dev/null"

echo "→ Subiendo binarios (sin activar)..."
scp "${WORK}/out/tinta-server" "${SSH_USER}@${SERVER}:${REMOTE_BIN_DIR}/tinta-server.new"
# El paso final sale de tu carpeta, no del commit: así un rollback a un tag
# viejo (REF=v1.3.0) usa la lógica de deploy actual.
scp "${REPO_ROOT}/scripts/deploy/remote-release.sh" "${SSH_USER}@${SERVER}:${REMOTE_BIN_DIR}/remote-release.sh"
if [ "${SKIP_MIGRATE:-0}" != "1" ]; then
  scp "${WORK}/out/tinta-migrate" "${SSH_USER}@${SERVER}:${REMOTE_BIN_DIR}/tinta-migrate.new"
  echo "→ Subiendo migraciones SQL de ${SHA:0:12} a ${REMOTE_MIGRATIONS}..."
  rsync -avz --delete \
    "${WORK}/db_migrations/postgres/" \
    "${SSH_USER}@${SERVER}:${REMOTE_MIGRATIONS}/"
fi

if [ "${SKIP_RESTART:-0}" = "1" ]; then
  echo ""
  echo "✓ Archivos arriba, sin activar. Para activarlos:"
  echo "    ssh ${SSH_USER}@${SERVER} \"SKIP_MIGRATE=${SKIP_MIGRATE:-0} bash ${REMOTE_BIN_DIR}/remote-release.sh '${VERSION}'\""
  exit 0
fi

ssh "${SSH_USER}@${SERVER}" "SKIP_MIGRATE=${SKIP_MIGRATE:-0} bash ${REMOTE_BIN_DIR}/remote-release.sh '${VERSION}'"

echo ""
echo "✓ Deploy completo: ${VERSION}."
