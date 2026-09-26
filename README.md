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

On the computer in the room, open [masseuse.ai/app](https://masseuse.ai/app):
it offers the download for that computer (Mac or Windows), and the app on
your phone can send it the address. The same files are on the
[releases page](https://github.com/FemLed/masseuse-camlink/releases).

The downloads are called **Masseuse.ai**: that is the one name a person
sees, on the disk image, the window and the permission prompts (the Mac
app's icon says **Masseuse**: macOS shows the extension of a bundle called
`Masseuse.ai.app`, `.ai` being a file type it knows, so the bundle is
`Masseuse.app`). `masseuse-camlink` is the program's name for engineers
(this repository, the command, the archives, the state directory) and the
same binary.

Every download is the same two programs: the **window**, which shows the
pairing code, the cameras and microphone and the stimulation unit and
walks you through them, and behind it the **connector**, `masseuse-camlink`,
which does the work and can also run alone in a terminal (docs/DESKTOP.md).

**Mac.** The download is a disk image, `Masseuse.ai-X.Y.Z.dmg`, for Apple
silicon and Intel alike. Open it, drag `Masseuse` into `Applications`, and
open it from there (a `Masseuse.ai` left there by 0.8.0 or 0.8.1 is the
same program and can go). The window opens with the pairing code; ffmpeg
and the unit drivers are included in the app, so there is nothing else to
install. Leave the window open while you use it; closing it stops the
program, and opening the app again while it runs only brings the window
forward. The system asks for the camera, the microphone and Bluetooth in
the application's name the first time each is used. The connector alone
runs in Terminal as `Masseuse.app/Contents/MacOS/masseuse-camlink -console`
(the `darwin_*` archives carry the same bare binary; run from anywhere but
Terminal, macOS refuses a bare binary however it is signed, which is what
the app is for).

**Windows.** The download is one file, `Masseuse.exe` (the Mac's bundle is
`Masseuse.app` for the same reason: the program is Masseuse.ai, the file
shows as Masseuse): save it anywhere and open it. The window opens with
the pairing code. The file carries the connector, ffmpeg and the unit
drivers inside itself and unpacks them under
`%LOCALAPPDATA%\masseuse-camlink` the first time it runs, so there is
nothing to extract and nothing else to install (releases before 0.13 were
a zip, and opening the program from inside Explorer's zip preview left
ffmpeg and the drivers behind in the zip; the one file has nothing to lose
that way). The package is signed by Principled Labs, Inc. with Azure
Artifact Signing; while a release is still new to Microsoft, Windows may
ask before the first start whether to run it: *More info*, then *Run
anyway* (a release published without the signing credentials is unsigned
and always asks). Video is encoded by Windows' own Media Foundation H.264
encoder (the graphics chip's, or the software one every Windows edition
carries except the N editions, which need Microsoft's *Media Feature Pack*
from Settings, *Optional features*). The program serves the camera and
microphone and a stimulation unit: over the computer's Bluetooth (Windows
10 version 1703 or newer), or over a USB serial cable through a unit driver
helper (see "Your stimulation device"). The `windows_*` archives carry the
bare `masseuse-camlink.exe` for people who run it from a terminal with
their own ffmpeg.

**The bare binaries.** The `masseuse-camlink_*` archives (`darwin_*` and
`windows_*`, amd64 and arm64) carry the connector alone, for a terminal:
unpack anywhere and run `masseuse-camlink`. It needs ffmpeg the same way:

- Windows, with the bare binary rather than the one file: `winget install Gyan.FFmpeg`, then open a new terminal
- macOS, with the bare binary rather than the app: `brew install ffmpeg`

Without ffmpeg the program still runs and can send a home network camera
(`-camera-url`).

With Go installed, `go install github.com/FemLed/masseuse-camlink/cmd/masseuse-camlink@latest`
builds the same code from the module proxy.

Every release is reproducible and signed; see [VERIFY.md](VERIFY.md). The
connector in every download is the published binary, byte for byte, and
so a rebuild; the window and ffmpeg are the two parts that are not rebuilt
byte for byte, each built by the same public workflow from the same commit
and covered by the release's signed checksums and provenance (VERIFY.md,
"The desktop window"). The macOS app, its disk image and the bare macOS
binaries are also signed with an Apple Developer ID and notarized: the app
and the image open from the Finder without a Gatekeeper refusal, and the
bare binaries run from Terminal. VERIFY.md, "The macOS binaries" and "The
macOS app", show how to check the signer and how to compare the connector
inside with a rebuild. The Windows `Masseuse.exe` is the window followed by
the published `windows_amd64` connector, an ffmpeg built the same way and
the helpers as its payload, then signed; `cmd/pestrip` takes the signature
and the payload off and gives the connector back for comparison
(VERIFY.md, "The Windows package").

### Updates

From v0.11.0 the program keeps itself current, so there is no going back
to masseuse.ai/app for a new version. Fifteen seconds after it starts, and
every six hours after, it looks at the latest release on GitHub. A newer
one is downloaded and checked the way VERIFY.md tells a reader to check a
release, by the running program: the checksum file's signature must be the
release workflow's (this repository's, at a release tag), the download's
hash must be in that file, the SLSA provenance must name the download, and
on a Mac the application inside the disk image must carry the same
Developer ID and pass Gatekeeper. Then the new version is put in place and
started, but only when nothing is using the computer: no session has the
camera, and the unit is neither attached to one nor armed. During a session
the program says (in the window's application menu, under *Check for
updates*; on the console as a line)

```
Update: Masseuse.ai v0.11.1 downloaded and verified; installing when the session ends.
```

and afterwards `Updating to v0.11.1; back in a moment.`; the window closes
and the new version's opens, with the same pairing, `Updated to v0.11.1.`
among its first words (in a terminal, the new version's first lines follow
in the same window). An update downloaded but not yet installed when the
program was closed is installed the next time it opens. Nothing is asked;
nothing is run, moved or removed before it has verified.

What goes wrong is said in one line and changes nothing:
no network means no check until the next one; a release that does not
verify, or does not fit on the disk, is said so once and left alone for a
day (`Update v0.11.1 did not verify; staying on v0.11.0.`); a copy of the
program this user cannot replace where it is (`Masseuse` run from the disk
image rather than from Applications, a binary under `/usr/local/bin`) says
so at start and is left to be updated by hand. The version replaced is kept
out of sight (a Mac bundle under the state directory, `previous/`; the
files of the other layouts in `.previous/` inside the install) until the
new one has connected to the service once, then removed; if a new version
would not start, the previous one is put back and the window says so.
The restart itself is the shell's that started the program, when there is
one: the desktop window, or the `.command` file the Mac bundle writes for
Terminal, which runs the program in a loop. Either sets
`MASSEUSE_CAMLINK_RELAUNCH=1`, and an update ends the program with exit
code 75 for the shell to run the new version at the same path, on every
system. A Mac program started some other way opens the new bundle through
LaunchServices (a window of its own) and ends. The program never replaces
itself by `execve` there: Go's runtime, before an exec on Darwin, waits for
every preemption signal it has sent to be received, and a program that runs
Go code on CoreBluetooth's threads can wait forever (`update.ErrRelaunch`).
Without a shell, Windows starts the new program and ends.

`-no-update` (or `MASSEUSE_CAMLINK_UPDATE=off`) turns it off, for people
who manage their installs; `masseuse-camlink update` installs the latest
release now, from a terminal, with the program stopped. Builds from a
working tree do not update themselves. Whatever the update did is what an
install by hand would have done: the same release files, the same checks
(VERIFY.md, "What the updater verifies").

### The desktop window

The connector has no window of its own; the desktop application
(`desktop/`, docs/DESKTOP.md) is a separate program that runs it as a
child with `-ipc` and shows what it says. In that mode everything the
console would print goes out on standard output as one JSON object per
line (`hello`, `code`, `source`, `link`, `camera`, `units`, `device`,
`update`, `notice`, `blocked`, ...), the window's requests come in on
standard input the same way (`list_devices`, `set_source`, `select_unit`,
`update_now`, `quit`), and the log stays on standard error. Standard input
ending is a shutdown, so a window that was killed never leaves a connector
behind. `-install-root` names what the window was started from
(`Masseuse.app` or `Masseuse.exe`), which is what an update then replaces,
the window included. The
flags are the window's to pass; from a terminal, `masseuse-camlink` alone
is the program as described above.

## Use your computer's camera

```
$ masseuse-camlink
Masseuse.ai for your computer  (masseuse-camlink v0.8.0)
Identity 3fK9pQ2m… (state in /Users/you/Library/Application Support/masseuse-camlink)
Camera: Insta360 Link + Yeti Stereo Microphone (1280x720 30 fps, h264_videotoolbox). It is on only while a session reads it.

Pairing code: 7QK4-N2PX
Type it into the masseuse.ai app on your phone when it asks for the code from your computer; the dash is added for you.
```

Type the code into the app once, when its setup asks for the code from your
computer (eight letters and numbers; the dash is drawn for you). The app then
shows the camera by name, and the session takes this camera on its own once
the private room is ready (the app's Setup sheet can hand the view back to
the phone, or to this camera again). From then on the program keeps a quiet
connection to masseuse.ai and, whenever a session uses the camera, turns it
on and sends the picture for as long as the session lasts:

```
Camera link active: connected to the verified enclave.
Camera on: Insta360 Link + Yeti Stereo Microphone.
Sending 1280x720 30 fps, h264_videotoolbox: video 2.1 Mb/s, audio 64 kb/s
...
Camera off.
```

By default it uses the first camera and the first microphone it finds,
passing over an iPhone or iPad joined through Continuity Camera when the
computer has one of its own (those open over the air, slowly and not
always). To pick others:

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
for the day it is back. On macOS the devices are opened by name too, so a
phone coming into or out of reach through Continuity Camera between start
and session, which renumbers the list, does not point the capture at the
wrong device or at none; when a device still cannot be opened the list is
read again and the choice made afresh before the retry. `-video-size`,
`-fps`, `-bitrate` and `-encoder` change the picture (defaults 1280x720,
30, 2500k and the hardware encoder, with libx264 as fallback where the
ffmpeg has it; the one shipped in the app does not). `-bitrate` is the
ceiling: while the connection
cannot keep up the program steps the video down to 64, 40 or 24 % of it
and back up once it has been clear for a while (see below). Leave the
program running in the background, or set it up as a service; nothing else
is needed.

The first time a session turns the camera on, the system asks whether
Masseuse.ai may use the camera and the microphone; allow both. If the
camera's light never comes on and the window says the camera is on but no
picture is being sent (after ten seconds the connector says so too, and
why it thinks so), the system has refused it: on a Mac, System Settings ›
Privacy & Security › Camera, and Microphone, must list Masseuse and allow
it; on Windows, Settings › Privacy & security › Camera, *Let desktop apps
access your camera*, must be on, and the same under Microphone. v0.16.0
and v0.17.0 of the Mac app could not ask at all (the window was signed
without the camera and microphone entitlements macOS requires of the
application that runs ffmpeg, so the refusal was silent and Masseuse never
appeared in that list); the update to the next release fixes that, and the
question is asked at the first session after it.

While it runs, the computer does not go to sleep on its own. A laptop
left at the foot of the bed is not touched while the session's enclave
boots, and on battery macOS and Windows would put it to sleep a few
minutes after the last touch, stopping the camera and the microphone and
dropping the Bluetooth link to the unit; so the program holds the system's
own "stay awake" for as long as it runs (a power assertion on macOS,
`pmset -g assertions` lists it; a power request on Windows, `powercfg
/requests`), and says so on its first lines. The screen may still go dark, which the camera
does not need; closing the lid still sleeps the computer, so leave it open.
The hold ends with the program, however it ends. `-allow-sleep` runs
without it; a computer running the program as a service on battery will
otherwise never idle-sleep, which is the point on a laptop set up for
sessions and a battery cost anywhere else.

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

## Use OBS with your phone's camera

Your phone's camera is your face in a session, and by itself it goes from
the phone to the enclave and back with nothing in between. If you would
rather work on that picture first - a beauty filter, a virtual background,
colour grading - you can route it through OBS Studio on this computer and
send OBS's result back as your face:

```sh
masseuse-camlink -share-phone on -face-camera "OBS Virtual Camera"
```

```
Your phone's picture: sessions are asked to send it here, and programs on this computer can open it at
  rtsp://127.0.0.1:7446/phone-Wq8…
  (OBS: a Media Source with Local File unticked, that address as the Input, Network Buffering 0 MB.)
Front-facing camera: OBS Virtual Camera (1280x720 30 fps, h264_videotoolbox). It is on only while a session shows it as your face; until then your phone's own camera is.
```

In OBS, add a **Media Source**, untick *Local File*, paste the address as
the *Input*, set *Network Buffering* to 0 MB, and tick *Restart playback
when source becomes active* (the picture arrives only while a session is
running). Add your filters, then **Start Virtual Camera**. The next session
sends your phone's picture to this computer within a few seconds of
starting, and shows OBS's virtual camera as your face on the phone and in
the live stream, if you run one. Both choices are remembered; `-face-camera
none` puts your phone's own camera back as your face, and `-share-phone
off` stops the picture coming here. The app's Setup sheet has a "Use my
phone's camera instead" for the rest of a session.

This loop costs latency: the picture goes to the enclave, to this computer,
through OBS, and back, so your face on the phone runs a second or so behind
the room. That is the price of the effects, and only of the effects: with
nothing chosen here, your phone's camera takes the direct path it always
did, with none of it. Any other camera of this computer's works as the
front-facing camera too (`-face-camera 1`, by number or name from `devices`;
`-face-video-size`, `-face-fps` and `-face-bitrate` shape it), with no need
for `-share-phone`; it then costs that camera's own latency alone.

What the enclave analyses - the posture and face keypoints the session runs
on - is always your phone's own picture, before OBS touches it; what comes
back from this computer is shown and streamed, never analysed. Where the
picture goes is set out under "Where your video goes" below.

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
"Stop using this camera" or the end of a session; the next "Use this
camera" brings a new ticket. A new ticket that arrives while the link is up
(the session's room was leased again, as when its picture is being set up)
changes nothing on screen: the link stays, and the new ticket is the one
used if the link ever has to be redialed. While attached, the program also tells the service
every 30 s that it is still there, so if the computer sleeps or drops off
the network the app shows the camera offline within about 90 s rather than
whenever the dead connection is noticed.

## Your stimulation device

If an electrical stimulation device the connector supports is within reach
of the same computer, the connector serves it too: the service that runs
your session sees its status and can adjust it, within limits the connector
holds to. The reference device is the Mastago TENS unit (the Bluetooth
unit that advertises as `MASTOGO G-xxxx`). Nothing to set up: switch the
unit on and start the connector; the first time, macOS asks whether the
terminal may use Bluetooth (Windows asks nothing: Bluetooth only has to be
on, in Settings, *Bluetooth & devices*). The connector finds the unit
whether it is advertising or already open in the vendor's own app on this
computer, and says

```
Stimulation device connected: Mastago TENS G-12AB. It is held at zero until a session on your phone uses this computer.
```

and holds the unit paused at zero, with its own buttons live, until a
session on your phone uses this connector. That session arms it, which also
sets the unit's own countdown to the arm window (30 minutes, renewed while
the session keeps answering), so the unit stops by itself if this computer
dies with it armed. The connector puts the unit back to zero when the
session ends, when the service goes quiet for 15 s, when any command fails,
when the unit stops answering or switches itself off, and when you stop the
connector (Ctrl-C). One bound is yours to set for a session, from the
phone: the highest intensity the unit may be set to (15 of its 25 until you
choose; never more than 25). It holds for that session only; the next starts
from the default. If you lower it below where the unit is running, the
connector puts the unit to zero first and arms it again within the new
bound. Everything else is fixed: the connector moves the intensity one step
at a time, reading it back at every step, selects only the unit's own 32
programs, and refuses an intensity the unit itself refuses because the
pads are not on the skin. The service can only ask for what the connector
allows; those limits are in this program's source, not on the service.

To check the unit without a session:

```sh
masseuse-camlink estim probe
```

says what the connector can see over Bluetooth (units already open in
another program, units advertising), finds the unit, prints what it reports
and leaves it released. `-estim-ble off` leaves Bluetooth alone. On Windows
the connector talks to the Windows Runtime (Windows 10 version 1703 or newer) and names a unit by
its Bluetooth address (`C4:BE:84:70:29:3F`), which is the identifier
`-estim-ble` and `-estim-unit` take there; a unit another program has
open is found where Windows lists it among its connected devices, and the
scan finds the rest.

**Several units.** The connector serves one unit at a time and keeps an eye
on the others: every half minute it lists the units in reach without
connecting to them, tells the service the list, and with more than one
prints them numbered:

```
Stimulation units in reach (2):
  1  Mastago TENS G-12AB  (serving this one)
  2  Mastago TENS G-34CD
Type a number and Enter to serve another unit; the phone can pick one too. A unit in use by a session is switched once the session stops it.
```

Type a number to switch; the unit let go is put to zero first. The phone
offers the same choice on its Unit screen. Either way the choice is
remembered (`estim.json` in the state directory) and the next start serves
that unit; `-estim-unit G-34CD` (the suffix the unit advertises, or the
system's identifier for it) sets it from the command line, and
`-estim-unit any` forgets it, back to the first unit found. While a session
has the unit armed a switch waits: stop the unit on the phone first.
`-estim-ble G-12AB` still pins the Bluetooth family alone for one run.

**A unit another program has open.** If the vendor's app, or anything else
on this computer, already has the unit open, the connector shares that
link rather than fighting for it, and says so:

```
Another program on this computer has this unit open. Close it before a session, or its commands and Masseuse.ai's will collide.
```

Both programs' commands reach the unit through the one connection, and the
connector cannot tell the unit's replies to one from the other's. Close the
other program before a session; a write of its that spoils the connector's
read-back fails the command and puts the unit to zero.

**Stimulation units beyond the Mastago: driver helpers.** The Mastago
driver is in this repository. Drivers for further units are published by
masseuse.ai as *unit driver helpers*: separate programs named
`camlink-unit-<name>` that the download carries beside `ffmpeg`
(`Contents/Helpers/units/` in the macOS application, inside
`Masseuse.exe` on Windows, unpacked to `units\` under the state
directory, `units/` next to the program in the other archives) and that
this program runs as child processes. They are not part of this repository
and not open source; they are ordinary signed binaries anyone may inspect,
listed with their checksums in a signed manifest the release verifies
before bundling them ([VERIFY.md](VERIFY.md)). A helper is handed its
standard input and output and a state directory of its own, and finds its
unit itself (a serial port, a Bluetooth peripheral); it never sees the
camera or the microphone, and it speaks to the service only through this
program, which holds every helper's unit to the same limits as the
Mastago. The first line about them in the window says which were found:

```
Unit drivers: Mastago (built in) + 1 helper(s): example.
```

`-estim-helpers <dir>` looks in another directory; `-estim-helpers none`
runs without any. Remove `Helpers/units` from the application and you have
a connector that serves the Mastago alone, built entirely from this
repository. The protocol between the two, and how to write a helper, is
[docs/UNITS.md](docs/UNITS.md).

What travels between the connector and the service for this is described in
[docs/PROTOCOL.md](docs/PROTOCOL.md), section 7. It does not go through the
camera tunnel and the enclave never sees it. The connector names the device
family it serves with a `kind` (`mastago` for the driver in this
repository; a unit driver helper names its own family), and the service
shapes the session on that name: a session with no device is guided
differently from one with a device connected.

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
  If the connection to the enclave drops mid-session, they stay on for up
  to 15 s while it is re-established, then go off.
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

While a session uses this camera, your phone's camera stays live too: the
enclave shows it as a small inset over this camera's picture, with your
face's keypoints, and classifies the phone's microphone rather than this
computer's. So that the two pictures line up, each frame of this stream
carries the time its source gave it (ffmpeg's clock for the computer's
camera, the camera's own for one on the network), passed through unchanged;
the enclave aligns the two streams on that time. Both streams end at the
same enclave and nowhere else.

Your phone's picture reaches this computer only when you ask for it here
(`-share-phone on`): the enclave then copies the phone's stream, as it
arrived and without re-encoding it, through the same verified tunnel to
this program, which serves it on this computer alone - a loopback address
behind a secret path that other programs on this computer, such as OBS,
can open, and nothing off the computer can reach. The picture is never
written to disk by this program, and the address goes nowhere but your
screen and `source.json`. What this computer sends back as your face
(`-face-camera`) travels the tunnel like the camera does, to the same
enclave, which shows it and streams it on but never analyses it: the
keypoints, and everything the session derives from them, come from your
phone's own picture.

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
