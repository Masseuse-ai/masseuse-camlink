package main

import "github.com/wailsapp/wails/v3/pkg/w32"

// The page and the frame are dark whatever mode Windows is in (theme.go),
// and the menu bar's background is painted under its dropdowns as well
// (Wails applies the bar's brush to the submenus). Windows draws a
// dropdown's text, check-mark gutter and separators in the process's
// colour policy, which follows the system's setting unless told otherwise:
// on a computer in light mode that is black text on the bar's ink. Told
// otherwise, here: the policy is dark, so the dropdowns read white on ink
// in light mode as they do in dark mode. This runs after w32's own init,
// which asks for dark only when the system is dark; the exports are nil on
// a Windows without them (before 10 1809), which then keeps its own menus.
func init() {
	if w32.SetPreferredAppMode == nil {
		return
	}
	w32.SetPreferredAppMode(w32.PreferredAppModeForceDark)
	if w32.RefreshImmersiveColorPolicyState != nil {
		w32.RefreshImmersiveColorPolicyState()
	}
	if w32.FlushMenuThemes != nil {
		w32.FlushMenuThemes()
	}
}
