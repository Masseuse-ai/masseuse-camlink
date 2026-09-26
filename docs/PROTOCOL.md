# masseuse-camlink protocol

Two programs and the service between them:

- **connector** (`masseuse-camlink`): runs on a computer at home. It holds a
  persistent Ed25519 identity, keeps a long-lived event stream to the
  masseuse.ai service (the *rendezvous*), and, when told to, dials one
  enclave and carries one camera's TLS stream to it: its own stream, serving
  the computer's camera and microphone or a camera on the home network
  (section 6), or, when the session names a camera directly, that camera's
  ciphertext relayed untouched.
- **gateway** (`masseuse-camlink-gateway`): runs inside the video enclave
  (Confidential Space). It accepts exactly one connector per session, and
  exposes a loopback listener that the enclave's RTSP server connects to as if
  it were the camera.
- **rendezvous**: HTTPS endpoints on the masseuse.ai service that hand the
  phone a pairing code and hand both sides a one-time ticket. It never sees
  camera bytes.

The gateway decrypts nothing: the enclave's RTSP server terminates the
camera's TLS, whether the camera is a device on the network or the
connector's own stream. What the tunnel carries is opaque ciphertext; what
the service carries is a code, a ticket, an origin and the name of the
camera the connector offers.

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
  "codeExpiresAtMs": 1757400600000,
  "heartbeatEveryMs": 30000
}
```

`heartbeatEveryMs` is how often the connector is to send a heartbeat
(section 2.4) while its event stream is attached; absent means the service
takes no heartbeats.

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
| `clear` | `{"sessionId","reason"}` | close the tunnel to that session; release the stimulation device if it was attached to that session |
| `estim` | `{"sessionId","message"}` | hand `message` to the stimulation device link (section 7); an older connector ignores it |

On any disconnect the connector re-`hello`s with exponential backoff
(1 s .. 60 s, jittered). A `dial` for a session it is already connected to is
idempotent; a `dial` for a different origin replaces the current tunnel.

### 2.3 `POST /api/camlink/source`

After every successful hello, and whenever it changes, the connector reports
the camera it offers through its own stream (section 6):

```json
{
  "key": "<connectorKey>",
  "ts": 1757400000,
  "source": {"kind": "capture", "label": "Insta360 Link + Yeti Stereo Microphone", "ready": true},
  "sig": "<base64url Ed25519 signature>"
}
```

`kind` is `capture` (the computer's own camera and microphone) or `camera` (a
camera on the connector's network the connector pulls and re-serves).
`label` is what the phone shows, at most 64 characters. `ready` says whether
the connector can serve it now (ffmpeg present and the devices found, or the
network camera answering at startup). `sig` is over the UTF-8 string
`camlink-source-v1|<ts>|<key>|<kind>|<1 if ready else 0>|<label>`; the label
comes last so that any character in it is unambiguous. The service applies
the hello rules (`|now - ts| <= 60 s`, signature, rate limit), keeps the
latest report per connector for as long as it remembers the connector, and
shows it to a phone bound to that connector as `camlink.source`. A `404`
means an older service; the connector goes on without the card.

### 2.4 `POST /api/camlink/heartbeat`

The event stream tells the service a connector is online, but not always
that it has gone: a connector that loses power, or its network, leaves a
stream the service's front end may hold open long after. So while the
stream is attached the connector also says so, every `heartbeatEveryMs`
from the hello response:

```json
{
  "key": "<connectorKey>",
  "ts": 1757400000,
  "sig": "<base64url Ed25519 signature>"
}
```

`sig` is over the UTF-8 string `camlink-heartbeat-v1|<ts>|<key>`. The
service applies the hello rules (`|now - ts| <= 60 s`, signature; 30 per
minute per IP). Once a connector has sent one heartbeat, the service marks
it offline when 90 s pass without another, ending its stream and telling
bound phones (`camlink.online` false) exactly as if the stream had closed.
A connector that has never sent a heartbeat (an older connector) is judged
by its stream alone, as before. A `404` means an older service; the
connector stops sending them for that stream.

### 2.5 `POST /api/camlink/estim`

Messages from the stimulation device link (section 7) travel up in this
request; those from the service to the connector travel down as `estim`
events on the stream (section 2.2). Neither direction touches the tunnel,
which carries camera ciphertext and nothing else.

```json
{
  "key": "<connectorKey>",
  "ts": 1757400000,
  "sessionId": "<session id, or \"\" for a connector-level message>",
  "messages": "[{\"type\":\"heartbeat\"},{\"type\":\"device_status\",\"status\":{...}}]",
  "sig": "<base64url Ed25519 signature>"
}
```

`messages` is the JSON text of an array of message objects (section 7.3),
carried as a string so that what was signed is what the service reads, on
any JSON decoder. `sig` is over the UTF-8 string
`camlink-estim-v1|<ts>|<key>|<sessionId>|<messages>`, the string's text
last so that any character in it is unambiguous. The service applies the
hello rules (`|now - ts| <= 60 s`, signature) and allows 120 requests per
minute per connector; the body is at most 64 KiB and the array holds at
most 64 messages. Messages for a session are accepted
only while that session is bound to the connector: `409` says it is not, and
the connector releases the device and detaches. `404` means an older
service that links no devices; the connector keeps the device released and
sends nothing more.

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
- `DELETE /api/session/:id/camlink/estim` -> `204`: the phone's stop. The
  service sends the device link a release and detaches it for this session
  (section 7); the camera link is untouched.

While a session is bound to a connector and the session holds an enclave, the
service mints a 32-byte ticket, tells the enclave to expect
`{connectorKey, ticketHash, expiresAt}` (section 5) and sends the connector
`dial`. The session snapshot and its event stream carry
`camlink: {"bound","online","tunnel","source","estim"}`, `source` being the
connector's latest report (section 2.3) or absent, and `estim` the state of
the stimulation device link (section 7) or absent.

To use the connector's own camera the phone hands the enclave the fixed link
`rtsps://127.0.0.1:7443/camera` the way it would hand it any camera's link;
the enclave treats a loopback host as a tunnel target like any private
address (section 6).

## 4. Connector <-> gateway

The connector dials `wss://<origin>/ingest/tunnel` with
`Authorization: Bearer <ticket>` and the WebSocket subprotocol `camlink.v1`.
The origin's TLS certificate is verified against the public roots **and**
pinned to the SubjectPublicKeyInfo hash that the origin's attestation bound
(`tlsSpkiNonce` in `eat_nonce`), after the attestation token itself has been
verified against the policy published at `<service>/api/tee-policy`
(`internal/attest`). The served policy is applied on top of floors compiled
into the connector (`internal/attest`, `Floors`, `Production`): the token
issuer and key set, the software and hardware model, the image signing key,
the lowest release, the source repository and public registry, the Google
Cloud project and registry path the enclave runs from, and the DNS suffix
its hosts live under. A served policy can tighten any of these (a newer
minimum release, a narrower host suffix, an explicit digest list) and the
connector refuses one that contradicts them, so the service cannot steer a
connector to an enclave the connector's own build does not accept. The
policy fields are:

| field | served | floor |
|---|---|---|
| `issuer`, `jwksUrl`, `swname`, `hwmodel` | must equal the floor | Confidential Space, Intel TDX |
| `imageSignatures` | cut to the keys the floor trusts; none left is refused | the enclave release key |
| `minRelease` | raised to the floor when lower or absent | `v0.4.0` |
| `allowDebug` | always `false` | |
| `requireStable`, `requireGpuCc` | always `true` | |
| `sourceUri`, `imageRepo` | must equal the floor; filled when absent | `github.com/FemLed/masseuse-video-tee`, `ghcr.io/femled/masseuse-video-tee` |
| `teeSlotHostSuffixes` | kept where under the floor; none left is refused | `.tee.masseuse.ai` |
| `projectId` | must equal the floor; filled when absent | `prod-masseuse-video-tee` |
| `imageReferencePrefix` | must be under the floor; filled when absent | the enclave image in that project's Artifact Registry |
| `allowedImageDigests`, `expectedTrainerUrl`, `imageSources` | as served (only ever tighten) | |

Once the token verifies, and before the WebSocket is opened, the connector
checks the attested image's provenance (`internal/provenance`): from
`imageRepo` in the public registry it reads the Sigstore records attached to
`submods.container.image_digest` (OCI referrers, or cosign's
`sha256-<digest>` and `sha256-<digest>.att` tags) and requires, verified
against the Sigstore trust root with sigstore-go, a keyless signature whose
certificate is `https://<sourceUri>/.github/workflows/release.yml@refs/tags/<TEE_IMAGE_VERSION>`
from `https://token.actions.githubusercontent.com` for a run on `sourceUri`
at that tag and `TEE_IMAGE_COMMIT`, and SLSA provenance signed by
`slsa-github-generator`'s `generator_container_slsa3.yml` whose statement
names `git+https://<sourceUri>@refs/tags/<TEE_IMAGE_VERSION>` at that commit
through `.github/workflows/release.yml`. Both must be recorded in Rekor. A
token without a release stamp, a policy without `sourceUri`, or an image
without both records is refused. Verified results are cached for seven days
per digest, release and commit.

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

One target is reserved: `127.0.0.1:7443` is the connector's own stream
(section 6). An `OPEN` for it is answered from inside the connector process,
never by a dial, so nothing has to listen on that port and nothing else on
the computer can reach the stream. It is subject to the same single-target
lock.

## 5. Enclave control (loopback only)

The gateway's control API listens on `127.0.0.1:8091` inside the enclave and
is called by the pose producer, which in turn is called by the service over
the existing authenticated control plane.

- `POST /expect` `{"connectorKey","ticketHash","expiresAt"}` -> `200`.
  Replaces the current expectation. A connector already attached under the
  same key stays attached and keeps its target (the service re-expects on
  every new lease, and the camera has not moved); one attached under a
  different key is dropped, and the target with it.
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

## 6. The connector's own stream

The connector is itself an RTSPS server, reachable only through the tunnel:
the enclave names it with the fixed link `rtsps://127.0.0.1:7443/camera`,
the gateway opens streams to `127.0.0.1:7443`, and the connector answers them
in-process (section 4). Its certificate is self-signed ECDSA P-256, generated
on first run and kept as `camera-cert.pem` (mode 0600) in the state
directory, so its fingerprint is stable across restarts; the enclave probes
it through the tunnel and pins it for the session as it does with any
camera.

What the stream carries is one source, chosen on the command line and
remembered in `source.json`:

- **The computer's camera and microphone** (`capture`). The connector runs
  ffmpeg as a child process: avfoundation on macOS, dshow on Windows, v4l2
  and PulseAudio on Linux; 1280x720 at 30 frames per second by default, H.264
  from the hardware encoder (VideoToolbox, Media Foundation) or libx264, a
  keyframe every two seconds, Opus mono audio. ffmpeg publishes over plain
  RTSP to a loopback port the connector chose, on a path that is a fresh
  random secret, and the connector forwards the packets into its stream
  unchanged. The child runs only while a session is reading: it starts at the
  first `OPEN` for the reserved target and stops when the session ends, so
  the camera light is off between sessions. When the session's tunnel goes
  down while the session lasts (the service re-dials with a new ticket on
  every new lease, and the connector re-dials after a network failure) the
  child keeps running for 15 s, since the relay is back at the stream within
  seconds and a restart would put it back to zero just as the relay's
  `DESCRIBE` arrives; with no stream opened by then it stops. A `DESCRIBE`
  that arrives while ffmpeg is still starting waits for it (up to 12 s, longer
  than the relay's own 10 s patience for the answer, so that the connector is
  never what gives up first; the enclave gives the path 15 s to be ready).
  By default the capture uses the first camera and the first microphone
  listed, passing over an iPhone or iPad joined through Continuity Camera
  when the computer has a device of its own: those open over the air, in
  several seconds and not always.
- **A camera on the network** (`camera`). The connector is an RTSPS client of
  the camera (`-camera-url rtsps://user:password@host:port/path`; the host
  must be a private-network address or a name resolving only to such
  addresses). It pins the camera's certificate by SHA-256: the fingerprint
  given with `-camera-fingerprint`, or the one seen first, saved in
  `cameras.json` and printed. Media passes through; H.264 and H.265 are
  re-packetized so that no packet exceeds 1200 bytes of payload, which the
  stream's server requires. It too is pulled only while a session reads.

The connector describes the source to the service (section 2.3) so the phone
can name it; the phone still decides, and only the phone's capability can
hand the enclave a link.

**Trust.** With a camera named directly, the connector relays ciphertext it
cannot read. With its own stream, the connector holds the picture in the
clear on the person's computer: it is the camera. What holds is what held
before: the stream leaves the computer only inside TLS that one attested
enclave terminates, the connector dials nothing but that enclave, and the
enclave pins the connector's certificate through the tunnel. The connector
is open source and its releases reproducible (VERIFY.md), so what it does
with the picture can be read.

## 7. Stimulation device link

A connector also serves the electrical stimulation device plugged into the
same computer, if it is one the connector supports. Nothing turns this on: a
supported device found on a USB serial adapter is held released and, once a
session on the paired phone uses this connector, is attached to that session
for as long as it lasts. The service's control plane decides what the device
does; the connector holds it to fixed bounds and fails closed.

Supported today: the ErosTek MK-312BT over its serial link cable (an FTDI
adapter; 19200 baud, 8N1). The design is device-neutral: a driver speaks one
device's protocol, and other devices can follow.

### 7.1 Finding the device

The connector lists the USB serial adapters the system publishes
(`/dev/cu.usbserial*` and the system profiler on macOS, `/sys/class/tty` on
Linux, the registry on Windows) and probes them, FTDI parts first. A probe
listens before it speaks: an idle MK-312BT beacons `0x07` continuously, so
the device is recognized without a byte being written to a port that may
belong to something else. A silent port is spoken to only when the adapter
is an FTDI part (or was named with `-estim-port`), and then with two sync
bytes: a device still holding an old session key answers at once and is
reported as needing a power cycle; an empty port costs one short try.
`masseuse-camlink estim probe` runs the same scan once and prints the
device's status.

A session key agreed with the device is kept in the state directory
(`mk312-key`, mode 0600) so a connector that restarts mid-session resumes
the device instead of needing it power-cycled; the key is forgotten when the
device is unkeyed at close.

### 7.2 What the connector holds to

Whatever the service asks:

- Channel A only; Channel B is written to zero before and after every
  command.
- Level at most 85 of the device's 0..99 scale, moved one step per quarter
  second with a read-back at every step; `adjust_level` moves at most 5.
- Tempo (the device's MultiAdjust) 0..100 percent of the loaded pattern's
  range; `adjust_ma` moves at most 10.
- Patterns from a fixed allow-list of the device's own pattern numbers
  (`0x76..0x7B`, `0x80..0x84`); the connector names none of them.
- The power range is the connector's: high while armed, normal otherwise.

Arming: the device is armed when the service attaches a live session
(`companion_attached`); the attach is the consent, given on the phone. The
arm lasts at most 30 minutes and is renewed by every `heartbeat_ack`. The
connector releases the device (outputs to zero, front panel live, normal
power) and revokes the arm when the session detaches or is cleared, when
15 s pass without a heartbeat acknowledgment, when the arm window runs out,
when any command fails, when the device stops answering, and when the
connector exits (Ctrl-C, a signal). A release sets a latch that only the
next attach clears; a running level ramp stops at its next step. One
actuation runs at a time; a second is refused. The device is read in full
every 5 s while idle; a device that stops answering is released as far as
it can be, closed and forgotten, and its status reports every
safety-relevant field as null, which the service refuses to act on. A
device that reappears is picked up released and unarmed.

### 7.3 Messages

The link speaks the service's companion protocol; messages are JSON objects
with a `type`. Up (connector to service, section 2.5), for the attached
session:

| type | fields | when |
|---|---|---|
| `heartbeat` | | every 5 s while attached, followed by the two below |
| `device_status` | `status` | after every command, arm, release and heartbeat |
| `armed` | `armed`, `expiresAt` (RFC 3339 or null), `heldOff` (always false) | with `device_status` |
| `device_telemetry` | `frames` (at most 32) | every 2 s while attached, when frames were sampled |
| `device_ack` | `commandId`, `ok`, `result` or `error`, `status` | for every command |
| `detached` | `reason` | when the connector detaches on its own |

and at connector level (`sessionId` `""`), after every hello and whenever
it changes:

| type | fields | when |
|---|---|---|
| `device` | `kind` (`mk312bt`), `label`, `connected`, `capabilities` `{levelMax, channels, modes, tempo}` | a device is found or lost |

Down (service to connector, as `estim` events):

| message | connector action |
|---|---|
| `{"type":"control","payload":{"type":"companion_attached","sessionId",...}}` | attach to the session, report state, arm |
| `{"type":"control","payload":{"type":"mk312_command","commandId","sessionId","command":{"verb",...}}}` | run the command (below), acknowledge, report state |
| `{"type":"heartbeat_ack","serverTime"}` | renew the arm |
| `{"type":"detach","reason"}` | release and detach without replying |

Command verbs: `status`; `release`; `set_mode {mode}` (a pattern number);
`set_level {channel:"a", level}`; `adjust_level {channel:"a", delta}`;
`set_ma {percent}`; `adjust_ma {delta}`. A command whose `sessionId` is not
the attached session is refused (`release` excepted). `status` in every
message above is the device reading: `connected`, `port`, `mode` (pattern
number), `levelA`, `levelB`, `power`, `batteryPercent`, `adcOverride`,
`levelMA`, `maMin`, `maMax`, `maPercent`, `maPotOverride`, and the loaded
pattern's modulation state (`gateValue`, `modeRampValue`, `routineTimer`,
`sequence*`, and for each of the width, frequency and intensity blocks the
value, floor, ceiling, signed step, phase percent and direction, null when
the pattern leaves the block pinned). A telemetry frame is `atMs`,
`monotonicS`, `skipped`, `skipReason` and, when not skipped, `mode`,
`levelA`, `levelMA`, `maPercent` and the same modulation state.

**Trust.** The service signs nothing to the connector; the connector trusts
the authenticated event stream it already holds for camera dials. What the
service can do through it is bounded by the connector, in the open: the
caps, the arm window, the fail-closed rules and the allow-list above are
this program's, and its releases are reproducible (VERIFY.md). The person
at the computer stops everything with Ctrl-C, by ending the session on the
phone, or with the device's own power switch.
