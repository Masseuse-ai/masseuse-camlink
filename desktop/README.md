# desktop: the window around masseuse-camlink

The desktop shell: a native window (Wails v3) that shows what the connector
is doing and takes the person's choices. The connector itself,
`cmd/masseuse-camlink`, stays the separate CGO-free program VERIFY.md
describes; this shell starts it as a child and speaks to it over its
standard input and output, the way the connector speaks to its unit driver
helpers. The design, the screens, their states and copy, the event and
command model between the two, and what is built in which phase are in
[docs/DESKTOP.md](../docs/DESKTOP.md).

This is a Go module of its own (`github.com/FemLed/masseuse-camlink/desktop`),
so the connector's `go.mod` never learns about the window toolkit.

## How it runs

`ConnectorService` (`connector.go`) starts the connector on
`ServiceStartup`: `masseuse-camlink -ipc -state-dir DIR -install-root ROOT`,
found beside the shell (`Contents/MacOS/masseuse-camlink` in the bundle,
the unpacked payload on Windows, the same directory on Linux, or
`MASSEUSE_CAMLINK_BIN` while working on it; `locate.go`). Each JSON line
the connector writes is relayed to the page as the Wails event `connector`
and the last of each kind kept for a page that mounts later (`Snapshot`);
the page's requests are the typed methods (`ListDevices`, `SetSource`,
`SelectUnit`, `UpdateNow`, `Quit`, `Restart`) written to its standard
input. Its standard error is the log, `desktop.log` under the state
directory (Help, "Show the log"). Quitting sends `quit` and waits a bounded
few seconds; while a session has the camera or the unit armed, the person
is asked first. When the connector has installed an update it ends with
exit code 75, and the shell quits and starts itself again (`relaunch.go`).
In a browser the page runs on a mock of the connector
(`frontend/src/bridge/mock`); `?mock=1` asks for the mock inside the window
too. The page tells the window from a browser by the webview's own bridge
object (`frontend/src/shell/shell.ts`, `inShell`), which is there before
the Wails runtime is: the runtime's configuration (`window._wails.environment`,
what `System.IsDesktop()` and the platform read) is injected only once the
page has loaded, after the page's scripts on Windows and Linux, so the first
render waits for it (`runtimeReady`, bounded) instead of deciding without it.

## Working on it

```sh
cd desktop/frontend && npm install          # once
npm run dev                                 # the page in a browser, http://127.0.0.1:9245/
                                            # with the scenario panel (every state; ?scenario=<id>&os=<darwin|windows|linux>)
npm run typecheck && npm run build          # what CI runs

cd desktop
wails3 task connector:build                 # the connector into bin/, where the shell looks for it
wails3 task build && ./bin/Masseuse         # the window around the real connector
wails3 task run:all                         # both of the above
wails3 dev                                  # the window against the dev server, hot reload (needs bin/masseuse-camlink,
                                            # or MASSEUSE_CAMLINK_BIN=/path/to/masseuse-camlink)
wails3 generate bindings -ts -i             # after changing a Go service; frontend/bindings is committed
go test ./...                               # the supervisor against a fake connector
```

The shell passes its own arguments on to the connector, so
`./bin/Masseuse -state-dir /tmp/x -log-level debug -service https://...`
runs the window around a connector with those flags.

`wails3 task common:update:build-assets` regenerates the platform assets
under `build/` from `build/config.yml`; it also recreates `build/ios/`,
which is removed again (the shell is desktop only). On Windows the
resources linked into the program (the icon, the strings Explorer shows,
the manifest) are the committed `rsrc_windows_amd64.syso`, made by
`packaging/windows/make-syso.sh` from `winres.json` here; `wails3 task
build` links it as the release does and does not generate a second
resource object (`build/windows/Taskfile.yml`).

The shipped downloads are not this Taskfile's: the release assembles
`Masseuse.app` and `Masseuse.exe` around the shell, the connector, ffmpeg and
the unit driver helpers (`packaging/macos`, `packaging/windows`), signed as
they are today.
