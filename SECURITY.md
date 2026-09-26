# Security

## Reporting

Report vulnerabilities privately through GitHub's
[security advisory form](https://github.com/FemLed/masseuse-camlink/security/advisories/new)
for this repository. Do not open a public issue for a vulnerability. Reports
are acknowledged within three business days.

## What the connector can and cannot do

- It sends one camera to one attested enclave, only while a session you
  started on your phone is bound to it: your computer's own camera and
  microphone, a camera on your network it pulls for you, or, when the
  session names a network camera directly, that camera's bytes relayed
  untouched.
- The stream leaves your computer only inside TLS that the enclave
  terminates. When it relays a camera named by the session it cannot read
  the bytes. When it serves its own camera it holds the picture on your
  computer, as any camera program would, and sends it to that one enclave;
  its own stream has no listening port and is reachable only through the
  tunnel. The masseuse.ai service carries metadata only.
- The camera and microphone are captured only while a session is reading;
  the capture process is stopped when the session's tunnel ends.
- It refuses to dial anything but a single private-network `host:port` per
  session, so a compromised service cannot turn it into a proxy.
- It dials an enclave only after verifying that enclave's Confidential Space
  attestation (the image's signing key and release, debug state, hardware
  model) against the policy published at `https://masseuse.ai/api/tee-policy`,
  and pins the enclave's TLS key to the one bound into that attestation.
- Its identity is an Ed25519 key stored with mode 0600 in its state
  directory. Deleting the directory revokes every pairing.
- If a supported electrical stimulation device is plugged into the same
  computer (today the ErosTek MK-312BT over its serial cable), the connector
  relays the service's commands to it and its status back, over the same
  authenticated channel it uses for camera dials; the camera tunnel carries
  none of it. The connector holds the device to bounds the service cannot
  change: Channel A only, one step per quarter second with a read-back,
  patterns from a fixed allow-list, the level never past the device's own
  99. Two bounds within those are the attached session's to set from the
  phone and are restored to the defaults when it detaches: the power range
  the device is armed in (normal or high; high by default) and the highest
  level a command may set (85 by default). The device is armed only while a
  session started on your phone is attached, for at most 30 minutes per
  renewal, and is put back to zero (front panel live, normal power) when the
  session ends, when the service goes 15 s without acknowledging a
  heartbeat, when a command fails, when the device stops answering, and
  when the connector exits. Ctrl-C and the device's own power switch stop it
  at any time. The serial probe listens before it writes, so ports that
  belong to other devices are not written to.

## Supply chain

Every release is built by GitHub Actions from a tag, with a pinned Go
toolchain and reproducible flags, and ships with SLSA provenance, a cosign
keyless signature bundle and a checksum file. `VERIFY.md` walks through
verifying a download and rebuilding it byte for byte.
