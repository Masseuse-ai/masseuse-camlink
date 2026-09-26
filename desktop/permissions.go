package main

// The system's standing on this computer's camera and microphone for this
// application, in the words the page reads (frontend/src/bridge/types.ts,
// MediaPermission). They are macOS's AVAuthorizationStatus by name: on a
// Mac the application is asked once, in its own name, and a refusal is
// silent ever after (the camera never comes on, no prompt), so the shell
// asks itself while the person is at the computer choosing devices, on the
// Cameras screen, rather than leaving it to the first session
// (permissions_darwin.go). Windows and Linux have no such question for a
// desktop program and answer "authorized" (permissions_other.go); the
// Windows privacy switch is worded by the connector when a session finds
// the camera refused (internal/capture, stallTrouble).
const (
	// mediaNotDetermined: the system has not asked yet; asking brings the prompt.
	mediaNotDetermined = "notDetermined"
	// mediaRestricted: a profile or parental controls forbid it; nothing to ask.
	mediaRestricted = "restricted"
	// mediaDenied: refused at the prompt or switched off in System Settings since.
	mediaDenied = "denied"
	// mediaAuthorized: allowed; the grant covers ffmpeg, the shell's grandchild.
	mediaAuthorized = "authorized"
)

// mediaWord is the word for an AVAuthorizationStatus value (0 not
// determined, 1 restricted, 2 denied, 3 authorized). A value the system
// adds later reads as not determined: asking is the harmless answer to a
// standing the shell does not know.
func mediaWord(status int) string {
	switch status {
	case 1:
		return mediaRestricted
	case 2:
		return mediaDenied
	case 3:
		return mediaAuthorized
	default:
		return mediaNotDetermined
	}
}

// mediaEvent is the shell's own event on the standing: the connector never
// sends one (the page's ConnectorEvent, type "media").
func mediaEvent(camera, mic string) map[string]any {
	return map[string]any{"type": "media", "camera": camera, "mic": mic}
}
