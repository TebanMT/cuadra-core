#!/usr/bin/env bash
# 00-bootstrap.sh — primera pasada en un Hetzner CX33 fresh con Ubuntu 24.04.
#
# Qué hace:
#   1. Actualiza el sistema y agrega los repos que necesitamos.
#   2. Instala paquetes: Caddy, PostgreSQL 16, Go, ufw, fail2ban.
#   3. Crea el usuario `cuadra` (sin password, solo SSH key) y le da sudo
#      sin password para systemctl restart cuadra-server.
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
apt-get update
apt-get -y full-upgrade
apt-get -y install \
  ca-certificates curl gnupg lsb-release \
  ufw fail2ban unattended-upgrades \
  postgresql-16 postgresql-contrib-16 \
  build-essential git tmux jq htop \
  rsync

# ── 2. Caddy (HTTPS reverse proxy con cert auto) ───────────────────
if ! command -v caddy >/dev/null; then
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
    | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
    | tee /etc/apt/sources.list.d/caddy-stable.list
  apt-get update
  apt-get -y install caddy
fi

# ── 3. Go (para compilar cuadra-server in-place o si scp builds) ───
GO_VERSION="1.25.3"
if ! command -v go >/dev/null || [[ "$(go version)" != *"go${GO_VERSION}"* ]]; then
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go
  tar -C /usr/local -xzf /tmp/go.tgz
  rm /tmp/go.tgz
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
fi

# ── 4. Usuario `cuadra` ─────────────────────────────────────────────
if ! id -u cuadra >/dev/null 2>&1; then
  useradd -m -s /bin/bash cuadra
  usermod -aG sudo cuadra
fi

# Copiar la SSH key del root para que el deploy pueda hacer ssh cuadra@host.
mkdir -p /home/cuadra/.ssh
chmod 700 /home/cuadra/.ssh
if [ -f /root/.ssh/authorized_keys ]; then
  cp /root/.ssh/authorized_keys /home/cuadra/.ssh/authorized_keys
  chmod 600 /home/cuadra/.ssh/authorized_keys
fi
chown -R cuadra:cuadra /home/cuadra/.ssh

# Sudoers: que `cuadra` pueda restartar el service sin password (útil para
# el script de deploy posterior).
cat > /etc/sudoers.d/cuadra-systemctl <<'EOF'
cuadra ALL=(root) NOPASSWD: /bin/systemctl start cuadra-server, /bin/systemctl stop cuadra-server, /bin/systemctl restart cuadra-server, /bin/systemctl status cuadra-server, /bin/systemctl reload caddy, /bin/systemctl restart caddy
EOF
chmod 440 /etc/sudoers.d/cuadra-systemctl

# ── 5. Firewall (ufw) ──────────────────────────────────────────────
ufw --force reset
ufw default deny incoming
ufw default allow outgoing
ufw allow OpenSSH
ufw allow 80/tcp comment "HTTP (Caddy redirect a HTTPS)"
ufw allow 443/tcp comment "HTTPS (Caddy → cuadra-server)"
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

# ── 10. Directorios para cuadra ────────────────────────────────────
install -d -o cuadra -g cuadra -m 755 /var/lib/cuadra/uploads
install -d -o cuadra -g cuadra -m 755 /var/log/cuadra
install -d -o cuadra -g cuadra -m 755 /opt/cuadra/bin
install -d -o cuadra -g cuadra -m 755 /opt/cuadra/migrations

echo ""
echo "✓ Bootstrap completo."
echo ""
echo "Próximo paso (en tu laptop):"
echo "  bash 01-create-db.sh   # crea Postgres user + database"
echo "  bash 02-deploy-binary.sh   # build local + scp + systemd start"
echo ""
