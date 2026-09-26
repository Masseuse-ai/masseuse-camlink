# masseuse-camlink protocol

Two programs and the service between them:

- **connector** (`masseuse-camlink`): runs on a computer in the same home
  network as the camera. It holds a persistent Ed25519 identity, keeps a
  long-lived event stream to the masseuse.ai service (the *rendezvous*), and,
  when told to, dials one enclave and relays TLS ciphertext between the
  camera and that enclave.
- **gateway** (`masseuse-camlink-gateway`): runs inside the video enclave
  (Confidential Space). It accepts exactly one connector per session, and
  exposes a loopback listener that the enclave's RTSP server connects to as if
  it were the camera.
- **rendezvous**: HTTPS endpoints on the masseuse.ai service that hand the
  phone a pairing code and hand both sides a one-time ticket. It never sees
  camera bytes.

Neither the connector nor the gateway decrypts anything. The camera speaks
RTSPS (TLS) and the enclave terminates that TLS. What the connector carries is
opaque ciphertext; what the service carries is a code, a ticket and an origin.

All identifiers below are encoded as base64url without padding unless stated.
Hashes are SHA-256. Timestamps are Unix seconds unless a name ends in `Ms`.

## 1. Connector identity

On first run the connector generates an Ed25519 key pair and stores it with
mode 0600 in its state directory (`--state-dir`, default
`$XDG_STATE_HOME/masseuse-camlink` or the platform equivalent). The public
key (32 bytes, base64url) is the connector's identity, called `connectorKey`.
The same directory keeps `paired.json`: the SHA-256 (hex) of every phone token
the connector has been paired with.

## 2. Connector <-> rendezvous

Base URL: `https://masseuse.ai` (`--service`).

### 2.1 `POST /api/camlink/hello`

```json
{
  "key": "<connectorKey>",
  "ts": 1757400000,
  "pairedPhones": ["<hex sha256 of phone token>", "..."],
  "version": "v0.1.0",
  "sig": "<base64url Ed25519 signature>"
}
```

`sig` is over the UTF-8 string
`camlink-hello-v1|<ts>|<key>|<pairedPhones joined with ,>`.
The service rejects `|now - ts| > 60 s`, a bad signature, more than 32 paired
phones, and more than 10 hellos per minute per IP (429).

Response `200`:

```json
{
  "streamToken": "<opaque, valid for one event stream>",
  "code": "7QK4-N2PX",
  "codeExpiresAtMs": 1757400600000
}
```

The pairing code is 8 characters from `ABCDEFGHJKMNPQRSTUVWXYZ23456789`
(no 0/O/1/I/L), shown as two groups of four. It rotates every 10 minutes and
after 10 wrong attempts against it.

### 2.2 `GET /api/camlink/events`

`Authorization: Bearer <streamToken>`. Server-sent events; `: keepalive`
comment every 15 s. The connector is *online* while this stream is attached.
One stream per connector: a new stream replaces the previous one.

| event | data | connector action |
|---|---|---|
| `code` | `{"code","expiresAtMs"}` | print the code (also sent on rotation) |
| `paired` | `{"phoneTokenHash"}` | append to `paired.json` |
| `dial` | `{"sessionId","origin","ticket","ticketHash","expiresAtMs"}` | verify the origin's attestation, then open the tunnel (section 4) |
| `clear` | `{"sessionId","reason"}` | close the tunnel to that session |

On any disconnect the connector re-`hello`s with exponential backoff
(1 s .. 60 s, jittered). A `dial` for a session it is already connected to is
idempotent; a `dial` for a different origin replaces the current tunnel.

## 3. Phone <-> rendezvous

Cookie-authenticated session routes of the masseuse.ai service.

- `POST /api/session/:id/camlink/pair` `{"code"}` ->
  `200 {"connectorKey","phoneToken"}`. `phoneToken` is 32 random bytes; the
  service pushes `paired {phoneTokenHash}` to the connector and stores the
  hash for as long as the connector stays online. `404` unknown or expired
  code (5 attempts per minute per session).
- `POST /api/session/:id/camlink` `{"connectorKey","phoneToken"}` -> binds the
  connector to this session: `200 {"online": true|false}`. `403` when the hash
  of `phoneToken` is not among the connector's paired phones.
- `DELETE /api/session/:id/camlink` -> `204`, the service sends `clear`.

While a session is bound to a connector and the session holds an enclave, the
service mints a 32-byte ticket, tells the enclave to expect
`{connectorKey, ticketHash, expiresAt}` (section 5) and sends the connector
`dial`. The session snapshot and its event stream carry
`camlink: {"bound","online","tunnel"}`.

## 4. Connector <-> gateway

The connector dials `wss://<origin>/ingest/tunnel` with
`Authorization: Bearer <ticket>` and the WebSocket subprotocol `camlink.v1`.
The origin's TLS certificate is verified against the public roots **and**
pinned to the SubjectPublicKeyInfo hash that the origin's attestation bound
(`tlsSpkiNonce` in `eat_nonce`), after the attestation token itself has been
verified against the policy published at `<service>/api/tee-policy`
(`internal/attest`).

The gateway checks `sha256(ticket) == ticketHash` and `now < expiresAt`, else
`401`. The WebSocket then carries a single byte stream (binary messages,
`websocket.NetConn`) of frames:

```
type   uint8
stream uint32 big-endian
length uint32 big-endian   (<= 65536)
payload [length]byte
```

| type | name | direction | payload |
|---|---|---|---|
| 1 | OPEN | gateway -> connector | `host:port` the connector should dial |
| 2 | ACCEPT | connector -> gateway | empty |
| 3 | REFUSE | connector -> gateway | reason (UTF-8) |
| 4 | DATA | both | bytes |
| 5 | CLOSE | both | empty; no more DATA from the sender on this stream |
| 6 | RESET | both | reason; abort the stream (stream 0: the whole tunnel) |
| 7 | CHALLENGE | gateway -> connector | 32 random bytes, stream 0 |
| 8 | PROVE | connector -> gateway | Ed25519 signature, stream 0 |
| 9 | READY | gateway -> connector | empty, stream 0 |

Handshake on stream 0: the gateway sends `CHALLENGE`; the connector answers
`PROVE` with a signature over
`camlink-tunnel-v1|<origin host>|<ticketHash hex>|<nonce base64url>` under
its identity key; the gateway verifies it against the `connectorKey` it was
told to expect and sends `READY`, or `RESET` and closes.

Streams are opened by the gateway only (odd ids, starting at 1). The
connector dials the `host:port` in `OPEN` and answers `ACCEPT` or `REFUSE`.
The connector refuses anything that is not the single private-network target
of this tunnel: the first `host:port` it accepts is locked in, and a host is
only accepted when it is an RFC 1918, link-local or loopback literal, or a
name every address of which resolves to such an address (the connector then
dials the address it checked, not the name). It never becomes a general
proxy. The gateway pings every 30 s.

## 5. Enclave control (loopback only)

The gateway's control API listens on `127.0.0.1:8091` inside the enclave and
is called by the pose producer, which in turn is called by the service over
the existing authenticated control plane.

- `POST /expect` `{"connectorKey","ticketHash","expiresAt"}` -> `200`.
  Replaces the current expectation; a connector already attached under a
  different key is dropped.
- `POST /target` `{"host","port"}` -> `200`; `409` when no connector is
  attached. The host:port the next relay connection is opened to.
- `POST /clear` -> `200`: forget expectation and target, drop the connector.
- `GET /status` -> `{"expecting","connected","sinceMs","connectorKeyPrefix","target"}`
  (`target` is a boolean; the host never appears in status or logs).
- `GET /healthz` -> `200 ok`.

The relay listener is `127.0.0.1:7441`. Each accepted connection becomes an
`OPEN` to the attached connector; with no connector attached, or no target
set, the connection is closed immediately. The enclave's RTSP server is
pointed at `rtsps://127.0.0.1:7441/<path>` and verifies the camera's
certificate fingerprint through the tunnel exactly as it would directly.
