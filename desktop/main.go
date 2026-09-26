// Masseuse.ai for your computer: the window around masseuse-camlink.
//
// This program is the shell. It is a native window (Wails v3) that shows
// what the connector is doing and takes the person's choices: the pairing
// code, the camera and microphone, the stimulation unit. The connector
// itself, cmd/masseuse-camlink, stays the separate CGO-free program
// VERIFY.md describes, byte for byte; the shell starts it as a child and
// speaks to it over its standard input and output, the way the connector
// speaks to its unit driver helpers (docs/DESKTOP.md).
//
// This is a separate Go module on purpose: the connector's go.mod never
// learns about the window toolkit, so "the connector is entirely this
// repository" stays true and its build stays reproducible.
package main

import (
	"embed"
	"fmt"
	"log"
	"os"
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// The built frontend (frontend/dist) is served from inside the binary.
//
//go:embed all:frontend/dist
var assets embed.FS

// appName is what a person sees the program called: the window, the menu,
// the first line (cmd/masseuse-camlink/desktop.go says the same).
const appName = "Masseuse.ai"

// menuEvent carries a menu item's request to the window ("about",
// "check-updates"); the bindings generator emits its TypeScript type.
const menuEvent = "menu"

func init() {
	application.RegisterEvent[string](menuEvent)
}

func main() {
	// --version is answered without a window: the connector's updater runs
	// the new package's --version on Windows before swapping it in
	// (internal/update, checkVersion), and expects the tag first.
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println(shellVersion(), runtime.Version())
		return
	}
	connector := newConnectorService()
	app := application.New(application.Options{
		Name:        appName,
		Description: "Masseuse.ai for your computer",
		Services: []application.Service{
			application.NewService(connector),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			// Closing the window is quitting, on every platform, as
			// closing the terminal window is for the connector today:
			// the camera goes off and the unit is released.
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		// Opening the program again brings this window forward instead of
		// a second one (the connector holds its own lock on the state
		// directory as well).
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "ai.masseuse.camlink.desktop",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				if w, ok := application.Get().Window.GetByName(mainWindow); ok {
					w.Restore()
					w.Show()
					w.Focus()
				}
			},
		},
		ShouldQuit: connector.shouldQuit,
		// The last thing before the process ends, after the services are
		// down (the connector with them) and the single-instance lock is
		// released: when the connector ended with the relaunch code, an
		// update is in place and the new version is started from the same
		// place (relaunch.go). Run() does not return on every platform
		// (AppKit ends the process on quit), so this is the one place.
		PostShutdown: func() {
			if connector.shouldRelaunch() {
				if err := relaunchProgram(os.Args[1:]); err != nil {
					log.Printf("could not start the new version: %v", err)
				}
			}
		},
	})
	connector.app = app
	app.Menu.Set(buildMenu(app, connector))

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:      mainWindow,
		Title:     appName,
		Width:     1040,
		Height:    720,
		MinWidth:  1040,
		MinHeight: 720,
		// The brand's ink, so nothing lighter shows before the page paints.
		BackgroundColour: application.NewRGB(11, 10, 16),
		// Windows and Linux draw the application menu in the window.
		UseApplicationMenu: true,
		Mac: application.MacWindow{
			// The title bar is the page's own top bar; the traffic lights
			// sit over it and the top 52 px drag the window.
			TitleBar:                application.MacTitleBarHiddenInset,
			InvisibleTitleBarHeight: 52,
			Backdrop:                application.MacBackdropNormal,
		},
		Windows: application.WindowsWindow{
			// Dark, and the title bar and menu bar in the brand's colours
			// whatever mode Windows itself is in (theme.go).
			Theme:       application.Dark,
			CustomTheme: windowsTheme(),
		},
		URL: "/",
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

// mainWindow is the one window's name, for finding it again.
const mainWindow = "main"
