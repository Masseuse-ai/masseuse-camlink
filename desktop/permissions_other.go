//go:build !darwin

package main

// Windows and Linux put no question to a desktop program before it opens
// the camera (Windows has one privacy switch for every desktop program,
// which the connector words when a session finds the camera refused:
// internal/capture, stallTrouble), so the standing is authorized, there is
// nothing to ask, nothing to open, and nothing to keep reading. The page
// shows the cards on macOS only.

const mediaWatched = false

func mediaAuthStatus() (camera, mic string) { return mediaAuthorized, mediaAuthorized }

func requestMediaAccess() (camera, mic string) { return mediaAuthStatus() }

func openPrivacySettings(camera, mic string) error { return nil }
