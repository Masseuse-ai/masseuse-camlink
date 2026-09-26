# Unit driver helpers

The connector serves stimulation units through drivers. One driver, the
Mastago's, is in this repository (`internal/estim/mastago`). Drivers for
further units are *unit driver helpers*: separate programs, published by
masseuse.ai and not part of this repository, that the connector finds in
its units directory, runs as child processes and speaks to over their
standard input and output. This document is the whole of the arrangement:
where the helpers live, how the connector runs them, what passes between
the two, how the release gets them, how they are signed and verified, and
how to write one.

The reason for the split is what an audit of this repository should be
able to establish: that the program a person runs on their computer sends
one camera to one attested enclave and nothing else. That is true of the
connector with or without helpers. A helper is handed its own standard
input and output and a state directory of its own, and finds its unit
itself (a serial port, a Bluetooth peripheral). It receives no camera or
microphone handle, no network address, no pairing key; it reaches the
service only through the connector, which holds every helper's unit to
the same bounds as the Mastago (`internal/estim`, PROTOCOL.md section 7).
Remove the helpers and the connector is entirely this repository.

## 1. The units directory

The connector looks for helpers in one directory:

| where the connector runs | units directory |
| --- | --- |
| the macOS application (`Masseuse.app/Contents/MacOS/Masseuse`) | `Masseuse.app/Contents/Helpers/units/` |
| the Windows package (`Masseuse.exe`, the helpers inside it as a payload) | `bin\<id>\units\` under the state directory, where the payload is unpacked |
| anywhere else (the archives, a `go build`) | `units/` next to the executable |

`-estim-helpers <dir>` (environment `MASSEUSE_CAMLINK_HELPERS`) names
another directory; `-estim-helpers none` runs without helpers. A directory
that does not exist is no helpers.

A helper is a file in that directory named `camlink-unit-<name>`
(`camlink-unit-<name>.exe` on Windows) that is executable. `<name>` is the
family's short name and also what the helper answers to `hello` (below).
Anything else in the directory is ignored.

## 2. Lifecycle

At startup, after the camera is chosen and before pairing is read, the
connector greets each helper in the directory: it starts the program with
`--state-dir <dir>`, sends `hello`, and waits up to 15 s for the answer. A
helper that answers becomes a device family, registered after the Mastago
(`cmd/masseuse-camlink/helpers.go`); one that does not start, does not
answer, names another protocol version or gives no name is logged and
left out, and the connector runs on without it. The window's line says
what was found:

```
Unit drivers: Mastago (built in) + 1 helper(s): example.
Unit drivers: Mastago (built in); no helpers in /path/to/units.
Unit drivers: Mastago (built in); helpers off (-estim-helpers none).
```

`masseuse-camlink estim probe` prints the same line, then each family's
description (`describe`), then runs the search once and prints the
device's status, helpers included.

The helper process lives as long as the connector does (one process per
family), whether or not its unit is in reach. The connector ends it when
it exits. A helper that exits or closes its standard output while a call
is waiting fails that call as a loss with reason `stopped_answering`: the
connector reports the unit as not connected (`device.reason`), releases
whatever it can, and starts the program again on its next search. A
selection made before the restart (`select`) is repeated to the new
process.

The helper's state directory is `<state dir>/units/<name>/` under the
connector's own state directory (`Library/Application Support/masseuse-camlink`
on macOS, and so on), created by the helper if it needs it (mode 0700 is
its business). The connector never reads it.

The helper's standard error is its log: the connector reads it line by
line and writes each line into its own log at level `info`, tagged
`helper=<name>` (the program's path until it has said its name). It is
where a helper says what it found and why it failed; a helper never
writes anything but protocol lines to standard output.

## 3. The protocol

Newline-delimited JSON on the helper's standard input (requests from the
connector) and standard output (answers). One object per line; no object
spans lines.

A request is `{"id": n, "method": "...", "params": {...}}`, `id` a
positive integer the connector chooses, unique among the requests in
flight; `params` may be absent. Its answer is `{"id": n, "result": {...}}`
or `{"id": n, "error": {"code": "...", "message": "...", "reason": "..."}}`.
A message without an `id` is a notification, never answered; the one
notification defined goes from the connector to the helper (`cancel`).
Lines the helper cannot read are ignored; unknown methods are answered
with code `unsupported`.

Requests may overlap and are answered in any order: the connector lists
units every half minute while a device is open, and samples telemetry at
2 Hz between commands. Calls on the open device (`release` to `close`)
are made one at a time by the connector's own runtime, in the same way its
in-tree driver is called; a helper's driver need not be safer than that
driver is, but it must answer `list`, `describe` and `select` while a
device call is in flight.

### 3.1 Methods

Each one is one method of the connector's `estim.Finder` or `estim.Driver`
(`internal/estim/estim.go`), so their meaning is documented there and in
PROTOCOL.md section 7; the wire shapes are these. Types named in
backticks are the connector's, serialized as its own protocol serializes
them (PROTOCOL.md 7.3): `Unit`, `Capabilities`, `Status`, `Frame`,
`Command`, `Result`.

| method | params | result | what |
| --- | --- | --- | --- |
| `hello` | none | `{"protocol": 1, "name": "<name>", "kinds": ["<kind>", ...]}` | who the helper is; `protocol` must be `1`, `name` the family's short name, `kinds` every `kind` its drivers may report |
| `describe` | none | `{"text": "..."}` | what the family can see, for `estim probe`: text as the helper's `Describe` writes it, lines ending in `\n` |
| `list` | none | `{"units": [Unit, ...]}` | the family's units in reach, without taking any (`estim.Lister`); `unsupported` if the family cannot list |
| `select` | `{"unit": "<id or name>"}` | `{}` | restrict the family to one unit; `""` lifts it (`estim.Selector`). A unit of another family never matches |
| `find` | none | `{"kind", "label", "port", "capabilities": Capabilities, "held": bool, "renewsArm": bool}` | find and open the device; `no_device` when none. A device still open from an earlier `find` is closed (restoring the unit) before the search |
| `release` | none | `{}` | the device to zero, output stopped, its own controls live; a device with several power ranges back in the one it was found in at `find` (its own setting), whatever range `arm` selected |
| `arm` | `{"powerMode": "normal" or "high"}` | `{}` | arm in the range |
| `renewArm` | `{"until": "<RFC 3339>"}` | `{}` | bring the device's own countdown up to the time (`estim.ArmRenewer`); only sent when `find` said `renewsArm` |
| `status` | none | `{"status": Status}` | a full reading |
| `telemetry` | none | `{"frame": Frame}` | one 2 Hz sample |
| `execute` | `{"command": Command, "levelMax": n}` | `{"result": Result}` | one command, the level never set past `levelMax`. Long: a ramp takes as long as its steps. `cancelled` if the `cancel` notification ended it early |
| `close` | `{"restore": bool}` | `{}` | close the link, restoring the unit's own controls when asked |

The notification: `{"method": "cancel", "params": {"id": n}}` names an
`execute` in flight by its id; the helper's driver sees `cancelled()` true
from then on and ends the ramp where it is, answering `cancelled`. It is
sent when the connector's cancellation latch trips (a release in a hurry).

### 3.2 Errors

`code` is one of:

| code | meaning on the connector's side |
| --- | --- |
| `no_device` | `estim.ErrNoDevice`: the family sees no unit it may serve (a search's ordinary answer) |
| `loss` | `estim.LossError` with `reason`: the link to the unit ended. `reason` is one of the connector's reason codes (PROTOCOL.md 7.3: `idle_off`, `output_off`, `battery_off`, `button_off`, `link_lost`, `stopped_answering`) and becomes the `device` report's `reason`, so the phone can say what to do |
| `cancelled` | `estim.ErrCancelled`: an `execute` ended by `cancel` |
| `armed` | `estim.ErrArmed` |
| `no_driver` | a device call before `find`, or after `close` |
| `unsupported` | a method the helper does not have |
| `error` | any other failure; `message` says what |

A device call that fails with anything but `cancelled` puts the connector
on its usual path for a failed command: the unit is released and, when
the failure is a loss, reported as not connected with that reason.

### 3.3 An exchange

```
→ {"id":1,"method":"hello"}
← {"id":1,"result":{"protocol":1,"name":"example","kinds":["examplekind"]}}
→ {"id":2,"method":"list"}
← {"id":2,"result":{"units":[{"id":"/dev/cu.usbserial-10","kind":"examplekind","label":"Example unit","held":false}]}}
→ {"id":3,"method":"find"}
← {"id":3,"result":{"kind":"examplekind","label":"Example unit","port":"/dev/cu.usbserial-10","capabilities":{"levelMax":99,"channels":["a"],"modes":[0,1,2],"tempo":true,"powerModes":["normal","high"]},"held":false,"renewsArm":false}}
→ {"id":4,"method":"release"}
← {"id":4,"result":{}}
→ {"id":5,"method":"arm","params":{"powerMode":"normal"}}
← {"id":5,"result":{}}
→ {"id":6,"method":"execute","params":{"command":{"verb":"set_level","channel":"a","level":12},"levelMax":40}}
→ {"id":7,"method":"telemetry"}
← {"id":7,"result":{"frame":{"atMs":1700000000000,"monotonicS":12.5,"skipped":false,"skipReason":null,"levelA":6}}}
→ {"method":"cancel","params":{"id":6}}
← {"id":6,"error":{"code":"cancelled","message":"estim: cancelled by a release"}}
→ {"id":8,"method":"status"}
← {"id":8,"error":{"code":"loss","message":"the port closed under us","reason":"link_lost"}}
→ {"id":9,"method":"close","params":{"restore":true}}
← {"id":9,"result":{}}
```

## 4. Distribution, signing, verification

Helpers are published as their own releases, apart from the connector's,
one release per helper family that publishes on its own:
`https://masseuse.ai/app/units/<release>/` holds, for one helpers release,
where `<release>` is a bare version (`1.0.4`, the first family's, at the
root) or a family's prefix and its version (`<family>/0.1.0`, a
lower-case word before the version),

- `manifest.json`: `{"version": "<version>", "files": [{"name":
  "camlink-unit-<name>[.exe]", "os": "darwin|windows|linux", "arch":
  "all|amd64|arm64|arm", "sha256": "<hex>", "kinds": ["<kind>", ...]},
  ...]}`, one entry per helper per operating system and architecture, the
  hash that of the file as published. For macOS there are three entries
  per helper: `arm64` and `amd64`, the thin binaries, and `all`, the two
  joined with `lipo`, which is what the application bundle takes; a signed
  universal helper in a bundle is compared with the thin files through
  `machostrip`, signatures stripped on both sides (VERIFY.md);
- `manifest.json.sigstore.json`: a cosign bundle over the manifest, the
  helpers' signing key's signature with its entry in the Rekor transparency
  log; the key's public half is `packaging/units/cosign.pub` in this
  repository (one key for every family's releases), and `cosign
  verify-blob --key cosign.pub --bundle manifest.json.sigstore.json
  manifest.json` is the check;
- one file per entry, at `<os>_<arch>/<name>`.

`packaging/units/VERSION` in this repository pins the helpers releases a
connector release bundles, one `<release>` per line, and
`packaging/units/fetch-all.sh <os> <arch> <outdir>` is how the release
workflow gets them: for each release it runs `packaging/units/fetch.sh
<release> <os> <arch> <dir>`, which downloads the manifest and its bundle,
checks the signature with `cosign verify-blob --key
packaging/units/cosign.pub`, downloads each file for that platform and
checks its SHA-256 against the manifest, and refuses anything that does
not match; then it puts the releases' helpers together, refusing a helper
file named by two releases (a release cannot stand in for another's
helper). Nothing is left in `<outdir>` when anything fails. Nothing in
this repository needs a secret to build; the release workflow fetches and
verifies on every runner that packs helpers (`.github/workflows/release.yml`),
then bundles them: goreleaser packs `units/` into each archive
(`.goreleaser.yaml`), `packaging/windows/pack -u` signs nothing itself but
packs the helpers, Authenticode-signed by the workflow just before, into
the Windows package's payload as `units/`, and `packaging/macos/build-app.sh -u` copies the
universal ones into `Contents/Helpers/units/`, where
`packaging/macos/sign-notarize.sh` codesigns each (identifier
`ai.masseuse.camlink.unit.<name>`, hardened runtime, no entitlements)
before the bundle, like `Contents/Helpers/ffmpeg`, and notarizes them with
it. The release then checks that each bundled helper, signature stripped,
hashes to the manifest's entry for its architecture, and that the bundled
connector names its helpers when run. `ci.yml` does the fetch and the
unsigned bundle and zip on every pull request.

What that gives someone checking a release (VERIFY.md, "Unit driver
helpers"): the connector binary is reproducible from this repository as
before, the helpers are not, and every helper in a release is named, with
its hash, in a manifest signed by one key, so that a helper in the bundle
can be checked against the published manifest of the release pinned at
the tag. The signing key is held by masseuse.ai and used only by the
helpers' release workflows; a change of key is a change to `cosign.pub`
in this repository, in the open.

## 5. Writing a helper

A helper is any program that speaks section 3 on its standard input and
output. Written in Go against this module, it is a few lines: implement
`estim.Finder` (with `estim.Lister` and `estim.Selector` if the family
can list and select) and `estim.Driver` (with `estim.HeldReporter` and
`estim.ArmRenewer` where they apply) as the Mastago driver does, then

```go
package main

import (
	"log/slog"

	"github.com/FemLed/masseuse-camlink/internal/estim"
	"github.com/FemLed/masseuse-camlink/internal/estim/helper"
)

func main() {
	helper.Main(func(stateDir string, log *slog.Logger) (estim.Finder, helper.Options, error) {
		f := newFinder(stateDir, log) // yours
		return f, helper.Options{Name: "example", Kinds: []estim.Kind{"examplekind"}, Log: log}, nil
	})
}
```

`helper.Main` reads `--state-dir` and `--log-level`, logs to standard
error, and runs `helper.Serve`, which dispatches each request to the
finder and the driver it returned, carries `estim.LossError` reasons,
`estim.ErrNoDevice`, `estim.ErrCancelled` and `estim.ErrArmed` across as
their codes, and closes the device (restoring the unit) when the
connector goes away. Build it as `camlink-unit-<name>` and put it in the
units directory; `masseuse-camlink -estim-helpers <dir> estim probe`
shows what the connector makes of it.

In another language, mind: one JSON object per line, answers may come in
any order but each request gets exactly one, `hello` first, nothing but
protocol on standard output, and `close` with `restore` true must leave
the unit as the person's own controls expect it.

The connector's side of the protocol is `internal/estim/helper` (`Host`
and the proxy driver); its tests run a real Mastago driver through a
guest over pipes and a spawned helper process, and are the reference for
the wire shapes.
