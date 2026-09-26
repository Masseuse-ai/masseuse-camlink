package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
)

// What the program tells the person goes through a reporter. The console
// prints the lines in this file, one fact at a time as the program reaches
// it; the desktop window (-ipc, ipc.go) is told the same facts as JSON
// events and puts them where its screens want them. Every method is one
// fact, named for what happened, so the wording stays here and the callers
// say nothing about how it is shown.
type reporter interface {
	// Startup, in the order main.go reaches them.
	Banner(version, identity, stateDir string)
	Source(s sourceReport)
	Drivers(line string)
	PairedCount(n int)
	Awake(state awakeState, note string)
	// Ready says startup is over: the window's first word (hello) goes out.
	Ready()
	Stopped()

	// The service.
	Online(online bool)
	Code(code string, expiresAt time.Time)
	Paired(n int)

	// The camera link and the cameras.
	Link(state linkState, reason string)
	Enclave(p enclaveProof)
	CameraOn(label string)
	CameraOff()
	Sending(shape string, videoBps, audioBps float64, congested bool, backlog time.Duration)
	NotSending(reason string)
	FaceOn(label string)
	FaceOff()
	// Share is the phone's picture starting or stopping to arrive.
	Share(receiving bool, url string)

	// The stimulation unit.
	Units(units []estim.Unit, serving *estim.Descriptor, withPicker bool)
	// Device is the served unit found or lost; exiting says the connector
	// let go of it on its way out, which the console does not mention.
	Device(d estim.Descriptor, exiting bool)
	// DeviceState is the served unit's state as it changes (armed, its
	// levels): the window's, the console says nothing.
	DeviceState(d estim.Descriptor, st estim.Status, armed bool, bound int)

	// Updates: Update is said on the console too, UpdateQuiet is the
	// window's alone (a check that found nothing, a check under way).
	Update(state updateState, tag, text string)
	UpdateQuiet(state updateState, tag, text string)

	// Notice is anything else worth a line; Blocked is why the program
	// cannot run at all, said before it ends.
	Notice(level noticeLevel, text string)
	Blocked(kind blockedKind, detail string)
}

// sourceReport is what the connector offers: the camera (the body view),
// the phone's picture if asked for, and the front-facing camera if one is
// configured (main.go, the source report v2's fields).
type sourceReport struct {
	Kind  string
	Label string
	Ready bool
	// Note is why it is not ready, for the person.
	Note string
	// Shape describes the picture ("1280x720 30 fps, h264_videotoolbox").
	Shape string
	// Camera and Mic name the computer's devices (kind capture); URL is
	// the network camera (kind camera).
	Camera, Mic, URL string
	// Share is the phone's picture offered to this computer; nil when not
	// asked for.
	Share *shareReport
	// Face is the front-facing camera; nil for the phone's own camera.
	Face *faceReport
}

type shareReport struct {
	// Ready says the loopback address is listening.
	Ready bool
	// Address is where programs on this computer read it.
	Address string
	// Receiving says the phone's picture is arriving now.
	Receiving bool
}

type faceReport struct {
	Label  string
	Camera string
	Ready  bool
	Note   string
	Shape  string
}

// enclaveProof is the attested enclave's provenance, as checked against the
// public registry and the Sigstore log (policyAttester.checkProvenance).
type enclaveProof struct {
	Image, Release, Commit, Source, Registry, SignedBy string
	Cached                                             bool
}

type awakeState int

const (
	// awakeHeld: the computer is held awake while the program runs.
	awakeHeld awakeState = iota
	// awakeAllowed: -allow-sleep; the computer may sleep.
	awakeAllowed
	// awakeFailed: the hold could not be taken; note says why.
	awakeFailed
	// awakeUnsupported: this system has no such hold; nothing is said.
	awakeUnsupported
)

type linkState string

const (
	linkActive linkState = "active"
	linkOnHold linkState = "on-hold"
	// linkClosed: the session ended.
	linkClosed linkState = "closed"
	// linkReset: the enclave closed the link itself; reason says why.
	linkReset linkState = "reset"
)

type updateState string

const (
	updateCurrent    updateState = "current"
	updateChecking   updateState = "checking"
	updateStaged     updateState = "staged"
	updateInstalling updateState = "installing"
	updateFailed     updateState = "failed"
	updateOff        updateState = "off"
)

type noticeLevel string

const (
	noticeInfo  noticeLevel = "info"
	noticeWarn  noticeLevel = "warn"
	noticeError noticeLevel = "error"
)

type blockedKind string

const (
	blockedAlreadyRunning blockedKind = "already-running"
	blockedStateDir       blockedKind = "state-dir-unwritable"
)

// consoleReporter prints to a terminal: the program's lines as they have
// always read (README).
type consoleReporter struct {
	mu sync.Mutex
	w  io.Writer
	// err is where Blocked goes; standard error unless set.
	err io.Writer
}

// newConsole reports to w (standard output when nil).
func newConsole(w io.Writer) *consoleReporter {
	if w == nil {
		w = os.Stdout
	}
	return &consoleReporter{w: w, err: os.Stderr}
}

// stdConsole is the console on standard output: what a component with no
// reporter of its own says to.
var stdConsole reporter = newConsole(nil)

func (c *consoleReporter) printf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.w, format, args...)
}

func (c *consoleReporter) Banner(version, identity, stateDir string) {
	// The window's first line: the name on the download, and the program's
	// own name and version for anyone comparing with a release.
	c.printf("%s for your computer  (masseuse-camlink %s)\n", appName, version)
	c.printf("Identity %s… (state in %s)\n", identity, stateDir)
}

func (c *consoleReporter) Source(s sourceReport) {
	if !s.Ready {
		c.printf("Camera: %s is not available: %s\n", s.Label, s.Note)
	} else {
		c.printf("Camera: %s (%s). It is on only while a session reads it.\n", s.Label, s.Shape)
	}
	if f := s.Face; f != nil {
		if !f.Ready {
			c.printf("Front-facing camera: %s is not available: %s. Your phone's own camera stays your face.\n", f.Label, f.Note)
		} else {
			c.printf("Front-facing camera: %s (%s). It is on only while a session shows it as your face; until then your phone's own camera is.\n", f.Label, f.Shape)
		}
	}
	if sh := s.Share; sh != nil && sh.Ready {
		c.printf("Your phone's picture: sessions are asked to send it here, and programs on this computer can open it at\n  %s\n  (OBS: a Media Source with Local File unticked, that address as the Input, Network Buffering 0 MB.)\n", sh.Address)
	}
}

func (c *consoleReporter) Drivers(line string) { c.printf("%s\n", line) }

func (c *consoleReporter) PairedCount(n int) {
	if n > 0 {
		c.printf("Paired with %d phone(s). Sessions that use this camera connect automatically.\n", n)
	}
}

func (c *consoleReporter) Awake(state awakeState, note string) {
	switch state {
	case awakeAllowed:
		c.printf("This computer may go to sleep on its own (-allow-sleep); the camera, the microphone and the unit stop with it.\n")
	case awakeHeld:
		c.printf("This computer stays awake while %s runs (the screen may go dark; keep the lid open).\n", appName)
	case awakeFailed:
		c.printf("Could not keep this computer from sleeping (%s). Set it not to sleep while %s runs.\n", note, appName)
	}
}

func (c *consoleReporter) Ready()   {}
func (c *consoleReporter) Stopped() { c.printf("\nStopped.\n") }

func (c *consoleReporter) Online(bool) {}

func (c *consoleReporter) Code(code string, expiresAt time.Time) {
	c.printf("\nPairing code: %s\n", code)
	c.printf("Type it into the masseuse.ai app on your phone when it asks for the code from your computer; the dash is added for you.\n")
	if !expiresAt.IsZero() && expiresAt.Year() > 2000 {
		c.printf("(valid until %s; a new one appears here when it expires)\n\n", expiresAt.Local().Format("15:04"))
	}
}

func (c *consoleReporter) Paired(int) {
	c.printf("Paired with a phone. Sessions that use this camera connect automatically.\n")
}

func (c *consoleReporter) Link(state linkState, reason string) {
	switch state {
	case linkActive:
		c.printf("Camera link active: connected to the verified enclave.\n")
	case linkOnHold:
		c.printf("Camera link on hold: the enclave is not expecting this connector; waiting for the service.\n")
	case linkClosed:
		c.printf("Camera link closed.\n")
	case linkReset:
		c.printf("The enclave closed the camera link (%s); waiting for the service.\n", reason)
	}
}

func (c *consoleReporter) Enclave(p enclaveProof) {
	if p.Cached {
		return
	}
	digest := strings.TrimPrefix(p.Image, "sha256:")
	if len(digest) > 12 {
		digest = digest[:12]
	}
	c.printf("Enclave image %s… is %s %s (commit %.7s): signature and build provenance verified in the public registry and the Sigstore log.\n",
		digest, p.Source, p.Release, p.Commit)
}

func (c *consoleReporter) CameraOn(label string) { c.printf("Camera on: %s.\n", label) }
func (c *consoleReporter) CameraOff()            { c.printf("Camera off.\n") }

func (c *consoleReporter) Sending(shape string, videoBps, audioBps float64, congested bool, backlog time.Duration) {
	c.printf("Sending %s: video %s, audio %s\n", shape, rate(videoBps), rate(audioBps))
	if congested {
		c.printf("Connection congested: dropping video to keep up (backlog %.1f s).\n", backlog.Seconds())
	}
}

func (c *consoleReporter) NotSending(reason string) {
	if reason != "" {
		c.printf("Camera on but not sending yet: %s.\n", reason)
	} else {
		c.printf("Camera on but not sending yet (waiting for the source).\n")
	}
}

func (c *consoleReporter) FaceOn(label string) { c.printf("Front-facing camera on: %s.\n", label) }
func (c *consoleReporter) FaceOff()            { c.printf("Front-facing camera off.\n") }

func (c *consoleReporter) Share(receiving bool, url string) {
	if receiving {
		c.printf("Your phone's picture is arriving. Programs on this computer can open it at %s\n", url)
	} else {
		c.printf("Your phone's picture stopped.\n")
	}
}

func (c *consoleReporter) Units(units []estim.Unit, serving *estim.Descriptor, withPicker bool) {
	// A list of one is no choice; the console says nothing until there is
	// one to make.
	if len(units) > 1 {
		c.printf("%s", unitListing(units, serving, withPicker))
	}
}

func (c *consoleReporter) Device(d estim.Descriptor, exiting bool) {
	switch {
	case d.Connected:
		c.printf("Stimulation device connected: %s. It is held at zero until a session on your phone uses this computer.\n", d.Label)
		if d.Held {
			c.printf("%s\n", heldByAnotherLine)
		}
	case exiting:
		// The connector is exiting and let go of the device on purpose;
		// the service is told, the person is not.
	default:
		c.printf("%s\n", disconnectedLine(d.Reason))
	}
}

func (c *consoleReporter) DeviceState(estim.Descriptor, estim.Status, bool, int) {}

func (c *consoleReporter) Update(_ updateState, _, text string) { c.printf("%s\n", text) }

func (c *consoleReporter) UpdateQuiet(updateState, string, string) {}

func (c *consoleReporter) Notice(_ noticeLevel, text string) { c.printf("%s\n", text) }

func (c *consoleReporter) Blocked(kind blockedKind, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch kind {
	case blockedAlreadyRunning:
		fmt.Fprintln(c.err, appName+" is already running, in another window. Close that one first, or give this one its own -state-dir.")
	default:
		fmt.Fprintln(c.err, detail)
	}
}
