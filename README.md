# SSH over WebSocket

A prototype for reaching SSH servers on devices that can only make
**outbound** HTTPS connections, for example devices behind NAT or a firewall
that nobody can connect to directly.

```
 client-net                         device-net (internal)
┌────────────┐  WSS /connect   ┌─────────┐  WSS /agent (one socket,   ┌──────────────────────────┐
│ client     │ ──────────────▶ │  proxy  │ ◀──────────────────────────│ target                   │
│ sshtun (Go)│  1 per command  │  (Go)   │   many multiplexed streams)│ agent (Node) ─▶ sshd :22 │
└────────────┘                 └─────────┘                            └──────────────────────────┘
```

The idea in one paragraph: the device runs a small **agent** that connects
*out* to a **proxy** and keeps that WebSocket open. When a user runs
**`sshtun`**, it connects to the same proxy and asks for a device by name. The
proxy carves a new stream out of that device's WebSocket and joins the two.
SSH then runs end to end, from `sshtun` to the device's sshd, through that
path. The proxy and agent only move encrypted bytes; they never see what's
inside.

## Quick start

```sh
make up          # generates secrets/ and .env, builds and starts the 3 containers
make devices     # -> dev1
make shell       # interactive shell on dev1 as user "dev"
make down
```

More usage examples are in [Using it](#using-it).

## Layout

```
client/            sshtun, the user-facing CLI (Go module)
  main.go          flags, environment variables, command dispatch
  tunnel.go        opens the WebSocket to the proxy
  ssh.go           SSH handshake: host key check, key/password auth
  session.go       shell and exec commands
  cp.go            file copy over SFTP
  iface*.go        `tun`: virtual interface + userspace TCP/IP stack
proxy/             the relay server (Go module)
  main.go          flags, tokens, TLS
  internal/proxy/  HTTP endpoints, device registry, piping
  internal/mux/    the multiplexing protocol (Go side)
target/            the device (Docker image)
  Dockerfile, entrypoint.sh, sshd_config
  agent/src/index.js   connection to the proxy: connect, heartbeat, reconnect
  agent/src/mux.js     the multiplexing protocol (JavaScript side)
docker-compose.yml, Makefile, scripts/gen-secrets.sh   shared dev environment
```

Each folder builds on its own (`docker build ./proxy`, and so on).

---

# How it works

## The life of one `sshtun shell dev1`

Assume the target container has started and its agent is already connected to
the proxy.

1. **The agent is waiting.** At startup the agent opened
   `wss://proxy:8443/agent?device=dev1` with its agent token. The proxy
   recorded "dev1 is reachable through this WebSocket". That one socket stays
   open for as long as the device is up.
2. **The user runs `sshtun shell dev1`.** sshtun opens its own WebSocket to
   `wss://proxy:8443/connect?device=dev1`, with its client token.
3. **The proxy opens a stream.** It checks the client token, finds dev1's
   agent socket, and sends an `OPEN` frame for a new stream ID, say 7, over
   that socket.
4. **The agent connects to sshd.** It opens a plain TCP connection to
   `127.0.0.1:22` inside the device and replies `OPEN_OK` for stream 7.
5. **The proxy joins the two.** It accepts sshtun's WebSocket and copies bytes
   both ways: bytes from sshtun become `DATA` frames for stream 7 on the agent
   socket, and stream 7's `DATA` frames go back to sshtun.
6. **SSH runs end to end.** sshtun and sshd now perform a normal SSH
   handshake across that path. sshtun checks the device's host key, logs in
   with a key or password, and opens a shell. Everything from here on is
   encrypted between sshtun and sshd.
7. **Closing.** When the user types `exit`, sshd closes its connection. The
   agent sends `CLOSE` for stream 7, the proxy closes sshtun's WebSocket, and
   sshtun exits. The agent's own WebSocket stays open for the next session.

Six `sshtun` commands running at once are simply six streams (six IDs) on
the same agent WebSocket.

## What travels over the wire

The SSH bytes are wrapped in layers, a bit like envelopes inside envelopes:

```
             sshtun ⇄ proxy                      proxy ⇄ agent
┌───────────────────────────────┐   ┌───────────────────────────────────────┐
│ TLS (HTTPS, port 8443)        │   │ TLS (HTTPS, port 8443)                │
│ ┌───────────────────────────┐ │   │ ┌───────────────────────────────────┐ │
│ │ WebSocket binary messages │ │   │ │ WebSocket binary messages         │ │
│ │ ┌───────────────────────┐ │ │   │ │ ┌───────────────────────────────┐ │ │
│ │ │ SSH (encrypted,       │ │ │   │ │ │ mux frame: type | stream id | │ │ │
│ │ │ sshtun ⇄ sshd)        │ │ │   │ │ │   SSH bytes (same as left)    │ │ │
│ │ └───────────────────────┘ │ │   │ │ └───────────────────────────────┘ │ │
│ └───────────────────────────┘ │   │ └───────────────────────────────────┘ │
└───────────────────────────────┘   └───────────────────────────────────────┘
```

The proxy unwraps TLS and WebSocket on both sides and adds or removes the
small mux frame header. It cannot open the innermost SSH layer.

---

## Client: `sshtun` (`client/`)

`sshtun` is a normal SSH client (`golang.org/x/crypto/ssh`) that uses a
WebSocket where `ssh` would use a TCP connection.

**Configuration** (`main.go`). Every flag also has an environment variable:
`-proxy`/`SSHTUN_PROXY`, `-token`/`SSHTUN_TOKEN`, `-ca`/`SSHTUN_CA`,
`-user`/`SSHTUN_USER`, `-i`/`SSHTUN_IDENTITY`, and
`-known-hosts`/`SSHTUN_KNOWN_HOSTS`. In the client container they're all set
by `docker-compose.yml`, so commands work without flags. `run()` dispatches
to `devices`, `shell`, `exec`, `cp` or `tun`.

**Opening the tunnel** (`tunnel.go`, `dialTunnel`). Connects to
`/connect?device=<id>` with `Authorization: Bearer <token>`, trusting the CA
from `-ca`. If the proxy says no, sshtun shows why: 401 for a bad token, 404
when the device isn't connected, 502 when the device's sshd can't be reached.
The WebSocket is then wrapped as a `net.Conn` (`websocket.NetConn`), so the
SSH library sees an ordinary network connection. That wrapping is the whole
trick on the client side.

**The SSH handshake** (`ssh.go`, `connect`).
- *Is this really dev1?* The device's host key must match the entry for
  `dev1` in `known_hosts`. Because this check runs end to end, even a
  compromised proxy couldn't impersonate the device. A mismatch stops the
  connection with `key mismatch`.
- *Who am I?* `authMethods` tries the identity key first, if the file exists.
  If the server still wants a password, it prompts
  `tester@dev1's password:` with up to three attempts. The prompt reads from
  `/dev/tty`, not stdin, so it works even while stdin carries data (as in
  `exec` with piped input, or `cp`). `SSHTUN_PASSWORD` skips the prompt for
  scripts.

**Commands** (`session.go`, `cp.go`). All of them use the SSH client returned
by `connect`. SSH carries several *channels* inside one connection, so these
are simply different channel types:
- `shell` asks for a terminal (PTY), switches your terminal to raw mode so
  keys like Ctrl-C go to the remote side, and forwards window resizes.
- `exec` runs one command and exits with the remote command's exit code.
- `cp` starts the SFTP subsystem and copies a single file, with scp-style
  `device:path` syntax and a progress line.
- `devices` is a plain HTTPS `GET /devices`; it doesn't use SSH.

**Virtual interface** (`iface.go`, `tun`). Unlike the other commands, `tun`
doesn't speak SSH itself. It gives the device an IP address on the client
machine, so ordinary `ssh`, `scp`, `rsync` and `ping` can be used directly:

```
ssh dev@10.77.0.2
  │  kernel: route 10.77.0.2 → sshtun0
  ▼
sshtun0 (TUN)  ── raw IP packets ──▶  netstack inside sshtun (pretends to be 10.77.0.2)
                                        │ TCP :22  → accept, then dialTunnel() → /connect → sshd
                                        │ ICMP echo → GET /ping → proxy pings the agent → reply
                                        └ anything else → refused (TCP RST) or dropped
```

- `tun.CreateTUN` (from wireguard-go) creates the interface (`sshtun0`,
  `sshtun1`, … on Linux; `utunN` on macOS), and `configureInterface` gives
  it a point-to-point link: `10.77.0.1` for this machine and `10.77.0.2` for
  the device by default.
- Packets read from the TUN are handed to **gVisor netstack**, a TCP/IP stack
  written in Go that runs inside sshtun and owns the address `10.77.0.2`. It
  completes the TCP handshake, so each SSH connection becomes a normal byte
  stream, which is piped into its own `/connect` WebSocket, exactly like
  `sshtun shell`. The connection is accepted only once the tunnel is up, so an
  unreachable device shows as "connection refused".
- **IP packets never cross the tunnel.** The proxy and agent see the same
  streams as always, so the device needs no TUN, no privileges and no
  protocol change.
- ICMP echo is intercepted before netstack (which would otherwise answer it
  locally). It's answered only after the proxy round-trips a ping to the
  agent, so `ping` shows real reachability and latency up to the agent. It
  doesn't check sshd itself.
- SSH authentication and host key checking are done by your `ssh` client,
  end to end. Its `known_hosts` entry is keyed by `10.77.0.2`, not `dev1`.
- Ctrl-C closes the TUN, which removes the interface, address and route.

## Proxy (`proxy/`)

The proxy has two jobs: keep track of which devices are connected, and pair
each client connection with a stream to the right device.

**Startup** (`main.go`). Reads `AGENT_TOKENS` (`dev1=<token>,dev2=<token>`,
one token per device) and `CLIENT_TOKENS` (a list of tokens; any of them may
reach any device for now). It serves HTTPS when given `-cert`/`-key`.

**Endpoints** (`internal/proxy/proxy.go`):

| Endpoint | Used by | What it does |
|---|---|---|
| `GET /agent?device=dev1` | agent | Checks that device's token, upgrades to WebSocket, and registers it |
| `GET /connect?device=dev1` | sshtun | Checks the client token, opens a stream to the device, and pipes bytes |
| `GET /devices` | sshtun | Lists connected device IDs as JSON |
| `GET /ping?device=dev1` | sshtun `tun` | Round-trips a WebSocket ping to the agent: 204, 404 if not connected, 504 if it doesn't answer |
| `GET /healthz` | monitoring | Returns `ok` |

**The device registry** (`register`, `unregister`, `lookup`). A map from
device ID to that device's live WebSocket session. If a device reconnects
while an old connection is still registered, the new one replaces it and the
old one is closed. The proxy pings every agent every 20 seconds
(`keepalive`). If a ping goes unanswered for 10 seconds, it drops the session
and removes the device.

**Pairing a client with a device** (`handleConnect`):
1. Check the client token (401 if invalid).
2. Look up the device (404 if not connected).
3. Open a stream to the device and wait up to 10 seconds for `OPEN_OK` (502
   if the agent can't reach sshd). This happens *before* accepting the
   client's WebSocket, so failures reach sshtun as clear HTTP errors.
4. Accept the client's WebSocket and `pipe()` bytes both ways until either
   side closes, then close both.

**The multiplexer** (`internal/mux/`). `Session` wraps one agent WebSocket.
Its `Run` loop reads every frame from the agent and hands it to the right
`Stream` by ID. A `Stream` behaves like a normal connection: it has `Read`,
`Write` and `Close`. That's why `pipe()` can treat it exactly like the
client's WebSocket.
- `Write` splits data into `DATA` frames of up to 32 KiB, and waits whenever
  the agent hasn't granted enough credit (see [Flow control](#flow-control)).
- `Read` returns buffered data from the agent and sends credit back in
  `WINDOW` frames, batched rather than one per read.

**What the proxy can and can't see.** It knows which client token connected,
to which device, when, and how many bytes moved. It can't see commands,
output, file contents or passwords: those are inside SSH's encryption.

## Target (`target/`)

The target container runs two programs: **sshd** and the **agent**. The
agent never reads SSH traffic. It's a byte bridge: for each stream the proxy
opens, it opens a local TCP connection to sshd and copies bytes in both
directions.

**Building the image** (`Dockerfile`). Starts from `node:22-alpine`, installs
`openssh` (sshd plus the SFTP server that `sshtun cp` uses), creates the
`dev` user, and installs the agent's single dependency, the `ws` WebSocket
library. Alpine creates accounts as locked (`!` in `/etc/shadow`), which sshd
refuses even for key logins. The Dockerfile changes it to `*`, meaning "no
valid password": key login works and password login can't.

**Container startup** (`entrypoint.sh`), each time the container starts:
1. **Install the keys.** Copies the host key and `dev`'s `authorized_keys`
   from the mounted `/secrets` into place with strict permissions. Mounted
   files keep the host machine's permissions, and sshd refuses keys that look
   readable by others.
2. **Create the password user** (`TARGET_USER`, default `tester`) if missing,
   set its password from `TARGET_PASSWORD` (stored as a SHA-512 hash), and add
   a `Match User` block so password login is allowed for that user only.
3. **Remove the password from the environment** (`unset TARGET_PASSWORD`).
4. **Start sshd** in the background.
5. **Replace itself with the agent** (`exec node ...`). The agent becomes
   process 1, so `docker stop` signals it directly, and it never sees the
   password.

**SSH server settings** (`sshd_config`). `ListenAddress 127.0.0.1` means sshd
only accepts connections from inside the container, so the agent is the only
way in. Key login is on; password login is off except for the `Match` user;
root login is off; `Subsystem sftp internal-sftp` enables file transfer.

**Connecting to the proxy** (`agent/src/index.js`). Settings come from
`PROXY_URL`, `DEVICE_ID`, `AGENT_TOKEN`, and optionally `CA_FILE`, `SSH_HOST`
and `SSH_PORT` (defaults `127.0.0.1:22`). `connect()`:
- **Dials out** to `/agent?device=<id>` with the agent token. Because the
  device makes the connection, this works from behind NAT and firewalls.
- **On open**, creates a `Mux` on the socket. Everything after that happens
  in `mux.js`.
- **Heartbeat:** each ping from the proxy (every 20 s) resets a 60-second
  timer. If pings stop, for example because the network died without closing
  the connection, the agent kills the socket instead of waiting forever.
- **Reconnect:** when the socket closes, it retries after 1 s, then 2, 4, 8,
  and so on up to 30 s. A successful connection resets the delay to 1 s.

**Multiplexing** (`agent/src/mux.js`). `Mux` keeps a map from stream ID to
`Stream` and routes each incoming frame to the right one. It refuses
duplicate IDs and more than 64 streams at once. If the WebSocket closes,
every sshd connection is closed too, so nothing is left behind. For one
`Stream`:
1. **Open.** On `OPEN`, connect to sshd. On success reply `OPEN_OK`; on
   failure, for example when sshd is down, reply `OPEN_FAIL` with the reason.
2. **Client → sshd.** Each `DATA` frame is written to the sshd socket.
3. **sshd → client.** Data from sshd is queued and sent as `DATA` frames, as
   far as the proxy's credit allows.
4. **Close.** If sshd closes, send `CLOSE`. If the proxy sends `CLOSE`, close
   the sshd socket. Teardown runs only once, whichever side closes first.

---

## Multiplexing protocol (proxy ⇄ agent)

Every binary WebSocket message is one frame:
`type (1 byte) | stream id (4 bytes, big-endian) | payload`.

| Type | Name      | Direction     | Meaning | Payload |
|------|-----------|---------------|---------|---------|
| 0x01 | OPEN      | proxy → agent | A client wants a new connection | uint32 receive window |
| 0x02 | OPEN_OK   | agent → proxy | sshd connection is ready | uint32 receive window |
| 0x03 | OPEN_FAIL | agent → proxy | sshd connection failed | UTF-8 reason |
| 0x04 | DATA      | both          | Stream bytes | up to 32 KiB |
| 0x05 | CLOSE     | both          | Stream is finished | none |
| 0x06 | WINDOW    | both          | "You may send N more bytes" | uint32 credit |

The Go implementation is `proxy/internal/mux`; the JavaScript one is
`target/agent/src/mux.js`. They must stay in sync.

### Flow control

Many streams share one WebSocket. Without limits, one fast transfer could
fill memory, or bury a slow interactive session behind megabytes of queued
data. So each direction of each stream has a **credit budget**:

- When a stream opens, each side announces how much it's willing to buffer
  (2 MiB), in `OPEN` and `OPEN_OK`.
- Each `DATA` frame spends credit. With no credit left, the sender stops: the
  proxy's `Write` waits, and the agent stops reading from sshd
  (`socket.pause()`). TCP then naturally slows down whoever is producing the
  data.
- The receiver hands credit back in a `WINDOW` frame only once the data has
  actually been consumed: written to sshd on the agent side, or read by the
  pipe on the proxy side. So a slow reader at either end slows the sender all
  the way back.
- A peer that sends more than its credit is breaking the protocol, and that
  stream is closed.

2 MiB matches SSH's own per-channel window, so the tunnel never limits a
transfer more than SSH itself would. Announcing the budget, rather than
hard-coding it on both sides, lets proxies and agents of different versions
work together.

---

## Security model

| Credential | Presented by → checked by | Proves |
|---|---|---|
| `CLIENT_TOKEN` | sshtun → proxy | This client may use the proxy |
| `AGENT_TOKEN` (per device) | agent → proxy | This agent really is dev1 |
| Proxy TLS certificate (dev CA) | proxy → sshtun and agent | They're talking to the real proxy |
| Device host key (`known_hosts`) | sshd → sshtun, end to end | The SSH server really is dev1, even if the proxy were compromised |
| User key or password | sshtun → sshd, end to end | The user may log in as `dev` or `tester` |

Because the last two checks are end to end, the proxy can route traffic but
can't impersonate a device or read sessions. `scripts/gen-secrets.sh`
creates all of these for the dev environment: a dev CA and proxy
certificate, the client key pair (its public half becomes `dev`'s
`authorized_keys`), the device host key, a `known_hosts` entry pinning it as
`dev1`, and random tokens plus the password user's credentials in `.env`.

Network isolation in `docker-compose.yml` backs this up: the client and
target are on separate networks with the proxy on both, `device-net` has no
internet access, and sshd only listens inside the target. The tunnel is the
only way to reach the device.

## Known risks

This is a prototype. The issues below were found in a security review of the
current code; none of them is fixed yet. Most are acceptable for local
testing, but the high and medium ones should be addressed before any real
deployment.

### High

**Failed logins by one user lock out every user of a device (confirmed).**
Every SSH connection reaches sshd from the agent at `127.0.0.1`, so sshd sees
all users as one machine. OpenSSH 10's per-source penalties (on by default, no
exemptions configured) then block that address after repeated failures. In a
test, 12 wrong passwords for `tester` blocked `dev`, who has a valid key, for
about 20 seconds with `handshake failed: EOF`. Continued failures extend the
block up to 10 minutes. Anyone with a client token, or one faulty script, can
keep a device locked.
*Mitigation:* `PerSourcePenaltyExemptList 127.0.0.1` in `target/sshd_config`,
with rate limiting moved to the proxy, which knows who each client is.

**One shared client token reaches every device and never expires.** All users
share `CLIENT_TOKEN`, it opens a stream to any device, and `/devices` lists
them all. Revoking it means editing `.env` and restarting the proxy, and
sessions already open survive that. The SSH login is still required, but the
token gate is all-or-nothing.
*Mitigation:* per-user credentials with a list of allowed devices, and
short-lived tokens or client certificates (mTLS).

**A stolen agent token can take a device offline.** A second agent connecting
as `dev1` replaces the first (`register` in `proxy/internal/proxy/proxy.go`).
Host-key pinning stops an impostor from capturing any logins, because sshtun
refuses the wrong host key before sending credentials. But the impostor can
keep the real device unreachable, and the two agents would keep displacing
each other.
*Mitigation:* a client certificate per device, and refusing or alerting on a
second connection for the same device.

### Medium

**The agent runs as root.** Node runs as process 1 under `root`, though only
sshd needs root. A bug in the agent or the `ws` library would give root on the
device.
*Mitigation:* start sshd as root, then run the agent as an unprivileged user.

**No resource limits.** The proxy's `http.Server` (`proxy/main.go`) has no
timeouts, so slow clients can hold connections open (slowloris). There are no
per-token connection limits and no idle timeouts. The agent accepts at most
64 streams, so one token holder can occupy all of them, and sshd's
`MaxStartups 10:30:100` limit on not-yet-authenticated connections is also
shared by all users.
*Mitigation:* server timeouts, per-token connection limits and idle timeouts
on the proxy.

**Secrets handling is dev-grade.** The CA private key (`secrets/ca.key`) sits
next to everything else; anyone holding it can mint a proxy certificate,
intercept TLS and steal bearer tokens (SSH sessions stay protected).
`secrets/proxy.key` is world-readable (`644`) so the container's unprivileged
user can read it. `TARGET_PASSWORD` is unset inside the target but still
visible through `docker inspect`. The client private key has no passphrase.
*Mitigation:* keep the CA key offline, and pass secrets as Docker secrets or
files rather than environment variables.

**The proxy allows plain HTTP.** Started without `-cert`, it serves
unencrypted HTTP and the bearer tokens cross the network in clear text.
*Mitigation:* refuse to start without TLS unless an explicit `-insecure` flag
is given.

### Low

- **`sshtun tun` shares access with the whole client machine.** While it
  runs, any local user or process can reach the device's sshd at its address
  without knowing `CLIENT_TOKEN`. If IP forwarding is on, so can other hosts
  that route to it. SSH authentication still applies, but the proxy token is
  no longer a gate, which matters while password login and the lockout issue
  remain. The client container also needs `NET_ADMIN` and `/dev/net/tun`
  (granted in `docker-compose.yml`) for this command only.
- **No audit trail.** sshd logs `127.0.0.1` for every session, and the proxy
  logs the remote address but not which credential was used. Together with
  the shared token, there's no record of who did what.
- **Token visible in the process list.** Passing `-token` on the command line
  exposes it in `ps` to other local users; prefer `SSHTUN_TOKEN`.
- **Password login is weaker than keys.** The client token limits who can
  try, but password guessing only becomes properly bounded once the lockout
  issue is fixed and the proxy enforces rate limits.

## When things go wrong

| Event | What happens |
|---|---|
| Wrong client token | Proxy answers 401; sshtun prints it and exits 1 |
| Device not connected | Proxy answers 404 |
| Device's sshd is down | Agent replies `OPEN_FAIL`; proxy answers 502 |
| Host key doesn't match | sshtun stops before sending any credentials (`key mismatch`) |
| Proxy restarts or the network drops | Open sessions end; the agent reconnects within seconds |
| Network dies silently | Missed pings: the proxy drops the agent within about 30 s, and the agent reconnects within 60 s |
| One client killed mid-transfer | Only that stream closes; other sessions on the same device continue |
| Device offline while `sshtun tun` runs | `ping` gets no replies; `ssh` gets "connection refused"; both recover when the agent reconnects |
| Malformed frame | The side that received it closes the WebSocket; the agent reconnects |

---

## Using it

All commands below run inside the client container
(`docker compose exec -it client sh`), where the configuration is already set.

```sh
sshtun devices                         # list connected devices
sshtun shell dev1                      # interactive shell as "dev"
sshtun exec dev1 uname -a              # run one command
sshtun exec dev1 'exit 3'; echo $?     # -> 3, the remote exit code
```

### Copying files

`sshtun cp` copies a single file in either direction with scp-style paths. It
runs SFTP inside the tunnel, which is also what modern OpenSSH `scp` uses.

```sh
sshtun cp app.tar dev1:/tmp/          # into a directory, keeps the file name
sshtun cp app.tar dev1:/tmp/new.tar   # explicit name
sshtun cp app.tar dev1:               # into the remote user's home
sshtun cp dev1:/tmp/app.tar .         # download
```

Permission bits are carried over to newly created files. When stderr is a
terminal, a progress line shows percent, bytes and rate. Directories aren't
copied directly; tar them first. An interrupted copy leaves a partial file
under the destination name, so rerun the copy before using it.

### Password login

Besides the key-only `dev` user, the target creates a password user at startup
from `TARGET_USER` / `TARGET_PASSWORD` in `.env` (default user `tester`, with a
random password). sshd allows password authentication for that user only.

```sh
grep TARGET_PASSWORD .env                      # the generated password
docker compose exec -it client sshtun -user tester shell dev1
tester@dev1's password:
```

`sshtun` tries the identity key first (skipped if the file is missing; `-i ''`
disables it), then prompts for a password on the terminal, with up to three
attempts. For scripts, `SSHTUN_PASSWORD` supplies the password without a
prompt. To use a different user or password, edit `.env` and run
`docker compose up -d target`.

### Virtual interface

`sshtun tun` gives a device an IP address on your machine for as long as it
runs, so standard tools work against it. It needs root (or `CAP_NET_ADMIN`).

```sh
# terminal 1 (inside the client container)
sshtun tun dev1
sshtun0: device dev1 is at 10.77.0.2 (ssh on port 22, ping); Ctrl-C to remove

# terminal 2
ping 10.77.0.2
ssh -i /secrets/client_ed25519 dev@10.77.0.2
scp -i /secrets/client_ed25519 app.tar dev@10.77.0.2:/tmp/
ssh tester@10.77.0.2                       # password user
```

Only port 22 is forwarded; other TCP ports are refused, and UDP and IPv6 are
dropped. For more than one device, give each its own address:
`sshtun tun -ip 10.77.1.2 -local 10.77.1.1 dev2`. On a macOS host, run it with
`sudo`; the interface will be `utunN`.

### From the host machine

The proxy is also published on `localhost:8443`:

```sh
set -a; . ./.env; set +a
(cd client && go build -o ../sshtun .)
./sshtun -proxy wss://localhost:8443 -token "$CLIENT_TOKEN" \
  -ca secrets/ca.crt -i secrets/client_ed25519 -known-hosts secrets/known_hosts \
  -user dev shell dev1
```

### Regenerating secrets

`./scripts/gen-secrets.sh` keeps existing secrets and only adds missing `.env`
entries. `FORCE=1 ./scripts/gen-secrets.sh` regenerates everything; run
`make up` afterwards.

## Tests

```sh
make test
```

`proxy/internal/proxy/proxy_test.go` runs the real Node agent against the
proxy, with a TCP echo server in place of sshd. It checks auth, unknown
devices, `/ping`, and 8 concurrent 4 MiB streams for byte-exact round trips.

## Next steps

- Address the [known risks](#known-risks), starting with the high ones.
- `sshtun cp -r` for directories.
- Supervise sshd in the target container. It's started once by the
  entrypoint, and if it crashes, every new session gets `OPEN_FAIL` until the
  container restarts.
