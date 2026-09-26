package main

import (
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// buildMenu is the application menu, in each platform's own shape.
//
// macOS: the application menu (About, Check for updates, Hide, Quit), Edit,
// Window and Help, in the menu bar at the top of the screen. Windows and
// Linux have no application menu: File carries the update check and Quit,
// Help carries the links and About, and the window shows the bar
// (UseApplicationMenu in main.go).
//
// About opens the page's own About dialog through a "menu" event, so it is
// the same on every platform and can carry links; the update check is a
// "menu" event too until the connector link is wired (connector.go).
//
// The version and the state of updates live here as well, as two lines that
// cannot be chosen, under the update check: what the connector's first lines
// and its update lines say in the terminal today (ConnectorService.setStatus
// relabels them as the connector reports).
func buildMenu(app *application.App, c *ConnectorService) *application.Menu {
	menu := app.NewMenu()
	about := func(*application.Context) { app.Event.Emit(menuEvent, "about") }
	// The check is the connector's (update_now); the page hears the same
	// request so it can show the check under way.
	checkUpdates := func(*application.Context) {
		app.Event.Emit(menuEvent, "check-updates")
		_ = c.UpdateNow()
	}
	status := func(in *application.Menu) {
		in.Add("Check for updates…").OnClick(checkUpdates)
		c.versionItem = in.Add(appName + " " + shellVersion()).SetEnabled(false)
		c.updateItem = in.Add("Updates: waiting for the connector").SetEnabled(false)
	}

	if runtime.GOOS == "darwin" {
		// The first menu is the application menu; macOS names it after
		// the application whatever the label says.
		appMenu := menu.AddSubmenu(appName)
		appMenu.Add("About " + appName).OnClick(about)
		appMenu.AddSeparator()
		status(appMenu)
		appMenu.AddSeparator()
		appMenu.AddRole(application.Hide)
		appMenu.AddRole(application.HideOthers)
		appMenu.AddRole(application.UnHide)
		appMenu.AddSeparator()
		appMenu.AddRole(application.Quit)
	} else {
		fileMenu := menu.AddSubmenu("File")
		status(fileMenu)
		fileMenu.AddSeparator()
		fileMenu.AddRole(application.Quit)
	}
	c.menu = menu

	// Copy and paste, for the address of a camera on the network.
	menu.AddRole(application.EditMenu)
	if runtime.GOOS == "darwin" {
		menu.AddRole(application.WindowMenu)
	}

	help := menu.AddSubmenu("Help")
	link := func(name string) func(*application.Context) {
		return func(*application.Context) { _ = c.OpenLink(name) }
	}
	help.Add("Learn more about " + appName).OnClick(link("learn-more"))
	help.Add("How it stays private").OnClick(link("privacy"))
	help.Add("Verify this download").OnClick(link("verify"))
	help.Add("Report a security issue").OnClick(link("security"))
	help.AddSeparator()
	help.Add("Privacy Policy").OnClick(link("privacy-policy"))
	help.Add("Terms of Service").OnClick(link("terms"))
	help.AddSeparator()
	help.Add("Show the log").OnClick(func(*application.Context) { _ = c.ShowLog() })
	help.Add("Open the state folder").OnClick(func(*application.Context) { _ = c.RevealStateDir() })
	if runtime.GOOS != "darwin" {
		help.AddSeparator()
		help.Add("About " + appName).OnClick(about)
	}
	return menu
}
