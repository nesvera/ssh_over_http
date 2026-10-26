FROM node:22-alpine

# openssh pulls in sshd, scp and sftp-server (used by `sshtun cp`).
# The dev account gets "*" instead of a locked "!" password so sshd accepts
# key logins while password logins remain impossible.
RUN apk add --no-cache openssh \
 && adduser -D -s /bin/sh dev \
 && sed -i -E 's/^dev:!+:/dev:*:/' /etc/shadow

WORKDIR /opt/agent
COPY agent/package.json agent/package-lock.json ./
RUN npm ci --omit=dev
COPY agent/src ./src

COPY sshd_config /etc/ssh/sshd_config
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
