#!/usr/bin/env bash
# upload-env.sh — sube tu .env.prod local al server con permisos seguros.
#
# Se ejecuta DESDE TU LAPTOP. Reemplaza `/opt/tinta/tinta-server.env`
# en el servidor con el contenido de un archivo local. Hace `systemctl
# restart tinta-server` al final para que el binario tome los nuevos
# valores.
#
# Antes de subir muestra un DIFF contra el archivo que ya corre en el server
# y pide confirmación — es un reemplazo TOTAL, así no rotás un secret
# (JWT_SECRET, etc.) sin querer. Los valores van REDACTADOS por defecto
# (solo se ve qué llaves cambian); SHOW_VALUES=1 los muestra completos.
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
#   ASSUME_YES      Si ='1', salta el diff + la confirmación (automatización).
#   SHOW_VALUES     Si ='1', el diff muestra los VALORES completos de los
#                   secrets. Default: redactados (solo qué llaves cambian).
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

# ── Diff contra el server + confirmación ──────────────────────────────────
# Es un reemplazo TOTAL del archivo remoto. Mostramos qué cambia respecto a
# lo que YA corre en el server para no rotar secrets sin querer.
CURRENT=$(mktemp)
trap 'rm -f "${CURRENT}"' EXIT   # trae secrets del server — borrar siempre
chmod 600 "${CURRENT}"

# Bajamos el archivo actual exacto (bytes íntegros, sin pasar por una var que
# recorte newlines). Distinguimos "no existe aún" de "no pude conectar".
set +e
ssh "${SSH_USER}@${SERVER}" "cat ${REMOTE_PATH}" >"${CURRENT}" 2>/dev/null
rc=$?
set -e
if [ "${rc}" -ne 0 ]; then
  if ssh "${SSH_USER}@${SERVER}" true 2>/dev/null; then
    echo "→ ${REMOTE_PATH} no existe aún — sería un archivo NUEVO."
    : >"${CURRENT}"
  else
    echo "::error:: no pude conectar a ${SSH_USER}@${SERVER} para comparar." >&2
    exit 1
  fi
fi

# Redactor de valores (default). En cada línea de diff (+/-/contexto) enmascara
# lo que va tras el '='. Las llaves, comentarios y headers se mantienen.
redact() {
  if [ "${SHOW_VALUES:-0}" = "1" ]; then
    cat
  else
    sed -E 's/^([ +-][A-Za-z0-9_]+=).*/\1<redacted>/'
  fi
}

if [ "${SHOW_VALUES:-0}" = "1" ]; then
  echo "→ Diff (server '-' → local '+'), VALORES VISIBLES:"
else
  echo "→ Diff (server '-' → local '+'), valores redactados (SHOW_VALUES=1 para verlos):"
fi

# pipefail hace que el `if` refleje el exit de diff (0=idénticos, 1=hay cambios).
if diff -u --label "server: ${REMOTE_PATH}" --label "local: ${ENV_FILE}" \
     "${CURRENT}" "${ENV_FILE}" | redact; then
  echo "✓ Idénticos — el server ya tiene exactamente este contenido. Nada que subir."
  [ "${ASSUME_YES:-0}" = "1" ] || exit 0
fi

if [ "${ASSUME_YES:-0}" != "1" ]; then
  printf "¿Subir estos cambios a %s? [y/N] " "${SERVER}"
  read -r answer </dev/tty
  case "${answer}" in
    [yY] | [yY][eE][sS]) echo "→ Confirmado." ;;
    *) echo "Abortado — no se subió nada."; exit 1 ;;
  esac
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
