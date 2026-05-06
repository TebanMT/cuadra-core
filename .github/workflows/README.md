# GitHub Actions — cuadra-core

## Workflows

### `ci.yml`
Corre en **cada PR + push a main + workflow_dispatch**.

| Job | Qué hace |
|---|---|
| `lint` | `gofmt -l` (debe estar vacío) + `go vet` con tag `server` y `sidecar`. |
| `test` | Levanta Postgres 16 como service, aplica `db_migrations/postgres/`, y corre `go test -tags "server sidecar" -race -count=1 ./...`. Esto incluye los integration tests que gatean en `DATABASE_URL`. |
| `build` | Cross-compila `cuadra-server` para `linux/amd64` con `CGO_ENABLED=0` y verifica que el sidecar también compile (CGO on, host arch). En `push` a main sube el binario como artifact para que `deploy.yml` lo reuse. |

### `deploy.yml`
Corre en **push a main (después de CI) y workflow_dispatch manual**.

Reusa el binario de CI (no recompila), lo manda al Hetzner CX33 con `scp` atómico, sincroniza `db_migrations/postgres/`, las aplica con `psql -v ON_ERROR_STOP=1`, y reinicia `cuadra-server` vía `sudo systemctl`.

Si `cuadra-server` no queda `is-active` en 30s, falla el job y dumpea `journalctl -u cuadra-server -n 50`.

## Configuración inicial (una sola vez)

### 1. Generar el SSH key dedicado para CI

En tu laptop, **no reuses tu key personal**:

```bash
ssh-keygen -t ed25519 -f ~/.ssh/cuadra-deploy -C "gha-deploy@cuadra-core" -N ""
```

Eso genera `~/.ssh/cuadra-deploy` (privada) y `~/.ssh/cuadra-deploy.pub` (pública).

### 2. Autorizar la key pública en el server

```bash
cat ~/.ssh/cuadra-deploy.pub | ssh cuadra@204.168.214.238 \
  'cat >> ~/.ssh/authorized_keys'
```

Verifica que entra:

```bash
ssh -i ~/.ssh/cuadra-deploy cuadra@204.168.214.238 'whoami'
# → cuadra
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
| `DEPLOY_HOST` | `204.168.214.238` (o `api.cuadra.app` cuando el DNS esté listo) |
| `DEPLOY_SSH_KEY` | Contenido completo de `~/.ssh/cuadra-deploy` (incluyendo `-----BEGIN OPENSSH PRIVATE KEY-----` y `-----END...-----`) |
| `DEPLOY_KNOWN_HOSTS` | Contenido completo de `/tmp/known_hosts` |
| `SMOKE_TEST_URL` | (opcional) `https://api.cuadra.app/health`. Default si no se setea. |

### 5. Configurar el environment "production" (opcional)

Repo → **Settings → Environments → New environment** → `production`.

Puedes activar:
- **Required reviewers**: te exige darle "Approve" antes de cada deploy. Útil si quieres revisar el commit antes de que vaya a prod.
- **Deployment branches**: solo `main`.

Si no lo configuras, el workflow corre sin gating manual — el merge a main ya es la aprobación.

## Cómo se ve un deploy normal

```
1. Mergeás un PR a main.
2. CI corre (lint + test + build) — ~3-4 min.
3. Si CI pasa, deploy.yml arranca:
     - download-artifact (binario de CI)
     - scp atómico
     - rsync migraciones
     - psql -f de cada *.sql
     - sudo systemctl restart cuadra-server
     - poll is-active hasta 30s
     - smoke test https://api.cuadra.app/health
4. Ves "✓" en el PR + check verde en Actions.
```

Total: ~5-6 min desde merge hasta servicio reiniciado.

## Deploy manual (rollback / hotfix)

Cuando quieres deployar un commit que no está en main, o re-correr el último deploy:

1. **Actions** tab → **Deploy cuadra-server** → **Run workflow** → elegir branch/tag.
2. El workflow recompila desde el código del branch elegido (no usa artifact, porque no hubo CI).

Para rollback rápido:

```bash
# Encuentra el commit anterior bueno.
git log --oneline -10

# Crea un branch temporal apuntando ahí.
git push origin <commit-sha>:refs/heads/rollback

# En GitHub Actions: Run workflow → branch=rollback.
```

> **Cuidado con migraciones**: las del repo son forward-only. Si el commit anterior tenía menos migraciones, las que ya están aplicadas en Postgres no se revierten — solo se "saltan" las nuevas. Si necesitas revertir schema, hazlo a mano con un `*.sql` de rollback.

## Rotar el SSH key

Si la deploy key se compromete:

```bash
# 1. Generar nueva.
ssh-keygen -t ed25519 -f ~/.ssh/cuadra-deploy-new -C "gha-deploy@cuadra-core" -N ""

# 2. Autorizar la nueva.
cat ~/.ssh/cuadra-deploy-new.pub | ssh cuadra@$DEPLOY_HOST 'cat >> ~/.ssh/authorized_keys'

# 3. Quitar la vieja.
ssh cuadra@$DEPLOY_HOST '
  grep -v "gha-deploy@cuadra-core" ~/.ssh/authorized_keys > /tmp/ak
  mv /tmp/ak ~/.ssh/authorized_keys
  chmod 600 ~/.ssh/authorized_keys
'
# (Re-agregá la pub nueva si el grep la borró.)

# 4. Update secret DEPLOY_SSH_KEY en GitHub con la nueva privada.
# 5. Borrá la key vieja localmente.
shred -u ~/.ssh/cuadra-deploy ~/.ssh/cuadra-deploy.pub
mv ~/.ssh/cuadra-deploy-new ~/.ssh/cuadra-deploy
mv ~/.ssh/cuadra-deploy-new.pub ~/.ssh/cuadra-deploy.pub
```

## Troubleshooting

**`Permission denied (publickey)` al hacer scp:**
- La pub no quedó en `~/.ssh/authorized_keys` del usuario `cuadra`. Verifica con `ssh -i ~/.ssh/cuadra-deploy cuadra@$DEPLOY_HOST 'cat ~/.ssh/authorized_keys'`.

**`Host key verification failed`:**
- `DEPLOY_KNOWN_HOSTS` está vacío o desfasado. Re-genera con `ssh-keyscan -H` y reemplaza el secret.

**`sudo: a password is required`:**
- El sudoers de `00-bootstrap.sh` no se aplicó. Verifica `/etc/sudoers.d/cuadra-systemctl` en el server.

**Migraciones fallan con `permission denied for schema public`:**
- El user `cuadra` no tiene permisos. Re-correr `01-create-db.sh` (es idempotente y rota el GRANT).

**Tests pasan en local pero no en CI:**
- En CI se aplican migraciones desde cero contra Postgres limpio. Si tu test asume datos, agrega un seed/setup propio.
