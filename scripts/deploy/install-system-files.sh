#!/usr/bin/env bash
# install-system-files.sh — instala/actualiza los archivos de sistema
# (systemd unit + Caddyfile) en el servidor.
#
# Se ejecuta DESDE TU LAPTOP. Va con sudo en remoto. Lo corres una vez al
# inicio y cada vez que cambia el .service o el Caddyfile en el repo.
#
# Uso:
#   SERVER=204.168.214.238 bash install-system-files.sh
#
# Antes de correr esto la primera vez:
#   1. `bash 00-bootstrap.sh` ya corrió en el server (vía ssh root).
#   2. Editaste y subiste a mano `/opt/cuadra/cuadra-server.env` con los
#      secretos reales — este script NO toca el .env (es secret material).

set -euo pipefail

: "${SERVER:?Falta SERVER}"
SSH_USER="${SSH_USER:-root}"   # systemd + Caddy requieren root para instalar.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

echo "→ Subiendo archivos a /tmp/..."
scp "${REPO_ROOT}/scripts/systemd/cuadra-server.service" \
    "${SSH_USER}@${SERVER}:/tmp/cuadra-server.service"
scp "${REPO_ROOT}/scripts/deploy/Caddyfile" \
    "${SSH_USER}@${SERVER}:/tmp/Caddyfile"

echo "→ Instalando en el servidor..."
ssh "${SSH_USER}@${SERVER}" bash -s <<'EOF'
set -euo pipefail

# systemd unit.
install -o root -g root -m 644 /tmp/cuadra-server.service /etc/systemd/system/cuadra-server.service
rm /tmp/cuadra-server.service
systemctl daemon-reload

# El servicio queda enabled aunque el binario aún no exista — el primer
# `02-deploy-binary.sh` lo va a arrancar de verdad.
systemctl enable cuadra-server || true

# Caddyfile.
install -o root -g root -m 644 /tmp/Caddyfile /etc/caddy/Caddyfile
rm /tmp/Caddyfile

# Validar antes de recargar — un Caddyfile inválido tumba HTTPS.
caddy validate --config /etc/caddy/Caddyfile
systemctl reload caddy || systemctl restart caddy

# Si el .env no existe aún, dejar un placeholder con permisos correctos
# para que el operador sepa qué editar.
if [ ! -f /opt/cuadra/cuadra-server.env ]; then
  install -o cuadra -g cuadra -m 600 /dev/null /opt/cuadra/cuadra-server.env
  echo "⚠ /opt/cuadra/cuadra-server.env está vacío — copia el .example y rellénalo antes del primer deploy."
fi

echo "✓ systemd + Caddy listos."
EOF

echo ""
echo "✓ Archivos de sistema instalados."
echo ""
echo "Si es tu primera vez, ahora:"
echo "  1. Sube a /opt/cuadra/cuadra-server.env los valores reales:"
echo "       scp scripts/deploy/cuadra-server.env.example \\"
echo "         root@${SERVER}:/opt/cuadra/cuadra-server.env.tmpl"
echo "       ssh root@${SERVER}"
echo "       # editas el archivo, lo mueves a cuadra-server.env, chown cuadra:cuadra, chmod 600"
echo "  2. bash 02-deploy-binary.sh"
