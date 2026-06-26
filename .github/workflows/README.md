# GitHub Actions — cuadra-core

> **Modelo:** un solo ambiente (prod). Los commits a `main` se validan pero
> **no** despliegan. Producción solo se actualiza cuando empujas un **tag
> semver** (`vX.Y.Z`). El tag es la única puerta a prod y te da semantic
> versioning real — la versión queda visible en `GET /health`.

## Workflows

### `ci.yml` — checks de calidad
Corre en **cada PR + push a main + workflow_dispatch**. También es
**reutilizable** (`workflow_call`): `release.yml` lo invoca antes de
desplegar.

| Job | Qué hace |
|---|---|
| `lint` | `gofmt -l` (debe estar vacío) + `go vet` con tag `server` y `sidecar`. |
| `test` | Levanta Postgres 16 como service, aplica `db_migrations/postgres/`, y corre `go test -tags "server sidecar" -race -count=1 ./...` (incluye los integration tests que gatean en `DATABASE_URL`). |

`ci.yml` **ya no compila el binario de release ni sube artifacts** — eso
se mueve a `release.yml`. Un break de compilación igual lo cachan `go vet`
+ `go test` (ambos compilan los dos tags), así que main sigue protegido.

### `release.yml` — build + deploy a prod
Corre **solo en push de un tag semver** (`v1.4.0`, `v1.4.0-rc.1`) o por
**workflow_dispatch** manual.

| Job | Qué hace |
|---|---|
| `checks` | Reusa `ci.yml` (lint + test) sobre el commit exacto que estás taggeando. Si no pasa, no construye ni despliega. |
| `build` | Cross-compila `tinta-server` linux/amd64 con `CGO_ENABLED=0`, estampando `-X main.version=$(git describe --tags --always --dirty)`. Sube el binario como artifact del run. |
| `deploy` | Baja el artifact, `scp` atómico al Hetzner CX33, `rsync` de `db_migrations/postgres/` a `/opt/tinta/migrations/`, reinicia `tinta-server` vía `sudo systemctl`, y hace smoke test a `/health`. **Las migraciones NO se aplican con `psql` en el deploy** — el binario corre `ApplyPostgresMigrations` (version-aware) al arrancar, así que el restart aplica solo las versiones nuevas leyendo de `/opt/tinta/migrations`. |

> **El gate duro del deploy es el `systemctl is-active`** (si el servicio no
> queda activo en 30s, el job falla). El smoke test a `/health` es
> **advisory**: si la versión no coincide o el DNS/TLS aún no está listo,
> solo emite un `::warning::`, no tumba el release. Tras el primer release,
> confirma a mano: `curl -s https://api.entinta.app/health`.

Si `tinta-server` no queda `is-active` en 30s, falla el job y dumpea
`journalctl -u tinta-server -n 50`.

### `lock-step-check.yml` — paridad de migraciones
Corre en push a main + PR que toquen `db_migrations/**`. Verifica que toda
migración sincronizable viva en `postgres/` y `sqlite/` en paralelo
(ADR-002 §5). No cambia con el flujo de tags.

## Cómo cortar un release

```bash
# 1. Mergeás todos los PRs/commits que quieras a main (CI los valida, no
#    despliega nada).
# 2. Cuando quieras publicar, taggeás semver y empujás el tag:
git tag v1.4.0
git push origin v1.4.0
# 3. release.yml arranca:
#      checks (lint + test) → build (estampa v1.4.0) → deploy
# 4. Verificás en prod:
curl -s https://api.entinta.app/health
#   → {"status":"ok","service":"tinta-server","version":"v1.4.0"}
```

Total: ~6-8 min desde el push del tag hasta el servicio reiniciado.

> **Semver:** `MAJOR.MINOR.PATCH`. Bump PATCH para fixes, MINOR para
> features compatibles, MAJOR para cambios que rompan. Prereleases con
> sufijo: `v1.4.0-rc.1`.

## Configuración inicial (una sola vez)

### 1. Generar el SSH key dedicado para CI

En tu laptop, **no reuses tu key personal**:

```bash
ssh-keygen -t ed25519 -f ~/.ssh/tinta-deploy -C "gha-deploy@tinta" -N ""
```

Eso genera `~/.ssh/tinta-deploy` (privada) y `~/.ssh/tinta-deploy.pub` (pública).

### 2. Autorizar la key pública en el server

```bash
cat ~/.ssh/tinta-deploy.pub | ssh tinta@204.168.214.238 \
  'cat >> ~/.ssh/authorized_keys'
```

Verifica que entra:

```bash
ssh -i ~/.ssh/tinta-deploy tinta@204.168.214.238 'whoami'
# → tinta
```

### 3. Capturar el host key del server

```bash
ssh-keyscan -H 204.168.214.238 > /tmp/known_hosts
```

Inspecciona el archivo — son 2-3 líneas (RSA, ECDSA, Ed25519). Lo vas a pegar tal cual en el secret de GitHub.

### 4. Configurar los secrets en GitHub

Repo → **Settings → Secrets and variables → Actions → New repository secret**.

| Nombre | Valor |
|---|---|
| `DEPLOY_HOST` | `204.168.214.238` (o `api.entinta.app` cuando el DNS esté listo) |
| `DEPLOY_SSH_KEY` | Contenido completo de `~/.ssh/tinta-deploy` (incluyendo `-----BEGIN OPENSSH PRIVATE KEY-----` y `-----END...-----`) |
| `DEPLOY_KNOWN_HOSTS` | Contenido completo de `/tmp/known_hosts` |
| `SMOKE_TEST_URL` | (opcional) `https://api.entinta.app/health`. Default si no se setea. |

### 5. Configurar el environment "production" (opcional pero recomendado)

Repo → **Settings → Environments → New environment** → `production`.

Puedes activar:
- **Required reviewers**: te exige darle "Approve" antes de cada deploy.
  Con el flujo de tags esto es doble red: el tag dispara, pero el deploy
  espera tu OK. Útil si quieres revisar antes de que el release vaya a prod.
- **Deployment branches and tags**: restringe qué refs pueden desplegar.
  ⚠️ **Si lo activas, elige la regla de _tags_ (selected tags) con patrón
  `v*` — NO una regla de _branches_.** Como el deploy se dispara desde un
  tag (no desde `main`), una regla solo-branches deja el job `deploy` en
  estado **`skipped`** y el release nunca llega al server (aunque `checks`
  y `build` salgan verdes). Tras configurarlo, verifica en el primer release
  que el job `deploy` aparezca como `waiting`/`success`, no `skipped`.

Si no lo configuras, el push del tag corre el release completo sin gating manual.

## Deploy manual / rollback

Cuando quieres re-desplegar un tag, o desplegar un commit que no taggeaste:

1. **Actions** tab → **Release tinta-server** → **Run workflow**.
2. En "Use workflow from" elige el tag/branch, o pásalo en el input `ref`
   (acepta un tag `v1.3.0` o un SHA).
3. Re-corre `checks` + `build` + `deploy` desde ese ref.

Para **rollback rápido a la versión anterior**: dispará el workflow con
`ref` = el tag bueno previo (ej. `v1.3.0`). El binario se recompila desde
ese código y `git describe` lo estampa con ese tag.

> **Emergencia sin pasar por Actions:** `scripts/deploy/02-deploy-binary.sh`
> desde tu laptop compila + sube + reinicia directo (también estampa la
> versión vía `git describe`). Es el bypass de último recurso.

> **Cuidado con migraciones**: son forward-only y las aplica el binario al
> arrancar, version-aware (salta las que ya están en `_migrations`). Si
> haces rollback a un commit con menos migraciones, las ya aplicadas en
> Postgres no se revierten — solo no corren las nuevas. Para revertir schema,
> hazlo a mano con un `*.sql` de rollback. Una migración que FALLA al aplicar
> impide que el binario levante (`log.Fatalf`), así que el deploy falla en el
> poll de `is-active` en lugar de dejar prod a medias.

## Rotar el SSH key

Si la deploy key se compromete:

```bash
# 1. Generar nueva.
ssh-keygen -t ed25519 -f ~/.ssh/tinta-deploy-new -C "gha-deploy@tinta" -N ""

# 2. Autorizar la nueva.
cat ~/.ssh/tinta-deploy-new.pub | ssh tinta@$DEPLOY_HOST 'cat >> ~/.ssh/authorized_keys'

# 3. Quitar la vieja.
ssh tinta@$DEPLOY_HOST '
  grep -v "gha-deploy@tinta" ~/.ssh/authorized_keys > /tmp/ak
  mv /tmp/ak ~/.ssh/authorized_keys
  chmod 600 ~/.ssh/authorized_keys
'
# (Re-agrega la pub nueva si el grep la borró.)

# 4. Update secret DEPLOY_SSH_KEY en GitHub con la nueva privada.
# 5. Borrá la key vieja localmente.
shred -u ~/.ssh/tinta-deploy ~/.ssh/tinta-deploy.pub
mv ~/.ssh/tinta-deploy-new ~/.ssh/tinta-deploy
mv ~/.ssh/tinta-deploy-new.pub ~/.ssh/tinta-deploy.pub
```

## Troubleshooting

**El push del tag no disparó nada:**
- El tag no matchea el patrón. Tiene que ser `vMAJOR.MINOR.PATCH`
  (`v1.4.0`), opcionalmente con sufijo de prerelease (`v1.4.0-rc.1`).
  `1.4.0` (sin `v`) o `v1.4` (sin patch) **no** disparan.
- ¿Empujaste el tag? `git push origin v1.4.0` (el `git push` normal no
  manda tags).

**`/health` no muestra la versión nueva tras el deploy:**
- El restart no levantó el binario nuevo. Revisa el step "Reiniciar
  tinta-server" y `journalctl -u tinta-server`.

**`Permission denied (publickey)` al hacer scp:**
- La pub no quedó en `~/.ssh/authorized_keys` del usuario `tinta`. Verifica con `ssh -i ~/.ssh/tinta-deploy tinta@$DEPLOY_HOST 'cat ~/.ssh/authorized_keys'`.

**`Host key verification failed`:**
- `DEPLOY_KNOWN_HOSTS` está vacío o desfasado. Re-genera con `ssh-keyscan -H` y reemplaza el secret.

**`sudo: a password is required`:**
- El sudoers de `00-bootstrap.sh` no se aplicó. Verifica `/etc/sudoers.d/tinta-systemctl` en el server.

**Migraciones fallan con `permission denied for schema public`:**
- El user `tinta` no tiene permisos. Re-correr `01-create-db.sh` (es idempotente y rota el GRANT).

**Tests pasan en local pero no en CI:**
- En CI se aplican migraciones desde cero contra Postgres limpio. Si tu test asume datos, agrega un seed/setup propio.
