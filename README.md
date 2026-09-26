# masseuse-camlink

A small program that lets [masseuse.ai](https://masseuse.ai) watch a camera on
your home network, without opening a port on your router, without a VPN, and
without anyone but the video enclave being able to see the picture.

Home cameras that speak RTSPS (UniFi Protect, Wyze, and others) only answer on
the local network. `masseuse-camlink` runs on any computer that is on that
network, a laptop, a NAS or a Raspberry Pi, and carries the camera's encrypted
stream to the one confidential-computing enclave your session is using. It
never has the key to that stream, so neither it nor the masseuse.ai service
can watch it.

## Install

Download the archive for your platform from the
[releases page](https://github.com/FemLed/masseuse-camlink/releases), or use
the container image on a NAS or Raspberry Pi:

```sh
docker run -d --name masseuse-camlink --restart unless-stopped --network host \
  -v masseuse-camlink:/state ghcr.io/femled/masseuse-camlink:latest
docker logs masseuse-camlink
```

With Go installed, `go install github.com/FemLed/masseuse-camlink/cmd/masseuse-camlink@latest`
builds the same code from the module proxy.

Every release is reproducible and signed; see [VERIFY.md](VERIFY.md). The
macOS binaries are also signed with an Apple Developer ID and notarized, so
they run without a Gatekeeper warning; VERIFY.md, "The macOS binaries", shows
how to check the signer and how to compare them with a rebuild all the same.

## Use

```
$ masseuse-camlink
masseuse-camlink v0.1.0
Pairing code: 7QK4-N2PX
Enter it in the masseuse.ai app: Camera > Home network camera.
```

Type the code into the app once. From then on the program keeps a quiet
connection to masseuse.ai and, whenever you start a session that uses your
home camera, relays the stream for as long as the session lasts. Leave it
running in the background, or set it up as a service; nothing else is needed.

Flags: `--service https://masseuse.ai` (the rendezvous service),
`--state-dir DIR` (where the identity key and pairings live), `--version`.

## How it stays private

- **Ciphertext only.** The camera encrypts its stream (RTSPS) and the enclave
  decrypts it. The connector relays bytes it cannot read.
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
  private-network camera address your session names. It is not a proxy.
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
the camera has a microphone, and discards both. The code that runs there is
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
