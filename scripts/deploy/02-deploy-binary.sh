#!/usr/bin/env bash
# 02-deploy-binary.sh — build local + scp + systemd reload.
#
# Se ejecuta DESDE TU LAPTOP. Cross-compila cuadra-server para
# linux/amd64, lo sube a /opt/cuadra/bin/, copia las migraciones SQL,
# las aplica a Postgres y reinicia el servicio.
#
# El binario `cuadra-server` no usa CGO (solo el sidecar local), así que
# cross-compilar desde Mac funciona sin tooling extra.
#
# Uso:
#   SERVER=204.168.214.238 \
#     bash 02-deploy-binary.sh
#
# Variables opcionales:
#   SSH_USER (default: cuadra) — usuario para el deploy. cuadra ya tiene
#     NOPASSWD para systemctl restart cuadra-server.
#   SKIP_MIGRATE=1 — solo redeploy del binario, sin tocar SQL.
#   SKIP_RESTART=1 — copia archivos pero no reinicia (útil para precargar).

set -euo pipefail

: "${SERVER:?Falta SERVER (ip o hostname)}"
SSH_USER="${SSH_USER:-cuadra}"
REMOTE_BIN="/opt/cuadra/bin/cuadra-server"
REMOTE_MIGRATIONS="/opt/cuadra/migrations"

# Resolver el repo root desde la ubicación de este script.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}"

echo "→ Build linux/amd64 desde ${REPO_ROOT}..."
mkdir -p bin
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -tags server -trimpath -ldflags="-s -w" \
  -o bin/cuadra-server-linux-amd64 ./cmd/server

# Tamaño para sanity check.
ls -lh bin/cuadra-server-linux-amd64

echo "→ Subiendo binario a ${SSH_USER}@${SERVER}:${REMOTE_BIN}..."
# Subir a un path .new y rotar atómicamente para evitar binario corrupto si
# el scp se corta.
scp bin/cuadra-server-linux-amd64 "${SSH_USER}@${SERVER}:${REMOTE_BIN}.new"
ssh "${SSH_USER}@${SERVER}" "
  set -e
  chmod +x ${REMOTE_BIN}.new
  mv ${REMOTE_BIN}.new ${REMOTE_BIN}
"

if [ "${SKIP_MIGRATE:-0}" != "1" ]; then
  echo "→ Subiendo migraciones SQL..."
  rsync -avz --delete \
    db_migrations/postgres/ \
    "${SSH_USER}@${SERVER}:${REMOTE_MIGRATIONS}/"

  echo "→ Aplicando migraciones (psql -v ON_ERROR_STOP=1)..."
  ssh "${SSH_USER}@${SERVER}" bash -s <<'EOF'
set -euo pipefail
source /opt/cuadra/cuadra-server.env
for f in $(ls -1 /opt/cuadra/migrations/*.sql | sort); do
  echo "  applying $(basename "$f")"
  psql "${DATABASE_URL}" -v ON_ERROR_STOP=1 -f "$f" >/dev/null
done
echo "✓ Migraciones aplicadas"
EOF
fi

if [ "${SKIP_RESTART:-0}" != "1" ]; then
  echo "→ Reiniciando cuadra-server..."
  ssh "${SSH_USER}@${SERVER}" "sudo /bin/systemctl restart cuadra-server"

  echo "→ Esperando health..."
  sleep 2
  ssh "${SSH_USER}@${SERVER}" "sudo /bin/systemctl status cuadra-server --no-pager -l | head -20"

  # Health check directo al puerto interno (no expuesto a internet).
  ssh "${SSH_USER}@${SERVER}" "curl -fsS http://127.0.0.1:8080/health || echo '(no /health endpoint, ok)'"
fi

echo ""
echo "✓ Deploy completo."
