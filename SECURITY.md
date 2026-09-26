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
- If a supported electrical stimulation device is within reach of the same
  computer (the reference device is the Mastago TENS unit, over Bluetooth
  Low Energy), the connector relays the service's commands to it and its
  status back, over the same authenticated channel it uses for camera
  dials; the camera tunnel carries none of it. The connector holds the
  device to bounds the service cannot change: one channel, one intensity
  step per 0.4 s with a read-back, programs from the device's own fixed
  list, the intensity never past the device's own scale (25 on the
  Mastago). The bounds within those are the attached session's to set from
  the phone and are restored to the defaults when it detaches: the highest
  intensity a command may set (15 of 25 by default on the Mastago) and, on
  a device with several power ranges, the range it is armed in. The device
  is armed only while a session started on your phone is attached, for at
  most 30 minutes per renewal, and is put back to zero (output stopped, the
  device's own controls live) when the session ends, when the service goes
  15 s without acknowledging a heartbeat, when a command fails, when the
  device stops answering, and when the connector exits. Arming also sets
  the device's own countdown, where it has one, to the arm window, so the
  device stops by itself if the connector dies. Ctrl-C and the device's own
  power button stop it at any time. The Bluetooth finder connects only to a
  peripheral that offers the device's own service or advertises its name,
  and takes it as found only once it answers a program query.
- Drivers for stimulation units other than the Mastago are unit driver
  helpers (`docs/UNITS.md`): separate programs published by masseuse.ai,
  bundled beside `ffmpeg`, that the connector runs as child processes and
  speaks to over their standard input and output. A helper receives its
  standard input and output and a state directory of its own, and nothing
  else: no camera or microphone handle, no network address, no pairing
  key. It reaches the service only through the connector, which holds a
  helper's unit to the same bounds as the Mastago, and it can be removed
  (`Helpers/units`, or `-estim-helpers none`) leaving a connector built
  entirely from this repository. Helpers are not open source; each release
  names every helper it bundles, with its hash, in a manifest signed by
  the key whose public half is `packaging/units/cosign.pub`, and the
  release workflow verifies that before bundling (`VERIFY.md`).

- It updates itself (README "Updates") only from this repository's
  releases, and only after verifying, with the Sigstore trust root it
  carries, that the release's checksum file was signed by this
  repository's release workflow at a release tag, that the download's hash
  is in that file, that the SLSA provenance names the download, and on a
  Mac that the application is signed by the same Developer ID and accepted
  by Gatekeeper (VERIFY.md, "What the updater verifies"). The service at
  masseuse.ai names no version and serves no file, so it cannot push code
  to a connector. Nothing is run, moved or removed before it verifies; the
  version replaced is kept until the new one has connected once; a new
  version is installed only while no session is using the computer.
  `-no-update` turns it off.

## Supply chain

Every release is built by GitHub Actions from a tag, with a pinned Go
toolchain and reproducible flags, and ships with SLSA provenance, a cosign
keyless signature bundle and a checksum file. `VERIFY.md` walks through
verifying a download and rebuilding it byte for byte.
