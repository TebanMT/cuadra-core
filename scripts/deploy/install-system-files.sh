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
#   2. Editaste y subiste a mano `/opt/tinta/tinta-server.env` con los
#      secretos reales — este script NO toca el .env (es secret material).

set -euo pipefail

: "${SERVER:?Falta SERVER}"
SSH_USER="${SSH_USER:-root}"   # systemd + Caddy requieren root para instalar.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

echo "→ Subiendo archivos a /tmp/..."
scp "${REPO_ROOT}/scripts/systemd/tinta-server.service" \
    "${SSH_USER}@${SERVER}:/tmp/tinta-server.service"
scp "${REPO_ROOT}/scripts/deploy/Caddyfile" \
    "${SSH_USER}@${SERVER}:/tmp/Caddyfile"

echo "→ Instalando en el servidor..."
ssh "${SSH_USER}@${SERVER}" bash -s <<'EOF'
set -euo pipefail

# ── Pre-check: Caddy debe estar instalado. ─────────────────────────
# Si por alguna razón el bootstrap no terminó el step de Caddy
# (apt repo malformado, paquete saltado, etc), instalamos acá antes
# de intentar copiar el Caddyfile — si no, revienta con
# "/etc/caddy/Caddyfile: No such file or directory".
if ! command -v caddy >/dev/null 2>&1 || [ ! -d /etc/caddy ]; then
  echo "→ Caddy no está instalado o /etc/caddy falta — instalando…"
  apt-get -y install debian-keyring debian-archive-keyring apt-transport-https curl gnupg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
    | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
    > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update
  apt-get -y install caddy
  echo "✓ Caddy instalado: $(caddy version)"
fi

# systemd unit.
install -o root -g root -m 644 /tmp/tinta-server.service /etc/systemd/system/tinta-server.service
rm /tmp/tinta-server.service
systemctl daemon-reload

# El servicio queda enabled aunque el binario aún no exista — el primer
# `02-deploy-binary.sh` lo va a arrancar de verdad.
systemctl enable tinta-server || true

# Caddyfile — `mkdir -p` por si /etc/caddy todavía no existe (paquete
# recién instalado a veces no lo crea hasta el primer arranque).
mkdir -p /etc/caddy
install -o root -g root -m 644 /tmp/Caddyfile /etc/caddy/Caddyfile
rm /tmp/Caddyfile

# Validar antes de recargar — un Caddyfile inválido tumba HTTPS.
caddy validate --config /etc/caddy/Caddyfile
systemctl enable caddy 2>/dev/null || true
systemctl reload caddy || systemctl restart caddy

# Si el .env no existe aún, dejar un placeholder con permisos correctos
# para que el operador sepa qué editar.
if [ ! -f /opt/tinta/tinta-server.env ]; then
  install -o tinta -g tinta -m 600 /dev/null /opt/tinta/tinta-server.env
  echo "⚠ /opt/tinta/tinta-server.env está vacío — copia el .example y rellénalo antes del primer deploy."
fi

echo "✓ systemd + Caddy listos."
EOF

echo ""
echo "✓ Archivos de sistema instalados."
echo ""
echo "Si es tu primera vez, ahora:"
echo "  1. Sube a /opt/tinta/tinta-server.env los valores reales:"
echo "       scp scripts/deploy/tinta-server.env.example \\"
echo "         root@${SERVER}:/opt/tinta/tinta-server.env.tmpl"
echo "       ssh root@${SERVER}"
echo "       # editas el archivo, lo mueves a tinta-server.env, chown tinta:tinta, chmod 600"
echo "  2. bash 02-deploy-binary.sh"
