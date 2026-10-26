#!/usr/bin/env bash
# Generates dev-only credentials for the docker-compose environment:
#   secrets/ca.crt, proxy.crt/.key  TLS for the proxy (SAN: proxy, localhost)
#   secrets/client_ed25519[.pub]    client SSH key pair (pub -> authorized_keys)
#   secrets/host_ed25519[.pub]      target SSH host key
#   secrets/known_hosts             pins the host key under the device ID
#   .env                            proxy bearer tokens and the target's
#                                   password-login user, read by docker compose
# Re-run with FORCE=1 to regenerate everything. Without it, existing secrets
# are kept and only missing .env entries are added.
set -euo pipefail
cd "$(dirname "$0")/.."

DEVICE_ID=${DEVICE_ID:-dev1}
OUT=secrets

# Appends KEY=VALUE to .env unless KEY is already set there.
ensure_env() {
  grep -q "^$1=" .env 2>/dev/null || echo "$1=$2" >> .env
}

if [[ ${FORCE:-0} == 1 ]]; then
  rm -rf "$OUT" .env
fi

if [[ -f $OUT/ca.crt ]]; then
  echo "secrets/ already exists (FORCE=1 to regenerate)"
else
  mkdir -p "$OUT"

  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 365 \
    -subj "/CN=ssh-over-http dev CA" -keyout "$OUT/ca.key" -out "$OUT/ca.crt" 2>/dev/null

  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -subj "/CN=proxy" -keyout "$OUT/proxy.key" -out "$OUT/proxy.csr" 2>/dev/null
  openssl x509 -req -in "$OUT/proxy.csr" -CA "$OUT/ca.crt" -CAkey "$OUT/ca.key" -CAcreateserial \
    -days 365 -out "$OUT/proxy.crt" \
    -extfile <(printf 'subjectAltName=DNS:proxy,DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n') 2>/dev/null
  rm -f "$OUT/proxy.csr" "$OUT/ca.srl"
  # The proxy container runs as non-root and must read its key.
  chmod 644 "$OUT/proxy.key"

  ssh-keygen -q -t ed25519 -N "" -C "sshtun-client" -f "$OUT/client_ed25519"
  ssh-keygen -q -t ed25519 -N "" -C "$DEVICE_ID-host" -f "$OUT/host_ed25519"
  echo "$DEVICE_ID $(cut -d' ' -f1,2 "$OUT/host_ed25519.pub")" > "$OUT/known_hosts"

  echo "generated $OUT/ for device $DEVICE_ID"
fi

ensure_env DEVICE_ID "$DEVICE_ID"
ensure_env AGENT_TOKEN "$(openssl rand -hex 24)"
ensure_env CLIENT_TOKEN "$(openssl rand -hex 24)"
ensure_env TARGET_USER tester
ensure_env TARGET_PASSWORD "$(openssl rand -hex 8)"
