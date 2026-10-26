#!/bin/sh
set -eu

# Bind-mounted secrets keep host ownership/permissions, which sshd rejects;
# install copies with the modes it requires.
install -m 600 /secrets/host_ed25519 /etc/ssh/ssh_host_ed25519_key
install -m 644 /secrets/host_ed25519.pub /etc/ssh/ssh_host_ed25519_key.pub
install -d -m 700 -o dev -g dev /home/dev/.ssh
install -m 600 -o dev -g dev /secrets/client_ed25519.pub /home/dev/.ssh/authorized_keys

# Optional password-login user. Password auth stays off for everyone else.
if [ -n "${TARGET_USER:-}" ]; then
  if [ -z "${TARGET_PASSWORD:-}" ]; then
    echo "entrypoint: TARGET_PASSWORD must be set with TARGET_USER" >&2
    exit 1
  fi
  id "$TARGET_USER" >/dev/null 2>&1 || adduser -D -s /bin/sh "$TARGET_USER"
  echo "$TARGET_USER:$TARGET_PASSWORD" | chpasswd -c sha512
  if ! grep -q "^Match User $TARGET_USER\$" /etc/ssh/sshd_config; then
    printf '\nMatch User %s\n    PasswordAuthentication yes\n' "$TARGET_USER" >> /etc/ssh/sshd_config
  fi
fi
unset TARGET_PASSWORD

/usr/sbin/sshd -e
exec node /opt/agent/src/index.js
