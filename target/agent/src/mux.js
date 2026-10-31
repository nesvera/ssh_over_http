'use strict';

// Agent side of the multiplexing protocol. Wire format and semantics are
// documented in proxy/internal/mux/frame.go and must stay in sync with it.

const net = require('net');
const WebSocket = require('ws');

const FRAME = {
  OPEN: 0x01,
  OPEN_OK: 0x02,
  OPEN_FAIL: 0x03,
  DATA: 0x04,
  CLOSE: 0x05,
  WINDOW: 0x06,
};

const HEADER_SIZE = 5;
const MAX_PAYLOAD = 32 * 1024;
const INITIAL_WINDOW = 2 * 1024 * 1024;
const MAX_STREAMS = 64;

function encode(type, id, payload) {
  const len = payload ? payload.length : 0;
  const buf = Buffer.allocUnsafe(HEADER_SIZE + len);
  buf.writeUInt8(type, 0);
  buf.writeUInt32BE(id, 1);
  if (len) payload.copy(buf, HEADER_SIZE);
  return buf;
}

function u32(n) {
  const b = Buffer.allocUnsafe(4);
  b.writeUInt32BE(n, 0);
  return b;
}

class Mux {
  constructor(ws, { targetHost, defaultPort, allowedPorts, log }) {
    this.ws = ws;
    this.targetHost = targetHost;
    this.defaultPort = defaultPort; // for OPEN without a port: the SSH server
    this.allowedPorts = allowedPorts;
    this.log = log;
    this.streams = new Map();

    ws.on('message', (data, isBinary) => this.onMessage(data, isBinary));
    ws.on('close', () => this.destroyAll());
  }

  send(type, id, payload) {
    if (this.ws.readyState === WebSocket.OPEN) this.ws.send(encode(type, id, payload));
  }

  onMessage(data, isBinary) {
    if (!isBinary || data.length < HEADER_SIZE) {
      this.log('malformed frame, dropping connection');
      this.ws.close(1002, 'malformed frame');
      return;
    }
    const type = data[0];
    const id = data.readUInt32BE(1);
    const payload = data.subarray(HEADER_SIZE);
    const stream = this.streams.get(id);

    switch (type) {
      case FRAME.OPEN:
        if (payload.length !== 4 && payload.length !== 6) {
          this.send(FRAME.OPEN_FAIL, id, Buffer.from('malformed OPEN'));
          break;
        }
        this.open(id, payload.readUInt32BE(0), payload.length === 6 ? payload.readUInt16BE(4) : this.defaultPort);
        break;
      case FRAME.DATA:
        if (stream) stream.onData(payload);
        break;
      case FRAME.WINDOW:
        if (stream && payload.length === 4) stream.onWindow(payload.readUInt32BE(0));
        break;
      case FRAME.CLOSE:
        if (stream) stream.onRemoteClose();
        break;
      default:
        this.log(`stream ${id}: ignoring unknown frame type ${type}`);
    }
  }

  open(id, sendWindow, port) {
    if (!this.allowedPorts.has(port)) {
      this.log(`stream ${id}: refused port ${port}`);
      this.send(FRAME.OPEN_FAIL, id, Buffer.from(`port ${port} not allowed`));
      return;
    }
    if (this.streams.has(id)) {
      this.send(FRAME.OPEN_FAIL, id, Buffer.from('stream id in use'));
      return;
    }
    if (this.streams.size >= MAX_STREAMS) {
      this.send(FRAME.OPEN_FAIL, id, Buffer.from('too many streams'));
      return;
    }
    this.streams.set(id, new Stream(this, id, sendWindow, port));
  }

  destroyAll() {
    for (const stream of this.streams.values()) stream.destroy();
    this.streams.clear();
  }
}

class Stream {
  constructor(mux, id, sendWindow, port) {
    this.mux = mux;
    this.id = id;
    this.sendWindow = sendWindow; // credit we have with the proxy, as it advertised
    this.recvWindow = INITIAL_WINDOW; // credit the proxy has with us, as we advertise
    this.pending = []; // socket chunks waiting for send window
    this.connected = false;
    this.localEnded = false;
    this.done = false;

    const socket = net.connect({ host: mux.targetHost, port });
    this.socket = socket;
    socket.setNoDelay(true);

    socket.on('connect', () => {
      this.connected = true;
      mux.log(`stream ${id}: opened to port ${port}`);
      mux.send(FRAME.OPEN_OK, id, u32(INITIAL_WINDOW));
    });
    socket.on('data', (chunk) => {
      if (this.done) return;
      this.pending.push(chunk);
      this.flush();
    });
    socket.on('end', () => {
      this.localEnded = true;
      this.flush();
    });
    socket.on('error', (err) => {
      if (!this.connected) {
        mux.send(FRAME.OPEN_FAIL, id, Buffer.from(err.message));
        this.finish(false);
      } else {
        mux.log(`stream ${id}: socket error: ${err.message}`);
      }
    });
    socket.on('close', () => {
      // An abrupt close discards anything still waiting for window.
      if (this.connected) this.finish(true);
    });
  }

  // Send queued socket data as far as the proxy's window allows.
  flush() {
    if (this.done) return;
    while (this.pending.length && this.sendWindow > 0) {
      const head = this.pending[0];
      const n = Math.min(head.length, this.sendWindow, MAX_PAYLOAD);
      this.mux.send(FRAME.DATA, this.id, head.subarray(0, n));
      this.sendWindow -= n;
      if (n === head.length) this.pending.shift();
      else this.pending[0] = head.subarray(n);
    }
    if (this.pending.length) {
      this.socket.pause();
    } else if (this.localEnded) {
      this.finish(true);
    } else {
      this.socket.resume();
    }
  }

  onWindow(n) {
    this.sendWindow += n;
    this.flush();
  }

  onData(payload) {
    if (payload.length > this.recvWindow) {
      this.mux.log(`stream ${this.id}: flow control violation`);
      this.finish(true);
      return;
    }
    this.recvWindow -= payload.length;
    const n = payload.length;
    // Return credit only once the bytes have been handed to sshd, so a slow
    // consumer applies backpressure all the way to the client.
    this.socket.write(payload, () => {
      if (this.done) return;
      this.recvWindow += n;
      this.mux.send(FRAME.WINDOW, this.id, u32(n));
    });
  }

  onRemoteClose() {
    this.finish(false);
  }

  // Tear down the stream once; notify tells the proxy we closed it.
  finish(notify) {
    if (this.done) return;
    this.done = true;
    this.mux.streams.delete(this.id);
    if (notify) this.mux.send(FRAME.CLOSE, this.id);
    // end() lets queued writes to sshd drain before the FIN.
    this.socket.end();
    if (this.connected) this.mux.log(`stream ${this.id}: closed`);
  }

  destroy() {
    this.done = true;
    this.socket.destroy();
  }
}

module.exports = { Mux, FRAME, HEADER_SIZE, MAX_PAYLOAD, INITIAL_WINDOW };
