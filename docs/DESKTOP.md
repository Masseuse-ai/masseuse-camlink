# The desktop window

The downloads at masseuse.ai/app run the connector in a terminal window
today: it prints the pairing code, names the camera and the unit, and says
what it is doing in lines of text. This document describes the window that
replaces that terminal: what it shows, how it is built, and how it relates
to the connector whose properties VERIFY.md describes. The code is under
`desktop/`.

## 1. Shape

Two programs, one download.

- **The connector**, `cmd/masseuse-camlink`, unchanged: the CGO-free,
  reproducible program that pairs, streams the camera to one attested
  enclave and serves the unit within bounds. Everything an audit
  establishes about the download is about this program.
- **The shell**, `desktop/`: a native window (Wails v3, the system's
  WebView, a Go module of its own) that starts the connector as a child
  process and speaks to it over its standard input and output, in JSON
  lines, the way the connector speaks to its unit driver helpers
  (docs/UNITS.md). The shell holds no key, no pairing, no camera and no
  unit: it shows what the connector reports and passes on what the person
  chooses. It needs signing; it does not need to be reproducible for the
  download to be verifiable, because the program that matters is the
  connector inside it, byte for byte the published binary.

```
phone app  <->  masseuse.ai service  <->  connector  -->  attested enclave
                                             ^
                                   JSON lines over stdio
                                             v
                                           shell  <->  page (React, shadcn/ui)
```

Closing the window quits, on every platform, as closing the terminal window
does today: the camera goes off, the unit is put to zero and released.
Quit asks first only while a session has the camera or the unit is armed.

## 2. Screens

The window is 1040 x 720, no smaller, dark only,
in the masseuse.ai web app's brand (its colour tokens, type and components,
verbatim; `desktop/frontend/src/index.css`). On macOS the title bar is
hidden and the page's own title strip stands in for it, the traffic lights
over it; Windows keeps its title bar and shows the application menu under
it.

### Shape

Everything sits on one grid (`index.css`): an 8-pt base, a 32-pt inset on
both sides for everything under the title strip (`px-inset`), a 24-pt
gutter (`gap-gutter`), twelve columns for a screen's body (`grid-12`,
`col-span-N`), and a type scale of six sizes (`type-display` for a screen's
title, `type-title` for a card's, `type-body`, `type-secondary`,
`type-caption`, `type-label` for the eyebrow over a card's contents). A
card has 20 pt inside, a well 16, and a list row is 52 pt tall with the
glyph in its own column and a badge trailing (`ui/Card.tsx`, `ROW`); the
radio rows for cameras, microphones and units share it.

The chrome is three rows on that grid (`ui/TopBar.tsx`): the title strip
with the wordmark alone (52 pt on macOS, inset for the traffic lights; 44
on Windows, at the content inset) and, at its right, a word about
the service only while there is one, *Reaching masseuse.ai…* or
*Reconnecting…*; connected, nothing, since Ready's Session box carries the
link's standing where it matters. Under it the masthead (`ui/Masthead.tsx`),
a 40-pt row with a hairline beneath: why the person is here at all, the
tagline in the brand's serif ("See your whole body respond to
electrostimulation.") at the left and the differentiator as two figures in
rose with their words small beside them (300+ data points · 10× a second)
at the right, on every step but Blocked. Then the steps, a 40-pt row on the
same left edge.

Every step is laid out on one template (`ui/Page.tsx`): a header with the
title, the lead at a reading measure and, on the title's line at the right,
the one control that belongs to the whole screen (Cameras' *Behind you /
Your face*, Ready's phones); the body on the twelve columns; and a footer
of one height with the actions at the right, the primary rightmost, and
room at the left for a word about them (*Connect a camera to continue.*,
the makers' trademark line under the supported units). Pair sets its title
inside the body, with the code, the two columns centred on one axis: the
code on seven columns, and on five one card that is the phone's side of
it. Cameras is two lists on six columns each; the face tab a split, the
choice on five and what it asks for on seven. Unit is the units found on
six and the one serving (or what a session may do with one) on the other
six. Ready is four cards on three columns each over the Session box on all
twelve, itself seven and five. Lists that can grow scroll inside their
card, never the window.

The first run is four steps under the top bar, **Pair, Cameras and
microphone, Unit, Ready**, each opening only when the one before is done
by the connector's own facts: a phone paired, a camera the connector can
serve chosen, a unit found. Nothing skips a step (no secondary action
leads on, and the bar's steps ahead stay disabled), and nothing past Pair
can be reached before a phone has paired: a paired phone can see this
computer's camera, so pairing comes first. Going back is always allowed. A
computer that is already paired opens on Ready. Afterwards Ready is home
and the steps are where a choice is changed (`frontend/src/ui/StepBar.tsx`,
the reducer's `ui/go` and `hello` in `frontend/src/bridge/store.tsx`).

| Screen | What it shows | States |
| --- | --- | --- |
| **Pair** | Under the heading, the line on what the code does ("The code securely connects your masseuse to this computer."). The code, large, in the cells the phone's code input draws, with a ring under it that is wiped clockwise over the code's ten-minute life, as an authenticator app draws one, and the time left in words beside the ring ("Code rotates in 9 minutes and 5 seconds", counting down by the second); the three things to do on the phone, one line each. | fresh code (nothing leads on until a phone pairs); the last minute (ring, code and the words in ember; the code's cells breathing, the ring still); not yet reachable (empty cells, "Reaching masseuse.ai…"); a lapsed code waiting for the next (empty cells); a phone just paired (straight on to the camera; the check on Pair in the bar is the confirmation); back here after pairing in this run (the way on is the camera); pairing another phone from Ready; a computer paired on an earlier run opens on Ready instead |
| **Cameras and microphone** | Two tabs, one per view the room shows; under the heading, for Behind you, a line on what the camera behind the person watches and what the face alone accounts for. **Behind you**: this computer's cameras as ffmpeg lists them, each a card with what it is (built in, USB, virtual camera, Continuity Camera), and last in the list "A camera on your network", which opens inline to the RTSPS address and an optional certificate pin; the microphones, with "No microphone" last. A virtual camera such as OBS's carries the word that it shows what its program outputs. The picture itself (size, rate, bit rate, encoder) is the connector's and the enclave's to manage between them and is not offered; nothing on the screen names the encoding. **Your face**: which picture the room shows as the person's face and streams on: *Your phone's camera* (the default: "As captured; nothing passes through this computer") or *OBS Studio* (advanced, "Apply filters or use a dedicated front-facing camera"), which opens three steps: "Pull your phone's camera into OBS Studio", marked optional (the loopback address with a copy button; or a dedicated camera on the computer as OBS's source instead, only step 2's camera going back to masseuse.ai), "Apply filters in OBS Studio, then select your post-processed front-facing camera" (this computer's cameras, virtual ones first; the camera chosen behind the person greyed), and "Enable it in https://masseuse.ai" (under Setup on the phone: *Show the computer's picture as my face*, and *Send my phone's picture to the computer* if step 1 was used). The page says nothing of the loop's latency or of what the analysis reads (the room keeps reading the phone's own picture): that is plumbing, not the person's concern. | Behind you: the usual; looking (the cards' shapes until the connector's first answer); no camera the connector can serve (in the first run, Continue waits, "Connect a camera to continue."); a virtual camera chosen; a remembered device not connected (the stand-in shown, the missing one greyed and named); no camera; locked while a session has the camera; a network camera. Your face: the phone's camera; processed and setting up (the address up, the phone not sending, no virtual camera yet); processed and ready (the phone's picture arriving, OBS Virtual Camera chosen); the face camera also chosen behind you; locked while the room shows OBS's picture; an older connector (no share or face in its report: one line, nothing to choose) |
| **Unit** | The units found (Bluetooth or USB serial by their family), the one served, the one another program has open; the served unit's standing (held at zero or armed, battery, level, bound). With none found, the units masseuse.ai works with, each with its maker's mark, as the phone's "Which units work?" sheet lists them (Mastogo units and the DG-Lab Coyote over Bluetooth, the E-Stim Systems 2B and the ErosTek MK-312BT over a serial link cable), with the trademark line; with units found, the same list behind "Which units work?". | looking, none yet (in the first run, Done waits for a unit); one connected; several; another program has it open; Bluetooth permission needed (macOS); Bluetooth off; disconnected, by reason (idle, battery, button, link); armed by a session (switching waits) |
| **Ready** | What this computer offers (behind you, microphone, your face, unit, each with Change) and what the session is doing: waiting; the camera link active with its rates and the enclave's proof (release, commit, source, registry, signer: the three `enclave …` log lines made readable); with the face through OBS, a Face meter and the loop's three hops (the phone's picture to this computer, OBS's picture back, shown as the face); congested; on hold; closed. | idle; session active; face processed through OBS and live; congested; on hold; update downloaded and waiting; updates off; without a unit |
| **Blocked** | Whole-window: the program cannot run as it is, and the one thing to do. | already running in another window; the connector stopped (its last lines); the state folder cannot be written |
| **About** | A dialog from the application menu: the program, versions (connector and window), the identity's first characters, the state folder, the links (how it stays private, verify this download, report a security issue, the source). | |

Notices, the lines the connector prints in passing (a device standing in, a
congested connection, an update downloaded or refused), stack at the foot of
the window; information goes by itself, a warning waits to be dismissed.
There is no footer: the version and the state of updates are the
application menu's (below), and that the computer stays awake while the
window is open is said on Ready and in About.

### Copy

The program is **Masseuse.ai** in every line a person reads;
`masseuse-camlink` appears only in About and the menu's version line. Sentence
case, no exclamation marks, the phone app's voice (`your masseuse`, `your
phone`, `the session`). Where the connector already says it well, its line
is used as is: the code line, the camera line, the unit disconnected lines
(`cmd/masseuse-camlink/estim.go`, `disconnectedLine`), the update lines. The
vocabulary stays clinical: a stimulation unit, a TENS unit, the intensity.
The tagline ("See your whole body respond to electrostimulation.", in
the brand's serif italic) and the two figures (300+ data points, analyzed 10×
a second) live in one place, `frontend/src/ui/Masthead.tsx`, and stand in
the masthead row on every step (`ui/TopBar.tsx`).

### Menus

Native, built in `desktop/menu.go`, in each platform's shape.

- macOS: **Masseuse.ai** (About Masseuse.ai · Check for updates… · the
  version and the update state as two lines that cannot be chosen · Hide,
  Hide Others, Show All · Quit Masseuse.ai ⌘Q) · **Edit** · **Window** ·
  **Help**.
- Windows: **File** (Check for updates… · the same two status lines · Quit
  Masseuse.ai Ctrl+Q) · **Edit** · **Help** (… · About Masseuse.ai). The
  title bar and the menu bar are drawn in the brand's colours whatever mode
  Windows itself is in (`desktop/theme.go`; the dropdowns stay the
  system's).
- The status lines say what the connector's first lines and update lines
  say in the terminal today (`Masseuse.ai v0.13.0 · masseuse-camlink`; `Up
  to date`, `Looking for a newer release…`, `v0.13.1 downloaded and
  verified; installing when the session ends`, `Updates are off: …`), and
  are relabelled as the connector reports (`ConnectorService.setStatus`;
  in this phase the page reports its mock's state through `ReportStatus`,
  which goes once the shell hears the connector itself).
- **Help**: Learn more about Masseuse.ai (masseuse.ai/app) · How it stays
  private (README) · Verify this download (VERIFY.md) · Report a security
  issue (SECURITY.md) · Show the log · Open the state folder.

About opens the page's dialog through a `menu` event, so it is the same
everywhere and can carry links. The Help links are a fixed list in the
shell (`connector.go`, `links`); the page can open those and nothing else.

## 3. What passes between the shell and the connector

The connector's `-ipc` mode (`cmd/masseuse-camlink/ipc.go`) writes one JSON
object per line on standard output and reads one per line on standard
input; the log stays on standard error. Everything the console would print
goes through one `reporter` (`report.go`): the console implementation
prints the lines the terminal has always shown, the IPC implementation
sends the same facts as the events below. The page's types are in
`desktop/frontend/src/bridge/types.ts` and mirror the Go shapes by hand;
`ipc_test.go` pins the Go side. The first line is always the `hello`, then
the `source`; events that happen during startup wait behind them.

Events, connector to shell:

| `type` | carries | the console's line |
| --- | --- | --- |
| `hello` | `version`, `identity` (the key's first characters), `stateDir`, `updates` (`on`/`off`) and `updatesNote`, `awake` and `awakeNote`, `drivers` (the unit drivers line), `phones` paired | the first lines |
| `online` | `online` | (the log) |
| `code` | `code`, `expiresAt` (RFC 3339, UTC) | "Pairing code: …" |
| `paired` | `phones` | "Paired with a phone." |
| `devices` | `cameras`, `mics` (`{kind,id,name}`), `substitutions` (`{kind,wanted,using}`), `error` when ffmpeg or the listing failed; the answer to `list_devices`, never sent unasked: the page asks once the `hello` has arrived and, while the Cameras screen is open and the window visible, every ten seconds, one ask outstanding at a time (each has the connector run ffmpeg's enumeration), so a camera plugged in appears on its own (`frontend/src/bridge/store.tsx`, `useDeviceListing`) | `devices` |
| `source` | `kind`, `label`, `ready`, `note`, `shape`, `camera`, `mic` or `url`; `share` (`{ready,address,receiving}`) when the phone's picture is asked for, absent otherwise; `face` (`{label,camera,ready,note}`), `null` for the phone's own camera; sent again whenever any of it changes, the phone's picture arriving included | "Camera: …", "Front-facing camera: …", "Your phone's picture: rtsp://…" |
| `link` | `state` (`active`, `on-hold`, `closed`), `reason`; with `active`, `enclave` (`{image,release,commit,source,registry,signedBy,cached}`) as last verified | "Camera link active …", "Enclave image … verified" |
| `camera` | `on`; `stats` (`{videoBps,audioBps,congested,backlogS}`) every 10 s while sending | "Camera on", "Sending …", "Connection congested …" |
| `face` | `on` (the front-facing capture) | "Front-facing camera on/off" |
| `units` | `units` (`{id,kind,label,held}`), `scanning` | "Stimulation units in reach" |
| `device` | `descriptor`: the connector's Descriptor (`kind`, `label`, `id`, `connected`, `held`, `reason`, `capabilities`) with `status` (`batteryPercent`, `power`, `mode`, `levelA`, `levelB`, `outputting`) and `armed` (`{levelBound}`) while a session has it; sent when found or lost and, once a second, when that state changes | "Stimulation device connected/disconnected …" |
| `update` | `state` (`current`, `checking`, `staged`, `installing`, `failed`, `off`), `tag`, `text` | the `Update …` lines; `checking` and a check that found nothing are the window's alone |
| `notice` | `level` (`info`, `warn`, `error`), `text` | anything else printed |
| `blocked` | `kind` (`already-running`, `state-dir-unwritable`), `detail`; sent at once, the program ends | the exits |

Commands, shell to connector: `{"type":"list_devices"}`;
`{"type":"set_source","choice":{…}}` with the source flags as fields
(`camera`, `mic`, `videoSize`, `fps`, `bitrate`, `encoder`, or `url` and
`fingerprint`; `faceCamera`, the connector's `-face-camera`, `"phone"` for
none; `share` true or false, `sharePort`), each part changed only when
spoken of and remembered in `source.json`, reported to the service, then
said as a `source`; a change of the camera while a session reads it, or of
the front-facing camera while the enclave shows it, waits and is applied
when the camera goes off; `{"type":"select_unit","id":"…"}` (refused with a
notice while armed); `{"type":"update_now"}`; `{"type":"quit"}`. Standard
input reaching its end is a shutdown, like the terminal window closing, so
the connector never outlives a shell that was killed.

The shell starts the connector with `-ipc -state-dir DIR -install-root
ROOT` and `MASSEUSE_CAMLINK_RELAUNCH=1`: an update then replaces ROOT (the
bundle, the package executable or the desktop archive's directory) and the
connector ends with exit code 75 for the shell to start the new version.
The shell relays events to the page as the Wails event `connector` and
exposes the commands as typed methods of `ConnectorService`
(`desktop/connector.go`; the page's `send(command)` dispatches onto them).
The bindings are generated as TypeScript with interfaces
(`wails3 generate bindings -ts -i`) and committed under
`desktop/frontend/bindings`.

## 4. Phases

1. **The window on a mock** (done): `desktop/` scaffolded with Wails v3;
   the page complete against a scripted mock of the connector
   (`frontend/src/bridge/mock`), every state reachable from the scenario
   panel in development builds or by `?scenario=<id>`; the native menus,
   About and the Help links real; `ConnectorService` present with its typed
   methods answering "not wired yet".
2. **The connector's `-ipc` mode** (done): `cmd/masseuse-camlink -ipc`
   emits the events above and reads the commands, through one `reporter`
   interface with the console as its other implementation, so the terminal
   keeps printing what it printed. `set_source` re-resolves the offers
   while the camera is off (`apply.go`); `select_unit` is
   `estimLink.choose`; `update_now` is `updater.checkNow`; `-install-root`
   and `update.DetectRoot` make the shell's install the one an update
   replaces, and `Restart` answers the shell on every system. Tested
   against pipes (`ipc_test.go`), the built program included.
3. **The link** (done): `ConnectorService.ServiceStartup` starts the
   connector found beside the shell (`desktop/locate.go`) with `-ipc
   -state-dir … -install-root …` and `MASSEUSE_CAMLINK_RELAUNCH=1`, relays
   its lines as the `connector` event, keeps the last of each kind for a
   page that mounts late (`Snapshot`), writes the page's requests to its
   standard input, and on `ServiceShutdown` sends `quit`, closes its input
   and waits a bounded few seconds; `ShouldQuit` refuses and asks through
   a native dialog while a session has the camera or the unit is armed;
   exit code 75 quits the window and starts the program again
   (`relaunch.go`); any other end is a `blocked connector-stopped` the page
   can start again from (`Restart`). The page swaps the mock for the Wails
   bridge (`frontend/src/bridge/wails`), `?mock=1` keeping the mock inside
   the window. On Windows the shell unpacks its own payload, the connector
   among its files (`internal/payload.UnpackUnder`). Tested against a fake
   connector (`desktop/connector_test.go`) and run around the real one.
4. **Packaging and release** (done): `Masseuse.app` with the shell as its
   executable and the connector at `Contents/MacOS/masseuse-camlink`
   (signed as `ai.masseuse.camlink.connector`), ffmpeg and
   `Helpers/units` inside (`packaging/macos`, `LSUIElement` gone, the
   usage strings in the application's name). The shell being the
   executable makes it the process macOS holds responsible for what its
   children open: under the hardened runtime it needs the camera and
   microphone entitlements itself for ffmpeg's opening them to be allowed,
   or even asked about (`packaging/macos/device.entitlements`, on the
   shell and on ffmpeg; v0.16.0 and v0.17.0 signed the shell without them
   and the camera stayed off, silently; `assess.sh entitlements` now gates
   every bundle). `Masseuse.exe` as the shell
   (GUI subsystem, `desktop/rsrc_windows_amd64.syso`) with the connector
   added to the payload (`packaging/windows/pack -s`). The shell is built
   per platform in the release (`macos-26` universal, `windows-2022`), the
   connector stays the goreleaser build and is compared byte for byte
   inside every download, and `update-check-*` run the connector from
   inside each download with `-install-root`. VERIFY.md gained "The
   desktop window"; README's Install is the window's. (A Linux desktop
   archive was published up to v0.17.0 and is no longer: the program is
   offered for macOS and Windows.)

### The face view and OBS

The loop is the connector's and the enclave's (the "phone picture through
OBS" plan): the room sends the phone's picture to this computer through the
connector's tunnel, the connector serves it on loopback RTSP for OBS
(`-share-port`, `-no-share`), and a second capture (`-face-camera`, a
virtual camera such as OBS's) carries OBS's picture back, which the room
shows as the person's face and streams on, never analysing it; the phone's
own picture alone feeds the analysis. The window's part: the person chooses
the face view under Cameras › Your face, the choice configures the
connector's offer and is reported to the service, and the phone opens both
directions from its Setup sheet (*Send my phone's picture to the computer*,
*Show the computer's picture as my face*); until it does, the steps say they
are waiting. Choosing the phone's camera changes nothing: the picture goes
straight to the room, as today, with no detour and no added delay.

### Names

The connector reports units by the labels its drivers give them
(`Mastago TENS G-12AB`, the Mastago family's kind `mastago`); the list of
units that work uses the makers' names as the phone app has them, `Mastogo`
among them, as the unit advertises itself. Both stand; the list is the
brand's, the labels are the connector's.

## 5. Before a tag

The release jobs check what a machine can check. On each system, by hand,
with the assembled download (the unsigned CI artifacts do for all but the
Gatekeeper and SmartScreen prompts):

- Open the download: the window comes up with a pairing code; the
  application menu (or the window's menu bar) shows the connector's
  version and the update line; Help opens the pages; About opens.
- Pair a phone, pick a camera and microphone, pick the phone's camera or
  OBS Studio for the face view, pick a unit; the phone sees the choices
  (the source report). The ready screen shows the link active during a
  session.
- Quit during a session: the question appears; *Keep running* keeps it;
  *Quit* ends the session, the camera goes off, the unit is released, and
  no `masseuse-camlink` process is left (`pgrep`, Task Manager).
- Kill the window (`kill -9`, End task): the connector ends with it
  (standard input closed).
- An update: run the download with `MASSEUSE_CAMLINK_UPDATE_AS=v0.10.0`
  in the environment; within a minute the window closes and the new
  version's opens, with the same pairing, `Updated to vX.Y.Z` in the menu;
  the previous install is under `previous/` (Mac) or `.previous/`.
- The log (Help, "Show the log") holds the connector's lines and the
  shell's.

## 6. Open questions

- A local preview of the chosen camera in the picker. It would help with a
  virtual camera (is OBS outputting?) but sits oddly beside "the camera is on
  only while a session reads it" and needs the WebView's media permission
  work; left out of the page until decided.
- Decided against: controls for the picture (size, rate, bit rate, encoder).
  The connector and the enclave manage the link between them; the window
  is a front on what the program does, not a place to constrain it. The
  connector's flags remain for people who run it from a terminal.
- A system tray presence, so the window can be closed without quitting.
  Today closing quits, as the terminal did; a tray is a later option if
  people ask for it.
- Start at login (Wails has `app.Autostart`). Not asked for; the connector
  runs while the window is open.
