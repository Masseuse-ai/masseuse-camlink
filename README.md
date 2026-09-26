# masseuse-camlink

A small program that lets [masseuse.ai](https://masseuse.ai) use your
computer's camera and microphone, or a camera on your home network, without
opening a port on your router, without a VPN, and without anyone but the
video enclave being able to see the picture.

The camera you already have is enough. `masseuse-camlink` runs on the laptop
or desktop in the room and sends its camera and microphone, the built-in
ones or a USB camera and microphone plugged into it, to the one
confidential-computing enclave your session is using; the camera is on only
while a session is watching. If you have a home camera that speaks RTSPS
(UniFi Protect and others), it can send that instead. The masseuse.ai
service never sees the picture: it carries a pairing code and a ticket, and
the stream goes from your computer to the enclave inside TLS.

## Install

Download the archive for your platform from the
[releases page](https://github.com/FemLed/masseuse-camlink/releases) and
unpack it anywhere. To send your computer's camera it needs
[ffmpeg](https://ffmpeg.org), which does the capturing and encoding:

- macOS: `brew install ffmpeg`
- Windows: `winget install Gyan.FFmpeg`, then open a new terminal
- Linux: `sudo apt install ffmpeg` (or your distribution's package)

Without ffmpeg the program still runs and can send a home network camera.
On a NAS or Raspberry Pi the container image does that:

```sh
docker run -d --name masseuse-camlink --restart unless-stopped --network host \
  -v masseuse-camlink:/state ghcr.io/femled/masseuse-camlink:latest \
  -camera-url rtsps://user:password@192.168.1.20:322/live
docker logs masseuse-camlink
```

With Go installed, `go install github.com/FemLed/masseuse-camlink/cmd/masseuse-camlink@latest`
builds the same code from the module proxy.

Every release is reproducible and signed; see [VERIFY.md](VERIFY.md). The
macOS binaries are also signed with an Apple Developer ID and notarized, so
they run without a Gatekeeper warning; VERIFY.md, "The macOS binaries", shows
how to check the signer and how to compare them with a rebuild all the same.

## Use your computer's camera

```
$ masseuse-camlink
masseuse-camlink v0.6.0
Identity 3fK9pQ2m… (state in /Users/you/Library/Application Support/masseuse-camlink)
Camera: Insta360 Link + Yeti Stereo Microphone (1280x720 30 fps, h264_videotoolbox). It is on only while a session reads it.

Pairing code: 7QK4-N2PX
Enter it in the masseuse.ai app: Camera > Computer or home camera.
```

Type the code into the app once. The app then shows the camera by name, with
a button, **Use this camera**, that points the session at it. From then on
the program keeps a quiet connection to masseuse.ai and, whenever a session
uses the camera, turns it on and sends the picture for as long as the session
lasts:

```
Camera link active: connected to the verified enclave.
Camera on: Insta360 Link + Yeti Stereo Microphone.
Sending 1280x720 30 fps, h264_videotoolbox: video 2.1 Mb/s, audio 64 kb/s
...
Camera off.
```

By default it uses the first camera and the first microphone it finds. To
pick others:

```
$ masseuse-camlink devices
Cameras
  0  Insta360 Link
  1  FaceTime HD Camera
Microphones
  0  Yeti Stereo Microphone
  1  MacBook Pro Microphone

$ masseuse-camlink -camera 1 -mic 1
```

A number or (part of) a name works; `-mic none` sends video only. The
choice is remembered by name, so later starts need no flags and survive
the devices being renumbered. If a remembered device is not plugged in at
start, the first of its kind stands in and a line says so (`Insta360 Link
is not connected; using FaceTime HD Camera.`); the remembered choice stays
for the day it is back. `-video-size`, `-fps`, `-bitrate` and `-encoder`
change the picture (defaults 1280x720, 30, 2500k and the hardware encoder,
with libx264 as fallback). `-bitrate` is the ceiling: while the connection
cannot keep up the program steps the video down to 64, 40 or 24 % of it
and back up once it has been clear for a while (see below). Leave the
program running in the background, or set it up as a service; nothing else
is needed.

## Use a camera on your network

```sh
masseuse-camlink -camera-url rtsps://user:password@192.168.1.20:322/live
```

The camera has to speak RTSPS (encrypted RTSP) on your local network. The
program checks its certificate the first time and remembers the fingerprint
(printed, and kept in `cameras.json` in the state directory), so a replaced
or impersonated camera is refused later; pass `-camera-fingerprint` with the
SHA-256 from the camera's own settings to pin it yourself. The camera's
password stays on your computer and is used on your network only. Sessions
then pull the camera only while they are watching.

Other flags: `-service https://masseuse.ai` (the rendezvous service),
`-state-dir DIR` (where the identity key, pairings and camera choice live),
`-log-level debug`, `-version`.

## When the connection cannot keep up

The program watches how far behind the enclave is. A slow or stalling
uplink (Wi‑Fi hiccups, someone else's upload) shows up as these lines:

```
Connection congested: dropping video to keep up (backlog 1.4 s).
Connection cannot keep up: video now 1.6 Mb/s
Video back to 2.5 Mb/s
```

The first means whole video frames are being left out rather than queued,
so what does arrive is current and the audio keeps flowing; the picture
resumes at the next keyframe. The second is the bit rate ladder stepping
down (64, 40, 24 % of `-bitrate`), the third the step back up after a few
clean minutes. The session sees a rougher picture, not a frozen one. A hard
stall of several seconds that happens to coincide with the enclave's
periodic ping can still end the link; the program then reconnects as it
always did, and the session picks the camera up again.

Two lines say the link was ended on purpose and the program is waiting
rather than redialing:

```
Camera link on hold: the enclave is not expecting this connector; waiting for the service.
The enclave closed the camera link (session cleared); waiting for the service.
```

The first is a ticket the enclave no longer holds (the session moved on);
the second is the session letting the camera go. Both are normal after
"Use phone camera" or the end of a session; the next "Use this camera"
brings a new ticket. While attached, the program also tells the service
every 30 s that it is still there, so if the computer sleeps or drops off
the network the app shows the camera offline within about 90 s rather than
whenever the dead connection is noticed.

## Your stimulation device

If an electrical stimulation device the connector supports is plugged into
the same computer, the connector serves it too: the service that runs your
session sees its status and can adjust it, within limits the connector
holds to. Supported today: the ErosTek MK-312BT over its serial link cable
(the FTDI USB adapter it ships with). Nothing to set up: plug the cable in,
switch the device on, and start the connector. It says

```
Stimulation device connected: ErosTek MK-312BT. It is held at zero until a session on your phone uses this computer.
```

and holds the device at zero, with its front panel live and its power range
at normal, until a session on your phone uses this connector. That session
arms it; the arm lasts as long as the session keeps answering, and the
connector puts the device back to zero when the session ends, when the
service goes quiet for 15 s, when any command fails, when the device stops
answering, and when you stop the connector (Ctrl-C). The connector never
sets Channel A above 85 of the device's 99, never touches Channel B, moves
the level one step at a time, and selects only patterns from a fixed list.
The service can only ask for what the connector allows; those limits are in
this program's source, not on the service.

To check the device without a session:

```sh
masseuse-camlink estim probe
```

lists the USB serial adapters, finds the device, prints what it reports and
leaves it released. If the connector picks the wrong adapter, name the right
one with `-estim-port /dev/cu.usbserial-XXXX` (macOS), `/dev/ttyUSB0`
(Linux) or `COM5` (Windows). A device that "holds the key of an earlier
session" was left mid-conversation by a program that stopped without
closing: switch it off for ten seconds and on again. On Linux your user
needs access to the serial device (usually the `dialout` group).

What travels between the connector and the service for this is described in
[docs/PROTOCOL.md](docs/PROTOCOL.md), section 7. It does not go through the
camera tunnel and the enclave never sees it.

## How it stays private

- **Your computer to the enclave, and nowhere else.** The stream leaves your
  computer only inside TLS that the enclave terminates. With a network
  camera named directly by a session the connector relays ciphertext it
  cannot read; with your computer's camera, or a network camera it pulls for
  you, the connector is the camera: it holds the picture on your computer
  and sends it to one place. The service never carries it.
- **One attested peer.** Before dialing an enclave the connector verifies the
  enclave's Confidential Space attestation against the published policy and
  pins the enclave's TLS key to the one bound into that attestation. It does
  not dial anything else. The policy can only tighten floors compiled into
  the connector (the token issuer, the image signing key, the lowest
  release, the project and registry the enclave runs from, the host suffix),
  so the service cannot steer it to an enclave this build does not accept.
  It then checks, in the public registry and the Sigstore transparency log,
  that the attested image digest is what the enclave repository's release
  workflow signed and built at the release the image claims; an image
  without that public record is refused.
- **One private target.** The connector will only connect to the single
  private-network camera address your session names, or serve its own
  camera when the session names that. It is not a proxy. Its own stream has
  no open port: the enclave reaches it only through the tunnel, so nothing
  else on your computer or network can watch it.
- **Camera on only when watched.** The camera and microphone are captured
  only while a session is reading the stream, and never between sessions.
- **Open and reproducible.** Apache-2.0, built from a pinned Go toolchain
  with SLSA provenance and keyless signatures. `VERIFY.md` shows how to check
  a download and rebuild it byte for byte.

The protocol between the connector, the enclave and the service is
documented in [docs/PROTOCOL.md](docs/PROTOCOL.md). Security reports:
[SECURITY.md](SECURITY.md).

## Where your video goes

The only place the connector sends your camera's stream is a masseuse.ai
video enclave: a Google Cloud Confidential Space VM that decrypts the stream
inside hardware-isolated memory, runs person detection and keypoint detection
on the frames, classifies non-speech vocalizations in the audio track when
there is a microphone, and discards both. The code that runs there is
public: [FemLed/masseuse-video-tee](https://github.com/FemLed/masseuse-video-tee)
holds every line that touches frames or audio, and its `README.md` says what
leaves the enclave (numbers, never frames or sound).

You do not have to take that on trust. Every enclave image is built by that
repository's release workflow on GitHub Actions from a tagged commit, with
SLSA provenance and a keyless signature; the workflow alone holds the key
the enclave's launcher checks the image against, and it stamps the release
tag and source commit into the image, where the attestation reports them.
When the connector dials an enclave it logs three things:

```
enclave verified    image=sha256:… signer=cfb085b9… instance=… dbgstat=disabled-since-boot release=vX.Y.Z commit=…
enclave source      image=sha256:… source=github.com/FemLed/masseuse-video-tee@vX.Y.Z registry=ghcr.io/femled/masseuse-video-tee
                    verify="slsa-verifier verify-image ghcr.io/femled/masseuse-video-tee@sha256:… --source-uri github.com/FemLed/masseuse-video-tee --source-tag vX.Y.Z"
enclave provenance  image=sha256:… release=vX.Y.Z commit=… signed_by=https://github.com/FemLed/masseuse-video-tee/.github/workflows/release.yml@refs/tags/vX.Y.Z signature_log_index=… builder=https://github.com/slsa-framework/slsa-github-generator/… provenance_log_index=…
```

The third line is the connector doing, from the public registry and the
Sigstore transparency log, what the `verify` command does: checking that
this exact digest carries a logged signature by that repository's release
workflow at that tag, and logged build provenance naming that tag and
commit. It refuses the enclave otherwise. You can repeat the check yourself
with the `verify` command (or `sh scripts/verify-enclave.sh --origin
https://slot-N.tee.masseuse.ai`, which reads the digest and release off a
live enclave's attestation). [VERIFY.md](VERIFY.md), "The enclave your
camera streams to", walks through it.

## Build from source

```sh
GOTOOLCHAIN=go1.27.1 go build ./cmd/...
go test -race ./...
```

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
