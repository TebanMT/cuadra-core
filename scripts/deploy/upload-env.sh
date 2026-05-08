#!/usr/bin/env bash
# upload-env.sh — sube tu .env.prod local al server con permisos seguros.
#
# Se ejecuta DESDE TU LAPTOP. Reemplaza `/opt/tinta/tinta-server.env`
# en el servidor con el contenido de un archivo local. Hace `systemctl
# restart tinta-server` al final para que el binario tome los nuevos
# valores.
#
# Uso típico (al rotar un secret):
#   SERVER=204.168.214.238 bash scripts/deploy/upload-env.sh .env.prod
#
# Variables:
#   SERVER          IP o hostname del server. Required.
#   ENV_FILE        Path al archivo local. Default: el primer arg, o .env.prod.
#   SSH_USER        Usuario para subir al /tmp. Default: root.
#   SKIP_RESTART    Si ='1', no reinicia el servicio (útil si vas a hacer
#                   más cambios después).
#
# Seguridad:
#   - Usa scp + install con perms 600 — el archivo nunca queda visible
#     a otros users del server.
#   - Borra la copia temporal en /tmp después de moverla.
#   - NO borra tu copia local (la mantienes como source of truth).

set -euo pipefail

: "${SERVER:?Falta SERVER (ip o hostname del Hetzner)}"

ENV_FILE="${1:-.env.prod}"
SSH_USER="${SSH_USER:-root}"
REMOTE_PATH="/opt/tinta/tinta-server.env"

if [ ! -f "${ENV_FILE}" ]; then
  echo "::error:: archivo no existe: ${ENV_FILE}" >&2
  echo "Si es la primera vez, copia el template:" >&2
  echo "  cp scripts/deploy/tinta-server.env.example .env.prod" >&2
  echo "  \$EDITOR .env.prod" >&2
  exit 1
fi

# Sanity check — que no estés a punto de subir un .example sin rellenar.
if grep -qE '^[A-Z_]+=CHANGE_ME' "${ENV_FILE}"; then
  echo "::error:: el archivo todavía tiene placeholders 'CHANGE_ME':" >&2
  grep -nE '^[A-Z_]+=CHANGE_ME' "${ENV_FILE}" >&2
  echo "Edita el archivo y reemplazá los placeholders antes de subir." >&2
  exit 1
fi

# Sanity check — verificar que las vars críticas estén presentes.
REQUIRED=(JWT_SECRET DATABASE_URL PUBLIC_BASE_URL CORS_ALLOWED_ORIGINS)
MISSING=()
for var in "${REQUIRED[@]}"; do
  grep -qE "^${var}=." "${ENV_FILE}" || MISSING+=("${var}")
done
if [ ${#MISSING[@]} -gt 0 ]; then
  echo "::error:: faltan variables requeridas en ${ENV_FILE}:" >&2
  printf '  - %s\n' "${MISSING[@]}" >&2
  exit 1
fi

echo "→ Subiendo ${ENV_FILE} → ${SSH_USER}@${SERVER}:${REMOTE_PATH}..."
scp "${ENV_FILE}" "${SSH_USER}@${SERVER}:/tmp/tinta-server.env.new"

ssh "${SSH_USER}@${SERVER}" bash -s <<EOF
set -euo pipefail
install -o tinta -g tinta -m 600 /tmp/tinta-server.env.new ${REMOTE_PATH}
rm /tmp/tinta-server.env.new
echo "✓ ${REMOTE_PATH} actualizado (perms: \$(stat -c '%a %U:%G' ${REMOTE_PATH}))"
EOF

if [ "${SKIP_RESTART:-0}" != "1" ]; then
  echo "→ Reiniciando tinta-server para aplicar los nuevos valores..."
  ssh "${SSH_USER}@${SERVER}" 'sudo /bin/systemctl restart tinta-server'
  sleep 2
  ssh "${SSH_USER}@${SERVER}" 'sudo /bin/systemctl is-active tinta-server'
fi

echo ""
echo "✓ .env subido y aplicado."
echo "  Recordatorio: tu copia local ${ENV_FILE} sigue siendo source of"
echo "  truth. Mantén un backup encriptado en tu password manager."
