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

Every release is reproducible and signed; see [VERIFY.md](VERIFY.md).

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
  not dial anything else.
- **One private target.** The connector will only connect to the single
  private-network camera address your session names. It is not a proxy.
- **Open and reproducible.** Apache-2.0, built from a pinned Go toolchain
  with SLSA provenance and keyless signatures. `VERIFY.md` shows how to check
  a download and rebuild it byte for byte.

The protocol between the connector, the enclave and the service is
documented in [docs/PROTOCOL.md](docs/PROTOCOL.md). Security reports:
[SECURITY.md](SECURITY.md).

## Build from source

```sh
GOTOOLCHAIN=go1.27.1 go build ./cmd/...
go test -race ./...
```

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
