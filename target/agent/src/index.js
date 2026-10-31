'use strict';

// Device agent: keeps one WebSocket open to the proxy and bridges each
// multiplexed stream to a local TCP port: the SSH server unless the stream
// names another port, which must be listed in ALLOWED_PORTS.

const fs = require('fs');
const WebSocket = require('ws');
const { Mux, HEADER_SIZE, MAX_PAYLOAD } = require('./mux');

const config = {
  proxyUrl: required('PROXY_URL'),
  deviceId: required('DEVICE_ID'),
  token: required('AGENT_TOKEN'),
  caFile: process.env.CA_FILE || '',
  sshHost: process.env.SSH_HOST || '127.0.0.1',
  sshPort: port(process.env.SSH_PORT || '22', 'SSH_PORT'),
};
// Only the SSH server by default; other services are opt-in per device.
config.allowedPorts = new Set(
  (process.env.ALLOWED_PORTS || String(config.sshPort)).split(',').map((p) => port(p.trim(), 'ALLOWED_PORTS')),
);

// The proxy pings every 20s; assume the link is dead after missing a few.
const HEARTBEAT_TIMEOUT_MS = 60_000;
const MAX_BACKOFF_MS = 30_000;

function required(name) {
  const v = process.env[name];
  if (!v) {
    console.error(`agent: ${name} must be set`);
    process.exit(1);
  }
  return v;
}

function port(s, name) {
  const n = Number(s);
  if (!/^[0-9]+$/.test(s) || n < 1 || n > 65535) {
    console.error(`agent: ${name}: invalid port ${JSON.stringify(s)}`);
    process.exit(1);
  }
  return n;
}

function log(msg) {
  console.log(`${new Date().toISOString()} agent[${config.deviceId}]: ${msg}`);
}

let backoff = 1000;

function connect() {
  const url = new URL('agent', config.proxyUrl.endsWith('/') ? config.proxyUrl : `${config.proxyUrl}/`);
  url.searchParams.set('device', config.deviceId);

  const ws = new WebSocket(url, {
    headers: { Authorization: `Bearer ${config.token}` },
    ca: config.caFile ? fs.readFileSync(config.caFile) : undefined,
    maxPayload: HEADER_SIZE + MAX_PAYLOAD,
    perMessageDeflate: false,
  });

  let heartbeat;
  const resetHeartbeat = () => {
    clearTimeout(heartbeat);
    heartbeat = setTimeout(() => {
      log('heartbeat timeout, reconnecting');
      ws.terminate();
    }, HEARTBEAT_TIMEOUT_MS);
  };

  ws.on('open', () => {
    const ports = [...config.allowedPorts].join(',');
    log(`connected to ${url.origin}, forwarding to ${config.sshHost} ports ${ports} (ssh ${config.sshPort})`);
    backoff = 1000;
    resetHeartbeat();
    new Mux(ws, {
      targetHost: config.sshHost,
      defaultPort: config.sshPort,
      allowedPorts: config.allowedPorts,
      log,
    });
  });
  ws.on('ping', resetHeartbeat);
  ws.on('error', (err) => log(`connection error: ${err.message || err.code}`));
  ws.on('close', (code) => {
    clearTimeout(heartbeat);
    log(`disconnected (code ${code}), retrying in ${backoff / 1000}s`);
    setTimeout(connect, backoff);
    backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
  });
}

connect();
