//go:build darwin

package main

// The shell asks macOS for the camera and the microphone itself. It is the
// bundle's executable, the process macOS holds responsible for what its
// children open (ffmpeg, under the connector), and it carries the camera
// and microphone entitlements and the usage strings the system shows
// (packaging/macos/device.entitlements, Info.plist), so the grant it wins
// here is the one ffmpeg's avfoundation input is checked against later.
// The question used to come only when a session first opened the camera,
// on a screen that may be dark with no one looking at the Mac; the
// connector then ran with no picture (internal/capture, stallTrouble).
//
// AVFoundation's authorization calls are the ones the system's own
// applications use: authorizationStatusForMediaType: reads the standing
// without a prompt, requestAccessForMediaType: prompts while it is not
// determined and answers at once otherwise (a refusal is never asked about
// again; System Settings is the way back). Both are macOS 10.14 or newer;
// the floor is 12 (build/darwin) and 13 (the release). This is the module's
// one cgo file of its own; the window toolkit is cgo already, so nothing
// changes in how the shell is built. A process without the two usage
// strings in its Info.plist is ended by the system when it asks, which is
// why build/darwin/Info.plist and Info.dev.plist carry them too.

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AVFoundation -framework Foundation

#import <AVFoundation/AVFoundation.h>
#import <dispatch/dispatch.h>

static AVMediaType mediaType(int video) {
	return video ? AVMediaTypeVideo : AVMediaTypeAudio;
}

// mediaStatus is this application's standing on the media type, as
// AVAuthorizationStatus (0 not determined, 1 restricted, 2 denied, 3
// authorized). A question to the system, never a prompt.
static int mediaStatus(int video) {
	return (int)[AVCaptureDevice authorizationStatusForMediaType:mediaType(video)];
}

// requestMedia asks for the media type and waits for the answer: the
// system's prompt while the standing is not determined (the person takes
// as long as they like; only the goroutine calling this waits), the
// standing answer at once otherwise. Returns the standing afterwards.
static int requestMedia(int video) {
	dispatch_semaphore_t done = dispatch_semaphore_create(0);
	[AVCaptureDevice requestAccessForMediaType:mediaType(video) completionHandler:^(BOOL granted) {
		(void)granted;
		dispatch_semaphore_signal(done);
	}];
	dispatch_semaphore_wait(done, DISPATCH_TIME_FOREVER);
	return mediaStatus(video);
}
*/
import "C"

import "os/exec"

// mediaWatched says the standing can change while the program runs (a
// switch in System Settings), so the shell keeps reading it.
const mediaWatched = true

// mediaAuthStatus is the standing on the camera and the microphone.
func mediaAuthStatus() (camera, mic string) {
	camera = mediaWord(int(C.mediaStatus(1)))
	mic = mediaWord(int(C.mediaStatus(0)))
	return camera, mic
}

// requestMediaAccess asks for the camera and then the microphone, each
// prompt in turn, and returns the standing after both answers. It blocks
// for as long as the prompts are up; the caller runs it off the main
// thread (ConnectorService.RequestMediaAccess).
func requestMediaAccess() (camera, mic string) {
	camera = mediaWord(int(C.requestMedia(1)))
	mic = mediaWord(int(C.requestMedia(0)))
	return camera, mic
}

// openPrivacySettings opens System Settings on Privacy & Security: the
// Camera pane while the camera is not allowed, the Microphone pane when
// only the microphone is. Through open, as a link would.
func openPrivacySettings(camera, mic string) error {
	pane := "Privacy_Camera"
	if camera == mediaAuthorized && mic != mediaAuthorized {
		pane = "Privacy_Microphone"
	}
	return exec.Command("/usr/bin/open", "x-apple.systempreferences:com.apple.preference.security?"+pane).Run()
}
