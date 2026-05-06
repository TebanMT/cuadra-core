#!/usr/bin/env bash
# 01-create-db.sh — crea el usuario y la base de datos de Postgres en el
# servidor de producción.
#
# Se ejecuta DESDE TU LAPTOP. Hace ssh al servidor como root y corre los
# comandos vía sudo -u postgres psql.
#
# Idempotente — si el role o la DB ya existen, no falla.
#
# Uso:
#   SERVER=204.168.214.238 \
#   DB_PASSWORD='<password-fuerte>' \
#     bash 01-create-db.sh
#
# Después de correr esto, anota DATABASE_URL para meterla en el .env del
# servidor (ver scripts/deploy/cuadra-server.env.example).

set -euo pipefail

: "${SERVER:?Falta SERVER (ip o hostname del Hetzner)}"
: "${DB_PASSWORD:?Falta DB_PASSWORD — generala con: openssl rand -base64 32 | tr -d '/+=' | head -c 32}"

DB_USER="${DB_USER:-cuadra}"
DB_NAME="${DB_NAME:-cuadra}"
SSH_USER="${SSH_USER:-root}"

echo "→ Creando role y database en Postgres del servidor ${SERVER}..."

ssh "${SSH_USER}@${SERVER}" bash -s <<EOF
set -euo pipefail

# Role (crearlo solo si no existe).
sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='${DB_USER}'" | grep -q 1 || \
  sudo -u postgres psql -c "CREATE ROLE ${DB_USER} LOGIN PASSWORD '${DB_PASSWORD}';"

# Database (idem).
sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='${DB_NAME}'" | grep -q 1 || \
  sudo -u postgres psql -c "CREATE DATABASE ${DB_NAME} OWNER ${DB_USER};"

# Permisos (por idempotencia, los re-aplicamos siempre).
sudo -u postgres psql -c "GRANT ALL PRIVILEGES ON DATABASE ${DB_NAME} TO ${DB_USER};"
sudo -u postgres psql -d ${DB_NAME} -c "GRANT ALL ON SCHEMA public TO ${DB_USER};"

# Por si el password cambió (rotación), siempre lo re-asignamos.
sudo -u postgres psql -c "ALTER ROLE ${DB_USER} WITH PASSWORD '${DB_PASSWORD}';"

echo "✓ Postgres listo: role=${DB_USER} db=${DB_NAME}"
EOF

echo ""
echo "✓ Database creada. Tu DATABASE_URL para el .env del servidor es:"
echo ""
echo "    DATABASE_URL=postgresql://${DB_USER}:${DB_PASSWORD}@127.0.0.1:5432/${DB_NAME}?sslmode=disable"
echo ""
echo "Próximo paso:"
echo "  bash 02-deploy-binary.sh"
