#!/usr/bin/env bash
# 00-bootstrap.sh — primera pasada en un Hetzner CX33 fresh con Ubuntu 24.04.
#
# Qué hace:
#   1. Actualiza el sistema y agrega los repos que necesitamos.
#   2. Instala paquetes: Caddy, PostgreSQL 16, Go, ufw, fail2ban.
#   3. Crea el usuario `tinta` (sin password, solo SSH key) y le da sudo
#      sin password para systemctl restart tinta-server.
#   4. Configura ufw (firewall): SSH/22, HTTP/80, HTTPS/443.
#   5. Endurece SSH: deshabilita login root + password auth.
#
# Asume que se ejecuta como root vía:
#
#   ssh root@<server-ip>
#   bash 00-bootstrap.sh
#
# Idempotente — re-ejecutarlo es seguro.

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "Tiene que correr como root." >&2
  exit 1
fi

# ── 1. Sistema base ─────────────────────────────────────────────────
# Esperar a que cualquier `unattended-upgrades` que arrancó al boot
# termine — sino el `apt-get install` siguiente puede competir por el
# lock y dejar paquetes a medias.
while pgrep -x unattended-upgrade >/dev/null 2>&1; do
  echo "→ esperando que termine unattended-upgrades..."
  sleep 5
done

apt-get update
apt-get -y full-upgrade

# Instalación en una pasada. Dividida visualmente por categoría para
# debug si algún paquete falta en la distribución elegida.
apt-get -y install \
  ca-certificates curl gnupg lsb-release \
  ufw fail2ban unattended-upgrades \
  postgresql postgresql-contrib \
  build-essential git tmux jq htop \
  rsync

# Sanity check post-install — `apt-get -y install` con muchos paquetes
# puede devolver 0 aunque uno específico haya fallado. Verificar que los
# críticos quedaron bien antes de seguir.
id postgres >/dev/null 2>&1 || {
  echo "::error:: PostgreSQL no se instaló — el user 'postgres' no existe."
  echo "Reintentando install explícito..."
  apt-get update
  apt-get -y install postgresql postgresql-contrib
  id postgres >/dev/null
}
command -v psql >/dev/null || {
  echo "::error:: cliente psql no disponible después del install."
  exit 1
}
echo "✓ Postgres listo: $(psql --version | head -1)"

# ── 2. Caddy (HTTPS reverse proxy con cert auto) ───────────────────
# Re-instala si el binario falta O si /etc/caddy no existe (señal de
# install corrupto del paso anterior). Idempotente — si ya está bien,
# no hace nada destructivo.
if ! command -v caddy >/dev/null 2>&1 || [ ! -d /etc/caddy ]; then
  apt-get -y install debian-keyring debian-archive-keyring apt-transport-https

  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
    | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg

  # `> file` (no `tee`) para sobrescribir limpio cualquier basura previa.
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
    > /etc/apt/sources.list.d/caddy-stable.list

  apt-get update
  apt-get -y install caddy

  # Sanity check — si esto falla, abortamos. Mejor que un 404 silencioso
  # cuando el primer deploy intente plantar el Caddyfile.
  command -v caddy >/dev/null
  test -d /etc/caddy
  echo "✓ Caddy instalado: $(caddy version)"
fi

# ── 3. Go (para compilar tinta-server in-place o si scp builds) ───
GO_VERSION="1.25.3"
if ! command -v go >/dev/null || [[ "$(go version)" != *"go${GO_VERSION}"* ]]; then
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go
  tar -C /usr/local -xzf /tmp/go.tgz
  rm /tmp/go.tgz
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
fi

# ── 4. Usuario `tinta` ─────────────────────────────────────────────
if ! id -u tinta >/dev/null 2>&1; then
  useradd -m -s /bin/bash tinta
  usermod -aG sudo tinta
fi

# Copiar la SSH key del root para que el deploy pueda hacer ssh tinta@host.
mkdir -p /home/tinta/.ssh
chmod 700 /home/tinta/.ssh
if [ -f /root/.ssh/authorized_keys ]; then
  cp /root/.ssh/authorized_keys /home/tinta/.ssh/authorized_keys
  chmod 600 /home/tinta/.ssh/authorized_keys
fi
chown -R tinta:tinta /home/tinta/.ssh

# Sudoers: que `tinta` pueda restartar el service sin password (útil para
# el script de deploy posterior).
cat > /etc/sudoers.d/tinta-systemctl <<'EOF'
tinta ALL=(root) NOPASSWD: /bin/systemctl start tinta-server, /bin/systemctl stop tinta-server, /bin/systemctl restart tinta-server, /bin/systemctl status tinta-server, /bin/systemctl reload caddy, /bin/systemctl restart caddy
EOF
chmod 440 /etc/sudoers.d/tinta-systemctl

# ── 5. Firewall (ufw) ──────────────────────────────────────────────
ufw --force reset
ufw default deny incoming
ufw default allow outgoing
ufw allow OpenSSH
ufw allow 80/tcp comment "HTTP (Caddy redirect a HTTPS)"
ufw allow 443/tcp comment "HTTPS (Caddy → tinta-server)"
ufw --force enable

# ── 6. SSH hardening ───────────────────────────────────────────────
sed -i \
  -e 's/^#\?PermitRootLogin .*/PermitRootLogin prohibit-password/' \
  -e 's/^#\?PasswordAuthentication .*/PasswordAuthentication no/' \
  -e 's/^#\?ChallengeResponseAuthentication .*/ChallengeResponseAuthentication no/' \
  -e 's/^#\?KbdInteractiveAuthentication .*/KbdInteractiveAuthentication no/' \
  /etc/ssh/sshd_config
systemctl reload ssh

# ── 7. fail2ban (auto-ban brute-force a SSH) ───────────────────────
systemctl enable --now fail2ban

# ── 8. Unattended security upgrades ────────────────────────────────
dpkg-reconfigure -f noninteractive unattended-upgrades

# ── 9. Postgres listo para crear la DB ─────────────────────────────
systemctl enable --now postgresql
echo "Postgres status:"
sudo -u postgres psql -c '\conninfo'

# ── 10. Directorios para tinta ─────────────────────────────────────
install -d -o tinta -g tinta -m 755 /var/lib/tinta/uploads
install -d -o tinta -g tinta -m 755 /var/log/tinta
install -d -o tinta -g tinta -m 755 /opt/tinta/bin
install -d -o tinta -g tinta -m 755 /opt/tinta/migrations

echo ""
echo "✓ Bootstrap completo."
echo ""
echo "Próximo paso (en tu laptop):"
echo "  bash 01-create-db.sh   # crea Postgres user + database"
echo "  bash 02-deploy-binary.sh   # build local + scp + systemd start"
echo ""
