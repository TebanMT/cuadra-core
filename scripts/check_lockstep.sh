#!/usr/bin/env bash
# Lock-step check de migraciones (ADR-002 §5).
#
# Parsea db_migrations/lock_step.txt y valida:
#   1. Cada .sql en db_migrations/postgres/ está listado como `pair:` o
#      `cloud_only:`.
#   2. Cada .sql en db_migrations/sqlite/ está listado como `pair:` o
#      `sqlite_fix_only:`.
#   3. Toda referencia del manifiesto apunta a un archivo que existe.
#
# Sale con código != 0 si algún check falla — CI bloquea el merge.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
MANIFEST="${ROOT_DIR}/db_migrations/lock_step.txt"
PG_DIR="${ROOT_DIR}/db_migrations/postgres"
LITE_DIR="${ROOT_DIR}/db_migrations/sqlite"

if [ ! -f "${MANIFEST}" ]; then
  echo "::error::Lock-step manifest faltante: ${MANIFEST}"
  exit 1
fi

errors=0
err() {
  echo "::error::$*" >&2
  errors=$((errors + 1))
}

# ---------------------------------------------------------------------------
# Paso 1: recoger los nombres declarados en el manifiesto, por categoría.
# ---------------------------------------------------------------------------

declared_pg=()
declared_lite=()

while IFS= read -r line; do
  # Ignorar comentarios y líneas vacías.
  case "${line}" in
    \#*|"") continue ;;
  esac

  case "${line}" in
    pair:*)
      pg_file=$(echo "${line}" | sed -nE 's/.*pg=([^ ]+).*/\1/p')
      lite_file=$(echo "${line}" | sed -nE 's/.*sqlite=([^ ]+).*/\1/p')
      if [ -z "${pg_file}" ] || [ -z "${lite_file}" ]; then
        err "línea pair: malformada: ${line}"
        continue
      fi
      declared_pg+=("${pg_file}")
      declared_lite+=("${lite_file}")
      ;;
    cloud_only:*)
      pg_file=$(echo "${line}" | sed -nE 's/.*pg=([^ ]+).*/\1/p')
      if [ -z "${pg_file}" ]; then
        err "línea cloud_only: malformada: ${line}"
        continue
      fi
      declared_pg+=("${pg_file}")
      ;;
    sqlite_fix_only:*)
      lite_file=$(echo "${line}" | sed -nE 's/.*sqlite=([^ ]+).*/\1/p')
      if [ -z "${lite_file}" ]; then
        err "línea sqlite_fix_only: malformada: ${line}"
        continue
      fi
      declared_lite+=("${lite_file}")
      ;;
    *)
      err "línea con prefijo desconocido en lock_step.txt: ${line}"
      ;;
  esac
done < "${MANIFEST}"

# ---------------------------------------------------------------------------
# Paso 2: validar que cada archivo declarado existe en disco.
# ---------------------------------------------------------------------------

for f in "${declared_pg[@]}"; do
  if [ ! -f "${PG_DIR}/${f}" ]; then
    err "lock_step.txt referencia pg=${f} pero el archivo no existe en ${PG_DIR}"
  fi
done

for f in "${declared_lite[@]}"; do
  if [ ! -f "${LITE_DIR}/${f}" ]; then
    err "lock_step.txt referencia sqlite=${f} pero el archivo no existe en ${LITE_DIR}"
  fi
done

# ---------------------------------------------------------------------------
# Paso 3: validar que cada archivo en disco está declarado en el manifiesto.
# ---------------------------------------------------------------------------

has() {
  local needle="$1"
  shift
  for h in "$@"; do
    if [ "${h}" = "${needle}" ]; then
      return 0
    fi
  done
  return 1
}

for f in $(cd "${PG_DIR}" && ls *.sql 2>/dev/null); do
  if ! has "${f}" "${declared_pg[@]}"; then
    err "db_migrations/postgres/${f} NO está en lock_step.txt — agregalo como pair: o cloud_only:"
  fi
done

for f in $(cd "${LITE_DIR}" && ls *.sql 2>/dev/null); do
  if ! has "${f}" "${declared_lite[@]}"; then
    err "db_migrations/sqlite/${f} NO está en lock_step.txt — agregalo como pair: o sqlite_fix_only:"
  fi
done

# ---------------------------------------------------------------------------
# Salida
# ---------------------------------------------------------------------------

if [ "${errors}" -ne 0 ]; then
  echo ""
  echo "Lock-step check falló: ${errors} error(es)."
  echo "Mirá db_migrations/lock_step.txt para entender el formato."
  exit 1
fi

echo "Lock-step OK: $(echo "${declared_pg[@]}" | wc -w | tr -d ' ') migraciones PG, $(echo "${declared_lite[@]}" | wc -w | tr -d ' ') SQLite declaradas y verificadas."
