package main

import "github.com/wailsapp/wails/v3/pkg/application"

// windowsTheme is the brand's colours for the parts of the window Windows
// draws itself: the title bar and the menu bar the application menu is
// shown in (docs/DESKTOP.md, "Menus"). The page is dark only, in the
// masseuse.ai tokens (frontend/src/index.css: ink #0b0a10, ink-soft
// #15121d, bone #f4eef2, bone-dim #b9adb8); this is the same for the frame.
//
// Wails paints its own default dark menu bar only while Windows itself is
// in dark mode (the native popup menus' text follows that process-wide
// policy, and it will not draw a dark bar under text it cannot recolour),
// so on a computer in light mode Theme: Dark alone leaves a white bar with
// "File Edit Help" over the dark page. A custom theme is honoured in both
// modes: the menu bar is owner-drawn with these colours, and the caption
// colours go to DWM (Windows 11; Windows 10 keeps the dark caption that
// Theme: Dark asks for). The popups themselves stay the system's.
func windowsTheme() application.ThemeSettings {
	ink := application.NewRGBPtr(0x0b, 0x0a, 0x10)
	inkSoft := application.NewRGBPtr(0x15, 0x12, 0x1d)
	bone := application.NewRGBPtr(0xf4, 0xee, 0xf2)
	boneDim := application.NewRGBPtr(0xb9, 0xad, 0xb8)

	// The same frame whether Windows calls the mode dark or light; the
	// title dims with the window when another one is in front.
	active := &application.WindowTheme{BorderColour: ink, TitleBarColour: ink, TitleTextColour: bone}
	inactive := &application.WindowTheme{BorderColour: ink, TitleBarColour: ink, TitleTextColour: boneDim}
	// All three states are read when the theme is applied, so all three
	// are set.
	menu := &application.MenuBarTheme{
		Default:  &application.TextTheme{Text: bone, Background: ink},
		Hover:    &application.TextTheme{Text: bone, Background: inkSoft},
		Selected: &application.TextTheme{Text: bone, Background: inkSoft},
	}
	return application.ThemeSettings{
		DarkModeActive:    active,
		DarkModeInactive:  inactive,
		LightModeActive:   active,
		LightModeInactive: inactive,
		DarkModeMenuBar:   menu,
		LightModeMenuBar:  menu,
	}
}
